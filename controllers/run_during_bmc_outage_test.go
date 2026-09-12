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
	"encoding/json"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
	"github.com/projectbeskar/beskar7/internal/auth"
)

// A claimed PhysicalHost keeps its provisioning state through a BMC outage
// (bmc_outage_test.go), because neither the inspector nor the installed OS
// needs the BMC. Its run used to stop there all the same: apart from the
// inspector's two reports, the host acted on its annotations only after a
// successful BMC connection. During a network-level outage it read no
// inspection report, applied none of the machine's inspection requests and
// published no bootstrap token or boot nonce the machine had just minted:
//
//   - A machine whose inspection timeout ran out meanwhile failed with
//     InspectionTimedOut, although the inspector had reported and went on to
//     deploy the host.
//   - A machine whose inspect-complete waited sat Inspecting until a
//     MachineHealthCheck replaced it.
//   - An inspector the machine had just powered on could not fetch /boot: the
//     nonce in its iPXE script was not in the host's status.
//
// A BMC failure that needs a fix still holds the annotations until the
// connection works, as before.

// inUseHost creates a claimed host that is InUse with a healthy BMC
// connection, carrying annotations for its next reconcile.
func inUseHost(namespace, name, machineName string, annotations map[string]string) client.ObjectKey {
	host := claimedPhysicalHost(namespace, name, machineName)
	host.Finalizers = []string{PhysicalHostFinalizer}
	host.Annotations = annotations
	Expect(k8sClient.Create(ctx, host)).To(Succeed())
	host.Status.State = infrav1.StateInUse
	host.Status.Ready = true
	setTrue(host, infrav1.RedfishConnectionReadyCondition, infrav1.RedfishConnectedReason)
	Expect(k8sClient.Status().Update(ctx, host)).To(Succeed())
	return client.ObjectKeyFromObject(host)
}

// inspectedHost creates a claimed host that has read its inspection report,
// with a healthy BMC connection: the host its machine validates.
func inspectedHost(namespace, name, machineName string) client.ObjectKey {
	key := provisioningHost(namespace, name, machineName, infrav1.StateInspecting, nil)
	host := getPhysicalHost(key)
	host.Status.InspectionPhase = infrav1.InspectionPhaseComplete
	host.Status.InspectionReport = buildInspectionReport(InspectionReportRequest{
		Manufacturer: "Acme", Model: "Fast-1000", CPUs: []CPUData{{ID: "cpu0", Cores: 8}},
	})
	setTrue(host, infrav1.HostInspectedCondition, infrav1.HostInspectedReason)
	Expect(k8sClient.Status().Update(ctx, host)).To(Succeed())
	return key
}

// inspectionCredentials mints a bearer token and a boot nonce the way
// triggerInspection does, and returns the annotations that signal them to the
// host together with the plaintexts the inspector would present.
func inspectionCredentials() (map[string]string, string, string) {
	token, tokenHash, err := auth.MintToken()
	Expect(err).NotTo(HaveOccurred())
	issuedAt, expiresAt := auth.LifetimeFor(time.Now())
	tokenValue, err := json.Marshal(BootstrapTokenAnnotationValue{Hash: tokenHash, IssuedAt: issuedAt, ExpiresAt: expiresAt})
	Expect(err).NotTo(HaveOccurred())

	nonce, nonceHash, err := auth.MintToken()
	Expect(err).NotTo(HaveOccurred())
	nonceValue, err := json.Marshal(BootNonceAnnotationValue{Hash: nonceHash, ExpiresAt: auth.NonceLifetimeFor(time.Now())})
	Expect(err).NotTo(HaveOccurred())

	return map[string]string{
		BootstrapTokenAnnotation: string(tokenValue),
		BootNonceAnnotation:      string(nonceValue),
	}, token, nonce
}

// judgeWithInspectionTimeout hands a host, exactly as published, to the
// Beskar7Machine holding it, run with the given --inspection-timeout.
func judgeWithInspectionTimeout(host *infrav1.PhysicalHost, timeout time.Duration) *infrav1.Beskar7Machine {
	machine := &infrav1.Beskar7Machine{ObjectMeta: metav1.ObjectMeta{
		Name: host.Spec.ConsumerRef.Name, Namespace: host.Namespace,
	}}
	r := &Beskar7MachineReconciler{
		Client: k8sClient, Scheme: k8sClient.Scheme(),
		Log:               ctrl.Log.WithName("outage-run-machine"),
		InspectionTimeout: timeout,
	}
	_, err := r.handlePhysicalHostState(ctx, r.Log, machine, host)
	Expect(err).NotTo(HaveOccurred())
	return machine
}

