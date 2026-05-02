package redfish

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/stmcginnis/gofish"
	"github.com/stmcginnis/gofish/redfish"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// gofishClient implements the Client interface using the gofish library.
type gofishClient struct {
	gofishClient *gofish.APIClient
	apiEndpoint  string // Store the original endpoint address
}

var log = logf.Log.WithName("redfish-client")

// NewClient creates a new Redfish client.
func NewClient(ctx context.Context, address, username, password string, insecure bool) (Client, error) {
	logger := logf.Log.WithName("redfish-client")
	logger.V(1).Info("Creating new Redfish client", "rawAddress", address, "insecure", insecure)

	// Parse and validate the address URL
	parsedURL, err := url.Parse(address)
	if err != nil {
		logger.Error(err, "Failed to parse provided Redfish address", "rawAddress", address)
		// If parsing fails, try adding https and parse again
		parsedURL, err = url.Parse("https://" + address)
		if err != nil {
			logger.Error(err, "Failed to parse Redfish address even after adding https scheme", "address", address)
			return nil, fmt.Errorf("invalid Redfish address format: %s: %w", address, err)
		}
	}

	// Ensure scheme is present
	if parsedURL.Scheme == "" {
		parsedURL.Scheme = "https" // Default to https
		logger.Info("Defaulted address scheme to https", "processedAddress", parsedURL.String())
	}

	// Use the validated and cleaned URL string
	endpointURL := parsedURL.String()

	config := gofish.ClientConfig{
		Endpoint:  endpointURL, // Use the processed URL
		Username:  username,
		Password:  password,
		Insecure:  insecure,
		BasicAuth: true,
	}

	// Log the final config before connecting. Username is omitted (SEC-4); use
	// PasswordProvided (bool) to diagnose "did the secret get read at all" without
	// emitting credentials.
	logger.V(1).Info("Attempting gofish.ConnectContext with config",
		"Endpoint", config.Endpoint,
		"PasswordProvided", (config.Password != ""),
		"Insecure", config.Insecure,
		"BasicAuth", config.BasicAuth)

	c, err := gofish.ConnectContext(ctx, config)
	if err != nil {
		logger.Error(err, "Failed to connect to Redfish endpoint", "address", endpointURL)
		return nil, fmt.Errorf("failed to connect to Redfish endpoint %s: %w", endpointURL, err)
	}

	logger.Info("Successfully connected to Redfish endpoint", "address", endpointURL)

	return &gofishClient{
		gofishClient: c,
		apiEndpoint:  endpointURL,
	}, nil
}

// NewClientWithHTTPClient creates a new Redfish client using a custom HTTP client.
// Note: The underlying gofish library does not currently accept a custom http.Client.
// This function preserves API compatibility for tests and callers that wish to provide
// a client (e.g., to disable TLS verification). The provided httpClient is ignored,
// and the 'insecure' parameter is used to control TLS verification instead.
func NewClientWithHTTPClient(
	ctx context.Context,
	address, username, password string,
	insecure bool,
	_ *http.Client,
) (Client, error) {
	return NewClient(ctx, address, username, password, insecure)
}

// Close disconnects the client.
func (c *gofishClient) Close(ctx context.Context) {
	if c.gofishClient != nil {
		log.Info("Disconnecting Redfish client", "address", c.apiEndpoint)
		c.gofishClient.Logout()
		c.gofishClient = nil
	}
}

// getSystemService retrieves the first ComputerSystem instance.
// Helper function to avoid repetition.
func (c *gofishClient) getSystemService(ctx context.Context) (*redfish.ComputerSystem, error) {
	if c.gofishClient == nil {
		return nil, fmt.Errorf("redfish client is not connected")
	}
	service := c.gofishClient.Service
	systems, err := service.Systems()
	if err != nil {
		log.Error(err, "Failed to retrieve systems from Redfish service root")
		return nil, fmt.Errorf("failed to retrieve systems: %w", err)
	}
	if len(systems) == 0 {
		log.Error(nil, "No systems found on Redfish endpoint")
		return nil, fmt.Errorf("no systems found")
	}
	if len(systems) > 1 {
		log.Info("Multiple systems found, using the first one", "systemID", systems[0].ID)
	}
	return systems[0], nil
}

// GetSystemInfo retrieves basic system details.
func (c *gofishClient) GetSystemInfo(ctx context.Context) (*SystemInfo, error) {
	system, err := c.getSystemService(ctx)
	if err != nil {
		return nil, err
	}

	info := &SystemInfo{
		Manufacturer: system.Manufacturer,
		Model:        system.Model,
		SerialNumber: system.SerialNumber,
		Status:       system.Status,
	}
	log.Info("Retrieved system info", "Manufacturer", info.Manufacturer, "Model", info.Model, "SerialNumber", info.SerialNumber, "Status", info.Status.State)
	return info, nil
}

