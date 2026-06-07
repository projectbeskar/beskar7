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

// Tests for the v4.1 contract provision-failed fast-fail flow:
//
//   - POST /api/v1/provision-failed: valid bearer + StateDeploying → 202 + annotation set
//   - POST /api/v1/provision-failed: missing / wrong bearer → 401
//   - POST /api/v1/provision-failed: oversized / garbage body → still 202 (body advisory)
//   - POST /api/v1/provision-failed: host NOT in StateDeploying → no transition
//   - PhysicalHost: ProvisionFailedRequestAnnotation → StateError + sanitized ErrorMessage
//   - sanitizeFailureReason: control chars/newlines stripped, length-capped, empty→generic
//   - Beskar7Machine StateError (deploy-failure path): terminal failure, DeploymentFailed reason

package controllers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"

	infrastructurev1beta1 "github.com/projectbeskar/beskar7/api/v1beta1"
	"github.com/projectbeskar/beskar7/internal/auth"
	internalredfish "github.com/projectbeskar/beskar7/internal/redfish"
)

// buildProvisionFailedMux wires the ProvisionFailedHandler onto a ServeMux using the
// same route pattern as SetupCallbackServer, bound to an httptest.Server. Avoids
// calling SetupCallbackServer (which requires a real cert dir) — tests everything
// below TLS.
func buildProvisionFailedMux() (*http.ServeMux, *ProvisionFailedHandler) {
	log := ctrl.Log.WithName("provision-failed-handler-test")
	handler := &ProvisionFailedHandler{
		Client: k8sClient,
		Log:    log,
	}
	verifier := newBearerTokenVerifier(k8sClient, log)
	mux := http.NewServeMux()
	mux.Handle("POST /api/v1/provision-failed/{namespace}/{hostName}",
		auth.RequireBearer(log, verifier, handler))
	return mux, handler
}

// ── provision-failed handler HTTP tests ──────────────────────────────────────

