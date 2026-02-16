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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"

	infrastructurev1beta1 "github.com/projectbeskar/beskar7/api/v1beta1"
)

// Utils tests are integrated into the main controller suite
// No separate test function needed - Ginkgo will find the Describe blocks

var _ = Describe("Utils", func() {
	Describe("isPaused", func() {
		It("should return false when no pause annotation is present", func() {
			obj := &infrastructurev1beta1.PhysicalHost{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-host",
					Namespace: "default",
				},
			}
			Expect(isPaused(obj)).To(BeFalse())
		})

		It("should return true when pause annotation is set to false", func() {
			obj := &infrastructurev1beta1.PhysicalHost{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-host",
					Namespace: "default",
					Annotations: map[string]string{
						clusterv1.PausedAnnotation: "false",
					},
				},
			}
			Expect(isPaused(obj)).To(BeTrue())
		})

		It("should return true when pause annotation is set to true", func() {
			obj := &infrastructurev1beta1.PhysicalHost{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-host",
					Namespace: "default",
					Annotations: map[string]string{
						clusterv1.PausedAnnotation: "true",
					},
				},
			}
			Expect(isPaused(obj)).To(BeTrue())
		})

		It("should return true when pause annotation has invalid value", func() {
			obj := &infrastructurev1beta1.PhysicalHost{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-host",
					Namespace: "default",
					Annotations: map[string]string{
						clusterv1.PausedAnnotation: "invalid",
					},
				},
			}
			Expect(isPaused(obj)).To(BeTrue())
		})
	})

	Describe("isClusterPaused", func() {
		It("should return false when cluster is nil", func() {
			Expect(isClusterPaused(nil)).To(BeFalse())
		})

		It("should return false when cluster has no pause annotation", func() {
			cluster := &clusterv1.Cluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cluster",
					Namespace: "default",
				},
			}
			Expect(isClusterPaused(cluster)).To(BeFalse())
		})

		It("should return true when cluster pause annotation is set to false", func() {
			cluster := &clusterv1.Cluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cluster",
					Namespace: "default",
					Annotations: map[string]string{
						clusterv1.PausedAnnotation: "false",
					},
				},
			}
			Expect(isClusterPaused(cluster)).To(BeTrue())
		})

		It("should return true when cluster pause annotation is set to true", func() {
			cluster := &clusterv1.Cluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cluster",
					Namespace: "default",
					Annotations: map[string]string{
						clusterv1.PausedAnnotation: "true",
					},
				},
			}
			Expect(isClusterPaused(cluster)).To(BeTrue())
		})

		It("should return true when cluster pause annotation has invalid value", func() {
			cluster := &clusterv1.Cluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cluster",
					Namespace: "default",
					Annotations: map[string]string{
						clusterv1.PausedAnnotation: "invalid",
					},
				},
			}
			Expect(isClusterPaused(cluster)).To(BeTrue())
		})
	})
})
