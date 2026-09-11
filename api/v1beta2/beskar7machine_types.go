package v1beta2

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// Condition types on Beskar7Machine. All are metav1.Condition (Cluster API
// v1beta2 contract): Ready is the summary CAPI mirrors into the owning
// Machine's InfrastructureReady condition, and Paused reflects
// Cluster.spec.paused / the cluster.x-k8s.io/paused annotation.
const (
	// InfrastructureReadyCondition reports on the readiness of the infrastructure provider.
	InfrastructureReadyCondition = "InfrastructureReady"
	// PhysicalHostAssociatedCondition indicates whether the Beskar7Machine has
	// successfully associated with a PhysicalHost.
	PhysicalHostAssociatedCondition = "PhysicalHostAssociated"
	// BootstrapDataReadyCondition indicates the bootstrap data secret named by
	// Machine.Spec.Bootstrap.DataSecretName is present and the per-host bootstrap
	// URL has been signaled to the PhysicalHost.
	BootstrapDataReadyCondition = "BootstrapDataReady"
)

// Reasons for the True state of the Beskar7Machine conditions (metav1.Condition
// requires one) and for the terminal phase.
const (
	// ProvisionedReason (InfrastructureReady=True): the host is provisioned and
	// the ProviderID is set.
	ProvisionedReason = "Provisioned"
	// PhysicalHostAssociatedReason (PhysicalHostAssociated=True): a PhysicalHost
	// is claimed for this machine.
	PhysicalHostAssociatedReason = "PhysicalHostAssociated"
	// BootstrapDataReadyReason (BootstrapDataReady=True): the bootstrap data
	// Secret exists and the per-host bootstrap URL has been signalled.
	BootstrapDataReadyReason = "BootstrapDataReady"
	// PhaseFailed is the Status.Phase of a machine that hit a terminal failure.
	// It is the terminal marker: the controller stops reconciling such a machine
	// (deletion still works), and Ready/InfrastructureReady carry the reason.
	PhaseFailed = "Failed"
)

// Reasons for condition failures
const (
	// PhysicalHostAssociationFailedReason (Severity=Warning) indicates that the Beskar7Machine
	// failed to associate with a PhysicalHost.
	PhysicalHostAssociationFailedReason string = "PhysicalHostAssociationFailed"
	// WaitingForPhysicalHostReason (Severity=Info) indicates that the Beskar7Machine
	// is waiting for an available PhysicalHost to be claimed.
	WaitingForPhysicalHostReason string = "WaitingForPhysicalHost"

	// NoMatchingPhysicalHostReason (Severity=Info) documents that the machine is
	// waiting because no Available PhysicalHost satisfies its placement
	// constraint — its HostSelector, the failure domain CAPI assigned to the
	// owning Machine, or both — as opposed to WaitingForPhysicalHost, where the
	// inventory itself is empty.
	NoMatchingPhysicalHostReason string = "NoMatchingPhysicalHost"
	// WaitingForHostReason (Severity=Info) indicates waiting for a host (alias for compatibility)
	WaitingForHostReason string = "WaitingForHost"
	// PhysicalHostNotReadyReason (Severity=Info) indicates that the associated PhysicalHost
	// is not yet in a Ready state (e.g., still provisioning).
	PhysicalHostNotReadyReason string = "PhysicalHostNotReady"
	// PhysicalHostErrorReason (Severity=Error, terminal) indicates that the associated
	// PhysicalHost is in an Error state that needs a change to clear — refused or
	// missing credentials, a rejected certificate, a malformed address. An
	// unreachable BMC is not one of them: see WaitingForBMCReason.
	PhysicalHostErrorReason string = "PhysicalHostError"
	// WaitingForBMCReason (Severity=Info, not terminal) indicates that the associated
	// PhysicalHost cannot reach its BMC right now (its RedfishConnectionReady condition
	// is False with BMCUnreachable). An outage clears by itself and the host keeps
	// retrying, so the machine waits and carries on once the host is healthy again.
	WaitingForBMCReason string = "WaitingForBMC"
	// ReleasePhysicalHostFailedReason (Severity=Warning) indicates that releasing the
	// associated PhysicalHost failed during deletion.
	ReleasePhysicalHostFailedReason string = "ReleasePhysicalHostFailed"
	// WaitingForBootstrapDataReason (Severity=Info) indicates that
	// Machine.Spec.Bootstrap.DataSecretName is not yet set by the bootstrap provider.
	WaitingForBootstrapDataReason string = "WaitingForBootstrapData"
	// BootstrapDataUnavailableReason (Severity=Error, terminal) indicates that the named
	// bootstrap data Secret was not found in the Beskar7Machine's namespace.
	BootstrapDataUnavailableReason string = "BootstrapDataUnavailable"
	// HardwareRequirementsNotMetReason (Severity=Error, terminal) indicates that the
	// inspection report shows the host does not meet the Beskar7Machine's
	// HardwareRequirements (CPU/memory/disk). The BMC's hardware cannot change at
	// runtime, so this is terminal — the operator must lower the requirements,
	// allocate to a different host, or replace the hardware.
	HardwareRequirementsNotMetReason string = "HardwareRequirementsNotMet"
	// InvalidHostSelectorReason (Severity=Error, terminal) indicates that the
	// Beskar7Machine's HostSelector cannot be parsed (for example an unknown
	// matchExpressions operator). Such a selector can never match, so the spec
	// has to change: fix the template and roll the machine.
	InvalidHostSelectorReason string = "InvalidHostSelector"
	// InspectionTimedOutReason (Severity=Error, terminal) indicates that the inspection
	// image did not POST a report within DefaultInspectionTimeout. Likely causes:
	// misconfigured iPXE, host couldn't reach the manager's callback endpoint, or an
	// inspection image bug. Terminal because the controller has no way to recover
	// automatically; the operator must investigate and either delete-and-recreate the
	// Beskar7Machine or fix the iPXE setup.
	InspectionTimedOutReason string = "InspectionTimedOut"
	// DeploymentFailedReason (Severity=Error, terminal) indicates that the inspector
	// explicitly reported a deploy failure via POST /api/v1/provision-failed (v4.1).
	// Distinct from DeploymentTimedOut (timeout waiting for the callback) and from
	// PhysicalHostError (Redfish/BMC-level error). Use this reason when the inspector
	// itself signals that the image fetch, digest verify, disk write, or COS_OEM
	// inject failed and provisioning cannot proceed.
	DeploymentFailedReason string = "DeploymentFailed"
)

