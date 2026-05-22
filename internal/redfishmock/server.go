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

// Package redfishmock provides a multi-vendor Redfish HTTP fake suitable for
// unit tests and in-cluster smoke testing. It has no build tags and no
// dependency on net/http/httptest, so it can be compiled into a standalone
// binary.
//
// Quick usage — unit tests (via compat helper):
//
//	srv := redfishmock.NewMockRedfishServerWithHTTPTest(redfishmock.VendorDell)
//	defer srv.Close()
//	// srv.GetURL() returns the httptest TLS server URL.
//
// Quick usage — standalone binary:
//
//	srv := redfishmock.NewMockRedfishServer(redfishmock.VendorGeneric)
//	http.ListenAndServe(":8080", srv.Handler())
package redfishmock

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Redfish URL constants
const (
	RedfishAPIRoot    = "/redfish/v1/"
	RedfishSystemPath = "/redfish/v1/Systems/1"
	BiosEnabledState  = "Enabled"
)

// System information constants
const (
	DefaultSystemID = "1"
)

// VendorType represents different hardware vendors.
type VendorType string

const (
	VendorDell       VendorType = "Dell Inc."
	VendorHPE        VendorType = "HPE"
	VendorLenovo     VendorType = "Lenovo"
	VendorSupermicro VendorType = "Supermicro"
	VendorGeneric    VendorType = "Generic"
)

// PowerState represents the current power state.
type PowerState string

const (
	PowerStateOff              PowerState = "Off"
	PowerStateOn               PowerState = "On"
	PowerStateForceOff         PowerState = "ForceOff"
	PowerStateForceRestart     PowerState = "ForceRestart"
	PowerStateGracefulShutdown PowerState = "GracefulShutdown"
	PowerStateGracefulRestart  PowerState = "GracefulRestart"
)

// BootSourceOverrideTarget represents boot override targets.
type BootSourceOverrideTarget string

const (
	BootSourceNone       BootSourceOverrideTarget = "None"
	BootSourcePxe        BootSourceOverrideTarget = "Pxe"
	BootSourceHdd        BootSourceOverrideTarget = "Hdd"
	BootSourceCd         BootSourceOverrideTarget = "Cd"
	BootSourceUefiTarget BootSourceOverrideTarget = "UefiTarget"
)

// MockRedfishServer is a stateful Redfish BMC fake. Create it with
// NewMockRedfishServer; serve it by passing Handler() to any net/http server.
//
// SystemInfo and BiosAttributes are exported so tests in other packages can
// inspect vendor-specific initialisation without needing accessor methods.
type MockRedfishServer struct {
	// SystemInfo holds the vendor-specific system snapshot set at construction.
	SystemInfo SystemInfo
	// BiosAttributes holds vendor-specific BIOS key-value pairs set at construction.
	BiosAttributes map[string]interface{}
	// AuthEnabled controls whether HTTP Basic Auth is enforced. Tests may
	// toggle this directly between requests.
	AuthEnabled bool

	vendor       VendorType
	mu           sync.RWMutex
	powerState   PowerState
	bootSource   BootSourceOverrideTarget
	virtualMedia []VirtualMedia
	failures     FailureConfig
	requestLog   []RequestLog
	credentials  map[string]string // username -> password
}

// SystemInfo represents basic system information.
type SystemInfo struct {
	Manufacturer   string
	Model          string
	SerialNumber   string
	ProcessorCount int
	MemoryGB       int
	PowerState     PowerState
	Health         string
	UUID           string
}

// VirtualMedia represents virtual media configuration.
type VirtualMedia struct {
	ID             string
	ImageURL       string
	Inserted       bool
	WriteProtected bool
	ConnectedVia   string
}

// FailureConfig configures various failure scenarios.
type FailureConfig struct {
	NetworkErrors   bool
	AuthFailures    bool
	SlowResponses   bool
	PartialFailures bool
	VendorQuirks    bool
	PowerFailures   bool
	MediaFailures   bool
}

// RequestLog tracks a single HTTP request for debugging.
type RequestLog struct {
	Timestamp time.Time
	Method    string
	URL       string
	Body      string
	Response  int
}

