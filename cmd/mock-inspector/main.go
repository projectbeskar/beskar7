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

// mock-inspector is a one-shot binary that simulates an iPXE-booted inspection
// image POSTing hardware details to the Beskar7 controller's bootstrap
// callback endpoint. It replaces the inline kubectl-run-curl pod used in
// hack/smoke/run.sh's layer_5_inspection function.
//
// Usage (in-cluster Job):
//
//	mock-inspector --namespace beskar7-smoke --host-name smoke-host-01
//
// The binary exits 0 on a 2xx response and non-zero on any failure.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// inspectionReportRequest is the JSON payload the controller's inspection
// handler expects. It MUST stay in sync with
// controllers/inspection_handler.go's InspectionReportRequest.
//
// Note: namespace and hostName fields exist for backward compat with legacy
// inspectors; the controller derives both from the URL path, not the body.
type inspectionReportRequest struct {
	Manufacturer string     `json:"manufacturer,omitempty"`
	Model        string     `json:"model,omitempty"`
	SerialNumber string     `json:"serialNumber,omitempty"`
	CPUs         []cpuData  `json:"cpus,omitempty"`
	Memory       []memData  `json:"memory,omitempty"`
	Disks        []diskData `json:"disks,omitempty"`
	NICs         []nicData  `json:"nics,omitempty"`
	BootMode     string     `json:"bootModeDetected,omitempty"`
	FirmwareVer  string     `json:"firmwareVersion,omitempty"`
}

type cpuData struct {
	ID        string `json:"id,omitempty"`
	Vendor    string `json:"vendor,omitempty"`
	Model     string `json:"model,omitempty"`
	Cores     int    `json:"cores,omitempty"`
	Threads   int    `json:"threads,omitempty"`
	Frequency string `json:"frequency,omitempty"`
}

type memData struct {
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Capacity string `json:"capacity,omitempty"`
	Speed    string `json:"speed,omitempty"`
}

type diskData struct {
	Name         string `json:"name,omitempty"`
	Model        string `json:"model,omitempty"`
	SizeGB       int    `json:"sizeGB,omitempty"`
	Type         string `json:"type,omitempty"`
	SerialNumber string `json:"serialNumber,omitempty"`
}

type nicData struct {
	Name        string   `json:"name,omitempty"`
	MACAddress  string   `json:"macAddress,omitempty"`
	Driver      string   `json:"driver,omitempty"`
	Speed       string   `json:"speed,omitempty"`
	IPAddresses []string `json:"ipAddresses,omitempty"`
}

// physicalHostGVR is the GroupVersionResource for PhysicalHost CRD lookups via
// the dynamic client. Defined here so it is not re-derived on every call.
var physicalHostGVR = schema.GroupVersionResource{
	Group:    "infrastructure.cluster.x-k8s.io",
	Version:  "v1beta1",
	Resource: "physicalhosts",
}

