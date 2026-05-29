package v1beta1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
)

// PhysicalHost states - simplified for iPXE + inspection workflow
const (
	// StateNone is the default state before reconciliation
	StateNone = ""
	// StateUnknown indicates the host state could not be determined
	StateUnknown = "Unknown"
	// StateEnrolling indicates the controller is trying to establish connection
	StateEnrolling = "Enrolling"
	// StateAvailable indicates the host is ready to be claimed
	StateAvailable = "Available"
	// StateInUse indicates the host is claimed and being used by a Beskar7Machine
	StateInUse = "InUse"
	// StateInspecting indicates the inspection image is running on the host
	StateInspecting = "Inspecting"
	// StateReady indicates inspection is complete and host is ready for provisioning
	StateReady = "Ready"
	// StateError indicates the host is in an error state
	StateError = "Error"
)

// Inspection phases
const (
	InspectionPending    = "Pending"
	InspectionInProgress = "InProgress"
	InspectionComplete   = "Complete"
	InspectionFailed     = "Failed"
	InspectionTimeout    = "Timeout"
)

// RedfishConnection contains the information needed to connect to a Redfish service
type RedfishConnection struct {
	// Address is the URL of the Redfish service
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern="^(https?://)[a-zA-Z0-9.-]+(:[0-9]+)?(/.*)?$"
	Address string `json:"address"`

	// CredentialsSecretRef is the name of the secret containing the Redfish credentials
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	CredentialsSecretRef string `json:"credentialsSecretRef"`

	// InsecureSkipVerify determines whether to skip TLS certificate verification.
	// Mutually exclusive with CABundleSecretRef: setting both is rejected during
	// reconcile (a custom CA bundle and "skip verification" together is incoherent).
	// +kubebuilder:default=false
	// +optional
	InsecureSkipVerify *bool `json:"insecureSkipVerify,omitempty"`

	// CABundleSecretRef references a Secret in the same namespace as the
	// PhysicalHost containing PEM-encoded CA certificates used to verify
	// the BMC's TLS server certificate. The Secret data must include either
	// a "ca.crt" or "tls.crt" key. If both are present, "ca.crt" wins.
	//
	// When this field is set, the manager builds a custom *http.Client whose
	// TLS roots include the supplied bundle and passes it to gofish.
	// InsecureSkipVerify must be false (the default) — the two are mutually
	// exclusive; presence of CABundleSecretRef means "verify against this
	// bundle" and InsecureSkipVerify=true would defeat that.
	//
	// When unset, behaviour is unchanged: the manager builds an http.Client
	// whose TLS Transport uses system roots and the InsecureSkipVerify flag.
	// +optional
	CABundleSecretRef *corev1.LocalObjectReference `json:"caBundleSecretRef,omitempty"`
}

// HardwareDetails contains information about the physical host hardware
type HardwareDetails struct {
	// Manufacturer is the manufacturer of the physical host
	Manufacturer string `json:"manufacturer,omitempty"`

	// Model is the model of the physical host
	Model string `json:"model,omitempty"`

	// SerialNumber is the serial number of the physical host
	SerialNumber string `json:"serialNumber,omitempty"`

	// Status contains the current status of the host
	Status HardwareStatus `json:"status,omitempty"`
}

// HardwareStatus contains the current status of the host hardware
type HardwareStatus struct {
	// Health is the health status of the host
	// +optional
	Health string `json:"health,omitempty"`

	// HealthRollup is the overall health status
	// +optional
	HealthRollup string `json:"healthRollup,omitempty"`

	// State is the current state of the host
	// +optional
	State string `json:"state,omitempty"`
}

// PhysicalHostSpec defines the desired state of PhysicalHost
// Simplified for power management only - provisioning happens via iPXE + inspection.
type PhysicalHostSpec struct {
	// RedfishConnection contains the connection details for the Redfish endpoint
	RedfishConnection RedfishConnection `json:"redfishConnection"`

	// ConsumerRef is a reference to the Beskar7Machine that is using this host
	// +optional
	ConsumerRef *corev1.ObjectReference `json:"consumerRef,omitempty"`
}

