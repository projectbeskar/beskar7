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
	"context"
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/stmcginnis/gofish/schemas"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
	internalredfish "github.com/projectbeskar/beskar7/internal/redfish"
)

// RELEASE-POWER. A host released soon after its OS booted can lose the graceful
// shutdown sent at release (the guest is not handling ACPI yet), and nothing
// sent it again: the released host stayed on, with its kubelet running. The
// release now holds the Beskar7Machine's finalizer, and with it the host's
// claim, until the host reads Off, forcing it off once the grace has passed.
//
// These specs drive the real Reconcile against envtest. The time the shutdown
// was requested is what the controller persists, so a spec moves the clock by
// rewriting that annotation, which is also what a manager restart would find.
var _ = Describe("Releasing a host that ignores the graceful shutdown (RELEASE-POWER)", func() {
	const (
		machineName = "release-power-machine"
		hostName    = "release-power-host"
		clusterName = "release-power-cluster"
		bmcAddress  = "https://mock-redfish.example.invalid:8443"
	)

	var (
		ns         *corev1.Namespace
		mockRf     *internalredfish.MockClient
		factory    internalredfish.RedfishClientFactory
		machineKey client.ObjectKey
		hostKey    client.ObjectKey
	)

	newReconciler := func() *Beskar7MachineReconciler {
		return &Beskar7MachineReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			Log:                  ctrl.Log.WithName("release-power"),
			RedfishClientFactory: factory,
		}
	}

	// newHost creates the claimed host in the given state, with the guest
	// running and ignoring the graceful shutdown unless a spec says otherwise.
	newHost := func(state string) {
		host := claimedPhysicalHost(ns.Name, hostName, machineName)
		Expect(k8sClient.Create(ctx, host)).To(Succeed())
		host.Status.State = state
		host.Status.Ready = state == infrav1.StateReady
		Expect(k8sClient.Status().Update(ctx, host)).To(Succeed())
	}

	reconcileMachine := func(r *Beskar7MachineReconciler) (ctrl.Result, error) {
		return r.Reconcile(ctx, ctrl.Request{NamespacedName: machineKey})
	}

	// deleteMachine marks the Beskar7Machine deleted; its finalizer keeps it.
	// The first pass over a machine only records its Paused condition and
	// returns, so it is run here, leaving the BMC untouched for the spec.
	deleteMachine := func() {
		m := &infrav1.Beskar7Machine{}
		Expect(k8sClient.Get(ctx, machineKey, m)).To(Succeed())
		Expect(k8sClient.Delete(ctx, m)).To(Succeed())
		_, err := reconcileMachine(newReconciler())
		Expect(err).NotTo(HaveOccurred())
		Expect(mockRf.ClearBootSourceOverrideCalled).To(BeFalse())
	}

	machineGone := func() bool {
		return apierrors.IsNotFound(k8sClient.Get(ctx, machineKey, &infrav1.Beskar7Machine{}))
	}

	getMachine := func() *infrav1.Beskar7Machine {
		m := &infrav1.Beskar7Machine{}
		Expect(k8sClient.Get(ctx, machineKey, m)).To(Succeed())
		return m
	}

	// requestedAt is the shutdown-requested time the controller persisted.
	requestedAt := func() time.Time {
		at, ok := releasePowerOffRequestedAt(getMachine())
		Expect(ok).To(BeTrue(), "the release must have recorded when it requested the shutdown")
		return at
	}

	// ago moves the recorded request into the past by d.
	ago := func(d time.Duration) {
		m := getMachine()
		base := m.DeepCopy()
		setReleasePowerOffRequestedAt(m, time.Now().Add(-d))
		Expect(k8sClient.Patch(ctx, m, client.MergeFrom(base))).To(Succeed())
	}

	expectStillClaimed := func() {
		Expect(machineGone()).To(BeFalse(), "the finalizer must hold the machine while the host may still be on")
		host := getPhysicalHost(hostKey)
		Expect(host.Spec.ConsumerRef).NotTo(BeNil(), "a claimed host cannot be claimed, and powered on, by another machine")
		Expect(host.Status.State).NotTo(Equal(infrav1.StateAvailable))
	}

	expectReleased := func() {
		Eventually(machineGone).Should(BeTrue(), "the finalizer must be removed")
		Expect(getPhysicalHost(hostKey).Spec.ConsumerRef).To(BeNil())
	}

	BeforeEach(func() {
		ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "release-power-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		Expect(k8sClient.Create(ctx, bmcCredentialsSecret(ns.Name))).To(Succeed())

		cluster := &clusterv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns.Name},
			Spec:       clusterv1.ClusterSpec{Paused: ptr.To(false)},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		capiMachine := &clusterv1.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name: machineName, Namespace: ns.Name,
				Labels: map[string]string{clusterv1.ClusterNameLabel: cluster.Name},
			},
			Spec: clusterv1.MachineSpec{
				ClusterName: cluster.Name,
				InfrastructureRef: clusterv1.ContractVersionedObjectReference{
					APIGroup: infrav1.GroupVersion.Group, Kind: "Beskar7Machine", Name: machineName,
				},
				Bootstrap: clusterv1.Bootstrap{DataSecretName: ptr.To("release-power-bootstrap")},
			},
		}
		Expect(k8sClient.Create(ctx, capiMachine)).To(Succeed())
		b7m := &infrav1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name: machineName, Namespace: ns.Name,
				Finalizers: []string{Beskar7MachineFinalizer},
				Labels:     map[string]string{clusterv1.ClusterNameLabel: cluster.Name},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: clusterv1.GroupVersion.String(), Kind: "Machine",
					Name: capiMachine.Name, UID: capiMachine.UID,
				}},
			},
			Spec: func() infrav1.Beskar7MachineSpec {
				s := minimalImageSpec()
				s.ProviderID = providerID(ns.Name, hostName)
				return s
			}(),
		}
		Expect(k8sClient.Create(ctx, b7m)).To(Succeed())

		machineKey = client.ObjectKeyFromObject(b7m)
		hostKey = client.ObjectKey{Namespace: ns.Name, Name: hostName}

		mockRf = internalredfish.NewMockClient()
		mockRf.PowerState = schemas.OnPowerState
		mockRf.IgnoreGracefulShutdown = true
		factory = func(context.Context, string, string, string, bool, []byte) (internalredfish.Client, error) {
			return mockRf, nil
		}

		DeferCleanup(func() {
			m := &infrav1.Beskar7Machine{}
			if err := k8sClient.Get(ctx, machineKey, m); err == nil {
				base := m.DeepCopy()
				m.Finalizers = nil
				Expect(k8sClient.Patch(ctx, m, client.MergeFrom(base))).To(Succeed())
			}
			Expect(k8sClient.Delete(ctx, ns)).To(Succeed())
		})
	})

	Context("a provisioned host", func() {
		BeforeEach(func() { newHost(infrav1.StateReady) })

		It("is released at once when the graceful shutdown takes effect", func() {
			mockRf.IgnoreGracefulShutdown = false
			deleteMachine()

			result, err := reconcileMachine(newReconciler())

			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsZero()).To(BeTrue())
			Expect(mockRf.SetPowerStateCalled).To(BeTrue())
			Expect(mockRf.ForcePowerOffCalled).To(BeFalse())
			expectReleased()
		})

		It("keeps the claim while the host is shutting down, and releases it without forcing once it is off", func() {
			deleteMachine()

			result, err := reconcileMachine(newReconciler())
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(releasePowerPollInterval))
			Expect(requestedAt()).To(BeTemporally("~", time.Now(), 10*time.Second))
			Expect(mockRf.SetPowerStateCalled).To(BeTrue(), "the graceful shutdown is still what is asked first")
			Expect(mockRf.ClearBootSourceOverrideCalled).To(BeTrue())
			expectStillClaimed()

			By("the guest finishing its shutdown, seen by a restarted manager")
			mockRf.PowerState = schemas.OffPowerState
			result, err = reconcileMachine(newReconciler())

			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsZero()).To(BeTrue())
			Expect(mockRf.ForcePowerOffCalled).To(BeFalse(), "a host that is off needs no force")
			expectReleased()
		})

		It("forces the host off, once, when it still has not shut down after the grace, and then releases it", func() {
			deleteMachine()
			r := newReconciler()
			_, err := reconcileMachine(r)
			Expect(err).NotTo(HaveOccurred())

			By("a pass inside the grace, which only looks")
			ago(releaseShutdownGrace / 2)
			result, err := reconcileMachine(r)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(releasePowerPollInterval))
			Expect(mockRf.ForcePowerOffCalled).To(BeFalse())
			expectStillClaimed()

			By("the grace passing, seen by a restarted manager")
			ago(releaseShutdownGrace + time.Second)
			result, err = reconcileMachine(newReconciler())

			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsZero()).To(BeTrue())
			Expect(mockRf.ForcePowerOffCount).To(Equal(1))
			Expect(mockRf.PowerState).To(Equal(schemas.OffPowerState))
			expectReleased()
		})

		It("does not put the grace period out by a manager restart or a repeated pass", func() {
			deleteMachine()
			_, err := reconcileMachine(newReconciler())
			Expect(err).NotTo(HaveOccurred())
			first := requestedAt()

			for range 3 {
				_, err = reconcileMachine(newReconciler())
				Expect(err).NotTo(HaveOccurred())
			}

			Expect(requestedAt()).To(Equal(first), "the request is made, and dated, once")
		})

		It("keeps retrying a ForcePowerOff the BMC refuses, then releases once it is accepted", func() {
			deleteMachine()
			_, err := reconcileMachine(newReconciler())
			Expect(err).NotTo(HaveOccurred())
			ago(releaseShutdownGrace + time.Second)
			mockRf.ShouldFail["ForcePowerOff"] = errors.New("BMC busy")

			result, err := reconcileMachine(newReconciler())

			Expect(err).NotTo(HaveOccurred(), "a BMC error is retried, never returned for the workqueue to back off")
			Expect(result.RequeueAfter).To(Equal(releasePowerPollInterval))
			Expect(mockRf.ForcePowerOffCount).To(Equal(1))
			expectStillClaimed()

			delete(mockRf.ShouldFail, "ForcePowerOff")
			result, err = reconcileMachine(newReconciler())

			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsZero()).To(BeTrue())
			Expect(mockRf.ForcePowerOffCount).To(Equal(2))
			expectReleased()
		})

		It("retries an unreachable BMC without failing the machine, and releases the host anyway once the bound passes", func() {
			deleteMachine()
			_, err := reconcileMachine(newReconciler())
			Expect(err).NotTo(HaveOccurred())

			By("the BMC dropping the connection while the host is being waited on")
			mockRf.ShouldFail["GetPowerState"] = refusedConnection(bmcAddress)
			result, err := reconcileMachine(newReconciler())
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(releasePowerPollInterval))
			expectStillClaimed()
			Expect(isTerminallyFailed(getMachine())).To(BeFalse(), "a BMC outage never fails a machine")

			By("the BMC refusing to build a client at all")
			factory = failingBMC(refusedConnection(bmcAddress))
			result, err = reconcileMachine(newReconciler())
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(releasePowerPollInterval))
			expectStillClaimed()

			By("the bound running out with the BMC still down")
			ago(releaseShutdownGrace + releaseForceOffWindow + time.Second)
			result, err = reconcileMachine(newReconciler())
			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsZero()).To(BeTrue())
			Expect(mockRf.ForcePowerOffCalled).To(BeFalse(), "nothing could be sent to a BMC that never answered")
			expectReleased()
		})

		It("stops waiting at once when the credentials no longer authorise the BMC, which waiting cannot fix", func() {
			deleteMachine()
			_, err := reconcileMachine(newReconciler())
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "bmc-credentials", Namespace: ns.Name}})).To(Succeed())

			result, err := reconcileMachine(newReconciler())

			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsZero()).To(BeTrue())
			Expect(mockRf.ForcePowerOffCalled).To(BeFalse())
			expectReleased()
		})

		It("does not wait on a request that was never accepted", func() {
			mockRf.ShouldFail["SetPowerState"] = errors.New("BMC busy")
			deleteMachine()

			result, err := reconcileMachine(newReconciler())

			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsZero()).To(BeTrue(), "a dead BMC must not hold a deletion up, as before")
			expectReleased()
		})

		It("honours the force-release annotation set while the release is waiting", func() {
			deleteMachine()
			_, err := reconcileMachine(newReconciler())
			Expect(err).NotTo(HaveOccurred())
			expectStillClaimed()
			ago(releaseShutdownGrace + time.Second)
			m := getMachine()
			base := m.DeepCopy()
			m.Annotations[ForceReleaseAnnotation] = "true"
			Expect(k8sClient.Patch(ctx, m, client.MergeFrom(base))).To(Succeed())

			result, err := reconcileMachine(newReconciler())

			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsZero()).To(BeTrue())
			Expect(mockRf.ForcePowerOffCalled).To(BeFalse(), "force-release skips the BMC")
			expectReleased()
		})

		It("waits no longer than the bound when the recorded time is in the future", func() {
			deleteMachine()
			_, err := reconcileMachine(newReconciler())
			Expect(err).NotTo(HaveOccurred())
			ago(-time.Hour)

			result, err := reconcileMachine(newReconciler())

			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(releasePowerPollInterval))
			Expect(requestedAt()).To(BeTemporally("~", time.Now(), 10*time.Second),
				"a time from the future is taken as now, so the bound still holds")
		})

		It("does nothing while the cluster is paused, and carries on when it resumes", func() {
			deleteMachine()
			_, err := reconcileMachine(newReconciler())
			Expect(err).NotTo(HaveOccurred())
			ago(releaseShutdownGrace + time.Second)

			cluster := &clusterv1.Cluster{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: clusterName}, cluster)).To(Succeed())
			cluster.Spec.Paused = ptr.To(true)
			Expect(k8sClient.Update(ctx, cluster)).To(Succeed())
			result, err := reconcileMachine(newReconciler())
			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsZero()).To(BeTrue())
			Expect(mockRf.ForcePowerOffCalled).To(BeFalse(), "a paused machine's host is left alone")
			expectStillClaimed()

			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cluster), cluster)).To(Succeed())
			cluster.Spec.Paused = ptr.To(false)
			Expect(k8sClient.Update(ctx, cluster)).To(Succeed())
			// The pass that notices the resume records the Paused condition and returns.
			for range 2 {
				_, err = reconcileMachine(newReconciler())
				Expect(err).NotTo(HaveOccurred())
			}

			Expect(mockRf.ForcePowerOffCount).To(Equal(1))
			expectReleased()
		})
	})

	// A host released mid-run is running the inspector, which ignores ACPI; it
	// was forced off and released in the same pass before this change, and is
	// still.
	DescribeTable("a host released mid-run is forced off and released at once",
		func(state string) {
			newHost(state)
			deleteMachine()

			result, err := reconcileMachine(newReconciler())

			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsZero()).To(BeTrue())
			Expect(mockRf.ForcePowerOffCount).To(Equal(1))
			Expect(mockRf.SetPowerStateCalled).To(BeFalse())
			Expect(mockRf.ClearBootSourceOverrideCalled).To(BeTrue())
			expectReleased()
		},
		Entry("Inspecting", infrav1.StateInspecting),
		Entry("Deploying", infrav1.StateDeploying),
		Entry("Error", infrav1.StateError),
	)
})
