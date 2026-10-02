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
	"errors"
	"strings"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
)

// PROV-1: the inspector's two reports, /provisioned and /provision-failed, were
// each lost in a case the other survived.
//
//   - A host that v0.8.0 or earlier put in Error over Deploying for a BMC failure
//     (deployInterruptedByBMCError) took a /provision-failed report but not a
//     /provisioned one: the handler ignored it, and a report that was already
//     waiting was cleared as unexpected. The inspector does not need the BMC and
//     went on to finish the deployment, so once the BMC answered the host went
//     back to InUse, where its machine booted the inspector again on a disk that
//     was already written.
//   - A status write that did not land must not cost the report either. The
//     reconciler removes the annotations a pass consumed only after the deferred
//     patch has succeeded (restoreConsumedAnnotations), so the next pass finds
//     the report again.

// interruptedDeployHost creates a claimed host in the state v0.8.0 and earlier
// left a BMC failure in: Error over a deployment (DeployingTimestamp set), the
// message the BMC's own, and RedfishConnectionReady false. The current
// controller never writes it, so the fixture does.
func interruptedDeployHost(namespace, name, machineName string, annotations map[string]string) client.ObjectKey {
	key := provisioningHost(namespace, name, machineName, infrav1.StateDeploying, annotations)
	host := getPhysicalHost(key)
	host.Status.State = infrav1.StateError
	host.Status.Ready = false
	host.Status.ErrorMessage = "Redfish connection failed: unauthorized"
	setFalse(host, infrav1.RedfishConnectionReadyCondition, infrav1.RedfishConnectionFailedReason, "Connection failed: unauthorized")
	Expect(k8sClient.Status().Update(ctx, host)).To(Succeed())
	Expect(deployInterruptedByBMCError(getPhysicalHost(key))).To(BeTrue(), "the fixture must exercise the branch under test")
	return key
}

// refusingStatusWrite returns a client whose first status patch containing
// fragment fails, and the flag that says whether it has.
func refusingStatusWrite(fragment string) (client.Client, *atomic.Bool) {
	base, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
	Expect(err).NotTo(HaveOccurred())
	var refused atomic.Bool
	return interceptor.NewClient(base, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, subResource string, obj client.Object,
			patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if data, err := patch.Data(obj); err == nil && strings.Contains(string(data), fragment) &&
				refused.CompareAndSwap(false, true) {
				return errors.New("injected: the status write carrying " + fragment + " did not land")
			}
			return c.SubResource(subResource).Patch(ctx, obj, patch, opts...)
		},
	}), &refused
}

