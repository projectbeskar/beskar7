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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/cluster-api/util/conditions"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
)

// hostWaitingForNetworkData reports whether a claimed host must not be booted
// yet because its network data cannot be served: it names a network data
// Secret, and NetworkDataReady is not True for the host's current spec.
//
// Booting anyway would only reach the /networkdata endpoint's 409, fail the
// machine, and have its replacement wipe the same host again. Waiting costs
// nothing, and fixing the Secret wakes the host, and through it this machine.
// A condition from an older generation does not count, so a ref changed since
// the host last checked it waits for that check. The 409 still covers a
// Secret that changes after the boot.
func hostWaitingForNetworkData(physicalHost *infrav1.PhysicalHost) bool {
	if physicalHost.Spec.NetworkDataSecretRef == "" {
		return false
	}
	cond := conditions.Get(physicalHost, infrav1.NetworkDataReadyCondition)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		return true
	}
	return cond.ObservedGeneration != physicalHost.Generation
}

// networkDataWaitMessage says why the machine waits, from the host's condition.
func networkDataWaitMessage(physicalHost *infrav1.PhysicalHost) string {
	cond := conditions.Get(physicalHost, infrav1.NetworkDataReadyCondition)
	if cond == nil || cond.Status == metav1.ConditionTrue {
		return "network data Secret " + physicalHost.Spec.NetworkDataSecretRef + " not checked yet"
	}
	return cond.Reason + ": " + cond.Message
}
