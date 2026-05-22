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
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// newTestServer wires the mock's Handler() into an httptest.Server. We use
// the plain (non-TLS) variant to keep individual tests fast — TLS would
// require a self-signed cert per test and add ~10ms each. The compat helper
// (NewMockRedfishServerWithHTTPTest, exercised separately below) is the
// one that uses TLS; that's the relevant variant for gofish-against-mock
// integration tests in test/emulation.
func newTestServer(t *testing.T, vendor VendorType) (*MockRedfishServer, *httptest.Server) {
	t.Helper()
	mock := NewMockRedfishServer(vendor)
	srv := httptest.NewServer(mock.Handler())
	t.Cleanup(srv.Close)
	return mock, srv
}

// authedGET performs a GET with HTTP Basic Auth. Returns the response;
// callers are responsible for body.Close.
func authedGET(t *testing.T, baseURL, path, user, pass string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, baseURL+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return resp
}

// decodeJSON reads and JSON-decodes the body into out. Closes the body.
func decodeJSON(t *testing.T, resp *http.Response, out interface{}) {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode body: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Construction & vendor initialisation
// ---------------------------------------------------------------------------

func TestNewMockRedfishServer_VendorDefaults(t *testing.T) {
	// wantBiosKey="" means "no vendor-specific BIOS attrs expected" — the
	// init switch in initBIOSAttributes intentionally has no case for
	// VendorGeneric, so its BIOS map is initialised but empty.
	for _, tc := range []struct {
		vendor      VendorType
		wantMfg     string
		wantBiosKey string
	}{
		{VendorDell, "Dell Inc.", "KernelArgs"},
		{VendorHPE, "HPE", "BootOrderPolicy"},
		{VendorLenovo, "Lenovo", "SystemBootSequence"},
		{VendorSupermicro, "Supermicro", "BootFeature"},
		{VendorGeneric, "Generic Manufacturer", ""},
	} {
		t.Run(string(tc.vendor), func(t *testing.T) {
			m := NewMockRedfishServer(tc.vendor)
			if m.Vendor() != tc.vendor {
				t.Errorf("Vendor() = %q, want %q", m.Vendor(), tc.vendor)
			}
			if m.SystemInfo.Manufacturer != tc.wantMfg {
				t.Errorf("Manufacturer = %q, want %q", m.SystemInfo.Manufacturer, tc.wantMfg)
			}
			if m.BiosAttributes == nil {
				t.Errorf("BiosAttributes is nil; expected initialised map")
			}
			if tc.wantBiosKey != "" {
				if _, ok := m.BiosAttributes[tc.wantBiosKey]; !ok {
					t.Errorf("BiosAttributes missing key %q for vendor %s (have: %v)",
						tc.wantBiosKey, tc.vendor, mapKeysAny(m.BiosAttributes))
				}
			}
			if !m.AuthEnabled {
				t.Errorf("AuthEnabled = false on init; expected true")
			}
			if m.SystemInfo.PowerState != PowerStateOn {
				t.Errorf("PowerState = %q, want On", m.SystemInfo.PowerState)
			}
		})
	}
}

func TestNewMockRedfishServer_EmptyRequestLog(t *testing.T) {
	m := NewMockRedfishServer(VendorGeneric)
	if log := m.GetRequestLog(); len(log) != 0 {
		t.Errorf("GetRequestLog on fresh server returned %d entries, want 0", len(log))
	}
}

// ---------------------------------------------------------------------------
// Handler routing
// ---------------------------------------------------------------------------

func TestHandler_ServiceRoot(t *testing.T) {
	_, srv := newTestServer(t, VendorGeneric)
	resp := authedGET(t, srv.URL, "/redfish/v1/", "admin", "password123")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("service root status = %d, want 200", resp.StatusCode)
	}
	var body map[string]interface{}
	decodeJSON(t, resp, &body)
	for _, key := range []string{"@odata.id", "@odata.type", "Systems", "Managers", "RedfishVersion"} {
		if _, ok := body[key]; !ok {
			t.Errorf("service root response missing %q (got: %v)", key, mapKeys(body))
		}
	}
}

