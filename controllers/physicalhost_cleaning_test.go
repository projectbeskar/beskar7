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
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	dto "github.com/prometheus/client_model/go"
	"github.com/stmcginnis/gofish/schemas"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/beskar7/beskar7/api/v1beta2"
	"github.com/beskar7/beskar7/internal/auth"
	internalmetrics "github.com/beskar7/beskar7/internal/metrics"
	internalredfish "github.com/beskar7/beskar7/internal/redfish"
)

const fixtureCleaningImageURL = "https://images.example.invalid/inspector"

// fixedBMC returns a factory that always hands out mock, so a spec can read
// what the controller did to the BMC.
func fixedBMC(mock *internalredfish.MockClient) internalredfish.RedfishClientFactory {
	return func(_ context.Context, _, _, _ string, _ bool, _ []byte) (internalredfish.Client, error) {
		return mock, nil
	}
}

// bindCleaningReport sets a cleaning report annotation on the host, bound to
// its cleaning credentials as the cleaning handlers bind it.
func bindCleaningReport(key client.ObjectKey, annotation, value string) {
	current := getPhysicalHost(key)
	binder, err := newCallbackBinder(current, readBootstrapCredentials(getCredentialSecret(key)))
	Expect(err).NotTo(HaveOccurred())
	reported := current.DeepCopy()
	binder.setAnnotation(reported, annotation, value, "")
	Expect(k8sClient.Patch(ctx, reported, client.MergeFrom(current))).To(Succeed())
}