// Beskar7MachineSpec defines the desired state of Beskar7Machine.
// Simplified for iPXE + inspection workflow.
type Beskar7MachineSpec struct {
	// ProviderID is the unique identifier as specified by the cloud provider.
	// Format b7://<namespace>/<physicalhost-name>; set once the host is
	// provisioned, and mirrored by Cluster API into the owning Machine.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	ProviderID string `json:"providerID,omitempty"`

	// InspectionImageURL is the base URL of a location serving the inspection
	// image's boot artifacts. The controller renders an iPXE script that boots
	// "<InspectionImageURL>/vmlinuz" with "<InspectionImageURL>/initrd.img" as
	// the initrd (contract v2+ §4.1; see controllers/boot_handler.go). It is
	// not itself an iPXE script or a single kernel/initrd URL — both artifacts
	// must be reachable under this one base URL. The inspection image collects
	// hardware information and reports it back to Beskar7.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern="^https?://[^\\s]+$"
	InspectionImageURL string `json:"inspectionImageURL"`

	// TargetImageURL is the URL of the Kairos whole-disk raw image the inspector
	// writes to the target disk during provisioning. The image is served over
	// http(s); integrity is verified by the inspector against TargetImageDigest
	// (digest pinning, not TLS — see contract §8.1). Plain HTTP is permitted
	// because the digest is the sole integrity and authenticity anchor for the
	// OS image.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern="^https?://[^\\s]+$"
	TargetImageURL string `json:"targetImageURL"`

	// TargetImageDigest is the expected SHA-256 digest of the bytes at
	// TargetImageURL, formatted as "sha256:<64-lowercase-hex>". The inspector
	// verifies the written image against this digest and refuses to proceed to
	// mount, inject user-data, or reboot on a mismatch (contract §8.1). It is
	// the sole integrity and authenticity anchor for the OS image — the image
	// is fetched over plain HTTP and there is no signature; the operator must
	// compute this digest over the exact, pinned artifact served at TargetImageURL.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern="^sha256:[a-f0-9]{64}$"
	TargetImageDigest string `json:"targetImageDigest"`

	// TargetDisk optionally pins the disk the inspector writes the OS image to.
	// A stable device path (/dev/disk/by-id/..., /dev/disk/by-path/...) or a
	// kernel name (/dev/nvme0n1, sda). When set, the inspector uses exactly that
	// device and aborts (never falling back) if it is ineligible; when empty, the
	// inspector auto-selects the smallest eligible disk (contract §5 beskar7.disk,
	// §9.1 step 2). Non-secret.
	// +kubebuilder:validation:Pattern="^[A-Za-z0-9._:/+-]+$"
	// +optional
	TargetDisk string `json:"targetDisk,omitempty"`

	// StaticIP pins a static IPv4 address on the provisioning NIC instead of
	// DHCP. For use on DHCP-less or VLAN-pinned provisioning networks where no
	// DHCP server is present (contract v3 §5 beskar7.ip, §8.2).
	//
	// Format: the kernel ip= subset "<ip>::<gw>:<mask>[:<dns>]" where:
	//   - <ip>   dotted IPv4 (required)
	//   - ::     always-empty server-IP field (two colons; required separator)
	//   - <gw>   dotted IPv4 default gateway (optional; omit for a gateway-less net)
	//   - <mask> dotted IPv4 netmask (255.255.255.0) or bare CIDR prefix (0–32)
	//   - <dns>  dotted IPv4 resolver (optional)
	//
	// Examples:
	//   "192.168.150.10::192.168.150.1:255.255.255.0"
	//   "10.0.0.5:::24:8.8.8.8"
	//
	// When set, /boot renders this value as beskar7.ip=<value> on the inspector
	// kernel cmdline; the inspector then configures the selected NIC statically
	// and skips DHCP entirely. When empty, DHCP is used (the default).
	//
	// The inspector selects the NIC to configure by BOOTIF (if present on the
	// cmdline), then falls back to its single NIC or the DHCP-race winner on
	// multi-NIC hosts (§8.2). A multi-NIC host using beskar7.ip without BOOTIF
	// is rejected by the inspector.
	//
	// Non-secret. The controller validates this field server-side at render time
	// regardless of CRD admission (C-1a injection guard, SEC-7).
	// +kubebuilder:validation:Pattern=`^([0-9]{1,3}\.){3}[0-9]{1,3}::(([0-9]{1,3}\.){3}[0-9]{1,3})?:(([0-9]{1,3}\.){3}[0-9]{1,3}|[0-9]{1,2})(:([0-9]{1,3}\.){3}[0-9]{1,3})?$`
	// +optional
	StaticIP *string `json:"staticIP,omitempty"`

	// HardwareRequirements specifies minimum hardware requirements for this machine.
	// The inspection phase will validate against these requirements.
	// +optional
	HardwareRequirements *HardwareRequirements `json:"hardwareRequirements,omitempty"`

	// HostSelector restricts which PhysicalHosts this machine may claim, by the
	// hosts' labels. Standard label-selector semantics: matchLabels ANDed with
	// matchExpressions. An absent or empty selector allows any Available host,
	// which is the behaviour before this field existed. It is ANDed with the
	// failure domain CAPI assigns to the owning Machine, when there is one.
	// Use it to give a control plane and a worker pool disjoint inventories,
	// or to pin a pool to a rack. A selector that cannot be parsed is a
	// terminal failure (InvalidHostSelector): it can never match, so the spec
	// has to change.
	// +optional
	HostSelector *metav1.LabelSelector `json:"hostSelector,omitempty"`
}

