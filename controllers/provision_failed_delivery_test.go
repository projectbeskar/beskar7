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
	"net/url"
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

// The inspector's /provision-failed report used to be lost before the host
// applied it (run_failure_test.go covers what happens after):
//
//   - The host applied it only after a successful BMC connection. A failure
//     that needs a fix — the credentials Secret gone, a certificate the BMC
//     changed after a firmware reset — wrote its own Error over Deploying first,
//     and once the BMC answered again the host dropped the report as a
//     duplicate and went back to InUse. The handler dropped a report that
//     arrived while the host sat in that Error. During an outage the report
//     waited, and the machine could time out first.
//   - The handler took a report only from a Deploying host. The inspector
//     starts Phase 2 as soon as it has posted its inspection report, and a fast
//     failure (a target image URL that answers 404) reports before the
//     Beskar7Machine has validated that report and the host is Deploying. The
//     machine then waited out the deployment timeout and failed with
//     DeploymentTimedOut instead of DeploymentFailed.

// settlePhysicalHost reconciles a host whose BMC answers until a pass leaves
// it unchanged, and returns the host as persisted.
func settlePhysicalHost(r *PhysicalHostReconciler, key client.ObjectKey) *infrav1.PhysicalHost {
	for range 5 {
		before := getPhysicalHost(key).ResourceVersion
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		if after := getPhysicalHost(key); after.ResourceVersion == before {
			return after
		}
	}
	Fail("the PhysicalHost did not settle within 5 reconciles")
	return nil
}

// editRedfishConnection edits a host's RedfishConnection and returns it as it was.
func editRedfishConnection(key client.ObjectKey, edit func(*infrav1.RedfishConnection)) infrav1.RedfishConnection {
	host := getPhysicalHost(key)
	before := host.Spec.RedfishConnection
	edited := host.DeepCopy()
	edit(&edited.Spec.RedfishConnection)
	Expect(k8sClient.Patch(ctx, edited, client.MergeFrom(host))).To(Succeed())
	return before
}

// reportDeployFailure posts a deploy failure the way the /provision-failed
// handler does once the bearer token has been checked.
func reportDeployFailure(key client.ObjectKey, report string) {
	handler := &ProvisionFailedHandler{Client: k8sClient, Log: ctrl.Log.WithName("report-delivery-handler")}
	Expect(handler.signalProvisionFailed(ctx, handler.Log, key.Namespace, key.Name, report)).To(Succeed())
}

// postInspectionReport posts an 8-core inspection report the way the
// inspection handler does.
func postInspectionReport(key client.ObjectKey) {
	handler := &InspectionHandler{Client: k8sClient, Log: ctrl.Log.WithName("report-delivery-inspection")}
	Expect(handler.processInspectionReport(ctx, handler.Log, key.Namespace, key.Name, InspectionReportRequest{
		Manufacturer: "Acme", Model: "Fast-1000", CPUs: []CPUData{{ID: "cpu0", Cores: 8}},
	})).To(Succeed())
}

// validateOnMachine hands a host that has read its inspection report to the
// Beskar7Machine holding it, which validates the report and, if the hardware
// fits, asks the host to start deploying.
func validateOnMachine(host *infrav1.PhysicalHost, reqs *infrav1.HardwareRequirements) *infrav1.Beskar7Machine {
	machine := &infrav1.Beskar7Machine{
		ObjectMeta: metav1.ObjectMeta{Name: host.Spec.ConsumerRef.Name, Namespace: host.Namespace},
		Spec:       infrav1.Beskar7MachineSpec{HardwareRequirements: reqs},
	}
	r := &Beskar7MachineReconciler{
		Client: k8sClient, Scheme: k8sClient.Scheme(),
		Log: ctrl.Log.WithName("report-delivery-machine"),
	}
	_, err := r.handlePhysicalHostState(ctx, r.Log, machine, host)
	Expect(err).NotTo(HaveOccurred())
	return machine
}

// requestInspectionStep sets the inspection-request annotation the way the
// Beskar7Machine does.
func requestInspectionStep(key client.ObjectKey, value string) {
	host := getPhysicalHost(key)
	requested := host.DeepCopy()
	if requested.Annotations == nil {
		requested.Annotations = map[string]string{}
	}
	requested.Annotations[InspectionRequestAnnotation] = value
	Expect(k8sClient.Patch(ctx, requested, client.MergeFrom(host))).To(Succeed())
}

