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
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrastructurev1beta1 "github.com/projectbeskar/beskar7/api/v1beta1"
	"github.com/projectbeskar/beskar7/internal/auth"
)

// ── helpers ──────────────────────────────────────────────────────────────────

const (
	bootTestAPIBase = "https://beskar7.example.com:8082"
	bootTestCABytes = "FAKE-CA-PEM-DATA"

	// bootTestDigest is a realistic sha256 digest used in fixtures.
	// The 64-char hex string below is arbitrary but correctly formed.
	bootTestDigest = "sha256:a3b4c5d6e7f80102030405060708090a0b0c0d0e0f101112131415161718191a"
)

// buildBootMux wires a BootHandler onto a ServeMux using the same route pattern
// as SetupCallbackServer. Tests drive it via httptest.Server.
func buildBootMux(cfg BootHandlerConfig) *http.ServeMux {
	log := ctrl.Log.WithName("boot-handler-test")
	handler := &BootHandler{
		Client: k8sClient,
		Log:    log,
		Config: cfg,
	}
	mux := http.NewServeMux()
	mux.Handle("GET /api/v1/boot/{namespace}/{hostName}/{nonce}", handler)
	return mux
}

// bootTestConfig returns a BootHandlerConfig suitable for tests.
func bootTestConfig() BootHandlerConfig {
	return BootHandlerConfig{
		APIBase: bootTestAPIBase,
		CABytes: []byte(bootTestCABytes),
	}
}

// ── fixtures ─────────────────────────────────────────────────────────────────

// bootTestFixture creates the full set of Kubernetes objects needed for a
// happy-path /boot test:
//   - PhysicalHost in testNs
//   - Beskar7Machine (consumer) in testNs with InspectionImageURL + TargetImageURL set
//   - ConsumerRef linking the host to the machine
//   - bootstrap-token Secret holding plaintext-token
//
// Returns (physicalHost, b7machine, bearerTokenPlaintext, noncePlaintext).
// The caller is responsible for minting and storing the boot nonce on the host's
// Status.Bootstrap (use setHostBootNonce).
func bootTestFixture(testNs string) (
	*infrastructurev1beta1.PhysicalHost,
	*infrastructurev1beta1.Beskar7Machine,
	string, /* bearerToken plaintext */
	string, /* nonce plaintext */
) {
	By("creating PhysicalHost")
	ph := &infrastructurev1beta1.PhysicalHost{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "boot-handler-host",
			Namespace: testNs,
		},
		Spec: infrastructurev1beta1.PhysicalHostSpec{
			RedfishConnection: infrastructurev1beta1.RedfishConnection{
				Address:              "https://192.168.99.1",
				CredentialsSecretRef: "irrelevant",
			},
		},
	}
	Expect(k8sClient.Create(ctx, ph)).To(Succeed())

	By("creating Beskar7Machine (consumer)")
	b7m := &infrastructurev1beta1.Beskar7Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "boot-handler-b7m",
			Namespace: testNs,
		},
		Spec: infrastructurev1beta1.Beskar7MachineSpec{
			InspectionImageURL: "https://boot.example.com/inspect",
			TargetImageURL:     "https://boot.example.com/kairos.tar.gz",
			TargetImageDigest:  bootTestDigest,
		},
	}
	Expect(k8sClient.Create(ctx, b7m)).To(Succeed())

	By("linking ConsumerRef on PhysicalHost")
	freshPH := &infrastructurev1beta1.PhysicalHost{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ph.Name, Namespace: testNs}, freshPH)).To(Succeed())
	base := freshPH.DeepCopy()
	freshPH.Spec.ConsumerRef = &corev1.ObjectReference{
		Kind:       "Beskar7Machine",
		APIVersion: InfrastructureAPIVersion,
		Name:       b7m.Name,
		Namespace:  b7m.Namespace,
		UID:        b7m.UID,
	}
	Expect(k8sClient.Patch(ctx, freshPH, client.MergeFrom(base))).To(Succeed())

	By("minting bearer token and creating bootstrap-token Secret")
	bearerPlaintext, bearerHash, err := auth.MintToken()
	Expect(err).NotTo(HaveOccurred())

	noncePlaintext, nonceHash, err := auth.MintToken()
	Expect(err).NotTo(HaveOccurred())

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      bootstrapTokenSecretName(ph.Name),
			Namespace: testNs,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"plaintext-token":      []byte(bearerPlaintext),
			"plaintext-boot-nonce": []byte(noncePlaintext),
		},
	}
	Expect(k8sClient.Create(ctx, secret)).To(Succeed())

	By("writing BootNonceHash + BootNonceExpiresAt to PhysicalHost Status.Bootstrap")
	setHostBootNonce(freshPH.Name, testNs, nonceHash, bearerHash, 10*time.Minute)

	// Return fresh copies so callers hold the latest resourceVersion.
	gotPH := &infrastructurev1beta1.PhysicalHost{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ph.Name, Namespace: testNs}, gotPH)).To(Succeed())

	return gotPH, b7m, bearerPlaintext, noncePlaintext
}

// setHostBootNonce writes BootNonceHash, BootNonceExpiresAt, and TokenHash to
// the host's Status.Bootstrap via Status().Update. Call after the host exists.
func setHostBootNonce(hostName, ns, nonceHash, tokenHash string, ttl time.Duration) {
	ph := &infrastructurev1beta1.PhysicalHost{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: hostName, Namespace: ns}, ph)).To(Succeed())
	expiresAt := metav1.NewTime(time.Now().Add(ttl))
	issuedAt := metav1.NewTime(time.Now())
	tokenExpiresAt := metav1.NewTime(time.Now().Add(30 * time.Minute))
	ph.Status.Bootstrap = &infrastructurev1beta1.BootstrapStatus{
		TokenHash:          tokenHash,
		IssuedAt:           &issuedAt,
		ExpiresAt:          &tokenExpiresAt,
		BootNonceHash:      nonceHash,
		BootNonceExpiresAt: &expiresAt,
	}
	Expect(k8sClient.Status().Update(ctx, ph)).To(Succeed())
}

// doBoot issues GET /api/v1/boot/{namespace}/{host}/{nonce} against server.
func doBoot(serverURL, namespace, hostName, nonce string) *http.Response {
	url := fmt.Sprintf("%s/api/v1/boot/%s/%s/%s", serverURL, namespace, hostName, nonce)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, bytes.NewReader(nil))
	Expect(err).NotTo(HaveOccurred())
	resp, err := http.DefaultClient.Do(req)
	Expect(err).NotTo(HaveOccurred())
	return resp
}

// readBody reads and closes the response body, returning a string.
func readBody(resp *http.Response) string {
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	Expect(err).NotTo(HaveOccurred())
	return string(b)
}

// ── specs ─────────────────────────────────────────────────────────────────────

