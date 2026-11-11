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
	"github.com/stmcginnis/gofish/schemas"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
	"github.com/projectbeskar/beskar7/internal/auth"
	internalredfish "github.com/projectbeskar/beskar7/internal/redfish"
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

var _ = Describe("Cleaning a released PhysicalHost (cleaningMode Metadata)", func() {
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
		withMode.Spec.CleaningMode = mode
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
		Expect(getPhysicalHost(key).Status.CleaningTimestamp).NotTo(BeNil())
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
		Expect(host.Status.CleaningTimestamp).NotTo(BeNil())
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
		Expect(host.Status.CleaningTimestamp).To(BeNil())
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

	It("holds the host in Error after /clean-failed, until the retry annotation", func() {
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
		current := getPhysicalHost(key)
		retry := current.DeepCopy()
		retry.Annotations = map[string]string{RetryCleaningAnnotation: "true"}
		Expect(k8sClient.Patch(ctx, retry, client.MergeFrom(current))).To(Succeed())
		reconcile(r, key)
		retried := getPhysicalHost(key)
		Expect(retried.Status.State).To(Equal(infrav1.StateCleaning))
		Expect(retried.Status.CleaningTimestamp).NotTo(BeNil())
		Expect(retried.Annotations).NotTo(HaveKey(RetryCleaningAnnotation))
	})

	It("holds the host in Error when the run does not report in time", func() {
		r := newReconciler()
		key := bootedHost(r, "clean-timeout")
		current := getPhysicalHost(key)
		late := current.DeepCopy()
		past := metav1.NewTime(time.Now().Add(-2 * time.Minute))
		late.Status.CleaningTimestamp = &past
		Expect(k8sClient.Status().Patch(ctx, late, client.MergeFrom(current))).To(Succeed())

		reconcile(r, key)

		host := getPhysicalHost(key)
		Expect(host.Status.State).To(Equal(infrav1.StateError))
		Expect(conditions.GetReason(host, infrav1.HostCleanedCondition)).To(Equal(infrav1.CleaningTimedOutReason))
		Expect(bmc.ForcePowerOffCalled).To(BeTrue())
	})

	It("makes the host Available without a wipe when the owner sets cleaningMode Disabled mid-run", func() {
		r := newReconciler()
		key := bootedHost(r, "clean-skip")
		current := getPhysicalHost(key)
		disabled := current.DeepCopy()
		disabled.Spec.CleaningMode = infrav1.CleaningModeDisabled
		Expect(k8sClient.Patch(ctx, disabled, client.MergeFrom(current))).To(Succeed())

		reconcile(r, key)

		host := getPhysicalHost(key)
		Expect(host.Status.State).To(Equal(infrav1.StateAvailable))
		Expect(conditions.GetReason(host, infrav1.HostCleanedCondition)).To(Equal(infrav1.CleaningSkippedReason))
		Expect(readBootstrapCredentials(getCredentialSecret(key)).token).To(BeEmpty())
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

	It("makes a host released with cleaningMode Disabled Available at once", func() {
		key := releasedHost("no-clean", infrav1.CleaningModeDisabled)
		reconcile(newReconciler(), key)
		host := getPhysicalHost(key)
		Expect(host.Status.State).To(Equal(infrav1.StateAvailable))
		Expect(conditions.Has(host, infrav1.HostCleanedCondition)).To(BeFalse())
		Expect(bmc.SetBootSourcePXECalled).To(BeFalse())
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
			Expect(newBearerTokenVerifier(k8sClient, ctrl.Log)(cleaningToken, request("provisioned"))).NotTo(Succeed())
			Expect(newBearerTokenVerifier(k8sClient, ctrl.Log)(cleaningToken, request("bootstrap"))).NotTo(Succeed())
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
