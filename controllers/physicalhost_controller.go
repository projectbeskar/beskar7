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
	"fmt"
	"time"

	"github.com/go-logr/logr"
	infrastructurev1beta1 "github.com/projectbeskar/beskar7/api/v1beta1"
	internalmetrics "github.com/projectbeskar/beskar7/internal/metrics"
	internalredfish "github.com/projectbeskar/beskar7/internal/redfish"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	conditions "sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	// PhysicalHostFinalizer allows PhysicalHostReconciler to clean up resources before removal
	PhysicalHostFinalizer = "physicalhost.infrastructure.cluster.x-k8s.io"

	// InspectionRequestAnnotation is set by Beskar7Machine to signal an inspection intent.
	// The PhysicalHost controller reads it and drives InspectionPhase / State accordingly.
	// Valid values: "inspect", "timeout".
	InspectionRequestAnnotation = "infrastructure.cluster.x-k8s.io/inspection-request"
)

// PhysicalHostReconciler reconciles a PhysicalHost object.
// Simplified for power management only - provisioning happens via iPXE + inspection.
type PhysicalHostReconciler struct {
	client.Client
	Log                  logr.Logger
	Scheme               *runtime.Scheme
	Recorder             record.EventRecorder
	RedfishClientFactory internalredfish.RedfishClientFactory
}

// NewPhysicalHostReconciler creates a new PhysicalHostReconciler
func NewPhysicalHostReconciler(
	c client.Client,
	scheme *runtime.Scheme,
	redfishFactory internalredfish.RedfishClientFactory,
	logger logr.Logger,
	recorder record.EventRecorder,
) *PhysicalHostReconciler {
	return &PhysicalHostReconciler{
		Client:               c,
		Log:                  logger,
		Scheme:               scheme,
		Recorder:             recorder,
		RedfishClientFactory: redfishFactory,
	}
}

//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=physicalhosts,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=physicalhosts/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=physicalhosts/finalizers,verbs=update
// Secret access is read-only and restricted to BMC credential lookup
// (see getRedfishCredentials). list/watch are required because
// SetupWithManager registers a .Watches(&corev1.Secret{}, ...) informer
// to trigger reconciles on credential rotation; controller-runtime's
// cached client backs that informer with a list+watch on the cluster.
// SEC-2 (D-007): the cluster-wide list/watch on Secret is the residual
// scope after PR-7. Eliminating it requires either dropping the watch
// or replacing it with a label-selected partial cache (v0.5 follow-up).
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// ConfigMaps are only ever fetched by name (the inspection-result
// ConfigMap referenced from the InspectionResultAnnotation) and
// upserted/deleted by the inspection handler. The informer also needs to
// watch ConfigMaps so the controller-runtime cache can serve cached Gets
// from the InspectionHandler's CreateOrUpdate path — without list+watch
// the reflector loops on "configmaps is forbidden" and the first POST
// times out waiting for the informer to sync. The previous SEC-2 / D-007
// note that omitted these is superseded by the inspection-handler design.
//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile handles PhysicalHost reconciliation.
// Simplified workflow: Connect via Redfish → Verify connection → Report ready.
func (r *PhysicalHostReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, reterr error) {
	logger := r.Log.WithValues("physicalhost", req.NamespacedName)
	logger.Info("Starting reconciliation")

	// Fetch the PhysicalHost instance
	physicalHost := &infrastructurev1beta1.PhysicalHost{}
	if err := r.Get(ctx, req.NamespacedName, physicalHost); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("PhysicalHost resource not found, ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Unable to fetch PhysicalHost")
		return ctrl.Result{}, err
	}

	// Recompute state-gauge and availability metrics on every reconcile so the
	// gauges stay current even when reconcile short-circuits (deletion, finalizer
	// add, pause). A List error here is non-fatal — the metric is best-effort.
	r.recomputePhysicalHostMetrics(ctx, logger, req.Namespace)

	// Respect the cluster.x-k8s.io/paused annotation. PhysicalHost is standalone
	// inventory with no owner Cluster, so only the resource-level annotation
	// applies (there is no cluster-pause to inherit). Returning before the patch
	// helper is set up means a paused host is left completely untouched — no
	// finalizer add, no Redfish I/O, no status write. Reconciliation resumes on
	// the next event after the annotation is removed.
	if isPaused(physicalHost) {
		logger.Info("PhysicalHost reconciliation is paused")
		return ctrl.Result{}, nil
	}

	// Initialize patch helper. A single deferred Patch replaces both r.Update (finalizer)
	// and r.Status().Update calls — controller-runtime merges spec and status in one round-trip.
	patchHelper, err := patch.NewHelper(physicalHost, r.Client)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to init patch helper for PhysicalHost: %w", err)
	}
	defer func() {
		if perr := patchHelper.Patch(ctx, physicalHost,
			patch.WithStatusObservedGeneration{},
		); perr != nil {
			logger.Error(perr, "failed to patch PhysicalHost")
			if reterr == nil {
				reterr = perr
			}
		}
		logger.Info("Finished reconciliation")
	}()

	// Handle deletion
	if !physicalHost.ObjectMeta.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, logger, physicalHost)
	}

	// Add finalizer if not present; the deferred patch persists it.
	if controllerutil.AddFinalizer(physicalHost, PhysicalHostFinalizer) {
		logger.Info("Adding finalizer")
		return ctrl.Result{Requeue: true}, nil
	}

	// Reconcile normal operation
	return r.reconcileNormal(ctx, logger, physicalHost)
}