var _ = Describe("Boot GET handler (D-009 / D-010)", func() {
	const (
		Timeout  = 10 * time.Second
		Interval = 250 * time.Millisecond
	)

	var (
		testNs *corev1.Namespace
		server *httptest.Server
	)

	BeforeEach(func() {
		testNs = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "boot-handler-test-"},
		}
		Expect(k8sClient.Create(ctx, testNs)).To(Succeed())

		server = httptest.NewServer(buildBootMux(bootTestConfig()))
	})

	AfterEach(func() {
		server.Close()
		Expect(k8sClient.Delete(ctx, testNs)).To(Succeed())
	})

	// ── 1. Happy path ──────────────────────────────────────────────────────

	It("happy path: valid fresh nonce → 200 with correct iPXE script, ConsumedAt set", func() {
		ph, b7m, _, nonce := bootTestFixture(testNs.Name)

		resp := doBoot(server.URL, testNs.Name, ph.Name, nonce)
		body := readBody(resp)
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		Expect(resp.Header.Get("Content-Type")).To(Equal("text/plain"))

		By("asserting iPXE script structure")
		Expect(body).To(HavePrefix("#!ipxe\n"))
		Expect(body).To(ContainSubstring(b7m.Spec.InspectionImageURL + "/vmlinuz"))
		Expect(body).To(ContainSubstring("beskar7.api=" + bootTestAPIBase))
		Expect(body).To(ContainSubstring("beskar7.namespace=" + testNs.Name))
		Expect(body).To(ContainSubstring("beskar7.host=" + ph.Name))
		Expect(body).To(ContainSubstring("beskar7.token="))
		Expect(body).To(ContainSubstring("beskar7.target=" + b7m.Spec.TargetImageURL))
		Expect(body).To(ContainSubstring("beskar7.target-digest=" + b7m.Spec.TargetImageDigest))
		Expect(body).To(ContainSubstring("beskar7.ca="))
		Expect(body).To(ContainSubstring("initrd " + b7m.Spec.InspectionImageURL + "/initrd.img"))
		Expect(body).To(ContainSubstring("\nboot\n"))

		By("asserting beskar7.target-digest appears between beskar7.target and beskar7.ca (contract §4.1 ordering)")
		targetIdx := strings.Index(body, "beskar7.target=")
		digestIdx := strings.Index(body, "beskar7.target-digest=")
		caIdx := strings.Index(body, "beskar7.ca=")
		Expect(targetIdx).To(BeNumerically("<", digestIdx),
			"beskar7.target must precede beskar7.target-digest")
		Expect(digestIdx).To(BeNumerically("<", caIdx),
			"beskar7.target-digest must precede beskar7.ca")

		By("asserting BootNonceConsumedAt is set")
		Eventually(func(g Gomega) {
			got := &infrastructurev1beta1.PhysicalHost{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ph.Name, Namespace: testNs.Name}, got)).To(Succeed())
			g.Expect(got.Status.Bootstrap).NotTo(BeNil())
			g.Expect(got.Status.Bootstrap.BootNonceConsumedAt).NotTo(BeNil(),
				"BootNonceConsumedAt must be set after first /boot fetch")
		}, Timeout, Interval).Should(Succeed())
	})

	// ── 2. Single-use under concurrency (load-bearing) ────────────────────

	It("single-use under concurrency: two goroutines, both 200, identical body, ConsumedAt set once", func() {
		ph, _, _, nonce := bootTestFixture(testNs.Name)

		const workers = 2
		results := make([]struct {
			status int
			body   string
		}, workers)

		var wg sync.WaitGroup
		for i := 0; i < workers; i++ {
			i := i
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer GinkgoRecover()
				resp := doBoot(server.URL, testNs.Name, ph.Name, nonce)
				b := readBody(resp)
				results[i].status = resp.StatusCode
				results[i].body = b
			}()
		}
		wg.Wait()

		By("both fetches must succeed with 200")
		for i, r := range results {
			Expect(r.status).To(Equal(http.StatusOK),
				fmt.Sprintf("goroutine %d: expected 200, got %d (body: %q)", i, r.status, r.body))
		}

		By("byte-identical bodies (§4.1 idempotency guarantee)")
		Expect(results[0].body).To(Equal(results[1].body),
			"both fetches must return byte-identical iPXE scripts")

		By("BootNonceConsumedAt set exactly once — value stable after second fetch")
		var firstConsumedAt *metav1.Time
		Eventually(func(g Gomega) {
			got := &infrastructurev1beta1.PhysicalHost{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ph.Name, Namespace: testNs.Name}, got)).To(Succeed())
			g.Expect(got.Status.Bootstrap).NotTo(BeNil())
			g.Expect(got.Status.Bootstrap.BootNonceConsumedAt).NotTo(BeNil())
			firstConsumedAt = got.Status.Bootstrap.BootNonceConsumedAt
		}, Timeout, Interval).Should(Succeed())

		// A brief pause then a third fetch confirms value stability.
		time.Sleep(50 * time.Millisecond)
		got := &infrastructurev1beta1.PhysicalHost{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ph.Name, Namespace: testNs.Name}, got)).To(Succeed())
		Expect(got.Status.Bootstrap.BootNonceConsumedAt.Time).To(BeTemporally("==", firstConsumedAt.Time),
			"BootNonceConsumedAt must not be advanced by subsequent fetches")
	})

	// ── 3. Already-consumed retry ──────────────────────────────────────────

	It("already-consumed: identical render, no second patch, ConsumedAt value unchanged", func() {
		ph, b7m, _, nonce := bootTestFixture(testNs.Name)

		By("pre-consuming the nonce")
		consumedAt := metav1.NewTime(time.Now().Add(-1 * time.Second))
		freshPH := &infrastructurev1beta1.PhysicalHost{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ph.Name, Namespace: testNs.Name}, freshPH)).To(Succeed())
		freshPH.Status.Bootstrap.BootNonceConsumedAt = &consumedAt
		Expect(k8sClient.Status().Update(ctx, freshPH)).To(Succeed())

		By("issuing a /boot fetch against an already-consumed nonce")
		resp := doBoot(server.URL, testNs.Name, ph.Name, nonce)
		body := readBody(resp)
		Expect(resp.StatusCode).To(Equal(http.StatusOK))

		By("body is the correct iPXE script")
		Expect(body).To(ContainSubstring(b7m.Spec.InspectionImageURL + "/vmlinuz"))
		Expect(body).To(ContainSubstring("beskar7.token="))

		By("ConsumedAt is unchanged — same second as the pre-set value")
		got := &infrastructurev1beta1.PhysicalHost{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ph.Name, Namespace: testNs.Name}, got)).To(Succeed())
		Expect(got.Status.Bootstrap.BootNonceConsumedAt).NotTo(BeNil())
		// metav1.Time serializes to second-precision RFC3339; compare truncated.
		Expect(got.Status.Bootstrap.BootNonceConsumedAt.Truncate(time.Second)).To(
			BeTemporally("==", consumedAt.Truncate(time.Second)),
			"ConsumedAt must not be advanced by a second /boot fetch")
	})

	// ── 4. Opaque failures ─────────────────────────────────────────────────

	It("opaque 404: expired nonce", func() {
		ph, _, _, nonce := bootTestFixture(testNs.Name)

		By("overwriting the expiry to the past")
		freshPH := &infrastructurev1beta1.PhysicalHost{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ph.Name, Namespace: testNs.Name}, freshPH)).To(Succeed())
		expiredAt := metav1.NewTime(time.Now().Add(-1 * time.Hour))
		freshPH.Status.Bootstrap.BootNonceExpiresAt = &expiredAt
		Expect(k8sClient.Status().Update(ctx, freshPH)).To(Succeed())

		resp := doBoot(server.URL, testNs.Name, ph.Name, nonce)
		body := readBody(resp)
		Expect(resp.StatusCode).To(Equal(http.StatusNotFound))
		Expect(body).To(ContainSubstring(bootHandlerOpaqueFailureBody))
	})

	It("opaque 404: wrong nonce", func() {
		ph, _, _, _ := bootTestFixture(testNs.Name)
		// Use a freshly minted token as the "wrong nonce" — 256-bit entropy makes
		// collisions impossible.
		wrongNonce, _, err := auth.MintToken()
		Expect(err).NotTo(HaveOccurred())

		resp := doBoot(server.URL, testNs.Name, ph.Name, wrongNonce)
		body := readBody(resp)
		Expect(resp.StatusCode).To(Equal(http.StatusNotFound))
		Expect(body).To(ContainSubstring(bootHandlerOpaqueFailureBody))
	})

	It("opaque 404: unknown host", func() {
		resp := doBoot(server.URL, testNs.Name, "no-such-host", "some-nonce")
		body := readBody(resp)
		Expect(resp.StatusCode).To(Equal(http.StatusNotFound))
		Expect(body).To(ContainSubstring(bootHandlerOpaqueFailureBody))
	})

	It("opaque 404: no Beskar7Machine consumer (ConsumerRef nil)", func() {
		By("creating a host with no ConsumerRef")
		ph := &infrastructurev1beta1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "boot-no-consumer",
				Namespace: testNs.Name,
			},
			Spec: infrastructurev1beta1.PhysicalHostSpec{
				RedfishConnection: infrastructurev1beta1.RedfishConnection{
					Address:              "https://192.168.99.2",
					CredentialsSecretRef: "irrelevant",
				},
			},
		}
		Expect(k8sClient.Create(ctx, ph)).To(Succeed())

		noncePlaintext, nonceHash, err := auth.MintToken()
		Expect(err).NotTo(HaveOccurred())
		_, tokenHash, err := auth.MintToken()
		Expect(err).NotTo(HaveOccurred())
		setHostBootNonce(ph.Name, testNs.Name, nonceHash, tokenHash, 10*time.Minute)

		// Create the token secret (the handler reads it after resolving the
		// consumer, but we still create it to isolate the "no consumer" branch).
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      bootstrapTokenSecretName(ph.Name),
				Namespace: testNs.Name,
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{"plaintext-token": []byte("irrelevant")},
		})).To(Succeed())

		resp := doBoot(server.URL, testNs.Name, ph.Name, noncePlaintext)
		body := readBody(resp)
		Expect(resp.StatusCode).To(Equal(http.StatusNotFound))
		Expect(body).To(ContainSubstring(bootHandlerOpaqueFailureBody))
	})

	// Empty InspectionImageURL: CRD validation forbids an empty string at
	// admission time, so we use a fake client (which skips API validation) to
	// stage the object. Same technique as the oversize-bootstrap test.
	It("opaque 404: empty InspectionImageURL on Beskar7Machine (fake client)", func() {
		noncePlaintext, nonceHash, err := auth.MintToken()
		Expect(err).NotTo(HaveOccurred())
		_, tokenHash, err := auth.MintToken()
		Expect(err).NotTo(HaveOccurred())

		nonceExpiresAt := metav1.NewTime(time.Now().Add(10 * time.Minute))
		issuedAt := metav1.NewTime(time.Now())
		tokenExpiresAt := metav1.NewTime(time.Now().Add(30 * time.Minute))

		ph := &infrastructurev1beta1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{Name: "h-empty-inspect", Namespace: "n"},
			Spec: infrastructurev1beta1.PhysicalHostSpec{
				RedfishConnection: infrastructurev1beta1.RedfishConnection{
					Address: "https://192.168.1.1", CredentialsSecretRef: "x",
				},
				ConsumerRef: &corev1.ObjectReference{
					Kind:       "Beskar7Machine",
					APIVersion: InfrastructureAPIVersion,
					Name:       "b7m-empty",
					Namespace:  "n",
				},
			},
			Status: infrastructurev1beta1.PhysicalHostStatus{
				Bootstrap: &infrastructurev1beta1.BootstrapStatus{
					TokenHash:          tokenHash,
					IssuedAt:           &issuedAt,
					ExpiresAt:          &tokenExpiresAt,
					BootNonceHash:      nonceHash,
					BootNonceExpiresAt: &nonceExpiresAt,
				},
			},
		}
		// Beskar7Machine with empty InspectionImageURL (bypassing CRD validation
		// via fake client — testing the handler's own guard, not the CRD schema).
		b7mEmpty := &infrastructurev1beta1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "b7m-empty", Namespace: "n"},
			Spec: infrastructurev1beta1.Beskar7MachineSpec{
				InspectionImageURL: "", // empty — triggers the handler's guard
				TargetImageURL:     "https://boot.example.com/target.tar.gz",
			},
		}
		tokenSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: bootstrapTokenSecretName("h-empty-inspect"), Namespace: "n"},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{"plaintext-token": []byte("fake-token")},
		}
		fakeClient := fake.NewClientBuilder().
			WithScheme(k8sClient.Scheme()).
			WithObjects(b7mEmpty, tokenSecret).
			WithStatusSubresource(ph).
			WithObjects(ph).
			Build()
		// Set status directly (fake client allows this without Status().Update).
		// Re-Get to apply the Status.Bootstrap we set in the object literal above.
		// Fake client populates status from WithObjects when WithStatusSubresource is set.

		handler := &BootHandler{
			Client: fakeClient,
			Log:    ctrl.Log.WithName("boot-empty-inspect-test"),
			Config: bootTestConfig(),
		}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/boot/n/h-empty-inspect/"+noncePlaintext, nil)
		req.SetPathValue("namespace", "n")
		req.SetPathValue("hostName", "h-empty-inspect")
		req.SetPathValue("nonce", noncePlaintext)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		Expect(w.Code).To(Equal(http.StatusNotFound),
			"empty InspectionImageURL must be an opaque 404, not a 200")
		Expect(w.Body.String()).To(ContainSubstring(bootHandlerOpaqueFailureBody))
	})

	It("opaque 404: missing bootstrap-token Secret", func() {
		ph := &infrastructurev1beta1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "boot-no-secret",
				Namespace: testNs.Name,
			},
			Spec: infrastructurev1beta1.PhysicalHostSpec{
				RedfishConnection: infrastructurev1beta1.RedfishConnection{
					Address:              "https://192.168.99.4",
					CredentialsSecretRef: "irrelevant",
				},
			},
		}
		Expect(k8sClient.Create(ctx, ph)).To(Succeed())

		b7m := &infrastructurev1beta1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "boot-b7m-no-secret",
				Namespace: testNs.Name,
			},
			Spec: infrastructurev1beta1.Beskar7MachineSpec{
				InspectionImageURL: "https://boot.example.com/inspect",
				TargetImageURL:     "https://boot.example.com/target.tar.gz",
				TargetImageDigest:  bootTestDigest,
			},
		}
		Expect(k8sClient.Create(ctx, b7m)).To(Succeed())

		freshPH := &infrastructurev1beta1.PhysicalHost{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ph.Name, Namespace: testNs.Name}, freshPH)).To(Succeed())
		base := freshPH.DeepCopy()
		freshPH.Spec.ConsumerRef = &corev1.ObjectReference{
			Kind:       "Beskar7Machine",
			APIVersion: InfrastructureAPIVersion,
			Name:       b7m.Name,
			Namespace:  b7m.Namespace,
		}
		Expect(k8sClient.Patch(ctx, freshPH, client.MergeFrom(base))).To(Succeed())

		noncePlaintext, nonceHash, err := auth.MintToken()
		Expect(err).NotTo(HaveOccurred())
		_, tokenHash, err := auth.MintToken()
		Expect(err).NotTo(HaveOccurred())
		setHostBootNonce(ph.Name, testNs.Name, nonceHash, tokenHash, 10*time.Minute)
		// Deliberately do NOT create the bootstrap-token Secret.

		resp := doBoot(server.URL, testNs.Name, ph.Name, noncePlaintext)
		body := readBody(resp)
		Expect(resp.StatusCode).To(Equal(http.StatusNotFound))
		Expect(body).To(ContainSubstring(bootHandlerOpaqueFailureBody))
	})

	// ── 5. Injection rejection (C-1a / SEC-7) ────────────────────────────

	// buildInjectionFakeHandler creates a BootHandler backed by a fake client
	// containing a PhysicalHost with a valid boot nonce and a Beskar7Machine whose
	// spec fields are set by the caller. Used to test injection rejection without
	// round-tripping through CRD admission validation (which would reject the
	// malicious value before it reaches the handler).
	//
	// We define it as a local closure rather than a top-level helper so it can
	// capture the test namespace's context cleanly.
	buildInjectionFakeHandler := func(inspectionURL, targetURL string) (*BootHandler, string) {
		noncePlaintext, nonceHash, err := auth.MintToken()
		Expect(err).NotTo(HaveOccurred())
		_, tokenHash, err := auth.MintToken()
		Expect(err).NotTo(HaveOccurred())

		nonceExpiresAt := metav1.NewTime(time.Now().Add(10 * time.Minute))
		issuedAt := metav1.NewTime(time.Now())
		tokenExpiresAt := metav1.NewTime(time.Now().Add(30 * time.Minute))

		ph := &infrastructurev1beta1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{Name: "h-inject", Namespace: "n"},
			Spec: infrastructurev1beta1.PhysicalHostSpec{
				RedfishConnection: infrastructurev1beta1.RedfishConnection{
					Address: "https://192.168.1.1", CredentialsSecretRef: "x",
				},
				ConsumerRef: &corev1.ObjectReference{
					Kind:       "Beskar7Machine",
					APIVersion: InfrastructureAPIVersion,
					Name:       "b7m-inject",
					Namespace:  "n",
				},
			},
			Status: infrastructurev1beta1.PhysicalHostStatus{
				Bootstrap: &infrastructurev1beta1.BootstrapStatus{
					TokenHash:          tokenHash,
					IssuedAt:           &issuedAt,
					ExpiresAt:          &tokenExpiresAt,
					BootNonceHash:      nonceHash,
					BootNonceExpiresAt: &nonceExpiresAt,
				},
			},
		}
		b7m := &infrastructurev1beta1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "b7m-inject", Namespace: "n"},
			Spec: infrastructurev1beta1.Beskar7MachineSpec{
				InspectionImageURL: inspectionURL,
				TargetImageURL:     targetURL,
				TargetImageDigest:  bootTestDigest,
			},
		}
		tokenSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: bootstrapTokenSecretName("h-inject"), Namespace: "n"},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{"plaintext-token": []byte("fake-token")},
		}
		fakeClient := fake.NewClientBuilder().
			WithScheme(k8sClient.Scheme()).
			WithObjects(b7m, tokenSecret).
			WithStatusSubresource(ph).
			WithObjects(ph).
			Build()

		handler := &BootHandler{
			Client: fakeClient,
			Log:    ctrl.Log.WithName("boot-inject-test"),
			Config: bootTestConfig(),
		}
		return handler, noncePlaintext
	}

	// doBootDirect issues ServeHTTP directly against a handler using path values
	// set via SetPathValue, bypassing the mux routing.
	doBootDirect := func(handler *BootHandler, namespace, hostName, nonce string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/boot/"+namespace+"/"+hostName+"/"+nonce, nil)
		req.SetPathValue("namespace", namespace)
		req.SetPathValue("hostName", hostName)
		req.SetPathValue("nonce", nonce)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w
	}

	// Table-driven injection tests — one row per injection vector.
	// Each case mutates exactly one of the two URL fields; the other is kept valid.
	type injectionCase struct {
		name          string
		inspectionURL string
		targetURL     string
	}
	injectionCases := []injectionCase{
		// InspectionImageURL injection vectors
		{name: "InspectionImageURL with space", inspectionURL: "http://x/ beskar7.token=ATTACKER", targetURL: "https://ok.example.com/target"},
		{name: "InspectionImageURL with newline", inspectionURL: "http://x/\nbeskar7.token=ATTACKER", targetURL: "https://ok.example.com/target"},
		{name: "InspectionImageURL with tab", inspectionURL: "http://x/\tbeskar7.token=ATTACKER", targetURL: "https://ok.example.com/target"},
		{name: "InspectionImageURL with control char", inspectionURL: "http://x/\x01evil", targetURL: "https://ok.example.com/target"},
		{name: "InspectionImageURL with non-http scheme", inspectionURL: "file:///etc/passwd", targetURL: "https://ok.example.com/target"},
		// TargetImageURL injection vectors
		{name: "TargetImageURL with space", inspectionURL: "https://ok.example.com/inspect", targetURL: "http://x/ beskar7.api=ATTACKER"},
		{name: "TargetImageURL with newline", inspectionURL: "https://ok.example.com/inspect", targetURL: "http://x/\nbeskar7.api=ATTACKER"},
		{name: "TargetImageURL with tab", inspectionURL: "https://ok.example.com/inspect", targetURL: "http://x/\tbeskar7.api=ATTACKER"},
		{name: "TargetImageURL with control char", inspectionURL: "https://ok.example.com/inspect", targetURL: "http://x/\x01evil"},
		{name: "TargetImageURL with non-http scheme", inspectionURL: "https://ok.example.com/inspect", targetURL: "file:///etc/passwd"},
	}

	for _, tc := range injectionCases {
		tc := tc // capture range variable
		It("injection rejection (SEC-7): "+tc.name+" → opaque 404", func() {
			handler, nonce := buildInjectionFakeHandler(tc.inspectionURL, tc.targetURL)
			w := doBootDirect(handler, "n", "h-inject", nonce)

			Expect(w.Code).To(Equal(http.StatusNotFound),
				"injection attempt must yield opaque 404, not a 200 with injected script")
			Expect(w.Body.String()).To(ContainSubstring(bootHandlerOpaqueFailureBody))
			// Confirm the injected payload does not appear in the response body.
			Expect(w.Body.String()).NotTo(ContainSubstring("ATTACKER"),
				"injected payload must not appear in response body")
		})
	}

	// ── 6. Digest validation (contract §5 / §8.1 / SEC-7) ───────────────────
	//
	// validateBootDigest unit tests exercise the helper directly. Each case is
	// also an implicit integration test because renderBootScript calls it before
	// rendering; an invalid digest would yield an opaque 404 on the live handler.

	type digestCase struct {
		name    string
		digest  string
		wantErr bool
	}
	digestCases := []digestCase{
		// ── accept ──
		{
			name:    "valid lowercase sha256",
			digest:  "sha256:a3b4c5d6e7f80102030405060708090a0b0c0d0e0f101112131415161718191a",
			wantErr: false,
		},
		{
			name:    "valid sha256 all-zeros",
			digest:  "sha256:0000000000000000000000000000000000000000000000000000000000000000",
			wantErr: false,
		},
		// ── reject ──
		{
			name:    "empty string",
			digest:  "",
			wantErr: true,
		},
		{
			name:    "missing sha256: prefix",
			digest:  "a3b4c5d6e7f80102030405060708090a0b0c0d0e0f101112131415161718191a",
			wantErr: true,
		},
		{
			name:    "wrong algorithm prefix (sha512:)",
			digest:  "sha512:a3b4c5d6e7f80102030405060708090a0b0c0d0e0f101112131415161718191a",
			wantErr: true,
		},
		{
			name:    "too short (63 hex chars)",
			digest:  "sha256:a3b4c5d6e7f80102030405060708090a0b0c0d0e0f10111213141516171819",
			wantErr: true,
		},
		{
			name:    "too long (65 hex chars)",
			digest:  "sha256:a3b4c5d6e7f80102030405060708090a0b0c0d0e0f101112131415161718191a1b",
			wantErr: true,
		},
		{
			name:    "uppercase hex (non-canonical per contract §5/§8.1)",
			digest:  "sha256:A3B4C5D6E7F80102030405060708090A0B0C0D0E0F101112131415161718191A",
			wantErr: true,
		},
		{
			name:    "mixed case hex",
			digest:  "sha256:A3b4c5d6e7f80102030405060708090a0b0c0d0e0f101112131415161718191a",
			wantErr: true,
		},
		{
			name:    "embedded space (cmdline injection vector)",
			digest:  "sha256:a3b4c5d6e7f80102030405060708 beskar7.token=ATTACKER00000000000000000",
			wantErr: true,
		},
		{
			name:    "embedded newline (cmdline injection vector)",
			digest:  "sha256:a3b4c5d6e7f80102030405060708\nbeskar7.token=ATTACKER0000000000000000",
			wantErr: true,
		},
		{
			name:    "non-hex chars in digest body",
			digest:  "sha256:z3b4c5d6e7f80102030405060708090a0b0c0d0e0f101112131415161718191a",
			wantErr: true,
		},
	}

	for _, tc := range digestCases {
		tc := tc // capture range variable
		It("validateBootDigest: "+tc.name, func() {
			err := validateBootDigest(tc.digest)
			if tc.wantErr {
				Expect(err).To(HaveOccurred(),
					"expected validateBootDigest to reject %q but it accepted it", tc.digest)
			} else {
				Expect(err).NotTo(HaveOccurred(),
					"expected validateBootDigest to accept %q but it rejected: %v", tc.digest, err)
			}
		})
	}

	// ── 6b. Digest handler integration: invalid digest on Beskar7Machine → opaque 404

	It("opaque 404: invalid TargetImageDigest on Beskar7Machine (fake client)", func() {
		noncePlaintext, nonceHash, err := auth.MintToken()
		Expect(err).NotTo(HaveOccurred())
		_, tokenHash, err := auth.MintToken()
		Expect(err).NotTo(HaveOccurred())

		nonceExpiresAt := metav1.NewTime(time.Now().Add(10 * time.Minute))
		issuedAt := metav1.NewTime(time.Now())
		tokenExpiresAt := metav1.NewTime(time.Now().Add(30 * time.Minute))

		ph := &infrastructurev1beta1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{Name: "h-bad-digest", Namespace: "n"},
			Spec: infrastructurev1beta1.PhysicalHostSpec{
				RedfishConnection: infrastructurev1beta1.RedfishConnection{
					Address: "https://192.168.1.1", CredentialsSecretRef: "x",
				},
				ConsumerRef: &corev1.ObjectReference{
					Kind:       "Beskar7Machine",
					APIVersion: InfrastructureAPIVersion,
					Name:       "b7m-bad-digest",
					Namespace:  "n",
				},
			},
			Status: infrastructurev1beta1.PhysicalHostStatus{
				Bootstrap: &infrastructurev1beta1.BootstrapStatus{
					TokenHash:          tokenHash,
					IssuedAt:           &issuedAt,
					ExpiresAt:          &tokenExpiresAt,
					BootNonceHash:      nonceHash,
					BootNonceExpiresAt: &nonceExpiresAt,
				},
			},
		}
		// TargetImageDigest is intentionally malformed — uppercase hex, rejected
		// by validateBootDigest (contract §5/§8.1, SEC-7). Bypasses CRD validation
		// via fake client to test the handler's own guard.
		b7mBadDigest := &infrastructurev1beta1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "b7m-bad-digest", Namespace: "n"},
			Spec: infrastructurev1beta1.Beskar7MachineSpec{
				InspectionImageURL: "https://boot.example.com/inspect",
				TargetImageURL:     "https://boot.example.com/target.tar.gz",
				TargetImageDigest:  "sha256:A3B4C5D6E7F80102030405060708090A0B0C0D0E0F101112131415161718191A",
			},
		}
		tokenSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: bootstrapTokenSecretName("h-bad-digest"), Namespace: "n"},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{"plaintext-token": []byte("fake-token")},
		}
		fakeClient := fake.NewClientBuilder().
			WithScheme(k8sClient.Scheme()).
			WithObjects(b7mBadDigest, tokenSecret).
			WithStatusSubresource(ph).
			WithObjects(ph).
			Build()

		handler := &BootHandler{
			Client: fakeClient,
			Log:    ctrl.Log.WithName("boot-bad-digest-test"),
			Config: bootTestConfig(),
		}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/boot/n/h-bad-digest/"+noncePlaintext, nil)
		req.SetPathValue("namespace", "n")
		req.SetPathValue("hostName", "h-bad-digest")
		req.SetPathValue("nonce", noncePlaintext)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		Expect(w.Code).To(Equal(http.StatusNotFound),
			"invalid TargetImageDigest must yield an opaque 404, not a 200 with a broken script")
		Expect(w.Body.String()).To(ContainSubstring(bootHandlerOpaqueFailureBody))
	})

	// ── 7. Script length cap (SEC-10) ──────────────────────────────────────

	It("length cap (SEC-10): oversized CA → opaque 404", func() {
		// Construct a BootHandlerConfig with a CA large enough to push the
		// rendered script over bootScriptMaxBytes. The base script without the
		// CA is around 200 bytes; 4096 bytes of CA ensures we exceed the cap.
		oversizedCA := make([]byte, bootScriptMaxBytes)
		for i := range oversizedCA {
			oversizedCA[i] = 'A' // not a real PEM, but the cap check fires before parsing
		}
		largeCfg := BootHandlerConfig{
			APIBase: bootTestAPIBase,
			CABytes: oversizedCA,
		}

		noncePlaintext, nonceHash, err := auth.MintToken()
		Expect(err).NotTo(HaveOccurred())
		_, tokenHash, err := auth.MintToken()
		Expect(err).NotTo(HaveOccurred())

		nonceExpiresAt := metav1.NewTime(time.Now().Add(10 * time.Minute))
		issuedAt := metav1.NewTime(time.Now())
		tokenExpiresAt := metav1.NewTime(time.Now().Add(30 * time.Minute))

		ph := &infrastructurev1beta1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{Name: "h-large-ca", Namespace: "n"},
			Spec: infrastructurev1beta1.PhysicalHostSpec{
				RedfishConnection: infrastructurev1beta1.RedfishConnection{
					Address: "https://192.168.1.1", CredentialsSecretRef: "x",
				},
				ConsumerRef: &corev1.ObjectReference{
					Kind:       "Beskar7Machine",
					APIVersion: InfrastructureAPIVersion,
					Name:       "b7m-large-ca",
					Namespace:  "n",
				},
			},
			Status: infrastructurev1beta1.PhysicalHostStatus{
				Bootstrap: &infrastructurev1beta1.BootstrapStatus{
					TokenHash:          tokenHash,
					IssuedAt:           &issuedAt,
					ExpiresAt:          &tokenExpiresAt,
					BootNonceHash:      nonceHash,
					BootNonceExpiresAt: &nonceExpiresAt,
				},
			},
		}
		b7m := &infrastructurev1beta1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "b7m-large-ca", Namespace: "n"},
			Spec: infrastructurev1beta1.Beskar7MachineSpec{
				InspectionImageURL: "https://boot.example.com/inspect",
				TargetImageURL:     "https://boot.example.com/target.tar.gz",
				TargetImageDigest:  bootTestDigest,
			},
		}
		tokenSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: bootstrapTokenSecretName("h-large-ca"), Namespace: "n"},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{"plaintext-token": []byte("fake-token")},
		}
		fakeClient := fake.NewClientBuilder().
			WithScheme(k8sClient.Scheme()).
			WithObjects(b7m, tokenSecret).
			WithStatusSubresource(ph).
			WithObjects(ph).
			Build()

		handler := &BootHandler{
			Client: fakeClient,
			Log:    ctrl.Log.WithName("boot-large-ca-test"),
			Config: largeCfg,
		}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/boot/n/h-large-ca/"+noncePlaintext, nil)
		req.SetPathValue("namespace", "n")
		req.SetPathValue("hostName", "h-large-ca")
		req.SetPathValue("nonce", noncePlaintext)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		Expect(w.Code).To(Equal(http.StatusNotFound),
			"oversized script must be an opaque 404, not a 200 with a truncated script")
		Expect(w.Body.String()).To(ContainSubstring(bootHandlerOpaqueFailureBody))
	})

	// ── 8. Rate limiting ───────────────────────────────────────────────────

	It("rate limiting: burst exceeded from one IP → 429; different IP unaffected", func() {
		// Drive the handler directly via httptest.ResponseRecorder so we control
		// RemoteAddr precisely without touching the API server (the rate-limiter
		// fires before the nonce verification).
		bootLog := ctrl.Log.WithName("boot-rate-limit-test")
		handler := &BootHandler{
			Client: k8sClient,
			Log:    bootLog,
			Config: bootTestConfig(),
		}

		const ip1 = "10.0.0.1"
		const ip2 = "10.0.0.2"

		// Exhaust the burst from ip1. Each call to allow() burns 1 token from
		// the bucket (burst=5). The burst+1-th call must be rejected.
		for i := 0; i < bootIPRateLimitBurst; i++ {
			Expect(handler.allowIP(ip1)).To(BeTrue(),
				fmt.Sprintf("call %d (within burst) must be allowed", i+1))
		}
		Expect(handler.allowIP(ip1)).To(BeFalse(),
			"call burst+1 from ip1 must be rate-limited")

		By("different IP is unaffected by ip1's exhausted bucket")
		Expect(handler.allowIP(ip2)).To(BeTrue(),
			"ip2 has an independent bucket and must not be limited")
	})

	// ── 9. No secret leakage ───────────────────────────────────────────────

	It("no secret leakage: nonce/token/script never appear in log sink", func() {
		// Wire a log sink that captures every keyed value.
		sink := &logCaptureSink{}
		capLog := logWithSink(sink)

		bootHandler := &BootHandler{
			Client: k8sClient,
			Log:    capLog,
			Config: bootTestConfig(),
		}
		mux := http.NewServeMux()
		mux.Handle("GET /api/v1/boot/{namespace}/{hostName}/{nonce}", bootHandler)
		logServer := httptest.NewServer(mux)
		defer logServer.Close()

		ph, _, bearerToken, nonce := bootTestFixture(testNs.Name)

		resp := doBoot(logServer.URL, testNs.Name, ph.Name, nonce)
		body := readBody(resp)
		Expect(resp.StatusCode).To(Equal(http.StatusOK))

		By("assert the nonce does not appear in any log key/value")
		for _, entry := range sink.entries {
			Expect(entry).NotTo(ContainSubstring(nonce),
				"nonce must never appear in logs")
			Expect(entry).NotTo(ContainSubstring(bearerToken),
				"bearer token must never appear in logs")
			// The rendered script contains the bearer token; assert it's not logged.
			Expect(entry).NotTo(ContainSubstring(body),
				"rendered iPXE script must never appear in logs")
		}
	})
})

