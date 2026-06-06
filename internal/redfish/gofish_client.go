package redfish

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/stmcginnis/gofish"
	"github.com/stmcginnis/gofish/redfish"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// defaultHTTPTimeout is the per-call HTTP timeout injected into the http.Client
// passed to gofish. A wedged BMC will be abandoned after this duration; the
// reconcile loop will requeue and retry.
const defaultHTTPTimeout = 30 * time.Second

// gofishClient implements the Client interface using the gofish library.
type gofishClient struct {
	gofishClient *gofish.APIClient
	apiEndpoint  string // Store the original endpoint address
}

var log = logf.Log.WithName("redfish-client")

// newHTTPClient builds an *http.Client that mirrors gofish's internal transport
// defaults while adding a per-call Timeout and honouring TLS configuration.
//
// When caBundle is non-empty, the returned client validates BMC certificates
// against the supplied PEM bundle (system roots are NOT additionally trusted —
// callers who want both must concatenate them upstream) and InsecureSkipVerify
// is forced to false regardless of the insecure flag. The caller is expected
// to have rejected the (insecure=true, caBundle!=nil) combination earlier;
// the factory rejects it explicitly to surface the programming error.
//
// When caBundle is empty, behaviour is the prior contract: system roots, with
// InsecureSkipVerify driven by the operator-opt-in flag.
//
// Returns an error only when caBundle is non-empty and contains no usable PEM
// certificates — silent fallback to system roots in that case would defeat the
// operator's intent.
func newHTTPClient(insecure bool, caBundle []byte) (*http.Client, error) {
	defaultTransport := http.DefaultTransport.(*http.Transport)
	tlsConfig := &tls.Config{
		// G402 — InsecureSkipVerify is operator-opt-in via Spec.RedfishConnection.InsecureSkipVerify.
		// The caBundle branch below overrides this to false unconditionally.
		InsecureSkipVerify: insecure, // #nosec G402
	}
	if len(caBundle) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caBundle) {
			return nil, fmt.Errorf("redfish CA bundle: no valid PEM certificates found")
		}
		tlsConfig.RootCAs = pool
		// A custom CA bundle and InsecureSkipVerify=true are incoherent.
		// The factory should have rejected this combination already; force
		// false here as defence in depth.
		tlsConfig.InsecureSkipVerify = false
	}
	transport := &http.Transport{
		Proxy:                 defaultTransport.Proxy,
		DialContext:           defaultTransport.DialContext,
		MaxIdleConns:          defaultTransport.MaxIdleConns,
		IdleConnTimeout:       defaultTransport.IdleConnTimeout,
		ExpectContinueTimeout: defaultTransport.ExpectContinueTimeout,
		TLSHandshakeTimeout:   10 * time.Second,
		TLSClientConfig:       tlsConfig,
	}
	return &http.Client{
		Timeout:   defaultHTTPTimeout,
		Transport: transport,
	}, nil
}