var _ = Describe("v4.1 ProvisionFailedHandler HTTP", func() {
	var (
		testNs     *corev1.Namespace
		ph         *infrastructurev1beta1.PhysicalHost
		tokenPlain string
	)

	BeforeEach(func() {
		testNs = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "d016-http-"}}
		Expect(k8sClient.Create(ctx, testNs)).To(Succeed())

		var hash string
		var err error
		tokenPlain, hash, err = auth.MintToken()
		Expect(err).NotTo(HaveOccurred())
		_, expiresAt := auth.LifetimeFor(time.Now())

		ph = &infrastructurev1beta1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{Name: "host-pfail-http", Namespace: testNs.Name},
			Spec: infrastructurev1beta1.PhysicalHostSpec{
				RedfishConnection: infrastructurev1beta1.RedfishConnection{
					Address:              "https://192.168.2.210",
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

	It("returns 202 Accepted with {\"status\":\"accepted\"} for a valid bearer POST in StateDeploying", func() {
		mux, _ := buildProvisionFailedMux()
		srv := httptest.NewServer(mux)
		defer srv.Close()

		body := bytes.NewBufferString(`{"reason":"image fetch failed: connection refused"}`)
		req, err := http.NewRequest(http.MethodPost,
			srv.URL+"/api/v1/provision-failed/"+testNs.Name+"/"+ph.Name,
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

		By("Verifying ProvisionFailedRequestAnnotation was set on the PhysicalHost")
		updated := &infrastructurev1beta1.PhysicalHost{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ph.Name, Namespace: testNs.Name}, updated)).To(Succeed())
		Expect(updated.Annotations).To(HaveKey(ProvisionFailedRequestAnnotation))
		val := updated.Annotations[ProvisionFailedRequestAnnotation]
		Expect(val).To(HavePrefix(provisionFailedReasonPrefix))
		Expect(val).To(ContainSubstring("image fetch failed"))
	})

	It("returns 401 for missing bearer token", func() {
		mux, _ := buildProvisionFailedMux()
		srv := httptest.NewServer(mux)
		defer srv.Close()

		req, err := http.NewRequest(http.MethodPost,
			srv.URL+"/api/v1/provision-failed/"+testNs.Name+"/"+ph.Name,
			bytes.NewBufferString(`{"reason":"fail"}`))
		Expect(err).NotTo(HaveOccurred())
		// Deliberately no Authorization header.

		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()

		Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized))
	})

	It("returns 401 for a wrong bearer token", func() {
		mux, _ := buildProvisionFailedMux()
		srv := httptest.NewServer(mux)
		defer srv.Close()

		req, err := http.NewRequest(http.MethodPost,
			srv.URL+"/api/v1/provision-failed/"+testNs.Name+"/"+ph.Name,
			bytes.NewBufferString(`{"reason":"fail"}`))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer this-is-the-wrong-token")

		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()

		Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized))
	})

	It("accepts an oversized body and still returns 202 for valid bearer", func() {
		mux, _ := buildProvisionFailedMux()
		srv := httptest.NewServer(mux)
		defer srv.Close()

		// Body larger than provisionFailedMaxBodyBytes (64 KiB). The handler caps and
		// discards; body size alone must not cause an error.
		bigBody := bytes.Repeat([]byte("x"), provisionFailedMaxBodyBytes+1024)
		req, err := http.NewRequest(http.MethodPost,
			srv.URL+"/api/v1/provision-failed/"+testNs.Name+"/"+ph.Name,
			bytes.NewReader(bigBody))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+tokenPlain)

		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()

		Expect(resp.StatusCode).To(Equal(http.StatusAccepted),
			"oversized body must not fail; body is advisory only")
	})

	It("accepts a garbage (non-JSON) body and still returns 202 for valid bearer", func() {
		mux, _ := buildProvisionFailedMux()
		srv := httptest.NewServer(mux)
		defer srv.Close()

		req, err := http.NewRequest(http.MethodPost,
			srv.URL+"/api/v1/provision-failed/"+testNs.Name+"/"+ph.Name,
			bytes.NewBufferString("THIS IS NOT JSON !!!"))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+tokenPlain)

		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()

		Expect(resp.StatusCode).To(Equal(http.StatusAccepted),
			"non-JSON body must not fail; body is advisory only")

		By("Verifying annotation uses the generic reason when body is unparseable")
		updated := &infrastructurev1beta1.PhysicalHost{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ph.Name, Namespace: testNs.Name}, updated)).To(Succeed())
		Expect(updated.Annotations).To(HaveKeyWithValue(ProvisionFailedRequestAnnotation, provisionFailedReasonGeneric))
	})

	It("does NOT set annotation when host is NOT in StateDeploying", func() {
		// Move host to a non-Deploying state (e.g. StateInspecting).
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ph.Name, Namespace: testNs.Name}, ph)).To(Succeed())
		ph.Status.State = infrastructurev1beta1.StateInspecting
		Expect(k8sClient.Status().Update(ctx, ph)).To(Succeed())

		mux, _ := buildProvisionFailedMux()
		srv := httptest.NewServer(mux)
		defer srv.Close()

		req, err := http.NewRequest(http.MethodPost,
			srv.URL+"/api/v1/provision-failed/"+testNs.Name+"/"+ph.Name,
			bytes.NewBufferString(`{"reason":"fail"}`))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+tokenPlain)

		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()

		Expect(resp.StatusCode).To(Equal(http.StatusAccepted),
			"handler returns 202 even when host not in Deploying (no-op, not an error)")

		By("Verifying annotation was NOT set on a non-Deploying host")
		updated := &infrastructurev1beta1.PhysicalHost{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ph.Name, Namespace: testNs.Name}, updated)).To(Succeed())
		Expect(updated.Annotations).NotTo(HaveKey(ProvisionFailedRequestAnnotation),
			"must not set annotation on non-Deploying host")
	})
})

// ── PhysicalHost: applyProvisionFailedRequestAnnotation ──────────────────────

