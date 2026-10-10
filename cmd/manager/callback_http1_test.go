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
	"crypto/tls"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// negotiatedProtocol completes a TLS handshake with addr, offering the given
// ALPN protocols on top of cfg, and returns the protocol the server selected
// ("" when it selected none). A refused handshake is an error.
func negotiatedProtocol(addr string, cfg *tls.Config, offer ...string) (string, error) {
	c := cfg.Clone()
	c.NextProtos = offer
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", addr, c)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()
	return conn.ConnectionState().NegotiatedProtocol, nil
}

// TestCallbackServerServesHTTP1Only dials the real callback server, started by
// setupManager, with clients that offer HTTP/2 and checks that none gets it. The
// server is reachable from the provisioning network and negotiates its protocol
// before any bearer check, so an HTTP/2 server bug there needs no credential
// to reach.
//
// The client that offers only h2 is the case that matters. Advertising http/1.1
// in the server's tls.Config is not enough on its own: net/http, finding no
// Protocols setting, installs its HTTP/2 handler and puts "h2" back into the
// ALPN list, so a client offering h2 alone still gets it.
func TestCallbackServerServesHTTP1Only(t *testing.T) {
	restCfg := startEnvtest(t)
	admin, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	fx := createCallbackFixture(t, admin, createNamespace(t, admin, "http1-only-"))

	certDir := t.TempDir()
	pool := writeServingCert(t, certDir)
	base := runCallbackOnlyManager(t, restCfg, certDir)
	addr := strings.TrimPrefix(base, "https://")
	trusting := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}

	t.Run("a client offering h2 and http/1.1 is served http/1.1", func(t *testing.T) {
		got, err := negotiatedProtocol(addr, trusting, "h2", "http/1.1")
		if err != nil {
			t.Fatalf("handshake: %v", err)
		}
		if got != "http/1.1" {
			t.Fatalf("negotiated %q, want %q", got, "http/1.1")
		}
	})

	t.Run("a client offering only h2 is not served h2", func(t *testing.T) {
		// The server is up and trusted (the subtest above), so a refusal here
		// is the ALPN mismatch and not a transport problem.
		got, err := negotiatedProtocol(addr, trusting, "h2")
		if err == nil && got == "h2" {
			t.Fatal("negotiated h2; the callback server must serve HTTP/1.1 only")
		}
		t.Logf("h2-only client: negotiated %q, handshake error: %v", got, err)
	})

	t.Run("HTTP/1.1 requests are still served", func(t *testing.T) {
		// A transport that would use h2 if the server allowed it.
		c := &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				ForceAttemptHTTP2: true,
				TLSClientConfig:   trusting,
			},
		}
		t.Cleanup(c.CloseIdleConnections)

		for _, tc := range []struct {
			name, method, url, bearer string
			want                      int
		}{
			{"unauthenticated /healthz", http.MethodGet, base + "/healthz", "", http.StatusOK},
			{"bearer-gated bootstrap GET", http.MethodGet, fx.route(base, "bootstrap"), fx.token, http.StatusOK},
			{"bearer-gated route without a token", http.MethodGet, fx.route(base, "bootstrap"), "", http.StatusUnauthorized},
		} {
			req, err := http.NewRequest(tc.method, tc.url, nil)
			if err != nil {
				t.Fatalf("%s: build request: %v", tc.name, err)
			}
			if tc.bearer != "" {
				req.Header.Set("Authorization", "Bearer "+tc.bearer)
			}
			resp, err := c.Do(req)
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Errorf("%s: status %d, want %d", tc.name, resp.StatusCode, tc.want)
			}
			if resp.ProtoMajor != 1 {
				t.Errorf("%s: answered over %s, want HTTP/1.1", tc.name, resp.Proto)
			}
		}
	})
}