var _ = Describe("The inspector's /provisioned report when a BMC Error holds a host that was deploying", func() {
	const retryInterval = 2 * time.Second

	var (
		ns             *corev1.Namespace
		gate           *bmcGate
		hostReconciler *PhysicalHostReconciler
	)

	BeforeEach(func() {
		ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "provisioned-bmc-error-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		Expect(k8sClient.Create(ctx, bmcCredentialsSecret(ns.Name))).To(Succeed())
		gate = &bmcGate{reachable: true}
		hostReconciler = &PhysicalHostReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			Log:                    ctrl.Log.WithName("provisioned-bmc-error-host"),
			Recorder:               record.NewFakeRecorder(10),
			RedfishClientFactory:   gate.factory(),
			TransientRetryInterval: retryInterval,
		}
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, ns)).To(Succeed())
	})

	reconcileInOutage := func(key client.ObjectKey) *infrav1.PhysicalHost {
		result, err := hostReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(retryInterval), "the outage keeps its retry cadence")
		host := getPhysicalHost(key)
		Expect(conditions.GetReason(host, infrav1.RedfishConnectionReadyCondition)).To(Equal(infrav1.BMCUnreachableReason))
		return host
	}

	// expectProvisionedNotReinspected checks the host went to Ready on the
	// strength of the report, and not back to InUse, where its machine boots
	// the inspector again.
	expectProvisionedNotReinspected := func(host *infrav1.PhysicalHost, before *infrav1.PhysicalHost) {
		Expect(host.Status.State).To(Equal(infrav1.StateReady))
		Expect(host.Status.Ready).To(BeTrue())
		Expect(host.Status.ErrorMessage).To(BeEmpty(), "the BMC's message does not outlive the report")
		Expect(host.Status.InspectionPhase).To(Equal(before.Status.InspectionPhase))
		Expect(host.Status.DeployingTimestamp).To(Equal(before.Status.DeployingTimestamp))
		Expect(host.Annotations).NotTo(HaveKey(ProvisionedRequestAnnotation))
		Expect(host.Annotations).NotTo(HaveKey(InspectionRequestAnnotation))
	}

	DescribeTable("a report the inspector posts while the BMC Error holds the host",
		func(bmcDown bool) {
			key := interruptedDeployHost(ns.Name, "blip-host", "blip-machine", nil)
			before := getPhysicalHost(key)
			gate.setReachable(!bmcDown)

			By("the inspector posting /provisioned")
			reportProvisioned(key)
			Expect(getPhysicalHost(key).Annotations).To(HaveKeyWithValue(ProvisionedRequestAnnotation, "provisioned"),
				"the handler takes the report: the BMC failure says nothing about the deployment")

			By("reconciling the host")
			var provisioned *infrav1.PhysicalHost
			if bmcDown {
				// The first pass applies the report and keeps the annotation until
				// status shows it; the second removes it.
				reconcileInOutage(key)
				provisioned = reconcileInOutage(key)
			} else {
				provisioned = settlePhysicalHost(hostReconciler, key)
			}
			expectProvisionedNotReinspected(provisioned, before)

			By("handing the host to its machine")
			expectProvisioned(provisionOnMachine(provisioned, gate.factory()), provisioned)

			By("the BMC answering")
			gate.setReachable(true)
			recovered := settlePhysicalHost(hostReconciler, key)
			expectProvisionedNotReinspected(recovered, before)
			Expect(conditions.IsTrue(recovered, infrav1.RedfishConnectionReadyCondition)).To(BeTrue())
		},
		Entry("while the BMC still fails", true),
		Entry("after the BMC has answered again", false),
	)

	// The handler is not the only way the annotation reaches the host: a report
	// the handler took while the host was still Deploying can be waiting when the
	// Error lands.
	It("applies a report that is already waiting on the host", func() {
		key := interruptedDeployHost(ns.Name, "waiting-host", "waiting-machine",
			map[string]string{ProvisionedRequestAnnotation: "provisioned"})
		before := getPhysicalHost(key)

		provisioned := settlePhysicalHost(hostReconciler, key)
		expectProvisionedNotReinspected(provisioned, before)
		expectProvisioned(provisionOnMachine(provisioned, gate.factory()), provisioned)
	})

	It("takes a repeat of a report the host has already applied", func() {
		key := interruptedDeployHost(ns.Name, "repeat-host", "repeat-machine", nil)
		before := getPhysicalHost(key)
		reportProvisioned(key)
		provisioned := settlePhysicalHost(hostReconciler, key)
		expectProvisionedNotReinspected(provisioned, before)

		By("the inspector retrying the call")
		reportProvisioned(key)
		Expect(getPhysicalHost(key).Annotations).To(HaveKey(ProvisionedRequestAnnotation))
		repeated := settlePhysicalHost(hostReconciler, key)
		expectProvisionedNotReinspected(repeated, before)
	})

	It("lets a failure report waiting with it win", func() {
		report := sanitizeFailureReason("COS_OEM inject failed")
		key := interruptedDeployHost(ns.Name, "both-host", "both-machine", map[string]string{
			ProvisionedRequestAnnotation:     "provisioned",
			ProvisionFailedRequestAnnotation: report,
		})

		failed := settlePhysicalHost(hostReconciler, key)
		Expect(failed.Status.State).To(Equal(infrav1.StateError))
		Expect(failed.Status.ErrorMessage).To(Equal(report))
		Expect(failed.Status.Ready).To(BeFalse())
		Expect(failed.Annotations).NotTo(HaveKey(ProvisionedRequestAnnotation))
		Expect(failed.Annotations).NotTo(HaveKey(ProvisionFailedRequestAnnotation))

		machine, _ := judgeHost(failed)
		Expect(isTerminallyFailed(machine)).To(BeTrue())
		Expect(conditions.GetReason(machine, infrav1.InfrastructureReadyCondition)).To(Equal(infrav1.DeploymentFailedReason))
	})

	It("clears a report that is waiting on a host that was released, without a transition", func() {
		key := interruptedDeployHost(ns.Name, "released-host", "released-machine",
			map[string]string{ProvisionedRequestAnnotation: "provisioned"})
		releasePhysicalHost(key)

		released := settlePhysicalHost(hostReconciler, key)
		Expect(released.Annotations).NotTo(HaveKey(ProvisionedRequestAnnotation),
			"the report was about the claim that ended")
		Expect(released.Status.State).To(Equal(infrav1.StateAvailable), "not Ready: nobody holds the host")
		Expect(released.Spec.ConsumerRef).To(BeNil())
	})
})

