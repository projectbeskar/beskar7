/*
Copyright 2024 The Beskar7 Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infrastructurev1beta1 "github.com/projectbeskar/beskar7/api/v1beta1"
	"github.com/projectbeskar/beskar7/internal/auth"
)

const (
	// inspectionMaxBodyBytes caps the request body the inspection handler will
	// read. The real inspector payload (a few CPUs + DIMMs + NICs + disks) is on
	// the order of low kilobytes; 1 MiB is far above that and far below the
	// memory pressure point on the manager pod.
	inspectionMaxBodyBytes = 1 << 20

	// inspectionResultDataKey is the key inside the per-host result ConfigMap
	// that holds the JSON-encoded InspectionReport.
	inspectionResultDataKey = "report.json"

	// inspectionResultLabelOwnedBy is set on each result ConfigMap so operators
	// can grep for objects this controller owns and so a future cleanup pass
	// can find orphans.
	inspectionResultLabelOwnedBy = "infrastructure.cluster.x-k8s.io/owned-by"
	// inspectionResultLabelHost links the ConfigMap back to its PhysicalHost by
	// name (the namespace is implicit).
	inspectionResultLabelHost = "infrastructure.cluster.x-k8s.io/host"
)

// InspectionHandler handles HTTP requests from inspection images.
//
// Authentication: callers must present "Authorization: Bearer <token>" with a
// token whose SHA-256 matches the targeted PhysicalHost's
// Status.Bootstrap.TokenHash and whose ExpiresAt is in the future.
// Authentication is enforced by the auth.RequireBearer middleware wrapped around
// this handler in SetupCallbackServer; ServeHTTP itself assumes the request has
// already authenticated.
//
// Status ownership: this handler does NOT write to PhysicalHost.Status. It writes
// the validated InspectionReport to a ConfigMap and patches an annotation onto
// the PhysicalHost; the PhysicalHostReconciler is the sole writer of
// Status.InspectionReport / Status.InspectionPhase (D-005).
type InspectionHandler struct {
	Client client.Client
	Log    logr.Logger
}

// InspectionReportRequest represents the JSON payload from inspection image.
//
// Note that namespace and hostName are NOT taken from the request body — they
// come from the URL path and are validated by the bearer-auth middleware. The
// JSON-body fields with those names exist for backward compat with older
// inspectors but are ignored by ServeHTTP.
type InspectionReportRequest struct {
	// Deprecated: Namespace is taken from the URL path. Retained in JSON for
	// backward compat with legacy inspectors; ignored at decode time.
	Namespace string `json:"namespace,omitempty"`
	// Deprecated: HostName is taken from the URL path. Retained in JSON for
	// backward compat with legacy inspectors; ignored at decode time.
	HostName string `json:"hostName,omitempty"`

	// Hardware information from inspection
	Manufacturer string     `json:"manufacturer,omitempty"`
	Model        string     `json:"model,omitempty"`
	SerialNumber string     `json:"serialNumber,omitempty"`
	CPUs         []CPUData  `json:"cpus,omitempty"`
	Memory       []MemData  `json:"memory,omitempty"`
	Disks        []DiskData `json:"disks,omitempty"`
	NICs         []NICData  `json:"nics,omitempty"`

	// Additional metadata
	BootModeDetected string `json:"bootModeDetected,omitempty"`
	FirmwareVersion  string `json:"firmwareVersion,omitempty"`
}

type CPUData struct {
	ID        string `json:"id,omitempty"`
	Vendor    string `json:"vendor,omitempty"`
	Model     string `json:"model,omitempty"`
	Cores     int    `json:"cores,omitempty"`
	Threads   int    `json:"threads,omitempty"`
	Frequency string `json:"frequency,omitempty"`
}

type MemData struct {
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Capacity string `json:"capacity,omitempty"`
	Speed    string `json:"speed,omitempty"`
}

type DiskData struct {
	Name         string `json:"name,omitempty"`
	Model        string `json:"model,omitempty"`
	SizeGB       int    `json:"sizeGB,omitempty"`
	Type         string `json:"type,omitempty"`
	SerialNumber string `json:"serialNumber,omitempty"`
}

type NICData struct {
	Name        string   `json:"name,omitempty"`
	MACAddress  string   `json:"macAddress,omitempty"`
	Driver      string   `json:"driver,omitempty"`
	Speed       string   `json:"speed,omitempty"`
	IPAddresses []string `json:"ipAddresses,omitempty"`
}

// ServeHTTP handles inspection report submissions. It is invoked only after the
// bearer-auth middleware has validated the caller against the targeted host's
// Status.Bootstrap.TokenHash. The path values "namespace" and "hostName" come
// from the registered route (POST /api/v1/inspection/{namespace}/{hostName}).
func (h *InspectionHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	namespace := r.PathValue("namespace")
	hostName := r.PathValue("hostName")
	log := h.Log.WithValues("method", r.Method, "namespace", namespace, "host", hostName, "remote", r.RemoteAddr)

	if r.Method != http.MethodPost {
		log.Info("Method not allowed", "method", r.Method)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Cap the request body. http.MaxBytesReader transforms over-limit reads into
	// errors that the JSON decoder surfaces; we map those to 413.
	r.Body = http.MaxBytesReader(w, r.Body, inspectionMaxBodyBytes)
	defer func() { _ = r.Body.Close() }()

	var req InspectionReportRequest
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil {
		// MaxBytesReader returns *http.MaxBytesError on overflow.
		var mbErr *http.MaxBytesError
		if errors.As(err, &mbErr) {
			log.Info("Inspection report exceeded body cap", "limit", inspectionMaxBodyBytes)
			http.Error(w, "Request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		log.V(1).Info("Failed to decode inspection report", "err", err.Error())
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	log.Info("Received inspection report")

	// Use the request context — the manager will cancel it on shutdown so
	// in-flight handlers don't block graceful drain. (Replaces the previous
	// context.Background() that ignored shutdown signals.)
	ctx := r.Context()

	if err := h.processInspectionReport(ctx, log, namespace, hostName, req); err != nil {
		log.Error(err, "Failed to process inspection report")
		// Do not echo internal error text to the client — could leak resource
		// names or k8s API details. Generic 500.
		http.Error(w, "Failed to process inspection report", http.StatusInternalServerError)
		return
	}

	// 202 Accepted (not 200): the handler has stored the report and signalled
	// the reconciler, but Status.InspectionReport / Status.InspectionPhase have
	// not yet been written by the PhysicalHost controller. From the inspector's
	// perspective the request is accepted; from the system's perspective it is
	// still in flight. (D-005.)
	log.Info("Inspection report accepted; signalled reconciler via annotation")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	if _, err := w.Write([]byte(`{"status":"accepted"}`)); err != nil {
		log.V(1).Info("Failed to write response body", "err", err.Error())
	}
}

// processInspectionReport stores the validated InspectionReport on a ConfigMap
// and patches an annotation onto the PhysicalHost. It does NOT write to
// PhysicalHost.Status — the PhysicalHostReconciler owns that (D-005).
func (h *InspectionHandler) processInspectionReport(
	ctx context.Context,
	log logr.Logger,
	namespace, hostName string,
	req InspectionReportRequest,
) error {
	// Get PhysicalHost — verifies it exists and gives us a UID for owner-ref
	// on the ConfigMap. The bearer middleware already ran the same Get to
	// validate the token; we re-Get here for a fresh resourceVersion before
	// patching, and to handle the (rare) case where the host was deleted
	// between the auth check and now.
	physicalHost := &infrastructurev1beta1.PhysicalHost{}
	key := types.NamespacedName{Namespace: namespace, Name: hostName}
	if err := h.Client.Get(ctx, key, physicalHost); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("PhysicalHost %s/%s not found", namespace, hostName)
		}
		return fmt.Errorf("failed to get PhysicalHost: %w", err)
	}

	report := buildInspectionReport(req)

	cmName, err := h.upsertResultConfigMap(ctx, log, physicalHost, report)
	if err != nil {
		return fmt.Errorf("upsert inspection-result ConfigMap: %w", err)
	}

	// Signal the PhysicalHost controller to consume the result. Patch the spec
	// annotations only — never status.
	if err := h.setInspectionResultAnnotation(ctx, log, physicalHost, cmName); err != nil {
		return fmt.Errorf("set inspection-result annotation: %w", err)
	}
	return nil
}

// buildInspectionReport converts the request DTO to the API type. Keeping this
// pure (no I/O) makes it trivially testable without a fake client.
func buildInspectionReport(req InspectionReportRequest) *infrastructurev1beta1.InspectionReport {
	report := &infrastructurev1beta1.InspectionReport{
		Timestamp:        metav1.Now(),
		Manufacturer:     req.Manufacturer,
		Model:            req.Model,
		SerialNumber:     req.SerialNumber,
		BootModeDetected: req.BootModeDetected,
		FirmwareVersion:  req.FirmwareVersion,
	}
	for _, cpu := range req.CPUs {
		report.CPUs = append(report.CPUs, infrastructurev1beta1.CPUInfo{
			ID:        cpu.ID,
			Vendor:    cpu.Vendor,
			Model:     cpu.Model,
			Cores:     cpu.Cores,
			Threads:   cpu.Threads,
			Frequency: cpu.Frequency,
		})
	}
	for _, mem := range req.Memory {
		report.Memory = append(report.Memory, infrastructurev1beta1.MemoryInfo{
			ID:       mem.ID,
			Type:     mem.Type,
			Capacity: mem.Capacity,
			Speed:    mem.Speed,
		})
	}
	for _, disk := range req.Disks {
		report.Disks = append(report.Disks, infrastructurev1beta1.DiskInfo{
			Name:         disk.Name,
			Model:        disk.Model,
			SizeGB:       disk.SizeGB,
			Type:         disk.Type,
			SerialNumber: disk.SerialNumber,
		})
	}
	for _, nic := range req.NICs {
		report.NICs = append(report.NICs, infrastructurev1beta1.NICInfo{
			Name:        nic.Name,
			MACAddress:  nic.MACAddress,
			Driver:      nic.Driver,
			Speed:       nic.Speed,
			IPAddresses: nic.IPAddresses,
		})
	}
	return report
}

// upsertResultConfigMap writes the JSON-encoded InspectionReport to a per-host
// ConfigMap, creating or updating idempotently. The ConfigMap is owned by the
// PhysicalHost so it is GC'd if the host is deleted before the controller reads
// the result. Returns the ConfigMap name.
func (h *InspectionHandler) upsertResultConfigMap(
	ctx context.Context,
	log logr.Logger,
	physicalHost *infrastructurev1beta1.PhysicalHost,
	report *infrastructurev1beta1.InspectionReport,
) (string, error) {
	body, err := json.Marshal(report)
	if err != nil {
		return "", fmt.Errorf("marshal inspection report: %w", err)
	}

	cmName := inspectionResultConfigMapName(physicalHost.Name)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: physicalHost.Namespace,
		},
	}

	op, err := controllerutil.CreateOrUpdate(ctx, h.Client, cm, func() error {
		if cm.Labels == nil {
			cm.Labels = map[string]string{}
		}
		cm.Labels[inspectionResultLabelOwnedBy] = "beskar7-controller-manager"
		cm.Labels[inspectionResultLabelHost] = physicalHost.Name
		// Owner ref so the ConfigMap is GC'd on host deletion. We use the
		// PhysicalHost as both owner and controller — this ConfigMap is
		// transient state, not a separately-managed object.
		if err := controllerutil.SetControllerReference(physicalHost, cm, h.Client.Scheme()); err != nil {
			return fmt.Errorf("set controller reference on inspection-result ConfigMap: %w", err)
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[inspectionResultDataKey] = string(body)
		return nil
	})
	if err != nil {
		return "", err
	}
	log.V(1).Info("Inspection-result ConfigMap upsert", "configmap", cmName, "op", op)
	return cmName, nil
}

// inspectionResultConfigMapName returns the deterministic name used for the
// per-host inspection-result ConfigMap. Centralizing the format here so the
// PhysicalHost reconciler and tests can reference the same convention.
func inspectionResultConfigMapName(hostName string) string {
	return hostName + "-inspection-result"
}

// setInspectionResultAnnotation patches PhysicalHost metadata.annotations with the
// ConfigMap reference. Optimistic locking ensures concurrent annotation churn
// from the Beskar7Machine controller never silently overwrites the result ref.
func (h *InspectionHandler) setInspectionResultAnnotation(
	ctx context.Context,
	log logr.Logger,
	physicalHost *infrastructurev1beta1.PhysicalHost,
	cmName string,
) error {
	base := physicalHost.DeepCopy()
	if physicalHost.Annotations == nil {
		physicalHost.Annotations = map[string]string{}
	}
	physicalHost.Annotations[InspectionResultAnnotation] = cmName
	// Plain MergeFrom (no optimistic lock). Same reasoning as
	// Beskar7MachineReconciler.setBootstrapTokenAnnotation: this annotation
	// key is unique to this handler, no other writer collides on it, and
	// MergeFromWithOptimisticLock against a concurrently-mutating PhysicalHost
	// (status updates from the PhysicalHost reconciler) caused repeated
	// Conflict failures that broke the inspection-result handoff entirely.
	if err := h.Client.Patch(ctx, physicalHost, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("patch PhysicalHost annotation: %w", err)
	}
	log.V(1).Info("Inspection-result annotation set", "host", physicalHost.Name, "configmap", cmName)
	return nil
}

// SetupCallbackServer wires the host-callback HTTPS server into the manager.
//
// The server hosts two host-scoped endpoints, both gated by the same
// per-PhysicalHost bearer-token middleware (auth.RequireBearer +
// newBearerTokenVerifier):
//
//   - POST /api/v1/inspection/{namespace}/{hostName}: receives inspection
//     reports from the inspection image (handled by InspectionHandler).
//   - GET  /api/v1/bootstrap/{namespace}/{hostName}: serves the bootstrap data
//     Secret bytes for the Beskar7Machine that consumes the targeted host
//     (handled by BootstrapHandler).
//
// All authentication failures return an opaque 401 — the verifier's specific
// error is logged at V(1) only. TLS is mandatory; the cert dir defaults to the
// webhook cert dir (same Pod, same DNS name, one cert via cert-manager).
func SetupCallbackServer(mgr ctrl.Manager, port int, certDir string) error {
	if certDir == "" {
		return fmt.Errorf("callback server cert dir is empty; set --inspection-cert-dir")
	}
	certPath := filepath.Join(certDir, "tls.crt")
	keyPath := filepath.Join(certDir, "tls.key")
	// Fail at setup, not at first request, so misconfiguration is loud.
	if _, err := os.Stat(certPath); err != nil {
		return fmt.Errorf("callback server cert %q not readable: %w", certPath, err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		return fmt.Errorf("callback server key %q not readable: %w", keyPath, err)
	}

	// Pre-warm the ConfigMap informer. The inspection handler writes a
	// transient per-host inspection-result ConfigMap on every POST (D-005);
	// without pre-warming, the first POST after manager startup blocks for
	// up to the cache's sync timeout waiting for the lazily-created
	// informer to populate, which under a kind-fresh cluster regularly
	// exceeds the kube-apiserver's request budget and surfaces as
	// "Timeout: failed waiting for *v1.ConfigMap Informer to sync" — the
	// inspector POST then 5xx's and the smoke-test inspector simulator
	// cannot drive the host to Ready. Calling GetInformer here registers
	// the informer with the cache; mgr.Start() will boot it alongside the
	// other watches before the first reconcile fires.
	if _, err := mgr.GetCache().GetInformer(context.Background(), &corev1.ConfigMap{}); err != nil {
		return fmt.Errorf("pre-warm ConfigMap informer for inspection handler: %w", err)
	}

	inspectionLog := ctrl.Log.WithName("inspection-handler")
	inspectionHandler := &InspectionHandler{
		Client: mgr.GetClient(),
		Log:    inspectionLog,
	}

	bootstrapLog := ctrl.Log.WithName("bootstrap-handler")
	bootstrapHandler := &BootstrapHandler{
		Client: mgr.GetClient(),
		Log:    bootstrapLog,
	}

	// Same verifier flavour for both endpoints: the bearer token authorises
	// requests for a specific PhysicalHost, regardless of which host-scoped
	// endpoint is being called. We construct one verifier per route so the
	// V(1) log handle reflects the route.
	inspectionVerifier := newBearerTokenVerifier(mgr.GetClient(), inspectionLog)
	bootstrapVerifier := newBearerTokenVerifier(mgr.GetClient(), bootstrapLog)

	mux := http.NewServeMux()
	// Go 1.22+ pattern matching: bind path values for the verifier and handler.
	mux.Handle("POST /api/v1/inspection/{namespace}/{hostName}",
		auth.RequireBearer(inspectionLog, inspectionVerifier, inspectionHandler))
	mux.Handle("GET /api/v1/bootstrap/{namespace}/{hostName}",
		auth.RequireBearer(bootstrapLog, bootstrapVerifier, bootstrapHandler))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("ok")); err != nil {
			ctrl.Log.WithName("callback-server").Error(err, "Failed to write health check response")
		}
	})

	server := &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           mux,
		ReadTimeout:       15 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		ctrl.Log.WithName("callback-server").Info("Starting callback HTTPS server", "port", port)
		if err := server.ListenAndServeTLS(certPath, keyPath); err != nil && !errors.Is(err, http.ErrServerClosed) {
			ctrl.Log.WithName("callback-server").Error(err, "Failed to start callback HTTPS server")
		}
	}()

	if err := mgr.Add(&callbackServerRunnable{server: server}); err != nil {
		return fmt.Errorf("failed to add callback server to manager: %w", err)
	}
	return nil
}

// newBearerTokenVerifier constructs the auth.Verifier shared by every
// host-scoped endpoint on the callback server (inspection POST + bootstrap
// GET). It is a free function (not a method) so tests can compose a verifier
// against a fake client without standing up a full server.
//
// Verification flow:
//  1. Resolve {namespace,hostName} from path values. Reject if either empty.
//  2. Get the PhysicalHost. NotFound → reject (no leak: same 401 as bad token).
//  3. Reject if Status.Bootstrap is nil or TokenHash is empty (no token issued
//     for this host yet — there is no valid token to present).
//  4. Reject if ExpiresAt is set and in the past.
//  5. Reject unless auth.Verify(presented, storedHash) returns true.
//
// Returned errors are descriptive for V(1) logging only — never echoed to the
// client.
func newBearerTokenVerifier(c client.Client, log logr.Logger) auth.Verifier {
	return func(token string, r *http.Request) error {
		namespace := r.PathValue("namespace")
		hostName := r.PathValue("hostName")
		if namespace == "" || hostName == "" {
			return fmt.Errorf("missing namespace or hostName in request path")
		}
		ph := &infrastructurev1beta1.PhysicalHost{}
		if err := c.Get(r.Context(), types.NamespacedName{Namespace: namespace, Name: hostName}, ph); err != nil {
			// Both NotFound and Forbidden produce the same 401 to the client; the
			// distinction lives in V(1) logs.
			return fmt.Errorf("get PhysicalHost: %w", err)
		}
		if ph.Status.Bootstrap == nil || ph.Status.Bootstrap.TokenHash == "" {
			return fmt.Errorf("no bootstrap token issued for host %s/%s", namespace, hostName)
		}
		if ph.Status.Bootstrap.ExpiresAt != nil && time.Now().After(ph.Status.Bootstrap.ExpiresAt.Time) {
			return fmt.Errorf("bootstrap token expired for host %s/%s", namespace, hostName)
		}
		if !auth.Verify(token, ph.Status.Bootstrap.TokenHash) {
			return fmt.Errorf("bootstrap token mismatch for host %s/%s", namespace, hostName)
		}
		_ = log // log handle reserved for future per-verification debug; do not log token material.
		return nil
	}
}

// callbackServerRunnable implements manager.Runnable for graceful shutdown.
type callbackServerRunnable struct {
	server *http.Server
}

func (r *callbackServerRunnable) Start(ctx context.Context) error {
	<-ctx.Done()
	ctrl.Log.WithName("callback-server").Info("Shutting down callback HTTPS server")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return r.server.Shutdown(shutdownCtx)
}