// HardwareRequirements specifies hardware requirements for a machine.
type HardwareRequirements struct {
	// MinCPUCores is the minimum number of CPU cores required.
	// +optional
	// +kubebuilder:validation:Minimum=1
	MinCPUCores int32 `json:"minCPUCores,omitempty"`

	// MinMemoryGB is the minimum amount of memory in GB required.
	// +optional
	// +kubebuilder:validation:Minimum=1
	MinMemoryGB int32 `json:"minMemoryGB,omitempty"`

	// MinDiskGB is the minimum disk space in GB required.
	// +optional
	// +kubebuilder:validation:Minimum=1
	MinDiskGB int32 `json:"minDiskGB,omitempty"`
}

// Beskar7MachineInitializationStatus carries CAPI v1beta2 contract fields
// describing one-shot initialisation milestones of the infrastructure machine.
// CAPI core lifts `status.initialization.provisioned` from the
// InfrastructureMachine into the parent Machine's
// `status.initialization.infrastructureProvisioned`.
// +kubebuilder:validation:MinProperties=1
type Beskar7MachineInitializationStatus struct {
	// Provisioned is true when the machine is fully provisioned: the host has
	// been claimed, inspected, and the ProviderID is set. The Beskar7Machine
	// controller sets this in lockstep with status.ready=true.
	// +optional
	Provisioned *bool `json:"provisioned,omitempty"`
}

