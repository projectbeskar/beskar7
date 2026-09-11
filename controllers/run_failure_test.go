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
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/stmcginnis/gofish/common"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
	internalredfish "github.com/projectbeskar/beskar7/internal/redfish"
)

// The inspector's /provision-failed report and the Beskar7Machine's inspection
// timeout both end a provisioning run in Error. Neither used to outlive the
// reconcile that applied it: the claimed-host branch of reconcileNormal put any
// claimed host that was not InUse, Inspecting, Deploying or Ready back at InUse
// in the same pass, so only InUse was persisted. The machine never saw the
// inspector's reason, and on InUse it booted the inspector again. These specs
// drive the real PhysicalHostReconciler.Reconcile and pin that such an Error
// now stays on the host until it is released, whatever its BMC does meanwhile.

// provisioningHost creates a claimed host part-way through a run with a healthy
// BMC connection, as the annotation handlers would have left it, carrying
// annotations for its next reconcile.
func provisioningHost(namespace, name, machineName, state string, annotations map[string]string) client.ObjectKey {
	host := claimedPhysicalHost(namespace, name, machineName)
	host.Finalizers = []string{PhysicalHostFinalizer}
	host.Annotations = annotations
	Expect(k8sClient.Create(ctx, host)).To(Succeed())

	started := metav1.NewTime(time.Now().Add(-time.Minute))
	host.Status.State = state
	host.Status.Ready = true
	host.Status.InspectionTimestamp = &started
	host.Status.InspectionPhase = infrav1.InspectionPhaseBooting
	if state == infrav1.StateDeploying {
		host.Status.InspectionPhase = infrav1.InspectionPhaseComplete
		host.Status.DeployingTimestamp = &started
	}
	setTrue(host, infrav1.RedfishConnectionReadyCondition, infrav1.RedfishConnectedReason)
	Expect(k8sClient.Status().Update(ctx, host)).To(Succeed())
	return client.ObjectKeyFromObject(host)
}

func reachableBMC() internalredfish.RedfishClientFactory {
	return (&bmcGate{reachable: true}).factory()
}

func getPhysicalHost(key client.ObjectKey) *infrav1.PhysicalHost {
	host := &infrav1.PhysicalHost{}
	Expect(k8sClient.Get(ctx, key, host)).To(Succeed())
	return host
}

// releasePhysicalHost clears the claim the way Beskar7Machine deletion does.
func releasePhysicalHost(key client.ObjectKey) {
	host := getPhysicalHost(key)
	released := host.DeepCopy()
	released.Spec.ConsumerRef = nil
	Expect(k8sClient.Patch(ctx, released, client.MergeFrom(host))).To(Succeed())
}

// judgeHost hands a host, exactly as published, to the Beskar7Machine that
// holds it. The reconciler has no client and no Redfish factory: judging a
// failed run must not need either.
func judgeHost(host *infrav1.PhysicalHost) (*infrav1.Beskar7Machine, ctrl.Result) {
	machine := &infrav1.Beskar7Machine{ObjectMeta: metav1.ObjectMeta{
		Name: host.Spec.ConsumerRef.Name, Namespace: host.Namespace,
	}}
	r := &Beskar7MachineReconciler{Log: ctrl.Log.WithName("run-failure-machine")}
	result, err := r.handlePhysicalHostState(ctx, r.Log, machine, host)
	Expect(err).NotTo(HaveOccurred())
	return machine, result
}