// InspectionPhase represents the current phase of hardware inspection
type InspectionPhase string

const (
	// InspectionPhasePending indicates inspection has not started
	InspectionPhasePending InspectionPhase = "Pending"
	// InspectionPhaseBooting indicates the inspection image is booting
	InspectionPhaseBooting InspectionPhase = "Booting"
	// InspectionPhaseInProgress indicates inspection is actively running
	InspectionPhaseInProgress InspectionPhase = "InProgress"
	// InspectionPhaseComplete indicates inspection finished successfully
	InspectionPhaseComplete InspectionPhase = "Complete"
	// InspectionPhaseFailed indicates inspection encountered an error
	InspectionPhaseFailed InspectionPhase = "Failed"
	// InspectionPhaseTimeout indicates inspection did not complete in time
	InspectionPhaseTimeout InspectionPhase = "Timeout"
)

// InspectionReport contains hardware information collected during inspection
type InspectionReport struct {
	// Timestamp when the inspection was performed
	Timestamp metav1.Time `json:"timestamp"`

	// Manufacturer is the system manufacturer
	// +optional
	Manufacturer string `json:"manufacturer,omitempty"`

	// Model is the system model
	// +optional
	Model string `json:"model,omitempty"`

	// SerialNumber is the system serial number
	// +optional
	SerialNumber string `json:"serialNumber,omitempty"`

	// BootModeDetected is the boot mode detected by inspector (UEFI, Legacy)
	// +optional
	BootModeDetected string `json:"bootModeDetected,omitempty"`

	// FirmwareVersion is the BIOS/UEFI version
	// +optional
	FirmwareVersion string `json:"firmwareVersion,omitempty"`

	// CPUs contains CPU information (array of CPUs)
	// +optional
	CPUs []CPUInfo `json:"cpus,omitempty"`

	// Memory contains memory module information (array of DIMMs)
	// +optional
	Memory []MemoryInfo `json:"memory,omitempty"`

	// Disks contains information about storage devices
	// +optional
	Disks []DiskInfo `json:"disks,omitempty"`

	// NICs contains network interface information
	// +optional
	NICs []NICInfo `json:"nics,omitempty"`
}

// CPUInfo contains information about a CPU
type CPUInfo struct {
	// ID is the CPU identifier
	// +optional
	ID string `json:"id,omitempty"`

	// Vendor is the CPU vendor (e.g., GenuineIntel, AuthenticAMD)
	// +optional
	Vendor string `json:"vendor,omitempty"`

	// Model is the CPU model name
	// +optional
	Model string `json:"model,omitempty"`

	// Cores is the number of cores
	// +optional
	Cores int `json:"cores,omitempty"`

	// Threads is the number of threads
	// +optional
	Threads int `json:"threads,omitempty"`

	// Frequency is the CPU frequency (e.g., "3.1GHz")
	// +optional
	Frequency string `json:"frequency,omitempty"`
}

// MemoryInfo contains information about a memory module
type MemoryInfo struct {
	// ID is the memory module identifier (e.g., DIMM0)
	// +optional
	ID string `json:"id,omitempty"`

	// Type is the memory type (e.g., DDR4, DDR5)
	// +optional
	Type string `json:"type,omitempty"`

	// Capacity is the memory capacity (e.g., "32GB")
	// +optional
	Capacity string `json:"capacity,omitempty"`

	// Speed is the memory speed (e.g., "3200MHz")
	// +optional
	Speed string `json:"speed,omitempty"`
}