func main() {
	var (
		namespace          string
		hostName           string
		kubeconfig         string
		waitForBootstrap   time.Duration
		insecureSkipVerify bool
		caBundleFile       string
		connectTimeout     time.Duration
	)

	flag.StringVar(&namespace, "namespace", "", "Namespace containing the PhysicalHost (required).")
	flag.StringVar(&hostName, "host-name", "", "PhysicalHost metadata.name (required).")
	flag.StringVar(&kubeconfig, "kubeconfig", "",
		"Path to kubeconfig file. Empty defaults to in-cluster service-account credentials.")
	flag.DurationVar(&waitForBootstrap, "wait-for-bootstrap", 3*time.Minute,
		"How long to poll for PhysicalHost.Status.Bootstrap.URL + TokenHash to be populated.")
	flag.BoolVar(&insecureSkipVerify, "insecure-skip-verify", true,
		"Skip TLS verification when POSTing to the inspection endpoint. The callback "+
			"server cert typically covers beskar7-webhook-service, not the "+
			"controller-manager service DNS name the bootstrap URL resolves to. "+
			"Mutually exclusive with --ca-bundle-file.")
	flag.StringVar(&caBundleFile, "ca-bundle-file", "",
		"Path to a PEM-encoded CA bundle to trust instead of the system pool. "+
			"Mutually exclusive with --insecure-skip-verify=true.")
	flag.DurationVar(&connectTimeout, "connect-timeout", 30*time.Second,
		"Total HTTP transaction budget for the inspection POST.")
	flag.Parse()

	if namespace == "" {
		log.Fatal("--namespace is required")
	}
	if hostName == "" {
		log.Fatal("--host-name is required")
	}
	if caBundleFile != "" && insecureSkipVerify {
		log.Fatal("--ca-bundle-file and --insecure-skip-verify=true are mutually exclusive")
	}

	// Build the Kubernetes REST config.
	restCfg, err := buildRestConfig(kubeconfig)
	if err != nil {
		log.Fatalf("build kubeconfig: %v", err)
	}

	dynClient, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		log.Fatalf("build dynamic client: %v", err)
	}
	typedClient, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		log.Fatalf("build typed client: %v", err)
	}

	ctx := context.Background()

	// Step 3+4: poll for Bootstrap.{URL,TokenHash} and verify the token.
	bootstrapURL, token, err := waitAndVerifyToken(ctx, dynClient, typedClient, namespace, hostName, waitForBootstrap)
	if err != nil {
		log.Fatalf("bootstrap token verification failed: %v", err)
	}

	// Step 5: derive inspection URL.
	inspectionURL, err := deriveInspectionURL(bootstrapURL)
	if err != nil {
		log.Fatalf("derive inspection URL: %v", err)
	}
	log.Printf("inspection URL: %s", inspectionURL)

	// Step 6: build payload.
	payload := buildPayload(hostName)

	// Step 7: POST.
	httpClient, err := buildHTTPClient(insecureSkipVerify, caBundleFile, connectTimeout)
	if err != nil {
		log.Fatalf("build HTTP client: %v", err)
	}

	if err := postReport(ctx, httpClient, inspectionURL, token, payload); err != nil {
		log.Fatalf("inspection POST failed: %v", err)
	}

	log.Printf("mock-inspector: inspection accepted for host %s/%s", namespace, hostName)

	// Step 8 (contract v4 / D-015): after inspection the controller advances the
	// host to StateDeploying and only sets StateReady/ProviderID once it receives
	// the provisioning-complete callback. The real inspector POSTs /provisioned
	// after writing the OS image + injecting the cloud-config; the mock simulates
	// that here. We wait for StateDeploying first because the provisioned handler
	// only transitions a host that is already Deploying (a callback that races
	// ahead of the inspection→Deploying reconcile would be dropped).
	provisionedURL, err := deriveProvisionedURL(bootstrapURL)
	if err != nil {
		log.Fatalf("derive provisioned URL: %v", err)
	}
	if err := waitForState(ctx, dynClient, namespace, hostName, 2*time.Minute, "Deploying", "Ready"); err != nil {
		log.Fatalf("waiting for StateDeploying before provisioned callback: %v", err)
	}
	if err := postProvisioned(ctx, httpClient, provisionedURL, token); err != nil {
		log.Fatalf("provisioned POST failed: %v", err)
	}

	log.Printf("mock-inspector: provisioned callback accepted for host %s/%s", namespace, hostName)
}

// deriveProvisionedURL swaps "/api/v1/bootstrap/" for "/api/v1/provisioned/" in
// the bootstrap URL (contract v4). Returns an error if the bootstrap path
// segment is absent.
func deriveProvisionedURL(bootstrapURL string) (string, error) {
	const (
		bootstrapSeg   = "/api/v1/bootstrap/"
		provisionedSeg = "/api/v1/provisioned/"
	)
	if !strings.Contains(bootstrapURL, bootstrapSeg) {
		return "", fmt.Errorf("bootstrap URL %q does not contain %q; cannot derive provisioned URL", bootstrapURL, bootstrapSeg)
	}
	return strings.Replace(bootstrapURL, bootstrapSeg, provisionedSeg, 1), nil
}