// NewMockRedfishServer creates a new MockRedfishServer initialised for the
// given vendor. The handler is ready to serve immediately via Handler(); no
// network listener is started. Use NewMockRedfishServerWithHTTPTest for the
// httptest-backed variant used by unit tests.
func NewMockRedfishServer(vendor VendorType) *MockRedfishServer {
	mrs := &MockRedfishServer{
		vendor:         vendor,
		powerState:     PowerStateOn,
		bootSource:     BootSourceNone,
		virtualMedia:   make([]VirtualMedia, 2),
		failures:       FailureConfig{},
		requestLog:     make([]RequestLog, 0),
		AuthEnabled:    true,
		credentials:    map[string]string{"admin": "password123"},
		BiosAttributes: make(map[string]interface{}),
	}

	mrs.virtualMedia[0] = VirtualMedia{ID: "CD", Inserted: false, WriteProtected: true, ConnectedVia: "URI"}
	mrs.virtualMedia[1] = VirtualMedia{ID: "USB", Inserted: false, WriteProtected: false, ConnectedVia: "URI"}

	mrs.initSystemInfo()
	mrs.initBIOSAttributes()

	return mrs
}

// Vendor returns the VendorType this server was initialised with.
func (mrs *MockRedfishServer) Vendor() VendorType {
	return mrs.vendor
}

// Handler returns the http.Handler that implements the Redfish API fake.
// Pass this to any net/http or httptest server.
func (mrs *MockRedfishServer) Handler() http.Handler {
	return http.HandlerFunc(mrs.handleRequest)
}

// SetFailureMode enables or disables failure scenarios.
func (mrs *MockRedfishServer) SetFailureMode(config FailureConfig) {
	mrs.mu.Lock()
	defer mrs.mu.Unlock()
	mrs.failures = config
}

// GetRequestLog returns a snapshot of the request log.
func (mrs *MockRedfishServer) GetRequestLog() []RequestLog {
	mrs.mu.RLock()
	defer mrs.mu.RUnlock()
	logCopy := make([]RequestLog, len(mrs.requestLog))
	copy(logCopy, mrs.requestLog)
	return logCopy
}

// SetCredentials replaces the single allowed username/password pair.
func (mrs *MockRedfishServer) SetCredentials(username, password string) {
	mrs.mu.Lock()
	defer mrs.mu.Unlock()
	mrs.credentials = map[string]string{username: password}
}

// DisableAuth disables HTTP Basic Auth enforcement.
func (mrs *MockRedfishServer) DisableAuth() {
	mrs.mu.Lock()
	defer mrs.mu.Unlock()
	mrs.AuthEnabled = false
}

// initSystemInfo sets vendor-specific system information.
func (mrs *MockRedfishServer) initSystemInfo() {
	switch mrs.vendor {
	case VendorDell:
		mrs.SystemInfo = SystemInfo{
			Manufacturer:   "Dell Inc.",
			Model:          "PowerEdge R750",
			SerialNumber:   "DELL123456789",
			ProcessorCount: 2,
			MemoryGB:       128,
			PowerState:     PowerStateOn,
			Health:         "OK",
			UUID:           "4c4c4544-0033-3310-8051-b4c04f4d3132",
		}
	case VendorHPE:
		mrs.SystemInfo = SystemInfo{
			Manufacturer:   "HPE",
			Model:          "ProLiant DL380 Gen10",
			SerialNumber:   "HPE987654321",
			ProcessorCount: 2,
			MemoryGB:       64,
			PowerState:     PowerStateOn,
			Health:         "OK",
			UUID:           "30373237-3132-584d-5131-333032584d51",
		}
	case VendorLenovo:
		mrs.SystemInfo = SystemInfo{
			Manufacturer:   "Lenovo",
			Model:          "ThinkSystem SR650",
			SerialNumber:   "LEN555666777",
			ProcessorCount: 2,
			MemoryGB:       96,
			PowerState:     PowerStateOn,
			Health:         "OK",
			UUID:           "01234567-89ab-cdef-0123-456789abcdef",
		}
	case VendorSupermicro:
		mrs.SystemInfo = SystemInfo{
			Manufacturer:   "Supermicro",
			Model:          "X12DPi-NT6",
			SerialNumber:   "SMC111222333",
			ProcessorCount: 2,
			MemoryGB:       256,
			PowerState:     PowerStateOn,
			Health:         "OK",
			UUID:           "fedcba98-7654-3210-fedc-ba9876543210",
		}
	default:
		mrs.SystemInfo = SystemInfo{
			Manufacturer:   "Generic Manufacturer",
			Model:          "Generic Server",
			SerialNumber:   "GEN000111222",
			ProcessorCount: 1,
			MemoryGB:       32,
			PowerState:     PowerStateOn,
			Health:         "OK",
			UUID:           "12345678-1234-5678-1234-123456789012",
		}
	}
}

