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
	"crypto/x509"
	"errors"
	"fmt"
	"net/url"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/stmcginnis/gofish/common"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
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

// A BMC that stops answering is a fact about the world, not about the
// Beskar7Machine holding the host: it usually clears by itself, and the
// PhysicalHost retries until it does (physicalhost_transient_retry_test.go).
// These specs pin the machine's half of that: it waits the outage out and
// carries on, a host part-way through provisioning comes back where it was,
// and every host error that needs a change to clear stays terminal.

func bmcCredentialsSecret(namespace string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bmc-credentials", Namespace: namespace},
		Data:       map[string][]byte{"username": []byte("admin"), "password": []byte("pw")},
	}
}

func claimedPhysicalHost(namespace, name, machineName string) *infrav1.PhysicalHost {
	return &infrav1.PhysicalHost{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: infrav1.PhysicalHostSpec{
			RedfishConnection: infrav1.RedfishConnection{
				Address:              "https://mock-redfish.example.invalid:8443",
				CredentialsSecretRef: "bmc-credentials",
			},
			ConsumerRef: &corev1.ObjectReference{
				Kind: "Beskar7Machine", Name: machineName, Namespace: namespace,
				APIVersion: infrav1.GroupVersion.String(),
			},
		},
	}
}

func refusingBMCAfterServiceRoot(address string) internalredfish.RedfishClientFactory {
	dropping := internalredfish.NewMockClient()
	dropping.ShouldFail["GetSystemInfo"] = fmt.Errorf("failed to retrieve systems: %w", refusedConnection(address))
	return func(_ context.Context, _, _, _ string, _ bool, _ []byte) (internalredfish.Client, error) {
		return dropping, nil
	}
}

func failingBMC(err error) internalredfish.RedfishClientFactory {
	return func(_ context.Context, _, _, _ string, _ bool, _ []byte) (internalredfish.Client, error) {
		return nil, err
	}
}