// reconcileNormal handles normal (non-deletion) reconciliation.
func (r *PhysicalHostReconciler) reconcileNormal(ctx context.Context, logger logr.Logger, physicalHost *infrastructurev1beta1.PhysicalHost) (ctrl.Result, error) {
	logger.Info("Reconciling PhysicalHost", "currentState", physicalHost.Status.State)

	// Get Redfish credentials
	username, password, err := r.getRedfishCredentials(ctx, physicalHost)
	if err != nil {
		logger.Error(err, "Failed to get Redfish credentials")
		r.updateStatus(physicalHost, infrastructurev1beta1.StateError, false, err.Error())
		conditions.MarkFalse(physicalHost, infrastructurev1beta1.RedfishConnectionReadyCondition,
			infrastructurev1beta1.MissingCredentialsReason, clusterv1.ConditionSeverityError,
			"Failed to retrieve credentials: %v", err)
		internalmetrics.RecordError("physicalhost", physicalHost.Namespace, internalmetrics.ErrorTypeConnection)
		// Return the error without an explicit RequeueAfter so the workqueue's
		// exponential rate-limiter (configured in SetupWithManager) governs the
		// retry interval. A fixed 1-minute requeue would override the rate-limiter
		// and ping a persistently-misconfigured host every 60s indefinitely.
		return ctrl.Result{}, err
	}

	// Determine insecure setting
	insecure := false
	if physicalHost.Spec.RedfishConnection.InsecureSkipVerify != nil {
		insecure = *physicalHost.Spec.RedfishConnection.InsecureSkipVerify
	}

	// Reject the (insecure=true, caBundleSecretRef!=nil) combination terminally.
	// There is no PhysicalHost validating webhook, so this is the gate; we set a
	// clear condition + ErrorMessage and stop reconciling rather than silently
	// picking one side of the conflict. Returning a non-error result with a
	// long requeue avoids hot-looping on a misconfigured spec.
	if err := validateRedfishTLSCombination(insecure, physicalHost.Spec.RedfishConnection.CABundleSecretRef); err != nil {
		logger.Error(err, "Invalid Redfish TLS configuration")
		r.updateStatus(physicalHost, infrastructurev1beta1.StateError, false, err.Error())
		conditions.MarkFalse(physicalHost, infrastructurev1beta1.RedfishConnectionReadyCondition,
			infrastructurev1beta1.InsecureCABundleConflictReason, clusterv1.ConditionSeverityError,
			"%s", err.Error())
		internalmetrics.RecordError("physicalhost", physicalHost.Namespace, internalmetrics.ErrorTypeValidation)
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
	}

	// Fetch optional CA bundle (returns nil bytes when CABundleSecretRef is unset).
	caBundle, err := fetchRedfishCABundle(ctx, r.Client, physicalHost)
	if err != nil {
		logger.Error(err, "Failed to fetch Redfish CA bundle")
		r.updateStatus(physicalHost, infrastructurev1beta1.StateError, false, err.Error())
		conditions.MarkFalse(physicalHost, infrastructurev1beta1.RedfishConnectionReadyCondition,
			infrastructurev1beta1.CABundleFetchFailedReason, clusterv1.ConditionSeverityError,
			"%s", err.Error())
		internalmetrics.RecordError("physicalhost", physicalHost.Namespace, internalmetrics.ErrorTypeConnection)
		// Workqueue exponential backoff via SetupWithManager handles the retry cadence.
		return ctrl.Result{}, err
	}

	// Create Redfish client
	rfClient, err := r.RedfishClientFactory(ctx,
		physicalHost.Spec.RedfishConnection.Address,
		username,
		password,
		insecure,
		caBundle,
	)
	if err != nil {
		logger.Error(err, "Failed to create Redfish client")
		r.updateStatus(physicalHost, infrastructurev1beta1.StateError, false, fmt.Sprintf("Redfish connection failed: %v", err))
		conditions.MarkFalse(physicalHost, infrastructurev1beta1.RedfishConnectionReadyCondition,
			infrastructurev1beta1.RedfishConnectionFailedReason, clusterv1.ConditionSeverityError,
			"Connection failed: %v", err)
		internalmetrics.RecordRedfishConnection(physicalHost.Namespace, internalmetrics.ProvisioningOutcomeFailed, internalmetrics.ErrorTypeConnection)
		internalmetrics.RecordError("physicalhost", physicalHost.Namespace, internalmetrics.ErrorTypeConnection)
		// Workqueue exponential backoff via SetupWithManager handles the retry cadence.
		return ctrl.Result{}, err
	}
	defer rfClient.Close(ctx)

	// Redfish client created successfully — count the connection.
	internalmetrics.RecordRedfishConnection(physicalHost.Namespace, internalmetrics.ProvisioningOutcomeSuccess, internalmetrics.ErrorTypeUnknown)

	// Get system information
	sysInfo, err := rfClient.GetSystemInfo(ctx)
	if err != nil {
		logger.Error(err, "Failed to get system info from Redfish")
		r.updateStatus(physicalHost, infrastructurev1beta1.StateError, false, fmt.Sprintf("Failed to query system: %v", err))
		conditions.MarkFalse(physicalHost, infrastructurev1beta1.RedfishConnectionReadyCondition,
			infrastructurev1beta1.RedfishQueryFailedReason, clusterv1.ConditionSeverityError,
			"Query failed: %v", err)
		internalmetrics.RecordError("physicalhost", physicalHost.Namespace, internalmetrics.ErrorTypeTransient)
		// Workqueue exponential backoff via SetupWithManager handles the retry cadence.
		return ctrl.Result{}, err
	}

	// Update hardware details
	physicalHost.Status.HardwareDetails = infrastructurev1beta1.HardwareDetails{
		Manufacturer: sysInfo.Manufacturer,
		Model:        sysInfo.Model,
		SerialNumber: sysInfo.SerialNumber,
		Status: infrastructurev1beta1.HardwareStatus{
			Health:       string(sysInfo.Status.Health),
			HealthRollup: string(sysInfo.Status.HealthRollup),
			State:        string(sysInfo.Status.State),
		},
	}

	// Get power state
	powerState, err := rfClient.GetPowerState(ctx)
	if err != nil {
		logger.Error(err, "Failed to get power state")
		// Non-fatal, continue
	} else {
		physicalHost.Status.ObservedPowerState = string(powerState)
		logger.Info("Observed power state", "state", powerState)
	}

	// Detect network addresses
	addresses, err := rfClient.GetNetworkAddresses(ctx)
	if err != nil {
		logger.Error(err, "Failed to get network addresses", "error", err)
		// Non-fatal, continue without addresses
	} else {
		physicalHost.Status.Addresses = internalredfish.ConvertToMachineAddresses(addresses)
		logger.Info("Retrieved network addresses", "count", len(addresses))
	}

	// Connection successful - mark as ready
	conditions.MarkTrue(physicalHost, infrastructurev1beta1.RedfishConnectionReadyCondition)

	// Act on inspection-request annotation set by Beskar7Machine controller.
	// The Beskar7Machine controller writes only to PhysicalHost.Spec.Annotations (a spec
	// write via MergeFrom patch); we read it here and drive the Status transition ourselves,
	// keeping status ownership inside this controller.
	r.applyInspectionRequest(ctx, logger, physicalHost)

	// Consume the bootstrap-url annotation set by the Beskar7Machine controller and
	// persist the value to Status.Bootstrap.URL. This keeps status ownership inside
	// this controller (same pattern as applyInspectionRequest / BUG-1 fix).
	r.applyBootstrapURLAnnotation(logger, physicalHost)

	// Consume the bootstrap-token annotation (PR-5.2 / D-004): the Beskar7Machine
	// controller signals the freshly minted token's hash + lifetime here; we
	// persist them to Status.Bootstrap so the inspection HTTPS handler can verify
	// bearer tokens against the stored hash.
	r.applyBootstrapTokenAnnotation(logger, physicalHost)

	// Consume the inspection-result annotation (PR-5.2 / D-005): the inspection
	// HTTP handler stored the validated InspectionReport on a ConfigMap and
	// pointed at it via this annotation. We persist the report to Status, mark
	// HostInspectedCondition, transition state to Ready, then delete the
	// ConfigMap and clear the annotation so we don't act on it twice.
	r.applyInspectionResultAnnotation(ctx, logger, physicalHost)

	// Determine state based on ConsumerRef
	if physicalHost.Spec.ConsumerRef != nil {
		// Host is claimed
		if physicalHost.Status.State != infrastructurev1beta1.StateInUse &&
			physicalHost.Status.State != infrastructurev1beta1.StateInspecting &&
			physicalHost.Status.State != infrastructurev1beta1.StateReady {
			logger.Info("Host claimed, transitioning to InUse", "consumer", physicalHost.Spec.ConsumerRef.Name)
			r.updateStatus(physicalHost, infrastructurev1beta1.StateInUse, true, "")
		}
	} else {
		// Host is available
		if physicalHost.Status.State != infrastructurev1beta1.StateAvailable {
			logger.Info("Host available, transitioning to Available")
			r.updateStatus(physicalHost, infrastructurev1beta1.StateAvailable, true, "")
			conditions.MarkTrue(physicalHost, infrastructurev1beta1.HostAvailableCondition)
		}
	}

	logger.Info("Reconciliation complete", "state", physicalHost.Status.State, "ready", physicalHost.Status.Ready)
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

// applyInspectionRequest reads the InspectionRequestAnnotation and, when present, drives
// Status.State and Status.InspectionPhase accordingly, then removes the annotation so it
// is not acted on twice.
func (r *PhysicalHostReconciler) applyInspectionRequest(ctx context.Context, logger logr.Logger, physicalHost *infrastructurev1beta1.PhysicalHost) {
	ann := physicalHost.Annotations[InspectionRequestAnnotation]
	if ann == "" {
		return
	}

	switch ann {
	case "inspect":
		logger.Info("Applying inspection-request annotation: starting inspection")
		if physicalHost.Status.InspectionTimestamp == nil {
			t := metav1.Now()
			physicalHost.Status.InspectionTimestamp = &t
		}
		physicalHost.Status.State = infrastructurev1beta1.StateInspecting
		physicalHost.Status.InspectionPhase = infrastructurev1beta1.InspectionPhaseBooting

	case "inspect-complete":
		logger.Info("Applying inspection-request annotation: marking inspection complete")
		physicalHost.Status.State = infrastructurev1beta1.StateReady
		physicalHost.Status.InspectionPhase = infrastructurev1beta1.InspectionPhaseComplete
		conditions.MarkTrue(physicalHost, infrastructurev1beta1.HostInspectedCondition)

	case "timeout":
		logger.Info("Applying inspection-request annotation: recording inspection timeout")
		physicalHost.Status.InspectionPhase = infrastructurev1beta1.InspectionPhaseTimeout
		physicalHost.Status.State = infrastructurev1beta1.StateError
		physicalHost.Status.ErrorMessage = "Inspection timed out"

	default:
		logger.Info("Unknown inspection-request annotation value, ignoring", "value", ann)
	}

	// Remove the annotation so we don't act on it again. The deferred patch persists this.
	delete(physicalHost.Annotations, InspectionRequestAnnotation)
}

// applyBootstrapURLAnnotation reads the BootstrapURLAnnotation and, when present,
// persists the URL to Status.Bootstrap.URL and removes the annotation so it is
// not acted on again. Mirrors the pattern of applyInspectionRequest.
func (r *PhysicalHostReconciler) applyBootstrapURLAnnotation(logger logr.Logger, physicalHost *infrastructurev1beta1.PhysicalHost) {
	url := physicalHost.Annotations[BootstrapURLAnnotation]
	if url == "" {
		return
	}

	if physicalHost.Status.Bootstrap == nil {
		physicalHost.Status.Bootstrap = &infrastructurev1beta1.BootstrapStatus{}
	}
	physicalHost.Status.Bootstrap.URL = url
	logger.Info("Applied bootstrap-url annotation to Status.Bootstrap.URL", "host", physicalHost.Name)

	// Remove the annotation so we don't act on it again. The deferred patch persists this.
	delete(physicalHost.Annotations, BootstrapURLAnnotation)
}

// applyBootstrapTokenAnnotation reads the BootstrapTokenAnnotation, JSON-decodes
// the {hash, issuedAt, expiresAt} payload, and persists those values to
// Status.Bootstrap. The plaintext token is delivered out-of-band via a Secret —
// the annotation only carries the hash + lifetime.
//
// Same idempotent "annotation in, status out, annotation cleared" pattern as
// applyBootstrapURLAnnotation. Malformed JSON is logged and the annotation is
// left in place so the next reconcile (or operator) can investigate; clearing
// would silently drop a token-state signal.
func (r *PhysicalHostReconciler) applyBootstrapTokenAnnotation(logger logr.Logger, physicalHost *infrastructurev1beta1.PhysicalHost) {
	raw := physicalHost.Annotations[BootstrapTokenAnnotation]
	if raw == "" {
		return
	}
	var value BootstrapTokenAnnotationValue
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		logger.Error(err, "Failed to decode bootstrap-token annotation; leaving in place for investigation",
			"host", physicalHost.Name)
		return
	}
	if value.Hash == "" {
		logger.Info("bootstrap-token annotation has empty hash; ignoring", "host", physicalHost.Name)
		delete(physicalHost.Annotations, BootstrapTokenAnnotation)
		return
	}

	if physicalHost.Status.Bootstrap == nil {
		physicalHost.Status.Bootstrap = &infrastructurev1beta1.BootstrapStatus{}
	}
	// Copy the hash (safe to log later — see Status.Bootstrap.TokenHash docstring).
	physicalHost.Status.Bootstrap.TokenHash = value.Hash
	issuedAt := value.IssuedAt
	expiresAt := value.ExpiresAt
	physicalHost.Status.Bootstrap.IssuedAt = &issuedAt
	physicalHost.Status.Bootstrap.ExpiresAt = &expiresAt
	logger.Info("Applied bootstrap-token annotation to Status.Bootstrap", "host", physicalHost.Name)

	delete(physicalHost.Annotations, BootstrapTokenAnnotation)
}

