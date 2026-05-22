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

package v1beta1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Beskar7MachineTemplateSpec defines the desired state of Beskar7MachineTemplate
// +kubebuilder:object:generate=true
type Beskar7MachineTemplateSpec struct {
	Template Beskar7MachineTemplateResource `json:"template"`
}

// Beskar7MachineTemplateResource defines the template resource for Beskar7Machine
// +kubebuilder:object:generate=true
type Beskar7MachineTemplateResource struct {
	// Spec is the specification of the desired behavior of the machine.
	Spec Beskar7MachineSpec `json:"spec"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:path=beskar7machinetemplates,scope=Namespaced,categories=cluster-api,shortName=b7mt
// +kubebuilder:storageversion
// +kubebuilder:metadata:labels=cluster.x-k8s.io/v1beta1=v1beta1
// +kubebuilder:metadata:labels=cluster.x-k8s.io/v1beta2=v1beta1

// Beskar7MachineTemplate is the Schema for the beskar7machinetemplates API.
//
// The `cluster-api` category is required for `clusterctl move`: CAPI walks
// resources in the `cluster-api` category when migrating a workload cluster
// between management clusters. Without it, Beskar7MachineTemplate objects
// would be left behind during a move (BUG-9 / Phase 8).
type Beskar7MachineTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec Beskar7MachineTemplateSpec `json:"spec,omitempty"`
}

//+kubebuilder:object:root=true

// Beskar7MachineTemplateList contains a list of Beskar7MachineTemplate
type Beskar7MachineTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Beskar7MachineTemplate `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Beskar7MachineTemplate{}, &Beskar7MachineTemplateList{})
}
