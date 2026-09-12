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
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
	internalredfish "github.com/projectbeskar/beskar7/internal/redfish"
)

// The inspector's /provisioned report used to be lost when it raced the host's
// state (provision_failed_delivery_test.go covers the failure report):
//
//   - The handler took a report only from a host that was Deploying or Ready.
//     The inspector does not wait for the host (contract §9.2): it deploys as
//     soon as /bootstrap answers and posts /provisioned once the image is on
//     the disk. The host goes to Deploying only when it applies the machine's
//     inspect-complete, which it then did only after reaching its BMC, so a BMC
//     that stopped answering at the wrong moment left the host Inspecting until
//     the deployment was over. The report was dropped, the host went to
//     Deploying once the BMC answered, and its machine waited out
//     --deployment-timeout and failed with DeploymentTimedOut; a
//     MachineHealthCheck then replaced a host that had deployed fine. The host
//     now applies inspect-complete during an outage too
//     (run_during_bmc_outage_test.go); a report can still come in before the
//     machine has validated the inspection report.
//   - The host applied the report only after a successful BMC connection, so a
//     Deploying host kept it through an outage, long enough for the deployment
//     timeout to fail the machine first.

// reportProvisioned posts a finished deployment the way the /provisioned
// handler does once the bearer token has been checked.
func reportProvisioned(key client.ObjectKey) {
	handler := &ProvisionedHandler{Client: k8sClient, Log: ctrl.Log.WithName("provisioned-delivery-handler")}
	Expect(handler.signalProvisioned(ctx, handler.Log, key.Namespace, key.Name)).To(Succeed())
}

// provisionOnMachine hands a host, exactly as published, to the Beskar7Machine
// holding it. A machine that finds its host Ready clears the boot override
// through bmc, and goes on without it when the BMC does not answer.
func provisionOnMachine(host *infrav1.PhysicalHost, bmc internalredfish.RedfishClientFactory) *infrav1.Beskar7Machine {
	machine := &infrav1.Beskar7Machine{ObjectMeta: metav1.ObjectMeta{
		Name: host.Spec.ConsumerRef.Name, Namespace: host.Namespace,
	}}
	r := &Beskar7MachineReconciler{
		Client: k8sClient, Scheme: k8sClient.Scheme(),
		Log:                  ctrl.Log.WithName("provisioned-delivery-machine"),
		RedfishClientFactory: bmc,
	}
	_, err := r.handlePhysicalHostState(ctx, r.Log, machine, host)
	Expect(err).NotTo(HaveOccurred())
	return machine
}

func expectProvisioned(machine *infrav1.Beskar7Machine, host *infrav1.PhysicalHost) {
	Expect(isTerminallyFailed(machine)).To(BeFalse())
	Expect(machine.Status.Ready).To(BeTrue())
	Expect(ptr.Deref(machine.Status.Phase, "")).To(Equal("Provisioned"))
	Expect(ptr.Deref(machine.Status.Initialization.Provisioned, false)).To(BeTrue())
	Expect(machine.Spec.ProviderID).To(Equal(providerID(host.Namespace, host.Name)))
	Expect(conditions.GetReason(machine, infrav1.InfrastructureReadyCondition)).To(Equal(infrav1.ProvisionedReason))
}

