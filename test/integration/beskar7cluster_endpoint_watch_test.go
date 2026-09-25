//go:build integration

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

package integration

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
)

// Exercises the Cluster watch added to Beskar7ClusterReconciler.SetupWithManager
// (D-027): once discovery was removed there was no other way for the reconciler
// to learn that Cluster.spec.controlPlaneEndpoint changed, since it now only
// mirrors a value someone else sets. Under the old code this same edit would
// only have been picked up by the 30s "endpoint not ready" poll (also removed);
// this spec proves the watch by requiring the transition well inside that
// window instead of relying on a coincidental poll landing in time.
var _ = Describe("Beskar7Cluster control-plane endpoint watch", func() {
	var (
		specCtx     context.Context
		ns          *corev1.Namespace
		capiCluster *clusterv1.Cluster
		b7cluster   *infrav1.Beskar7Cluster
		key         client.ObjectKey
	)

	BeforeEach(func() {
		specCtx = context.Background()
		ns = createNamespace(specCtx)

		b7ClusterName := "ep-watch-b7cluster"

		capiCluster = &clusterv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "ep-watch-cluster",
				Namespace: ns.Name,
			},
			Spec: clusterv1.ClusterSpec{
				// The watch mapper (util.ClusterToInfrastructureMapFunc) only
				// enqueues a request when this points back at the Beskar7Cluster
				// under test.
				InfrastructureRef: clusterv1.ContractVersionedObjectReference{
					APIGroup: infrav1.GroupVersion.Group,
					Kind:     "Beskar7Cluster",
					Name:     b7ClusterName,
				},
			},
		}
		Expect(k8sClient.Create(specCtx, capiCluster)).To(Succeed())

		b7cluster = &infrav1.Beskar7Cluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      b7ClusterName,
				Namespace: ns.Name,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: clusterv1.GroupVersion.String(),
						Kind:       "Cluster",
						Name:       capiCluster.Name,
						UID:        capiCluster.UID,
					},
				},
			},
		}
		Expect(k8sClient.Create(specCtx, b7cluster)).To(Succeed())
		key = client.ObjectKeyFromObject(b7cluster)
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(context.Background(), ns)).To(Succeed())
	})

	It("provisions promptly once Cluster.spec.controlPlaneEndpoint is patched", func() {
		By("confirming the endpoint starts unset")
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(specCtx, key, b7cluster)).To(Succeed())
			g.Expect(b7cluster.Status.Ready).To(BeFalse())
		}, eventuallyTimeout, eventuallyInterval).Should(Succeed())

		By("patching Cluster.spec.controlPlaneEndpoint directly, the way a user or a bootstrap provider would")
		Expect(k8sClient.Get(specCtx, client.ObjectKeyFromObject(capiCluster), capiCluster)).To(Succeed())
		base := capiCluster.DeepCopy()
		capiCluster.Spec.ControlPlaneEndpoint = clusterv1.APIEndpoint{Host: "203.0.113.50", Port: 6443}
		Expect(k8sClient.Patch(specCtx, capiCluster, client.MergeFrom(base))).To(Succeed())

		// 10s is well under the 30s poll the old code relied on; passing here
		// depends on the watch firing, not on a periodic requeue landing in time.
		By("observing the Beskar7Cluster provision without waiting for a periodic poll")
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(specCtx, key, b7cluster)).To(Succeed())
			g.Expect(b7cluster.Status.Ready).To(BeTrue())
			g.Expect(b7cluster.Status.ControlPlaneEndpoint.Host).To(Equal("203.0.113.50"))
			g.Expect(b7cluster.Status.ControlPlaneEndpoint.Port).To(Equal(int32(6443)))
			g.Expect(b7cluster.Status.Initialization.Provisioned).To(HaveValue(BeTrue()))
		}, "10s", eventuallyInterval).Should(Succeed())
	})
})