var _ = Describe("v4.1 PhysicalHost applyProvisionFailedRequestAnnotation", func() {
	var testNs *corev1.Namespace

	BeforeEach(func() {
		testNs = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "d016-ph-"}}
		Expect(k8sClient.Create(ctx, testNs)).To(Succeed())
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, testNs)).To(Succeed())
	})

	It("transitions Deploying→Error and sets ErrorMessage from annotation value", func() {
		errorMsg := provisionFailedReasonPrefix + "image digest mismatch"
		ph := &infrastructurev1beta1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "host-pf-deploying",
				Namespace: testNs.Name,
				Annotations: map[string]string{
					ProvisionFailedRequestAnnotation: errorMsg,
				},
			},
			Spec: infrastructurev1beta1.PhysicalHostSpec{
				RedfishConnection: infrastructurev1beta1.RedfishConnection{
					Address:              "https://192.168.2.220",
					CredentialsSecretRef: "dummy-creds",
				},
			},
		}
		Expect(k8sClient.Create(ctx, ph)).To(Succeed())
		ph.Status.State = infrastructurev1beta1.StateDeploying
		Expect(k8sClient.Status().Update(ctx, ph)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ph.Name, Namespace: testNs.Name}, ph)).To(Succeed())

		r := &PhysicalHostReconciler{
			Client: k8sClient,
			Log:    ctrl.Log.WithName("d016-ph-test"),
			Scheme: k8sClient.Scheme(),
		}
		r.applyProvisionFailedRequestAnnotation(r.Log, ph)

		By("Verifying State == Error")
		Expect(ph.Status.State).To(Equal(infrastructurev1beta1.StateError))

		By("Verifying ErrorMessage is set to the annotation value")
		Expect(ph.Status.ErrorMessage).To(Equal(errorMsg))

		By("Verifying Ready is false")
		Expect(ph.Status.Ready).To(BeFalse())

		By("Verifying annotation was cleared")
		Expect(ph.Annotations).NotTo(HaveKey(ProvisionFailedRequestAnnotation))
	})

	It("does NOT transition when host is NOT in StateDeploying", func() {
		errorMsg := provisionFailedReasonPrefix + "some error"
		ph := &infrastructurev1beta1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "host-pf-notdeploying",
				Namespace: testNs.Name,
				Annotations: map[string]string{
					ProvisionFailedRequestAnnotation: errorMsg,
				},
			},
			Spec: infrastructurev1beta1.PhysicalHostSpec{
				RedfishConnection: infrastructurev1beta1.RedfishConnection{
					Address:              "https://192.168.2.221",
					CredentialsSecretRef: "dummy-creds",
				},
			},
		}
		Expect(k8sClient.Create(ctx, ph)).To(Succeed())
		ph.Status.State = infrastructurev1beta1.StateReady // not Deploying
		Expect(k8sClient.Status().Update(ctx, ph)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ph.Name, Namespace: testNs.Name}, ph)).To(Succeed())

		r := &PhysicalHostReconciler{
			Client: k8sClient,
			Log:    ctrl.Log.WithName("d016-notdeploying-test"),
			Scheme: k8sClient.Scheme(),
		}
		r.applyProvisionFailedRequestAnnotation(r.Log, ph)

		By("Verifying State remains Ready (no transition)")
		Expect(ph.Status.State).To(Equal(infrastructurev1beta1.StateReady),
			"host not in Deploying must not be transitioned to Error")

		By("Verifying annotation was cleared")
		Expect(ph.Annotations).NotTo(HaveKey(ProvisionFailedRequestAnnotation),
			"annotation must be cleared even when state guard fires")
	})

	It("clears annotation idempotently when host is already in StateError", func() {
		errorMsg := provisionFailedReasonPrefix + "already failed"
		ph := &infrastructurev1beta1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "host-pf-already-error",
				Namespace: testNs.Name,
				Annotations: map[string]string{
					ProvisionFailedRequestAnnotation: errorMsg,
				},
			},
			Spec: infrastructurev1beta1.PhysicalHostSpec{
				RedfishConnection: infrastructurev1beta1.RedfishConnection{
					Address:              "https://192.168.2.222",
					CredentialsSecretRef: "dummy-creds",
				},
			},
		}
		Expect(k8sClient.Create(ctx, ph)).To(Succeed())
		ph.Status.State = infrastructurev1beta1.StateError
		ph.Status.ErrorMessage = errorMsg
		Expect(k8sClient.Status().Update(ctx, ph)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ph.Name, Namespace: testNs.Name}, ph)).To(Succeed())

		r := &PhysicalHostReconciler{
			Client: k8sClient,
			Log:    ctrl.Log.WithName("d016-already-error-test"),
			Scheme: k8sClient.Scheme(),
		}
		r.applyProvisionFailedRequestAnnotation(r.Log, ph)

		By("Verifying State remains Error (no double-transition)")
		Expect(ph.Status.State).To(Equal(infrastructurev1beta1.StateError))
		Expect(ph.Status.ErrorMessage).To(Equal(errorMsg))

		By("Verifying annotation was cleared")
		Expect(ph.Annotations).NotTo(HaveKey(ProvisionFailedRequestAnnotation))
	})
})

// ── sanitizeFailureReason unit tests ─────────────────────────────────────────