// doWithCtx runs op in a goroutine and returns either op's result or ctx.Err()
// if ctx is cancelled first. The goroutine continues to completion on cancellation
// (we cannot interrupt a synchronous gofish call mid-flight); its result is
// discarded. This is the simplest pattern that respects reconcile-context
// cancellation without requiring upstream gofish ctx support.
func doWithCtx(ctx context.Context, op func() error) error {
	done := make(chan error, 1)
	go func() {
		done <- op()
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// NewClient creates a new Redfish client.
//
// caBundle, when non-empty, is the PEM-encoded CA used to verify the BMC's
// TLS server certificate. caBundle != nil with insecure == true is rejected
// up front: the two are mutually exclusive and silently picking one would
// hide a likely operator misconfiguration.
func NewClient(ctx context.Context, address, username, password string, insecure bool, caBundle []byte) (Client, error) {
	logger := logf.Log.WithName("redfish-client")
	logger.V(1).Info("Creating new Redfish client", "rawAddress", address, "insecure", insecure, "caBundleProvided", len(caBundle) > 0)

	if insecure && len(caBundle) > 0 {
		return nil, fmt.Errorf("redfish: InsecureSkipVerify=true is mutually exclusive with a CA bundle; choose one")
	}

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

	httpClient, err := newHTTPClient(insecure, caBundle)
	if err != nil {
		return nil, fmt.Errorf("failed to build HTTP client for %s: %w", endpointURL, err)
	}

	config := gofish.ClientConfig{
		Endpoint:   endpointURL, // Use the processed URL
		Username:   username,
		Password:   password,
		Insecure:   insecure, // retained for documentation intent; HTTPClient below takes precedence
		BasicAuth:  true,
		HTTPClient: httpClient,
	}

	// Log the final config before connecting. Username is omitted (SEC-4); use
	// PasswordProvided (bool) to diagnose "did the secret get read at all" without
	// emitting credentials.
	logger.V(1).Info("Attempting gofish.ConnectContext with config",
		"Endpoint", config.Endpoint,
		"PasswordProvided", (config.Password != ""),
		"Insecure", config.Insecure,
		"CABundleProvided", len(caBundle) > 0,
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

// NewClientWithHTTPClient creates a Redfish client using the caller-supplied
// *http.Client wholesale. Used by integration tests that need to point at an
// httptest.Server with a self-signed cert and full transport control.
//
// The supplied httpClient is passed straight to gofish.ClientConfig.HTTPClient;
// the insecure flag is ignored beyond logging (caller has already configured
// the transport's TLSClientConfig). httpClient must not be nil — the whole
// point of this constructor is the explicit client.
func NewClientWithHTTPClient(
	ctx context.Context,
	address, username, password string,
	insecure bool,
	httpClient *http.Client,
) (Client, error) {
	logger := logf.Log.WithName("redfish-client")
	if httpClient == nil {
		return nil, fmt.Errorf("redfish: NewClientWithHTTPClient called with nil httpClient (programming error)")
	}
	logger.V(1).Info("Creating Redfish client with caller-supplied http.Client",
		"rawAddress", address, "insecure", insecure)

	parsedURL, err := url.Parse(address)
	if err != nil {
		parsedURL, err = url.Parse("https://" + address)
		if err != nil {
			return nil, fmt.Errorf("invalid Redfish address format: %s: %w", address, err)
		}
	}
	if parsedURL.Scheme == "" {
		parsedURL.Scheme = "https"
	}
	endpointURL := parsedURL.String()

	config := gofish.ClientConfig{
		Endpoint:   endpointURL,
		Username:   username,
		Password:   password,
		Insecure:   insecure,
		BasicAuth:  true,
		HTTPClient: httpClient,
	}
	logger.V(1).Info("Attempting gofish.ConnectContext with caller-supplied http.Client",
		"Endpoint", config.Endpoint,
		"PasswordProvided", (config.Password != ""),
		"Insecure", config.Insecure,
		"BasicAuth", config.BasicAuth)

	c, err := gofish.ConnectContext(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Redfish endpoint %s: %w", endpointURL, err)
	}

	return &gofishClient{
		gofishClient: c,
		apiEndpoint:  endpointURL,
	}, nil
}

// Close disconnects the client. It uses a fresh 5-second context so that a
// partitioned BMC doesn't block a deferred Close indefinitely, even when the
// caller's ctx is already cancelled.
func (c *gofishClient) Close(_ context.Context) {
	if c.gofishClient != nil {
		log.Info("Disconnecting Redfish client", "address", c.apiEndpoint)
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := doWithCtx(closeCtx, func() error {
			c.gofishClient.Logout()
			return nil
		}); err != nil {
			log.V(1).Info("Redfish logout did not complete cleanly", "address", c.apiEndpoint, "reason", err)
		}
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
	var systems []*redfish.ComputerSystem
	if err := doWithCtx(ctx, func() error {
		var inner error
		systems, inner = service.Systems()
		return inner
	}); err != nil {
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
// Mapping Off → GracefulShutdown ensures the OS can flush state before power
// is removed. Callers that need an immediate power-cut must use ForcePowerOff.
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
		resetType = redfish.GracefulShutdownResetType
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
	if err := doWithCtx(ctx, func() error { return system.Reset(resetType) }); err != nil {
		log.Error(err, "Failed to set power state", "desiredState", state)
		return fmt.Errorf("failed to set power state to %s: %w", state, err)
	}
	log.Info("Successfully requested power state change", "desiredState", state)
	return nil
}

// ForcePowerOff forces an immediate power-off, bypassing OS shutdown.
// Use only for unrecoverable error paths; prefer SetPowerState(Off) which
// performs a graceful shutdown.
func (c *gofishClient) ForcePowerOff(ctx context.Context) error {
	system, err := c.getSystemService(ctx)
	if err != nil {
		return fmt.Errorf("failed to get system for force power-off: %w", err)
	}
	log.Info("Forcing system power-off (bypassing graceful shutdown)")
	if err := doWithCtx(ctx, func() error { return system.Reset(redfish.ForceOffResetType) }); err != nil {
		log.Error(err, "Failed to force power-off")
		return fmt.Errorf("failed to force power-off: %w", err)
	}
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
	if err := doWithCtx(ctx, func() error { return system.SetBoot(boot) }); err != nil {
		log.Error(err, "Failed to set boot source override to PXE")
		return fmt.Errorf("failed to set boot source override to PXE: %w", err)
	}

	log.Info("Successfully set boot source to PXE")
	return nil
}

// ClearBootSourceOverride disables any pending one-shot or persistent boot
// source override so the next claimant is not surprised by a stale PXE-once
// setting left from the previous provisioning run.
func (c *gofishClient) ClearBootSourceOverride(ctx context.Context) error {
	system, err := c.getSystemService(ctx)
	if err != nil {
		return fmt.Errorf("failed to get system to clear boot source override: %w", err)
	}

	// Clearing a boot override requires BOTH fields: Enabled=Disabled AND
	// Target=None. Sending Enabled=Disabled alone is rejected by spec-strict
	// Redfish implementations (sushy-tools returns
	// "400: Missing the BootSourceOverrideTarget ... element"), which silently
	// stranded the post-provision boot-to-disk transition (D-015 #2) and the
	// release-path override clear. Target=None is the Redfish-canonical "no
	// override" target and reverts the host to its normal boot order.
	boot := redfish.Boot{
		BootSourceOverrideEnabled: redfish.DisabledBootSourceOverrideEnabled,
		BootSourceOverrideTarget:  redfish.NoneBootSourceOverrideTarget,
	}
	log.Info("Clearing boot source override")
	if err := doWithCtx(ctx, func() error { return system.SetBoot(boot) }); err != nil {
		log.Error(err, "Failed to clear boot source override")
		return fmt.Errorf("failed to clear boot source override: %w", err)
	}
	log.Info("Successfully cleared boot source override")
	return nil
}

// Reset performs a system reset.
func (c *gofishClient) Reset(ctx context.Context) error {
	system, err := c.getSystemService(ctx)
	if err != nil {
		return fmt.Errorf("failed to get system for reset: %w", err)
	}

	log.Info("Attempting to reset system")
	if err := doWithCtx(ctx, func() error { return system.Reset(redfish.ForceRestartResetType) }); err != nil {
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
	var ethernetInterfaces []*redfish.EthernetInterface
	if err := doWithCtx(ctx, func() error {
		var inner error
		ethernetInterfaces, inner = system.EthernetInterfaces()
		return inner
	}); err != nil {
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
		var networkInterfaces []*redfish.NetworkInterface
		if err := doWithCtx(ctx, func() error {
			var inner error
			networkInterfaces, inner = system.NetworkInterfaces()
			return inner
		}); err != nil {
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
	var networkPorts []*redfish.NetworkPort
	if err := doWithCtx(ctx, func() error {
		var inner error
		networkPorts, inner = netIntf.NetworkPorts()
		return inner
	}); err != nil {
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
	var networkDeviceFunctions []*redfish.NetworkDeviceFunction
	if err := doWithCtx(ctx, func() error {
		var inner error
		networkDeviceFunctions, inner = netIntf.NetworkDeviceFunctions()
		return inner
	}); err != nil {
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