// applyInspectionResultAnnotation reads the InspectionResultAnnotation, fetches
// the referenced ConfigMap, decodes the JSON-encoded InspectionReport, and
// persists it to Status. Then transitions Status.InspectionPhase to Complete,
// marks HostInspectedCondition true, and best-effort deletes the ConfigMap and
// clears the annotation so the result is consumed exactly once (D-005).
//
// Errors fetching/decoding the ConfigMap are logged but not returned: the
// reconcile's deferred patch still proceeds. The annotation is cleared only
// after a successful read, so a missing or malformed ConfigMap doesn't strand
// the state machine — the inspector can re-POST and replace it.
func (r *PhysicalHostReconciler) applyInspectionResultAnnotation(ctx context.Context, logger logr.Logger, physicalHost *infrastructurev1beta1.PhysicalHost) {
	cmName := physicalHost.Annotations[InspectionResultAnnotation]
	if cmName == "" {
		return
	}

	cm := &corev1.ConfigMap{}
	cmKey := types.NamespacedName{Namespace: physicalHost.Namespace, Name: cmName}
	if err := r.Get(ctx, cmKey, cm); err != nil {
		if apierrors.IsNotFound(err) {
			// The handler created an annotation but the ConfigMap is gone (deleted
			// by GC or out-of-band cleanup). Clear the annotation so we stop
			// trying to consume it; the inspector can re-POST.
			logger.Info("Inspection-result ConfigMap not found; clearing annotation",
				"configmap", cmName)
			delete(physicalHost.Annotations, InspectionResultAnnotation)
			return
		}
		logger.Error(err, "Failed to fetch inspection-result ConfigMap; will retry on next reconcile",
			"configmap", cmName)
		return
	}

	raw, ok := cm.Data[inspectionResultDataKey]
	if !ok {
		logger.Info("Inspection-result ConfigMap missing report.json; ignoring", "configmap", cmName)
		// Drop the bad CM and clear the annotation so the state machine is unblocked.
		_ = r.Delete(ctx, cm)
		delete(physicalHost.Annotations, InspectionResultAnnotation)
		return
	}

	report := &infrastructurev1beta1.InspectionReport{}
	if err := json.Unmarshal([]byte(raw), report); err != nil {
		logger.Error(err, "Failed to decode inspection report from ConfigMap; deleting bad ConfigMap",
			"configmap", cmName)
		_ = r.Delete(ctx, cm)
		delete(physicalHost.Annotations, InspectionResultAnnotation)
		return
	}

	// Persist to Status. This is the SOLE place Status.InspectionReport is
	// written by the controller — D-005 invariant.
	physicalHost.Status.InspectionReport = report
	physicalHost.Status.InspectionPhase = infrastructurev1beta1.InspectionPhaseComplete
	conditions.MarkTrue(physicalHost, infrastructurev1beta1.HostInspectedCondition)
	logger.Info("Applied inspection report to Status.InspectionReport", "host", physicalHost.Name)

	// One-shot consumption: delete the ConfigMap and clear the annotation.
	// Errors deleting the ConfigMap are logged at V(1); the OwnerReference
	// already guarantees GC if the host is later deleted.
	if err := r.Delete(ctx, cm); err != nil && !apierrors.IsNotFound(err) {
		logger.V(1).Info("Failed to delete inspection-result ConfigMap (will be retried by GC)",
			"configmap", cmName, "err", err.Error())
	}
	delete(physicalHost.Annotations, InspectionResultAnnotation)
}