// ── log capture helpers ───────────────────────────────────────────────────────

// logCaptureSink implements logr.LogSink, recording every log entry as a
// flat string so tests can scan for forbidden material (nonce, token, script).
type logCaptureSink struct {
	mu      sync.Mutex
	entries []string
}

func (s *logCaptureSink) append(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, msg)
}

func (s *logCaptureSink) Init(_ logr.RuntimeInfo) {}
func (s *logCaptureSink) Enabled(_ int) bool      { return true }
func (s *logCaptureSink) Info(_ int, msg string, kv ...interface{}) {
	s.append(fmt.Sprint(append([]interface{}{msg}, kv...)...))
}
func (s *logCaptureSink) Error(err error, msg string, kv ...interface{}) {
	s.append(fmt.Sprint(append([]interface{}{msg, err}, kv...)...))
}
func (s *logCaptureSink) WithValues(kv ...interface{}) logr.LogSink {
	// Return a child that shares the same entry slice.
	return &logCaptureSinkChild{parent: s, static: fmt.Sprint(kv...)}
}
func (s *logCaptureSink) WithName(_ string) logr.LogSink { return s }

// logCaptureSinkChild is returned by WithValues so that static key-value
// pairs set by WithValues are also scanned for forbidden material.
type logCaptureSinkChild struct {
	parent *logCaptureSink
	static string
}

