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

// Tests for the D-015 contract-v4 provisioning-complete flow:
//   - inspect-complete → StateDeploying (not StateReady)
//   - provisioned callback → StateReady → Beskar7Machine Ready + ProviderID
//   - ClearBootSourceOverride called on first provisioning
//   - deploy-timeout: terminal failure when host stuck in Deploying

package controllers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	infrastructurev1beta1 "github.com/projectbeskar/beskar7/api/v1beta1"
	"github.com/projectbeskar/beskar7/internal/auth"
	internalredfish "github.com/projectbeskar/beskar7/internal/redfish"
)

// buildProvisionedMux wires the ProvisionedHandler onto a ServeMux using the same
// route pattern as SetupCallbackServer, bound to an httptest.Server. Avoids calling
// SetupCallbackServer (which requires a real cert dir) — tests everything below TLS.
func buildProvisionedMux() (*http.ServeMux, *ProvisionedHandler) {
	log := ctrl.Log.WithName("provisioned-handler-test")
	handler := &ProvisionedHandler{
		Client: k8sClient,
		Log:    log,
	}
	verifier := newBearerTokenVerifier(k8sClient, log)
	mux := http.NewServeMux()
	mux.Handle("POST /api/v1/provisioned/{namespace}/{hostName}",
		auth.RequireBearer(log, verifier, handler))
	return mux, handler
}

// ── D-015 flow: inspect-complete → StateDeploying ───────────────────────────

var _ = Describe("D-015 PhysicalHost inspect-complete → StateDeploying", func() {
	var testNs *corev1.Namespace

	BeforeEach(func() {
		testNs = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "d015-deploying-"}}
		Expect(k8sClient.Create(ctx, testNs)).To(Succeed())
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, testNs)).To(Succeed())
	})

	It("should transition host to StateDeploying (not StateReady) on inspect-complete annotation", func() {
		ph := &infrastructurev1beta1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "host-inspect-complete",
				Namespace: testNs.Name,
				Annotations: map[string]string{
					InspectionRequestAnnotation: "inspect-complete",
				},
			},
			Spec: infrastructurev1beta1.PhysicalHostSpec{
				RedfishConnection: infrastructurev1beta1.RedfishConnection{
					Address:              "https://192.168.1.200",
					CredentialsSecretRef: "dummy-creds",
				},
			},
		}
		Expect(k8sClient.Create(ctx, ph)).To(Succeed())
		ph.Status.State = infrastructurev1beta1.StateInspecting
		ph.Status.InspectionPhase = infrastructurev1beta1.InspectionPhaseComplete
		Expect(k8sClient.Status().Update(ctx, ph)).To(Succeed())

		// Re-fetch so annotations round-trip correctly.
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ph.Name, Namespace: testNs.Name}, ph)).To(Succeed())

		// Call the annotation handler directly (PhysicalHostReconciler method).
		r := &PhysicalHostReconciler{
			Client: k8sClient,
			Log:    ctrl.Log.WithName("d015-test"),
			Scheme: k8sClient.Scheme(),
		}
		r.applyInspectionRequest(ctx, r.Log, ph)

		By("Verifying State == Deploying (not Ready)")
		Expect(ph.Status.State).To(Equal(infrastructurev1beta1.StateDeploying),
			"inspect-complete must land in Deploying, not Ready (D-015)")

		By("Verifying annotation was cleared")
		Expect(ph.Annotations).NotTo(HaveKey(InspectionRequestAnnotation))

		By("Verifying DeployingTimestamp was set")
		Expect(ph.Status.DeployingTimestamp).NotTo(BeNil(),
			"DeployingTimestamp must be set when entering Deploying")

		By("Verifying no spurious provisioned annotation was set")
		Expect(ph.Annotations).NotTo(HaveKey(ProvisionedRequestAnnotation))
	})
})

// ── D-015 flow: StateDeploying + provisioned → StateReady ───────────────────

