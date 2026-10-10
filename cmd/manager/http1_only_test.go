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
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"slices"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

// TestWebhookAndMetricsOptions_HTTP1Only checks the TLS options the manager
// hands controller-runtime. Both servers build their tls.Config as
// NextProtos=["h2"] and then run TLSOpts over it, so the options must leave
// only http/1.1 behind.
func TestWebhookAndMetricsOptions_HTTP1Only(t *testing.T) {
	for name, opts := range map[string][]func(*tls.Config){
		"webhook": buildWebhookOptions(9443, "/certs").TLSOpts,
		"metrics": buildMetricsOptions(":8443", true).TLSOpts,
	} {
		t.Run(name, func(t *testing.T) {
			cfg := &tls.Config{NextProtos: []string{"h2"}}
			for _, op := range opts {
				op(cfg)
			}
			if want := []string{"http/1.1"}; !slices.Equal(cfg.NextProtos, want) {
				t.Fatalf("NextProtos after TLSOpts = %v, want %v", cfg.NextProtos, want)
			}
		})
	}
}

// TestWebhookAndMetricsServersServeHTTP1Only starts a manager with the options
// main builds for its webhook and secure metrics servers, as TestSetupManager
// starts its managers, and dials both with clients that offer HTTP/2. The
// callback server has its own test (TestCallbackServerServesHTTP1Only).
//
// Each server must select http/1.1 for a client offering h2 and http/1.1, must
// not serve h2 to a client offering only h2, and must still answer a request:
// the webhook the apiserver's way (HTTPS, no ALPN preference) and /metrics to
// an authenticated scraper.
func TestWebhookAndMetricsServersServeHTTP1Only(t *testing.T) {
	restCfg := startEnvtest(t)
	ctx := context.Background()
	admin, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}

	certDir := t.TempDir()
	pool := writeServingCert(t, certDir)
	webhookPort, metricsPort := freePort(t), freePort(t)
	webhookAddr := fmt.Sprintf("127.0.0.1:%d", webhookPort)
	metricsAddr := fmt.Sprintf("127.0.0.1:%d", metricsPort)

	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                buildMetricsOptions(metricsAddr, true),
		WebhookServer:          webhook.NewServer(buildWebhookOptions(webhookPort, certDir)),
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		t.Fatalf("create manager: %v", err)
	}
	// Asking for the webhook server is what schedules it to start; with no
	// webhook registered nothing else does.
	_ = mgr.GetWebhookServer()

	mgrCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- mgr.Start(mgrCtx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("manager exited with error: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Error("manager did not stop within 30s of cancel")
		}
	})

	// The metrics server serves a self-signed certificate it generates when
	// given no cert dir, so the scraper below cannot verify it. Only the
	// protocol is under test.
	metricsTLS := &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}
	webhookTLS := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}

	for _, srv := range []struct {
		name string
		addr string
		tls  *tls.Config
	}{
		{"webhook", webhookAddr, webhookTLS},
		{"metrics", metricsAddr, metricsTLS},
	} {
		t.Run(srv.name+" selects http/1.1 for a client offering h2", func(t *testing.T) {
			// Both servers start with the manager; wait for the listener.
			deadline := time.Now().Add(30 * time.Second)
			for {
				got, err := negotiatedProtocol(srv.addr, srv.tls, "h2", "http/1.1")
				if err == nil {
					if got != "http/1.1" {
						t.Fatalf("negotiated %q, want %q", got, "http/1.1")
					}
					return
				}
				if time.Now().After(deadline) {
					t.Fatalf("no TLS handshake with %s within 30s: %v", srv.addr, err)
				}
				time.Sleep(100 * time.Millisecond)
			}
		})
		t.Run(srv.name+" does not serve h2 to a client offering only h2", func(t *testing.T) {
			got, err := negotiatedProtocol(srv.addr, srv.tls, "h2")
			if err == nil && got == "h2" {
				t.Fatal("negotiated h2")
			}
			t.Logf("h2-only client: negotiated %q, handshake error: %v", got, err)
		})
	}

	t.Run("webhook answers an HTTP/1.1 request", func(t *testing.T) {
		resp := get(t, webhookTLS, "https://"+webhookAddr+"/not-registered", "")
		// controller-runtime's mux has no handler for the path.
		if resp.status != http.StatusNotFound || resp.protoMajor != 1 {
			t.Fatalf("got status %d over HTTP/%d, want 404 over HTTP/1", resp.status, resp.protoMajor)
		}
	})

	t.Run("metrics answers an authenticated scraper over HTTP/1.1", func(t *testing.T) {
		token := metricsScraperToken(t, admin)
		unauthenticated := get(t, metricsTLS, "https://"+metricsAddr+"/metrics", "")
		if unauthenticated.status != http.StatusUnauthorized || unauthenticated.protoMajor != 1 {
			t.Fatalf("without a token: status %d over HTTP/%d, want 401 over HTTP/1",
				unauthenticated.status, unauthenticated.protoMajor)
		}
		// RBAC and the token become visible to the apiserver asynchronously.
		deadline := time.Now().Add(30 * time.Second)
		for {
			resp := get(t, metricsTLS, "https://"+metricsAddr+"/metrics", token)
			if resp.status == http.StatusOK {
				if resp.protoMajor != 1 {
					t.Fatalf("served /metrics over HTTP/%d, want HTTP/1", resp.protoMajor)
				}
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("authenticated GET /metrics: status %d after 30s", resp.status)
			}
			time.Sleep(200 * time.Millisecond)
		}
	})
}

type getResult struct {
	status     int
	protoMajor int
}

// get sends one GET, with a bearer token when given, from a transport that
// would use HTTP/2 if the server allowed it.
func get(t *testing.T, cfg *tls.Config, url, bearer string) getResult {
	t.Helper()
	c := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			ForceAttemptHTTP2: true,
			TLSClientConfig:   cfg,
			DisableKeepAlives: true,
		},
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("read response from %s: %v", url, err)
	}
	return getResult{status: resp.StatusCode, protoMajor: resp.ProtoMajor}
}

// metricsScraperToken returns a ServiceAccount token that may GET /metrics,
// the identity config/rbac/metrics_reader_role.yaml gives a Prometheus. The
// ClusterRole and its binding are removed when the test ends.
func metricsScraperToken(t *testing.T, c client.Client) string {
	t.Helper()
	ctx := context.Background()
	ns := createNamespace(t, c, "http1-metrics-")

	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "scraper", Namespace: ns}}
	mustCreate(t, c, sa)
	role := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "http1-metrics-reader-"},
		Rules:      []rbacv1.PolicyRule{{NonResourceURLs: []string{"/metrics"}, Verbs: []string{"get"}}},
	}
	mustCreate(t, c, role)
	t.Cleanup(func() { _ = c.Delete(context.Background(), role) })
	binding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "http1-metrics-reader-"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: sa.Name, Namespace: ns}},
	}
	mustCreate(t, c, binding)
	t.Cleanup(func() { _ = c.Delete(context.Background(), binding) })

	tokenRequest := &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: ptr.To[int64](3600)}}
	if err := c.SubResource("token").Create(ctx, sa, tokenRequest); err != nil {
		t.Fatalf("request a token for %s/%s: %v", ns, sa.Name, err)
	}
	return tokenRequest.Status.Token
}