func (c *logCaptureSinkChild) Init(_ logr.RuntimeInfo) {}
func (c *logCaptureSinkChild) Enabled(_ int) bool      { return true }
func (c *logCaptureSinkChild) Info(_ int, msg string, kv ...interface{}) {
	c.parent.append(fmt.Sprint(append([]interface{}{c.static, msg}, kv...)...))
}
func (c *logCaptureSinkChild) Error(err error, msg string, kv ...interface{}) {
	c.parent.append(fmt.Sprint(append([]interface{}{c.static, msg, err}, kv...)...))
}
func (c *logCaptureSinkChild) WithValues(kv ...interface{}) logr.LogSink {
	return &logCaptureSinkChild{parent: c.parent, static: c.static + fmt.Sprint(kv...)}
}
func (c *logCaptureSinkChild) WithName(_ string) logr.LogSink { return c }

// logWithSink constructs a logr.Logger backed by the given sink.
func logWithSink(s logr.LogSink) logr.Logger {
	return logr.New(s)
}

// ── TargetDisk feature tests (contract §5 beskar7.disk, §9.1 step 2) ─────────

var _ = Describe("buildBootIPXEScript — TargetDisk rendering", func() {
	const (
		testInspectionURL = "https://boot.example.com/inspect"
		testAPIBase       = "https://beskar7.example.com:8082"
		testNamespace     = "default"
		testHost          = "host-01"
		testToken         = "tok-abc123"
		testTargetURL     = "https://images.example.com/kairos.img"
		testCA            = "FAKECABASE64=="
	)

	It("renders beskar7.disk immediately after beskar7.ca when TargetDisk is set", func() {
		disk := "/dev/disk/by-id/nvme-FOO"
		script := buildBootIPXEScript(
			testInspectionURL,
			testAPIBase,
			testNamespace,
			testHost,
			testToken,
			testTargetURL,
			bootTestDigest,
			testCA,
			disk,
			"", // no BOOTIF
			"", // no StaticIP
		)

		By("asserting beskar7.disk appears in the kernel line")
		Expect(script).To(ContainSubstring("beskar7.disk=" + disk))

		By("asserting beskar7.disk is positioned immediately after beskar7.ca")
		caIdx := strings.Index(script, "beskar7.ca=")
		diskIdx := strings.Index(script, "beskar7.disk=")
		Expect(diskIdx).To(BeNumerically(">", caIdx),
			"beskar7.disk must appear after beskar7.ca")
		// The token between the two params must be exactly " beskar7.disk=":
		// no intervening content (no other params between them).
		caEnd := caIdx + len("beskar7.ca=") + len(testCA)
		Expect(script[caEnd:caEnd+len(" beskar7.disk=")]).To(Equal(" beskar7.disk="),
			"beskar7.disk must be the immediate successor of beskar7.ca with a single space")

		By("asserting beskar7.disk appears before the initrd line")
		initrdIdx := strings.Index(script, "\ninitrd ")
		Expect(diskIdx).To(BeNumerically("<", initrdIdx),
			"beskar7.disk must precede the initrd line")
	})

	It("omits beskar7.disk entirely when TargetDisk is empty", func() {
		script := buildBootIPXEScript(
			testInspectionURL,
			testAPIBase,
			testNamespace,
			testHost,
			testToken,
			testTargetURL,
			bootTestDigest,
			testCA,
			"", // no disk
			"", // no BOOTIF
			"", // no StaticIP
		)

		By("asserting beskar7.disk is absent")
		Expect(script).NotTo(ContainSubstring("beskar7.disk"),
			"empty TargetDisk must produce no beskar7.disk token in the script")

		By("asserting the kernel line ends with beskar7.ca=<value> immediately followed by newline+initrd")
		caIdx := strings.Index(script, "beskar7.ca=")
		Expect(caIdx).To(BeNumerically(">", 0))
		caEnd := caIdx + len("beskar7.ca=") + len(testCA)
		Expect(script[caEnd]).To(Equal(byte('\n')),
			"no trailing space after beskar7.ca when TargetDisk is empty — byte-identical to pre-change format")
	})

	// ── BOOTIF rendering (D-013) ────────────────────────────────────────────

	It("renders BOOTIF after beskar7.disk when both disk and bootif are set", func() {
		disk := "/dev/sda"
		bootif := "01-52-54-00-12-34-56"
		script := buildBootIPXEScript(
			testInspectionURL,
			testAPIBase,
			testNamespace,
			testHost,
			testToken,
			testTargetURL,
			bootTestDigest,
			testCA,
			disk,
			bootif,
			"", // no StaticIP
		)

		By("asserting BOOTIF appears in the kernel line")
		Expect(script).To(ContainSubstring("BOOTIF=" + bootif))

		By("asserting BOOTIF is positioned after beskar7.disk")
		diskIdx := strings.Index(script, "beskar7.disk=")
		bootifIdx := strings.Index(script, "BOOTIF=")
		Expect(bootifIdx).To(BeNumerically(">", diskIdx),
			"BOOTIF must appear after beskar7.disk")

		By("asserting BOOTIF appears before the initrd line")
		initrdIdx := strings.Index(script, "\ninitrd ")
		Expect(bootifIdx).To(BeNumerically("<", initrdIdx),
			"BOOTIF must precede the initrd line")

		By("asserting BOOTIF is immediately after beskar7.disk with a single space")
		diskEnd := diskIdx + len("beskar7.disk=") + len(disk)
		Expect(script[diskEnd:diskEnd+len(" BOOTIF=")]).To(Equal(" BOOTIF="),
			"BOOTIF must be the immediate successor of beskar7.disk with a single space")
	})

	It("renders BOOTIF after beskar7.ca (no disk) when only bootif is set", func() {
		bootif := "01-52-54-00-12-34-56"
		script := buildBootIPXEScript(
			testInspectionURL,
			testAPIBase,
			testNamespace,
			testHost,
			testToken,
			testTargetURL,
			bootTestDigest,
			testCA,
			"", // no disk
			bootif,
			"", // no StaticIP
		)

		By("asserting BOOTIF appears in the kernel line")
		Expect(script).To(ContainSubstring("BOOTIF=" + bootif))

		By("asserting beskar7.disk is absent")
		Expect(script).NotTo(ContainSubstring("beskar7.disk"))

		By("asserting BOOTIF is immediately after beskar7.ca with a single space")
		caIdx := strings.Index(script, "beskar7.ca=")
		caEnd := caIdx + len("beskar7.ca=") + len(testCA)
		Expect(script[caEnd:caEnd+len(" BOOTIF=")]).To(Equal(" BOOTIF="),
			"BOOTIF must be the immediate successor of beskar7.ca with a single space when disk is absent")

		By("asserting BOOTIF appears before the initrd line")
		bootifIdx := strings.Index(script, "BOOTIF=")
		initrdIdx := strings.Index(script, "\ninitrd ")
		Expect(bootifIdx).To(BeNumerically("<", initrdIdx),
			"BOOTIF must precede the initrd line")
	})

	It("omits BOOTIF entirely when bootif is empty, output byte-identical to pre-D-013 format", func() {
		script := buildBootIPXEScript(
			testInspectionURL,
			testAPIBase,
			testNamespace,
			testHost,
			testToken,
			testTargetURL,
			bootTestDigest,
			testCA,
			"", // no disk
			"", // no BOOTIF
			"", // no StaticIP
		)
		scriptWithBootif := buildBootIPXEScript(
			testInspectionURL,
			testAPIBase,
			testNamespace,
			testHost,
			testToken,
			testTargetURL,
			bootTestDigest,
			testCA,
			"",
			"01-52-54-00-12-34-56",
			"", // no StaticIP
		)

		By("asserting BOOTIF is absent when bootif is empty")
		Expect(script).NotTo(ContainSubstring("BOOTIF"),
			"empty bootif must produce no BOOTIF token in the script")

		By("asserting the kernel line ends with beskar7.ca=<value> immediately followed by newline")
		caIdx := strings.Index(script, "beskar7.ca=")
		caEnd := caIdx + len("beskar7.ca=") + len(testCA)
		Expect(script[caEnd]).To(Equal(byte('\n')),
			"no trailing space after beskar7.ca when bootif is empty — byte-identical to pre-D-013 format")

		By("the BOOTIF variant is different from the no-BOOTIF variant")
		Expect(script).NotTo(Equal(scriptWithBootif))
	})
})

