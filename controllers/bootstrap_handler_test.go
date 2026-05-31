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
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrastructurev1beta1 "github.com/projectbeskar/beskar7/api/v1beta1"
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
		physicalHost *infrastructurev1beta1.PhysicalHost
		b7machine    *infrastructurev1beta1.Beskar7Machine
		ownerMachine *clusterv1.Machine
		server       *httptest.Server
	)

	// setHostBootstrap mints + stores a bearer-token hash on the host's
	// Status.Bootstrap so the verifier accepts the returned plaintext.
	setHostBootstrap := func(hash string, expiresIn time.Duration) {
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

	// linkConsumer points the host's Spec.ConsumerRef at the test
	// Beskar7Machine. Status writes happen through Status().Update; spec
	// writes here go through Patch to mimic the real claim path.
	linkConsumer := func() {
		ph := &infrastructurev1beta1.PhysicalHost{}
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
		got := &infrastructurev1beta1.Beskar7Machine{}
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
		physicalHost = &infrastructurev1beta1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "bootstrap-handler-host",
				Namespace: testNs.Name,
			},
			Spec: infrastructurev1beta1.PhysicalHostSpec{
				RedfishConnection: infrastructurev1beta1.RedfishConnection{
					Address:              "https://192.168.77.10",
					CredentialsSecretRef: "irrelevant",
				},
			},
		}
		Expect(k8sClient.Create(ctx, physicalHost)).To(Succeed())

		// Beskar7Machine (consumer of the host).
		b7machine = &infrastructurev1beta1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "bootstrap-handler-b7m",
				Namespace: testNs.Name,
			},
			Spec: infrastructurev1beta1.Beskar7MachineSpec{
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
				ClusterName: "fake-cluster",
				Bootstrap:   clusterv1.Bootstrap{},
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

		_, hash, err := auth.MintToken()
		Expect(err).NotTo(HaveOccurred())
		setHostBootstrap(hash, 30*time.Minute)

		resp := getBootstrap("")
		defer func() { _ = resp.Body.Close() }()
		Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized))
	})

	It("rejects GET with an expired bearer token (401)", func() {
		linkConsumer()
		bindMachineOwner()

		plaintext, hash, err := auth.MintToken()
		Expect(err).NotTo(HaveOccurred())
		// ExpiresAt 1 hour in the past.
		setHostBootstrap(hash, -1*time.Hour)

		resp := getBootstrap(plaintext)
		defer func() { _ = resp.Body.Close() }()
		Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized))
	})

	It("returns 404 when PhysicalHost has no Beskar7Machine consumer", func() {
		// Token is valid (verifier passes — host has Status.Bootstrap), but
		// Spec.ConsumerRef is nil. The handler must walk the chain and refuse.
		plaintext, hash, err := auth.MintToken()
		Expect(err).NotTo(HaveOccurred())
		setHostBootstrap(hash, 30*time.Minute)

		// Do NOT call linkConsumer() — host has no ConsumerRef.
		resp := getBootstrap(plaintext)
		defer func() { _ = resp.Body.Close() }()
		Expect(resp.StatusCode).To(Equal(http.StatusNotFound))
	})

	It("returns 404 when the consumer Beskar7Machine has been deleted", func() {
		linkConsumer()

		// Delete the Beskar7Machine — ConsumerRef now dangles.
		Expect(k8sClient.Delete(ctx, b7machine)).To(Succeed())
		Eventually(func(g Gomega) {
			got := &infrastructurev1beta1.Beskar7Machine{}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: b7machine.Name, Namespace: testNs.Name}, got)
			g.Expect(err).To(HaveOccurred())
		}, Timeout, Interval).Should(Succeed())

		plaintext, hash, err := auth.MintToken()
		Expect(err).NotTo(HaveOccurred())
		setHostBootstrap(hash, 30*time.Minute)

		resp := getBootstrap(plaintext)
		defer func() { _ = resp.Body.Close() }()
		Expect(resp.StatusCode).To(Equal(http.StatusNotFound))
	})

	It("returns 404 when the owner Machine has no Spec.Bootstrap.DataSecretName", func() {
		linkConsumer()
		bindMachineOwner()
		// ownerMachine.Spec.Bootstrap.DataSecretName left nil.

		plaintext, hash, err := auth.MintToken()
		Expect(err).NotTo(HaveOccurred())
		setHostBootstrap(hash, 30*time.Minute)

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

		plaintext, hash, err := auth.MintToken()
		Expect(err).NotTo(HaveOccurred())
		setHostBootstrap(hash, 30*time.Minute)

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

		plaintext, hash, err := auth.MintToken()
		Expect(err).NotTo(HaveOccurred())
		setHostBootstrap(hash, 30*time.Minute)

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
		ph := &infrastructurev1beta1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{Name: "h", Namespace: "n"},
			Spec: infrastructurev1beta1.PhysicalHostSpec{
				ConsumerRef: &corev1.ObjectReference{
					Kind:       "Beskar7Machine",
					APIVersion: InfrastructureAPIVersion,
					Name:       "b7m",
					Namespace:  "n",
				},
			},
		}
		b7m := &infrastructurev1beta1.Beskar7Machine{
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
				ClusterName: "c",
				Bootstrap:   clusterv1.Bootstrap{DataSecretName: &dataSecretName},
			},
		}
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: "n"},
			Data:       map[string][]byte{bootstrapDataSecretKey: oversized},
		}
		fakeClient := fake.NewClientBuilder().
			WithScheme(k8sClient.Scheme()).
			WithObjects(ph, b7m, owner, secret).
			Build()
		handler := &BootstrapHandler{
			Client: fakeClient,
			Log:    ctrl.Log.WithName("bootstrap-handler-oversize-test"),
		}
		// Build a request whose path values mimic the live mux.
		req := httptest.NewRequest(http.MethodGet, "/api/v1/bootstrap/n/h", nil)
		req.SetPathValue("namespace", "n")
		req.SetPathValue("hostName", "h")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		Expect(w.Code).To(Equal(http.StatusInternalServerError),
			"oversize bootstrap secret is operator-fault — must be 500, not 404")
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

		plaintext, hash, err := auth.MintToken()
		Expect(err).NotTo(HaveOccurred())
		setHostBootstrap(hash, 30*time.Minute)

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