// reconcileDelete handles PhysicalHost deletion.
func (r *PhysicalHostReconciler) reconcileDelete(ctx context.Context, logger logr.Logger, physicalHost *infrastructurev1beta1.PhysicalHost) (ctrl.Result, error) {
	logger.Info("Reconciling PhysicalHost deletion")

	// If still claimed, log warning but allow deletion
	if physicalHost.Spec.ConsumerRef != nil {
		logger.Info("Warning: Deleting PhysicalHost that is still claimed", "consumer", physicalHost.Spec.ConsumerRef.Name)
		r.Recorder.Event(physicalHost, corev1.EventTypeWarning, "DeletingClaimedHost",
			fmt.Sprintf("Deleting host that is still claimed by %s", physicalHost.Spec.ConsumerRef.Name))
	}

	// Remove finalizer; the deferred patch persists this.
	if controllerutil.RemoveFinalizer(physicalHost, PhysicalHostFinalizer) {
		logger.Info("Finalizer removed")
	}

	return ctrl.Result{}, nil
}

// getRedfishCredentials retrieves Redfish credentials from the referenced secret.
func (r *PhysicalHostReconciler) getRedfishCredentials(ctx context.Context, physicalHost *infrastructurev1beta1.PhysicalHost) (string, string, error) {
	secretName := physicalHost.Spec.RedfishConnection.CredentialsSecretRef
	if secretName == "" {
		return "", "", fmt.Errorf("credentials secret reference is empty")
	}

	secret := &corev1.Secret{}
	secretKey := types.NamespacedName{
		Namespace: physicalHost.Namespace,
		Name:      secretName,
	}

	if err := r.Get(ctx, secretKey, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", "", fmt.Errorf("credentials secret %q not found", secretName)
		}
		return "", "", fmt.Errorf("failed to get credentials secret: %w", err)
	}

	username, ok := secret.Data["username"]
	if !ok {
		return "", "", fmt.Errorf("username not found in secret %q", secretName)
	}

	password, ok := secret.Data["password"]
	if !ok {
		return "", "", fmt.Errorf("password not found in secret %q", secretName)
	}

	return string(username), string(password), nil
}