// DiskInfo contains information about a storage disk
type DiskInfo struct {
	// Name is the device name (e.g., /dev/sda, /dev/nvme0n1)
	// +optional
	Name string `json:"name,omitempty"`

	// Model is the disk model
	// +optional
	Model string `json:"model,omitempty"`

	// SizeGB is the disk size in GB
	// +optional
	SizeGB int `json:"sizeGB,omitempty"`

	// Type is the disk type (SSD, HDD, NVMe)
	// +optional
	Type string `json:"type,omitempty"`

	// SerialNumber is the disk serial number
	// +optional
	SerialNumber string `json:"serialNumber,omitempty"`
}

// NICInfo contains information about a network interface card
type NICInfo struct {
	// Name is the interface name (e.g., eth0, ens3)
	// +optional
	Name string `json:"name,omitempty"`

	// MACAddress is the MAC address
	// +optional
	MACAddress string `json:"macAddress,omitempty"`

	// Driver is the network driver name
	// +optional
	Driver string `json:"driver,omitempty"`

	// Speed is the link speed (e.g., "1Gbps", "10Gbps")
	// +optional
	Speed string `json:"speed,omitempty"`

	// IPAddresses are the IP addresses assigned to this interface
	// +optional
	IPAddresses []string `json:"ipAddresses,omitempty"`
}

// PhysicalHostStatus defines the observed state of PhysicalHost
type PhysicalHostStatus struct {
	// Ready indicates if the host is ready and enrolled
	// +optional
	Ready bool `json:"ready,omitempty"`

	// State represents the current state of the host
	// +optional
	State string `json:"state,omitempty"`

	// ObservedPowerState is the last observed power state from Redfish endpoint
	// +optional
	ObservedPowerState string `json:"observedPowerState,omitempty"`

	// ErrorMessage contains details on the last error encountered
	// +optional
	ErrorMessage string `json:"errorMessage,omitempty"`

	// HardwareDetails contains information about the physical host hardware
	// +optional
	HardwareDetails HardwareDetails `json:"hardwareDetails,omitempty"`

	// Addresses contains the associated addresses for the host
	// +optional
	Addresses []clusterv1.MachineAddress `json:"addresses,omitempty"`

	// InspectionReport contains hardware details from the inspection phase
	// +optional
	InspectionReport *InspectionReport `json:"inspectionReport,omitempty"`

	// InspectionPhase tracks the current inspection progress
	// +optional
	InspectionPhase InspectionPhase `json:"inspectionPhase,omitempty"`

	// InspectionTimestamp is when inspection started
	// +optional
	InspectionTimestamp *metav1.Time `json:"inspectionTimestamp,omitempty"`

	// Bootstrap holds the per-host data the inspection image and target OS use
	// to fetch bootstrap data (cloud-init / Ignition) from the manager, plus
	// the hashed credential the manager checks on each fetch.
	// +optional
	Bootstrap *BootstrapStatus `json:"bootstrap,omitempty"`

	// Conditions defines current service state of the PhysicalHost
	// +optional
	Conditions clusterv1.Conditions `json:"conditions,omitempty"`
}

