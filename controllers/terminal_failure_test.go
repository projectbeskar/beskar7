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
	"context"
	"k8s.io/utils/ptr"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrastructurev1beta1 "github.com/projectbeskar/beskar7/api/v1beta1"
)

// Regression test for a contradiction observed on real hardware: a
// Beskar7Machine finished a provisioning run reporting
//
//	Ready=true, Phase=Provisioned, Initialization.Provisioned=true
//	FailureReason=InspectionTimedOut, FailureMessage="Inspection did not complete within 10m0s"
//
// The inspection timed out (the host was PXE-retrying), markTerminalFailure ran,
// and then the host recovered and reached Ready — at which point a later
// reconcile ran the normal state machine and stamped success over the terminal
// failure without ever consulting it.
//
// That pair is actively harmful rather than merely untidy: CAPI lifts
// FailureReason/FailureMessage onto the owning Machine and treats them as
// unrecoverable, so the object simultaneously tells an operator "provisioned"
// and tells MachineHealthCheck "replace me".
var _ = Describe("Beskar7Machine terminal failure handling", func() {
	var (
		ctx        context.Context
		testNs     *corev1.Namespace
		host       *infrastructurev1beta1.PhysicalHost
		b7machine  *infrastructurev1beta1.Beskar7Machine
		machine    *clusterv1.Machine
		reconciler *Beskar7MachineReconciler
	)

	BeforeEach(func() {
		ctx = context.Background()

		testNs = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "terminal-failure-test-"},
		}
		Expect(k8sClient.Create(ctx, testNs)).To(Succeed())

		// A PhysicalHost that has RECOVERED and is now Ready — the condition that
		// previously let the machine stamp success over its terminal failure.
		host = &infrastructurev1beta1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{Name: "recovered-host", Namespace: testNs.Name},
			Spec: infrastructurev1beta1.PhysicalHostSpec{
				RedfishConnection: infrastructurev1beta1.RedfishConnection{
					Address:              "https://192.168.1.100",
					CredentialsSecretRef: "creds",
				},
			},
		}
		Expect(k8sClient.Create(ctx, host)).To(Succeed())

		b7machine = &infrastructurev1beta1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "failed-machine", Namespace: testNs.Name},
			Spec: infrastructurev1beta1.Beskar7MachineSpec{
				InspectionImageURL: "http://boot-server/inspector",
				TargetImageURL:     "http://boot-server/images/kairos.raw",
				TargetImageDigest:  bootTestDigest,
			},
		}
		Expect(k8sClient.Create(ctx, b7machine)).To(Succeed())

		// Reconcile resolves the owning Cluster via GetClusterFromMetadata, so it
		// must exist for the reconcile to reach the terminal-failure guard.
		cluster := &clusterv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: "test-cluster", Namespace: testNs.Name},
			// The v1beta2 Cluster CRD requires a non-empty spec.
			Spec: clusterv1.ClusterSpec{Paused: ptr.To(false)},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

		bootstrapSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "failed-machine-bootstrap", Namespace: testNs.Name},
			Data:       map[string][]byte{"value": []byte("#cloud-config\n")},
		}
		Expect(k8sClient.Create(ctx, bootstrapSecret)).To(Succeed())

		bootstrapName := bootstrapSecret.Name
		machine = &clusterv1.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "failed-machine",
				Namespace: testNs.Name,
				Labels:    map[string]string{clusterv1.ClusterNameLabel: "test-cluster"},
			},
			Spec: clusterv1.MachineSpec{
				ClusterName:       "test-cluster",
				InfrastructureRef: clusterv1.ContractVersionedObjectReference{APIGroup: "infrastructure.cluster.x-k8s.io", Kind: "Beskar7Machine", Name: "fixture"},
				Bootstrap:         clusterv1.Bootstrap{DataSecretName: &bootstrapName},
			},
		}
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())

		// Reconcile bails early without an owning Machine, so attach the OwnerRef
		// the CAPI Machine controller would normally set — otherwise the test
		// would pass for the wrong reason (never reaching the terminal-failure
		// guard at all).
		b7machine.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       "Machine",
			Name:       machine.Name,
			UID:        machine.UID,
		}}
		b7machine.Labels = map[string]string{clusterv1.ClusterNameLabel: "test-cluster"}
		Expect(k8sClient.Update(ctx, b7machine)).To(Succeed())

		// Claim the host for this machine and mark it Ready: this is exactly the
		// dome situation where the host recovered after the machine had already
		// been marked terminally failed, and handleReadyHost then stamped
		// Ready/ProviderID over the failure.
		host.Spec.ConsumerRef = &corev1.ObjectReference{
			Kind:       "Beskar7Machine",
			Name:       b7machine.Name,
			Namespace:  testNs.Name,
			APIVersion: infrastructurev1beta1.GroupVersion.String(),
		}
		Expect(k8sClient.Update(ctx, host)).To(Succeed())
		host.Status.State = infrastructurev1beta1.StateReady
		host.Status.Ready = true
		Expect(k8sClient.Status().Update(ctx, host)).To(Succeed())

		reconciler = &Beskar7MachineReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			Log:    ctrl.Log.WithName("terminal-failure-test"),
		}
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, testNs)).To(Succeed())
	})

	It("must not stamp success over an existing terminal failure", func() {
		// Put the machine in the exact state markTerminalFailure leaves behind.
		reason := infrastructurev1beta1.InspectionTimedOutReason
		msg := "Inspection did not complete within 10m0s"
		phase := "Failed"
		b7machine.Status.FailureReason = &reason
		b7machine.Status.FailureMessage = &msg
		b7machine.Status.Phase = &phase
		b7machine.Status.Ready = false
		Expect(k8sClient.Status().Update(ctx, b7machine)).To(Succeed())

		// The first reconcile only adds the finalizer and requeues, so a single
		// call never reaches the state machine. Drive it until it settles —
		// otherwise this test would pass for the wrong reason.
		req := reconcile.Request{
			NamespacedName: types.NamespacedName{Name: b7machine.Name, Namespace: testNs.Name},
		}
		for i := 0; i < 3; i++ {
			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
		}

		updated := &infrastructurev1beta1.Beskar7Machine{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: b7machine.Name, Namespace: testNs.Name,
		}, updated)).To(Succeed())

		Expect(updated.Status.Ready).To(BeFalse(),
			"a terminally-failed machine must not become Ready even when its host recovers")
		Expect(updated.Spec.ProviderID).To(BeNil(),
			"a terminally-failed machine must not be assigned a ProviderID")
		if updated.Status.Initialization != nil {
			Expect(updated.Status.Initialization.Provisioned).To(BeFalse(),
				"a terminally-failed machine must not report Initialization.Provisioned")
		}

		// The failure itself is preserved: markTerminalFailure documents that
		// FailureReason is never cleared, so an operator can still see why.
		Expect(updated.Status.FailureReason).NotTo(BeNil())
		Expect(*updated.Status.FailureReason).To(Equal(reason))
		Expect(*updated.Status.Phase).To(Equal("Failed"))
	})

	It("still allows a terminally-failed machine to be deleted", func() {
		// Deletion must stay reachable so the host is released and, under a
		// MachineDeployment, CAPI can replace the failed replica.
		reason := infrastructurev1beta1.InspectionTimedOutReason
		msg := "Inspection did not complete within 10m0s"
		b7machine.Status.FailureReason = &reason
		b7machine.Status.FailureMessage = &msg
		Expect(k8sClient.Status().Update(ctx, b7machine)).To(Succeed())

		Expect(k8sClient.Delete(ctx, b7machine)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: b7machine.Name, Namespace: testNs.Name},
		})
		Expect(err).NotTo(HaveOccurred(),
			"the delete path must remain reachable for a terminally-failed machine")
	})
})