var _ = Describe("Beskar7Machine whose claimed PhysicalHost loses its BMC", func() {
	const retryInterval = 1 * time.Second

	It("waits out the outage instead of failing, and carries on once the host recovers", func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "bmc-outage-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		defer func() { Expect(k8sClient.Delete(ctx, ns)).To(Succeed()) }()

		Expect(k8sClient.Create(ctx, bmcCredentialsSecret(ns.Name))).To(Succeed())

		b7m := &infrav1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "outage-machine", Namespace: ns.Name},
			Spec: infrav1.Beskar7MachineSpec{
				InspectionImageURL: "http://boot/inspect.ipxe",
				TargetImageURL:     "http://boot/kairos.raw",
				TargetImageDigest:  bootTestDigest,
			},
		}
		Expect(k8sClient.Create(ctx, b7m)).To(Succeed())

		// Claimed by the machine above: this is the production shape.
		host := claimedPhysicalHost(ns.Name, "outage-host", b7m.Name)
		Expect(k8sClient.Create(ctx, host)).To(Succeed())
		hostKey := types.NamespacedName{Name: host.Name, Namespace: ns.Name}

		gate := &bmcGate{reachable: false}
		hostReconciler := &PhysicalHostReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			Log:                    ctrl.Log.WithName("bmc-outage-host"),
			Recorder:               record.NewFakeRecorder(100),
			RedfishClientFactory:   gate.factory(),
			TransientRetryInterval: retryInterval,
		}

		By("driving the claimed host to Error through a real transient BMC failure")
		_, err := hostReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: hostKey})
		Expect(err).NotTo(HaveOccurred())
		_, err = hostReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: hostKey})
		Expect(err).NotTo(HaveOccurred())

		errored := &infrav1.PhysicalHost{}
		Expect(k8sClient.Get(ctx, hostKey, errored)).To(Succeed())
		Expect(errored.Status.State).To(Equal(infrav1.StateError))
		Expect(errored.Status.ErrorMessage).NotTo(HavePrefix(provisionFailedReasonPrefix),
			"a BMC outage must not be mistaken for an inspector-reported deploy failure")
		Expect(errored.Status.ErrorMessage).To(ContainSubstring("BMC unreachable"))
		Expect(errored.Status.ErrorMessage).To(ContainSubstring("retrying every 1s"))
		hostCond := conditions.Get(errored, infrav1.RedfishConnectionReadyCondition)
		Expect(hostCond).NotTo(BeNil())
		Expect(hostCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(hostCond.Reason).To(Equal(infrav1.BMCUnreachableReason),
			"the host publishes the class, so the machine does not have to read it out of the message")

		By("letting the machine controller see its claimed host in Error")
		mockRf := internalredfish.NewMockClient()
		machineReconciler := &Beskar7MachineReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			Log: ctrl.Log.WithName("bmc-outage-machine"),
			RedfishClientFactory: func(_ context.Context, _, _, _ string, _ bool, _ []byte) (internalredfish.Client, error) {
				return mockRf, nil
			},
			BootstrapURLBase: "https://example.com:8082",
		}
		result, err := machineReconciler.handlePhysicalHostState(ctx, machineReconciler.Log, b7m, errored)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(And(BeNumerically(">", 0), BeNumerically("<=", time.Minute)),
			"a bounded requeue: the machine checks back by itself")

		Expect(isTerminallyFailed(b7m)).To(BeFalse(), "a transient BMC outage must not terminally fail the machine")
		Expect(b7m.Status.Ready).To(BeFalse())
		cond := conditions.Get(b7m, infrav1.InfrastructureReadyCondition)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(infrav1.WaitingForBMCReason))
		Expect(cond.Message).To(ContainSubstring("BMC unreachable"),
			"the machine quotes the host's BMC error, so it says what it is waiting for")

		By("letting the BMC answer again, so the host self-heals")
		gate.setReachable(true)
		_, err = hostReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: hostKey})
		Expect(err).NotTo(HaveOccurred())
		recovered := &infrav1.PhysicalHost{}
		Expect(k8sClient.Get(ctx, hostKey, recovered)).To(Succeed())
		Expect(recovered.Status.State).To(Equal(infrav1.StateInUse),
			"a claimed host that recovers goes back to InUse, not Error")

		By("confirming the machine recovers with it")
		Expect(isTerminallyFailed(b7m)).To(BeFalse(),
			"the machine is not Failed, so Reconcile does not skip it now that its host is healthy again")
		_, err = machineReconciler.handlePhysicalHostState(ctx, machineReconciler.Log, b7m, recovered)
		Expect(err).NotTo(HaveOccurred())
		Expect(ptr.Deref(b7m.Status.Phase, "")).To(Equal("Inspecting"),
			"the machine boots the inspector, as it would have without the outage")
		Expect(conditions.GetReason(b7m, infrav1.InfrastructureReadyCondition)).NotTo(Equal(infrav1.WaitingForBMCReason),
			"nothing is left saying the machine waits for a BMC that has answered")
		requested := &infrav1.PhysicalHost{}
		Expect(k8sClient.Get(ctx, hostKey, requested)).To(Succeed())
		Expect(requested.Annotations).To(HaveKeyWithValue(InspectionRequestAnnotation, "inspect"))
	})
})