var _ = Describe("validateBootDisk", func() {
	type diskCase struct {
		name    string
		input   string
		wantErr bool
	}
	cases := []diskCase{
		// ── accept ──
		{name: "empty string (optional field)", input: "", wantErr: false},
		{name: "kernel short name", input: "sda", wantErr: false},
		{name: "kernel nvme name", input: "nvme0n1", wantErr: false},
		{name: "by-id path", input: "/dev/disk/by-id/nvme-Samsung_SSD_990_PRO_2TB_S6Z0NX0W123456", wantErr: false},
		{name: "by-path path", input: "/dev/disk/by-path/pci-0000:02:00.0-nvme-1", wantErr: false},
		{name: "dot-separated label", input: "disk.by-id.nvme+foo", wantErr: false},
		// ── reject ──
		{name: "value with space (injection vector)", input: "/dev/sda beskar7.api=ATTACKER", wantErr: true},
		{name: "value with newline (injection vector)", input: "/dev/sda\nbeskar7.token=ATTACKER", wantErr: true},
		{name: "value with tab (injection vector)", input: "/dev/sda\tbeskar7.api=ATTACKER", wantErr: true},
		{name: "value with control char (injection vector)", input: "/dev/sda\x01evil", wantErr: true},
		{name: "value with shell metachar dollar", input: "/dev/sda$PWD", wantErr: true},
	}

	for _, tc := range cases {
		tc := tc
		It("validateBootDisk: "+tc.name, func() {
			err := validateBootDisk(tc.input)
			if tc.wantErr {
				Expect(err).To(HaveOccurred(),
					"expected validateBootDisk to reject %q but it accepted it", tc.input)
			} else {
				Expect(err).NotTo(HaveOccurred(),
					"expected validateBootDisk to accept %q but it rejected: %v", tc.input, err)
			}
		})
	}
})