var _ = Describe("D-015 StateDeploying + provisioned signal → StateReady", func() {
	var (
		testNs     *corev1.Namespace
		credSecret *corev1.Secret
		ph         *infrastructurev1beta1.PhysicalHost
		mockRf     *internalredfish.MockClient
		reconciler *Beskar7MachineReconciler
	)

	BeforeEach(func() {
		testNs = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "d015-ready-"}}
		Expect(k8sClient.Create(ctx, testNs)).To(Succeed())

		credSecret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "bmc-creds", Namespace: testNs.Name},
			Data: map[string][]byte{
				"username": []byte("admin"),
				"password": []byte("secret"),
			},
		}
		Expect(k8sClient.Create(ctx, credSecret)).To(Succeed())

		mockRf = internalredfish.NewMockClient()
		reconciler = &Beskar7MachineReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			Log:    ctrl.Log.WithName("d015-ready-test"),
			RedfishClientFactory: func(_ context.Context, _, _, _ string, _ bool, _ []byte) (internalredfish.Client, error) {
				return mockRf, nil
			},
			BootstrapURLBase: "https://example.com:8082",
		}

		now := metav1.Now()
		ph = &infrastructurev1beta1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{Name: "host-deploying", Namespace: testNs.Name},
			Spec: infrastructurev1beta1.PhysicalHostSpec{
				RedfishConnection: infrastructurev1beta1.RedfishConnection{
					Address:              "https://192.168.1.201",
					CredentialsSecretRef: credSecret.Name,
				},
			},
		}
		Expect(k8sClient.Create(ctx, ph)).To(Succeed())
		ph.Status.State = infrastructurev1beta1.StateDeploying
		ph.Status.DeployingTimestamp = &now
		Expect(k8sClient.Status().Update(ctx, ph)).To(Succeed())
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, testNs)).To(Succeed())
	})

	It("handleDeployingHost returns Provisioning phase and 30s requeue while deploying", func() {
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ph.Name, Namespace: testNs.Name}, ph)).To(Succeed())
		b7m := &infrastructurev1beta1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "b7m-deploying", Namespace: testNs.Name},
			Spec: infrastructurev1beta1.Beskar7MachineSpec{
				InspectionImageURL: "http://boot/inspect.ipxe",
				TargetImageURL:     "http://boot/kairos.raw",
				TargetImageDigest:  bootTestDigest,
			},
		}
		result, err := reconciler.handleDeployingHost(ctx, reconciler.Log, b7m, ph)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(30 * time.Second))
		Expect(b7m.Status.Phase).NotTo(BeNil())
		Expect(*b7m.Status.Phase).To(Equal("Provisioning"))
		Expect(b7m.Status.Ready).To(BeFalse())
		Expect(b7m.Status.FailureReason).To(BeNil())
	})

	It("provisioned annotation → PhysicalHost transitions Deploying→Ready", func() {
		phR := &PhysicalHostReconciler{
			Client: k8sClient,
			Log:    ctrl.Log.WithName("d015-phready"),
			Scheme: k8sClient.Scheme(),
		}
		// Simulate the signal the provisioned handler would write.
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ph.Name, Namespace: testNs.Name}, ph)).To(Succeed())
		if ph.Annotations == nil {
			ph.Annotations = map[string]string{}
		}
		ph.Annotations[ProvisionedRequestAnnotation] = "provisioned"
		phR.applyProvisionedRequestAnnotation(phR.Log, ph)

		Expect(ph.Status.State).To(Equal(infrastructurev1beta1.StateReady),
			"provisioned signal must drive Deploying→Ready")
		Expect(ph.Annotations).NotTo(HaveKey(ProvisionedRequestAnnotation),
			"annotation must be cleared after consumption")
	})

	It("handleReadyHost: sets ProviderID, Ready=true, Initialization.Provisioned=true, calls ClearBootSourceOverride", func() {
		// Move host to StateReady (as if provisioned callback landed).
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ph.Name, Namespace: testNs.Name}, ph)).To(Succeed())
		ph.Status.State = infrastructurev1beta1.StateReady
		Expect(k8sClient.Status().Update(ctx, ph)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ph.Name, Namespace: testNs.Name}, ph)).To(Succeed())

		b7m := &infrastructurev1beta1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "b7m-ready", Namespace: testNs.Name},
			Spec: infrastructurev1beta1.Beskar7MachineSpec{
				InspectionImageURL: "http://boot/inspect.ipxe",
				TargetImageURL:     "http://boot/kairos.raw",
				TargetImageDigest:  bootTestDigest,
			},
		}

		result, err := reconciler.handleReadyHost(ctx, reconciler.Log, b7m, ph)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(ctrl.Result{}))

		By("Verifying ProviderID is set")
		Expect(b7m.Spec.ProviderID).NotTo(BeNil())
		Expect(*b7m.Spec.ProviderID).To(Equal("b7://" + testNs.Name + "/" + ph.Name))

		By("Verifying Status.Ready and Initialization.Provisioned")
		Expect(b7m.Status.Ready).To(BeTrue())
		Expect(b7m.Status.Initialization).NotTo(BeNil())
		Expect(b7m.Status.Initialization.Provisioned).To(BeTrue())

		By("Verifying ClearBootSourceOverride was called on first provisioning")
		Expect(mockRf.ClearBootSourceOverrideCalled).To(BeTrue(),
			"ClearBootSourceOverride must be called on first provisioning (D-015 issue #2)")

		By("Verifying Phase == Provisioned")
		Expect(b7m.Status.Phase).NotTo(BeNil())
		Expect(*b7m.Status.Phase).To(Equal("Provisioned"))
	})

	It("handleReadyHost: does NOT call ClearBootSourceOverride on re-reconcile (already Ready)", func() {
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ph.Name, Namespace: testNs.Name}, ph)).To(Succeed())
		ph.Status.State = infrastructurev1beta1.StateReady
		Expect(k8sClient.Status().Update(ctx, ph)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ph.Name, Namespace: testNs.Name}, ph)).To(Succeed())

		provID := "b7://" + testNs.Name + "/" + ph.Name
		b7m := &infrastructurev1beta1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "b7m-already-ready", Namespace: testNs.Name},
			Spec: infrastructurev1beta1.Beskar7MachineSpec{
				InspectionImageURL: "http://boot/inspect.ipxe",
				TargetImageURL:     "http://boot/kairos.raw",
				TargetImageDigest:  bootTestDigest,
				ProviderID:         &provID,
			},
		}
		// Already provisioned — simulate a re-reconcile.
		b7m.Status.Ready = true

		_, err := reconciler.handleReadyHost(ctx, reconciler.Log, b7m, ph)
		Expect(err).NotTo(HaveOccurred())

		// firstProvisioning == false, so ClearBootSourceOverride must NOT be called.
		Expect(mockRf.ClearBootSourceOverrideCalled).To(BeFalse(),
			"ClearBootSourceOverride must NOT be called on re-reconcile (not first provisioning)")
	})
})

