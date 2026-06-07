package redfish

// Tests in this file exercise the real gofish client against the vendored
// DMTF public-rackmount1 corpus served as a static HTTP tree. The goal is
// read/parse robustness: does our wrapper survive real DMTF JSON that carries
// OEM blocks, extra enum values, and a richer link graph than our hand-rolled
// mock emits? Stateful operations (SetBootSourcePXE, power cycles) are NOT
// tested here — those belong in the mock-based state-machine tests that can
// assert "was called / was cleared". This lane is corpus = read only.
//
// The corpus is vendored at testdata/corpus/public-rackmount1/ (BSD-3-Clause,
// see testdata/corpus/LICENSE). It is a curated subset of the DMTF
// Redfish-Mockup-Server repository; only the files that gofish actually GETs
// for our 9-method interface are included.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gofishredfish "github.com/stmcginnis/gofish/redfish"
)

// corpusRoot is the filesystem path to the vendored DMTF mockup subtree.
// Using os.ReadFile rather than embed because testdata/ is already excluded
// from the binary by Go convention, and runtime.Callers-based path lookup is
// fragile. The test binary's working directory is the package directory under
// `go test`, so a relative path resolves correctly.
const corpusRoot = "testdata/corpus/public-rackmount1"

// newCorpusHandler returns an http.Handler that serves the vendored DMTF
// corpus. URL→file mapping mirrors the DMTF mockup server convention:
//
//	/redfish/v1/            → <corpusRoot>/redfish/v1/index.json
//	/redfish/v1/Systems     → <corpusRoot>/redfish/v1/Systems/index.json
//	/redfish/v1/odata       → <corpusRoot>/redfish/v1/odata/index.json
//
// Path traversal (any segment containing "..") is rejected with 400.
// Anything not in the corpus returns 404. gofish uses BasicAuth here, so
// there is no SessionService handshake — the handler needs no auth logic.
func newCorpusHandler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := path.Clean(r.URL.Path)

		// Reject any path that still contains ".." after cleaning — defence
		// against traversal even though path.Clean already handles most forms.
		for _, seg := range strings.Split(p, "/") {
			if seg == ".." {
				http.Error(w, "path traversal rejected", http.StatusBadRequest)
				return
			}
		}

		// Map URL path to filesystem path: strip the leading "/" and append
		// "index.json". The corpus root is relative to the package directory.
		rel := strings.TrimPrefix(p, "/")
		fsPath := filepath.Join(corpusRoot, filepath.FromSlash(rel), "index.json")

		data, err := os.ReadFile(fsPath)
		if err != nil {
			if os.IsNotExist(err) {
				http.NotFound(w, r)
				return
			}
			t.Logf("corpus handler: unexpected read error for %s: %v", fsPath, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	})
}

// TestCorpusReadRobustness_GetSystemInfo verifies that the real gofish client
// parses the DMTF public-rackmount1 ComputerSystem document and that our
// GetSystemInfo wrapper returns the verbatim values from the corpus JSON.
// Expected values are read from the vendored index.json — not invented.
//   - Manufacturer: "Contoso"
//   - Model:        "3500"
//   - SerialNumber: "437XR1138R2"
func TestCorpusReadRobustness_GetSystemInfo(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(newCorpusHandler(t))
	defer server.Close()

	httpClient := server.Client()
	httpClient.Timeout = 10 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := NewClientWithHTTPClient(ctx, server.URL, "u", "p", false, httpClient)
	if err != nil {
		t.Fatalf("NewClientWithHTTPClient: %v", err)
	}
	defer client.Close(ctx)

	info, err := client.GetSystemInfo(ctx)
	if err != nil {
		t.Fatalf("GetSystemInfo: %v", err)
	}

	// Values are verbatim from
	// testdata/corpus/public-rackmount1/redfish/v1/Systems/437XR1138R2/index.json
	if info.Manufacturer != "Contoso" {
		t.Errorf("Manufacturer: want %q, got %q", "Contoso", info.Manufacturer)
	}
	if info.Model != "3500" {
		t.Errorf("Model: want %q, got %q", "3500", info.Model)
	}
	if info.SerialNumber != "437XR1138R2" {
		t.Errorf("SerialNumber: want %q, got %q", "437XR1138R2", info.SerialNumber)
	}
}

