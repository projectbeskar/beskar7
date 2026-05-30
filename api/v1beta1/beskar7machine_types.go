package v1beta1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
)

const (
	// InfrastructureReadyCondition reports on the readiness of the infrastructure provider.
	InfrastructureReadyCondition clusterv1.ConditionType = "InfrastructureReady"
	// PhysicalHostAssociatedCondition indicates whether the Beskar7Machine has
	// successfully associated with a PhysicalHost.
	PhysicalHostAssociatedCondition clusterv1.ConditionType = "PhysicalHostAssociated"
	// MachineProvisionedCondition indicates whether the machine has been provisioned
	MachineProvisionedCondition clusterv1.ConditionType = "MachineProvisioned"
	// BootstrapDataReadyCondition indicates the bootstrap data secret named by
	// Machine.Spec.Bootstrap.DataSecretName is present and the per-host bootstrap
	// URL has been signaled to the PhysicalHost.
	BootstrapDataReadyCondition clusterv1.ConditionType = "BootstrapDataReady"
)

// Reasons for condition failures
const (
	// PhysicalHostAssociationFailedReason (Severity=Warning) indicates that the Beskar7Machine
	// failed to associate with a PhysicalHost.
	PhysicalHostAssociationFailedReason string = "PhysicalHostAssociationFailed"
	// WaitingForPhysicalHostReason (Severity=Info) indicates that the Beskar7Machine
	// is waiting for an available PhysicalHost to be claimed.
	WaitingForPhysicalHostReason string = "WaitingForPhysicalHost"
	// WaitingForHostReason (Severity=Info) indicates waiting for a host (alias for compatibility)
	WaitingForHostReason string = "WaitingForHost"
	// PhysicalHostNotReadyReason (Severity=Info) indicates that the associated PhysicalHost
	// is not yet in a Ready state (e.g., still provisioning).
	PhysicalHostNotReadyReason string = "PhysicalHostNotReady"
	// PhysicalHostErrorReason (Severity=Error) indicates that the associated PhysicalHost
	// is in an Error state.
	PhysicalHostErrorReason string = "PhysicalHostError"
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
	// InspectionTimedOutReason (Severity=Error, terminal) indicates that the inspection
	// image did not POST a report within DefaultInspectionTimeout. Likely causes:
	// misconfigured iPXE, host couldn't reach the manager's callback endpoint, or an
	// inspection image bug. Terminal because the controller has no way to recover
	// automatically; the operator must investigate and either delete-and-recreate the
	// Beskar7Machine or fix the iPXE setup.
	InspectionTimedOutReason string = "InspectionTimedOut"
)

// Beskar7MachineSpec defines the desired state of Beskar7Machine.
// Simplified for iPXE + inspection workflow.
type Beskar7MachineSpec struct {
	// ProviderID is the unique identifier as specified by the cloud provider.
	// +optional
	ProviderID *string `json:"providerID,omitempty"`

	// InspectionImageURL is the iPXE boot script URL that boots the inspection image.
	// The inspection image will collect hardware information and report back to Beskar7.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern="^https?://[^\\s]+$"
	InspectionImageURL string `json:"inspectionImageURL"`

	// TargetImageURL is the URL of the final OS image to boot via kexec after inspection.
	// This should be a kernel+initrd or complete bootable image.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern="^https?://[^\\s]+$"
	TargetImageURL string `json:"targetImageURL"`

	// ConfigurationURL is an optional URL for OS-specific configuration.
	// The inspection image will pass this to the target OS during kexec.
	// +kubebuilder:validation:Pattern="^https?://.*"
	// +optional
	ConfigurationURL string `json:"configurationURL,omitempty"`

	// HardwareRequirements specifies minimum hardware requirements for this machine.
	// The inspection phase will validate against these requirements.
	// +optional
	HardwareRequirements *HardwareRequirements `json:"hardwareRequirements,omitempty"`
}