// GetPowerState retrieves the current power state of the system.
func (c *gofishClient) GetPowerState(ctx context.Context) (redfish.PowerState, error) {
	system, err := c.getSystemService(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get system for power state check: %w", err)
	}
	log.Info("Retrieved power state", "state", system.PowerState)
	return system.PowerState, nil
}

// SetPowerState sets the desired power state of the system.
func (c *gofishClient) SetPowerState(ctx context.Context, state redfish.PowerState) error {
	system, err := c.getSystemService(ctx)
	if err != nil {
		return fmt.Errorf("failed to get system for setting power state: %w", err)
	}

	var resetType redfish.ResetType
	switch state {
	case redfish.OnPowerState:
		resetType = redfish.OnResetType
	case redfish.OffPowerState:
		resetType = redfish.ForceOffResetType
	default:
		// Try direct conversion if it matches a ResetType
		switch redfish.ResetType(state) {
		case redfish.OnResetType, redfish.ForceOffResetType, redfish.GracefulShutdownResetType, redfish.GracefulRestartResetType, redfish.ForceRestartResetType, redfish.NmiResetType, redfish.ForceOnResetType, redfish.PushPowerButtonResetType, redfish.PowerCycleResetType:
			resetType = redfish.ResetType(state)
		default:
			return fmt.Errorf("unsupported power state or reset type for SetPowerState: %s", state)
		}
	}

	log.Info("Attempting to set power state", "desiredState", state, "resetType", resetType)
	err = system.Reset(resetType)
	if err != nil {
		log.Error(err, "Failed to set power state", "desiredState", state)
		return fmt.Errorf("failed to set power state to %s: %w", state, err)
	}
	log.Info("Successfully requested power state change", "desiredState", state)
	return nil
}

// SetBootSourcePXE configures the system to boot from PXE/network for iPXE.
func (c *gofishClient) SetBootSourcePXE(ctx context.Context) error {
	system, err := c.getSystemService(ctx)
	if err != nil {
		return fmt.Errorf("failed to get system to set PXE boot: %w", err)
	}

	boot := redfish.Boot{
		BootSourceOverrideTarget:  redfish.PxeBootSourceOverrideTarget,
		BootSourceOverrideEnabled: redfish.OnceBootSourceOverrideEnabled,
	}
	log.Info("Attempting to set boot source override to PXE", "target", boot.BootSourceOverrideTarget, "enabled", boot.BootSourceOverrideEnabled)
	err = system.SetBoot(boot)
	if err != nil {
		log.Error(err, "Failed to set boot source override to PXE")
		return fmt.Errorf("failed to set boot source override to PXE: %w", err)
	}

	log.Info("Successfully set boot source to PXE")
	return nil
}

// Reset performs a system reset.
func (c *gofishClient) Reset(ctx context.Context) error {
	system, err := c.getSystemService(ctx)
	if err != nil {
		return fmt.Errorf("failed to get system for reset: %w", err)
	}

	log.Info("Attempting to reset system")
	err = system.Reset(redfish.ForceRestartResetType)
	if err != nil {
		log.Error(err, "Failed to reset system")
		return fmt.Errorf("failed to reset system: %w", err)
	}
	log.Info("Successfully reset system")
	return nil
}

// GetNetworkAddresses retrieves network interface addresses from the system.
func (c *gofishClient) GetNetworkAddresses(ctx context.Context) ([]NetworkAddress, error) {
	log := logf.FromContext(ctx)
	log.Info("Attempting to retrieve network addresses from Redfish")

	system, err := c.getSystemService(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get system for network address discovery: %w", err)
	}

	var addresses []NetworkAddress

	// Try to get EthernetInterfaces first (more common and reliable)
	ethernetInterfaces, err := system.EthernetInterfaces()
	if err != nil {
		log.Error(err, "Failed to retrieve ethernet interfaces")
		// Don't return error yet, try NetworkInterfaces as fallback
	} else {
		log.Info("Found ethernet interfaces", "count", len(ethernetInterfaces))
		for _, ethIntf := range ethernetInterfaces {
			interfaceAddresses := c.extractAddressesFromEthernetInterface(ctx, ethIntf)
			addresses = append(addresses, interfaceAddresses...)
		}
	}

	// If we didn't get addresses from EthernetInterfaces, try NetworkInterfaces
	if len(addresses) == 0 {
		log.Info("No addresses found via EthernetInterfaces, trying NetworkInterfaces fallback")
		networkInterfaces, err := system.NetworkInterfaces()
		if err != nil {
			log.Error(err, "Failed to retrieve network interfaces")
			return nil, fmt.Errorf("failed to retrieve both ethernet and network interfaces: %w", err)
		}
		log.Info("Found network interfaces", "count", len(networkInterfaces))
		for _, netIntf := range networkInterfaces {
			interfaceAddresses := c.extractAddressesFromNetworkInterface(ctx, netIntf)
			addresses = append(addresses, interfaceAddresses...)
		}
	}

	log.Info("Successfully retrieved network addresses", "totalAddresses", len(addresses))
	return addresses, nil
}

