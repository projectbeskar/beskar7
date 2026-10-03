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
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
	"github.com/projectbeskar/beskar7/internal/auth"
)

// A consumed boot nonce used to re-serve the script, and the bearer token in
// it, to anyone who presented it until the nonce expired, ten minutes after the
// mint: a nonce read off the provisioning network was as good as the host's own
// fetch. It now re-serves only to the client that consumed it, and only for
// two minutes after the consume (D-031): long enough for a NIC to retry, too
// narrow for a captured nonce to be worth anything. Every other fetch gets the
// same opaque failure as a wrong nonce.

const (
	bootTestConsumingClient = "192.0.2.10"
	bootTestOtherClient     = "192.0.2.20"
)

// serveBootFrom serves one /boot request for nonce on handler directly, as if
// it came from clientIP.
func serveBootFrom(handler *BootHandler, namespace, hostName, nonce, clientIP string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/boot/"+namespace+"/"+hostName+"/"+nonce, nil)
	req.RemoteAddr = net.JoinHostPort(clientIP, "40000")
	req.SetPathValue("namespace", namespace)
	req.SetPathValue("hostName", hostName)
	req.SetPathValue("nonce", nonce)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

var _ = Describe("Boot GET handler: re-serving a consumed nonce (D-031)", func() {
	var (
		testNs *corev1.Namespace
		server *httptest.Server
	)

	BeforeEach(func() {
		testNs = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "boot-nonce-reserve-"}}
		Expect(k8sClient.Create(ctx, testNs)).To(Succeed())

		// The test server sees every request from loopback; trusting it as a
		// proxy makes the handler attribute each one to its X-Forwarded-For
		// client, the same client the rate limiter keys on.
		cfg := bootTestConfig()
		proxies, err := ParseTrustedProxies("127.0.0.1,::1")
		Expect(err).NotTo(HaveOccurred())
		cfg.TrustedProxies = proxies
		server = httptest.NewServer(buildBootMux(cfg))
	})

	AfterEach(func() {
		server.Close()
		Expect(k8sClient.Delete(ctx, testNs)).To(Succeed())
	})

	fetchAs := func(clientIP, hostName, nonce string) (int, string) {
		url := fmt.Sprintf("%s/api/v1/boot/%s/%s/%s", server.URL, testNs.Name, hostName, nonce)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("X-Forwarded-For", clientIP)
		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		return resp.StatusCode, readBody(resp)
	}
	hostKey := func(ph *infrav1.PhysicalHost) types.NamespacedName {
		return types.NamespacedName{Namespace: ph.Namespace, Name: ph.Name}
	}
	// ageConsume moves the consume record's time back to age before now, as
	// if the consume had happened then.
	ageConsume := func(ph *infrav1.PhysicalHost, age time.Duration) {
		current := &infrav1.PhysicalHost{}
		Expect(k8sClient.Get(ctx, hostKey(ph), current)).To(Succeed())
		Expect(current.Status.Bootstrap).NotTo(BeNil())
		Expect(current.Status.Bootstrap.BootNonceConsumedAt).NotTo(BeNil())
		at := metav1.NewTime(time.Now().Add(-age))
		current.Status.Bootstrap.BootNonceConsumedAt = &at
		Expect(k8sClient.Status().Update(ctx, current)).To(Succeed())
	}

	It("re-serves the identical script to the client that consumed the nonce", func() {
		ph, _, token, nonce := bootTestFixture(testNs.Name)

		code, first := fetchAs(bootTestConsumingClient, ph.Name, nonce)
		Expect(code).To(Equal(http.StatusOK), first)
		Expect(first).To(ContainSubstring("beskar7.token=" + token))

		code, retry := fetchAs(bootTestConsumingClient, ph.Name, nonce)
		Expect(code).To(Equal(http.StatusOK), retry)
		Expect(retry).To(Equal(first), "a NIC retry gets byte-identical content (§4.1)")
	})

	It("refuses the consumed nonce to any other client, with the opaque failure", func() {
		ph, _, token, nonce := bootTestFixture(testNs.Name)

		code, body := fetchAs(bootTestConsumingClient, ph.Name, nonce)
		Expect(code).To(Equal(http.StatusOK), body)
		consumed := &infrav1.PhysicalHost{}
		Expect(k8sClient.Get(ctx, hostKey(ph), consumed)).To(Succeed())

		code, body = fetchAs(bootTestOtherClient, ph.Name, nonce)
		Expect(code).To(Equal(bootHandlerOpaqueFailureStatus))
		Expect(body).To(Equal(bootHandlerOpaqueFailureBody+"\n"), "the same failure as a wrong nonce: no oracle")
		Expect(body).NotTo(ContainSubstring(token))

		after := &infrav1.PhysicalHost{}
		Expect(k8sClient.Get(ctx, hostKey(ph), after)).To(Succeed())
		Expect(after.ResourceVersion).To(Equal(consumed.ResourceVersion),
			"a refused fetch must not rewrite the consume record in its own favour")

		By("the consuming client still being served")
		code, _ = fetchAs(bootTestConsumingClient, ph.Name, nonce)
		Expect(code).To(Equal(http.StatusOK))
	})

	It("re-serves within two minutes of the consume, and refuses after, even to the consuming client", func() {
		ph, _, token, nonce := bootTestFixture(testNs.Name)
		code, body := fetchAs(bootTestConsumingClient, ph.Name, nonce)
		Expect(code).To(Equal(http.StatusOK), body)

		By("a retry 1m50s after the consume")
		ageConsume(ph, 110*time.Second)
		code, body = fetchAs(bootTestConsumingClient, ph.Name, nonce)
		Expect(code).To(Equal(http.StatusOK), body)

		By("a retry 2m01s after the consume, with the nonce itself still unexpired")
		ageConsume(ph, 121*time.Second)
		code, body = fetchAs(bootTestConsumingClient, ph.Name, nonce)
		Expect(code).To(Equal(bootHandlerOpaqueFailureStatus))
		Expect(body).To(Equal(bootHandlerOpaqueFailureBody + "\n"))
		Expect(body).NotTo(ContainSubstring(token))
	})

	It("refuses a consume record that does not say which client consumed the nonce (written before D-031)", func() {
		ph, _, token, nonce := bootTestFixture(testNs.Name)

		By("a record naming the nonce but no client, as a v0.9 handler wrote it a second ago")
		current := &infrav1.PhysicalHost{}
		Expect(k8sClient.Get(ctx, hostKey(ph), current)).To(Succeed())
		at := metav1.NewTime(time.Now().Add(-time.Second))
		current.Status.Bootstrap = &infrav1.BootstrapStatus{
			BootNonceConsumedAt:   &at,
			BootNonceConsumedHash: auth.Hash(nonce),
		}
		Expect(k8sClient.Status().Update(ctx, current)).To(Succeed())

		code, body := fetchAs(bootTestConsumingClient, ph.Name, nonce)
		Expect(code).To(Equal(bootHandlerOpaqueFailureStatus), "nothing says this client consumed it")
		Expect(body).NotTo(ContainSubstring(token))
	})

	// The consume patch conflicts when another fetch recorded the consume
	// first. The loser re-reads the host and finds the record naming its
	// nonce; it renders only if the winner was its own client.
	DescribeTable("a fetch that loses the consume race",
		func(loserIP string, served bool) {
			nonce, _, err := auth.MintToken()
			Expect(err).NotTo(HaveOccurred())
			ph := &infrav1.PhysicalHost{
				ObjectMeta: metav1.ObjectMeta{Name: "h-race", Namespace: "n", UID: "h-race-uid"},
				Spec: infrav1.PhysicalHostSpec{
					RedfishConnection: infrav1.RedfishConnection{Address: "https://192.168.1.1", CredentialsSecretRef: "x"},
					ConsumerRef: &corev1.ObjectReference{
						Kind: "Beskar7Machine", APIVersion: InfrastructureAPIVersion, Name: "b7m-race", Namespace: "n",
					},
				},
			}
			b7m := &infrav1.Beskar7Machine{
				ObjectMeta: metav1.ObjectMeta{Name: "b7m-race", Namespace: "n"},
				Spec: infrav1.Beskar7MachineSpec{
					InspectionImageURL: "https://boot.example.com/inspect",
					TargetImageURL:     "https://boot.example.com/kairos.raw",
					TargetImageDigest:  bootTestDigest,
				},
			}
			tokenSecret := credentialSecret(ph, boundCredentialData(b7m.Name, "race-token", 30*time.Minute, nonce, 10*time.Minute))

			// The loser's consume patch is held back until the winner — a real
			// fetch from bootTestConsumingClient through the same handler — has
			// recorded its own consume, and then conflicts.
			var handler *BootHandler
			var winner *httptest.ResponseRecorder
			raced := false
			fakeClient := fake.NewClientBuilder().
				WithScheme(k8sClient.Scheme()).
				WithObjects(b7m, tokenSecret).
				WithStatusSubresource(ph).
				WithObjects(ph).
				WithInterceptorFuncs(interceptor.Funcs{
					SubResourcePatch: func(ctx context.Context, c client.Client, subResource string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
						if raced {
							return c.SubResource(subResource).Patch(ctx, obj, patch, opts...)
						}
						raced = true
						winner = serveBootFrom(handler, "n", ph.Name, nonce, bootTestConsumingClient)
						return apierrors.NewConflict(infrav1.GroupVersion.WithResource("physicalhosts").GroupResource(),
							obj.GetName(), errors.New("the object has been modified"))
					},
				}).
				Build()
			handler = &BootHandler{Client: fakeClient, Log: ctrl.Log.WithName("boot-race-test"), Config: bootTestConfig()}

			loser := serveBootFrom(handler, "n", ph.Name, nonce, loserIP)

			Expect(raced).To(BeTrue(), "the loser must have tried to record the consume")
			Expect(winner.Code).To(Equal(http.StatusOK), winner.Body.String())
			if served {
				Expect(loser.Code).To(Equal(http.StatusOK), loser.Body.String())
				Expect(loser.Body.String()).To(Equal(winner.Body.String()), "the race loser renders identically (§4.1)")
				return
			}
			Expect(loser.Code).To(Equal(bootHandlerOpaqueFailureStatus))
			Expect(loser.Body.String()).NotTo(ContainSubstring("race-token"))
		},
		Entry("from the winner's own client renders the identical script", bootTestConsumingClient, true),
		Entry("from another client gets the opaque failure", bootTestOtherClient, false),
	)
})