var _ = Describe("The inspector's /provisioned report when it races the host's state", func() {
	const retryInterval = 2 * time.Second

	var (
		ns             *corev1.Namespace
		gate           *bmcGate
		hostReconciler *PhysicalHostReconciler
	)

	BeforeEach(func() {
		ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "provisioned-race-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		Expect(k8sClient.Create(ctx, bmcCredentialsSecret(ns.Name))).To(Succeed())
		gate = &bmcGate{reachable: true}
		hostReconciler = &PhysicalHostReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			Log:                    ctrl.Log.WithName("provisioned-race-host"),
			Recorder:               record.NewFakeRecorder(10),
			RedfishClientFactory:   gate.factory(),
			TransientRetryInterval: retryInterval,
		}
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, ns)).To(Succeed())
	})

	// reconcileInOutage reconciles the host once while its BMC refuses
	// connections, and returns it as persisted.
	reconcileInOutage := func(key client.ObjectKey) *infrav1.PhysicalHost {
		result, err := hostReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(retryInterval), "the outage keeps its retry cadence")
		host := getPhysicalHost(key)
		Expect(conditions.GetReason(host, infrav1.RedfishConnectionReadyCondition)).To(Equal(infrav1.BMCUnreachableReason))
		return host
	}

	It("applies a report to a Deploying host without waiting for its BMC", func() {
		key := provisioningHost(ns.Name, "deploying-host", "deploying-machine", infrav1.StateDeploying, nil)
		gate.setReachable(false)
		Expect(reconcileInOutage(key).Status.State).To(Equal(infrav1.StateDeploying))

		By("the inspector posting /provisioned during the outage")
		reportProvisioned(key)
		provisioned := reconcileInOutage(key)
		Expect(provisioned.Status.State).To(Equal(infrav1.StateReady),
			"the report needs nothing from the BMC; waiting for it let the deployment timeout fail the machine first")
		Expect(provisioned.Status.Ready).To(BeTrue())
		Expect(provisioned.Status.ErrorMessage).To(BeEmpty())

		By("handing the host to its machine while the BMC is still down")
		expectProvisioned(provisionOnMachine(provisioned, gate.factory()), provisioned)

		By("reconciling through the rest of the outage, and after it")
		Expect(reconcileInOutage(key).Status.State).To(Equal(infrav1.StateReady))
		gate.setReachable(true)
		recovered := settlePhysicalHost(hostReconciler, key)
		Expect(recovered.Status.State).To(Equal(infrav1.StateReady), "the host comes back where it was")
		Expect(recovered.Annotations).NotTo(HaveKey(ProvisionedRequestAnnotation))
		Expect(conditions.IsTrue(recovered, infrav1.RedfishConnectionReadyCondition)).To(BeTrue())
	})

	// The inspector does not wait for the host, and a callback-only instance
	// keeps answering it while the controllers are away (a restart, a leader
	// election), so the report can come in before the machine has looked at the
	// inspection report, with the host's BMC answering or not.
	DescribeTable("a report that arrives before the machine has validated the inspection report",
		func(readFirst, bmcDown bool) {
			gate.setReachable(!bmcDown)
			key := provisioningHost(ns.Name, "early-host", "early-machine", infrav1.StateInspecting, nil)
			postInspectionReport(key)
			if readFirst {
				settlePhysicalHost(hostReconciler, key)
			}
			reportProvisioned(key)
			Expect(getPhysicalHost(key).Annotations).To(HaveKeyWithValue(ProvisionedRequestAnnotation, "provisioned"),
				"the handler takes a report from a host that is still Inspecting")

			By("reconciling the host: it reads the inspection report and keeps /provisioned")
			inspected := settlePhysicalHost(hostReconciler, key)
			Expect(inspected.Status.State).To(Equal(infrav1.StateInspecting))
			Expect(inspected.Status.InspectionPhase).To(Equal(infrav1.InspectionPhaseComplete))
			Expect(inspected.Annotations).To(HaveKeyWithValue(ProvisionedRequestAnnotation, "provisioned"))

			By("the machine validating the inspection report")
			validateOnMachine(inspected, nil)
			provisioned := settlePhysicalHost(hostReconciler, key)
			Expect(provisioned.Status.State).To(Equal(infrav1.StateReady))
			Expect(provisioned.Status.DeployingTimestamp).NotTo(BeNil())
			Expect(provisioned.Annotations).NotTo(HaveKey(ProvisionedRequestAnnotation))
			Expect(provisioned.Annotations).NotTo(HaveKey(InspectionRequestAnnotation))
			if bmcDown {
				Expect(conditions.GetReason(provisioned, infrav1.RedfishConnectionReadyCondition)).To(Equal(infrav1.BMCUnreachableReason),
					"the whole run went through without the BMC")
			}
			expectProvisioned(provisionOnMachine(provisioned, gate.factory()), provisioned)
		},
		Entry("when it arrives before the host has read the inspection report", false, false),
		Entry("when it arrives after the host has read the inspection report", true, false),
		Entry("when it arrives before the host has read the inspection report, while its BMC is down", false, true),
		Entry("when it arrives after the host has read the inspection report, while its BMC is down", true, true),
	)

	// The inspector posts /provision-failed after any non-202 from
	// /provisioned, having removed the join config from COS_OEM (contract §9.1
	// step 8), and a response lost on the way back is a non-202 to it although
	// the handler took the report.
	DescribeTable("a failure report that follows the success report before the host has applied either",
		func(inspecting bool) {
			state := infrav1.StateDeploying
			if inspecting {
				state = infrav1.StateInspecting
			}
			key := provisioningHost(ns.Name, "both-reports-host", "both-reports-machine", state, nil)
			if inspecting {
				gate.setReachable(false)
				postInspectionReport(key)
				reconcileInOutage(key)
			}

			reportProvisioned(key)
			report := sanitizeFailureReason("provisioned callback failed; removed 99_beskar7.yaml")
			reportDeployFailure(key, report)
			if inspecting {
				held := reconcileInOutage(key)
				Expect(held.Status.State).To(Equal(infrav1.StateInspecting))
				Expect(held.Annotations).To(HaveKey(ProvisionedRequestAnnotation))
				Expect(held.Annotations).To(HaveKey(ProvisionFailedRequestAnnotation))

				By("the machine validating the inspection report, so the host starts deploying")
				validateOnMachine(held, nil)
			}

			failed := settlePhysicalHost(hostReconciler, key)
			Expect(failed.Status.State).To(Equal(infrav1.StateError), "the failure wins: the disk no longer has its join config")
			Expect(failed.Status.ErrorMessage).To(Equal(report))
			Expect(failed.Status.Ready).To(BeFalse())
			Expect(failed.Annotations).NotTo(HaveKey(ProvisionedRequestAnnotation))
			Expect(failed.Annotations).NotTo(HaveKey(ProvisionFailedRequestAnnotation))

			machine, _ := judgeHost(failed)
			Expect(isTerminallyFailed(machine)).To(BeTrue())
			Expect(conditions.GetReason(machine, infrav1.InfrastructureReadyCondition)).To(Equal(infrav1.DeploymentFailedReason))
		},
		Entry("on a Deploying host", false),
		Entry("on a host still Inspecting, which keeps both until it is Deploying, while its BMC is down", true),
	)

	// The machine asks for inspect-complete whenever it reads its host as
	// Inspecting with a report in, and it can read a copy from before its first
	// request was applied. A kept report is applied in the pass right after the
	// host goes to Deploying, so such a request can reach a host that is Ready.
	DescribeTable("an inspection request that reaches a host that has applied the report",
		func(value string) {
			key := provisioningHost(ns.Name, "provisioned-host", "provisioned-machine", infrav1.StateDeploying,
				map[string]string{ProvisionedRequestAnnotation: "provisioned"})
			provisioned := settlePhysicalHost(hostReconciler, key)
			Expect(provisioned.Status.State).To(Equal(infrav1.StateReady))

			requestInspectionStep(key, value)
			after := settlePhysicalHost(hostReconciler, key)
			Expect(after.Annotations).NotTo(HaveKey(InspectionRequestAnnotation), "the request is consumed")
			Expect(after.Status.State).To(Equal(infrav1.StateReady))
			Expect(after.Status.Ready).To(BeTrue())
			Expect(after.Status.ErrorMessage).To(BeEmpty())
			Expect(after.Status.InspectionPhase).To(Equal(provisioned.Status.InspectionPhase))

			expectProvisioned(provisionOnMachine(after, gate.factory()), after)
		},
		Entry("inspect-complete", "inspect-complete"),
		Entry("inspect", "inspect"),
		Entry("timeout", "timeout"),
	)

	It("never applies a kept report when the machine rejects the hardware, and drops it when the host is released", func() {
		key := provisioningHost(ns.Name, "rejected-host", "rejected-machine", infrav1.StateInspecting, nil)
		postInspectionReport(key)
		reportProvisioned(key)
		inspected := settlePhysicalHost(hostReconciler, key)
		Expect(inspected.Annotations).To(HaveKeyWithValue(ProvisionedRequestAnnotation, "provisioned"))

		By("the machine rejecting the hardware")
		machine := validateOnMachine(inspected, &infrav1.HardwareRequirements{MinCPUCores: 16})
		Expect(isTerminallyFailed(machine)).To(BeTrue())
		Expect(conditions.GetReason(machine, infrav1.InfrastructureReadyCondition)).To(Equal(infrav1.HardwareRequirementsNotMetReason))
		Expect(getPhysicalHost(key).Annotations).NotTo(HaveKey(InspectionRequestAnnotation))

		By("reconciling the host: the report stays unapplied")
		still := settlePhysicalHost(hostReconciler, key)
		Expect(still.Status.State).To(Equal(infrav1.StateInspecting))
		Expect(still.Status.DeployingTimestamp).To(BeNil())

		By("releasing the host")
		releasePhysicalHost(key)
		released := settlePhysicalHost(hostReconciler, key)
		Expect(released.Status.State).To(Equal(infrav1.StateAvailable))
		Expect(released.Annotations).NotTo(HaveKey(ProvisionedRequestAnnotation),
			"the report was about the claim that ended")

		By("the host being claimed again")
		claimed := released.DeepCopy()
		claimed.Spec.ConsumerRef = &corev1.ObjectReference{
			Kind: "Beskar7Machine", Name: "next-machine", Namespace: ns.Name,
			APIVersion: infrav1.GroupVersion.String(),
		}
		Expect(k8sClient.Patch(ctx, claimed, client.MergeFrom(released))).To(Succeed())
		reclaimed := settlePhysicalHost(hostReconciler, key)
		Expect(reclaimed.Status.State).To(Equal(infrav1.StateInUse))
		Expect(reclaimed.Annotations).NotTo(HaveKey(ProvisionedRequestAnnotation))
	})

	// A host claimed again and waiting for its new inspector has no deployment
	// for such a report to be about: it comes from an inspector the previous
	// claim left running, with the bearer token the new claim reused.
	It("drops a report that arrives before the host's current run has an inspection report", func() {
		key := provisioningHost(ns.Name, "stale-report-host", "stale-report-machine", infrav1.StateInspecting, nil)
		reportProvisioned(key)
		Expect(getPhysicalHost(key).Annotations).To(HaveKey(ProvisionedRequestAnnotation),
			"the handler takes it: its copy of the host may predate the inspection report")

		dropped := settlePhysicalHost(hostReconciler, key)
		Expect(dropped.Annotations).NotTo(HaveKey(ProvisionedRequestAnnotation))
		Expect(dropped.Status.State).To(Equal(infrav1.StateInspecting))

		By("this run going on to inspect and deploy")
		postInspectionReport(key)
		validateOnMachine(settlePhysicalHost(hostReconciler, key), nil)
		deploying := settlePhysicalHost(hostReconciler, key)
		Expect(deploying.Status.State).To(Equal(infrav1.StateDeploying), "the dropped report does not come back")
	})

	// The deferred patch writes metadata before status, in calls of their own
	// (CAPI's patch.Helper). A pass that dropped the annotation and moved the
	// host to Ready lost the report whenever only the status write failed: the
	// host stayed Deploying for a deployment that was over.
	It("keeps the report until the host's status shows it", func() {
		key := provisioningHost(ns.Name, "status-write-host", "status-write-machine", infrav1.StateDeploying,
			map[string]string{ProvisionedRequestAnnotation: "provisioned"})

		base, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
		Expect(err).NotTo(HaveOccurred())
		var refused atomic.Bool
		refusing := interceptor.NewClient(base, interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, c client.Client, subResource string, obj client.Object,
				patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if data, err := patch.Data(obj); err == nil && strings.Contains(string(data), `"state":"Ready"`) &&
					refused.CompareAndSwap(false, true) {
					return errors.New("injected: the status write carrying Ready did not land")
				}
				return c.SubResource(subResource).Patch(ctx, obj, patch, opts...)
			},
		})
		refusingReconciler := &PhysicalHostReconciler{
			Client: refusing, Scheme: k8sClient.Scheme(),
			Log:                  ctrl.Log.WithName("provisioned-race-refused-status"),
			Recorder:             record.NewFakeRecorder(10),
			RedfishClientFactory: reachableBMC(),
		}

		By("reconciling once, with the status write failing")
		_, err = refusingReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).To(HaveOccurred())
		Expect(refused.Load()).To(BeTrue(), "the pass tried to write Ready")
		lost := getPhysicalHost(key)
		Expect(lost.Status.State).To(Equal(infrav1.StateDeploying))
		Expect(lost.Annotations).To(HaveKeyWithValue(ProvisionedRequestAnnotation, "provisioned"),
			"the report outlives a status write that did not land")

		By("reconciling again")
		provisioned := settlePhysicalHost(hostReconciler, key)
		Expect(provisioned.Status.State).To(Equal(infrav1.StateReady))
		Expect(provisioned.Annotations).NotTo(HaveKey(ProvisionedRequestAnnotation))
	})
})