// updateStatus is a helper to update PhysicalHost status fields.
func (r *PhysicalHostReconciler) updateStatus(ph *infrastructurev1beta1.PhysicalHost, state string, ready bool, errorMsg string) {
	ph.Status.State = state
	ph.Status.Ready = ready
	ph.Status.ErrorMessage = errorMsg
}

// recomputePhysicalHostMetrics lists all PhysicalHosts in the given namespace and
// emits state-gauge + availability metrics in a single List call. Called at the
// top of each Reconcile so metrics stay current even when the reconcile short-circuits.
// Errors are logged and swallowed — a metric failure must not affect reconcile correctness.
func (r *PhysicalHostReconciler) recomputePhysicalHostMetrics(ctx context.Context, logger logr.Logger, namespace string) {
	list := &infrastructurev1beta1.PhysicalHostList{}
	if err := r.List(ctx, list, client.InNamespace(namespace)); err != nil {
		logger.V(1).Info("Failed to list PhysicalHosts for metric recompute; skipping", "err", err.Error())
		return
	}
	counts := make(map[string]int, len(list.Items))
	availableCount := 0
	for _, h := range list.Items {
		counts[h.Status.State]++
		if h.Status.State == infrastructurev1beta1.StateAvailable {
			availableCount++
		}
	}
	internalmetrics.UpdatePhysicalHostStateCounts(namespace, counts)
	internalmetrics.UpdatePhysicalHostAvailability(namespace, availableCount, len(list.Items))
}

