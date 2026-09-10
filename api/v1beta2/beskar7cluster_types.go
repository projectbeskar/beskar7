package v1beta2

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// Beskar7Cluster specific conditions
const (
	// ControlPlaneEndpointReady documents the availability of the Control Plane Endpoint.
	ControlPlaneEndpointReady = "ControlPlaneEndpointReady"
)

// Beskar7Cluster condition reasons
const (
	// ControlPlaneEndpointNotSetReason indicates the ControlPlaneEndpoint is not defined in the spec.
	ControlPlaneEndpointNotSetReason = "ControlPlaneEndpointNotSet"
	// ControlPlaneEndpointSetReason (ControlPlaneEndpointReady=True): the endpoint is defined.
	ControlPlaneEndpointSetReason = "ControlPlaneEndpointSet"
)

// Beskar7ClusterSpec defines the desired state of Beskar7Cluster.
type Beskar7ClusterSpec struct {
	// ControlPlaneEndpoint represents the endpoint used to communicate with the control plane.
	// +kubebuilder:validation:Optional
	// +optional
	// omitzero: an endpoint is legitimately absent until the control plane has
	// an address, and the v1beta2 APIEndpoint schema rejects an empty object.
	ControlPlaneEndpoint clusterv1.APIEndpoint `json:"controlPlaneEndpoint,omitempty,omitzero"`
}

// Beskar7ClusterInitializationStatus carries CAPI v1beta2 contract fields
// describing one-shot initialisation milestones of the infrastructure cluster.
// See the CAPI proposal "improve-status-in-CAPI-resources" — CAPI core lifts
// `status.initialization.provisioned` from the InfrastructureCluster up into
// the parent Cluster's `status.initialization.infrastructureProvisioned`.
// +kubebuilder:validation:MinProperties=1
type Beskar7ClusterInitializationStatus struct {
	// Provisioned is true when the infrastructure cluster is fully initialised:
	// the control-plane endpoint is reachable and any failure domains have been
	// surfaced. The Beskar7Cluster controller sets this in lockstep with
	// status.ready=true.
	// +optional
	Provisioned *bool `json:"provisioned,omitempty"`
}

// Beskar7ClusterStatus defines the observed state of Beskar7Cluster.
type Beskar7ClusterStatus struct {
	// Ready indicates that the cluster is ready.
	// +optional
	Ready bool `json:"ready,omitempty"`

	// Initialization carries CAPI v1beta2 contract initialisation milestones.
	// On CAPI v1.10+ the KubeadmConfig and Machine controllers gate on
	// `Cluster.status.initialization.infrastructureProvisioned`, which CAPI
	// core derives from this nested field. Without it the bootstrap data
	// secret is never generated, and the Beskar7Machine bootstrap token mint
	// (which depends on a non-empty Machine.Spec.Bootstrap.DataSecretName)
	// never fires either. Required for end-to-end CAPI lifecycle.
	// +optional
	Initialization Beskar7ClusterInitializationStatus `json:"initialization,omitempty,omitzero"`

	// ControlPlaneEndpoint represents the endpoint used to communicate with the control plane.
	// +optional
	ControlPlaneEndpoint clusterv1.APIEndpoint `json:"controlPlaneEndpoint,omitempty,omitzero"`

	// FailureDomains is a list of failure domain objects synced from the infrastructure provider.
	// +optional
	FailureDomains []clusterv1.FailureDomain `json:"failureDomains,omitempty"`

	// Conditions defines current service state of the Beskar7Cluster. Ready is
	// the summary condition Cluster API mirrors into the owning Cluster.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=32
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:path=beskar7clusters,scope=Namespaced,categories=cluster-api
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Cluster",type="string",JSONPath=".metadata.labels.cluster\\.x-k8s\\.io/cluster-name",description="Cluster to which this Beskar7Cluster belongs"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.ready",description="Beskar7Cluster ready status"
// +kubebuilder:printcolumn:name="Endpoint",type="string",JSONPath=".spec.controlPlaneEndpoint.host",description="Control plane endpoint"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description="Time duration since creation of Beskar7Cluster"
// +kubebuilder:object:generate=true
// +kubebuilder:storageversion
// +kubebuilder:metadata:labels=cluster.x-k8s.io/v1beta2=v1beta2
// +kubebuilder:metadata:labels="clusterctl.cluster.x-k8s.io="
// +kubebuilder:metadata:labels=cluster.x-k8s.io/provider=infrastructure-beskar7
// Beskar7Cluster is the Schema for the beskar7clusters API.
//
// clusterctl.cluster.x-k8s.io is clusterctl's discovery label: `clusterctl move`
// builds its object graph only from CRDs that carry it (getCRDList in
// cluster-api's cmd/clusterctl/client/cluster/objectgraph.go). `clusterctl init`
// adds it to everything it installs, but a Helm or release-manifest install
// never goes through clusterctl, so the generated CRD carries it itself.
// cluster.x-k8s.io/provider is the provider contract's component label, with the
// value clusterctl derives for an infrastructure provider (infrastructure-<name>).
type Beskar7Cluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   Beskar7ClusterSpec   `json:"spec,omitempty"`
	Status Beskar7ClusterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// Beskar7ClusterList contains a list of Beskar7Cluster.
type Beskar7ClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Beskar7Cluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Beskar7Cluster{}, &Beskar7ClusterList{})
}

// GetConditions returns the conditions of the Beskar7Cluster (conditions.Getter).
func (c *Beskar7Cluster) GetConditions() []metav1.Condition {
	return c.Status.Conditions
}

// SetConditions sets the conditions of the Beskar7Cluster (conditions.Setter).
func (c *Beskar7Cluster) SetConditions(conditions []metav1.Condition) {
	c.Status.Conditions = conditions
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *Beskar7ClusterStatus) DeepCopyInto(out *Beskar7ClusterStatus) {
	*out = *in
	out.ControlPlaneEndpoint = in.ControlPlaneEndpoint
	if in.FailureDomains != nil {
		in, out := &in.FailureDomains, &out.FailureDomains
		*out = make([]clusterv1.FailureDomain, len(*in))
		copy(*out, *in)
	}
	if in.Initialization.Provisioned != nil {
		in, out := &in.Initialization.Provisioned, &out.Initialization.Provisioned
		*out = new(bool)
		**out = **in
	}
	if in.Conditions != nil {
		in, out := &in.Conditions, &out.Conditions
		*out = make([]metav1.Condition, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *Beskar7ClusterSpec) DeepCopyInto(out *Beskar7ClusterSpec) {
	*out = *in
	out.ControlPlaneEndpoint = in.ControlPlaneEndpoint
}