var _ = Describe("v4.1 sanitizeFailureReason", func() {
	It("returns generic message for empty input", func() {
		Expect(sanitizeFailureReason("")).To(Equal(provisionFailedReasonGeneric))
	})

	It("returns generic message for whitespace-only input", func() {
		Expect(sanitizeFailureReason("   \t  ")).To(Equal(provisionFailedReasonGeneric))
	})

	It("prefixes a normal reason with provisionFailedReasonPrefix", func() {
		result := sanitizeFailureReason("image fetch failed")
		Expect(result).To(Equal(provisionFailedReasonPrefix + "image fetch failed"))
	})

	It("strips newlines from the reason", func() {
		result := sanitizeFailureReason("line1\nline2\r\nline3")
		Expect(result).NotTo(ContainSubstring("\n"))
		Expect(result).NotTo(ContainSubstring("\r"))
		Expect(result).To(ContainSubstring("line1"))
		Expect(result).To(ContainSubstring("line2"))
		Expect(result).To(ContainSubstring("line3"))
	})

	It("strips ASCII control characters", func() {
		// Bell (\a), NUL (\x00), unit separator (\x1f)
		result := sanitizeFailureReason("bad\x00char\ahere\x1f")
		Expect(result).NotTo(ContainSubstring("\x00"))
		Expect(result).NotTo(ContainSubstring("\a"))
		Expect(result).NotTo(ContainSubstring("\x1f"))
		Expect(result).To(ContainSubstring("bad"))
		Expect(result).To(ContainSubstring("char"))
		Expect(result).To(ContainSubstring("here"))
	})

	It("caps reason to provisionFailedReasonMaxLen before prefixing", func() {
		longReason := strings.Repeat("x", provisionFailedReasonMaxLen+100)
		result := sanitizeFailureReason(longReason)
		// The capped reason must be exactly provisionFailedReasonMaxLen chars of 'x',
		// plus the prefix.
		expectedCapped := provisionFailedReasonPrefix + strings.Repeat("x", provisionFailedReasonMaxLen)
		Expect(result).To(Equal(expectedCapped))
	})

	It("preserves a short reason exactly (minus whitespace trim)", func() {
		result := sanitizeFailureReason("  disk write error  ")
		Expect(result).To(Equal(provisionFailedReasonPrefix + "disk write error"))
	})
})

// ── Beskar7Machine StateError → DeploymentFailed reason ──────────────────────

