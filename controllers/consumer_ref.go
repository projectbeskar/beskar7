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

package controllers

import (
	"k8s.io/apimachinery/pkg/types"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
)

// resolveConsumerBeskar7Machine returns the namespaced name of the
// Beskar7Machine that legitimately consumes host, or ok=false when host has
// no valid consumer.
//
// A ConsumerRef is valid only when it is set, names Kind "Beskar7Machine"
// with the current InfrastructureAPIVersion, and its Namespace is either
// empty or equal to host's own namespace. The returned key is always
// (host.Namespace, ConsumerRef.Name) — never ConsumerRef.Namespace itself —
// so a ConsumerRef naming a different namespace can never resolve to an
// object there; every caller sees the same "no valid consumer" outcome it
// would see for an absent ConsumerRef.
//
// Claims are always same-namespace: the Beskar7Machine controller only
// claims a PhysicalHost it has just listed from its own namespace, and it
// stamps ConsumerRef.Namespace with that same namespace. A ConsumerRef
// naming a different namespace is therefore never the product of a real
// claim — it can only be a value someone with patch access to PhysicalHosts
// wrote directly (SEC-12). Every caller must treat it exactly like no
// consumer at all: the callback handlers return their usual opaque
// not-found, and the Beskar7Machine controller's host-lookup scans skip the
// host rather than adopting or releasing it.
func resolveConsumerBeskar7Machine(host *infrav1.PhysicalHost) (types.NamespacedName, bool) {
	cr := host.Spec.ConsumerRef
	if cr == nil || cr.Kind != "Beskar7Machine" || cr.APIVersion != InfrastructureAPIVersion {
		return types.NamespacedName{}, false
	}
	if cr.Namespace != "" && cr.Namespace != host.Namespace {
		return types.NamespacedName{}, false
	}
	return types.NamespacedName{Namespace: host.Namespace, Name: cr.Name}, true
}