var _ = Describe("A claimed PhysicalHost's provisioning run while its BMC is unreachable", func() {
	const retryInterval = 2 * time.Second

	var (
		ns             *corev1.Namespace
		gate           *bmcGate
		hostReconciler *PhysicalHostReconciler
	)

	BeforeEach(func() {
		ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "outage-run-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		Expect(k8sClient.Create(ctx, bmcCredentialsSecret(ns.Name))).To(Succeed())
		gate = &bmcGate{reachable: false}
		hostReconciler = &PhysicalHostReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			Log:                    ctrl.Log.WithName("outage-run-host"),
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

	It("reads an inspection report posted during the outage, and the run goes on to provision the machine", func() {
		key := provisioningHost(ns.Name, "report-host", "report-machine", infrav1.StateInspecting, nil)
		Expect(reconcileInOutage(key).Status.State).To(Equal(infrav1.StateInspecting))

		By("the inspector posting its inspection report")
		postInspectionReport(key)
		inspected := reconcileInOutage(key)
		Expect(inspected.Annotations).NotTo(HaveKey(InspectionResultAnnotation), "the report is read without the BMC")
		Expect(inspected.Status.InspectionPhase).To(Equal(infrav1.InspectionPhaseComplete))
		Expect(inspected.Status.InspectionReport).NotTo(BeNil())
		Expect(conditions.IsTrue(inspected, infrav1.HostInspectedCondition)).To(BeTrue())
		Expect(inspected.Status.State).To(Equal(infrav1.StateInspecting))

		By("the machine reading the host after its inspection timeout has run out")
		// provisioningHost started the inspection a minute ago.
		machine := judgeWithInspectionTimeout(inspected, 30*time.Second)
		Expect(isTerminallyFailed(machine)).To(BeFalse(),
			"the report is in, so the machine validates it instead of failing with InspectionTimedOut")
		Expect(getPhysicalHost(key).Annotations).To(HaveKeyWithValue(InspectionRequestAnnotation, "inspect-complete"))

		By("the host starting the deployment")
		deploying := reconcileInOutage(key)
		Expect(deploying.Status.State).To(Equal(infrav1.StateDeploying))
		Expect(deploying.Status.DeployingTimestamp).NotTo(BeNil())
		Expect(deploying.Annotations).NotTo(HaveKey(InspectionRequestAnnotation))

		By("the inspector finishing the deployment")
		reportProvisioned(key)
		provisioned := settlePhysicalHost(hostReconciler, key)
		Expect(provisioned.Status.State).To(Equal(infrav1.StateReady))
		Expect(conditions.GetReason(provisioned, infrav1.RedfishConnectionReadyCondition)).To(Equal(infrav1.BMCUnreachableReason),
			"the whole run went through without the BMC")
		expectProvisioned(provisionOnMachine(provisioned, gate.factory()), provisioned)
	})

	// The machine powers the host on into the inspector through its own BMC
	// connection, then signals the inspection and the credentials it minted.
	It("starts the inspection the machine asked for, and publishes the credentials the inspector presents", func() {
		annotations, token, nonce := inspectionCredentials()
		annotations[InspectionRequestAnnotation] = "inspect"
		key := inUseHost(ns.Name, "booting-host", "booting-machine", annotations)

		inspecting := reconcileInOutage(key)
		Expect(inspecting.Status.State).To(Equal(infrav1.StateInspecting),
			"the host is already booting the inspector; the inspection must not wait for the BMC")
		Expect(inspecting.Status.InspectionTimestamp).NotTo(BeNil())
		Expect(inspecting.Status.ErrorMessage).To(BeEmpty())
		Expect(inspecting.Annotations).NotTo(HaveKey(InspectionRequestAnnotation))
		Expect(verifyBootNonce(nonce, inspecting)).To(BeTrue(), "/boot accepts the nonce in the inspector's iPXE script")
		Expect(inspecting.Status.Bootstrap).NotTo(BeNil())
		Expect(auth.Verify(token, inspecting.Status.Bootstrap.TokenHash)).To(BeTrue(),
			"the callbacks accept the inspector's bearer token")

		By("clearing the credential annotations once status carries them")
		Expect(inspecting.Annotations).To(HaveKey(BootstrapTokenAnnotation), "cleared one pass after status shows the mint")
		Expect(inspecting.Annotations).To(HaveKey(BootNonceAnnotation))
		cleared := reconcileInOutage(key)
		Expect(cleared.Annotations).NotTo(HaveKey(BootstrapTokenAnnotation))
		Expect(cleared.Annotations).NotTo(HaveKey(BootNonceAnnotation))
		Expect(verifyBootNonce(nonce, cleared)).To(BeTrue())

		machine, _ := judgeHost(cleared)
		Expect(isTerminallyFailed(machine)).To(BeFalse())
		Expect(ptr.Deref(machine.Status.Phase, "")).To(Equal("Inspecting"))
		Expect(conditions.GetReason(machine, infrav1.InfrastructureReadyCondition)).To(Equal(infrav1.PhysicalHostNotReadyReason))
	})

	DescribeTable("starts the deployment the machine asks for",
		func(droppedAfterServiceRoot bool) {
			if droppedAfterServiceRoot {
				hostReconciler.RedfishClientFactory = refusingBMCAfterServiceRoot("https://mock-redfish.example.invalid:8443")
			}
			key := inspectedHost(ns.Name, "inspected-host", "inspected-machine")
			requestInspectionStep(key, "inspect-complete")

			deploying := reconcileInOutage(key)
			Expect(deploying.Status.State).To(Equal(infrav1.StateDeploying),
				"left Inspecting, the machine waits for inspect-complete until a MachineHealthCheck replaces it")
			Expect(deploying.Status.DeployingTimestamp).NotTo(BeNil())
			Expect(deploying.Annotations).NotTo(HaveKey(InspectionRequestAnnotation))

			machine, _ := judgeHost(deploying)
			Expect(isTerminallyFailed(machine)).To(BeFalse())
			Expect(ptr.Deref(machine.Status.Phase, "")).To(Equal("Provisioning"))

			By("letting the BMC answer again")
			hostReconciler.RedfishClientFactory = gate.factory()
			gate.setReachable(true)
			recovered := settlePhysicalHost(hostReconciler, key)
			Expect(recovered.Status.State).To(Equal(infrav1.StateDeploying), "the host comes back where it was")
			Expect(conditions.IsTrue(recovered, infrav1.RedfishConnectionReadyCondition)).To(BeTrue())
		},
		Entry("when the BMC refuses connections", false),
		Entry("when the BMC drops the connection after answering the service root", true),
	)

	It("records the inspection timeout the machine sends, and keeps it through the BMC's recovery", func() {
		key := provisioningHost(ns.Name, "timed-out-host", "timed-out-machine", infrav1.StateInspecting, nil)
		requestInspectionStep(key, "timeout")

		timedOut := reconcileInOutage(key)
		Expect(timedOut.Status.State).To(Equal(infrav1.StateError))
		Expect(timedOut.Status.ErrorMessage).To(Equal(inspectionTimedOutMessage), "the run's reason, not the outage's")
		Expect(timedOut.Annotations).NotTo(HaveKey(InspectionRequestAnnotation))

		machine, _ := judgeHost(timedOut)
		Expect(isTerminallyFailed(machine)).To(BeTrue())
		Expect(conditions.GetReason(machine, infrav1.InfrastructureReadyCondition)).To(Equal(infrav1.InspectionTimedOutReason))

		By("letting the BMC answer again")
		gate.setReachable(true)
		after := settlePhysicalHost(hostReconciler, key)
		Expect(after.Status.State).To(Equal(infrav1.StateError), "only a release ends the run's Error")
		Expect(after.Status.ErrorMessage).To(Equal(inspectionTimedOutMessage))
	})

	// A failure that needs a fix writes its own Error over the host's state in
	// the pass that finds it. A request applied in that pass would go with that
	// state, and the host would come back InUse once the connection worked, where
	// its machine boots the inspector again. Left in place, the request is
	// applied in the pass that connects.
	It("leaves the machine's requests in place while a BMC failure that needs a fix holds the connection", func() {
		gate.setReachable(true)
		key := inspectedHost(ns.Name, "held-request-host", "held-request-machine")
		requestInspectionStep(key, "inspect-complete")
		Expect(k8sClient.Delete(ctx, bmcCredentialsSecret(ns.Name))).To(Succeed())

		_, err := hostReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).To(HaveOccurred(), "a missing credentials Secret keeps the workqueue's backoff")
		failing := getPhysicalHost(key)
		Expect(failing.Status.State).To(Equal(infrav1.StateError))
		Expect(conditions.GetReason(failing, infrav1.RedfishConnectionReadyCondition)).To(Equal(infrav1.MissingCredentialsReason))
		Expect(failing.Annotations).To(HaveKeyWithValue(InspectionRequestAnnotation, "inspect-complete"))

		By("restoring the credentials")
		Expect(k8sClient.Create(ctx, bmcCredentialsSecret(ns.Name))).To(Succeed())
		resumed := settlePhysicalHost(hostReconciler, key)
		Expect(resumed.Status.State).To(Equal(infrav1.StateDeploying), "not InUse, where the machine would boot the inspector again")
		Expect(resumed.Annotations).NotTo(HaveKey(InspectionRequestAnnotation))
	})
})
