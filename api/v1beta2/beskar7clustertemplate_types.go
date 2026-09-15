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
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// Beskar7ClusterTemplateSpec defines the desired state of Beskar7ClusterTemplate.
// +kubebuilder:object:generate=true
type Beskar7ClusterTemplateSpec struct {
	Template Beskar7ClusterTemplateResource `json:"template"`
}

// Beskar7ClusterTemplateResource is the template Cluster API stamps out to create
// a Beskar7Cluster for a topology-managed cluster.
// +kubebuilder:object:generate=true
type Beskar7ClusterTemplateResource struct {
	// ObjectMeta carries labels and annotations to propagate onto each
	// Beskar7Cluster created from this template. Cluster API reads it at
	// `spec.template.metadata` (the InfrastructureClusterTemplate contract);
	// only labels and annotations are honoured, other ObjectMeta fields are
	// ignored.
	// +optional
	ObjectMeta clusterv1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// Spec is the Beskar7ClusterSpec each generated Beskar7Cluster starts from.
	//
	// It is normal for this to be empty. The only field Beskar7ClusterSpec has is
	// controlPlaneEndpoint, and that is exactly the field you should NOT template:
	// every cluster needs its own endpoint, and the Beskar7Cluster controller
	// discovers one from the control-plane Machines when it is left unset. Setting
	// it here would give every cluster in the ClusterClass the same endpoint.
	Spec Beskar7ClusterSpec `json:"spec"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:path=beskar7clustertemplates,scope=Namespaced,categories=cluster-api,shortName=b7ct
// +kubebuilder:storageversion
// +kubebuilder:metadata:labels=cluster.x-k8s.io/v1beta2=v1beta2
// +kubebuilder:metadata:labels="clusterctl.cluster.x-k8s.io="
// +kubebuilder:metadata:labels=cluster.x-k8s.io/provider=infrastructure-beskar7

// Beskar7ClusterTemplate is the Schema for the beskar7clustertemplates API.
//
// It exists so Beskar7 can be used from a ClusterClass: a ClusterClass points
// `spec.infrastructure.templateRef` at one of these, and Cluster API's topology
// controller creates a Beskar7Cluster per Cluster from it. Without this CRD a
// ClusterClass naming Beskar7 cannot be created at all — which is the whole
// reason it exists (#209).
//
// There is deliberately no controller. Templates are inert: Cluster API reads
// them, and nothing in Beskar7 reconciles them. See Beskar7MachineTemplate for
// the same pattern, and Beskar7Cluster for the contract labels.
type Beskar7ClusterTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec Beskar7ClusterTemplateSpec `json:"spec,omitempty"`
}

//+kubebuilder:object:root=true

// Beskar7ClusterTemplateList contains a list of Beskar7ClusterTemplate.
type Beskar7ClusterTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Beskar7ClusterTemplate `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Beskar7ClusterTemplate{}, &Beskar7ClusterTemplateList{})
}
