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
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/annotations"
)

// isPaused checks if a resource has the pause annotation present.
// It returns true if the pause annotation exists (regardless of value).
func isPaused(obj metav1.Object) bool {
	return annotations.HasPaused(obj)
}

// isClusterPaused checks if the owner cluster has the pause annotation present.
// It returns true if the pause annotation exists (regardless of value).
func isClusterPaused(cluster *clusterv1.Cluster) bool {
	if cluster == nil {
		return false
	}
	return annotations.HasPaused(cluster)
}

// DefaultMaxConcurrentReconciles is the per-controller worker count used when
// none is configured. It matches controller-runtime's own default, so the
// zero value preserves historical single-worker behaviour exactly.
//
// Raising it is safe with respect to BMC load: controller-runtime never
// reconciles the same object key concurrently, so distinct workers always act
// on distinct PhysicalHosts — and therefore distinct BMCs. The reason to raise
// it is head-of-line blocking: a single unreachable BMC can occupy the only
// worker for a full 30s Redfish timeout, stalling reconciles for healthy hosts.
const DefaultMaxConcurrentReconciles = 1

// maxConcurrentOrDefault normalises an unset (zero) or negative worker count to
// DefaultMaxConcurrentReconciles.
func maxConcurrentOrDefault(n int) int {
	if n < 1 {
		return DefaultMaxConcurrentReconciles
	}
	return n
}
