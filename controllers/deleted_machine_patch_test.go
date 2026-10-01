/*
Copyright 2026 The Beskar7 Authors.

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
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
)

// A second deletion pass can work from a cached Beskar7Machine that the first
// pass already finished deleting: removing the finalizer there and patching
// fails with NotFound, and reporting that as a reconcile error made every such
// deletion log a false "Reconciler error" (seen on the lab).
var _ = Describe("Patching a Beskar7Machine that is already gone", func() {
	var testNs *corev1.Namespace

	BeforeEach(func() {
		testNs = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "deleted-patch-"}}
		Expect(k8sClient.Create(ctx, testNs)).To(Succeed())
	})
	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, testNs)).To(Succeed())
	})

	// staleDeletingMachine returns a copy of a Beskar7Machine as a pass would
	// read it mid-deletion, with a patch helper over it, after the object has
	// in fact been deleted.
	staleDeletingMachine := func() (*infrav1.Beskar7Machine, *patch.Helper) {
		m := &infrav1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name: "gone", Namespace: testNs.Name,
				Finalizers: []string{Beskar7MachineFinalizer},
			},
			Spec: infrav1.Beskar7MachineSpec{
				InspectionImageURL: "http://boot-server/inspect.ipxe",
				TargetImageURL:     "http://boot-server/kairos.raw",
				TargetImageDigest:  bootTestDigest,
			},
		}
		Expect(k8sClient.Create(ctx, m)).To(Succeed())
		Expect(k8sClient.Delete(ctx, m)).To(Succeed())
		stale := &infrav1.Beskar7Machine{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(m), stale)).To(Succeed())
		Expect(stale.DeletionTimestamp.IsZero()).To(BeFalse())
		helper, err := patch.NewHelper(stale, k8sClient)
		Expect(err).NotTo(HaveOccurred())

		// The first pass finishes the deletion.
		done := stale.DeepCopy()
		done.Finalizers = nil
		Expect(k8sClient.Patch(ctx, done, client.MergeFrom(stale))).To(Succeed())
		Eventually(func() bool {
			return k8sClient.Get(ctx, client.ObjectKeyFromObject(m), &infrav1.Beskar7Machine{}) != nil
		}).Should(BeTrue())
		return stale, helper
	}

	It("is not an error once the machine is being deleted", func() {
		stale, helper := staleDeletingMachine()
		controllerutil.RemoveFinalizer(stale, Beskar7MachineFinalizer)
		err := helper.Patch(ctx, stale)
		Expect(err).To(HaveOccurred(), "the patch must fail for this spec to mean anything")

		Expect(deletedBeforePatch(stale, err)).To(BeTrue())
	})

	It("is still an error for a machine that is not being deleted", func() {
		stale, helper := staleDeletingMachine()
		controllerutil.RemoveFinalizer(stale, Beskar7MachineFinalizer)
		err := helper.Patch(ctx, stale)
		Expect(err).To(HaveOccurred())
		stale.DeletionTimestamp = nil

		Expect(deletedBeforePatch(stale, err)).To(BeFalse())
	})

	It("is still an error when the patch failed for another reason", func() {
		stale, _ := staleDeletingMachine()
		Expect(deletedBeforePatch(stale, errors.New("connection refused"))).To(BeFalse())
	})
})