// defaultFactory sets RedfishClientFactory to the real gofish-backed constructor
// when the caller left it nil. Separated from SetupWithManager to keep the defaulting
// logic directly testable without spinning up a full controller-runtime Manager.
func (r *PhysicalHostReconciler) defaultFactory() error {
	if r.RedfishClientFactory == nil {
		r.RedfishClientFactory = internalredfish.NewClient
	}
	if r.RedfishClientFactory == nil {
		// Should be unreachable, but guard against a future programming error
		// that nilifies the field after this call.
		return fmt.Errorf("PhysicalHostReconciler: RedfishClientFactory is nil after defaulting")
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *PhysicalHostReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := r.defaultFactory(); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrastructurev1beta1.PhysicalHost{}).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.SecretToPhysicalHosts),
		).
		WithOptions(controller.Options{
			// Exponential backoff for transient Redfish failures. The previous
			// fixed RequeueAfter: 1*time.Minute on every error path meant a
			// persistently-misconfigured BMC was pinged every 60s forever; now
			// a failing host backs off geometrically (5s, 10s, 20s, ... capped
			// at 30m) so a wrong address / wrong creds / unreachable BMC settles
			// into a low-frequency check instead of a hot loop.
			RateLimiter: workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](
				5*time.Second,
				30*time.Minute,
			),
		}).
		Complete(r)
}

// SecretToPhysicalHosts maps Secret changes to PhysicalHost reconcile requests.
func (r *PhysicalHostReconciler) SecretToPhysicalHosts(ctx context.Context, obj client.Object) []reconcile.Request {
	secret, ok := obj.(*corev1.Secret)
	if !ok {
		r.Log.Error(nil, "Expected a Secret but got something else", "object", obj)
		return nil
	}

	// Find all PhysicalHosts in the same namespace that reference this secret
	physicalHostList := &infrastructurev1beta1.PhysicalHostList{}
	if err := r.List(ctx, physicalHostList, client.InNamespace(secret.Namespace)); err != nil {
		r.Log.Error(err, "Failed to list PhysicalHosts for Secret watch", "secret", secret.Name)
		return nil
	}

	var requests []reconcile.Request
	for _, ph := range physicalHostList.Items {
		if ph.Spec.RedfishConnection.CredentialsSecretRef == secret.Name {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Namespace: ph.Namespace,
					Name:      ph.Name,
				},
			})
		}
	}

	return requests
}