func TestHandler_SystemsCollectionAndGet(t *testing.T) {
	_, srv := newTestServer(t, VendorDell)

	// Collection
	resp := authedGET(t, srv.URL, "/redfish/v1/Systems", "admin", "password123")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("systems collection = %d, want 200", resp.StatusCode)
	}
	var coll map[string]interface{}
	decodeJSON(t, resp, &coll)
	if count, _ := coll["Members@odata.count"].(float64); count != 1 {
		t.Errorf("Members@odata.count = %v, want 1", coll["Members@odata.count"])
	}

	// Individual system reflects vendor SystemInfo
	resp = authedGET(t, srv.URL, "/redfish/v1/Systems/1", "admin", "password123")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("system get = %d, want 200", resp.StatusCode)
	}
	var sys map[string]interface{}
	decodeJSON(t, resp, &sys)
	if sys["Manufacturer"] != "Dell Inc." {
		t.Errorf("Manufacturer = %v, want Dell Inc.", sys["Manufacturer"])
	}
	if sys["PowerState"] != string(PowerStateOn) {
		t.Errorf("PowerState = %v, want On", sys["PowerState"])
	}
}

func TestHandler_ManagersAndVirtualMedia(t *testing.T) {
	_, srv := newTestServer(t, VendorGeneric)

	// Note on routing quirks the switch in handleRequest produces:
	//   - "/redfish/v1/Managers/1/VirtualMedia/nope" matches the
	//     /VirtualMedia/ prefix (catch-all) and returns 200 with the
	//     same payload as /VirtualMedia/1. Not ideal, but documented.
	//   - "/redfish/v1/Systems/1/Bios" matches the /Systems/ prefix
	//     before the explicit Bios case, so returns 200 with a System
	//     response. The explicit Bios 404 case is unreachable. Logged
	//     as a curiosity; not asserted here.
	for path, wantOK := range map[string]bool{
		"/redfish/v1/Managers":                      true,
		"/redfish/v1/Managers/1":                    true,
		"/redfish/v1/Managers/1/VirtualMedia":       true,
		"/redfish/v1/Managers/1/VirtualMedia/1":     true,
		"/redfish/v1/Managers/1/VirtualMedia/nope":  true,
		"/redfish/v1/something-that-does-not-exist": false,
	} {
		resp := authedGET(t, srv.URL, path, "admin", "password123")
		_ = resp.Body.Close()
		if wantOK && resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, resp.StatusCode)
		}
		if !wantOK && resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, resp.StatusCode)
		}
	}
}

// ---------------------------------------------------------------------------
// Auth (the spec-conformant unauthenticated-service-root rule is critical
// — gofish probes it before knowing whether credentials are needed)
// ---------------------------------------------------------------------------

func TestAuth_ServiceRootIsUnauthenticated(t *testing.T) {
	// Per DSP0266 §9.1, GET /redfish/v1/ MUST succeed without any
	// Authorization header. Regression test for the bug that surfaced in
	// the first end-to-end smoke run on virtrigaud.
	_, srv := newTestServer(t, VendorGeneric)
	resp := authedGET(t, srv.URL, "/redfish/v1/", "", "")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("anonymous GET /redfish/v1/ = %d, want 200", resp.StatusCode)
	}
}

func TestAuth_NonRootRequiresAuth(t *testing.T) {
	_, srv := newTestServer(t, VendorGeneric)
	resp := authedGET(t, srv.URL, "/redfish/v1/Systems/1", "", "")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous GET /redfish/v1/Systems/1 = %d, want 401", resp.StatusCode)
	}
	if got := resp.Header.Get("WWW-Authenticate"); !strings.HasPrefix(got, "Basic") {
		t.Errorf("WWW-Authenticate header = %q, want Basic challenge", got)
	}
}