// TestCorpusReadRobustness_GetPowerState verifies that gofish parses the
// PowerState field from the DMTF corpus and that GetPowerState returns it
// correctly. The corpus system has PowerState = "On".
func TestCorpusReadRobustness_GetPowerState(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(newCorpusHandler(t))
	defer server.Close()

	httpClient := server.Client()
	httpClient.Timeout = 10 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := NewClientWithHTTPClient(ctx, server.URL, "u", "p", false, httpClient)
	if err != nil {
		t.Fatalf("NewClientWithHTTPClient: %v", err)
	}
	defer client.Close(ctx)

	state, err := client.GetPowerState(ctx)
	if err != nil {
		t.Fatalf("GetPowerState: %v", err)
	}

	// Value is verbatim from
	// testdata/corpus/public-rackmount1/redfish/v1/Systems/437XR1138R2/index.json
	// "PowerState": "On"
	if state != gofishredfish.OnPowerState {
		t.Errorf("PowerState: want %q, got %q", gofishredfish.OnPowerState, state)
	}
}

// TestCorpusReadRobustness_GetNetworkAddresses verifies that the
// EthernetInterfaces walk succeeds against the real DMTF tree structure
// (collection → members → per-interface IPv4/IPv6 arrays) and that we parse
// at least the two physical NICs' addresses. This is the most complex read
// path: gofish fetches the collection, iterates members, fetches each
// interface, and our extractor walks IPv4Addresses + IPv6Addresses.
//
// Asserted values come from the corpus JSON:
//   - NIC 12446A3B0411: IPv4 192.168.0.10, MAC 12:44:6A:3B:04:11
//   - NIC 12446A3B8890: IPv4 192.168.0.11, MAC AA:BB:CC:DD:EE:00
func TestCorpusReadRobustness_GetNetworkAddresses(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(newCorpusHandler(t))
	defer server.Close()

	httpClient := server.Client()
	httpClient.Timeout = 10 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := NewClientWithHTTPClient(ctx, server.URL, "u", "p", false, httpClient)
	if err != nil {
		t.Fatalf("NewClientWithHTTPClient: %v", err)
	}
	defer client.Close(ctx)

	addrs, err := client.GetNetworkAddresses(ctx)
	if err != nil {
		t.Fatalf("GetNetworkAddresses: %v", err)
	}

	if len(addrs) == 0 {
		t.Fatal("GetNetworkAddresses: returned no addresses")
	}

	// Index the returned addresses by IP for order-independent assertions.
	byIP := make(map[string]NetworkAddress, len(addrs))
	for _, a := range addrs {
		byIP[a.Address] = a
	}

	// Physical NIC 1 — values from
	// testdata/corpus/…/EthernetInterfaces/12446A3B0411/index.json
	a0, ok := byIP["192.168.0.10"]
	if !ok {
		t.Errorf("expected address 192.168.0.10 from NIC 12446A3B0411, not found in %v", addrs)
	} else {
		if a0.MACAddress != "12:44:6A:3B:04:11" {
			t.Errorf("NIC 12446A3B0411 MAC: want %q, got %q", "12:44:6A:3B:04:11", a0.MACAddress)
		}
		if a0.Type != IPv4AddressType {
			t.Errorf("NIC 12446A3B0411 type: want IPv4, got %q", a0.Type)
		}
	}

	// Physical NIC 2 — values from
	// testdata/corpus/…/EthernetInterfaces/12446A3B8890/index.json
	a1, ok := byIP["192.168.0.11"]
	if !ok {
		t.Errorf("expected address 192.168.0.11 from NIC 12446A3B8890, not found in %v", addrs)
	} else {
		if a1.MACAddress != "AA:BB:CC:DD:EE:00" {
			t.Errorf("NIC 12446A3B8890 MAC: want %q, got %q", "AA:BB:CC:DD:EE:00", a1.MACAddress)
		}
		if a1.Type != IPv4AddressType {
			t.Errorf("NIC 12446A3B8890 type: want IPv4, got %q", a1.Type)
		}
	}
}
