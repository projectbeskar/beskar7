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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/beskar7/beskar7/api/v1beta2"
)

var _ = Describe("PhysicalHostPool admission rules", func() {
	It("keeps poolRef from being added, changed or removed", func() {
		withPool := testPoolMachine("crd-pool-machine", "default", poolTestPool)
		Expect(k8sClient.Create(ctx, withPool)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, withPool))).To(Succeed()) })

		withPool.Spec.PoolRef = poolTestGPUPool
		Expect(k8sClient.Update(ctx, withPool)).To(MatchError(ContainSubstring("poolRef is immutable")))

		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(withPool), withPool)).To(Succeed())
		withPool.Spec.PoolRef = ""
		Expect(k8sClient.Update(ctx, withPool)).To(MatchError(ContainSubstring("poolRef cannot be added or removed")))

		without := testPoolMachine("crd-nopool-machine", "default", "")
		Expect(k8sClient.Create(ctx, without)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, without))).To(Succeed()) })
		without.Spec.PoolRef = poolTestPool
		Expect(k8sClient.Update(ctx, without)).To(MatchError(ContainSubstring("poolRef cannot be added or removed")))
	})

	It("shows 0 for a status count that a merge patch never wrote", func() {
		pool := testPool("crd-zero-counts", "")
		Expect(k8sClient.Create(ctx, pool)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, pool))).To(Succeed()) })

		// Like updateStatus: only members changes, so the patch carries no available or claimed.
		base := pool.DeepCopy()
		pool.Status.Members = 2
		Expect(k8sClient.Status().Patch(ctx, pool, client.MergeFrom(base))).To(Succeed())

		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(infrav1.GroupVersion.WithKind("PhysicalHostPool"))
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pool), got)).To(Succeed())
		for _, field := range []string{"members", "available", "claimed"} {
			_, found, err := unstructured.NestedInt64(got.Object, "status", field)
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue(), "status.%s is missing", field)
		}
	})

	It("rejects a pool without namespaceSelector or with a name too long for a label key", func() {
		noSelector := testPool("crd-no-selector", "")
		noSelector.Spec.NamespaceSelector = nil
		Expect(k8sClient.Create(ctx, noSelector)).NotTo(Succeed())

		long := testPool(strings.Repeat("a", 64), "")
		Expect(k8sClient.Create(ctx, long)).To(MatchError(ContainSubstring("at most 63 characters")))

		ok := &infrav1.PhysicalHostPool{
			ObjectMeta: metav1.ObjectMeta{Name: "crd-ok"},
			Spec: infrav1.PhysicalHostPoolSpec{
				HostNamespaces:    []string{poolTestHostNamespace},
				NamespaceSelector: &metav1.LabelSelector{},
			},
		}
		Expect(k8sClient.Create(ctx, ok)).To(Succeed())
		Expect(k8sClient.Delete(ctx, ok)).To(Succeed())
	})
})