// HardwareRequirements specifies hardware requirements for a machine.
type HardwareRequirements struct {
	// MinCPUCores is the minimum number of CPU cores required.
	// +optional
	// +kubebuilder:validation:Minimum=1
	MinCPUCores int `json:"minCPUCores,omitempty"`

	// MinMemoryGB is the minimum amount of memory in GB required.
	// +optional
	// +kubebuilder:validation:Minimum=1
	MinMemoryGB int `json:"minMemoryGB,omitempty"`

	// MinDiskGB is the minimum disk space in GB required.
	// +optional
	// +kubebuilder:validation:Minimum=1
	MinDiskGB int `json:"minDiskGB,omitempty"`
}

// Beskar7MachineInitializationStatus carries CAPI v1beta2 contract fields
// describing one-shot initialisation milestones of the infrastructure machine.
// CAPI core lifts `status.initialization.provisioned` from the
// InfrastructureMachine into the parent Machine's
// `status.initialization.infrastructureProvisioned`.
type Beskar7MachineInitializationStatus struct {
	// Provisioned is true when the machine is fully provisioned: the host has
	// been claimed, inspected, and the ProviderID is set. The Beskar7Machine
	// controller sets this in lockstep with status.ready=true.
	// +optional
	Provisioned bool `json:"provisioned,omitempty"`
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
	Initialization *Beskar7MachineInitializationStatus `json:"initialization,omitempty"`

	// Phase represents the current phase of the machine
	Phase *string `json:"phase,omitempty"`

	// FailureReason will be set in the event that there is a terminal problem
	// reconciling the Machine and will contain a succinct value suitable
	// for machine interpretation.
	FailureReason *string `json:"failureReason,omitempty"`

	// FailureMessage will be set in the event that there is a terminal problem
	// reconciling the Machine and will contain a more verbose string suitable
	// for logging and human consumption.
	FailureMessage *string `json:"failureMessage,omitempty"`

	// Addresses contains the associated addresses for the machine.
	Addresses []clusterv1.MachineAddress `json:"addresses,omitempty"`

	// Conditions defines current service state of the Beskar7Machine.
	Conditions clusterv1.Conditions `json:"conditions,omitempty"`
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
// +kubebuilder:metadata:labels=cluster.x-k8s.io/v1beta1=v1beta1
// +kubebuilder:metadata:labels=cluster.x-k8s.io/v1beta2=v1beta1

// Beskar7Machine is the Schema for the beskar7machines API.
type Beskar7Machine struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   Beskar7MachineSpec   `json:"spec,omitempty"`
	Status Beskar7MachineStatus `json:"status,omitempty"`
}

// GetConditions returns the observations of the operational state of the Beskar7Machine resource.
func (m *Beskar7Machine) GetConditions() clusterv1.Conditions {
	return m.Status.Conditions
}

// SetConditions sets the underlying service state of the Beskar7Machine to the pre-defined clusterv1.Conditions.
func (m *Beskar7Machine) SetConditions(conditions clusterv1.Conditions) {
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
	if in.ProviderID != nil {
		in, out := &in.ProviderID, &out.ProviderID
		*out = new(string)
		**out = **in
	}
	if in.HardwareRequirements != nil {
		in, out := &in.HardwareRequirements, &out.HardwareRequirements
		*out = new(HardwareRequirements)
		**out = **in
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
	if in.FailureReason != nil {
		in, out := &in.FailureReason, &out.FailureReason
		*out = new(string)
		**out = **in
	}
	if in.FailureMessage != nil {
		in, out := &in.FailureMessage, &out.FailureMessage
		*out = new(string)
		**out = **in
	}
	if in.Addresses != nil {
		in, out := &in.Addresses, &out.Addresses
		*out = make([]clusterv1.MachineAddress, len(*in))
		copy(*out, *in)
	}
	if in.Conditions != nil {
		in, out := &in.Conditions, &out.Conditions
		*out = make(clusterv1.Conditions, len(*in))
		copy(*out, *in)
	}
}

func init() {
	SchemeBuilder.Register(&Beskar7Machine{}, &Beskar7MachineList{})
}
