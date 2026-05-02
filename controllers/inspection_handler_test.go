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
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	infrastructurev1beta1 "github.com/projectbeskar/beskar7/api/v1beta1"
	"github.com/projectbeskar/beskar7/internal/auth"
)

// buildInspectionMux wires the inspection handler exactly as SetupCallbackServer
// does, but bound to an httptest.Server we can drive directly. We avoid calling
// SetupCallbackServer here because it requires a real cert dir; the handler
// behavior under test is everything below TLS.
func buildInspectionMux() (*http.ServeMux, *InspectionHandler) {
	log := ctrl.Log.WithName("inspection-handler-test")
	handler := &InspectionHandler{
		Client: k8sClient,
		Log:    log,
	}
	verifier := newBearerTokenVerifier(k8sClient, log)
	mux := http.NewServeMux()
	mux.Handle("POST /api/v1/inspection/{namespace}/{hostName}",
		auth.RequireBearer(log, verifier, handler))
	return mux, handler
}

// makeReportBody returns a JSON-encoded inspection report payload of the given
// minimum size. Used for the body-cap test.
func makeReportBody(minBytes int) []byte {
	// Pad SerialNumber to inflate the body without needing a huge slice.
	pad := strings.Repeat("x", minBytes)
	body, err := json.Marshal(InspectionReportRequest{
		SerialNumber: pad,
		Manufacturer: "Test",
		Model:        "TestModel",
	})
	if err != nil {
		panic(err)
	}
	return body
}