// The host's deferred patch writes conditions in a call of their own, ahead of
// the rest of status (CAPI's patch.Helper). A host whose BMC has just answered
// is therefore published once with RedfishConnectionReady=True and State still
// Error. Keying the machine's decision on the condition reason alone would fail
// it on exactly that version, so this watches every version the recovery
// publishes and hands each one to the machine.
var _ = Describe("PhysicalHost versions published while it recovers from a BMC outage", func() {
	It("never hands its machine a version it would be failed on", func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "bmc-recovery-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		defer func() { Expect(k8sClient.Delete(ctx, ns)).To(Succeed()) }()

		Expect(k8sClient.Create(ctx, bmcCredentialsSecret(ns.Name))).To(Succeed())
		host := claimedPhysicalHost(ns.Name, "recovering-host", "waiting-machine")
		Expect(k8sClient.Create(ctx, host)).To(Succeed())
		hostKey := client.ObjectKeyFromObject(host)

		gate := &bmcGate{reachable: false}
		hostReconciler := &PhysicalHostReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			Log:                    ctrl.Log.WithName("bmc-recovery-host"),
			Recorder:               record.NewFakeRecorder(100),
			RedfishClientFactory:   gate.factory(),
			TransientRetryInterval: time.Second,
		}
		for range 2 { // the first pass only adds the finalizer
			_, err := hostReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: hostKey})
			Expect(err).NotTo(HaveOccurred())
		}
		errored := &infrav1.PhysicalHost{}
		Expect(k8sClient.Get(ctx, hostKey, errored)).To(Succeed())
		Expect(errored.Status.State).To(Equal(infrav1.StateError))

		watching, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
		Expect(err).NotTo(HaveOccurred())
		w, err := watching.Watch(ctx, &infrav1.PhysicalHostList{},
			client.InNamespace(ns.Name),
			client.MatchingFields{"metadata.name": host.Name},
			&client.ListOptions{Raw: &metav1.ListOptions{ResourceVersion: errored.ResourceVersion}})
		Expect(err).NotTo(HaveOccurred())
		defer w.Stop()

		By("letting the BMC answer and reconciling the host once")
		gate.setReachable(true)
		_, err = hostReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: hostKey})
		Expect(err).NotTo(HaveOccurred())

		var published []*infrav1.PhysicalHost
		deadline := time.After(10 * time.Second)
	collect:
		for {
			select {
			case ev, open := <-w.ResultChan():
				Expect(open).To(BeTrue(), "the watch closed before the host left Error")
				Expect(ev.Type).NotTo(Equal(watch.Error), "watch error: %v", ev.Object)
				h, isHost := ev.Object.(*infrav1.PhysicalHost)
				Expect(isHost).To(BeTrue(), "unexpected object on the watch: %T", ev.Object)
				published = append(published, h)
				if h.Status.State != infrav1.StateError {
					break collect
				}
			case <-deadline:
				Fail("the host did not leave Error within 10s of the recovery reconcile")
			}
		}
		Expect(published[len(published)-1].Status.State).To(Equal(infrav1.StateInUse))

		By("handing every version still in Error to a machine that is waiting on it")
		machineReconciler := &Beskar7MachineReconciler{Log: ctrl.Log.WithName("bmc-recovery-machine")}
		sawConditionAheadOfState := false
		for _, h := range published {
			if h.Status.State != infrav1.StateError {
				continue
			}
			if conditions.IsTrue(h, infrav1.RedfishConnectionReadyCondition) {
				sawConditionAheadOfState = true
			}
			machine := &infrav1.Beskar7Machine{ObjectMeta: metav1.ObjectMeta{Name: "waiting-machine", Namespace: ns.Name}}
			_, err := machineReconciler.handlePhysicalHostState(ctx, machineReconciler.Log, machine, h)
			Expect(err).NotTo(HaveOccurred())
			Expect(isTerminallyFailed(machine)).To(BeFalse(),
				"version %s (RedfishConnectionReady %s/%s) failed the machine",
				h.ResourceVersion, conditions.Get(h, infrav1.RedfishConnectionReadyCondition).Status,
				conditions.GetReason(h, infrav1.RedfishConnectionReadyCondition))
		}
		Expect(sawConditionAheadOfState).To(BeTrue(),
			"expected the patch helper to publish RedfishConnectionReady=True while State was still Error. "+
				"If a Cluster API bump changed that ordering, the ConditionTrue case in hostWaitingForBMC "+
				"no longer has a reason to exist")
	})
})

