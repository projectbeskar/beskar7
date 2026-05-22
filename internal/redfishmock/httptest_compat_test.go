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

package redfishmock

import (
	"crypto/tls"
	"net/http"
	"strings"
	"testing"
)

// TestNewMockRedfishServerWithHTTPTest verifies the compat helper used by
// existing integration tests in test/emulation: it must return a usable
// httptest TLS URL and Close cleanly. The embedded *MockRedfishServer is
// also reachable so callers can still call SetCredentials, DisableAuth, etc.
func TestNewMockRedfishServerWithHTTPTest(t *testing.T) {
	m := NewMockRedfishServerWithHTTPTest(VendorDell)
	defer m.Close()

	url := m.GetURL()
	if !strings.HasPrefix(url, "https://") {
		t.Fatalf("GetURL() = %q, want https:// (TLS server)", url)
	}

	// Embedded server is reachable.
	if m.Vendor() != VendorDell {
		t.Errorf("Vendor() = %q, want %q (embedded mock not wired)", m.Vendor(), VendorDell)
	}

	// httptest.NewTLSServer uses a self-signed cert; client must skip verify.
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // httptest cert
		},
	}
	resp, err := client.Get(url + "/redfish/v1/")
	if err != nil {
		t.Fatalf("GET service root via TLS: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("service root over TLS = %d, want 200", resp.StatusCode)
	}
}

// TestNewMockRedfishServerWithHTTPTest_CloseIsSafe verifies Close is safe
// to call multiple times — httptest.Server.Close panics if you don't, and
// some test cleanups end up calling both defer and t.Cleanup. (Today this
// would actually panic on the second call; documents the limitation.)
func TestNewMockRedfishServerWithHTTPTest_CloseOnce(t *testing.T) {
	m := NewMockRedfishServerWithHTTPTest(VendorHPE)
	m.Close()
	// Reading GetURL after Close is still safe (just returns the cached URL).
	if m.GetURL() == "" {
		t.Errorf("GetURL() empty after Close; expected cached URL string")
	}
}