// ── StaticIP feature tests (contract v3 §5 beskar7.ip, §8.2) ─────────────────

var _ = Describe("buildBootIPXEScript — StaticIP rendering", func() {
	const (
		testInspectionURL2 = "https://boot.example.com/inspect"
		testAPIBase2       = "https://beskar7.example.com:8082"
		testNamespace2     = "default"
		testHost2          = "host-01"
		testToken2         = "tok-abc123"
		testTargetURL2     = "https://images.example.com/kairos.img"
		testCA2            = "FAKECABASE64=="
	)

	It("renders beskar7.ip after beskar7.ca (no disk) when StaticIP is set", func() {
		staticIP := "192.168.150.10::192.168.150.1:255.255.255.0"
		script := buildBootIPXEScript(
			testInspectionURL2,
			testAPIBase2,
			testNamespace2,
			testHost2,
			testToken2,
			testTargetURL2,
			bootTestDigest,
			testCA2,
			"", // no disk
			"", // no BOOTIF
			staticIP,
		)

		By("asserting beskar7.ip appears in the kernel line")
		Expect(script).To(ContainSubstring("beskar7.ip=" + staticIP))

		By("asserting beskar7.ip is positioned immediately after beskar7.ca with a single space")
		caIdx := strings.Index(script, "beskar7.ca=")
		caEnd := caIdx + len("beskar7.ca=") + len(testCA2)
		Expect(script[caEnd:caEnd+len(" beskar7.ip=")]).To(Equal(" beskar7.ip="),
			"beskar7.ip must be the immediate successor of beskar7.ca with a single space when disk is absent")

		By("asserting beskar7.ip appears before the initrd line")
		ipIdx := strings.Index(script, "beskar7.ip=")
		initrdIdx := strings.Index(script, "\ninitrd ")
		Expect(ipIdx).To(BeNumerically("<", initrdIdx), "beskar7.ip must precede the initrd line")
	})

	It("renders beskar7.ip after beskar7.disk when both disk and staticIP are set", func() {
		disk := "/dev/sda"
		staticIP := "192.168.150.10::192.168.150.1:255.255.255.0"
		script := buildBootIPXEScript(
			testInspectionURL2,
			testAPIBase2,
			testNamespace2,
			testHost2,
			testToken2,
			testTargetURL2,
			bootTestDigest,
			testCA2,
			disk,
			"", // no BOOTIF
			staticIP,
		)

		By("asserting beskar7.disk and beskar7.ip both appear")
		Expect(script).To(ContainSubstring("beskar7.disk=" + disk))
		Expect(script).To(ContainSubstring("beskar7.ip=" + staticIP))

		By("asserting beskar7.ip is immediately after beskar7.disk with a single space")
		diskIdx := strings.Index(script, "beskar7.disk=")
		diskEnd := diskIdx + len("beskar7.disk=") + len(disk)
		Expect(script[diskEnd:diskEnd+len(" beskar7.ip=")]).To(Equal(" beskar7.ip="),
			"beskar7.ip must be the immediate successor of beskar7.disk with a single space")

		By("asserting ordering: beskar7.ca < beskar7.disk < beskar7.ip < initrd")
		caIdx := strings.Index(script, "beskar7.ca=")
		ipIdx := strings.Index(script, "beskar7.ip=")
		initrdIdx := strings.Index(script, "\ninitrd ")
		Expect(caIdx).To(BeNumerically("<", diskIdx))
		Expect(diskIdx).To(BeNumerically("<", ipIdx))
		Expect(ipIdx).To(BeNumerically("<", initrdIdx))
	})

	It("renders BOOTIF after beskar7.ip when all three optional params are set", func() {
		disk := "/dev/sda"
		staticIP := "192.168.150.10::192.168.150.1:255.255.255.0"
		bootif := "01-52-54-00-12-34-56"
		script := buildBootIPXEScript(
			testInspectionURL2,
			testAPIBase2,
			testNamespace2,
			testHost2,
			testToken2,
			testTargetURL2,
			bootTestDigest,
			testCA2,
			disk,
			bootif,
			staticIP,
		)

		By("asserting all three optional params appear")
		Expect(script).To(ContainSubstring("beskar7.disk=" + disk))
		Expect(script).To(ContainSubstring("beskar7.ip=" + staticIP))
		Expect(script).To(ContainSubstring("BOOTIF=" + bootif))

		By("asserting ordering: beskar7.disk < beskar7.ip < BOOTIF")
		diskIdx := strings.Index(script, "beskar7.disk=")
		ipIdx := strings.Index(script, "beskar7.ip=")
		bootifIdx := strings.Index(script, "BOOTIF=")
		Expect(diskIdx).To(BeNumerically("<", ipIdx), "beskar7.disk must precede beskar7.ip")
		Expect(ipIdx).To(BeNumerically("<", bootifIdx), "beskar7.ip must precede BOOTIF")
	})

	It("omits beskar7.ip entirely when StaticIP is empty, output byte-identical to v2 format", func() {
		scriptV2 := buildBootIPXEScript(
			testInspectionURL2,
			testAPIBase2,
			testNamespace2,
			testHost2,
			testToken2,
			testTargetURL2,
			bootTestDigest,
			testCA2,
			"", // no disk
			"", // no BOOTIF
			"", // no StaticIP
		)
		scriptWithIP := buildBootIPXEScript(
			testInspectionURL2,
			testAPIBase2,
			testNamespace2,
			testHost2,
			testToken2,
			testTargetURL2,
			bootTestDigest,
			testCA2,
			"", // no disk
			"", // no BOOTIF
			"192.168.150.10::192.168.150.1:255.255.255.0",
		)

		By("asserting beskar7.ip is absent when staticIP is empty")
		Expect(scriptV2).NotTo(ContainSubstring("beskar7.ip"),
			"empty StaticIP must produce no beskar7.ip token in the script")

		By("asserting the kernel line ends with beskar7.ca=<value> immediately followed by newline")
		caIdx := strings.Index(scriptV2, "beskar7.ca=")
		caEnd := caIdx + len("beskar7.ca=") + len(testCA2)
		Expect(scriptV2[caEnd]).To(Equal(byte('\n')),
			"no trailing space after beskar7.ca when StaticIP is empty — byte-identical to pre-v3 format")

		By("the StaticIP variant differs from the empty-StaticIP variant")
		Expect(scriptV2).NotTo(Equal(scriptWithIP))
	})
})

