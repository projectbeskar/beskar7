/*
Copyright 2026 The Beskar7 Authors.

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

package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
	"github.com/projectbeskar/beskar7/controllers"
	"github.com/projectbeskar/beskar7/internal/auth"
	internalmetrics "github.com/projectbeskar/beskar7/internal/metrics"
)

// recordingManager wraps a real manager and records what setupManager
// registers on it: every Runnable handed to Add, and whether the webhook
// server was requested. Registering a webhook is the only path that asks for
// the webhook server, and asking is what schedules it to start.
type recordingManager struct {
	ctrl.Manager
	runnables        []manager.Runnable
	webhookRequested bool
}

func (m *recordingManager) Add(r manager.Runnable) error {
	m.runnables = append(m.runnables, r)
	return m.Manager.Add(r)
}

func (m *recordingManager) GetWebhookServer() webhook.Server {
	m.webhookRequested = true
	return m.Manager.GetWebhookServer()
}

// controllerCount is the number of registered runnables that are
// controllers. controller-runtime's builder hands the manager the concrete
// controller, which implements the exported controller.Controller interface;
// the callback-server runnable does not.
func (m *recordingManager) controllerCount() int {
	n := 0
	for _, r := range m.runnables {
		if _, ok := r.(controller.Controller); ok {
			n++
		}
	}
	return n
}

// TestSetupManager wires a manager against envtest in both --controllers
// modes and checks what each registers. The callback-only case is the point
// of the test: no reconciler, no webhook, but the health probes and the
// bearer-gated callback routes still answer, with the bearer verifier reading
// PhysicalHost status through the manager's cache.
func TestSetupManager(t *testing.T) {
	restCfg := startEnvtest(t)
	internalmetrics.Init()

	certDir := t.TempDir()
	pool := writeServingCert(t, certDir)

	k8sClient, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}

	newManager := func(t *testing.T, probeAddr string) *recordingManager {
		t.Helper()
		// Controller names are process-global in controller-runtime; skipping
		// the check keeps the "all" case valid under -count>1.
		skipNameValidation := true
		mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
			Scheme:                 scheme,
			Metrics:                metricsserver.Options{BindAddress: "0"},
			HealthProbeBindAddress: probeAddr,
			// The "all" case registers the webhook, and starting the manager
			// then starts the webhook server: give it the test cert and a
			// free port instead of the production defaults.
			WebhookServer: webhook.NewServer(webhook.Options{Port: freePort(t), CertDir: certDir}),
			Controller:    config.Controller{SkipNameValidation: &skipNameValidation},
		})
		if err != nil {
			t.Fatalf("create manager: %v", err)
		}
		return &recordingManager{Manager: mgr}
	}

	baseConfig := func(callbackPort int) managerConfig {
		return managerConfig{
			bootstrapURLBase:        fmt.Sprintf("https://127.0.0.1:%d", callbackPort),
			inspectionPort:          callbackPort,
			inspectionCertDir:       certDir,
			inspectionTimeout:       controllers.DefaultInspectionTimeout,
			deploymentTimeout:       controllers.DefaultDeploymentTimeout,
			maxConcurrentReconciles: controllers.DefaultMaxConcurrentReconciles,
		}
	}

	// startManager runs mgr until the subtest ends and waits for its cache.
	startManager := func(t *testing.T, mgr ctrl.Manager) {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- mgr.Start(ctx) }()
		t.Cleanup(func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("manager exited with error: %v", err)
			}
		})
		if !mgr.GetCache().WaitForCacheSync(ctx) {
			t.Fatal("manager cache never synced")
		}
	}

	t.Run("callback-only registers no controller and still serves the callbacks", func(t *testing.T) {
		callbackPort, probePort := freePort(t), freePort(t)
		mgr := newManager(t, fmt.Sprintf("127.0.0.1:%d", probePort))
		cfg := baseConfig(callbackPort)
		cfg.controllers = controllersNone
		cfg.enableLeaderElection = true // the flag default; must be ignored, not rejected

		if err := setupManager(mgr, cfg); err != nil {
			t.Fatalf("setupManager: %v", err)
		}
		if n := mgr.controllerCount(); n != 0 {
			t.Fatalf("callback-only mode registered %d controllers, want 0", n)
		}
		if mgr.webhookRequested {
			t.Fatal("callback-only mode registered a webhook")
		}

		startManager(t, mgr)

		// Health probes on the manager's probe address.
		probeBase := fmt.Sprintf("http://127.0.0.1:%d", probePort)
		expectStatus(t, http.DefaultClient, http.MethodGet, probeBase+"/healthz", "", http.StatusOK)
		expectStatus(t, http.DefaultClient, http.MethodGet, probeBase+"/readyz", "", http.StatusOK)

		// The callback server's own /healthz over TLS.
		httpsClient := &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
			},
		}
		callbackBase := fmt.Sprintf("https://127.0.0.1:%d", callbackPort)
		expectStatus(t, httpsClient, http.MethodGet, callbackBase+"/healthz", "", http.StatusOK)

		// A bearer-gated route. The verifier reads the host's token hash from
		// PhysicalHost status through the cache, so this also proves the cache
		// serves the handlers with no controller registered.
		ctx := context.Background()
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "callback-only-"}}
		if err := k8sClient.Create(ctx, ns); err != nil {
			t.Fatalf("create namespace: %v", err)
		}
		plaintext, hash, err := auth.MintToken()
		if err != nil {
			t.Fatalf("mint token: %v", err)
		}
		host := &infrav1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{Name: "host-1", Namespace: ns.Name},
			Spec: infrav1.PhysicalHostSpec{
				RedfishConnection: infrav1.RedfishConnection{
					Address:              "https://192.0.2.10",
					CredentialsSecretRef: "bmc-creds",
				},
			},
		}
		if err := k8sClient.Create(ctx, host); err != nil {
			t.Fatalf("create PhysicalHost: %v", err)
		}
		// No controller ran, so no finalizer: the delete completes and the
		// host is gone before the "all" case starts reconcilers.
		t.Cleanup(func() {
			if err := k8sClient.Delete(context.Background(), host); err != nil {
				t.Errorf("delete PhysicalHost: %v", err)
			}
		})
		host.Status.Bootstrap = &infrav1.BootstrapStatus{
			TokenHash: hash,
			ExpiresAt: &metav1.Time{Time: time.Now().Add(time.Hour)},
		}
		if err := k8sClient.Status().Update(ctx, host); err != nil {
			t.Fatalf("update PhysicalHost status: %v", err)
		}

		route := fmt.Sprintf("%s/api/v1/bootstrap/%s/%s", callbackBase, ns.Name, host.Name)
		// The status update reaches the cache through a watch, so the
		// accepted-token case is polled; the rejections are checked once the
		// token is known to be visible.
		//
		// A host with no consumer is a 404 from the handler — anything but 401
		// means the bearer gate let the request through.
		expectStatus(t, httpsClient, http.MethodGet, route, "Bearer "+plaintext, http.StatusNotFound)
		expectStatus(t, httpsClient, http.MethodGet, route, "", http.StatusUnauthorized)
		expectStatus(t, httpsClient, http.MethodGet, route, "Bearer not-the-token", http.StatusUnauthorized)
	})

	t.Run("all registers the three controllers and the webhook", func(t *testing.T) {
		callbackPort := freePort(t)
		mgr := newManager(t, "0")
		cfg := baseConfig(callbackPort)
		cfg.controllers = controllersAll
		cfg.enableWebhook = true

		if err := setupManager(mgr, cfg); err != nil {
			t.Fatalf("setupManager: %v", err)
		}
		if n := mgr.controllerCount(); n != 3 {
			t.Fatalf("all mode registered %d controllers, want 3 (Beskar7Machine, Beskar7Cluster, PhysicalHost)", n)
		}
		if !mgr.webhookRequested {
			t.Fatal("all mode with --enable-webhook did not register the webhook")
		}
		// Start and stop it so the callback-server runnable shuts its listener
		// down instead of leaking it for the rest of the test binary.
		startManager(t, mgr)
	})
}

// expectStatus polls url with the given method and Authorization header
// until it answers with want, failing the test after ten seconds.
func expectStatus(t *testing.T, c *http.Client, method, url, authorization string, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last string
	for {
		req, err := http.NewRequest(method, url, nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		resp, err := c.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == want {
				return
			}
			last = fmt.Sprintf("status %d", resp.StatusCode)
		} else {
			last = err.Error()
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s %s (auth %q): want status %d, last result: %s", method, url, authorization, want, last)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// startEnvtest boots an envtest control plane with the Beskar7 CRDs and the
// minimal CAPI CRDs, resolving assets the same way controllers/suite_test.go
// does when KUBEBUILDER_ASSETS is unset.
func startEnvtest(t *testing.T) *rest.Config {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		out, err := exec.Command("bash", "-lc",
			"go run sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.20 use 1.31.x -p path").CombinedOutput()
		if err != nil {
			t.Fatalf("resolve envtest assets: %v\n%s", err, out)
		}
		t.Setenv("KUBEBUILDER_ASSETS", strings.TrimSpace(string(out)))
	}
	env := &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "config", "crd", "bases"),
			filepath.Join("..", "..", "config", "test-external-crds"),
		},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: os.Getenv("KUBEBUILDER_ASSETS"),
	}
	restCfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	restCfg.QPS = 1000
	restCfg.Burst = 2000
	restCfg.RateLimiter = flowcontrol.NewFakeAlwaysRateLimiter()
	return restCfg
}

// freePort returns a TCP port that was free a moment ago.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	return port
}

// writeServingCert writes a CA and a leaf serving certificate for 127.0.0.1
// into dir as ca.crt, tls.crt and tls.key — the layout SetupCallbackServer
// reads — and returns a pool that trusts the CA.
func writeServingCert(t *testing.T, dir string) *x509.CertPool {
	t.Helper()
	now := time.Now()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "beskar7-test-ca"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA certificate: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse CA certificate: %v", err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "beskar7-callback-test"},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create leaf certificate: %v", err)
	}
	leafKeyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		t.Fatalf("marshal leaf key: %v", err)
	}

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	files := map[string][]byte{
		"ca.crt":  caPEM,
		"tls.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		"tls.key": pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: leafKeyDER}),
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("append CA to pool")
	}
	return pool
}