// Every way the real PhysicalHost reconciler leaves a claimed host in Error,
// handed to the machine exactly as the host publishes it. Only the BMC outage
// is waited out; everything that needs a change to clear fails the machine with
// the reason it always had.
var _ = Describe("Beskar7Machine against each way its PhysicalHost reaches Error", func() {
	var ns *corev1.Namespace

	BeforeEach(func() {
		ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "bmc-fault-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, ns)).To(Succeed())
	})

	type hostFault struct {
		noCredentials bool
		connection    func(*infrav1.RedfishConnection)
		factory       internalredfish.RedfishClientFactory
		hostReason    string
		machineReason string
		terminal      bool
	}

	const bmcAddress = "https://mock-redfish.example.invalid:8443"

	DescribeTable("the machine's decision",
		func(fault hostFault) {
			if !fault.noCredentials {
				Expect(k8sClient.Create(ctx, bmcCredentialsSecret(ns.Name))).To(Succeed())
			}
			host := claimedPhysicalHost(ns.Name, "faulty-host", "faulty-machine")
			if fault.connection != nil {
				fault.connection(&host.Spec.RedfishConnection)
			}
			Expect(k8sClient.Create(ctx, host)).To(Succeed())
			hostKey := client.ObjectKeyFromObject(host)

			hostReconciler := &PhysicalHostReconciler{
				Client: k8sClient, Scheme: k8sClient.Scheme(),
				Log:                  ctrl.Log.WithName("bmc-fault-host"),
				Recorder:             record.NewFakeRecorder(10),
				RedfishClientFactory: fault.factory,
			}
			_, err := hostReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: hostKey})
			Expect(err).NotTo(HaveOccurred(), "the first pass only adds the finalizer")
			// The second pass fails the way the entry describes. The failures that
			// need a change return their error for the workqueue's backoff; that
			// is the host's business and not what this table is about.
			_, _ = hostReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: hostKey})

			published := &infrav1.PhysicalHost{}
			Expect(k8sClient.Get(ctx, hostKey, published)).To(Succeed())
			Expect(published.Status.State).To(Equal(infrav1.StateError))
			Expect(conditions.GetReason(published, infrav1.RedfishConnectionReadyCondition)).To(Equal(fault.hostReason))

			machine := &infrav1.Beskar7Machine{ObjectMeta: metav1.ObjectMeta{Name: "faulty-machine", Namespace: ns.Name}}
			machineReconciler := &Beskar7MachineReconciler{Log: ctrl.Log.WithName("bmc-fault-machine")}
			result, err := machineReconciler.handlePhysicalHostState(ctx, machineReconciler.Log, machine, published)
			Expect(err).NotTo(HaveOccurred())
			Expect(isTerminallyFailed(machine)).To(Equal(fault.terminal))
			Expect(conditions.GetReason(machine, infrav1.InfrastructureReadyCondition)).To(Equal(fault.machineReason))
			if fault.terminal {
				Expect(result).To(Equal(ctrl.Result{}), "a terminal failure does not requeue")
			} else {
				Expect(result.RequeueAfter).To(BeNumerically(">", 0), "a wait checks back by itself")
			}
		},
		Entry("the BMC refuses connections", hostFault{
			factory:    failingBMC(refusedConnection(bmcAddress)),
			hostReason: infrav1.BMCUnreachableReason, machineReason: infrav1.WaitingForBMCReason,
		}),
		Entry("the BMC drops the connection after answering the service root", hostFault{
			factory:    refusingBMCAfterServiceRoot(bmcAddress),
			hostReason: infrav1.BMCUnreachableReason, machineReason: infrav1.WaitingForBMCReason,
		}),
		Entry("the BMC rejects the credentials", hostFault{
			factory:    failingBMC(common.ConstructError(401, []byte("unauthorized"))),
			hostReason: infrav1.RedfishConnectionFailedReason, machineReason: infrav1.PhysicalHostErrorReason, terminal: true,
		}),
		Entry("the BMC presents a certificate the client rejects", hostFault{
			factory:    failingBMC(&url.Error{Op: "Get", URL: bmcAddress + "/redfish/v1/", Err: x509.UnknownAuthorityError{}}),
			hostReason: infrav1.RedfishConnectionFailedReason, machineReason: infrav1.PhysicalHostErrorReason, terminal: true,
		}),
		Entry("the BMC address cannot be parsed", hostFault{
			factory: failingBMC(fmt.Errorf("invalid Redfish address format: %s: %w", bmcAddress,
				&url.Error{Op: "parse", URL: bmcAddress, Err: errors.New("invalid port")})),
			hostReason: infrav1.RedfishConnectionFailedReason, machineReason: infrav1.PhysicalHostErrorReason, terminal: true,
		}),
		Entry("the BMC has no ComputerSystem", hostFault{
			factory: func(_ context.Context, _, _, _ string, _ bool, _ []byte) (internalredfish.Client, error) {
				empty := internalredfish.NewMockClient()
				empty.ShouldFail["GetSystemInfo"] = errors.New("no systems found")
				return empty, nil
			},
			hostReason: infrav1.RedfishQueryFailedReason, machineReason: infrav1.PhysicalHostErrorReason, terminal: true,
		}),
		Entry("the credentials Secret does not exist", hostFault{
			noCredentials: true,
			factory:       failingBMC(errors.New("the factory must not be reached")),
			hostReason:    infrav1.MissingCredentialsReason, machineReason: infrav1.PhysicalHostErrorReason, terminal: true,
		}),
		Entry("insecureSkipVerify is combined with a CA bundle", hostFault{
			connection: func(c *infrav1.RedfishConnection) {
				c.InsecureSkipVerify = ptr.To(true)
				c.CABundleSecretRef = "bmc-ca"
			},
			factory:    failingBMC(errors.New("the factory must not be reached")),
			hostReason: infrav1.InsecureCABundleConflictReason, machineReason: infrav1.PhysicalHostErrorReason, terminal: true,
		}),
		Entry("the CA bundle Secret does not exist", hostFault{
			connection: func(c *infrav1.RedfishConnection) { c.CABundleSecretRef = "bmc-ca" },
			factory:    failingBMC(errors.New("the factory must not be reached")),
			hostReason: infrav1.CABundleFetchFailedReason, machineReason: infrav1.PhysicalHostErrorReason, terminal: true,
		}),
	)

	// The provision-failed Error is set after a successful connection, so its
	// host reads RedfishConnectionReady=True — the same condition a host has on
	// its way out of an outage. The inspector's prefix is what keeps it terminal.
	It("still fails the machine with DeploymentFailed for the inspector's report on a reachable host", func() {
		host := claimedPhysicalHost(ns.Name, "deploy-failed-host", "deploy-failed-machine")
		host.Status.State = infrav1.StateError
		host.Status.ErrorMessage = provisionFailedReasonPrefix + "image digest mismatch"
		setTrue(host, infrav1.RedfishConnectionReadyCondition, infrav1.RedfishConnectedReason)

		machine := &infrav1.Beskar7Machine{ObjectMeta: metav1.ObjectMeta{Name: "deploy-failed-machine", Namespace: ns.Name}}
		machineReconciler := &Beskar7MachineReconciler{Log: ctrl.Log.WithName("bmc-fault-machine")}
		result, err := machineReconciler.handlePhysicalHostState(ctx, machineReconciler.Log, machine, host)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(ctrl.Result{}))
		Expect(isTerminallyFailed(machine)).To(BeTrue())
		Expect(conditions.GetReason(machine, infrav1.InfrastructureReadyCondition)).To(Equal(infrav1.DeploymentFailedReason))
	})
})