// ── D-015 deploy-timeout ─────────────────────────────────────────────────────

var _ = Describe("D-015 deploy-timeout: terminal failure when host stuck in Deploying", func() {
	var testNs *corev1.Namespace

	BeforeEach(func() {
		testNs = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "d015-timeout-"}}
		Expect(k8sClient.Create(ctx, testNs)).To(Succeed())
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, testNs)).To(Succeed())
	})

	It("marks terminal failure with DeploymentTimedOutReason when DeployingTimestamp exceeded", func() {
		// Set DeployingTimestamp 2× the timeout in the past.
		oldTime := metav1.NewTime(time.Now().Add(-2 * DefaultDeploymentTimeout))
		ph := &infrastructurev1beta1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{Name: "host-timeout", Namespace: testNs.Name},
			Spec: infrastructurev1beta1.PhysicalHostSpec{
				RedfishConnection: infrastructurev1beta1.RedfishConnection{
					Address:              "https://192.168.1.202",
					CredentialsSecretRef: "dummy-creds",
				},
			},
		}
		Expect(k8sClient.Create(ctx, ph)).To(Succeed())
		ph.Status.State = infrastructurev1beta1.StateDeploying
		ph.Status.DeployingTimestamp = &oldTime
		Expect(k8sClient.Status().Update(ctx, ph)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ph.Name, Namespace: testNs.Name}, ph)).To(Succeed())

		r := &Beskar7MachineReconciler{
			Client:            k8sClient,
			Scheme:            k8sClient.Scheme(),
			Log:               ctrl.Log.WithName("d015-timeout-test"),
			DeploymentTimeout: DefaultDeploymentTimeout,
		}
		b7m := &infrastructurev1beta1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "b7m-timeout", Namespace: testNs.Name},
			Spec: infrastructurev1beta1.Beskar7MachineSpec{
				InspectionImageURL: "http://boot/inspect.ipxe",
				TargetImageURL:     "http://boot/kairos.raw",
				TargetImageDigest:  bootTestDigest,
			},
		}

		result, err := r.handleDeployingHost(ctx, r.Log, b7m, ph)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(ctrl.Result{}), "terminal failure must not requeue")

		By("Verifying FailureReason == DeploymentTimedOutReason")
		Expect(b7m.Status.FailureReason).NotTo(BeNil())
		Expect(*b7m.Status.FailureReason).To(Equal(DeploymentTimedOutReason))
		Expect(b7m.Status.FailureMessage).NotTo(BeNil())
		Expect(*b7m.Status.FailureMessage).To(ContainSubstring("did not complete within"))

		By("Verifying Phase == Failed and Ready == false")
		Expect(b7m.Status.Phase).NotTo(BeNil())
		Expect(*b7m.Status.Phase).To(Equal("Failed"))
		Expect(b7m.Status.Ready).To(BeFalse())
	})

	It("does NOT time out when DeployingTimestamp is recent", func() {
		now := metav1.Now()
		ph := &infrastructurev1beta1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{Name: "host-recent", Namespace: testNs.Name},
			Spec: infrastructurev1beta1.PhysicalHostSpec{
				RedfishConnection: infrastructurev1beta1.RedfishConnection{
					Address:              "https://192.168.1.203",
					CredentialsSecretRef: "dummy-creds",
				},
			},
		}
		Expect(k8sClient.Create(ctx, ph)).To(Succeed())
		ph.Status.State = infrastructurev1beta1.StateDeploying
		ph.Status.DeployingTimestamp = &now
		Expect(k8sClient.Status().Update(ctx, ph)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ph.Name, Namespace: testNs.Name}, ph)).To(Succeed())

		r := &Beskar7MachineReconciler{
			Client:            k8sClient,
			Scheme:            k8sClient.Scheme(),
			Log:               ctrl.Log.WithName("d015-recent-test"),
			DeploymentTimeout: DefaultDeploymentTimeout,
		}
		b7m := &infrastructurev1beta1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "b7m-recent", Namespace: testNs.Name},
		}

		result, err := r.handleDeployingHost(ctx, r.Log, b7m, ph)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(30*time.Second), "must requeue every 30s while deploying")
		Expect(b7m.Status.FailureReason).To(BeNil(), "must not mark failure for a recent deploy")
	})
})