var _ = Describe("The inspector's /provision-failed report when the host's BMC fails", func() {
	const (
		bmcAddress    = "https://mock-redfish.example.invalid:8443"
		retryInterval = 2 * time.Second
	)

	var ns *corev1.Namespace

	BeforeEach(func() {
		ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "report-bmc-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		Expect(k8sClient.Create(ctx, bmcCredentialsSecret(ns.Name))).To(Succeed())
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, ns)).To(Succeed())
	})

	newHostReconciler := func() *PhysicalHostReconciler {
		return &PhysicalHostReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			Log:                    ctrl.Log.WithName("report-bmc-host"),
			Recorder:               record.NewFakeRecorder(10),
			RedfishClientFactory:   reachableBMC(),
			TransientRetryInterval: retryInterval,
		}
	}

	reconcileHost := func(r *PhysicalHostReconciler, key client.ObjectKey) (ctrl.Result, error) {
		return r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	}

	// bmcFault is one way the host's connection to its BMC fails: a BMC that
	// fails (factory), a credentials Secret that goes away, or a spec edit
	// (breakConnection). requeueAfter is what the reconcile returns when it
	// hands the workqueue no error.
	type bmcFault struct {
		factory         internalredfish.RedfishClientFactory
		dropCredentials bool
		breakConnection func(*infrav1.RedfishConnection)
		hostReason      string
		requeueAfter    time.Duration
	}

	// breakBMC makes the host's connection fail the way fault says, and returns
	// the function that puts it back.
	breakBMC := func(r *PhysicalHostReconciler, key client.ObjectKey, fault bmcFault) func() {
		if fault.factory != nil {
			r.RedfishClientFactory = fault.factory
		}
		if fault.dropCredentials {
			Expect(k8sClient.Delete(ctx, bmcCredentialsSecret(ns.Name))).To(Succeed())
		}
		var healthy infrav1.RedfishConnection
		if fault.breakConnection != nil {
			healthy = editRedfishConnection(key, fault.breakConnection)
		}
		return func() {
			if fault.dropCredentials {
				Expect(k8sClient.Create(ctx, bmcCredentialsSecret(ns.Name))).To(Succeed())
			}
			if fault.breakConnection != nil {
				editRedfishConnection(key, func(c *infrav1.RedfishConnection) { *c = healthy })
			}
			r.RedfishClientFactory = reachableBMC()
		}
	}

	expectFailedPass := func(fault bmcFault, result ctrl.Result, err error) {
		if fault.requeueAfter > 0 {
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(fault.requeueAfter), "the failure keeps its own retry cadence")
		} else {
			Expect(err).To(HaveOccurred(), "a failure that needs a change keeps the workqueue's backoff")
		}
	}

	missingCredentials := bmcFault{
		factory:         failingBMC(errors.New("the factory must not be reached")),
		dropCredentials: true,
		hostReason:      infrav1.MissingCredentialsReason,
	}
	rejectedCredentials := bmcFault{
		factory:    failingBMC(common.ConstructError(401, []byte("unauthorized"))),
		hostReason: infrav1.RedfishConnectionFailedReason,
	}

	DescribeTable("a report the host has not applied yet when its BMC fails",
		func(fault bmcFault) {
			report := sanitizeFailureReason("image fetch failed: 404 Not Found")
			key := provisioningHost(ns.Name, "pending-report-host", "pending-report-machine", infrav1.StateDeploying,
				map[string]string{ProvisionFailedRequestAnnotation: report})
			hostReconciler := newHostReconciler()
			restore := breakBMC(hostReconciler, key, fault)

			By("reconciling while the connection fails")
			result, err := reconcileHost(hostReconciler, key)
			expectFailedPass(fault, result, err)

			during := getPhysicalHost(key)
			Expect(during.Annotations).NotTo(HaveKey(ProvisionFailedRequestAnnotation),
				"the report is applied in the pass that cannot reach the BMC")
			Expect(during.Status.State).To(Equal(infrav1.StateError))
			Expect(during.Status.ErrorMessage).To(Equal(report), "the inspector's reason, not the BMC's")
			Expect(conditions.IsFalse(during, infrav1.RedfishConnectionReadyCondition)).To(BeTrue())
			Expect(conditions.GetReason(during, infrav1.RedfishConnectionReadyCondition)).To(Equal(fault.hostReason),
				"the condition reports the BMC")

			By("handing the host to its machine while the connection is down")
			machine, _ := judgeHost(during)
			Expect(isTerminallyFailed(machine)).To(BeTrue())
			Expect(conditions.GetReason(machine, infrav1.InfrastructureReadyCondition)).To(Equal(infrav1.DeploymentFailedReason),
				"the machine fails with the inspector's reason, not PhysicalHostError or a wait for the BMC")

			By("letting the BMC answer again")
			restore()
			_, err = reconcileHost(hostReconciler, key)
			Expect(err).NotTo(HaveOccurred())
			after := getPhysicalHost(key)
			Expect(conditions.IsTrue(after, infrav1.RedfishConnectionReadyCondition)).To(BeTrue())
			Expect(after.Status.State).To(Equal(infrav1.StateError),
				"the host must not go back to InUse, where its machine would boot the inspector again")
			Expect(after.Status.ErrorMessage).To(Equal(report))
		},
		Entry("the credentials Secret is deleted", missingCredentials),
		Entry("the BMC rejects the credentials", rejectedCredentials),
		Entry("the BMC presents a certificate the client rejects", bmcFault{
			factory:    failingBMC(&url.Error{Op: "Get", URL: bmcAddress + "/redfish/v1/", Err: x509.UnknownAuthorityError{}}),
			hostReason: infrav1.RedfishConnectionFailedReason,
		}),
		Entry("the BMC has no ComputerSystem", bmcFault{
			factory: func(_ context.Context, _, _, _ string, _ bool, _ []byte) (internalredfish.Client, error) {
				empty := internalredfish.NewMockClient()
				empty.ShouldFail["GetSystemInfo"] = errors.New("no systems found")
				return empty, nil
			},
			hostReason: infrav1.RedfishQueryFailedReason,
		}),
		Entry("insecureSkipVerify is combined with a CA bundle", bmcFault{
			breakConnection: func(c *infrav1.RedfishConnection) {
				c.InsecureSkipVerify = ptr.To(true)
				c.CABundleSecretRef = "bmc-ca"
			},
			hostReason:   infrav1.InsecureCABundleConflictReason,
			requeueAfter: 5 * time.Minute,
		}),
		Entry("the CA bundle Secret does not exist", bmcFault{
			breakConnection: func(c *infrav1.RedfishConnection) { c.CABundleSecretRef = "bmc-ca" },
			hostReason:      infrav1.CABundleFetchFailedReason,
		}),
		// An outage keeps Deploying (bmc_outage_test.go), so nothing was lost here
		// before; the report waited for the BMC, and an outage longer than what was
		// left of the deployment timeout failed the machine with DeploymentTimedOut.
		Entry("the BMC refuses connections", bmcFault{
			factory:    failingBMC(refusedConnection(bmcAddress)),
			hostReason: infrav1.BMCUnreachableReason, requeueAfter: retryInterval,
		}),
	)

	// The machine that reads the BMC's Error has usually failed already, with
	// PhysicalHostError: a failure that needs a fix is terminal (#190). This is
	// about the host: it records the inspector's reason, keeps the run's Error
	// once the BMC answers, and a machine that missed the BMC's Error fails with
	// DeploymentFailed instead of booting the inspector again.
	DescribeTable("a report posted while the host is in an Error its BMC wrote over the deployment",
		func(fault bmcFault) {
			key := provisioningHost(ns.Name, "bmc-error-host", "bmc-error-machine", infrav1.StateDeploying, nil)
			hostReconciler := newHostReconciler()
			restore := breakBMC(hostReconciler, key, fault)
			result, err := reconcileHost(hostReconciler, key)
			expectFailedPass(fault, result, err)
			errored := getPhysicalHost(key)
			Expect(errored.Status.State).To(Equal(infrav1.StateError))
			Expect(errored.Status.ErrorMessage).NotTo(HavePrefix(provisionFailedReasonPrefix))
			Expect(errored.Status.DeployingTimestamp).NotTo(BeNil())

			By("the inspector reporting its deploy failure now")
			report := sanitizeFailureReason("COS_OEM partition not found")
			reportDeployFailure(key, report)
			Expect(getPhysicalHost(key).Annotations).To(HaveKeyWithValue(ProvisionFailedRequestAnnotation, report),
				"the handler takes the report: the BMC failure says nothing about the deployment")

			By("reconciling while the connection still fails")
			result, err = reconcileHost(hostReconciler, key)
			expectFailedPass(fault, result, err)
			failed := getPhysicalHost(key)
			Expect(failed.Annotations).NotTo(HaveKey(ProvisionFailedRequestAnnotation))
			Expect(failed.Status.State).To(Equal(infrav1.StateError))
			Expect(failed.Status.ErrorMessage).To(Equal(report))
			Expect(conditions.GetReason(failed, infrav1.RedfishConnectionReadyCondition)).To(Equal(fault.hostReason))

			machine, _ := judgeHost(failed)
			Expect(isTerminallyFailed(machine)).To(BeTrue())
			Expect(conditions.GetReason(machine, infrav1.InfrastructureReadyCondition)).To(Equal(infrav1.DeploymentFailedReason))

			By("letting the BMC answer again")
			restore()
			_, err = reconcileHost(hostReconciler, key)
			Expect(err).NotTo(HaveOccurred())
			after := getPhysicalHost(key)
			Expect(after.Status.State).To(Equal(infrav1.StateError), "only a release ends the run's Error")
			Expect(after.Status.ErrorMessage).To(Equal(report))
		},
		Entry("the credentials Secret is deleted", missingCredentials),
		Entry("the BMC rejects the credentials", rejectedCredentials),
	)

	It("takes no report for a host whose BMC failed before it was deploying", func() {
		key := provisioningHost(ns.Name, "inspecting-bmc-error-host", "inspecting-bmc-error-machine", infrav1.StateInspecting, nil)
		hostReconciler := newHostReconciler()
		restore := breakBMC(hostReconciler, key, rejectedCredentials)
		_, err := reconcileHost(hostReconciler, key)
		Expect(err).To(HaveOccurred())
		Expect(getPhysicalHost(key).Status.State).To(Equal(infrav1.StateError))

		By("a deploy failure arriving for it")
		reportDeployFailure(key, sanitizeFailureReason("image fetch failed"))
		Expect(getPhysicalHost(key).Annotations).NotTo(HaveKey(ProvisionFailedRequestAnnotation),
			"no deployment was running for the report to be about")

		By("letting the BMC answer again")
		restore()
		recovered := settlePhysicalHost(hostReconciler, key)
		Expect(recovered.Status.State).To(Equal(infrav1.StateInUse), "an Error about the BMC clears as before")
		Expect(recovered.Status.ErrorMessage).To(BeEmpty())
	})
})