// A claimed host that is Inspecting, Deploying or Ready is part-way through, or
// done with, a provisioning run its BMC plays no part in: the inspector and the
// installed OS run without it. It keeps that state through an outage and only
// RedfishConnectionReady reports it. Before, the outage overwrote the state with
// Error and the recovery could only restore InUse, so a machine that stopped
// being failed for the outage would have booted the inspector again on a host
// it had already provisioned. An InUse host still goes to Error, which it comes
// back from by itself.
var _ = Describe("Claimed PhysicalHost part-way through provisioning when its BMC goes away", func() {
	const retryInterval = 2 * time.Second

	var ns *corev1.Namespace

	BeforeEach(func() {
		ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "bmc-substate-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		Expect(k8sClient.Create(ctx, bmcCredentialsSecret(ns.Name))).To(Succeed())
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, ns)).To(Succeed())
	})

	// busyHost creates a claimed host already in state with a healthy BMC
	// connection, the way the annotation handlers would have left it.
	busyHost := func(name, state string) types.NamespacedName {
		host := claimedPhysicalHost(ns.Name, name, "busy-machine")
		host.Finalizers = []string{PhysicalHostFinalizer}
		Expect(k8sClient.Create(ctx, host)).To(Succeed())
		host.Status.State = state
		host.Status.Ready = true
		setTrue(host, infrav1.RedfishConnectionReadyCondition, infrav1.RedfishConnectedReason)
		Expect(k8sClient.Status().Update(ctx, host)).To(Succeed())
		return client.ObjectKeyFromObject(host)
	}

	DescribeTable("the host's state through the outage",
		func(state string, kept bool) {
			hostKey := busyHost("busy-host", state)
			gate := &bmcGate{reachable: false}
			hostReconciler := &PhysicalHostReconciler{
				Client: k8sClient, Scheme: k8sClient.Scheme(),
				Log:                    ctrl.Log.WithName("bmc-substate-host"),
				Recorder:               record.NewFakeRecorder(10),
				RedfishClientFactory:   gate.factory(),
				TransientRetryInterval: retryInterval,
			}
			reconcile := func() (ctrl.Result, error) {
				return hostReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: hostKey})
			}

			By("reconciling while the BMC refuses connections")
			result, err := reconcile()
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(retryInterval), "the transient retry cadence applies either way")

			during := &infrav1.PhysicalHost{}
			Expect(k8sClient.Get(ctx, hostKey, during)).To(Succeed())
			cond := conditions.Get(during, infrav1.RedfishConnectionReadyCondition)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(infrav1.BMCUnreachableReason))
			Expect(cond.Message).To(ContainSubstring("BMC unreachable"))
			if kept {
				Expect(during.Status.State).To(Equal(state))
				Expect(during.Status.Ready).To(BeTrue())
				Expect(during.Status.ErrorMessage).To(BeEmpty(),
					"the condition carries the outage; errorMessage stays reserved for the Error state")
			} else {
				Expect(during.Status.State).To(Equal(infrav1.StateError))
				Expect(during.Status.ErrorMessage).To(ContainSubstring("BMC unreachable"))
			}

			By("repeating the failure without writing to the host")
			_, err = reconcile()
			Expect(err).NotTo(HaveOccurred())
			again := &infrav1.PhysicalHost{}
			Expect(k8sClient.Get(ctx, hostKey, again)).To(Succeed())
			Expect(again.ResourceVersion).To(Equal(during.ResourceVersion))

			By("letting the BMC answer again")
			gate.setReachable(true)
			_, err = reconcile()
			Expect(err).NotTo(HaveOccurred())
			after := &infrav1.PhysicalHost{}
			Expect(k8sClient.Get(ctx, hostKey, after)).To(Succeed())
			Expect(after.Status.State).To(Equal(state), "the host comes back where it was")
			Expect(after.Status.ErrorMessage).To(BeEmpty())
			Expect(conditions.IsTrue(after, infrav1.RedfishConnectionReadyCondition)).To(BeTrue())
		},
		Entry("Inspecting is kept", infrav1.StateInspecting, true),
		Entry("Deploying is kept", infrav1.StateDeploying, true),
		Entry("Ready is kept", infrav1.StateReady, true),
		Entry("InUse goes to Error and comes back by itself", infrav1.StateInUse, false),
	)

	It("leaves a provisioned machine Ready while its host's BMC is down", func() {
		hostKey := busyHost("provisioned-host", infrav1.StateReady)
		hostReconciler := &PhysicalHostReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			Log:                    ctrl.Log.WithName("bmc-substate-host"),
			Recorder:               record.NewFakeRecorder(10),
			RedfishClientFactory:   (&bmcGate{reachable: false}).factory(),
			TransientRetryInterval: retryInterval,
		}
		_, err := hostReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: hostKey})
		Expect(err).NotTo(HaveOccurred())
		host := &infrav1.PhysicalHost{}
		Expect(k8sClient.Get(ctx, hostKey, host)).To(Succeed())
		Expect(conditions.GetReason(host, infrav1.RedfishConnectionReadyCondition)).To(Equal(infrav1.BMCUnreachableReason))

		machine := &infrav1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "busy-machine", Namespace: ns.Name},
			Spec:       infrav1.Beskar7MachineSpec{ProviderID: providerID(ns.Name, host.Name)},
			Status: infrav1.Beskar7MachineStatus{
				Ready:          true,
				Phase:          ptr.To("Provisioned"),
				Initialization: infrav1.Beskar7MachineInitializationStatus{Provisioned: ptr.To(true)},
			},
		}
		setTrue(machine, infrav1.InfrastructureReadyCondition, infrav1.ProvisionedReason)

		// No Redfish client: a provisioned machine must not need its BMC.
		machineReconciler := &Beskar7MachineReconciler{Log: ctrl.Log.WithName("bmc-substate-machine")}
		_, err = machineReconciler.handlePhysicalHostState(ctx, machineReconciler.Log, machine, host)
		Expect(err).NotTo(HaveOccurred())
		Expect(isTerminallyFailed(machine)).To(BeFalse())
		Expect(machine.Status.Ready).To(BeTrue())
		Expect(ptr.Deref(machine.Status.Phase, "")).To(Equal("Provisioned"))
		Expect(conditions.IsTrue(machine, infrav1.InfrastructureReadyCondition)).To(BeTrue(),
			"a serving node whose out-of-band management blinked is still a serving node")
	})
})