var _ = Describe("Inspection HTTP handler (PR-5.2)", func() {
	const (
		Timeout  = time.Second * 10
		Interval = time.Millisecond * 250
	)

	var (
		testNs       *corev1.Namespace
		physicalHost *infrastructurev1beta1.PhysicalHost
		server       *httptest.Server
	)

	BeforeEach(func() {
		testNs = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "insp-handler-test-"},
		}
		Expect(k8sClient.Create(ctx, testNs)).To(Succeed())

		physicalHost = &infrastructurev1beta1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "insp-handler-host",
				Namespace: testNs.Name,
			},
			Spec: infrastructurev1beta1.PhysicalHostSpec{
				RedfishConnection: infrastructurev1beta1.RedfishConnection{
					Address:              "https://192.168.1.10",
					CredentialsSecretRef: "irrelevant",
				},
			},
		}
		Expect(k8sClient.Create(ctx, physicalHost)).To(Succeed())

		mux, _ := buildInspectionMux()
		server = httptest.NewServer(mux)
	})

	AfterEach(func() {
		server.Close()
		Expect(k8sClient.Delete(ctx, testNs)).To(Succeed())
	})

	postReport := func(token string, body []byte) *http.Response {
		req, err := http.NewRequest(http.MethodPost,
			fmt.Sprintf("%s/api/v1/inspection/%s/%s", server.URL, physicalHost.Namespace, physicalHost.Name),
			bytes.NewReader(body))
		Expect(err).NotTo(HaveOccurred())
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		return resp
	}

	setHostBootstrap := func(hash string, expiresIn time.Duration) {
		// Re-fetch to pick up any concurrent status writes.
		ph := &infrastructurev1beta1.PhysicalHost{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: physicalHost.Name, Namespace: physicalHost.Namespace}, ph)).To(Succeed())
		issuedAt := metav1.NewTime(time.Now())
		expiresAt := metav1.NewTime(issuedAt.Add(expiresIn))
		ph.Status.Bootstrap = &infrastructurev1beta1.BootstrapStatus{
			TokenHash: hash,
			IssuedAt:  &issuedAt,
			ExpiresAt: &expiresAt,
		}
		Expect(k8sClient.Status().Update(ctx, ph)).To(Succeed())
	}

	It("rejects POST without a bearer token (401)", func() {
		body := makeReportBody(0)
		resp := postReport("", body)
		defer func() { _ = resp.Body.Close() }()

		Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized))
	})

	It("rejects POST when no token has been issued for the host (401)", func() {
		body := makeReportBody(0)
		resp := postReport("anything", body)
		defer func() { _ = resp.Body.Close() }()

		Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized),
			"host has no Status.Bootstrap.TokenHash → must reject any presented token")
	})

	It("rejects POST with an expired token (401)", func() {
		plaintext, hash, err := auth.MintToken()
		Expect(err).NotTo(HaveOccurred())
		// Set ExpiresAt 1 hour in the past.
		setHostBootstrap(hash, -1*time.Hour)

		body := makeReportBody(0)
		resp := postReport(plaintext, body)
		defer func() { _ = resp.Body.Close() }()

		Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized),
			"expired ExpiresAt must reject the token")
	})

	It("rejects POST with the wrong token (401)", func() {
		_, hash, err := auth.MintToken()
		Expect(err).NotTo(HaveOccurred())
		setHostBootstrap(hash, 30*time.Minute)

		// Present a different plaintext.
		body := makeReportBody(0)
		resp := postReport("not-the-token", body)
		defer func() { _ = resp.Body.Close() }()

		Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized))
	})

	It("accepts a valid token, creates a result ConfigMap, and sets the inspection-result annotation (202)", func() {
		plaintext, hash, err := auth.MintToken()
		Expect(err).NotTo(HaveOccurred())
		setHostBootstrap(hash, 30*time.Minute)

		// Body with some structure so we can verify the round-trip.
		payload, err := json.Marshal(InspectionReportRequest{
			Manufacturer: "Acme",
			Model:        "Test-7000",
			SerialNumber: "SN-12345",
			CPUs:         []CPUData{{ID: "cpu0", Cores: 8, Threads: 16}},
			Memory:       []MemData{{ID: "DIMM0", Capacity: "32GiB"}},
		})
		Expect(err).NotTo(HaveOccurred())

		resp := postReport(plaintext, payload)
		defer func() { _ = resp.Body.Close() }()

		Expect(resp.StatusCode).To(Equal(http.StatusAccepted),
			"valid token + valid body must return 202 (handler does not write Status itself)")

		By("Verifying the result ConfigMap was created with the report.json key")
		cmName := inspectionResultConfigMapName(physicalHost.Name)
		Eventually(func(g Gomega) {
			cm := &corev1.ConfigMap{}
			g.Expect(k8sClient.Get(ctx,
				types.NamespacedName{Namespace: physicalHost.Namespace, Name: cmName}, cm)).To(Succeed())
			g.Expect(cm.Data).To(HaveKey(inspectionResultDataKey))
			// Decode and check a couple of fields.
			report := &infrastructurev1beta1.InspectionReport{}
			g.Expect(json.Unmarshal([]byte(cm.Data[inspectionResultDataKey]), report)).To(Succeed())
			g.Expect(report.Manufacturer).To(Equal("Acme"))
			g.Expect(report.Model).To(Equal("Test-7000"))
			g.Expect(report.SerialNumber).To(Equal("SN-12345"))
			g.Expect(report.CPUs).To(HaveLen(1))
			g.Expect(report.CPUs[0].Cores).To(Equal(8))
			// Owner reference back to PhysicalHost so the CM is GC'd on host delete.
			g.Expect(cm.OwnerReferences).NotTo(BeEmpty())
			g.Expect(cm.OwnerReferences[0].Kind).To(Equal("PhysicalHost"))
			g.Expect(cm.OwnerReferences[0].Name).To(Equal(physicalHost.Name))
			// Labels.
			g.Expect(cm.Labels).To(HaveKeyWithValue(inspectionResultLabelOwnedBy, "beskar7-controller-manager"))
			g.Expect(cm.Labels).To(HaveKeyWithValue(inspectionResultLabelHost, physicalHost.Name))
		}, Timeout, Interval).Should(Succeed())

		By("Verifying the inspection-result annotation was set on the PhysicalHost")
		Eventually(func(g Gomega) {
			got := &infrastructurev1beta1.PhysicalHost{}
			g.Expect(k8sClient.Get(ctx,
				types.NamespacedName{Name: physicalHost.Name, Namespace: physicalHost.Namespace}, got)).To(Succeed())
			g.Expect(got.Annotations).To(HaveKeyWithValue(InspectionResultAnnotation, cmName))
		}, Timeout, Interval).Should(Succeed())

		By("Verifying the handler did NOT write Status (D-005 invariant)")
		got := &infrastructurev1beta1.PhysicalHost{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: physicalHost.Name, Namespace: physicalHost.Namespace}, got)).To(Succeed())
		Expect(got.Status.InspectionReport).To(BeNil(),
			"handler must not have populated Status.InspectionReport — that is the PhysicalHost reconciler's job")
		Expect(got.Status.InspectionPhase).NotTo(Equal(infrastructurev1beta1.InspectionPhaseComplete),
			"handler must not have transitioned InspectionPhase")
	})

	It("rejects a body larger than the 1 MiB cap (413)", func() {
		plaintext, hash, err := auth.MintToken()
		Expect(err).NotTo(HaveOccurred())
		setHostBootstrap(hash, 30*time.Minute)

		// 2 MiB of padding — well above the 1 MiB cap.
		body := makeReportBody(2 << 20)
		Expect(len(body) > inspectionMaxBodyBytes).To(BeTrue(),
			"sanity: test must produce a body larger than the cap")

		resp := postReport(plaintext, body)
		defer func() { _ = resp.Body.Close() }()

		Expect(resp.StatusCode).To(Equal(http.StatusRequestEntityTooLarge),
			"oversized body must be rejected with 413, not silently truncated")
	})
})