var _ = Describe("The inspector's /provision-failed report when it arrives before the host is Deploying", func() {
	var (
		ns             *corev1.Namespace
		hostReconciler *PhysicalHostReconciler
	)

	BeforeEach(func() {
		ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "report-early-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		Expect(k8sClient.Create(ctx, bmcCredentialsSecret(ns.Name))).To(Succeed())
		hostReconciler = &PhysicalHostReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			Log:                  ctrl.Log.WithName("report-early-host"),
			Recorder:             record.NewFakeRecorder(10),
			RedfishClientFactory: reachableBMC(),
		}
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, ns)).To(Succeed())
	})

	report := sanitizeFailureReason("image fetch failed: 404 Not Found")

	DescribeTable("a fast deploy failure is kept until the machine has validated the inspection report",
		func(readFirst bool) {
			key := provisioningHost(ns.Name, "fast-failure-host", "fast-failure-machine", infrav1.StateInspecting, nil)

			By("the inspector posting its inspection report and, straight after, its deploy failure")
			postInspectionReport(key)
			if readFirst {
				settlePhysicalHost(hostReconciler, key)
			}
			reportDeployFailure(key, report)
			Expect(getPhysicalHost(key).Annotations).To(HaveKeyWithValue(ProvisionFailedRequestAnnotation, report),
				"the handler takes a report from a host that is still Inspecting")

			By("reconciling the host: it reads the inspection report and keeps the deploy failure")
			inspected := settlePhysicalHost(hostReconciler, key)
			Expect(inspected.Status.State).To(Equal(infrav1.StateInspecting))
			Expect(inspected.Status.InspectionPhase).To(Equal(infrav1.InspectionPhaseComplete))
			Expect(inspected.Status.InspectionReport).NotTo(BeNil())
			Expect(inspected.Status.ErrorMessage).To(BeEmpty(), "the machine has not validated the inspection report yet")
			Expect(inspected.Annotations).To(HaveKeyWithValue(ProvisionFailedRequestAnnotation, report))

			By("the machine validating the inspection report")
			Expect(isTerminallyFailed(validateOnMachine(inspected, nil))).To(BeFalse())
			Expect(getPhysicalHost(key).Annotations).To(HaveKeyWithValue(InspectionRequestAnnotation, "inspect-complete"))

			By("reconciling the host: it starts deploying and applies the report")
			failed := settlePhysicalHost(hostReconciler, key)
			Expect(failed.Annotations).NotTo(HaveKey(ProvisionFailedRequestAnnotation))
			Expect(failed.Annotations).NotTo(HaveKey(InspectionRequestAnnotation))
			Expect(failed.Status.State).To(Equal(infrav1.StateError))
			Expect(failed.Status.ErrorMessage).To(Equal(report))
			Expect(failed.Status.DeployingTimestamp).NotTo(BeNil())

			machine, result := judgeHost(failed)
			Expect(result).To(Equal(ctrl.Result{}))
			Expect(isTerminallyFailed(machine)).To(BeTrue())
			cond := conditions.Get(machine, infrav1.InfrastructureReadyCondition)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Reason).To(Equal(infrav1.DeploymentFailedReason), "not DeploymentTimedOut, 20 minutes later")
			Expect(cond.Message).To(ContainSubstring("image fetch failed: 404 Not Found"))
		},
		Entry("when it arrives before the host has read the inspection report", false),
		Entry("when it arrives after the host has read the inspection report", true),
	)

	// The machine asks for inspect-complete whenever it reads the host as
	// Inspecting with a report in, and it can read a copy of the host from
	// before its first request was applied.
	It("applies the kept report even when the machine asks again for the deployment to start", func() {
		key := provisioningHost(ns.Name, "asked-twice-host", "asked-twice-machine", infrav1.StateInspecting, nil)
		postInspectionReport(key)
		reportDeployFailure(key, report)
		validateOnMachine(settlePhysicalHost(hostReconciler, key), nil)

		By("the host applying the first request")
		_, err := hostReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(getPhysicalHost(key).Annotations).NotTo(HaveKey(InspectionRequestAnnotation))

		By("the machine sending it again")
		requestInspectionStep(key, "inspect-complete")
		failed := settlePhysicalHost(hostReconciler, key)
		Expect(failed.Annotations).NotTo(HaveKey(InspectionRequestAnnotation))
		Expect(failed.Annotations).NotTo(HaveKey(ProvisionFailedRequestAnnotation))
		Expect(failed.Status.State).To(Equal(infrav1.StateError))
		Expect(failed.Status.ErrorMessage).To(Equal(report))
	})

	It("never applies a kept report when the machine rejects the hardware, and drops it when the host is released", func() {
		key := provisioningHost(ns.Name, "rejected-host", "rejected-machine", infrav1.StateInspecting, nil)
		postInspectionReport(key)
		reportDeployFailure(key, report)
		inspected := settlePhysicalHost(hostReconciler, key)
		Expect(inspected.Annotations).To(HaveKeyWithValue(ProvisionFailedRequestAnnotation, report))

		By("the machine rejecting the hardware")
		machine := validateOnMachine(inspected, &infrav1.HardwareRequirements{MinCPUCores: 16})
		Expect(isTerminallyFailed(machine)).To(BeTrue())
		Expect(conditions.GetReason(machine, infrav1.InfrastructureReadyCondition)).To(Equal(infrav1.HardwareRequirementsNotMetReason))
		Expect(getPhysicalHost(key).Annotations).NotTo(HaveKey(InspectionRequestAnnotation))

		By("reconciling the host: the report stays unapplied")
		still := settlePhysicalHost(hostReconciler, key)
		Expect(still.Status.State).To(Equal(infrav1.StateInspecting))
		Expect(still.Status.ErrorMessage).To(BeEmpty())

		By("releasing the host")
		releasePhysicalHost(key)
		released := settlePhysicalHost(hostReconciler, key)
		Expect(released.Status.State).To(Equal(infrav1.StateAvailable))
		Expect(released.Annotations).NotTo(HaveKey(ProvisionFailedRequestAnnotation),
			"the report was about the claim that ended")
		Expect(released.Status.ErrorMessage).To(BeEmpty())
		Expect(released.Status.InspectionPhase).To(BeEmpty())
		Expect(conditions.IsFalse(released, infrav1.HostInspectedCondition)).To(BeTrue())

		By("the host being claimed again")
		claimed := released.DeepCopy()
		claimed.Spec.ConsumerRef = &corev1.ObjectReference{
			Kind: "Beskar7Machine", Name: "next-machine", Namespace: ns.Name,
			APIVersion: infrav1.GroupVersion.String(),
		}
		Expect(k8sClient.Patch(ctx, claimed, client.MergeFrom(released))).To(Succeed())
		reclaimed := settlePhysicalHost(hostReconciler, key)
		Expect(reclaimed.Status.State).To(Equal(infrav1.StateInUse))
		Expect(reclaimed.Annotations).NotTo(HaveKey(ProvisionFailedRequestAnnotation))
	})

	// A host claimed again and waiting for its new inspector cannot have a
	// deployment for such a report to be about. This is what an inspector left
	// running by the previous claim sends when the release's power-off did not
	// stop it, with the bearer token the new claim went on to reuse.
	It("drops a deploy failure that arrives before the host's current run has an inspection report", func() {
		key := provisioningHost(ns.Name, "early-report-host", "early-report-machine", infrav1.StateInspecting, nil)
		reportDeployFailure(key, report)
		Expect(getPhysicalHost(key).Annotations).To(HaveKey(ProvisionFailedRequestAnnotation),
			"the handler takes it: its copy of the host may predate the inspection report")

		dropped := settlePhysicalHost(hostReconciler, key)
		Expect(dropped.Annotations).NotTo(HaveKey(ProvisionFailedRequestAnnotation))
		Expect(dropped.Status.State).To(Equal(infrav1.StateInspecting))
		Expect(dropped.Status.ErrorMessage).To(BeEmpty())

		By("this run going on to inspect and deploy")
		postInspectionReport(key)
		validateOnMachine(settlePhysicalHost(hostReconciler, key), nil)
		deploying := settlePhysicalHost(hostReconciler, key)
		Expect(deploying.Status.State).To(Equal(infrav1.StateDeploying), "the dropped report does not come back")
		Expect(deploying.Status.ErrorMessage).To(BeEmpty())
	})

	// A failed run's Error ends only at release, so no inspection request may
	// start another step of the run on it.
	DescribeTable("an inspection request that reaches a host whose deployment the inspector reported failed",
		func(value string) {
			key := provisioningHost(ns.Name, "failed-run-host", "failed-run-machine", infrav1.StateDeploying,
				map[string]string{ProvisionFailedRequestAnnotation: report})
			failed := settlePhysicalHost(hostReconciler, key)
			Expect(failed.Status.State).To(Equal(infrav1.StateError))

			requestInspectionStep(key, value)
			after := settlePhysicalHost(hostReconciler, key)
			Expect(after.Annotations).NotTo(HaveKey(InspectionRequestAnnotation), "the request is consumed")
			Expect(after.Status.State).To(Equal(infrav1.StateError))
			Expect(after.Status.ErrorMessage).To(Equal(report))
			Expect(after.Status.InspectionPhase).To(Equal(failed.Status.InspectionPhase))

			machine, _ := judgeHost(after)
			Expect(conditions.GetReason(machine, infrav1.InfrastructureReadyCondition)).To(Equal(infrav1.DeploymentFailedReason))
		},
		Entry("inspect-complete", "inspect-complete"),
		Entry("inspect", "inspect"),
		Entry("timeout", "timeout"),
	)
})