var _ = Describe("Claimed PhysicalHost whose provisioning run failed", func() {
	const (
		bmcAddress    = "https://mock-redfish.example.invalid:8443"
		retryInterval = 2 * time.Second
	)

	var ns *corev1.Namespace

	BeforeEach(func() {
		ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "run-failure-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		Expect(k8sClient.Create(ctx, bmcCredentialsSecret(ns.Name))).To(Succeed())
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, ns)).To(Succeed())
	})

	newHostReconciler := func() *PhysicalHostReconciler {
		return &PhysicalHostReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			Log:                    ctrl.Log.WithName("run-failure-host"),
			Recorder:               record.NewFakeRecorder(10),
			RedfishClientFactory:   reachableBMC(),
			TransientRetryInterval: retryInterval,
		}
	}

	reconcileHost := func(r *PhysicalHostReconciler, key client.ObjectKey) (ctrl.Result, error) {
		return r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	}

	// deployFailedHost drives a Deploying host through the reconcile that
	// applies the inspector's report and returns it with the persisted message.
	deployFailedHost := func(r *PhysicalHostReconciler, name string) (client.ObjectKey, string) {
		report := sanitizeFailureReason("COS_OEM partition not found")
		key := provisioningHost(ns.Name, name, name+"-machine", infrav1.StateDeploying,
			map[string]string{ProvisionFailedRequestAnnotation: report})
		_, err := reconcileHost(r, key)
		Expect(err).NotTo(HaveOccurred())
		failed := getPhysicalHost(key)
		Expect(failed.Status.State).To(Equal(infrav1.StateError))
		Expect(failed.Status.ErrorMessage).To(Equal(report))
		return key, report
	}

	DescribeTable("the inspector's /provision-failed report, through a full reconcile",
		func(value, report string) {
			key := provisioningHost(ns.Name, "deploy-failed-host", "deploy-failed-machine", infrav1.StateDeploying,
				map[string]string{ProvisionFailedRequestAnnotation: value})
			hostReconciler := newHostReconciler()

			By("reconciling the host once, with its BMC answering")
			_, err := reconcileHost(hostReconciler, key)
			Expect(err).NotTo(HaveOccurred())

			failed := getPhysicalHost(key)
			Expect(failed.Annotations).NotTo(HaveKey(ProvisionFailedRequestAnnotation), "the report is consumed")
			Expect(failed.Status.State).To(Equal(infrav1.StateError),
				"the claimed-host branch must not put the host back at InUse in the pass that applied the report")
			Expect(failed.Status.ErrorMessage).To(Equal(report))
			Expect(failed.Status.ErrorMessage).To(HavePrefix(provisionFailedReasonPrefix),
				"the prefix is how the controllers tell the inspector's report from a BMC error")
			Expect(failed.Status.Ready).To(BeFalse())
			Expect(conditions.IsTrue(failed, infrav1.RedfishConnectionReadyCondition)).To(BeTrue())
			Expect(conditions.GetReason(failed, infrav1.HostAvailableCondition)).To(Equal(infrav1.HostClaimedReason))

			By("reconciling again: the Error stays, and the pass writes nothing")
			_, err = reconcileHost(hostReconciler, key)
			Expect(err).NotTo(HaveOccurred())
			again := getPhysicalHost(key)
			Expect(again.Status.State).To(Equal(infrav1.StateError))
			Expect(again.Status.ErrorMessage).To(Equal(report))
			Expect(again.ResourceVersion).To(Equal(failed.ResourceVersion))

			By("handing the persisted host to its machine")
			machine, result := judgeHost(again)
			Expect(result).To(Equal(ctrl.Result{}), "a terminal failure does not requeue")
			Expect(isTerminallyFailed(machine)).To(BeTrue())
			cond := conditions.Get(machine, infrav1.InfrastructureReadyCondition)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(infrav1.DeploymentFailedReason))
			Expect(cond.Message).To(ContainSubstring(report))
		},
		Entry("with a reason",
			sanitizeFailureReason("image digest mismatch"), provisionFailedReasonPrefix+"image digest mismatch"),
		Entry("with no reason, which the handler stores as the generic message",
			sanitizeFailureReason(""), provisionFailedReasonPrefix+"no details provided"),
		// A callback-only instance runs the handler in a process of its own, and
		// one still on the previous release wrote its generic report unprefixed.
		Entry("with the unprefixed generic message of a callback-only instance on the previous release",
			"inspector reported deploy failure (no details provided)",
			provisionFailedReasonPrefix+"inspector reported deploy failure (no details provided)"),
		Entry("set by hand with no value", "", provisionFailedReasonGeneric),
	)

	It("keeps an inspection timeout through a report the inspector posts late, and its machine reads it as InspectionTimedOut", func() {
		key := provisioningHost(ns.Name, "timed-out-host", "timed-out-machine", infrav1.StateInspecting,
			map[string]string{InspectionRequestAnnotation: "timeout"})
		hostReconciler := newHostReconciler()

		_, err := reconcileHost(hostReconciler, key)
		Expect(err).NotTo(HaveOccurred())
		timedOut := getPhysicalHost(key)
		Expect(timedOut.Annotations).NotTo(HaveKey(InspectionRequestAnnotation))
		Expect(timedOut.Status.State).To(Equal(infrav1.StateError))
		Expect(timedOut.Status.ErrorMessage).To(Equal(inspectionTimedOutMessage))
		Expect(timedOut.Status.InspectionPhase).To(Equal(infrav1.InspectionPhaseTimeout))
		Expect(timedOut.Status.Ready).To(BeFalse())

		By("letting the inspector's report arrive after the timeout")
		// The inspection handler does not look at the host's state, so a slow
		// inspector's report still lands, and the host applies it.
		handler := &InspectionHandler{Client: k8sClient, Log: ctrl.Log.WithName("run-failure-inspection")}
		Expect(handler.processInspectionReport(ctx, handler.Log, ns.Name, key.Name,
			InspectionReportRequest{Manufacturer: "Acme", Model: "Slow-1000"})).To(Succeed())
		_, err = reconcileHost(hostReconciler, key)
		Expect(err).NotTo(HaveOccurred())

		late := getPhysicalHost(key)
		Expect(late.Annotations).NotTo(HaveKey(InspectionResultAnnotation))
		Expect(late.Status.InspectionPhase).To(Equal(infrav1.InspectionPhaseComplete),
			"the late report moves the phase on, so the phase cannot be what marks the timeout")
		Expect(late.Status.State).To(Equal(infrav1.StateError))
		Expect(late.Status.ErrorMessage).To(Equal(inspectionTimedOutMessage))

		By("handing the persisted host to its machine")
		// The machine fails itself when it writes the timeout annotation and only
		// reads this Error if that reconcile's own status patch did not land. A
		// healthy connection must not read as a BMC it can wait for.
		machine, result := judgeHost(late)
		Expect(result).To(Equal(ctrl.Result{}))
		Expect(isTerminallyFailed(machine)).To(BeTrue())
		Expect(conditions.GetReason(machine, infrav1.InfrastructureReadyCondition)).To(Equal(infrav1.InspectionTimedOutReason))
	})

	// patchConnection edits a host's RedfishConnection and returns it as it was.
	patchConnection := func(key client.ObjectKey, edit func(*infrav1.RedfishConnection)) infrav1.RedfishConnection {
		host := getPhysicalHost(key)
		before := host.Spec.RedfishConnection
		edited := host.DeepCopy()
		edit(&edited.Spec.RedfishConnection)
		Expect(k8sClient.Patch(ctx, edited, client.MergeFrom(host))).To(Succeed())
		return before
	}

	// bmcFailure is one way the connection fails after the run did: a BMC that
	// fails (factory), a credentials Secret that goes away, or a spec edit
	// (breakConnection) that is undone afterwards. requeueAfter is what the
	// reconcile returns when it hands the workqueue no error.
	type bmcFailure struct {
		factory         internalredfish.RedfishClientFactory
		dropCredentials bool
		breakConnection func(*infrav1.RedfishConnection)
		hostReason      string
		requeueAfter    time.Duration
	}

	// Once the run has failed, a BMC failure is RedfishConnectionReady's to
	// report. The run's Error is the reason its machine fails with, and an
	// outage message in its place reads to the machine as an outage to wait out.
	DescribeTable("the run's Error through a later BMC failure",
		func(failure bmcFailure) {
			hostReconciler := newHostReconciler()
			key, report := deployFailedHost(hostReconciler, "failed-run-host")

			By("reconciling while the connection fails")
			if failure.factory != nil {
				hostReconciler.RedfishClientFactory = failure.factory
			}
			if failure.dropCredentials {
				Expect(k8sClient.Delete(ctx, bmcCredentialsSecret(ns.Name))).To(Succeed())
			}
			var healthy infrav1.RedfishConnection
			if failure.breakConnection != nil {
				healthy = patchConnection(key, failure.breakConnection)
			}
			result, err := reconcileHost(hostReconciler, key)
			if failure.requeueAfter > 0 {
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(failure.requeueAfter), "the failure keeps its own retry cadence")
			} else {
				Expect(err).To(HaveOccurred(), "a failure that needs a change keeps the workqueue's backoff")
			}

			during := getPhysicalHost(key)
			Expect(conditions.IsFalse(during, infrav1.RedfishConnectionReadyCondition)).To(BeTrue())
			Expect(conditions.GetReason(during, infrav1.RedfishConnectionReadyCondition)).To(Equal(failure.hostReason))
			Expect(during.Status.State).To(Equal(infrav1.StateError))
			Expect(during.Status.ErrorMessage).To(Equal(report),
				"the condition reports the BMC; the message keeps the inspector's reason")
			Expect(during.Status.Ready).To(BeFalse())

			By("repeating the failure without writing to the host")
			_, _ = reconcileHost(hostReconciler, key)
			Expect(getPhysicalHost(key).ResourceVersion).To(Equal(during.ResourceVersion))

			By("handing the host to its machine while the connection is down")
			machine, result := judgeHost(during)
			Expect(result).To(Equal(ctrl.Result{}))
			Expect(isTerminallyFailed(machine)).To(BeTrue(), "a failed run is not an outage to wait out")
			Expect(conditions.GetReason(machine, infrav1.InfrastructureReadyCondition)).To(Equal(infrav1.DeploymentFailedReason))

			By("letting the BMC answer again")
			if failure.dropCredentials {
				Expect(k8sClient.Create(ctx, bmcCredentialsSecret(ns.Name))).To(Succeed())
			}
			if failure.breakConnection != nil {
				patchConnection(key, func(c *infrav1.RedfishConnection) { *c = healthy })
			}
			hostReconciler.RedfishClientFactory = reachableBMC()
			_, err = reconcileHost(hostReconciler, key)
			Expect(err).NotTo(HaveOccurred())
			after := getPhysicalHost(key)
			Expect(conditions.IsTrue(after, infrav1.RedfishConnectionReadyCondition)).To(BeTrue())
			Expect(after.Status.State).To(Equal(infrav1.StateError), "only a release ends the run's Error")
			Expect(after.Status.ErrorMessage).To(Equal(report))
		},
		Entry("the BMC refuses connections", bmcFailure{
			factory:    failingBMC(refusedConnection(bmcAddress)),
			hostReason: infrav1.BMCUnreachableReason, requeueAfter: retryInterval,
		}),
		Entry("the BMC drops the connection after answering the service root", bmcFailure{
			factory:    refusingBMCAfterServiceRoot(bmcAddress),
			hostReason: infrav1.BMCUnreachableReason, requeueAfter: retryInterval,
		}),
		Entry("the BMC rejects the credentials", bmcFailure{
			factory:    failingBMC(common.ConstructError(401, []byte("unauthorized"))),
			hostReason: infrav1.RedfishConnectionFailedReason,
		}),
		Entry("the BMC has no ComputerSystem", bmcFailure{
			factory: func(_ context.Context, _, _, _ string, _ bool, _ []byte) (internalredfish.Client, error) {
				empty := internalredfish.NewMockClient()
				empty.ShouldFail["GetSystemInfo"] = errors.New("no systems found")
				return empty, nil
			},
			hostReason: infrav1.RedfishQueryFailedReason,
		}),
		Entry("the credentials Secret is deleted", bmcFailure{
			factory:         failingBMC(errors.New("the factory must not be reached")),
			dropCredentials: true,
			hostReason:      infrav1.MissingCredentialsReason,
		}),
		Entry("insecureSkipVerify is combined with a CA bundle", bmcFailure{
			breakConnection: func(c *infrav1.RedfishConnection) {
				c.InsecureSkipVerify = ptr.To(true)
				c.CABundleSecretRef = "bmc-ca"
			},
			hostReason:   infrav1.InsecureCABundleConflictReason,
			requeueAfter: 5 * time.Minute,
		}),
		Entry("the CA bundle Secret does not exist", bmcFailure{
			breakConnection: func(c *infrav1.RedfishConnection) { c.CABundleSecretRef = "bmc-ca" },
			hostReason:      infrav1.CABundleFetchFailedReason,
		}),
	)

	It("returns the host to Available once it is released", func() {
		hostReconciler := newHostReconciler()
		key, _ := deployFailedHost(hostReconciler, "released-host")

		releasePhysicalHost(key)
		_, err := reconcileHost(hostReconciler, key)
		Expect(err).NotTo(HaveOccurred())

		available := getPhysicalHost(key)
		Expect(available.Status.State).To(Equal(infrav1.StateAvailable))
		Expect(available.Status.Ready).To(BeTrue())
		Expect(available.Status.ErrorMessage).To(BeEmpty())
		Expect(available.Status.InspectionPhase).To(BeEmpty())
		Expect(available.Status.InspectionTimestamp).To(BeNil())
		Expect(available.Status.DeployingTimestamp).To(BeNil())
		Expect(conditions.IsTrue(available, infrav1.HostAvailableCondition)).To(BeTrue())
	})

	It("drops the run's Error when the host is released while its BMC is down", func() {
		key := provisioningHost(ns.Name, "released-in-outage", "released-in-outage-machine", infrav1.StateInspecting,
			map[string]string{InspectionRequestAnnotation: "timeout"})
		hostReconciler := newHostReconciler()
		_, err := reconcileHost(hostReconciler, key)
		Expect(err).NotTo(HaveOccurred())
		Expect(getPhysicalHost(key).Status.ErrorMessage).To(Equal(inspectionTimedOutMessage))

		By("releasing the host while its BMC refuses connections")
		releasePhysicalHost(key)
		hostReconciler.RedfishClientFactory = failingBMC(refusedConnection(bmcAddress))
		_, err = reconcileHost(hostReconciler, key)
		Expect(err).NotTo(HaveOccurred())
		unreachable := getPhysicalHost(key)
		Expect(unreachable.Status.State).To(Equal(infrav1.StateError))
		Expect(unreachable.Status.ErrorMessage).To(ContainSubstring("BMC unreachable"),
			"the run's Error went with the consumer that released the host")

		By("letting the BMC answer again")
		hostReconciler.RedfishClientFactory = reachableBMC()
		_, err = reconcileHost(hostReconciler, key)
		Expect(err).NotTo(HaveOccurred())
		available := getPhysicalHost(key)
		Expect(available.Status.State).To(Equal(infrav1.StateAvailable))
		Expect(available.Status.ErrorMessage).To(BeEmpty())
		Expect(available.Status.InspectionPhase).To(BeEmpty())
	})
})