// ── validateStaticIP unit tests (contract v3 §5, SEC-7) ──────────────────────

var _ = Describe("validateStaticIP", func() {
	type staticIPCase struct {
		name    string
		input   string
		wantErr bool
	}
	cases := []staticIPCase{
		// ── accept ──
		{
			name:    "valid ip+gw+netmask",
			input:   "192.168.150.10::192.168.150.1:255.255.255.0",
			wantErr: false,
		},
		{
			name:    "valid ip+gw+CIDR-prefix",
			input:   "10.0.0.5::10.0.0.1:24",
			wantErr: false,
		},
		{
			name:    "valid ip+no-gw+CIDR-prefix+dns",
			input:   "10.0.0.5:::24:8.8.8.8",
			wantErr: false,
		},
		{
			name:    "valid ip+no-gw+netmask",
			input:   "192.168.1.100:::255.255.255.0",
			wantErr: false,
		},
		{
			name:    "valid ip+gw+netmask+dns",
			input:   "192.168.150.10::192.168.150.1:255.255.255.0:8.8.8.8",
			wantErr: false,
		},
		{
			name:    "valid CIDR prefix 0 (single digit)",
			input:   "10.0.0.1:::0",
			wantErr: false,
		},
		{
			name:    "valid CIDR prefix 32 (two digits)",
			input:   "10.0.0.1:::32",
			wantErr: false,
		},
		{
			name:    "empty string (optional field)",
			input:   "",
			wantErr: false,
		},
		// ── reject ──
		{
			name:    "plain IPv4 with no colons (non-ip= shape)",
			input:   "192.168.1.1",
			wantErr: true,
		},
		{
			name:    "missing double-colon server-IP separator",
			input:   "192.168.1.1:192.168.1.1:255.255.255.0",
			wantErr: true,
		},
		{
			name:    "garbage string",
			input:   "not-an-ip-at-all",
			wantErr: true,
		},
		{
			name:    "value with space (cmdline injection vector)",
			input:   "192.168.1.1::192.168.1.254:24 beskar7.token=ATTACKER",
			wantErr: true,
		},
		{
			name:    "value with newline (cmdline injection vector)",
			input:   "192.168.1.1::192.168.1.254:24\nbeskar7.token=ATTACKER",
			wantErr: true,
		},
		{
			name:    "value with tab (cmdline injection vector)",
			input:   "192.168.1.1::192.168.1.254:24\tbeskar7.api=ATTACKER",
			wantErr: true,
		},
		{
			name:    "beskar7.x= appended after valid prefix (injection via extra param)",
			input:   "192.168.1.1::192.168.1.254:24:8.8.8.8 beskar7.x=INJECTED",
			wantErr: true,
		},
		{
			name:    "hostname instead of IP",
			input:   "host.example.com:::24",
			wantErr: true,
		},
		{
			name:    "CIDR prefix three digits (out of shape)",
			input:   "10.0.0.1:::128",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		tc := tc
		It("validateStaticIP: "+tc.name, func() {
			err := validateStaticIP(tc.input)
			if tc.wantErr {
				Expect(err).To(HaveOccurred(),
					"expected validateStaticIP to reject %q but it accepted it", tc.input)
			} else {
				Expect(err).NotTo(HaveOccurred(),
					"expected validateStaticIP to accept %q but it rejected: %v", tc.input, err)
			}
		})
	}
})

// ── formatBootif unit tests (D-013 / SEC-7 posture) ───────────────────────────

var _ = Describe("formatBootif", func() {
	type bootifCase struct {
		name       string
		input      string
		wantResult string
		wantOK     bool
	}
	cases := []bootifCase{
		// ── accept ──
		{
			name:       "well-formed lowercase MAC",
			input:      "52:54:00:12:34:56",
			wantResult: "01-52-54-00-12-34-56",
			wantOK:     true,
		},
		{
			name:       "well-formed uppercase MAC (lowercased in output)",
			input:      "52:54:00:AA:BB:CC",
			wantResult: "01-52-54-00-aa-bb-cc",
			wantOK:     true,
		},
		{
			name:       "all-zeros broadcast MAC",
			input:      "00:00:00:00:00:00",
			wantResult: "01-00-00-00-00-00-00",
			wantOK:     true,
		},
		{
			name:       "all-ff broadcast MAC",
			input:      "ff:ff:ff:ff:ff:ff",
			wantResult: "01-ff-ff-ff-ff-ff-ff",
			wantOK:     true,
		},
		// ── reject ──
		{
			name:   "empty string",
			input:  "",
			wantOK: false,
		},
		{
			name:   "too short (5 octets)",
			input:  "52:54:00:12:34",
			wantOK: false,
		},
		{
			name:   "too long (7 octets)",
			input:  "52:54:00:12:34:56:78",
			wantOK: false,
		},
		{
			name:   "non-hex characters (injection attempt)",
			input:  "zz:54:00:12:34:56",
			wantOK: false,
		},
		{
			name:   "spaces instead of colons",
			input:  "52 54 00 12 34 56",
			wantOK: false,
		},
		{
			name:   "dashes instead of colons (BOOTIF output form, not input form)",
			input:  "52-54-00-12-34-56",
			wantOK: false,
		},
		{
			name:   "injection: key=value appended after valid prefix",
			input:  "52:54:00:12:34:56 beskar7.api=ATTACKER",
			wantOK: false,
		},
		{
			name:   "injection: newline in value",
			input:  "52:54:00:12:34:56\nbeskar7.token=ATTACKER",
			wantOK: false,
		},
	}

	for _, tc := range cases {
		tc := tc
		It("formatBootif: "+tc.name, func() {
			result, ok := formatBootif(tc.input)
			if tc.wantOK {
				Expect(ok).To(BeTrue(),
					"expected formatBootif(%q) to succeed but it returned false", tc.input)
				Expect(result).To(Equal(tc.wantResult),
					"formatBootif(%q) returned %q, want %q", tc.input, result, tc.wantResult)
			} else {
				Expect(ok).To(BeFalse(),
					"expected formatBootif(%q) to return (false) but it returned (%q, true)", tc.input, result)
				Expect(result).To(Equal(""),
					"on failure formatBootif must return empty string")
			}
		})
	}
})
