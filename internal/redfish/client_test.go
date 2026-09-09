package redfish

import (
	"testing"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

func TestConvertToMachineAddresses(t *testing.T) {
	tests := []struct {
		name             string
		networkAddresses []NetworkAddress
		expected         []clusterv1.MachineAddress
	}{
		{
			name: "IPv4 private addresses",
			networkAddresses: []NetworkAddress{
				{
					Type:          IPv4AddressType,
					Address:       "192.168.1.100",
					Gateway:       "192.168.1.1",
					InterfaceName: "eth0",
					MACAddress:    "00:11:22:33:44:55",
				},
				{
					Type:          IPv4AddressType,
					Address:       "10.0.0.50",
					Gateway:       "10.0.0.1",
					InterfaceName: "eth1",
					MACAddress:    "00:11:22:33:44:66",
				},
			},
			expected: []clusterv1.MachineAddress{
				{
					Type:    clusterv1.MachineInternalIP,
					Address: "192.168.1.100",
				},
				{
					Type:    clusterv1.MachineInternalIP,
					Address: "10.0.0.50",
				},
			},
		},
		{
			name: "IPv4 public addresses",
			networkAddresses: []NetworkAddress{
				{
					Type:          IPv4AddressType,
					Address:       "8.8.8.8",
					Gateway:       "8.8.8.1",
					InterfaceName: "eth0",
					MACAddress:    "00:11:22:33:44:55",
				},
			},
			expected: []clusterv1.MachineAddress{
				{
					Type:    clusterv1.MachineExternalIP,
					Address: "8.8.8.8",
				},
			},
		},
		{
			name: "IPv6 addresses",
			networkAddresses: []NetworkAddress{
				{
					Type:          IPv6AddressType,
					Address:       "fe80::1234:5678:90ab:cdef",
					Gateway:       "",
					InterfaceName: "eth0",
					MACAddress:    "00:11:22:33:44:55",
				},
				{
					Type:          IPv6AddressType,
					Address:       "2001:db8::1",
					Gateway:       "2001:db8::ffff",
					InterfaceName: "eth1",
					MACAddress:    "00:11:22:33:44:66",
				},
			},
			expected: []clusterv1.MachineAddress{
				{
					Type:    clusterv1.MachineInternalIP,
					Address: "fe80::1234:5678:90ab:cdef",
				},
				{
					Type:    clusterv1.MachineExternalIP,
					Address: "2001:db8::1",
				},
			},
		},
		{
			name:             "empty input",
			networkAddresses: []NetworkAddress{},
			expected:         []clusterv1.MachineAddress{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ConvertToMachineAddresses(tt.networkAddresses)

			if len(result) != len(tt.expected) {
				t.Errorf("Expected %d addresses, got %d", len(tt.expected), len(result))
				return
			}

			for i, expected := range tt.expected {
				if result[i].Type != expected.Type {
					t.Errorf("Address %d: expected type %s, got %s", i, expected.Type, result[i].Type)
				}
				if result[i].Address != expected.Address {
					t.Errorf("Address %d: expected address %s, got %s", i, expected.Address, result[i].Address)
				}
			}
		})
	}
}

func TestIsPrivateIPv4(t *testing.T) {
	tests := []struct {
		ip       string
		expected bool
	}{
		{"192.168.1.1", true},
		{"10.0.0.1", true},
		{"172.16.0.1", true},
		{"127.0.0.1", true},
		{"169.254.1.1", true},
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"172.15.0.1", false}, // Just outside the private range
	}

	for _, tt := range tests {
		t.Run(tt.ip, func(t *testing.T) {
			result := isPrivateIPv4(tt.ip)
			if result != tt.expected {
				t.Errorf("For IP %s: expected %v, got %v", tt.ip, tt.expected, result)
			}
		})
	}
}

func TestIsPrivateIPv6(t *testing.T) {
	tests := []struct {
		ip       string
		expected bool
	}{
		{"fe80::1", true},      // Link-local
		{"fc00::1", true},      // ULA
		{"fd00::1", true},      // ULA
		{"::1", true},          // Loopback
		{"2001:db8::1", false}, // Global unicast (documentation range, but still public)
		{"2001:470::1", false}, // Global unicast (external)
	}

	for _, tt := range tests {
		t.Run(tt.ip, func(t *testing.T) {
			result := isPrivateIPv6(tt.ip)
			if result != tt.expected {
				t.Errorf("For IP %s: expected %v, got %v", tt.ip, tt.expected, result)
			}
		})
	}
}