// The specs above call the reconcilers by hand. This one runs both under a
// manager, where the report reaches the machine only through the PhysicalHost
// watch — the same watch that used to bring it the host back at InUse, on which
// it booted the inspector again.
var _ = Describe("Deploy failure with both controllers under a running manager", func() {
	It("fails the machine with DeploymentFailed and leaves the host in Error, without touching the BMC again", func() {
		testNs := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "run-failure-mgr-"}}
		Expect(k8sClient.Create(ctx, testNs)).To(Succeed())
		defer func() { Expect(k8sClient.Delete(ctx, testNs)).To(Succeed()) }()
		ns := testNs.Name
		const clusterName = "deploy-failure-cluster"

		Expect(k8sClient.Create(ctx, &clusterv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns},
			Spec:       clusterv1.ClusterSpec{Paused: ptr.To(false)},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "deploy-failure-bootstrap", Namespace: ns},
			Data:       map[string][]byte{"value": []byte("#cloud-config\n")},
		})).To(Succeed())
		machine := &clusterv1.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name: "deploy-failure", Namespace: ns,
				Labels: map[string]string{clusterv1.ClusterNameLabel: clusterName},
			},
			Spec: clusterv1.MachineSpec{
				ClusterName: clusterName,
				InfrastructureRef: clusterv1.ContractVersionedObjectReference{
					APIGroup: infrav1.GroupVersion.Group, Kind: "Beskar7Machine", Name: "deploy-failure",
				},
				Bootstrap: clusterv1.Bootstrap{DataSecretName: ptr.To("deploy-failure-bootstrap")},
			},
		}
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())
		Expect(k8sClient.Create(ctx, bmcCredentialsSecret(ns))).To(Succeed())
		b7m := &infrav1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name: "deploy-failure", Namespace: ns,
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
		hostKey := provisioningHost(ns, "deploy-failure-host", b7m.Name, infrav1.StateDeploying, nil)

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
			Client:               mgr.GetClient(),
			Scheme:               mgr.GetScheme(),
			Log:                  ctrl.Log.WithName("run-failure-mgr-host"),
			Recorder:             record.NewFakeRecorder(100),
			RedfishClientFactory: reachableBMC(),
		}).SetupWithManager(mgr)).To(Succeed())
		// The host is Deploying from the start, so the machine never has a reason
		// to reach for the BMC; before the fix it did, to boot the inspector again.
		var machineBMCCalls atomic.Int32
		Expect((&Beskar7MachineReconciler{
			Client: mgr.GetClient(),
			Scheme: mgr.GetScheme(),
			Log:    ctrl.Log.WithName("run-failure-mgr-machine"),
			RedfishClientFactory: func(_ context.Context, _, _, _ string, _ bool, _ []byte) (internalredfish.Client, error) {
				machineBMCCalls.Add(1)
				return internalredfish.NewMockClient(), nil
			},
			BootstrapURLBase: "https://example.com:8082",
		}).SetupWithManager(mgr)).To(Succeed())

		mgrCtx, mgrCancel := context.WithCancel(ctx)
		defer mgrCancel()
		go func() {
			defer GinkgoRecover()
			Expect(mgr.Start(mgrCtx)).To(Succeed())
		}()
		Expect(mgr.GetCache().WaitForCacheSync(mgrCtx)).To(BeTrue())

		By("waiting for the machine to follow its host's deployment")
		Eventually(func(g Gomega) {
			got := &infrav1.Beskar7Machine{}
			g.Expect(k8sClient.Get(ctx, machineKey, got)).To(Succeed())
			g.Expect(ptr.Deref(got.Status.Phase, "")).To(Equal("Provisioning"))
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())

		By("reporting a deploy failure the way the /provision-failed handler does")
		handler := &ProvisionFailedHandler{Client: k8sClient, Log: ctrl.Log.WithName("run-failure-mgr-handler")}
		Expect(handler.signalProvisionFailed(ctx, handler.Log, ns, hostKey.Name,
			sanitizeFailureReason("disk write I/O error"))).To(Succeed())

		By("waiting for the machine to fail with the inspector's reason")
		Eventually(func(g Gomega) {
			got := &infrav1.Beskar7Machine{}
			g.Expect(k8sClient.Get(ctx, machineKey, got)).To(Succeed())
			g.Expect(isTerminallyFailed(got)).To(BeTrue())
			cond := conditions.Get(got, infrav1.InfrastructureReadyCondition)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Reason).To(Equal(infrav1.DeploymentFailedReason))
			g.Expect(cond.Message).To(ContainSubstring("disk write I/O error"))
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())

		By("checking the host stays in Error and nothing reaches for the BMC")
		Consistently(func(g Gomega) {
			h := &infrav1.PhysicalHost{}
			g.Expect(k8sClient.Get(ctx, hostKey, h)).To(Succeed())
			g.Expect(h.Status.State).To(Equal(infrav1.StateError))
			g.Expect(h.Status.ErrorMessage).To(Equal(provisionFailedReasonPrefix + "disk write I/O error"))
			g.Expect(h.Annotations).NotTo(HaveKey(InspectionRequestAnnotation))
			g.Expect(machineBMCCalls.Load()).To(BeZero(), "the machine ran triggerInspection again")
		}, 3*time.Second, 200*time.Millisecond).Should(Succeed())
	})
})