var _ = Describe("Cleaning a released PhysicalHost (spec.cleaning.mode Metadata)", func() {
	var (
		ns  *corev1.Namespace
		bmc *internalredfish.MockClient
	)

	BeforeEach(func() {
		ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "cleaning-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		Expect(k8sClient.Create(ctx, bmcCredentialsSecret(ns.Name))).To(Succeed())
		bmc = internalredfish.NewMockClient()
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, ns)).To(Succeed())
	})

	newReconciler := func() *PhysicalHostReconciler {
		return &PhysicalHostReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			Log:                  ctrl.Log.WithName("cleaning-host"),
			Recorder:             events.NewFakeRecorder(100),
			RedfishClientFactory: fixedBMC(bmc),
			CleaningImageURL:     fixtureCleaningImageURL,
			CleaningTimeout:      time.Minute,
		}
	}

	reconcile := func(r *PhysicalHostReconciler, key client.ObjectKey) ctrl.Result {
		result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		return result
	}

	// releasedHost creates a host that served a claim with the given cleaning
	// mode, claim credentials included, and releases it as Beskar7Machine
	// deletion does.
	releasedHost := func(name string, mode infrav1.CleaningMode) client.ObjectKey {
		key := provisioningHost(ns.Name, name, name+"-machine", infrav1.StateReady, nil)
		current := getPhysicalHost(key)
		withMode := current.DeepCopy()
		withMode.Spec.Cleaning = &infrav1.CleaningSpec{Mode: mode}
		Expect(k8sClient.Patch(ctx, withMode, client.MergeFrom(current))).To(Succeed())
		ensureCallbackCredentials(key)
		releasePhysicalHost(key)
		return key
	}

	// bootedHost is a released host taken through the pass that boots its
	// cleaning run.
	bootedHost := func(r *PhysicalHostReconciler, name string) client.ObjectKey {
		key := releasedHost(name, infrav1.CleaningModeMetadata)
		reconcile(r, key)
		Expect(getPhysicalHost(key).CleaningStartedAt()).NotTo(BeNil())
		return key
	}

	It("boots the inspector in clean mode instead of making the host Available", func() {
		r := newReconciler()
		key := releasedHost("clean-boot", infrav1.CleaningModeMetadata)
		claimToken := callbackTokenOf(key)

		result := reconcile(r, key)

		host := getPhysicalHost(key)
		Expect(host.Status.State).To(Equal(infrav1.StateCleaning))
		Expect(host.Status.Ready).To(BeFalse())
		Expect(host.CleaningStartedAt()).NotTo(BeNil())
		Expect(conditions.GetReason(host, infrav1.HostCleanedCondition)).To(Equal(infrav1.CleaningInProgressReason))
		Expect(conditions.GetReason(host, infrav1.HostAvailableCondition)).To(Equal(infrav1.HostCleaningReason))
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))

		Expect(bmc.SetBootSourcePXECalled).To(BeTrue())
		Expect(bmc.PowerState).To(Equal(schemas.OnPowerState))

		creds := readBootstrapCredentials(getCredentialSecret(key))
		Expect(creds.forCleaning()).To(BeTrue())
		Expect(creds.consumer).To(BeEmpty())
		Expect(creds.cleaningImage).To(Equal(fixtureCleaningImageURL))
		Expect(creds.token).NotTo(Equal(claimToken), "the released claim's token must not survive into the cleaning run")
		Expect(creds.tokenExpiresAt).To(BeTemporally("~", time.Now().Add(auth.CleaningTokenLifetime), time.Minute))
		Expect(creds.nonceExpiresAt).To(BeTemporally("~", time.Now().Add(r.CleaningTimeout), 10*time.Second))
		Expect(host.Status.Bootstrap.TokenHash).To(Equal(auth.Hash(creds.token)))
	})

	It("restarts a host that is still on, so the PXE override takes effect", func() {
		bmc.PowerState = schemas.OnPowerState
		bootedHost(newReconciler(), "clean-restart")
		Expect(bmc.ResetCalled).To(BeTrue())
	})

	It("makes the host Available after a bound /cleaned report, powered off and without credentials", func() {
		r := newReconciler()
		key := bootedHost(r, "clean-done")
		bindCleaningReport(key, CleanedRequestAnnotation, cleanedRequestValue)

		reconcile(r, key)

		host := getPhysicalHost(key)
		Expect(host.Status.State).To(Equal(infrav1.StateAvailable))
		Expect(host.Status.Ready).To(BeTrue())
		Expect(host.CleaningStartedAt()).To(BeNil())
		Expect(conditions.IsTrue(host, infrav1.HostCleanedCondition)).To(BeTrue())
		Expect(conditions.IsTrue(host, infrav1.HostAvailableCondition)).To(BeTrue())
		Expect(host.Annotations).NotTo(HaveKey(CleanedRequestAnnotation))
		Expect(bmc.PowerState).To(Equal(schemas.OffPowerState))
		Expect(bmc.ClearBootSourceOverrideCalled).To(BeTrue())

		creds := readBootstrapCredentials(getCredentialSecret(key))
		Expect(creds.token).To(BeEmpty())
		Expect(creds.nonce).To(BeEmpty())
		Expect(creds.purpose).To(BeEmpty())
		Expect(host.Status.Bootstrap.TokenHash).To(BeEmpty())
	})

	It("removes HostCleaned when the wiped host is claimed again", func() {
		r := newReconciler()
		key := bootedHost(r, "clean-reclaim")
		bindCleaningReport(key, CleanedRequestAnnotation, cleanedRequestValue)
		reconcile(r, key)
		Expect(conditions.IsTrue(getPhysicalHost(key), infrav1.HostCleanedCondition)).To(BeTrue())

		current := getPhysicalHost(key)
		claimed := current.DeepCopy()
		claimed.Spec.ConsumerRef = claimedPhysicalHost(ns.Name, key.Name, "next-machine").Spec.ConsumerRef
		Expect(k8sClient.Patch(ctx, claimed, client.MergeFrom(current))).To(Succeed())
		reconcile(r, key)

		Expect(conditions.Has(getPhysicalHost(key), infrav1.HostCleanedCondition)).To(BeFalse())
	})

	It("ignores a /cleaned report bound to the released claim's credentials", func() {
		r := newReconciler()
		key := releasedHost("clean-forged", infrav1.CleaningModeMetadata)
		claimCreds := readBootstrapCredentials(getCredentialSecret(key))
		reconcile(r, key)

		current := getPhysicalHost(key)
		binder, err := newCallbackBinder(current, claimCreds)
		Expect(err).NotTo(HaveOccurred())
		forged := current.DeepCopy()
		binder.setAnnotation(forged, CleanedRequestAnnotation, cleanedRequestValue, "")
		Expect(k8sClient.Patch(ctx, forged, client.MergeFrom(current))).To(Succeed())

		reconcile(r, key)

		host := getPhysicalHost(key)
		Expect(host.Status.State).To(Equal(infrav1.StateCleaning))
		Expect(conditions.IsTrue(host, infrav1.HostCleanedCondition)).To(BeFalse())
		Expect(host.Annotations).NotTo(HaveKey(CleanedRequestAnnotation))
	})

	// failRun reports /clean-failed for a booted host and applies it.
	failRun := func(r *PhysicalHostReconciler, key client.ObjectKey) {
		bindCleaningReport(key, CleanFailedRequestAnnotation,
			sanitizeReportedReason("no disks found", cleanFailedReasonPrefix))
		reconcile(r, key)
		Expect(getPhysicalHost(key).Status.State).To(Equal(infrav1.StateError))
	}

	// setCleaningRequest sets spec.cleaning.request on the host.
	setCleaningRequest := func(key client.ObjectKey, request string) {
		current := getPhysicalHost(key)
		changed := current.DeepCopy()
		changed.Spec.Cleaning.Request = request
		Expect(k8sClient.Patch(ctx, changed, client.MergeFrom(current))).To(Succeed())
	}

	It("holds the host in Error after /clean-failed, until spec.cleaning.request changes", func() {
		r := newReconciler()
		key := bootedHost(r, "clean-failed")
		bindCleaningReport(key, CleanFailedRequestAnnotation, sanitizeReportedReason("no disks found", cleanFailedReasonPrefix))

		reconcile(r, key)
		failed := getPhysicalHost(key)
		Expect(failed.Status.State).To(Equal(infrav1.StateError))
		Expect(failed.Status.ErrorMessage).To(Equal(cleanFailedReasonPrefix + "no disks found"))
		Expect(conditions.GetReason(failed, infrav1.HostCleanedCondition)).To(Equal(infrav1.CleaningFailedReason))
		Expect(readBootstrapCredentials(getCredentialSecret(key)).token).To(BeEmpty(), "a failed run's token is retired")

		By("reconciling again with the BMC answering: the host stays held")
		reconcile(r, key)
		Expect(getPhysicalHost(key).Status.State).To(Equal(infrav1.StateError))

		By("asking for a retry")
		setCleaningRequest(key, "retry-1")
		reconcile(r, key)
		retried := getPhysicalHost(key)
		Expect(retried.Status.State).To(Equal(infrav1.StateCleaning))
		Expect(retried.CleaningStartedAt()).NotTo(BeNil())
		Expect(retried.ObservedCleaningRequest()).To(Equal("retry-1"))

		By("failing again: the same request value does not retry a second time")
		failRun(r, key)
		reconcile(r, key)
		Expect(getPhysicalHost(key).Status.State).To(Equal(infrav1.StateError))
	})

	It("records a request set before the failure, so it never retries the failure on its own", func() {
		r := newReconciler()
		key := bootedHost(r, "clean-early-request")

		By("changing the request while the run is still Cleaning")
		setCleaningRequest(key, "early")
		reconcile(r, key)
		running := getPhysicalHost(key)
		Expect(running.Status.State).To(Equal(infrav1.StateCleaning))
		Expect(running.ObservedCleaningRequest()).To(Equal("early"))

		By("failing the run: the host stays held")
		failRun(r, key)
		reconcile(r, key)
		Expect(getPhysicalHost(key).Status.State).To(Equal(infrav1.StateError))
	})

	It("does not retry when the request is cleared", func() {
		r := newReconciler()
		key := bootedHost(r, "clean-cleared-request")
		setCleaningRequest(key, "before")
		reconcile(r, key)
		failRun(r, key)

		setCleaningRequest(key, "")
		reconcile(r, key)
		host := getPhysicalHost(key)
		Expect(host.Status.State).To(Equal(infrav1.StateError))
		Expect(host.ObservedCleaningRequest()).To(BeEmpty())
	})

	It("holds the host in Error when the run does not report in time", func() {
		r := newReconciler()
		key := bootedHost(r, "clean-timeout")
		current := getPhysicalHost(key)
		late := current.DeepCopy()
		past := metav1.NewTime(time.Now().Add(-2 * time.Minute))
		late.SetCleaningStartedAt(&past)
		Expect(k8sClient.Status().Patch(ctx, late, client.MergeFrom(current))).To(Succeed())

		reconcile(r, key)

		host := getPhysicalHost(key)
		Expect(host.Status.State).To(Equal(infrav1.StateError))
		Expect(conditions.GetReason(host, infrav1.HostCleanedCondition)).To(Equal(infrav1.CleaningTimedOutReason))
		Expect(bmc.ForcePowerOffCalled).To(BeTrue())
	})

	It("makes the host Available without a wipe when the owner sets spec.cleaning.mode Disabled mid-run", func() {
		r := newReconciler()
		key := bootedHost(r, "clean-skip")
		current := getPhysicalHost(key)
		disabled := current.DeepCopy()
		disabled.Spec.Cleaning.Mode = infrav1.CleaningModeDisabled
		Expect(k8sClient.Patch(ctx, disabled, client.MergeFrom(current))).To(Succeed())

		reconcile(r, key)

		host := getPhysicalHost(key)
		Expect(host.Status.State).To(Equal(infrav1.StateAvailable))
		Expect(conditions.GetReason(host, infrav1.HostCleanedCondition)).To(Equal(infrav1.CleaningSkippedReason))
		Expect(readBootstrapCredentials(getCredentialSecret(key)).token).To(BeEmpty())
	})

	It("releases a host held after a failed run when the owner sets spec.cleaning.mode Disabled", func() {
		r := newReconciler()
		key := bootedHost(r, "clean-held-skip")
		failRun(r, key)
		recorder := events.NewFakeRecorder(100)
		r.Recorder = recorder
		current := getPhysicalHost(key)
		Expect(current.Status.State).To(Equal(infrav1.StateError))
		disabled := current.DeepCopy()
		disabled.Spec.Cleaning.Mode = infrav1.CleaningModeDisabled
		Expect(k8sClient.Patch(ctx, disabled, client.MergeFrom(current))).To(Succeed())
		reconcile(r, key)

		host := getPhysicalHost(key)
		Expect(host.Status.State).To(Equal(infrav1.StateAvailable))
		Expect(conditions.GetReason(host, infrav1.HostCleanedCondition)).To(Equal(infrav1.CleaningSkippedReason))
		runs := digestMetric(internalmetrics.PhysicalHostCleaningTotal, ns.Name, func(m *dto.Metric) float64 {
			return m.GetCounter().GetValue()
		})
		Expect(runs).To(HaveKeyWithValue(string(internalmetrics.CleaningOutcomeSkipped), 1.0))
		Expect(recorder.Events).To(Receive(ContainSubstring(eventCleaningSkipped)))
	})

	It("waits in Cleaning when the manager has no cleaning image", func() {
		r := newReconciler()
		r.CleaningImageURL = ""
		key := releasedHost("clean-unconfigured", infrav1.CleaningModeMetadata)

		reconcile(r, key)

		host := getPhysicalHost(key)
		Expect(host.Status.State).To(Equal(infrav1.StateCleaning))
		Expect(conditions.GetReason(host, infrav1.HostCleanedCondition)).To(Equal(infrav1.CleaningNotConfiguredReason))
		Expect(bmc.SetBootSourcePXECalled).To(BeFalse())
	})

	It("enters Cleaning even while the BMC is down at release", func() {
		r := newReconciler()
		r.RedfishClientFactory = (&bmcGate{reachable: false}).factory()
		r.TransientRetryInterval = time.Second
		key := releasedHost("clean-bmc-down", infrav1.CleaningModeMetadata)

		reconcile(r, key)
		host := getPhysicalHost(key)
		Expect(host.Status.State).To(Equal(infrav1.StateCleaning), "a BMC error must not overwrite Cleaning")
		Expect(conditions.GetReason(host, infrav1.HostCleanedCondition)).To(Equal(infrav1.CleaningPendingReason))
	})

	It("makes a host released with spec.cleaning.mode Disabled Available at once", func() {
		key := releasedHost("no-clean", infrav1.CleaningModeDisabled)
		reconcile(newReconciler(), key)
		host := getPhysicalHost(key)
		Expect(host.Status.State).To(Equal(infrav1.StateAvailable))
		Expect(conditions.Has(host, infrav1.HostCleanedCondition)).To(BeFalse())
		Expect(bmc.SetBootSourcePXECalled).To(BeFalse())
	})

	// clusterctl move recreates the host without status, so a host that was
	// Cleaning or held in Error arrives with no state.
	It("wipes an unclaimed host that has no state yet, as after clusterctl move", func() {
		host := claimedPhysicalHost(ns.Name, "clean-moved", "")
		host.Spec.ConsumerRef = nil
		host.Spec.Cleaning = &infrav1.CleaningSpec{Mode: infrav1.CleaningModeMetadata}
		host.Finalizers = []string{PhysicalHostFinalizer}
		Expect(k8sClient.Create(ctx, host)).To(Succeed())
		key := client.ObjectKeyFromObject(host)

		reconcile(newReconciler(), key)

		moved := getPhysicalHost(key)
		Expect(moved.Status.State).To(Equal(infrav1.StateCleaning))
		Expect(conditions.GetReason(moved, infrav1.HostAvailableCondition)).To(Equal(infrav1.HostCleaningReason))
		Expect(bmc.SetBootSourcePXECalled).To(BeTrue())
	})

	// The claim run's inspector was the claimant's choice, so a pool member's
	// report must not steer the next namespace's claim (matchAt: Claim).
	It("drops a pool member's inspection report at the wipe and keeps another host's", func() {
		withReport := func(key client.ObjectKey, pool string) {
			current := getPhysicalHost(key)
			labelled := current.DeepCopy()
			if pool != "" {
				labelled.Labels = map[string]string{poolLabelKey(pool): ""}
			}
			Expect(k8sClient.Patch(ctx, labelled, client.MergeFrom(current))).To(Succeed())
			current = getPhysicalHost(key)
			reported := current.DeepCopy()
			reported.Status.InspectionReport = &infrav1.InspectionReport{Timestamp: metav1.Now(), Manufacturer: "fixture"}
			Expect(k8sClient.Status().Patch(ctx, reported, client.MergeFrom(current))).To(Succeed())
		}
		r := newReconciler()
		member := releasedHost("clean-member", infrav1.CleaningModeMetadata)
		withReport(member, "shared")
		own := releasedHost("clean-own", infrav1.CleaningModeMetadata)
		withReport(own, "")

		reconcile(r, member)
		reconcile(r, own)

		Expect(getPhysicalHost(member).Status.InspectionReport).To(BeNil())
		Expect(getPhysicalHost(own).Status.InspectionReport).NotTo(BeNil())
	})

	// A pool claims only a host with HostCleaned=True, so an Available host that
	// was never wiped is wiped once instead of staying out of every pool.
	It("wipes an Available host once when spec.cleaning.mode becomes Metadata", func() {
		r := newReconciler()
		key := releasedHost("clean-later", infrav1.CleaningModeDisabled)
		reconcile(r, key)
		Expect(getPhysicalHost(key).Status.State).To(Equal(infrav1.StateAvailable))
		bmc.SetBootSourcePXECalled = false

		current := getPhysicalHost(key)
		withMode := current.DeepCopy()
		withMode.Spec.Cleaning = &infrav1.CleaningSpec{Mode: infrav1.CleaningModeMetadata}
		Expect(k8sClient.Patch(ctx, withMode, client.MergeFrom(current))).To(Succeed())
		reconcile(r, key)

		host := getPhysicalHost(key)
		Expect(host.Status.State).To(Equal(infrav1.StateCleaning))
		Expect(conditions.GetReason(host, infrav1.HostAvailableCondition)).To(Equal(infrav1.HostCleaningReason))
		Expect(bmc.SetBootSourcePXECalled).To(BeTrue())
	})

	It("leaves an Available host alone once it has been wiped", func() {
		r := newReconciler()
		key := bootedHost(r, "clean-done")
		bindCleaningReport(key, CleanedRequestAnnotation, cleanedRequestValue)
		reconcile(r, key)
		Expect(getPhysicalHost(key).Status.State).To(Equal(infrav1.StateAvailable))
		bmc.SetBootSourcePXECalled = false

		reconcile(r, key)

		host := getPhysicalHost(key)
		Expect(host.Status.State).To(Equal(infrav1.StateAvailable))
		Expect(conditions.IsTrue(host, infrav1.HostCleanedCondition)).To(BeTrue())
		Expect(bmc.SetBootSourcePXECalled).To(BeFalse())
	})

	Describe("BMC outage", func() {
		// unreachable makes r's BMC unreachable at the network level.
		unreachable := func(r *PhysicalHostReconciler) {
			r.RedfishClientFactory = (&bmcGate{reachable: false}).factory()
			r.TransientRetryInterval = time.Second
		}

		It("keeps a booted Cleaning host in its run", func() {
			r := newReconciler()
			key := bootedHost(r, "clean-booted-bmc-down")
			startedAt := getPhysicalHost(key).CleaningStartedAt()
			unreachable(r)

			reconcile(r, key)

			host := getPhysicalHost(key)
			Expect(host.Status.State).To(Equal(infrav1.StateCleaning))
			Expect(host.CleaningStartedAt().Equal(startedAt)).To(BeTrue(), "the run's clock is not reset")
			Expect(conditions.GetReason(host, infrav1.HostCleanedCondition)).To(Equal(infrav1.CleaningInProgressReason))
		})

		It("keeps a held host held, and still held once the BMC is back", func() {
			r := newReconciler()
			key := bootedHost(r, "clean-held-bmc-down")
			failRun(r, key)
			failedMessage := getPhysicalHost(key).Status.ErrorMessage
			unreachable(r)

			reconcile(r, key)

			host := getPhysicalHost(key)
			Expect(host.Status.State).To(Equal(infrav1.StateError))
			Expect(host.Status.ErrorMessage).To(Equal(failedMessage), "the BMC error does not replace the wipe failure")
			Expect(conditions.GetReason(host, infrav1.HostCleanedCondition)).To(Equal(infrav1.CleaningFailedReason))

			By("reaching the BMC again")
			r.RedfishClientFactory = fixedBMC(bmc)
			reconcile(r, key)
			Expect(getPhysicalHost(key).Status.State).To(Equal(infrav1.StateError))
		})
	})

	Describe("runs that must not start", func() {
		// optIn sets spec.cleaning on the host to Metadata with the given request.
		optIn := func(key client.ObjectKey, request string) {
			current := getPhysicalHost(key)
			changed := current.DeepCopy()
			changed.Spec.Cleaning = &infrav1.CleaningSpec{Mode: infrav1.CleaningModeMetadata, Request: request}
			Expect(k8sClient.Patch(ctx, changed, client.MergeFrom(current))).To(Succeed())
		}

		It("starts no run when mode and request change on a claimed host", func() {
			key := provisioningHost(ns.Name, "claimed-opt-in", "claimed-opt-in-machine", infrav1.StateReady, nil)
			optIn(key, "now")

			reconcile(newReconciler(), key)

			host := getPhysicalHost(key)
			Expect(host.Status.State).To(Equal(infrav1.StateReady))
			Expect(host.ObservedCleaningRequest()).To(Equal("now"))
			Expect(conditions.Has(host, infrav1.HostCleanedCondition)).To(BeFalse())
			Expect(bmc.SetBootSourcePXECalled).To(BeFalse())
		})

		It("starts no run when the request changes on a host held by a failed provisioning run", func() {
			report := sanitizeFailureReason("COS_OEM partition not found")
			key := provisioningHost(ns.Name, "provision-failed", "provision-failed-machine", infrav1.StateDeploying,
				map[string]string{ProvisionFailedRequestAnnotation: report})
			r := newReconciler()
			reconcile(r, key)
			Expect(getPhysicalHost(key).Status.State).To(Equal(infrav1.StateError))

			optIn(key, "retry")
			reconcile(r, key)

			host := getPhysicalHost(key)
			Expect(host.Status.State).To(Equal(infrav1.StateError))
			Expect(host.Status.ErrorMessage).To(Equal(report))
			Expect(host.ObservedCleaningRequest()).To(Equal("retry"))
			Expect(conditions.Has(host, infrav1.HostCleanedCondition)).To(BeFalse())
		})

		It("ignores a /cleaned report bound to an earlier cleaning run", func() {
			r := newReconciler()
			key := bootedHost(r, "clean-stale-run")
			firstRun := readBootstrapCredentials(getCredentialSecret(key))
			failRun(r, key)
			setCleaningRequest(key, "retry-1")
			reconcile(r, key)
			Expect(getPhysicalHost(key).CleaningStartedAt()).NotTo(BeNil(), "the retry has booted")

			current := getPhysicalHost(key)
			binder, err := newCallbackBinder(current, firstRun)
			Expect(err).NotTo(HaveOccurred())
			stale := current.DeepCopy()
			binder.setAnnotation(stale, CleanedRequestAnnotation, cleanedRequestValue, "")
			Expect(k8sClient.Patch(ctx, stale, client.MergeFrom(current))).To(Succeed())

			reconcile(r, key)

			host := getPhysicalHost(key)
			Expect(host.Status.State).To(Equal(infrav1.StateCleaning))
			Expect(conditions.IsTrue(host, infrav1.HostCleanedCondition)).To(BeFalse())
		})
	})

	Describe("credential separation", func() {
		It("authenticates a cleaning token on the cleaning routes only", func() {
			r := newReconciler()
			key := bootedHost(r, "clean-auth")
			cleaningToken := callbackTokenOf(key)

			request := func(route string) *http.Request {
				req := httptest.NewRequest(http.MethodPost, "/api/v1/"+route+"/"+key.Namespace+"/"+key.Name, nil)
				req.SetPathValue("namespace", key.Namespace)
				req.SetPathValue("hostName", key.Name)
				return req
			}
			Expect(newCleaningTokenVerifier(k8sClient, ctrl.Log)(cleaningToken, request("cleaned"))).To(Succeed())
			Expect(newBearerTokenVerifier(claimReader{Reader: k8sClient}, ctrl.Log)(cleaningToken, request("provisioned"))).NotTo(Succeed())
			Expect(newBearerTokenVerifier(claimReader{Reader: k8sClient}, ctrl.Log)(cleaningToken, request("bootstrap"))).NotTo(Succeed())
			Expect(newCleaningTokenVerifier(k8sClient, ctrl.Log)("not-the-token", request("cleaned"))).NotTo(Succeed())
		})

		It("refuses a claim's token on the cleaning routes", func() {
			key := provisioningHost(ns.Name, "claimed-auth", "claimed-auth-machine", infrav1.StateDeploying, nil)
			token := ensureCallbackCredentials(key)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/cleaned/"+key.Namespace+"/"+key.Name, nil)
			req.SetPathValue("namespace", key.Namespace)
			req.SetPathValue("hostName", key.Name)
			Expect(newCleaningTokenVerifier(k8sClient, ctrl.Log)(token, req)).NotTo(Succeed())
		})

		It("signals a /cleaned report through the handler", func() {
			r := newReconciler()
			key := bootedHost(r, "clean-handler")
			req := httptest.NewRequest(http.MethodPost, "/api/v1/cleaned/"+key.Namespace+"/"+key.Name, strings.NewReader(`{"status":"cleaned"}`))
			req.SetPathValue("namespace", key.Namespace)
			req.SetPathValue("hostName", key.Name)
			req.Header.Set("Authorization", "Bearer "+callbackTokenOf(key))
			w := httptest.NewRecorder()
			(&CleaningReportHandler{Client: k8sClient, Log: ctrl.Log}).ServeHTTP(w, req)
			Expect(w.Code).To(Equal(http.StatusAccepted))

			reconcile(r, key)
			Expect(getPhysicalHost(key).Status.State).To(Equal(infrav1.StateAvailable))
		})
	})
})

var _ = Describe("buildCleaningIPXEScript", func() {
	It("renders clean mode and none of a claim's parameters", func() {
		script := buildCleaningIPXEScript(cleaningScriptParams{
			InspectionImageURL: "https://img.example/inspector",
			APIBase:            "https://beskar7.example:8082",
			Namespace:          "ns",
			HostName:           "host",
			Token:              "tok",
			CABase64:           "Q0E=",
			Bootif:             "01-52-54-00-12-34-56",
		})
		Expect(script).To(Equal("#!ipxe\n" +
			"kernel https://img.example/inspector/vmlinuz beskar7.api=https://beskar7.example:8082 beskar7.namespace=ns beskar7.host=host beskar7.token=tok beskar7.mode=clean beskar7.ca=Q0E= BOOTIF=01-52-54-00-12-34-56\n" +
			"initrd https://img.example/inspector/initrd.img\n" +
			"boot\n"))
		for _, param := range []string{"beskar7.target", "beskar7.disk", "beskar7.provider-id"} {
			Expect(script).NotTo(ContainSubstring(param))
		}
	})
})