// The specs above call the reconcilers by hand. This one runs both under a
// manager, with the inspector's two reports already in when they start: the
// host is still Inspecting, and only the controllers can take it further.
var _ = Describe("A fast deploy failure with both controllers under a running manager", func() {
	It("fails the machine with DeploymentFailed, not DeploymentTimedOut, without touching the BMC", func() {
		testNs := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "report-early-mgr-"}}
		Expect(k8sClient.Create(ctx, testNs)).To(Succeed())
		defer func() { Expect(k8sClient.Delete(ctx, testNs)).To(Succeed()) }()
		ns := testNs.Name
		const clusterName = "fast-failure-cluster"

		Expect(k8sClient.Create(ctx, &clusterv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns},
			Spec:       clusterv1.ClusterSpec{Paused: ptr.To(false)},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "fast-failure-bootstrap", Namespace: ns},
			Data:       map[string][]byte{"value": []byte("#cloud-config\n")},
		})).To(Succeed())
		machine := &clusterv1.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name: "fast-failure", Namespace: ns,
				Labels: map[string]string{clusterv1.ClusterNameLabel: clusterName},
			},
			Spec: clusterv1.MachineSpec{
				ClusterName: clusterName,
				InfrastructureRef: clusterv1.ContractVersionedObjectReference{
					APIGroup: infrav1.GroupVersion.Group, Kind: "Beskar7Machine", Name: "fast-failure",
				},
				Bootstrap: clusterv1.Bootstrap{DataSecretName: ptr.To("fast-failure-bootstrap")},
			},
		}
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())
		Expect(k8sClient.Create(ctx, bmcCredentialsSecret(ns))).To(Succeed())
		b7m := &infrav1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name: "fast-failure", Namespace: ns,
				Labels: map[string]string{clusterv1.ClusterNameLabel: clusterName},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: clusterv1.GroupVersion.String(), Kind: "Machine",
					Name: machine.Name, UID: machine.UID,
				}},
			},
			Spec: infrav1.Beskar7MachineSpec{
				InspectionImageURL: "http://boot/inspect.ipxe",
				TargetImageURL:     "http://boot/missing.raw",
				TargetImageDigest:  bootTestDigest,
			},
		}
		Expect(k8sClient.Create(ctx, b7m)).To(Succeed())
		machineKey := client.ObjectKeyFromObject(b7m)
		hostKey := provisioningHost(ns, "fast-failure-host", b7m.Name, infrav1.StateInspecting, nil)

		By("the inspector posting its inspection report, then failing to fetch the target image")
		postInspectionReport(hostKey)
		report := sanitizeFailureReason("image fetch failed: 404 Not Found")
		reportDeployFailure(hostKey, report)

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
			Log:                  ctrl.Log.WithName("report-early-mgr-host"),
			Recorder:             record.NewFakeRecorder(100),
			RedfishClientFactory: reachableBMC(),
		}).SetupWithManager(mgr)).To(Succeed())
		// The host is Inspecting from the start, so the machine never has a reason
		// to reach for the BMC.
		var machineBMCCalls atomic.Int32
		Expect((&Beskar7MachineReconciler{
			Client: mgr.GetClient(),
			Scheme: mgr.GetScheme(),
			Log:    ctrl.Log.WithName("report-early-mgr-machine"),
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

		By("waiting for the machine to fail with the inspector's reason")
		Eventually(func(g Gomega) {
			got := &infrav1.Beskar7Machine{}
			g.Expect(k8sClient.Get(ctx, machineKey, got)).To(Succeed())
			g.Expect(isTerminallyFailed(got)).To(BeTrue())
			cond := conditions.Get(got, infrav1.InfrastructureReadyCondition)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Reason).To(Equal(infrav1.DeploymentFailedReason))
			g.Expect(cond.Message).To(ContainSubstring("image fetch failed: 404 Not Found"))
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())

		By("checking the host stays in Error and nothing reaches for the BMC")
		Consistently(func(g Gomega) {
			h := &infrav1.PhysicalHost{}
			g.Expect(k8sClient.Get(ctx, hostKey, h)).To(Succeed())
			g.Expect(h.Status.State).To(Equal(infrav1.StateError))
			g.Expect(h.Status.ErrorMessage).To(Equal(report))
			g.Expect(h.Status.DeployingTimestamp).NotTo(BeNil(), "the machine validated the inspection report first")
			g.Expect(machineBMCCalls.Load()).To(BeZero(), "the machine ran triggerInspection again")
		}, 3*time.Second, 200*time.Millisecond).Should(Succeed())
	})
})