// extractAddressesFromEthernetInterface extracts network addresses from an EthernetInterface.
func (c *gofishClient) extractAddressesFromEthernetInterface(ctx context.Context, ethIntf *redfish.EthernetInterface) []NetworkAddress {
	log := logf.FromContext(ctx)
	var addresses []NetworkAddress

	// Extract IPv4 addresses
	for _, ipv4 := range ethIntf.IPv4Addresses {
		if ipv4.Address != "" {
			address := NetworkAddress{
				Type:          IPv4AddressType,
				Address:       ipv4.Address,
				Gateway:       ipv4.Gateway,
				InterfaceName: ethIntf.Name,
				MACAddress:    ethIntf.MACAddress,
			}
			addresses = append(addresses, address)
			log.V(1).Info("Found IPv4 address", "interface", ethIntf.Name, "address", ipv4.Address, "gateway", ipv4.Gateway)
		}
	}

	// Extract IPv6 addresses
	for _, ipv6 := range ethIntf.IPv6Addresses {
		if ipv6.Address != "" {
			address := NetworkAddress{
				Type:          IPv6AddressType,
				Address:       ipv6.Address,
				Gateway:       ethIntf.IPv6DefaultGateway,
				InterfaceName: ethIntf.Name,
				MACAddress:    ethIntf.MACAddress,
			}
			addresses = append(addresses, address)
			log.V(1).Info("Found IPv6 address", "interface", ethIntf.Name, "address", ipv6.Address, "gateway", ethIntf.IPv6DefaultGateway)
		}
	}

	return addresses
}

// extractAddressesFromNetworkInterface extracts network addresses from a NetworkInterface.
func (c *gofishClient) extractAddressesFromNetworkInterface(ctx context.Context, netIntf *redfish.NetworkInterface) []NetworkAddress {
	log := logf.FromContext(ctx)
	var addresses []NetworkAddress

	// NetworkInterface doesn't directly contain IP addresses like EthernetInterface
	// We need to check if it has associated ports or device functions that might contain address info
	log.V(1).Info("Extracting addresses from NetworkInterface", "interface", netIntf.Name, "id", netIntf.ID)

	// Try to get NetworkPorts from the NetworkInterface
	networkPorts, err := netIntf.NetworkPorts()
	if err != nil {
		log.V(1).Info("Could not retrieve NetworkPorts from NetworkInterface", "interface", netIntf.Name, "error", err)
	} else if len(networkPorts) > 0 {
		log.V(1).Info("Found NetworkPorts", "interface", netIntf.Name, "count", len(networkPorts))
		for _, port := range networkPorts {
			// NetworkPorts might have associated addresses in some implementations
			// Check the OEM or vendor-specific fields if available
			log.V(2).Info("Found NetworkPort", "port", port.ID, "physicalPortNumber", port.PhysicalPortNumber)
		}
	}

	// Try to get NetworkDeviceFunctions from the NetworkInterface
	networkDeviceFunctions, err := netIntf.NetworkDeviceFunctions()
	if err != nil {
		log.V(1).Info("Could not retrieve NetworkDeviceFunctions from NetworkInterface", "interface", netIntf.Name, "error", err)
	} else if len(networkDeviceFunctions) > 0 {
		log.V(1).Info("Found NetworkDeviceFunctions", "interface", netIntf.Name, "count", len(networkDeviceFunctions))
		for _, devFunc := range networkDeviceFunctions {
			// NetworkDeviceFunction might contain Ethernet information
			if devFunc.Ethernet.MACAddress != "" {
				log.V(1).Info("Found NetworkDeviceFunction with Ethernet",
					"devFunc", devFunc.ID,
					"macAddress", devFunc.Ethernet.MACAddress)

				// Some implementations might include IP addresses in the Ethernet structure
				// However, this is not standard - typically IP addresses are only in EthernetInterfaces
				// We log the discovery but cannot extract IP addresses from this structure
			}
		}
	}

	// Note: NetworkInterface, NetworkPorts, and NetworkDeviceFunctions typically don't contain
	// IP address information in standard Redfish schemas. IP addresses are usually only available
	// through EthernetInterfaces. This traversal is implemented for completeness and vendor-specific
	// implementations that might extend these resources with IP information.

	log.V(1).Info("NetworkInterface traversal complete - no IP addresses found (this is expected)", "interface", netIntf.Name)
	return addresses
}