// initBIOSAttributes sets vendor-specific BIOS key-value pairs.
func (mrs *MockRedfishServer) initBIOSAttributes() {
	switch mrs.vendor {
	case VendorDell:
		mrs.BiosAttributes["KernelArgs"] = ""
		mrs.BiosAttributes["BootMode"] = "Uefi"
		mrs.BiosAttributes["SecureBoot"] = BiosEnabledState
	case VendorHPE:
		mrs.BiosAttributes["BootOrderPolicy"] = "AttemptOnce"
		mrs.BiosAttributes["UefiOptimizedBoot"] = BiosEnabledState
		mrs.BiosAttributes["SecureBootStatus"] = BiosEnabledState
	case VendorLenovo:
		mrs.BiosAttributes["SystemBootSequence"] = "UEFI First"
		mrs.BiosAttributes["SecureBootEnable"] = BiosEnabledState
	case VendorSupermicro:
		mrs.BiosAttributes["BootFeature"] = "UEFI"
		mrs.BiosAttributes["QuietBoot"] = BiosEnabledState
	}
}

// handleRequest is the main HTTP request handler.
func (mrs *MockRedfishServer) handleRequest(w http.ResponseWriter, r *http.Request) {
	mrs.logRequest(r)

	// Simulate slow responses if configured.
	if mrs.failures.SlowResponses {
		time.Sleep(5 * time.Second)
	}

	// Simulate network errors for non-root requests so the client can still connect.
	if mrs.failures.NetworkErrors && r.URL.Path != RedfishAPIRoot {
		http.Error(w, "Network Error", http.StatusInternalServerError)
		return
	}

	// Per DSP0266 (Redfish spec) §9.1, the service root at /redfish/v1/ MUST
	// be accessible unauthenticated for service discovery. gofish relies on
	// this: ConnectContext does an initial GET /redfish/v1/ without an
	// Authorization header before it knows whether credentials are even
	// required. Authenticating the service root would make us incompatible
	// with the standard client and most real BMCs.
	requiresAuth := !(r.Method == http.MethodGet && r.URL.Path == RedfishAPIRoot)
	if requiresAuth && mrs.AuthEnabled && !mrs.authenticate(r) {
		w.Header().Set("WWW-Authenticate", `Basic realm="Redfish"`)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("OData-Version", "4.0")

	switch {
	case r.URL.Path == RedfishAPIRoot && r.Method == http.MethodGet:
		mrs.handleServiceRoot(w, r)
	case r.URL.Path == "/redfish/v1/Systems" && r.Method == http.MethodGet:
		mrs.handleSystemsCollection(w, r)
	case strings.HasPrefix(r.URL.Path, "/redfish/v1/Systems/") && r.Method == http.MethodGet:
		mrs.handleSystemGet(w, r)
	case r.URL.Path == RedfishSystemPath && (r.Method == http.MethodPatch || r.Method == http.MethodPost):
		w.WriteHeader(http.StatusNoContent)
	case strings.HasPrefix(r.URL.Path, "/redfish/v1/Systems/") && strings.HasSuffix(r.URL.Path, "/Actions/ComputerSystem.Reset") && r.Method == http.MethodPost:
		mrs.handleSystemReset(w, r)
	case r.URL.Path == "/redfish/v1/Managers" && r.Method == http.MethodGet:
		mrs.handleManagersCollection(w, r)
	case strings.HasPrefix(r.URL.Path, "/redfish/v1/Managers/") && strings.HasSuffix(r.URL.Path, "/VirtualMedia") && r.Method == http.MethodGet:
		mrs.handleManagerVirtualMediaCollection(w, r)
	case strings.HasPrefix(r.URL.Path, "/redfish/v1/Managers/") && strings.Contains(r.URL.Path, "/VirtualMedia/") && strings.Contains(r.URL.Path, "/Actions/"):
		w.WriteHeader(http.StatusNoContent)
	case strings.HasPrefix(r.URL.Path, "/redfish/v1/Managers/") && strings.Contains(r.URL.Path, "/VirtualMedia/") && r.Method == http.MethodGet:
		mrs.handleVirtualMediaGet(w, r)
	case strings.HasPrefix(r.URL.Path, "/redfish/v1/Managers/") && r.Method == http.MethodGet:
		mrs.handleManagerGet(w, r)
	case strings.Contains(r.URL.Path, "VirtualMedia"):
		w.WriteHeader(http.StatusNotFound)
	case strings.Contains(r.URL.Path, "Bios"):
		w.WriteHeader(http.StatusNotFound)
	default:
		http.NotFound(w, r)
	}
}

// authenticate validates HTTP Basic Auth credentials.
func (mrs *MockRedfishServer) authenticate(r *http.Request) bool {
	if mrs.failures.AuthFailures {
		return false
	}

	username, password, ok := r.BasicAuth()
	if !ok {
		return false
	}

	mrs.mu.RLock()
	defer mrs.mu.RUnlock()

	expected, exists := mrs.credentials[username]
	return exists && expected == password
}

// logRequest appends a minimal request record to the request log.
func (mrs *MockRedfishServer) logRequest(r *http.Request) {
	mrs.mu.Lock()
	defer mrs.mu.Unlock()

	body := ""
	if r.Body != nil {
		body = "[body omitted]"
	}

	mrs.requestLog = append(mrs.requestLog, RequestLog{
		Timestamp: time.Now(),
		Method:    r.Method,
		URL:       r.URL.String(),
		Body:      body,
		Response:  200,
	})
}

func (mrs *MockRedfishServer) handleServiceRoot(w http.ResponseWriter, _ *http.Request) {
	response := map[string]interface{}{
		"@odata.type":    "#ServiceRoot.v1_5_0.ServiceRoot",
		"@odata.id":      "/redfish/v1/",
		"Id":             "RootService",
		"Name":           "Root Service",
		"RedfishVersion": "1.6.1",
		"UUID":           mrs.SystemInfo.UUID,
		"Systems":        map[string]string{"@odata.id": "/redfish/v1/Systems"},
		"Managers":       map[string]string{"@odata.id": "/redfish/v1/Managers"},
	}
	if err := json.NewEncoder(w).Encode(response); err != nil {
		http.Error(w, "encode error", http.StatusInternalServerError)
	}
}

func (mrs *MockRedfishServer) handleSystemsCollection(w http.ResponseWriter, _ *http.Request) {
	response := map[string]interface{}{
		"@odata.type":         "#ComputerSystemCollection.ComputerSystemCollection",
		"@odata.id":           "/redfish/v1/Systems",
		"Name":                "Computer System Collection",
		"Members@odata.count": 1,
		"Members":             []map[string]string{{"@odata.id": "/redfish/v1/Systems/1"}},
	}
	if err := json.NewEncoder(w).Encode(response); err != nil {
		http.Error(w, "encode error", http.StatusInternalServerError)
	}
}

func (mrs *MockRedfishServer) handleManagersCollection(w http.ResponseWriter, _ *http.Request) {
	response := map[string]interface{}{
		"@odata.type":         "#ManagerCollection.ManagerCollection",
		"@odata.id":           "/redfish/v1/Managers",
		"Name":                "Manager Collection",
		"Members@odata.count": 1,
		"Members":             []map[string]string{{"@odata.id": "/redfish/v1/Managers/1"}},
	}
	if err := json.NewEncoder(w).Encode(response); err != nil {
		http.Error(w, "encode error", http.StatusInternalServerError)
	}
}

func (mrs *MockRedfishServer) handleManagerGet(w http.ResponseWriter, _ *http.Request) {
	response := map[string]interface{}{
		"@odata.type": "#Manager.v1_9_0.Manager",
		"@odata.id":   "/redfish/v1/Managers/1",
		"Id":          "1",
		"Name":        "BMCManager",
		"Actions": map[string]interface{}{
			"#Manager.Reset": map[string]string{
				"target": "/redfish/v1/Managers/1/Actions/Manager.Reset",
			},
		},
		"VirtualMedia": map[string]string{"@odata.id": "/redfish/v1/Managers/1/VirtualMedia"},
	}
	if err := json.NewEncoder(w).Encode(response); err != nil {
		http.Error(w, "encode error", http.StatusInternalServerError)
	}
}

func (mrs *MockRedfishServer) handleManagerVirtualMediaCollection(w http.ResponseWriter, _ *http.Request) {
	response := map[string]interface{}{
		"@odata.type":         "#VirtualMediaCollection.VirtualMediaCollection",
		"@odata.id":           "/redfish/v1/Managers/1/VirtualMedia",
		"Name":                "Virtual Media Collection",
		"Members@odata.count": 1,
		"Members":             []map[string]string{{"@odata.id": "/redfish/v1/Managers/1/VirtualMedia/1"}},
	}
	if err := json.NewEncoder(w).Encode(response); err != nil {
		http.Error(w, "encode error", http.StatusInternalServerError)
	}
}

func (mrs *MockRedfishServer) handleVirtualMediaGet(w http.ResponseWriter, _ *http.Request) {
	response := map[string]interface{}{
		"@odata.type":    "#VirtualMedia.v1_5_0.VirtualMedia",
		"@odata.id":      "/redfish/v1/Managers/1/VirtualMedia/1",
		"Id":             "1",
		"Name":           "Virtual CD",
		"MediaTypes":     []string{"CD", "DVD"},
		"Image":          "",
		"Inserted":       false,
		"WriteProtected": true,
		"ConnectedVia":   "URI",
		"Actions": map[string]map[string]string{
			"#VirtualMedia.InsertMedia": {"target": "/redfish/v1/Managers/1/VirtualMedia/1/Actions/VirtualMedia.InsertMedia"},
			"#VirtualMedia.EjectMedia":  {"target": "/redfish/v1/Managers/1/VirtualMedia/1/Actions/VirtualMedia.EjectMedia"},
		},
	}
	if err := json.NewEncoder(w).Encode(response); err != nil {
		http.Error(w, "encode error", http.StatusInternalServerError)
	}
}

func (mrs *MockRedfishServer) handleSystemGet(w http.ResponseWriter, _ *http.Request) {
	mrs.mu.RLock()
	defer mrs.mu.RUnlock()

	response := map[string]interface{}{
		"@odata.type":  "#ComputerSystem.v1_10_0.ComputerSystem",
		"@odata.id":    "/redfish/v1/Systems/1",
		"Id":           "1",
		"Name":         "System",
		"SystemType":   "Physical",
		"Manufacturer": mrs.SystemInfo.Manufacturer,
		"Model":        mrs.SystemInfo.Model,
		"SerialNumber": mrs.SystemInfo.SerialNumber,
		"UUID":         mrs.SystemInfo.UUID,
		"ProcessorSummary": map[string]interface{}{
			"Count": mrs.SystemInfo.ProcessorCount,
		},
		"MemorySummary": map[string]interface{}{
			"TotalSystemMemoryGiB": mrs.SystemInfo.MemoryGB,
		},
		"PowerState": mrs.powerState,
		"Status": map[string]string{
			"State":  BiosEnabledState,
			"Health": mrs.SystemInfo.Health,
		},
		"Boot": map[string]interface{}{
			"BootSourceOverrideTarget":  mrs.bootSource,
			"BootSourceOverrideEnabled": "Once",
		},
		"Actions": map[string]interface{}{
			"#ComputerSystem.Reset": map[string]string{
				"target": "/redfish/v1/Systems/1/Actions/ComputerSystem.Reset",
			},
		},
		"Bios": map[string]string{"@odata.id": "/redfish/v1/Systems/1/Bios"},
	}
	if err := json.NewEncoder(w).Encode(response); err != nil {
		http.Error(w, "encode error", http.StatusInternalServerError)
	}
}

func (mrs *MockRedfishServer) handleSystemReset(w http.ResponseWriter, r *http.Request) {
	if mrs.failures.PowerFailures {
		http.Error(w, "Power operation failed", http.StatusInternalServerError)
		return
	}

	var req struct {
		ResetType string `json:"ResetType"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	mrs.mu.Lock()
	defer mrs.mu.Unlock()

	switch req.ResetType {
	case "On":
		mrs.powerState = PowerStateOn
	case "ForceOff":
		mrs.powerState = PowerStateOff
	case "GracefulShutdown":
		mrs.powerState = PowerStateOff
	case "ForceRestart":
		mrs.powerState = PowerStateOn
	default:
		http.Error(w, "Invalid ResetType", http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
