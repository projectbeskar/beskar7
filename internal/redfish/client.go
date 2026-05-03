package redfish

import (
	"context"
	"net"

	"github.com/stmcginnis/gofish/common"
	"github.com/stmcginnis/gofish/redfish"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
)

// Client represents a Redfish client - simplified for power management and iPXE boot only
type Client interface {
	// Close closes the client connection
	Close(ctx context.Context)

	// GetSystemInfo retrieves system information
	GetSystemInfo(ctx context.Context) (*SystemInfo, error)

	// GetPowerState retrieves the current power state
	GetPowerState(ctx context.Context) (redfish.PowerState, error)

	// SetPowerState sets the power state
	SetPowerState(ctx context.Context, state redfish.PowerState) error

	// SetBootSourcePXE configures the system to boot from PXE/network (iPXE)
	SetBootSourcePXE(ctx context.Context) error

	// Reset performs a system reset
	Reset(ctx context.Context) error

	// GetNetworkAddresses retrieves network interface addresses
	GetNetworkAddresses(ctx context.Context) ([]NetworkAddress, error)

	// ForcePowerOff forces an immediate power-off, bypassing OS shutdown.
	// Use only for unrecoverable error paths; prefer SetPowerState(Off) which
	// performs a graceful shutdown.
	ForcePowerOff(ctx context.Context) error

	// ClearBootSourceOverride disables any pending one-shot or persistent boot
	// source override. Used during host release so the next claimant is not
	// surprised by a stale PXE-once setting.
	ClearBootSourceOverride(ctx context.Context) error
}

// SystemInfo contains basic system information
type SystemInfo struct {
	Manufacturer string        `json:"manufacturer"`
	Model        string        `json:"model"`
	SerialNumber string        `json:"serialNumber"`
	Status       common.Status `json:"status"`
}

// NetworkAddressType represents the type of network address
type NetworkAddressType string

const (
	// IPv4AddressType represents an IPv4 address
	IPv4AddressType NetworkAddressType = "IPv4"
	// IPv6AddressType represents an IPv6 address
	IPv6AddressType NetworkAddressType = "IPv6"
)

// NetworkAddress represents a network interface address
type NetworkAddress struct {
	Type          NetworkAddressType `json:"type"`
	Address       string             `json:"address"`
	Gateway       string             `json:"gateway,omitempty"`
	InterfaceName string             `json:"interfaceName,omitempty"`
	MACAddress    string             `json:"macAddress,omitempty"`
}

// RedfishClientFactory defines the signature for a function that creates a Redfish client.
// It is defined here to be shared between PhysicalHost and Beskar7Machine controllers.
//
// caBundle, when non-empty, must be PEM-encoded CA certificates used to verify the
// BMC's TLS server certificate. When empty, the client falls back to system roots
// and honours the insecure flag. caBundle != nil with insecure == true is a
// programming error and is rejected at the factory layer.
type RedfishClientFactory func(ctx context.Context, address, username, password string, insecure bool, caBundle []byte) (Client, error)

// ConvertToMachineAddresses converts NetworkAddress slices to Cluster API MachineAddress format.
func ConvertToMachineAddresses(networkAddresses []NetworkAddress) []clusterv1.MachineAddress {
	var machineAddresses []clusterv1.MachineAddress

	for _, netAddr := range networkAddresses {
		// Determine the machine address type based on the network address
		var addrType clusterv1.MachineAddressType
		if netAddr.Type == IPv4AddressType {
			// Classify IPv4 addresses as Internal or External based on RFC 1918 private ranges
			if isPrivateIPv4(netAddr.Address) {
				addrType = clusterv1.MachineInternalIP
			} else {
				addrType = clusterv1.MachineExternalIP
			}
		} else if netAddr.Type == IPv6AddressType {
			// Classify IPv6 addresses as Internal or External based on RFC 4193 ULA and link-local
			if isPrivateIPv6(netAddr.Address) {
				addrType = clusterv1.MachineInternalIP
			} else {
				addrType = clusterv1.MachineExternalIP
			}
		} else {
			// Default to InternalIP for unknown types
			addrType = clusterv1.MachineInternalIP
		}

		machineAddr := clusterv1.MachineAddress{
			Type:    addrType,
			Address: netAddr.Address,
		}
		machineAddresses = append(machineAddresses, machineAddr)
	}

	return machineAddresses
}

// isPrivateIPv4 checks if an IPv4 address is in a private range (RFC 1918).
func isPrivateIPv4(ip string) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	privateCIDRs := []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"127.0.0.0/8",    // loopback
		"169.254.0.0/16", // link-local
	}
	for _, cidr := range privateCIDRs {
		if _, n, err := net.ParseCIDR(cidr); err == nil {
			if n.Contains(parsed) {
				return true
			}
		}
	}
	return false
}

// isPrivateIPv6 checks if an IPv6 address is in a private range.
func isPrivateIPv6(ip string) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	privateCIDRs := []string{
		"fc00::/7",  // Unique Local Address (ULA)
		"fe80::/10", // Link-local
		"::1/128",   // loopback
	}
	for _, cidr := range privateCIDRs {
		if _, n, err := net.ParseCIDR(cidr); err == nil {
			if n.Contains(parsed) {
				return true
			}
		}
	}
	// Treat other IPv6 addresses as external (public) by default
	return false
}
