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

package v1beta2

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PhysicalHostPoolLabelPrefix is the prefix of the label that puts a PhysicalHost
// in a pool: <prefix><pool name>, any value. A host can carry several.
const PhysicalHostPoolLabelPrefix = "pool.beskar7.infrastructure.cluster.x-k8s.io/"

// PhysicalHostPoolFinalizer keeps a pool until no host is claimed through it.
const PhysicalHostPoolFinalizer = "physicalhostpool.infrastructure.cluster.x-k8s.io"

// PoolClaimValidCondition is True while a host a Beskar7Machine claimed through
// its pool is still a member of that pool and the pool still grants the
// machine's namespace. Set only on machines with poolRef that hold a host in
// another namespace; not part of the Ready summary.
const PoolClaimValidCondition = "PoolClaimValid"

// ClaimsValidCondition on a PhysicalHostPool is True while every claim made
// through the pool is still valid.
const ClaimsValidCondition = "ClaimsValid"

// PoolGrantRevokedReason marks a claim that lost its pool membership or grant.
// The host stays claimed and the node keeps running, but the callback server
// refuses the host; nothing is released automatically.
const PoolGrantRevokedReason = "PoolGrantRevoked"

// PoolClaimValidReason marks a claim that is still valid.
const PoolClaimValidReason = "PoolClaimValid"

// PoolNotAvailableReason marks a Beskar7Machine whose poolRef names a pool that
// does not exist, does not grant the machine's namespace, or is disabled on the
// manager. One reason for all three, so a namespace cannot probe which pools exist.
const PoolNotAvailableReason = "PoolNotAvailable"

// PhysicalHostPoolSpec defines which PhysicalHosts are in the pool and who may
// claim them.
type PhysicalHostPoolSpec struct {
	// HostNamespaces lists the namespaces the pool takes members from. A
	// PhysicalHost carrying the pool label is a member only when its namespace is
	// listed here, so a label on a host in any other namespace does nothing.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MaxLength=63
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +listType=set
	HostNamespaces []string `json:"hostNamespaces"`

	// NamespaceSelector selects the namespaces whose Beskar7Machines may claim
	// members of the pool, by the namespaces' labels. Required; an empty
	// selector ({}) selects every namespace.
	// +required
	NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector"`
}

// PhysicalHostPoolStatus is the observed state of a PhysicalHostPool.
type PhysicalHostPoolStatus struct {
	// Members counts the hosts that carry the pool label in hostNamespaces.
	// +optional
	// +kubebuilder:default=0
	Members int32 `json:"members"`
	// Available counts the members a new claim could take now: Available,
	// unclaimed, and wiped on release. A host in two pools counts in both.
	// +optional
	// +kubebuilder:default=0
	Available int32 `json:"available"`
	// Claimed counts the hosts claimed through this pool (spec.consumerPool).
	// +optional
	// +kubebuilder:default=0
	Claimed int32 `json:"claimed"`
	// Conditions: ClaimsValid.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:path=physicalhostpools,scope=Cluster,categories=cluster-api,shortName=php
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Members",type="integer",JSONPath=".status.members"
// +kubebuilder:printcolumn:name="Available",type="integer",JSONPath=".status.available"
// +kubebuilder:printcolumn:name="Claimed",type="integer",JSONPath=".status.claimed"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description="Creation timestamp"
// +kubebuilder:storageversion
// +kubebuilder:metadata:labels="clusterctl.cluster.x-k8s.io="
// +kubebuilder:metadata:labels=cluster.x-k8s.io/provider=infrastructure-beskar7
// +kubebuilder:validation:XValidation:rule="self.metadata.name.size() <= 63",message="the pool name is part of a label key, so it must be at most 63 characters"
// PhysicalHostPool shares PhysicalHosts across namespaces. Hosts join it with the
// label PhysicalHostPoolLabelPrefix + <pool name>; a Beskar7Machine opts in with
// spec.poolRef.
type PhysicalHostPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PhysicalHostPoolSpec   `json:"spec"`
	Status PhysicalHostPoolStatus `json:"status,omitempty"`
}

// GetConditions returns the conditions of the PhysicalHostPool (conditions.Getter).
func (p *PhysicalHostPool) GetConditions() []metav1.Condition {
	return p.Status.Conditions
}

// SetConditions sets the conditions of the PhysicalHostPool (conditions.Setter).
func (p *PhysicalHostPool) SetConditions(conditions []metav1.Condition) {
	p.Status.Conditions = conditions
}

// +kubebuilder:object:root=true

// PhysicalHostPoolList contains a list of PhysicalHostPool.
type PhysicalHostPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PhysicalHostPool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PhysicalHostPool{}, &PhysicalHostPoolList{})
}