func TestAuth_WrongCredentialsRejected(t *testing.T) {
	_, srv := newTestServer(t, VendorGeneric)
	resp := authedGET(t, srv.URL, "/redfish/v1/Systems/1", "admin", "wrong")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong creds = %d, want 401", resp.StatusCode)
	}
}

func TestAuth_SetCredentialsReplacesPair(t *testing.T) {
	mock, srv := newTestServer(t, VendorGeneric)
	mock.SetCredentials("operator", "s3cret")

	// Old default should now be rejected.
	r1 := authedGET(t, srv.URL, "/redfish/v1/Systems/1", "admin", "password123")
	_ = r1.Body.Close()
	if r1.StatusCode != http.StatusUnauthorized {
		t.Errorf("old default after SetCredentials = %d, want 401", r1.StatusCode)
	}
	// New credentials work.
	r2 := authedGET(t, srv.URL, "/redfish/v1/Systems/1", "operator", "s3cret")
	_ = r2.Body.Close()
	if r2.StatusCode != http.StatusOK {
		t.Errorf("new creds = %d, want 200", r2.StatusCode)
	}
}

func TestAuth_DisableAuthSkipsCheck(t *testing.T) {
	mock, srv := newTestServer(t, VendorGeneric)
	mock.DisableAuth()
	if mock.AuthEnabled {
		t.Fatalf("AuthEnabled still true after DisableAuth")
	}
	resp := authedGET(t, srv.URL, "/redfish/v1/Systems/1", "", "")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("anonymous request with auth disabled = %d, want 200", resp.StatusCode)
	}
}