// The specs above call the reconcilers by hand. This one runs both under a
// manager, starting where a callback-only instance leaves a host while the
// controllers are away: the inspector's inspection report and its /provisioned
// report are both in, and the host's BMC is down when the controllers come
// back. The host's progress reaches the machine only through the PhysicalHost
// watch, and the machine reads every version the host publishes on its way from
// Inspecting to Ready.
var _ = Describe("A deployment reported while the controllers were away, with both under a running manager and the BMC down", func() {
	It("provisions the machine without the BMC, and it stays provisioned", func() {
		testNs := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "provisioned-race-mgr-"}}
		Expect(k8sClient.Create(ctx, testNs)).To(Succeed())
		defer func() { Expect(k8sClient.Delete(ctx, testNs)).To(Succeed()) }()
		ns := testNs.Name
		const (
			clusterName      = "outage-deploy-cluster"
			bootstrapURLBase = "https://example.com:8082"
		)

		Expect(k8sClient.Create(ctx, &clusterv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns},
			Spec:       clusterv1.ClusterSpec{Paused: ptr.To(false)},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "outage-deploy-bootstrap", Namespace: ns},
			Data:       map[string][]byte{"value": []byte("#cloud-config\n")},
		})).To(Succeed())
		machine := &clusterv1.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name: "outage-deploy", Namespace: ns,
				Labels: map[string]string{clusterv1.ClusterNameLabel: clusterName},
			},
			Spec: clusterv1.MachineSpec{
				ClusterName: clusterName,
				InfrastructureRef: clusterv1.ContractVersionedObjectReference{
					APIGroup: infrav1.GroupVersion.Group, Kind: "Beskar7Machine", Name: "outage-deploy",
				},
				Bootstrap: clusterv1.Bootstrap{DataSecretName: ptr.To("outage-deploy-bootstrap")},
			},
		}
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())
		Expect(k8sClient.Create(ctx, bmcCredentialsSecret(ns))).To(Succeed())
		b7m := &infrav1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name: "outage-deploy", Namespace: ns,
				Labels: map[string]string{clusterv1.ClusterNameLabel: clusterName},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: clusterv1.GroupVersion.String(), Kind: "Machine",
					Name: machine.Name, UID: machine.UID,
				}},
			},
			Spec: infrav1.Beskar7MachineSpec{
				InspectionImageURL: "http://boot/inspect.ipxe",
				TargetImageURL:     "http://boot/kairos.raw",
				TargetImageDigest:  bootTestDigest,
			},
		}
		Expect(k8sClient.Create(ctx, b7m)).To(Succeed())
		machineKey := client.ObjectKeyFromObject(b7m)

		hostKey := provisioningHost(ns, "outage-deploy-host", b7m.Name, infrav1.StateInspecting, nil)
		host := getPhysicalHost(hostKey)
		host.Status.Bootstrap = &infrav1.BootstrapStatus{
			URL: bootstrapURLBase + "/api/v1/bootstrap/" + ns + "/" + hostKey.Name,
		}
		Expect(k8sClient.Status().Update(ctx, host)).To(Succeed())

		By("the inspector posting its inspection report and, once deployed, /provisioned, with no controller running")
		postInspectionReport(hostKey)
		reportProvisioned(hostKey)

		gate := &bmcGate{reachable: false}
		skipNameValidation := true
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:                 k8sClient.Scheme(),
			Metrics:                metricsserver.Options{BindAddress: "0"},
			HealthProbeBindAddress: "0",
			// envtest never finishes deleting namespaces, so a cluster-wide cache
			// would hand these controllers every other spec's leftovers.
			Cache:      cache.Options{DefaultNamespaces: map[string]cache.Config{ns: {}}},
			Controller: config.Controller{SkipNameValidation: &skipNameValidation},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect((&PhysicalHostReconciler{
			Client:                 mgr.GetClient(),
			Scheme:                 mgr.GetScheme(),
			Log:                    ctrl.Log.WithName("provisioned-race-mgr-host"),
			Recorder:               record.NewFakeRecorder(100),
			RedfishClientFactory:   gate.factory(),
			TransientRetryInterval: time.Second,
		}).SetupWithManager(mgr)).To(Succeed())
		Expect((&Beskar7MachineReconciler{
			Client:               mgr.GetClient(),
			Scheme:               mgr.GetScheme(),
			Log:                  ctrl.Log.WithName("provisioned-race-mgr-machine"),
			RedfishClientFactory: gate.factory(),
			BootstrapURLBase:     bootstrapURLBase,
		}).SetupWithManager(mgr)).To(Succeed())

		mgrCtx, mgrCancel := context.WithCancel(ctx)
		defer mgrCancel()
		go func() {
			defer GinkgoRecover()
			Expect(mgr.Start(mgrCtx)).To(Succeed())
		}()
		Expect(mgr.GetCache().WaitForCacheSync(mgrCtx)).To(BeTrue())

		By("waiting for the machine to be provisioned while the BMC is still down")
		Eventually(func(g Gomega) {
			got := &infrav1.Beskar7Machine{}
			g.Expect(k8sClient.Get(ctx, machineKey, got)).To(Succeed())
			g.Expect(ptr.Deref(got.Status.Phase, "")).To(Equal("Provisioned"))
			g.Expect(got.Spec.ProviderID).To(Equal(providerID(ns, hostKey.Name)))
			h := &infrav1.PhysicalHost{}
			g.Expect(k8sClient.Get(ctx, hostKey, h)).To(Succeed())
			g.Expect(h.Status.State).To(Equal(infrav1.StateReady))
			g.Expect(conditions.GetReason(h, infrav1.RedfishConnectionReadyCondition)).To(Equal(infrav1.BMCUnreachableReason))
			g.Expect(h.Annotations).NotTo(HaveKey(ProvisionedRequestAnnotation))
			// Including an inspect-complete the machine sent again from a copy of
			// the host that was still Inspecting.
			g.Expect(h.Annotations).NotTo(HaveKey(InspectionRequestAnnotation))
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())

		By("checking neither goes back to deploying")
		Consistently(func(g Gomega) {
			h := &infrav1.PhysicalHost{}
			g.Expect(k8sClient.Get(ctx, hostKey, h)).To(Succeed())
			g.Expect(h.Status.State).To(Equal(infrav1.StateReady))
			got := &infrav1.Beskar7Machine{}
			g.Expect(k8sClient.Get(ctx, machineKey, got)).To(Succeed())
			g.Expect(ptr.Deref(got.Status.Phase, "")).To(Equal("Provisioned"))
			g.Expect(conditions.IsTrue(got, infrav1.InfrastructureReadyCondition)).To(BeTrue())
		}, 3*time.Second, 200*time.Millisecond).Should(Succeed())
	})
})