var _ = Describe("v4.1 Beskar7Machine StateError deploy-failure path", func() {
	var (
		testNs *corev1.Namespace
		mockRf *internalredfish.MockClient
		r      *Beskar7MachineReconciler
	)

	BeforeEach(func() {
		testNs = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "d016-b7m-"}}
		Expect(k8sClient.Create(ctx, testNs)).To(Succeed())

		credSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "bmc-creds-d016", Namespace: testNs.Name},
			Data: map[string][]byte{
				"username": []byte("admin"),
				"password": []byte("secret"),
			},
		}
		Expect(k8sClient.Create(ctx, credSecret)).To(Succeed())

		mockRf = internalredfish.NewMockClient()
		r = &Beskar7MachineReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			Log:    ctrl.Log.WithName("d016-b7m-test"),
			RedfishClientFactory: func(_ context.Context, _, _, _ string, _ bool, _ []byte) (internalredfish.Client, error) {
				return mockRf, nil
			},
			BootstrapURLBase: "https://example.com:8082",
		}
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, testNs)).To(Succeed())
	})

	It("marks terminal failure with DeploymentFailedReason when ErrorMessage has the inspector prefix", func() {
		deployFailMsg := provisionFailedReasonPrefix + "COS_OEM partition not found"

		ph := &infrastructurev1beta1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{Name: "host-b7m-pfail", Namespace: testNs.Name},
			Spec: infrastructurev1beta1.PhysicalHostSpec{
				RedfishConnection: infrastructurev1beta1.RedfishConnection{
					Address:              "https://192.168.2.230",
					CredentialsSecretRef: "bmc-creds-d016",
				},
			},
		}
		Expect(k8sClient.Create(ctx, ph)).To(Succeed())
		ph.Status.State = infrastructurev1beta1.StateError
		ph.Status.ErrorMessage = deployFailMsg
		Expect(k8sClient.Status().Update(ctx, ph)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ph.Name, Namespace: testNs.Name}, ph)).To(Succeed())

		b7m := &infrastructurev1beta1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "b7m-pfail", Namespace: testNs.Name},
			Spec: infrastructurev1beta1.Beskar7MachineSpec{
				InspectionImageURL: "http://boot/inspect.ipxe",
				TargetImageURL:     "http://boot/kairos.raw",
				TargetImageDigest:  bootTestDigest,
			},
		}

		result, err := r.handlePhysicalHostState(ctx, r.Log, b7m, ph)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(ctrl.Result{}), "terminal failure must not requeue")

		By("Verifying FailureReason == DeploymentFailedReason")
		Expect(b7m.Status.FailureReason).NotTo(BeNil())
		Expect(*b7m.Status.FailureReason).To(Equal(infrastructurev1beta1.DeploymentFailedReason))

		By("Verifying FailureMessage is set and contains the error detail")
		Expect(b7m.Status.FailureMessage).NotTo(BeNil())
		Expect(*b7m.Status.FailureMessage).To(ContainSubstring("COS_OEM partition not found"))

		By("Verifying Phase == Failed and Ready == false")
		Expect(b7m.Status.Phase).NotTo(BeNil())
		Expect(*b7m.Status.Phase).To(Equal("Failed"))
		Expect(b7m.Status.Ready).To(BeFalse())

		By("Verifying InfrastructureReadyCondition is False with DeploymentFailedReason")
		cond := conditions.Get(b7m, infrastructurev1beta1.InfrastructureReadyCondition)
		Expect(cond).NotTo(BeNil(), "InfrastructureReadyCondition must be set")
		Expect(cond.Status).To(Equal(corev1.ConditionFalse))
		Expect(cond.Reason).To(Equal(infrastructurev1beta1.DeploymentFailedReason))
	})

	It("marks terminal failure with PhysicalHostErrorReason when ErrorMessage lacks the inspector prefix", func() {
		// A Redfish/BMC error has no prefix from the inspector.
		bmcErr := "Redfish connection failed: timeout"

		ph := &infrastructurev1beta1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{Name: "host-b7m-bmcerr", Namespace: testNs.Name},
			Spec: infrastructurev1beta1.PhysicalHostSpec{
				RedfishConnection: infrastructurev1beta1.RedfishConnection{
					Address:              "https://192.168.2.231",
					CredentialsSecretRef: "bmc-creds-d016",
				},
			},
		}
		Expect(k8sClient.Create(ctx, ph)).To(Succeed())
		ph.Status.State = infrastructurev1beta1.StateError
		ph.Status.ErrorMessage = bmcErr
		Expect(k8sClient.Status().Update(ctx, ph)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ph.Name, Namespace: testNs.Name}, ph)).To(Succeed())

		b7m := &infrastructurev1beta1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "b7m-bmcerr", Namespace: testNs.Name},
			Spec: infrastructurev1beta1.Beskar7MachineSpec{
				InspectionImageURL: "http://boot/inspect.ipxe",
				TargetImageURL:     "http://boot/kairos.raw",
				TargetImageDigest:  bootTestDigest,
			},
		}

		result, err := r.handlePhysicalHostState(ctx, r.Log, b7m, ph)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(ctrl.Result{}))

		By("Verifying FailureReason == PhysicalHostErrorReason (not DeploymentFailed)")
		Expect(b7m.Status.FailureReason).NotTo(BeNil())
		Expect(*b7m.Status.FailureReason).To(Equal(infrastructurev1beta1.PhysicalHostErrorReason))

		By("Verifying InfrastructureReadyCondition has PhysicalHostErrorReason")
		cond := conditions.Get(b7m, infrastructurev1beta1.InfrastructureReadyCondition)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal(infrastructurev1beta1.PhysicalHostErrorReason))
	})
})

// ── Beskar7Machine markTerminalFailure with DeploymentFailed ─────────────────

var _ = Describe("v4.1 Beskar7Machine markTerminalFailure DeploymentFailed", func() {
	It("sets FailureReason, FailureMessage, Phase=Failed, Ready=false, and condition", func() {
		b7m := &infrastructurev1beta1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "b7m-terminal", Namespace: "default"},
		}
		r := &Beskar7MachineReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			Log:    ctrl.Log.WithName("d016-terminal-test"),
		}
		msg := "inspector reported deploy failure: disk write I/O error"
		r.markTerminalFailure(b7m, infrastructurev1beta1.DeploymentFailedReason, msg)

		Expect(b7m.Status.FailureReason).NotTo(BeNil())
		Expect(*b7m.Status.FailureReason).To(Equal(infrastructurev1beta1.DeploymentFailedReason))
		Expect(b7m.Status.FailureMessage).NotTo(BeNil())
		Expect(*b7m.Status.FailureMessage).To(Equal(msg))
		Expect(b7m.Status.Phase).NotTo(BeNil())
		Expect(*b7m.Status.Phase).To(Equal("Failed"))
		Expect(b7m.Status.Ready).To(BeFalse())

		cond := conditions.Get(b7m, infrastructurev1beta1.InfrastructureReadyCondition)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(corev1.ConditionFalse))
		Expect(cond.Reason).To(Equal(infrastructurev1beta1.DeploymentFailedReason))
		Expect(cond.Severity).To(Equal(clusterv1.ConditionSeverityError))
	})
})