func TestAuth_AuthFailuresFailureMode(t *testing.T) {
	mock, srv := newTestServer(t, VendorGeneric)
	mock.SetFailureMode(FailureConfig{AuthFailures: true})

	// Even with correct credentials, AuthFailures mode rejects every request.
	resp := authedGET(t, srv.URL, "/redfish/v1/Systems/1", "admin", "password123")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("AuthFailures with correct creds = %d, want 401", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Power reset
// ---------------------------------------------------------------------------

func TestSystemReset_TransitionsPowerState(t *testing.T) {
	_, srv := newTestServer(t, VendorGeneric)

	for _, tc := range []struct {
		resetType string
		wantState PowerState
	}{
		{"ForceOff", PowerStateOff},
		{"On", PowerStateOn},
		{"GracefulShutdown", PowerStateOff},
		{"ForceRestart", PowerStateOn},
	} {
		t.Run(tc.resetType, func(t *testing.T) {
			body, _ := json.Marshal(map[string]string{"ResetType": tc.resetType})
			req, _ := http.NewRequest(http.MethodPost,
				srv.URL+"/redfish/v1/Systems/1/Actions/ComputerSystem.Reset",
				bytes.NewReader(body))
			req.SetBasicAuth("admin", "password123")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("reset POST: %v", err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				t.Fatalf("reset %s = %d, want 204", tc.resetType, resp.StatusCode)
			}

			// Verify power state in subsequent GET reflects the reset.
			r2 := authedGET(t, srv.URL, "/redfish/v1/Systems/1", "admin", "password123")
			var sys map[string]interface{}
			decodeJSON(t, r2, &sys)
			if sys["PowerState"] != string(tc.wantState) {
				t.Errorf("PowerState after %s = %v, want %v", tc.resetType, sys["PowerState"], tc.wantState)
			}
		})
	}
}

func TestSystemReset_InvalidResetType(t *testing.T) {
	_, srv := newTestServer(t, VendorGeneric)

	body, _ := json.Marshal(map[string]string{"ResetType": "Implode"})
	req, _ := http.NewRequest(http.MethodPost,
		srv.URL+"/redfish/v1/Systems/1/Actions/ComputerSystem.Reset",
		bytes.NewReader(body))
	req.SetBasicAuth("admin", "password123")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("reset POST: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid ResetType = %d, want 400", resp.StatusCode)
	}
}

func TestSystemReset_PowerFailuresFailureMode(t *testing.T) {
	mock, srv := newTestServer(t, VendorGeneric)
	mock.SetFailureMode(FailureConfig{PowerFailures: true})

	body, _ := json.Marshal(map[string]string{"ResetType": "On"})
	req, _ := http.NewRequest(http.MethodPost,
		srv.URL+"/redfish/v1/Systems/1/Actions/ComputerSystem.Reset",
		bytes.NewReader(body))
	req.SetBasicAuth("admin", "password123")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("reset POST: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("PowerFailures mode = %d, want 500", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Request log (used by integration tests to assert call patterns)
// ---------------------------------------------------------------------------

func TestRequestLog_RecordsAndReturnsCopy(t *testing.T) {
	mock, srv := newTestServer(t, VendorGeneric)
	for _, p := range []string{"/redfish/v1/", "/redfish/v1/Systems", "/redfish/v1/Systems/1"} {
		resp := authedGET(t, srv.URL, p, "admin", "password123")
		_ = resp.Body.Close()
	}
	log := mock.GetRequestLog()
	if len(log) != 3 {
		t.Fatalf("GetRequestLog returned %d entries, want 3 (%v)", len(log), log)
	}
	// Snapshot semantics: mutating the returned slice does not affect later reads.
	log[0].URL = "MUTATED"
	log2 := mock.GetRequestLog()
	if log2[0].URL == "MUTATED" {
		t.Errorf("GetRequestLog returned a live reference, not a snapshot")
	}
}

// ---------------------------------------------------------------------------
// Failure modes (NetworkErrors)
// ---------------------------------------------------------------------------

func TestFailureMode_NetworkErrors_AffectsNonRootOnly(t *testing.T) {
	mock, srv := newTestServer(t, VendorGeneric)
	mock.SetFailureMode(FailureConfig{NetworkErrors: true})

	// Service root MUST still be reachable so the client can connect.
	r1 := authedGET(t, srv.URL, "/redfish/v1/", "", "")
	_ = r1.Body.Close()
	if r1.StatusCode != http.StatusOK {
		t.Errorf("service root in NetworkErrors mode = %d, want 200 (root must stay reachable)", r1.StatusCode)
	}

	// Anything else returns 500.
	r2 := authedGET(t, srv.URL, "/redfish/v1/Systems/1", "admin", "password123")
	_ = r2.Body.Close()
	if r2.StatusCode != http.StatusInternalServerError {
		t.Errorf("non-root in NetworkErrors mode = %d, want 500", r2.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Concurrency: -race must report clean
// ---------------------------------------------------------------------------

func TestConcurrent_RequestsAndStateMutation(t *testing.T) {
	_, srv := newTestServer(t, VendorGeneric)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			resp := authedGET(t, srv.URL, "/redfish/v1/Systems/1", "admin", "password123")
			_ = resp.Body.Close()
		}()
		go func() {
			defer wg.Done()
			body, _ := json.Marshal(map[string]string{"ResetType": "ForceOff"})
			req, _ := http.NewRequest(http.MethodPost,
				srv.URL+"/redfish/v1/Systems/1/Actions/ComputerSystem.Reset",
				bytes.NewReader(body))
			req.SetBasicAuth("admin", "password123")
			r, err := http.DefaultClient.Do(req)
			if err != nil {
				return
			}
			_ = r.Body.Close()
		}()
	}
	wg.Wait()
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func mapKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// mapKeysAny is the same as mapKeys but accepts the BiosAttributes map
// (which uses the same type today; kept as a separate symbol so callers
// can switch if the value type ever diverges).
func mapKeysAny(m map[string]interface{}) []string {
	return mapKeys(m)
}
