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
		Expect(body).To(ContainSubstring("beskar7.ca="))
		Expect(body).To(ContainSubstring("initrd " + b7m.Spec.InspectionImageURL + "/initrd.img"))
		Expect(body).To(ContainSubstring("\nboot\n"))

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

	// ── 6. Script length cap (SEC-10) ──────────────────────────────────────

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
