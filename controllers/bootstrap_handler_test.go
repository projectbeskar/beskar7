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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
	"github.com/projectbeskar/beskar7/internal/auth"
)

// buildBootstrapMux wires the bootstrap handler exactly as SetupCallbackServer
// does, but bound to an httptest.Server we can drive directly. The handler
// behaviour under test is everything below TLS.
func buildBootstrapMux() *http.ServeMux {
	log := ctrl.Log.WithName("bootstrap-handler-test")
	handler := &BootstrapHandler{
		Client: k8sClient,
		Log:    log,
	}
	verifier := newBearerTokenVerifier(k8sClient, log)
	mux := http.NewServeMux()
	mux.Handle("GET /api/v1/bootstrap/{namespace}/{hostName}",
		auth.RequireBearer(log, verifier, handler))
	return mux
}

var _ = Describe("Bootstrap GET handler (PR-5.3)", func() {
	const (
		Timeout  = time.Second * 10
		Interval = time.Millisecond * 250
	)

	var (
		testNs       *corev1.Namespace
		physicalHost *infrav1.PhysicalHost
		b7machine    *infrav1.Beskar7Machine
		ownerMachine *clusterv1.Machine
		server       *httptest.Server
	)

	// setHostBootstrap stores a bearer token in the host's bootstrap-token
	// Secret, bound to the test Beskar7Machine (D-029), so the verifier accepts
	// it while the host's ConsumerRef names that machine.
	setHostBootstrap := func(plaintext string, expiresIn time.Duration) {
		putCredentialSecret(client.ObjectKeyFromObject(physicalHost),
			boundCredentialData(b7machine.Name, plaintext, expiresIn, "", 0))
	}

	// serveDirect calls the handler itself with bearer token, as if the bearer
	// middleware had let the request through, and returns the recorded response.
	serveDirect := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/bootstrap/"+physicalHost.Namespace+"/"+physicalHost.Name, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.SetPathValue("namespace", physicalHost.Namespace)
		req.SetPathValue("hostName", physicalHost.Name)
		w := httptest.NewRecorder()
		(&BootstrapHandler{Client: k8sClient, Log: ctrl.Log.WithName("bootstrap-handler-direct")}).ServeHTTP(w, req)
		return w
	}

	// linkConsumer points the host's Spec.ConsumerRef at the test
	// Beskar7Machine. Status writes happen through Status().Update; spec
	// writes here go through Patch to mimic the real claim path.
	linkConsumer := func() {
		ph := &infrav1.PhysicalHost{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: physicalHost.Name, Namespace: physicalHost.Namespace}, ph)).To(Succeed())
		base := ph.DeepCopy()
		ph.Spec.ConsumerRef = &corev1.ObjectReference{
			Kind:       "Beskar7Machine",
			APIVersion: InfrastructureAPIVersion,
			Name:       b7machine.Name,
			Namespace:  b7machine.Namespace,
			UID:        b7machine.UID,
		}
		Expect(k8sClient.Patch(ctx, ph, client.MergeFrom(base))).To(Succeed())
	}

	// bindMachineOwner adds the CAPI Machine as an OwnerReference on the
	// Beskar7Machine, which is how util.GetOwnerMachine resolves the chain.
	bindMachineOwner := func() {
		got := &infrav1.Beskar7Machine{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: b7machine.Name, Namespace: b7machine.Namespace}, got)).To(Succeed())
		base := got.DeepCopy()
		got.OwnerReferences = append(got.OwnerReferences, metav1.OwnerReference{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       "Machine",
			Name:       ownerMachine.Name,
			UID:        ownerMachine.UID,
		})
		Expect(k8sClient.Patch(ctx, got, client.MergeFrom(base))).To(Succeed())
		// Re-fetch so subsequent ops see the updated object.
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: b7machine.Name, Namespace: b7machine.Namespace}, b7machine)).To(Succeed())
	}

	BeforeEach(func() {
		testNs = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "bootstrap-handler-test-"},
		}
		Expect(k8sClient.Create(ctx, testNs)).To(Succeed())

		// PhysicalHost (no ConsumerRef yet — set via linkConsumer per-spec).
		physicalHost = &infrav1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "bootstrap-handler-host",
				Namespace: testNs.Name,
			},
			Spec: infrav1.PhysicalHostSpec{
				RedfishConnection: infrav1.RedfishConnection{
					Address:              "https://192.168.77.10",
					CredentialsSecretRef: "irrelevant",
				},
			},
		}
		Expect(k8sClient.Create(ctx, physicalHost)).To(Succeed())

		// Beskar7Machine (consumer of the host).
		b7machine = &infrav1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "bootstrap-handler-b7m",
				Namespace: testNs.Name,
			},
			Spec: infrav1.Beskar7MachineSpec{
				InspectionImageURL: "http://boot-server/inspect.ipxe",
				TargetImageURL:     "http://boot-server/kairos.tar.gz",
				TargetImageDigest:  bootTestDigest,
			},
		}
		Expect(k8sClient.Create(ctx, b7machine)).To(Succeed())

		// CAPI Machine that owns the Beskar7Machine via OwnerReference. Its
		// Spec.Bootstrap.DataSecretName is set per-spec.
		ownerMachine = &clusterv1.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "bootstrap-handler-machine",
				Namespace: testNs.Name,
			},
			Spec: clusterv1.MachineSpec{
				ClusterName:       "fake-cluster",
				InfrastructureRef: clusterv1.ContractVersionedObjectReference{APIGroup: "infrastructure.cluster.x-k8s.io", Kind: "Beskar7Machine", Name: "fixture"},
				// The v1beta2 Machine CRD rejects an empty bootstrap; the ConfigRef is
				// inert in envtest and DataSecretName is still set per spec.
				Bootstrap: clusterv1.Bootstrap{ConfigRef: clusterv1.ContractVersionedObjectReference{APIGroup: "bootstrap.cluster.x-k8s.io", Kind: "KairosConfig", Name: "fixture"}},
			},
		}
		Expect(k8sClient.Create(ctx, ownerMachine)).To(Succeed())

		mux := buildBootstrapMux()
		server = httptest.NewServer(mux)
	})

	AfterEach(func() {
		server.Close()
		Expect(k8sClient.Delete(ctx, testNs)).To(Succeed())
	})

	getBootstrap := func(token string) *http.Response {
		req, err := http.NewRequest(http.MethodGet,
			fmt.Sprintf("%s/api/v1/bootstrap/%s/%s", server.URL, physicalHost.Namespace, physicalHost.Name),
			bytes.NewReader(nil))
		Expect(err).NotTo(HaveOccurred())
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		return resp
	}

	It("rejects GET without a bearer token (401)", func() {
		// Even with a fully-wired chain, no bearer header means the middleware
		// rejects before the handler runs.
		linkConsumer()
		bindMachineOwner()
		secretName := "bs-no-bearer"
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: testNs.Name},
			Data:       map[string][]byte{bootstrapDataSecretKey: []byte("ignored")},
		})).To(Succeed())
		ownerMachine.Spec.Bootstrap.DataSecretName = &secretName
		Expect(k8sClient.Update(ctx, ownerMachine)).To(Succeed())

		plaintext, _, err := auth.MintToken()
		Expect(err).NotTo(HaveOccurred())
		setHostBootstrap(plaintext, 30*time.Minute)

		resp := getBootstrap("")
		defer func() { _ = resp.Body.Close() }()
		Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized))
	})

	It("rejects GET with an expired bearer token (401)", func() {
		linkConsumer()
		bindMachineOwner()

		plaintext, _, err := auth.MintToken()
		Expect(err).NotTo(HaveOccurred())
		// ExpiresAt 1 hour in the past.
		setHostBootstrap(plaintext, -1*time.Hour)

		resp := getBootstrap(plaintext)
		defer func() { _ = resp.Body.Close() }()
		Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized))
	})

	It("rejects a host with no Beskar7Machine consumer (401), and the handler refuses it too (404)", func() {
		// The Secret holds an unexpired token bound to the machine, but
		// Spec.ConsumerRef is nil: callbacks for an unclaimed host are
		// rejected (D-029).
		plaintext, _, err := auth.MintToken()
		Expect(err).NotTo(HaveOccurred())
		setHostBootstrap(plaintext, 30*time.Minute)

		// Do NOT call linkConsumer() — host has no ConsumerRef.
		resp := getBootstrap(plaintext)
		defer func() { _ = resp.Body.Close() }()
		Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized))
		Expect(serveDirect(plaintext).Code).To(Equal(http.StatusNotFound))
	})

	It("rejects a token whose expiry in the Secret is missing or unparseable (401), however valid status says it is", func() {
		linkConsumer()
		bindMachineOwner()
		plaintext, hash, err := auth.MintToken()
		Expect(err).NotTo(HaveOccurred())
		future := metav1.NewTime(time.Now().Add(time.Hour))
		ph := &infrav1.PhysicalHost{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(physicalHost), ph)).To(Succeed())
		ph.Status.Bootstrap = &infrav1.BootstrapStatus{TokenHash: hash, ExpiresAt: &future}
		Expect(k8sClient.Status().Update(ctx, ph)).To(Succeed())

		for _, expiry := range [][]byte{nil, []byte("tomorrow")} {
			data := boundCredentialData(b7machine.Name, plaintext, time.Hour, "", 0)
			if expiry == nil {
				delete(data, bootstrapTokenExpiresAtSecretKey)
			} else {
				data[bootstrapTokenExpiresAtSecretKey] = expiry
			}
			putCredentialSecret(client.ObjectKeyFromObject(physicalHost), data)
			resp := getBootstrap(plaintext)
			_ = resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized), "expiry %q must fail closed", expiry)
		}
	})

	It("returns 404 when the consumer Beskar7Machine has been deleted", func() {
		linkConsumer()

		// Delete the Beskar7Machine — ConsumerRef now dangles.
		Expect(k8sClient.Delete(ctx, b7machine)).To(Succeed())
		Eventually(func(g Gomega) {
			got := &infrav1.Beskar7Machine{}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: b7machine.Name, Namespace: testNs.Name}, got)
			g.Expect(err).To(HaveOccurred())
		}, Timeout, Interval).Should(Succeed())

		plaintext, _, err := auth.MintToken()
		Expect(err).NotTo(HaveOccurred())
		setHostBootstrap(plaintext, 30*time.Minute)

		resp := getBootstrap(plaintext)
		defer func() { _ = resp.Body.Close() }()
		Expect(resp.StatusCode).To(Equal(http.StatusNotFound))
	})

	It("returns 404 when the owner Machine has no Spec.Bootstrap.DataSecretName", func() {
		linkConsumer()
		bindMachineOwner()
		// ownerMachine.Spec.Bootstrap.DataSecretName left nil.

		plaintext, _, err := auth.MintToken()
		Expect(err).NotTo(HaveOccurred())
		setHostBootstrap(plaintext, 30*time.Minute)

		resp := getBootstrap(plaintext)
		defer func() { _ = resp.Body.Close() }()
		Expect(resp.StatusCode).To(Equal(http.StatusNotFound))
	})

	It("returns 404 when the bootstrap data Secret is missing", func() {
		linkConsumer()
		bindMachineOwner()

		// Set DataSecretName to a Secret that does not exist.
		missingName := "does-not-exist"
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ownerMachine.Name, Namespace: testNs.Name}, ownerMachine)).To(Succeed())
		ownerMachine.Spec.Bootstrap.DataSecretName = &missingName
		Expect(k8sClient.Update(ctx, ownerMachine)).To(Succeed())

		plaintext, _, err := auth.MintToken()
		Expect(err).NotTo(HaveOccurred())
		setHostBootstrap(plaintext, 30*time.Minute)

		resp := getBootstrap(plaintext)
		defer func() { _ = resp.Body.Close() }()
		Expect(resp.StatusCode).To(Equal(http.StatusNotFound))
	})

	It("returns 404 when the bootstrap Secret has no 'value' key", func() {
		linkConsumer()
		bindMachineOwner()

		secretName := "bs-no-value-key"
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: testNs.Name},
			// Note: only "format", no "value".
			Data: map[string][]byte{"format": []byte("cloud-config")},
		})).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ownerMachine.Name, Namespace: testNs.Name}, ownerMachine)).To(Succeed())
		ownerMachine.Spec.Bootstrap.DataSecretName = &secretName
		Expect(k8sClient.Update(ctx, ownerMachine)).To(Succeed())

		plaintext, _, err := auth.MintToken()
		Expect(err).NotTo(HaveOccurred())
		setHostBootstrap(plaintext, 30*time.Minute)

		resp := getBootstrap(plaintext)
		defer func() { _ = resp.Body.Close() }()
		Expect(resp.StatusCode).To(Equal(http.StatusNotFound))
	})

	// The oversize-data branch exists as defense-in-depth: the Kubernetes API
	// server already caps Secret total size at 1 MiB, so a real envtest cannot
	// stage an oversize Secret. We exercise the branch via a fake
	// controller-runtime client (which does not enforce the API-server limit)
	// and a direct ServeHTTP call. This skips the bearer-auth middleware (we
	// test that elsewhere) and isolates the cap-check branch.
	It("returns 500 when the bootstrap Secret 'value' exceeds the 1 MiB cap (operator-fault)", func() {
		// Fake client preloaded with an over-cap Secret. We do not register any
		// other objects: the test exercises only the size-check branch.
		oversized := bytes.Repeat([]byte("x"), maxBootstrapDataSize+1)
		ph := &infrav1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{Name: "h", Namespace: "n"},
			Spec: infrav1.PhysicalHostSpec{
				ConsumerRef: &corev1.ObjectReference{
					Kind:       "Beskar7Machine",
					APIVersion: InfrastructureAPIVersion,
					Name:       "b7m",
					Namespace:  "n",
				},
			},
		}
		b7m := &infrav1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "b7m",
				Namespace: "n",
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: clusterv1.GroupVersion.String(),
					Kind:       "Machine",
					Name:       "owner",
					UID:        "owner-uid",
				}},
			},
		}
		secretName := "bs-oversized"
		dataSecretName := secretName
		owner := &clusterv1.Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: "n", UID: "owner-uid"},
			Spec: clusterv1.MachineSpec{
				ClusterName:       "c",
				InfrastructureRef: clusterv1.ContractVersionedObjectReference{APIGroup: "infrastructure.cluster.x-k8s.io", Kind: "Beskar7Machine", Name: "fixture"},
				Bootstrap:         clusterv1.Bootstrap{DataSecretName: &dataSecretName},
			},
		}
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: "n"},
			Data:       map[string][]byte{bootstrapDataSecretKey: oversized},
		}
		fakeClient := fake.NewClientBuilder().
			WithScheme(k8sClient.Scheme()).
			WithObjects(ph, b7m, owner, secret,
				credentialSecret("n", "h", boundCredentialData("b7m", "token", time.Hour, "", 0))).
			Build()
		handler := &BootstrapHandler{
			Client: fakeClient,
			Log:    ctrl.Log.WithName("bootstrap-handler-oversize-test"),
		}
		// Build a request whose path values mimic the live mux.
		req := httptest.NewRequest(http.MethodGet, "/api/v1/bootstrap/n/h", nil)
		req.Header.Set("Authorization", "Bearer token")
		req.SetPathValue("namespace", "n")
		req.SetPathValue("hostName", "h")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		Expect(w.Code).To(Equal(http.StatusInternalServerError),
			"oversize bootstrap secret is operator-fault — must be 500, not 404")
	})

	It("returns 404 and never serves the consumer's data when ConsumerRef names a Beskar7Machine in a different namespace (SEC-12)", func() {
		// A second namespace standing in for a different tenant. The host lives
		// in testNs (namespace A); its ConsumerRef is forged to point at a
		// Beskar7Machine in namespace B. Legitimate claims are always
		// same-namespace (Beskar7MachineReconciler.findAndClaimOrGetAssociatedHost
		// only lists hosts from its own namespace when claiming), so this
		// ConsumerRef could only come from direct PhysicalHost patch access —
		// exactly the SEC-12 threat model.
		otherNs := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "bootstrap-handler-test-crossns-"},
		}
		Expect(k8sClient.Create(ctx, otherNs)).To(Succeed())
		defer func() { Expect(k8sClient.Delete(ctx, otherNs)).To(Succeed()) }()

		crossMachine := &infrav1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "cross-ns-b7m",
				Namespace: otherNs.Name,
			},
			Spec: infrav1.Beskar7MachineSpec{
				InspectionImageURL: "http://boot-server/inspect.ipxe",
				TargetImageURL:     "http://boot-server/kairos.tar.gz",
				TargetImageDigest:  bootTestDigest,
			},
		}
		Expect(k8sClient.Create(ctx, crossMachine)).To(Succeed())

		crossOwnerMachine := &clusterv1.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "cross-ns-machine",
				Namespace: otherNs.Name,
			},
			Spec: clusterv1.MachineSpec{
				ClusterName:       "fake-cluster",
				InfrastructureRef: clusterv1.ContractVersionedObjectReference{APIGroup: "infrastructure.cluster.x-k8s.io", Kind: "Beskar7Machine", Name: "fixture"},
				Bootstrap:         clusterv1.Bootstrap{ConfigRef: clusterv1.ContractVersionedObjectReference{APIGroup: "bootstrap.cluster.x-k8s.io", Kind: "KairosConfig", Name: "fixture"}},
			},
		}
		Expect(k8sClient.Create(ctx, crossOwnerMachine)).To(Succeed())

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: crossMachine.Name, Namespace: otherNs.Name}, crossMachine)).To(Succeed())
		cmBase := crossMachine.DeepCopy()
		crossMachine.OwnerReferences = append(crossMachine.OwnerReferences, metav1.OwnerReference{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       "Machine",
			Name:       crossOwnerMachine.Name,
			UID:        crossOwnerMachine.UID,
		})
		Expect(k8sClient.Patch(ctx, crossMachine, client.MergeFrom(cmBase))).To(Succeed())

		secretName := "cross-ns-secret"
		secretPayload := []byte("#cloud-config\nSENSITIVE-NAMESPACE-B-CA-KEY-MATERIAL\n")
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: otherNs.Name},
			Data:       map[string][]byte{bootstrapDataSecretKey: secretPayload},
		})).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: crossOwnerMachine.Name, Namespace: otherNs.Name}, crossOwnerMachine)).To(Succeed())
		crossOwnerMachine.Spec.Bootstrap.DataSecretName = &secretName
		Expect(k8sClient.Update(ctx, crossOwnerMachine)).To(Succeed())

		// Forge the host's ConsumerRef to point at namespace B. A real claim
		// never produces this.
		ph := &infrav1.PhysicalHost{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: physicalHost.Name, Namespace: physicalHost.Namespace}, ph)).To(Succeed())
		phBase := ph.DeepCopy()
		ph.Spec.ConsumerRef = &corev1.ObjectReference{
			Kind:       "Beskar7Machine",
			APIVersion: InfrastructureAPIVersion,
			Name:       crossMachine.Name,
			Namespace:  otherNs.Name,
		}
		Expect(k8sClient.Patch(ctx, ph, client.MergeFrom(phBase))).To(Succeed())

		// Valid token for the host itself (namespace A), bound to a machine of
		// the forged ConsumerRef's name — the attacker needs nothing from
		// namespace B to reach this far, so the namespace pin is all that
		// stands in the way.
		plaintext, _, err := auth.MintToken()
		Expect(err).NotTo(HaveOccurred())
		putCredentialSecret(client.ObjectKeyFromObject(physicalHost),
			boundCredentialData(crossMachine.Name, plaintext, 30*time.Minute, "", 0))

		resp := getBootstrap(plaintext)
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())

		Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized),
			"a cross-namespace ConsumerRef resolves to no consumer: the same rejection as an unclaimed host")
		Expect(body).NotTo(ContainSubstring("SENSITIVE-NAMESPACE-B"),
			"namespace B's bootstrap data must never be served for a host in namespace A")

		By("the handler itself refusing it with the opaque 404, should anything let the request through")
		w := serveDirect(plaintext)
		Expect(w.Code).To(Equal(http.StatusNotFound))
		Expect(w.Body.String()).NotTo(ContainSubstring("SENSITIVE-NAMESPACE-B"))
	})

	It("refuses a token that does not match the host's current credentials, even with the chain intact", func() {
		linkConsumer()
		bindMachineOwner()
		secretName := "bs-stale-token"
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: testNs.Name},
			Data:       map[string][]byte{bootstrapDataSecretKey: []byte("CURRENT-CLAIM-DATA")},
		})).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ownerMachine.Name, Namespace: testNs.Name}, ownerMachine)).To(Succeed())
		ownerMachine.Spec.Bootstrap.DataSecretName = &secretName
		Expect(k8sClient.Update(ctx, ownerMachine)).To(Succeed())

		current, _, err := auth.MintToken()
		Expect(err).NotTo(HaveOccurred())
		setHostBootstrap(current, 30*time.Minute)

		// A request the verifier let through just before a release and re-mint
		// carries the old token; the handler checks it against its own read.
		stale, _, err := auth.MintToken()
		Expect(err).NotTo(HaveOccurred())
		w := serveDirect(stale)
		Expect(w.Code).To(Equal(http.StatusNotFound))
		Expect(w.Body.String()).NotTo(ContainSubstring("CURRENT-CLAIM-DATA"))

		Expect(serveDirect(current).Code).To(Equal(http.StatusOK), "the current token still fetches")
	})

	It("returns 200 with the Secret bytes when the chain is intact", func() {
		linkConsumer()
		bindMachineOwner()

		secretName := "bs-happy-path"
		userData := []byte("#cloud-config\nhostname: bootstrap-test\n")
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: testNs.Name},
			Data:       map[string][]byte{bootstrapDataSecretKey: userData},
		})).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ownerMachine.Name, Namespace: testNs.Name}, ownerMachine)).To(Succeed())
		ownerMachine.Spec.Bootstrap.DataSecretName = &secretName
		Expect(k8sClient.Update(ctx, ownerMachine)).To(Succeed())

		plaintext, _, err := auth.MintToken()
		Expect(err).NotTo(HaveOccurred())
		setHostBootstrap(plaintext, 30*time.Minute)

		resp := getBootstrap(plaintext)
		defer func() { _ = resp.Body.Close() }()

		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		Expect(resp.Header.Get("Content-Type")).To(Equal("application/octet-stream"))
		Expect(resp.Header.Get("Cache-Control")).To(Equal("no-store"),
			"Cache-Control: no-store prevents proxies from retaining bootstrap data")
		Expect(resp.Header.Get("Pragma")).To(Equal("no-cache"))

		body, err := io.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())
		Expect(body).To(Equal(userData),
			"response body must be the exact bytes from the Secret's 'value' key")
	})
})
