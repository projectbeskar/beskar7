package redfish

// Regression test for issue #218: a BMC that returns ComputerSystem's
// OperatingSystem property as a Redfish link object rather than a string.
//
// Redfish models OperatingSystem as a navigation property — a link to an
// OperatingSystem resource — and AMI MegaRAC SP-X (Redfish 1.15.1, schema
// bundle 2022.1) returns it that way:
//
//	"OperatingSystem": {"@odata.id": "/redfish/v1/Systems/Self/OperatingSystem"}
//
// gofish v0.20.0 typed the field as a plain string, so unmarshalling the
// ComputerSystem failed and EVERY call through Systems() returned
//
//	json: cannot unmarshal object into Go struct field .OperatingSystem of type string
//
// which put the PhysicalHost in Error before inspection could start. The field
// is a Link from gofish v0.21.0 onwards. This test pins the behaviour: it fails
// against v0.20.0 with the error above and passes once the client can parse a
// link-shaped OperatingSystem.
//
// The tree here is synthetic rather than vendored: it is modelled on the
// reporter's MegaRAC (singleton system at /redfish/v1/Systems/Self) and kept to
// the documents gofish GETs for this path. The DMTF corpus in
// corpus_robustness_test.go stays byte-for-byte upstream.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// megaRACSystemJSON is a ComputerSystem carrying OperatingSystem as a link,
// the shape reported in issue #218. The surrounding fields are the ones our
// GetSystemInfo reads, so a parse failure cannot hide behind empty values.
const megaRACSystemJSON = `{
  "@odata.type": "#ComputerSystem.v1_17_0.ComputerSystem",
  "@odata.id": "/redfish/v1/Systems/Self",
  "Id": "Self",
  "Name": "System",
  "Manufacturer": "AMI",
  "Model": "MegaRAC SP-X",
  "SerialNumber": "MEGARAC-0001",
  "PowerState": "On",
  "Boot": {
    "BootSourceOverrideEnabled": "Disabled",
    "BootSourceOverrideTarget": "None"
  },
  "OperatingSystem": {"@odata.id": "/redfish/v1/Systems/Self/OperatingSystem"},
  "Status": {"State": "Enabled", "Health": "OK"}
}`

// newMegaRACHandler serves the smallest Redfish tree that reaches the system
// document above: the service root, the Systems collection, and the system.
func newMegaRACHandler(t *testing.T) http.Handler {
	t.Helper()

	const serviceRoot = `{
  "@odata.type": "#ServiceRoot.v1_5_0.ServiceRoot",
  "@odata.id": "/redfish/v1/",
  "Id": "RootService",
  "Name": "Root Service",
  "RedfishVersion": "1.15.1",
  "Systems": {"@odata.id": "/redfish/v1/Systems"}
}`

	const systemsCollection = `{
  "@odata.type": "#ComputerSystemCollection.ComputerSystemCollection",
  "@odata.id": "/redfish/v1/Systems",
  "Name": "Computer System Collection",
  "Members@odata.count": 1,
  "Members": [{"@odata.id": "/redfish/v1/Systems/Self"}]
}`

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body string
		switch r.URL.Path {
		case "/redfish/v1/", "/redfish/v1":
			body = serviceRoot
		case "/redfish/v1/Systems":
			body = systemsCollection
		case "/redfish/v1/Systems/Self":
			body = megaRACSystemJSON
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, body)
	})
}

// TestOperatingSystemAsLinkIsParsed is the issue #218 regression: a system
// document whose OperatingSystem is a link must parse, and the values around
// it must survive intact.
func TestOperatingSystemAsLinkIsParsed(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(newMegaRACHandler(t))
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
		// Before the gofish upgrade this is:
		//   json: cannot unmarshal object into Go struct field .OperatingSystem of type string
		t.Fatalf("GetSystemInfo with a link-shaped OperatingSystem: %v", err)
	}

	if info.Manufacturer != "AMI" {
		t.Errorf("Manufacturer: want %q, got %q", "AMI", info.Manufacturer)
	}
	if info.Model != "MegaRAC SP-X" {
		t.Errorf("Model: want %q, got %q", "MegaRAC SP-X", info.Model)
	}
	if info.SerialNumber != "MEGARAC-0001" {
		t.Errorf("SerialNumber: want %q, got %q", "MEGARAC-0001", info.SerialNumber)
	}
}

// TestOperatingSystemAsLinkPowerState covers the other read our controllers
// make on every reconcile. It shares the failure with GetSystemInfo — both go
// through Systems() — so it guards the path that actually kept hosts stuck.
func TestOperatingSystemAsLinkPowerState(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(newMegaRACHandler(t))
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
		t.Fatalf("GetPowerState with a link-shaped OperatingSystem: %v", err)
	}
	if string(state) != "On" {
		t.Errorf("PowerState: want %q, got %q", "On", string(state))
	}
}