// ── D-015 provisioned handler: 202 path + body cap ──────────────────────────

var _ = Describe("D-015 ProvisionedHandler HTTP", func() {
	var (
		testNs     *corev1.Namespace
		ph         *infrastructurev1beta1.PhysicalHost
		tokenPlain string
	)

	BeforeEach(func() {
		testNs = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "d015-http-"}}
		Expect(k8sClient.Create(ctx, testNs)).To(Succeed())

		// Mint a bearer token and store its hash on the PhysicalHost so the verifier accepts it.
		var hash string
		var err error
		tokenPlain, hash, err = auth.MintToken()
		Expect(err).NotTo(HaveOccurred())
		_, expiresAt := auth.LifetimeFor(time.Now())

		ph = &infrastructurev1beta1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{Name: "host-http", Namespace: testNs.Name},
			Spec: infrastructurev1beta1.PhysicalHostSpec{
				RedfishConnection: infrastructurev1beta1.RedfishConnection{
					Address:              "https://192.168.1.210",
					CredentialsSecretRef: "dummy-creds",
				},
			},
		}
		Expect(k8sClient.Create(ctx, ph)).To(Succeed())
		ph.Status.State = infrastructurev1beta1.StateDeploying
		ph.Status.Bootstrap = &infrastructurev1beta1.BootstrapStatus{
			TokenHash: hash,
			ExpiresAt: &expiresAt,
		}
		Expect(k8sClient.Status().Update(ctx, ph)).To(Succeed())
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, testNs)).To(Succeed())
	})

	It("returns 202 Accepted with {\"status\":\"accepted\"} for a valid bearer POST", func() {
		mux, _ := buildProvisionedMux()
		srv := httptest.NewServer(mux)
		defer srv.Close()

		body := bytes.NewBufferString(`{"status":"provisioned"}`)
		req, err := http.NewRequest(http.MethodPost,
			srv.URL+"/api/v1/provisioned/"+testNs.Name+"/"+ph.Name,
			body)
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+tokenPlain)
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()

		Expect(resp.StatusCode).To(Equal(http.StatusAccepted),
			"valid bearer POST must return 202 Accepted")

		var respBody map[string]string
		Expect(json.NewDecoder(resp.Body).Decode(&respBody)).To(Succeed())
		Expect(respBody["status"]).To(Equal("accepted"))

		By("Verifying ProvisionedRequestAnnotation was set on the PhysicalHost")
		updated := &infrastructurev1beta1.PhysicalHost{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ph.Name, Namespace: testNs.Name}, updated)).To(Succeed())
		Expect(updated.Annotations).To(HaveKeyWithValue(ProvisionedRequestAnnotation, "provisioned"))
	})

	It("returns 401 for missing bearer token", func() {
		mux, _ := buildProvisionedMux()
		srv := httptest.NewServer(mux)
		defer srv.Close()

		req, err := http.NewRequest(http.MethodPost,
			srv.URL+"/api/v1/provisioned/"+testNs.Name+"/"+ph.Name,
			bytes.NewBufferString(`{"status":"provisioned"}`))
		Expect(err).NotTo(HaveOccurred())
		// Deliberately no Authorization header.

		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()

		Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized))
	})

	It("returns 401 for a wrong bearer token", func() {
		mux, _ := buildProvisionedMux()
		srv := httptest.NewServer(mux)
		defer srv.Close()

		req, err := http.NewRequest(http.MethodPost,
			srv.URL+"/api/v1/provisioned/"+testNs.Name+"/"+ph.Name,
			bytes.NewBufferString(`{"status":"provisioned"}`))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer this-is-the-wrong-token")

		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()

		Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized))
	})

	It("accepts an oversized body and still returns 202 for valid bearer", func() {
		mux, _ := buildProvisionedMux()
		srv := httptest.NewServer(mux)
		defer srv.Close()

		// Body larger than provisionedMaxBodyBytes (64 KiB). The handler caps and
		// discards; body size alone must not cause an error.
		bigBody := bytes.Repeat([]byte("x"), provisionedMaxBodyBytes+1024)
		req, err := http.NewRequest(http.MethodPost,
			srv.URL+"/api/v1/provisioned/"+testNs.Name+"/"+ph.Name,
			bytes.NewReader(bigBody))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+tokenPlain)

		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()

		Expect(resp.StatusCode).To(Equal(http.StatusAccepted))
	})
})

// ── D-015 deployment-timeout resolver ───────────────────────────────────────

var _ = Describe("D-015 deploymentTimeout resolver", func() {
	It("falls back to DefaultDeploymentTimeout when unset", func() {
		r := &Beskar7MachineReconciler{}
		Expect(r.deploymentTimeout()).To(Equal(DefaultDeploymentTimeout))
	})

	It("falls back to DefaultDeploymentTimeout for zero value", func() {
		r := &Beskar7MachineReconciler{DeploymentTimeout: 0}
		Expect(r.deploymentTimeout()).To(Equal(DefaultDeploymentTimeout))
	})

	It("honors a configured positive value", func() {
		r := &Beskar7MachineReconciler{DeploymentTimeout: 45 * time.Minute}
		Expect(r.deploymentTimeout()).To(Equal(45 * time.Minute))
	})
})
