package redfish

import (
	"context"
	"sync"

	"github.com/stmcginnis/gofish/common"
	"github.com/stmcginnis/gofish/redfish"
)

// MockClient provides a mock implementation of the Client interface for testing.
// Simplified for iPXE + inspection workflow (no VirtualMedia).
type MockClient struct {
	mu sync.Mutex // Protect access to mock state

	// Mockable fields
	SystemInfo      *SystemInfo
	PowerState      redfish.PowerState
	ShouldFail      map[string]error // Map method name to error to simulate failures
	BootSourceIsPXE bool

	// Network address fields
	NetworkAddresses        []NetworkAddress
	GetNetworkAddressesFunc func(ctx context.Context) ([]NetworkAddress, error)

	// Counters (optional, for verification)
	CloseCalled                   bool
	GetSystemInfoCalled           bool
	GetPowerStateCalled           bool
	SetPowerStateCalled           bool
	SetBootSourcePXECalled        bool
	ResetCalled                   bool
	GetNetworkAddressesCalled     bool
	ForcePowerOffCalled           bool
	ClearBootSourceOverrideCalled bool
}

// NewMockClient creates a new mock client with default values.
func NewMockClient() *MockClient {
	return &MockClient{
		SystemInfo: &SystemInfo{
			Manufacturer: "MockInc",
			Model:        "MockSystem",
			SerialNumber: "MOCK12345",
			Status:       common.Status{State: common.EnabledState},
		},
		PowerState: redfish.OffPowerState,
		ShouldFail: make(map[string]error),
	}
}

// failIfNeeded checks if a method call should fail based on the ShouldFail map.
func (m *MockClient) failIfNeeded(methodName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err, ok := m.ShouldFail[methodName]; ok {
		return err
	}
	return nil
}

// GetSystemInfo mock implementation.
func (m *MockClient) GetSystemInfo(ctx context.Context) (*SystemInfo, error) {
	m.mu.Lock()
	m.GetSystemInfoCalled = true
	m.mu.Unlock()
	if err := m.failIfNeeded("GetSystemInfo"); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.SystemInfo, nil
}

// GetPowerState mock implementation.
func (m *MockClient) GetPowerState(ctx context.Context) (redfish.PowerState, error) {
	m.mu.Lock()
	m.GetPowerStateCalled = true
	m.mu.Unlock()
	if err := m.failIfNeeded("GetPowerState"); err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.PowerState, nil
}

// SetPowerState mock implementation.
func (m *MockClient) SetPowerState(ctx context.Context, state redfish.PowerState) error {
	m.mu.Lock()
	m.SetPowerStateCalled = true
	m.mu.Unlock()
	if err := m.failIfNeeded("SetPowerState"); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.PowerState = state
	return nil
}

// SetBootSourcePXE mock implementation.
func (m *MockClient) SetBootSourcePXE(ctx context.Context) error {
	m.mu.Lock()
	m.SetBootSourcePXECalled = true
	m.mu.Unlock()
	if err := m.failIfNeeded("SetBootSourcePXE"); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.BootSourceIsPXE = true
	return nil
}

// Reset mock implementation.
func (m *MockClient) Reset(ctx context.Context) error {
	m.mu.Lock()
	m.ResetCalled = true
	m.mu.Unlock()
	if err := m.failIfNeeded("Reset"); err != nil {
		return err
	}
	// Simulate a reset by cycling power state
	m.mu.Lock()
	defer m.mu.Unlock()
	m.PowerState = redfish.OnPowerState
	return nil
}

// GetNetworkAddresses mock implementation.
func (m *MockClient) GetNetworkAddresses(ctx context.Context) ([]NetworkAddress, error) {
	m.mu.Lock()
	m.GetNetworkAddressesCalled = true
	m.mu.Unlock()

	if err := m.failIfNeeded("GetNetworkAddresses"); err != nil {
		return nil, err
	}
	if m.GetNetworkAddressesFunc != nil {
		return m.GetNetworkAddressesFunc(ctx)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	// Return a copy of the stored network addresses
	addresses := make([]NetworkAddress, len(m.NetworkAddresses))
	copy(addresses, m.NetworkAddresses)
	return addresses, nil
}

// Close mock implementation.
func (m *MockClient) Close(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.CloseCalled = true
}

// ForcePowerOff mock implementation.
func (m *MockClient) ForcePowerOff(ctx context.Context) error {
	m.mu.Lock()
	m.ForcePowerOffCalled = true
	m.mu.Unlock()
	if err := m.failIfNeeded("ForcePowerOff"); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.PowerState = redfish.OffPowerState
	return nil
}

// ClearBootSourceOverride mock implementation.
func (m *MockClient) ClearBootSourceOverride(ctx context.Context) error {
	m.mu.Lock()
	m.ClearBootSourceOverrideCalled = true
	m.mu.Unlock()
	if err := m.failIfNeeded("ClearBootSourceOverride"); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.BootSourceIsPXE = false
	return nil
}
