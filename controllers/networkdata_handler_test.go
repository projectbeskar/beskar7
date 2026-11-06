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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
	"github.com/projectbeskar/beskar7/internal/auth"
)

const networkDataRoute = "GET /api/v1/network-data/{namespace}/{hostName}"

// buildNetworkDataMux wires the network data handler as SetupCallbackServer does.
func buildNetworkDataMux() *http.ServeMux {
	log := ctrl.Log.WithName("network-data-handler-test")
	handler := &NetworkDataHandler{Client: k8sClient, Log: log}
	mux := http.NewServeMux()
	mux.Handle(networkDataRoute, auth.RequireBearer(log, newBearerTokenVerifier(k8sClient, log), handler))
	return mux
}

var _ = Describe("Network data GET handler (v4.3)", func() {
	const (
		hostName    = "networkdata-host"
		machineName = "networkdata-b7m"
		secretName  = "networkdata-secret"
	)

	var (
		testNs       *corev1.Namespace
		physicalHost *infrav1.PhysicalHost
		server       *httptest.Server
		token        string
		payload      = []byte("network:\n  version: 2\n  ethernets:\n    boot:\n      dhcp4: true\n")
	)

	setRef := func(ref string) {
		ph := getPhysicalHost(client.ObjectKeyFromObject(physicalHost))
		base := ph.DeepCopy()
		ph.Spec.NetworkDataSecretRef = ref
		Expect(k8sClient.Patch(ctx, ph, client.MergeFrom(base))).To(Succeed())
	}

	putNetworkDataSecret := func(annotatedHost string, data []byte) {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: testNs.Name},
			Data:       map[string][]byte{networkDataSecretKey: data},
		}
		if annotatedHost != "" {
			secret.Annotations = map[string]string{NetworkDataHostAnnotation: annotatedHost}
		}
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())
	}

	get := func(bearer string) *http.Response {
		req, err := http.NewRequest(http.MethodGet,
			fmt.Sprintf("%s/api/v1/network-data/%s/%s", server.URL, testNs.Name, hostName), bytes.NewReader(nil))
		Expect(err).NotTo(HaveOccurred())
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		return resp
	}

	expectStatus := func(bearer string, status int) {
		resp := get(bearer)
		defer func() { _ = resp.Body.Close() }()
		Expect(resp.StatusCode).To(Equal(status))
	}

	BeforeEach(func() {
		testNs = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "network-data-handler-test-"}}
		Expect(k8sClient.Create(ctx, testNs)).To(Succeed())

		physicalHost = &infrav1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{Name: hostName, Namespace: testNs.Name},
			Spec: infrav1.PhysicalHostSpec{
				RedfishConnection: infrav1.RedfishConnection{
					Address:              "https://192.168.77.11",
					CredentialsSecretRef: "irrelevant",
				},
			},
		}
		Expect(k8sClient.Create(ctx, physicalHost)).To(Succeed())

		b7machine := &infrav1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{Name: machineName, Namespace: testNs.Name},
			Spec: infrav1.Beskar7MachineSpec{
				InspectionImageURL: "http://boot-server/inspect.ipxe",
				TargetImageURL:     "http://boot-server/kairos.tar.gz",
				TargetImageDigest:  bootTestDigest,
			},
		}
		Expect(k8sClient.Create(ctx, b7machine)).To(Succeed())

		hostKey := client.ObjectKeyFromObject(physicalHost)
		setHostConsumer(hostKey, machineName)
		var err error
		token, _, err = auth.MintToken()
		Expect(err).NotTo(HaveOccurred())
		putCredentialSecret(hostKey, boundCredentialData(machineName, token, 30*time.Minute, "", 0))

		server = httptest.NewServer(buildNetworkDataMux())
	})

	AfterEach(func() {
		server.Close()
		Expect(k8sClient.Delete(ctx, testNs)).To(Succeed())
	})

	It("rejects a request without a bearer token (401)", func() {
		setRef(secretName)
		putNetworkDataSecret(hostName, payload)
		expectStatus("", http.StatusUnauthorized)
	})

	It("rejects a token that is not the host's (401)", func() {
		setRef(secretName)
		putNetworkDataSecret(hostName, payload)
		other, _, err := auth.MintToken()
		Expect(err).NotTo(HaveOccurred())
		expectStatus(other, http.StatusUnauthorized)
	})

	It("answers 404 when the host has no network data", func() {
		expectStatus(token, http.StatusNotFound)
	})

	It("serves the Secret bytes verbatim, uncached (200)", func() {
		setRef(secretName)
		putNetworkDataSecret(hostName, payload)

		resp := get(token)
		defer func() { _ = resp.Body.Close() }()
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		Expect(resp.Header.Get(headerContentType)).To(Equal(contentTypeOctetStream))
		Expect(resp.Header.Get(headerCacheControl)).To(Equal(cacheControlNoStore))
		body, err := io.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())
		Expect(body).To(Equal(payload))
	})

	It("refuses a Secret that does not name the host (409)", func() {
		setRef(secretName)
		putNetworkDataSecret("another-host", payload)
		expectStatus(token, http.StatusConflict)
	})

	It("refuses a Secret without the opt-in annotation (409)", func() {
		setRef(secretName)
		putNetworkDataSecret("", payload)
		expectStatus(token, http.StatusConflict)
	})

	It("refuses a missing Secret (409)", func() {
		setRef(secretName)
		expectStatus(token, http.StatusConflict)
	})

	It("refuses an oversize value (409)", func() {
		setRef(secretName)
		putNetworkDataSecret(hostName, bytes.Repeat([]byte("a"), maxNetworkDataSize+1))
		expectStatus(token, http.StatusConflict)
	})

	It("serves nothing once the host is released (401)", func() {
		setRef(secretName)
		putNetworkDataSecret(hostName, payload)
		ph := getPhysicalHost(client.ObjectKeyFromObject(physicalHost))
		base := ph.DeepCopy()
		ph.Spec.ConsumerRef = nil
		Expect(k8sClient.Patch(ctx, ph, client.MergeFrom(base))).To(Succeed())
		expectStatus(token, http.StatusUnauthorized)
	})
})