var _ = Describe("The inspector's /provision-failed report when the status write that applies it fails", func() {
	var ns *corev1.Namespace

	BeforeEach(func() {
		ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "report-status-write-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		Expect(k8sClient.Create(ctx, bmcCredentialsSecret(ns.Name))).To(Succeed())
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, ns)).To(Succeed())
	})

	DescribeTable("keeps the report until the host's status shows it",
		func(bmcDown bool) {
			gate := &bmcGate{reachable: !bmcDown}
			key := provisioningHost(ns.Name, "status-write-host", "status-write-machine", infrav1.StateDeploying, nil)
			report := sanitizeFailureReason("image fetch failed: 404 Not Found")
			reportDeployFailure(key, report)

			refusing, refused := refusingStatusWrite(`"state":"Error"`)
			refusingReconciler := &PhysicalHostReconciler{
				Client: refusing, Scheme: k8sClient.Scheme(),
				Log:                    ctrl.Log.WithName("report-status-write-refused"),
				Recorder:               record.NewFakeRecorder(10),
				RedfishClientFactory:   gate.factory(),
				TransientRetryInterval: 2 * time.Second,
			}

			By("reconciling once, with the status write failing")
			_, err := refusingReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).To(HaveOccurred())
			Expect(refused.Load()).To(BeTrue(), "the pass tried to write Error")
			lost := getPhysicalHost(key)
			Expect(lost.Status.State).To(Equal(infrav1.StateDeploying))
			Expect(lost.Annotations).To(HaveKeyWithValue(ProvisionFailedRequestAnnotation, report),
				"the report outlives a status write that did not land; losing it left the machine to the deployment timeout")

			By("reconciling again")
			next, err := refusingReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			if bmcDown {
				Expect(next.RequeueAfter).To(Equal(2 * time.Second))
			}
			failed := getPhysicalHost(key)
			Expect(failed.Status.State).To(Equal(infrav1.StateError))
			Expect(failed.Status.ErrorMessage).To(Equal(report))
			Expect(failed.Status.Ready).To(BeFalse())
			Expect(failed.Annotations).NotTo(HaveKey(ProvisionFailedRequestAnnotation))

			machine, _ := judgeHost(failed)
			Expect(isTerminallyFailed(machine)).To(BeTrue())
			Expect(conditions.GetReason(machine, infrav1.InfrastructureReadyCondition)).To(Equal(infrav1.DeploymentFailedReason))
		},
		Entry("when the BMC answers", false),
		Entry("when the BMC is down", true),
	)

	It("keeps the report when the write that applies it to a BMC Error over a deployment fails", func() {
		key := interruptedDeployHost(ns.Name, "status-write-bmc-host", "status-write-bmc-machine", nil)
		report := sanitizeFailureReason("COS_OEM partition not found")
		reportDeployFailure(key, report)

		refusing, refused := refusingStatusWrite(report)
		refusingReconciler := &PhysicalHostReconciler{
			Client: refusing, Scheme: k8sClient.Scheme(),
			Log:                  ctrl.Log.WithName("report-status-write-refused-bmc"),
			Recorder:             record.NewFakeRecorder(10),
			RedfishClientFactory: reachableBMC(),
		}

		_, err := refusingReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).To(HaveOccurred())
		Expect(refused.Load()).To(BeTrue(), "the pass tried to write the report")
		Expect(getPhysicalHost(key).Annotations).To(HaveKeyWithValue(ProvisionFailedRequestAnnotation, report))

		failed := settlePhysicalHost(refusingReconciler, key)
		Expect(failed.Status.State).To(Equal(infrav1.StateError))
		Expect(failed.Status.ErrorMessage).To(Equal(report))
		Expect(failed.Annotations).NotTo(HaveKey(ProvisionFailedRequestAnnotation))
	})
})