// BootstrapStatus is the per-host bootstrap data fetch coordinates and the
// hashed credential used to authenticate inspection POSTs and bootstrap GETs.
// See decision D-004 in PROJECT_CONTEXT.md.
type BootstrapStatus struct {
	// URL is the manager-served HTTPS endpoint that returns the host's
	// bootstrap secret bytes. Computed deterministically from the manager's
	// bootstrap-url-base and the host's namespace+name.
	// +optional
	URL string `json:"url,omitempty"`

	// TokenHash is the hex-encoded SHA-256 of the per-host bearer token
	// used to authenticate inspection POSTs and bootstrap GETs. The
	// plaintext token is never persisted on this object — only the hash.
	// The hash by itself cannot be used to forge a valid bearer header,
	// so its presence in reconcile logs is acceptable.
	// +optional
	TokenHash string `json:"tokenHash,omitempty"`

	// IssuedAt is the time the current token was minted.
	// +optional
	IssuedAt *metav1.Time `json:"issuedAt,omitempty"`

	// ExpiresAt is the time the current token stops being accepted.
	// Defaults to IssuedAt + 30m at mint time (D-004).
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`

	// BootNonceHash is the hex-encoded SHA-256 of the per-host boot nonce
	// minted at inspection time (D-009). The nonce gates the
	// GET /api/v1/boot/{ns}/{host}/{nonce} endpoint. Only the hash is stored
	// here — the plaintext rides a per-host Secret under key "plaintext-boot-nonce".
	// The hash cannot be used to forge a valid nonce URL, so its presence
	// in reconcile logs is acceptable.
	// +optional
	BootNonceHash string `json:"bootNonceHash,omitempty"`

	// BootNonceExpiresAt is the time the current boot nonce stops being
	// accepted. Defaults to mint time + 10 min (BootNonceLifetime, D-009).
	// A shorter window than the bearer-token lifetime is intentional: the
	// nonce is single-use and consumed at first boot, so a long window only
	// widens the race window for a co-located attacker.
	// +optional
	BootNonceExpiresAt *metav1.Time `json:"bootNonceExpiresAt,omitempty"`

	// BootNonceConsumedAt is the timestamp at which the boot nonce was
	// single-use consumed by the GET /api/v1/boot handler (D-010). Nil
	// until that handler fires. Once set, the nonce is permanently spent:
	// no controller in this PR clears or re-uses it. A new nonce is minted
	// on every re-provision cycle (triggerInspection detects ConsumedAt != nil
	// and forces a fresh mint). Written exclusively by the /boot handler
	// (D-010) — nothing in this PR writes this field.
	// +optional
	BootNonceConsumedAt *metav1.Time `json:"bootNonceConsumedAt,omitempty"`
}

// Redfish conditions and reasons - simplified for power management only
const (
	RedfishConnectionReadyCondition clusterv1.ConditionType = "RedfishConnectionReady"
	HostAvailableCondition          clusterv1.ConditionType = "HostAvailable"
	HostInspectedCondition          clusterv1.ConditionType = "HostInspected"

	// Reasons
	MissingCredentialsReason      string = "MissingCredentials"
	SecretGetFailedReason         string = "SecretGetFailed"
	SecretNotFoundReason          string = "SecretNotFound"
	MissingSecretDataReason       string = "MissingSecretData"
	RedfishConnectionFailedReason string = "RedfishConnectionFailed"
	RedfishQueryFailedReason      string = "RedfishQueryFailed"
	PowerOnFailedReason           string = "PowerOnFailed"
	PowerOffFailedReason          string = "PowerOffFailed"
	SetBootPXEFailedReason        string = "SetBootPXEFailed"
	InspectionFailedReason        string = "InspectionFailed"
	InspectionTimeoutReason       string = "InspectionTimeout"
	// InsecureCABundleConflictReason is set when InsecureSkipVerify=true is
	// combined with CABundleSecretRef!=nil — the two are mutually exclusive
	// (a CA bundle and "skip verification" together is incoherent). The
	// reconciler rejects this configuration terminally.
	InsecureCABundleConflictReason string = "InsecureCABundleConflict"
	// CABundleFetchFailedReason is set when the CA bundle Secret cannot be
	// retrieved or does not contain a usable "ca.crt"/"tls.crt" data key.
	CABundleFetchFailedReason string = "CABundleFetchFailed"
)

// RedfishConnectionInfo contains the information needed to connect to a Redfish service
type RedfishConnectionInfo struct {
	// Address is the URL of the Redfish service
	Address string `json:"address"`

	// CredentialsSecretRef is the name of the secret containing the Redfish credentials
	CredentialsSecretRef string `json:"credentialsSecretRef"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:path=physicalhosts,scope=Namespaced,categories=cluster-api,shortName=ph
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="State",type="string",JSONPath=".status.state",description="Current state of the Physical Host"
// +kubebuilder:printcolumn:name="Ready",type="boolean",JSONPath=".status.ready",description="Indicates if the host is ready"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description="Creation timestamp"
// +kubebuilder:storageversion
// +kubebuilder:metadata:labels=cluster.x-k8s.io/v1beta1=v1beta1
// +kubebuilder:metadata:labels=cluster.x-k8s.io/v1beta2=v1beta1

// PhysicalHost is the Schema for the physicalhosts API
type PhysicalHost struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PhysicalHostSpec   `json:"spec,omitempty"`
	Status PhysicalHostStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PhysicalHostList contains a list of PhysicalHost
type PhysicalHostList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PhysicalHost `json:"items"`
}

