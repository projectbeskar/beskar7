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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
)

// Releases up to v0.5.0 used Cluster API's deprecated clusterv1.Conditions,
// where `reason` is optional, and stored True conditions with none. v0.6.0's
// CRDs generate `reason` as required with minLength 1, so the API server
// rejects every status patch that carries those conditions forward and the
// object freezes with whatever the old controller last wrote — while still
// reading healthy under `kubectl get`, because nothing rewrites it.
//
// Reproducing that needs genuinely invalid stored data, which the current CRD
// will not accept. So these specs relax the schema exactly where v0.5.0's was
// looser, write the legacy shape, put the strict schema back, and only then
// reconcile — which is the upgrade, in order.
var _ = Describe("Conditions written by a pre-v0.6.0 controller", func() {
	var testNs *corev1.Namespace

	BeforeEach(func() {
		testNs = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "legacy-cond-"}}
		Expect(k8sClient.Create(ctx, testNs)).To(Succeed())
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, testNs)).To(Succeed())
	})

	// withRelaxedConditionReason drops the `reason` requirement from a CRD for
	// the duration of fn, restoring it afterwards, so a legacy object can be
	// stored the way the older release stored it.
	withRelaxedConditionReason := func(crdName string, fn func()) {
		key := types.NamespacedName{Name: crdName}
		crd := &apiextensionsv1.CustomResourceDefinition{}
		Expect(k8sClient.Get(ctx, key, crd)).To(Succeed())

		props := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"].Properties["conditions"].Items.Schema
		strictRequired := append([]string(nil), props.Required...)
		strictReasonVal := props.Properties["reason"]
		strictReason := *strictReasonVal.DeepCopy()

		var relaxedRequired []string
		for _, r := range strictRequired {
			if r != "reason" && r != "message" {
				relaxedRequired = append(relaxedRequired, r)
			}
		}
		props.Required = relaxedRequired
		loose := props.Properties["reason"]
		loose.MinLength = nil
		loose.Pattern = ""
		props.Properties["reason"] = loose
		Expect(k8sClient.Update(ctx, crd)).To(Succeed())

		defer func() {
			restored := &apiextensionsv1.CustomResourceDefinition{}
			Expect(k8sClient.Get(ctx, key, restored)).To(Succeed())
			p := restored.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"].Properties["conditions"].Items.Schema
			p.Required = strictRequired
			p.Properties["reason"] = strictReason
			Expect(k8sClient.Update(ctx, restored)).To(Succeed())
		}()

		fn()
	}

	// legacyCondition is what v0.5.0 actually left behind: a type and a status,
	// and nothing else.
	legacyCondition := func(t string) metav1.Condition {
		return metav1.Condition{Type: t, Status: metav1.ConditionTrue, LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Hour))}
	}

	It("no longer blocks a PhysicalHost from being patched", func() {
		host := &infrav1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{Name: "legacy-host", Namespace: testNs.Name},
			Spec: infrav1.PhysicalHostSpec{
				RedfishConnection: infrav1.RedfishConnection{
					Address:              "https://192.168.2.250",
					CredentialsSecretRef: "absent-on-purpose",
				},
			},
		}
		Expect(k8sClient.Create(ctx, host)).To(Succeed())

		withRelaxedConditionReason("physicalhosts.infrastructure.cluster.x-k8s.io", func() {
			host.Status.Conditions = []metav1.Condition{
				legacyCondition(infrav1.RedfishConnectionReadyCondition),
				legacyCondition(infrav1.HostAvailableCondition),
			}
			// The API server swaps in a CRD's new schema asynchronously, so the
			// first write after relaxing it can still be validated against the
			// strict one and rejected. A rejected update changes nothing, so
			// retrying it as-is is safe.
			Eventually(func() error {
				return k8sClient.Status().Update(ctx, host)
			}, 10*time.Second, 100*time.Millisecond).Should(Succeed())
		})

		By("confirming the stored object really is the legacy shape")
		stored := &infrav1.PhysicalHost{}
		key := client.ObjectKeyFromObject(host)
		Expect(k8sClient.Get(ctx, key, stored)).To(Succeed())
		Expect(stored.Status.Conditions).NotTo(BeEmpty())
		for _, c := range stored.Status.Conditions {
			Expect(c.Reason).To(BeEmpty(), "the seeded conditions must have no reason, or this proves nothing")
		}

		By("reconciling, which is where the status patch used to be rejected")
		r := &PhysicalHostReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Log: ctrl.Log.WithName("legacy-host-test")}
		// The BMC credentials are absent, so reconcile reports that and returns —
		// but it still patches status, which is the path under test.
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred(), "a legacy object must not wedge the reconciler")

		By("checking every stored condition now satisfies the schema")
		Expect(k8sClient.Get(ctx, key, stored)).To(Succeed())
		for _, c := range stored.Status.Conditions {
			Expect(c.Reason).NotTo(BeEmpty(), "condition %q still has no reason", c.Type)
		}
	})

	It("repairs only what is invalid, leaving good conditions untouched", func() {
		log := ctrl.Log.WithName("legacy-unit")
		keep := metav1.Condition{
			Type:               infrav1.HostAvailableCondition,
			Status:             metav1.ConditionTrue,
			Reason:             infrav1.HostAvailableReason,
			LastTransitionTime: metav1.NewTime(time.Now().Add(-3 * time.Hour)),
		}
		host := &infrav1.PhysicalHost{}
		host.SetConditions([]metav1.Condition{keep, legacyCondition(infrav1.RedfishConnectionReadyCondition)})

		Expect(repairLegacyConditions(host, log)).To(BeTrue(), "it should report that it changed something")

		got := host.GetConditions()
		Expect(got[0].Reason).To(Equal(infrav1.HostAvailableReason), "a valid condition must not be rewritten")
		Expect(got[0].LastTransitionTime).To(Equal(keep.LastTransitionTime), "nor may its transition time move")
		Expect(got[1].Reason).To(Equal(LegacyConditionReason))

		By("and doing nothing at all on a healthy object")
		Expect(repairLegacyConditions(host, log)).To(BeFalse())
	})
})