// Beskar7MachineStatus defines the observed state of Beskar7Machine.
type Beskar7MachineStatus struct {
	// Ready indicates whether the machine is ready
	Ready bool `json:"ready,omitempty"`

	// Initialization carries CAPI v1beta2 contract initialisation milestones.
	// On CAPI v1.10+ the Machine controller surfaces this into
	// `Machine.status.initialization.infrastructureProvisioned`. Without it,
	// CAPI never advances the Machine past Pending and never marks the parent
	// Cluster as available.
	// +optional
	Initialization Beskar7MachineInitializationStatus `json:"initialization,omitempty,omitzero"`

	// Phase represents the current phase of the machine
	Phase *string `json:"phase,omitempty"`

	// Addresses contains the associated addresses for the machine.
	Addresses []clusterv1.MachineAddress `json:"addresses,omitempty"`

	// Conditions defines current service state of the Beskar7Machine. Ready is
	// the summary condition Cluster API mirrors into the owning Machine; a
	// terminal failure is Ready=False and InfrastructureReady=False with the
	// failure reason, together with Phase=Failed.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=32
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:path=beskar7machines,scope=Namespaced,categories=cluster-api
// +kubebuilder:subresource:status
// +kubebuilder:storageversion
// +kubebuilder:printcolumn:name="Cluster",type="string",JSONPath=".metadata.labels.cluster\\.x-k8s\\.io/cluster-name",description="Cluster to which this Beskar7Machine belongs"
// +kubebuilder:printcolumn:name="Machine",type="string",JSONPath=".metadata.labels.cluster\\.x-k8s\\.io/machine-name",description="Machine to which this Beskar7Machine belongs"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase",description="Beskar7Machine phase"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description="Time duration since creation of Beskar7Machine"
// +kubebuilder:object:generate=true
// +kubebuilder:metadata:labels=cluster.x-k8s.io/v1beta2=v1beta2
// +kubebuilder:metadata:labels="clusterctl.cluster.x-k8s.io="
// +kubebuilder:metadata:labels=cluster.x-k8s.io/provider=infrastructure-beskar7
// Beskar7Machine is the Schema for the beskar7machines API.
//
// clusterctl.cluster.x-k8s.io and cluster.x-k8s.io/provider: see Beskar7Cluster.
type Beskar7Machine struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   Beskar7MachineSpec   `json:"spec,omitempty"`
	Status Beskar7MachineStatus `json:"status,omitempty"`
}

// GetConditions returns the conditions of the Beskar7Machine (conditions.Getter).
func (m *Beskar7Machine) GetConditions() []metav1.Condition {
	return m.Status.Conditions
}

// SetConditions sets the conditions of the Beskar7Machine (conditions.Setter).
func (m *Beskar7Machine) SetConditions(conditions []metav1.Condition) {
	m.Status.Conditions = conditions
}

// +kubebuilder:object:root=true

// Beskar7MachineList contains a list of Beskar7Machine.
type Beskar7MachineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Beskar7Machine `json:"items"`
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *Beskar7MachineSpec) DeepCopyInto(out *Beskar7MachineSpec) {
	*out = *in
	if in.StaticIP != nil {
		in, out := &in.StaticIP, &out.StaticIP
		*out = new(string)
		**out = **in
	}
	if in.HardwareRequirements != nil {
		in, out := &in.HardwareRequirements, &out.HardwareRequirements
		*out = new(HardwareRequirements)
		**out = **in
	}
	if in.HostSelector != nil {
		in, out := &in.HostSelector, &out.HostSelector
		*out = new(metav1.LabelSelector)
		(*in).DeepCopyInto(*out)
	}
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *HardwareRequirements) DeepCopyInto(out *HardwareRequirements) {
	*out = *in
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new HardwareRequirements.
func (in *HardwareRequirements) DeepCopy() *HardwareRequirements {
	if in == nil {
		return nil
	}
	out := new(HardwareRequirements)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *Beskar7MachineStatus) DeepCopyInto(out *Beskar7MachineStatus) {
	*out = *in
	if in.Phase != nil {
		in, out := &in.Phase, &out.Phase
		*out = new(string)
		**out = **in
	}
	if in.Initialization.Provisioned != nil {
		in, out := &in.Initialization.Provisioned, &out.Initialization.Provisioned
		*out = new(bool)
		**out = **in
	}
	if in.Addresses != nil {
		in, out := &in.Addresses, &out.Addresses
		*out = make([]clusterv1.MachineAddress, len(*in))
		copy(*out, *in)
	}
	if in.Conditions != nil {
		in, out := &in.Conditions, &out.Conditions
		*out = make([]metav1.Condition, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

func init() {
	SchemeBuilder.Register(&Beskar7Machine{}, &Beskar7MachineList{})
}