// GetConditions returns the conditions for the PhysicalHost
func (h *PhysicalHost) GetConditions() clusterv1.Conditions {
	return h.Status.Conditions
}

// SetConditions sets the conditions for the PhysicalHost
func (h *PhysicalHost) SetConditions(conditions clusterv1.Conditions) {
	h.Status.Conditions = conditions
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *PhysicalHostSpec) DeepCopyInto(out *PhysicalHostSpec) {
	*out = *in
	// RedfishConnection contains pointer fields (InsecureSkipVerify,
	// CABundleSecretRef) — must use the type's own DeepCopyInto so the
	// pointers are not shared.
	in.RedfishConnection.DeepCopyInto(&out.RedfishConnection)
	if in.ConsumerRef != nil {
		in, out := &in.ConsumerRef, &out.ConsumerRef
		*out = new(corev1.ObjectReference)
		**out = **in
	}
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *PhysicalHostStatus) DeepCopyInto(out *PhysicalHostStatus) {
	*out = *in
	// HardwareDetails is a value type, so direct assignment is fine
	out.HardwareDetails = in.HardwareDetails
	if in.Addresses != nil {
		in, out := &in.Addresses, &out.Addresses
		*out = make([]clusterv1.MachineAddress, len(*in))
		copy(*out, *in)
	}
	if in.InspectionReport != nil {
		in, out := &in.InspectionReport, &out.InspectionReport
		*out = new(InspectionReport)
		(*in).DeepCopyInto(*out)
	}
	if in.InspectionTimestamp != nil {
		in, out := &in.InspectionTimestamp, &out.InspectionTimestamp
		*out = (*in).DeepCopy()
	}
	if in.Bootstrap != nil {
		in, out := &in.Bootstrap, &out.Bootstrap
		*out = new(BootstrapStatus)
		(*in).DeepCopyInto(*out)
	}
	if in.Conditions != nil {
		in, out := &in.Conditions, &out.Conditions
		*out = make(clusterv1.Conditions, len(*in))
		copy(*out, *in)
	}
}

// DeepCopyInto is an autogenerated deepcopy function for InspectionReport
func (in *InspectionReport) DeepCopyInto(out *InspectionReport) {
	*out = *in
	in.Timestamp.DeepCopyInto(&out.Timestamp)
	if in.CPUs != nil {
		in, out := &in.CPUs, &out.CPUs
		*out = make([]CPUInfo, len(*in))
		copy(*out, *in)
	}
	if in.Memory != nil {
		in, out := &in.Memory, &out.Memory
		*out = make([]MemoryInfo, len(*in))
		copy(*out, *in)
	}
	if in.Disks != nil {
		in, out := &in.Disks, &out.Disks
		*out = make([]DiskInfo, len(*in))
		copy(*out, *in)
	}
	if in.NICs != nil {
		in, out := &in.NICs, &out.NICs
		*out = make([]NICInfo, len(*in))
		copy(*out, *in)
	}
}

// DeepCopy is an autogenerated deepcopy function for InspectionReport
func (in *InspectionReport) DeepCopy() *InspectionReport {
	if in == nil {
		return nil
	}
	out := new(InspectionReport)
	in.DeepCopyInto(out)
	return out
}

func init() {
	SchemeBuilder.Register(&PhysicalHost{}, &PhysicalHostList{})
}