// The specs above call the reconcilers by hand. This one runs both controllers
// under a manager, which is the only way to see the machine resume on its own:
// once the BMC answers, nothing touches the machine — the host's recovery
// reaches it through the PhysicalHost watch.
var _ = Describe("BMC outage with both controllers under a running manager", func() {
	const retryInterval = 1 * time.Second

	var (
		testNs    *corev1.Namespace
		gate      *bmcGate
		mgrCancel context.CancelFunc
	)

	BeforeEach(func() {
		testNs = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "bmc-outage-mgr-"}}
		Expect(k8sClient.Create(ctx, testNs)).To(Succeed())

		skipNameValidation := true
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:                 k8sClient.Scheme(),
			Metrics:                metricsserver.Options{BindAddress: "0"},
			HealthProbeBindAddress: "0",
			// envtest never finishes deleting namespaces, so a cluster-wide cache
			// would hand these controllers every other spec's leftovers.
			Cache:      cache.Options{DefaultNamespaces: map[string]cache.Config{testNs.Name: {}}},
			Controller: config.Controller{SkipNameValidation: &skipNameValidation},
		})
		Expect(err).NotTo(HaveOccurred())

		gate = &bmcGate{reachable: false}
		Expect((&PhysicalHostReconciler{
			Client:                 mgr.GetClient(),
			Scheme:                 mgr.GetScheme(),
			Log:                    ctrl.Log.WithName("bmc-outage-mgr-host"),
			Recorder:               record.NewFakeRecorder(100),
			RedfishClientFactory:   gate.factory(),
			TransientRetryInterval: retryInterval,
		}).SetupWithManager(mgr)).To(Succeed())
		Expect((&Beskar7MachineReconciler{
			Client:               mgr.GetClient(),
			Scheme:               mgr.GetScheme(),
			Log:                  ctrl.Log.WithName("bmc-outage-mgr-machine"),
			RedfishClientFactory: gate.factory(),
			BootstrapURLBase:     "https://example.com:8082",
		}).SetupWithManager(mgr)).To(Succeed())

		var mgrCtx context.Context
		mgrCtx, mgrCancel = context.WithCancel(ctx)
		go func() {
			defer GinkgoRecover()
			Expect(mgr.Start(mgrCtx)).To(Succeed())
		}()
		Expect(mgr.GetCache().WaitForCacheSync(mgrCtx)).To(BeTrue())
	})

	AfterEach(func() {
		mgrCancel()
		Expect(k8sClient.Delete(ctx, testNs)).To(Succeed())
	})

	It("parks the machine while the BMC is down and resumes it once the host recovers", func() {
		ns := testNs.Name
		const clusterName = "outage-cluster"

		// The Cluster, Machine and bootstrap Secret the machine's Reconcile
		// resolves before it ever looks at its host.
		Expect(k8sClient.Create(ctx, &clusterv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns},
			Spec:       clusterv1.ClusterSpec{Paused: ptr.To(false)},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "outage-bootstrap", Namespace: ns},
			Data:       map[string][]byte{"value": []byte("#cloud-config\n")},
		})).To(Succeed())
		machine := &clusterv1.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name: "outage-machine", Namespace: ns,
				Labels: map[string]string{clusterv1.ClusterNameLabel: clusterName},
			},
			Spec: clusterv1.MachineSpec{
				ClusterName: clusterName,
				InfrastructureRef: clusterv1.ContractVersionedObjectReference{
					APIGroup: infrav1.GroupVersion.Group, Kind: "Beskar7Machine", Name: "outage-machine",
				},
				Bootstrap: clusterv1.Bootstrap{DataSecretName: ptr.To("outage-bootstrap")},
			},
		}
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())
		Expect(k8sClient.Create(ctx, bmcCredentialsSecret(ns))).To(Succeed())

		b7m := &infrav1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name: "outage-machine", Namespace: ns,
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
		host := claimedPhysicalHost(ns, "outage-host", b7m.Name)
		Expect(k8sClient.Create(ctx, host)).To(Succeed())
		hostKey := client.ObjectKeyFromObject(host)

		By("waiting for the machine to report that it waits for its host's BMC")
		Eventually(func(g Gomega) {
			got := &infrav1.Beskar7Machine{}
			g.Expect(k8sClient.Get(ctx, machineKey, got)).To(Succeed())
			g.Expect(conditions.GetReason(got, infrav1.InfrastructureReadyCondition)).To(Equal(infrav1.WaitingForBMCReason))
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())

		By("checking it keeps waiting, not Failed, across several host retries")
		Consistently(func(g Gomega) {
			got := &infrav1.Beskar7Machine{}
			g.Expect(k8sClient.Get(ctx, machineKey, got)).To(Succeed())
			g.Expect(isTerminallyFailed(got)).To(BeFalse())
			g.Expect(conditions.GetReason(got, infrav1.InfrastructureReadyCondition)).To(Equal(infrav1.WaitingForBMCReason))
		}, 3*retryInterval, 200*time.Millisecond).Should(Succeed())

		By("letting the BMC answer again, and waiting for the machine to boot the inspector by itself")
		gate.setReachable(true)
		Eventually(func(g Gomega) {
			got := &infrav1.Beskar7Machine{}
			g.Expect(k8sClient.Get(ctx, machineKey, got)).To(Succeed())
			g.Expect(isTerminallyFailed(got)).To(BeFalse())
			g.Expect(ptr.Deref(got.Status.Phase, "")).To(Equal("Inspecting"))
			g.Expect(conditions.GetReason(got, infrav1.InfrastructureReadyCondition)).NotTo(Equal(infrav1.WaitingForBMCReason))

			h := &infrav1.PhysicalHost{}
			g.Expect(k8sClient.Get(ctx, hostKey, h)).To(Succeed())
			g.Expect(h.Status.State).To(Equal(infrav1.StateInspecting))
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())
	})
})