// waitForState polls PhysicalHost.Status.State until it equals one of wantStates
// or the timeout elapses.
func waitForState(
	ctx context.Context,
	dynClient dynamic.Interface,
	namespace, hostName string,
	timeout time.Duration,
	wantStates ...string,
) error {
	const pollInterval = 3 * time.Second
	want := make(map[string]bool, len(wantStates))
	for _, s := range wantStates {
		want[s] = true
	}
	log.Printf("waiting up to %s for PhysicalHost %s/%s to reach state %v", timeout, namespace, hostName, wantStates)
	deadline := time.Now().Add(timeout)
	for {
		ph, getErr := dynClient.Resource(physicalHostGVR).Namespace(namespace).Get(ctx, hostName, metav1.GetOptions{})
		if getErr == nil {
			if st := nestedString(ph.Object, "status", "state"); want[st] {
				log.Printf("PhysicalHost %s/%s reached state %s", namespace, hostName, st)
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for PhysicalHost %s/%s to reach one of %v", namespace, hostName, wantStates)
		}
		time.Sleep(pollInterval)
	}
}

// postProvisioned POSTs the contract-v4 provisioning-complete callback. The body
// is the fixed advisory JSON the controller treats as advisory-only; the
// authenticated POST itself is the signal. Returns an error on a non-2xx status.
func postProvisioned(ctx context.Context, httpClient *http.Client, url, token string) error {
	body := []byte(`{"status":"provisioned"}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	log.Printf("POSTing provisioned callback to %s (tokenPresent: true)", url)

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("provisioned POST returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	log.Printf("provisioned POST succeeded: status=%d", resp.StatusCode)
	return nil
}

// buildRestConfig returns a *rest.Config from the provided kubeconfig path or,
// if the path is empty, from the in-cluster service-account credentials.
func buildRestConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}
	return cfg, nil
}

// waitAndVerifyToken polls PhysicalHost.Status.Bootstrap.{URL,TokenHash} and
// the per-host bootstrap-token Secret until both are set and the plaintext
// token's sha256 matches the stored hash. Returns the bootstrap URL and the
// plaintext token for use in the inspection POST.
//
// The 6-attempt hash cross-check loop (2 s apart) papers over the window where
// the Secret plaintext can lead Status.Bootstrap.TokenHash by a few seconds
// (BootstrapToken annotation in flight from Beskar7Machine to PhysicalHost
// reconciler). The controller-side fix (bootstrapTokenStillValid) closes the
// main race; this loop makes the runner robust against the first observation.
func waitAndVerifyToken(
	ctx context.Context,
	dynClient dynamic.Interface,
	typedClient kubernetes.Interface,
	namespace, hostName string,
	timeout time.Duration,
) (bootstrapURL, token string, err error) {
	const pollInterval = 3 * time.Second

	log.Printf("waiting up to %s for PhysicalHost %s/%s Bootstrap.URL + TokenHash", timeout, namespace, hostName)

	deadline := time.Now().Add(timeout)
	var storedHash string

	for {
		if time.Now().After(deadline) {
			return "", "", fmt.Errorf("timed out waiting for PhysicalHost %s/%s bootstrap.url and bootstrap.tokenHash to be set", namespace, hostName)
		}
		ph, getErr := dynClient.Resource(physicalHostGVR).Namespace(namespace).Get(ctx, hostName, metav1.GetOptions{})
		if getErr != nil {
			log.Printf("polling PhysicalHost: %v (retrying in %s)", getErr, pollInterval)
			time.Sleep(pollInterval)
			continue
		}
		bootstrapURL = nestedString(ph.Object, "status", "bootstrap", "url")
		storedHash = nestedString(ph.Object, "status", "bootstrap", "tokenHash")
		if bootstrapURL != "" && storedHash != "" {
			break
		}
		time.Sleep(pollInterval)
	}

	hashPrefix := storedHash
	if len(hashPrefix) > 12 {
		hashPrefix = hashPrefix[:12]
	}
	log.Printf("PhysicalHost bootstrap.url set; verifying token hash (prefix: %s...)", hashPrefix)

	// Hash cross-check with short retry (6 attempts, 2 s apart).
	const (
		hashRetries  = 6
		hashInterval = 2 * time.Second
	)
	secretName := hostName + "-bootstrap-token"
	for attempt := 1; attempt <= hashRetries; attempt++ {
		secret, getErr := typedClient.CoreV1().Secrets(namespace).Get(ctx, secretName, metav1.GetOptions{})
		if getErr != nil {
			log.Printf("attempt %d/%d: get Secret %s/%s: %v", attempt, hashRetries, namespace, secretName, getErr)
			if attempt < hashRetries {
				time.Sleep(hashInterval)
			}
			continue
		}

		plaintext, hashOK := extractAndVerifyToken(secret, storedHash)
		if !hashOK {
			log.Printf("attempt %d/%d: token sha256 mismatch (retrying in %s)", attempt, hashRetries, hashInterval)
			// Re-fetch the hash from PhysicalHost in case the annotation just landed.
			ph, getErr := dynClient.Resource(physicalHostGVR).Namespace(namespace).Get(ctx, hostName, metav1.GetOptions{})
			if getErr == nil {
				storedHash = nestedString(ph.Object, "status", "bootstrap", "tokenHash")
			}
			if attempt < hashRetries {
				time.Sleep(hashInterval)
			}
			continue
		}
		token = plaintext
		break
	}

	if token == "" {
		return "", "", fmt.Errorf("token Secret %s/%s never converged with PhysicalHost bootstrap.tokenHash after %d attempts", namespace, secretName, hashRetries)
	}

	return bootstrapURL, token, nil
}

// extractAndVerifyToken reads Data["plaintext-token"] from the Secret, computes
// sha256, and compares to storedHash in constant-time. Returns the plaintext
// and true on match. The plaintext is never logged.
func extractAndVerifyToken(secret *corev1.Secret, storedHash string) (plaintext string, ok bool) {
	raw, exists := secret.Data["plaintext-token"]
	if !exists || len(raw) == 0 {
		return "", false
	}
	plaintext = string(raw)
	computed := tokenSHA256Hex(plaintext)
	if !hashEqual(computed, storedHash) {
		// Log only the first 12 hex chars — not sensitive (cannot reverse to plaintext).
		computedPrefix := computed
		if len(computedPrefix) > 12 {
			computedPrefix = computedPrefix[:12]
		}
		storedPrefix := storedHash
		if len(storedPrefix) > 12 {
			storedPrefix = storedPrefix[:12]
		}
		log.Printf("hash mismatch: computed prefix=%s... stored prefix=%s...", computedPrefix, storedPrefix)
		return "", false
	}
	// Log only the first 12 chars of the matching hash — not the token.
	prefix := computed
	if len(prefix) > 12 {
		prefix = prefix[:12]
	}
	log.Printf("token sha256 verified (prefix: %s...)", prefix)
	return plaintext, true
}

// tokenSHA256Hex returns the lowercase hex encoding of sha256(plaintext). It is
// extracted as a free function so the test can drive it without a live cluster.
func tokenSHA256Hex(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// hashEqual compares two hex-encoded SHA-256 strings. String equality is safe
// here: this is a one-shot CLI tool, not a server; timing side-channels on
// the hash comparison would require physical access to the Job pod's CPU
// performance counters. The internal/auth.Verify function uses
// crypto/subtle.ConstantTimeCompare for the server-side path because that
// endpoint is accessible over the network.
func hashEqual(a, b string) bool {
	return a == b
}

// deriveInspectionURL swaps "/api/v1/bootstrap/" for "/api/v1/inspection/" in
// the bootstrap URL. Returns an error if the bootstrap path segment is absent.
func deriveInspectionURL(bootstrapURL string) (string, error) {
	const (
		bootstrapSeg  = "/api/v1/bootstrap/"
		inspectionSeg = "/api/v1/inspection/"
	)
	if !strings.Contains(bootstrapURL, bootstrapSeg) {
		return "", fmt.Errorf("bootstrap URL %q does not contain %q; cannot derive inspection URL", bootstrapURL, bootstrapSeg)
	}
	return strings.Replace(bootstrapURL, bootstrapSeg, inspectionSeg, 1), nil
}

// buildPayload returns the inspection report to POST. All field values are
// fixed defaults that mirror the payload the bash layer_5_inspection function
// sends; SerialNumber is composed from hostName for traceability in logs.
//
// Future extension point: a --report-file flag could load the payload from
// disk, enabling custom scenarios. Not implemented here to keep scope tight.
func buildPayload(hostName string) inspectionReportRequest {
	return inspectionReportRequest{
		Manufacturer: "MockInspector",
		Model:        "smoke-test",
		SerialNumber: "MOCK-" + strings.ToUpper(hostName),
		BootMode:     "UEFI",
		FirmwareVer:  "1.0.0",
		CPUs: []cpuData{
			{
				ID:        "cpu0",
				Vendor:    "GenuineIntel",
				Model:     "Mock CPU",
				Cores:     4,
				Threads:   8,
				Frequency: "3.0GHz",
			},
		},
		Memory: []memData{
			{
				ID:       "DIMM0",
				Type:     "DDR4",
				Capacity: "16GB",
				Speed:    "3200MHz",
			},
		},
		Disks: []diskData{
			{
				Name:         "sda",
				Model:        "VirtualDisk",
				SizeGB:       100,
				Type:         "SSD",
				SerialNumber: "MOCK-DISK-001",
			},
		},
		NICs: []nicData{
			{
				Name:        "eth0",
				MACAddress:  stableMAC(hostName),
				Driver:      "virtio",
				Speed:       "1Gbps",
				IPAddresses: []string{"10.0.0.42"},
			},
		},
	}
}

// stableMAC derives a deterministic fake MAC address from the host name using
// FNV-32a so the address is stable across runs but unique per host. The OUI
// prefix de:ad:be is locally administered (bit 1 of first octet set).
func stableMAC(hostName string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(hostName))
	n := h.Sum32()
	b0 := byte(n >> 24)
	b1 := byte(n >> 16)
	// Force locally-administered, unicast.
	b0 = (b0 | 0x02) & 0xfe
	return fmt.Sprintf("de:ad:be:%02x:%02x:%02x", b0, b1, byte(n>>8))
}

// buildHTTPClient constructs an *http.Client with the requested TLS policy and
// timeout. When insecure is true the server certificate is not verified (useful
// when the callback URL points at a controller-manager Service whose cert-manager
// certificate covers a different DNS name). When caBundleFile is non-empty a
// custom CA pool is loaded from disk instead of the system pool.
func buildHTTPClient(insecure bool, caBundleFile string, timeout time.Duration) (*http.Client, error) {
	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}

	if insecure {
		// InsecureSkipVerify accepted here by design: the smoke-test callback
		// server's cert-manager certificate covers beskar7-webhook-service, not
		// the controller-manager service the bootstrap URL resolves to. In
		// production deployments with a properly SANed certificate, pass
		// --insecure-skip-verify=false and optionally --ca-bundle-file.
		tlsCfg.InsecureSkipVerify = true // G402: intentional; flag is --insecure-skip-verify; documented in flag usage
	} else if caBundleFile != "" {
		pem, err := os.ReadFile(caBundleFile)
		if err != nil {
			return nil, fmt.Errorf("read CA bundle %q: %w", caBundleFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("CA bundle %q contained no valid PEM certificates", caBundleFile)
		}
		tlsCfg.RootCAs = pool
	}

	return &http.Client{
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
		Timeout:   timeout,
	}, nil
}

// buildInspectionRequest encodes payload as JSON and constructs an
// *http.Request with Authorization: Bearer <token> and Content-Type:
// application/json. Extracted from postReport so tests can inspect the request
// without a live HTTP server.
//
// The Authorization header value is never logged. Only "tokenPresent: true"
// appears in any log line when the header is set.
func buildInspectionRequest(ctx context.Context, url, token string, payload inspectionReportRequest) (*http.Request, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	return req, nil
}

// nestedString walks a nested map[string]interface{} using the provided field
// path and returns the string value at the leaf, or "" if any step is absent or
// not a string. This replaces k8s.io/apimachinery/pkg/apis/meta/v1/unstructured.NestedString
// to avoid importing the unstructured package in this CLI binary.
func nestedString(obj map[string]interface{}, fields ...string) string {
	m := obj
	for i, f := range fields {
		v, ok := m[f]
		if !ok {
			return ""
		}
		if i == len(fields)-1 {
			s, _ := v.(string)
			return s
		}
		m, ok = v.(map[string]interface{})
		if !ok {
			return ""
		}
	}
	return ""
}

// postReport builds and executes the inspection POST. On 2xx it logs success.
// On non-2xx it logs the status code and up to 500 chars of the response body,
// then returns an error.
func postReport(ctx context.Context, httpClient *http.Client, url, token string, payload inspectionReportRequest) error {
	req, err := buildInspectionRequest(ctx, url, token, payload)
	if err != nil {
		return err
	}

	log.Printf("POSTing inspection report to %s (tokenPresent: true, bodyBytes: %d)", url, req.ContentLength)

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 500))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("inspection POST returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	log.Printf("inspection POST succeeded: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	return nil
}
