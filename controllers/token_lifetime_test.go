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
	"net/http"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
	"github.com/projectbeskar/beskar7/internal/auth"
	internalredfish "github.com/projectbeskar/beskar7/internal/redfish"
)

// D-031. The bearer token lived its full mint lifetime whatever happened to the
// run: it kept authenticating callbacks for up to an hour after the host was
// Ready, /boot could hand out a token about to expire mid-inspection, and the
// mint took over any Secret that happened to sit under the host's
// bootstrap-token name. These specs drive the Beskar7Machine controller's
// credential paths against envtest.
var _ = Describe("Bearer token lifetime and the bootstrap-token Secret's owner (D-031)", func() {
	var (
		testNs  *corev1.Namespace
		hostKey client.ObjectKey
		machine *infrav1.Beskar7Machine
		r       *Beskar7MachineReconciler
	)

	BeforeEach(func() {
		testNs = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "token-lifetime-"}}
		Expect(k8sClient.Create(ctx, testNs)).To(Succeed())
		host := claimedPhysicalHost(testNs.Name, "lifetime-host", "lifetime-machine")
		Expect(k8sClient.Create(ctx, host)).To(Succeed())
		hostKey = client.ObjectKeyFromObject(host)
		machine = &infrav1.Beskar7Machine{ObjectMeta: metav1.ObjectMeta{
			Name: "lifetime-machine", Namespace: testNs.Name, UID: "lifetime-machine-uid",
		}}
		r = &Beskar7MachineReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			Log: ctrl.Log.WithName("token-lifetime"),
			RedfishClientFactory: func(context.Context, string, string, string, bool, []byte) (internalredfish.Client, error) {
				return internalredfish.NewMockClient(), nil
			},
			BootstrapURLBase: "https://callback.example.com:8082",
		}
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, testNs)).To(Succeed())
	})

	secretKey := func() types.NamespacedName {
		return types.NamespacedName{Namespace: hostKey.Namespace, Name: bootstrapTokenSecretName(hostKey.Name)}
	}
	getSecret := func() *corev1.Secret {
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, secretKey(), secret)).To(Succeed())
		return secret
	}
	// boundTo returns Secret data binding a token with the given remaining life
	// to machine, the way its own mint would have.
	boundTo := func(token string, remaining time.Duration) map[string][]byte {
		data := boundCredentialData(machine.Name, token, remaining, "", 0)
		data[bootstrapConsumerUIDSecretKey] = []byte(machine.UID)
		return data
	}
	readyHost := func() *infrav1.PhysicalHost {
		host := getPhysicalHost(hostKey)
		host.Status.State = infrav1.StateReady
		return host
	}

	Context("when the host is Ready", func() {
		It("cuts the token's life to five minutes, keeps the token itself, and writes the Secret once", func() {
			Expect(r.ensureBootstrapCredentials(ctx, r.Log, machine, getPhysicalHost(hostKey), time.Now())).To(Succeed())
			minted := readBootstrapCredentials(getSecret())
			Expect(minted.tokenExpiresAt).To(BeTemporally(">", time.Now().Add(50*time.Minute)))

			readyAt := time.Now()
			_, err := r.handleReadyHost(ctx, r.Log, machine, readyHost())
			Expect(err).NotTo(HaveOccurred())
			cut := getSecret()
			creds := readBootstrapCredentials(cut)
			Expect(creds.tokenExpiresAt).To(BeTemporally("~", readyAt.Add(5*time.Minute), 2*time.Second),
				"the grace covers the inspector's /provisioned retries, and no more")
			Expect(creds.token).To(Equal(minted.token), "the token itself is not replaced")
			Expect(creds.tokenIssuedAt).To(Equal(minted.tokenIssuedAt))
			Expect(creds.nonce).To(Equal(minted.nonce))
			Expect(creds.nonceExpiresAt).To(Equal(minted.nonceExpiresAt))
			Expect(creds.consumer).To(Equal(minted.consumer))
			Expect(creds.consumerUID).To(Equal(minted.consumerUID))

			By("a later pass over the Ready host writing nothing")
			_, err = r.handleReadyHost(ctx, r.Log, machine, readyHost())
			Expect(err).NotTo(HaveOccurred())
			Expect(getSecret().ResourceVersion).To(Equal(cut.ResourceVersion))
		})

		It("still accepts the inspector's /provisioned retry within the grace, and nothing once it has passed", func() {
			Expect(r.ensureBootstrapCredentials(ctx, r.Log, machine, getPhysicalHost(hostKey), time.Now())).To(Succeed())
			token := readBootstrapCredentials(getSecret()).token
			_, err := r.handleReadyHost(ctx, r.Log, machine, readyHost())
			Expect(err).NotTo(HaveOccurred())

			verify := newBearerTokenVerifier(k8sClient, ctrl.Log.WithName("token-lifetime-verifier"))
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/provisioned/"+hostKey.Namespace+"/"+hostKey.Name, nil)
			Expect(err).NotTo(HaveOccurred())
			req.SetPathValue("namespace", hostKey.Namespace)
			req.SetPathValue("hostName", hostKey.Name)
			Expect(verify(token, req)).To(Succeed(), "a retry whose first 202 was lost must still authenticate")

			By("the grace running out")
			secret := getSecret()
			secret.Data[bootstrapTokenExpiresAtSecretKey] = credentialTime(time.Now().Add(-time.Second))
			Expect(k8sClient.Update(ctx, secret)).To(Succeed())
			Expect(verify(token, req)).NotTo(Succeed())
		})

		DescribeTable("never lengthens a token's life, nor gives one a life it does not have",
			func(data map[string][]byte) {
				putCredentialSecret(hostKey, data)
				before := getSecret()
				_, err := r.handleReadyHost(ctx, r.Log, machine, readyHost())
				Expect(err).NotTo(HaveOccurred())
				after := getSecret()
				Expect(after.ResourceVersion).To(Equal(before.ResourceVersion))
				Expect(after.Data).To(Equal(before.Data))
			},
			Entry("a token that expires sooner than the grace", func() map[string][]byte {
				return boundCredentialData("lifetime-machine", "short-token", 2*time.Minute, "", 0)
			}()),
			Entry("a token whose expiry is missing, which never verifies", func() map[string][]byte {
				data := boundCredentialData("lifetime-machine", "no-expiry-token", time.Hour, "", 0)
				delete(data, bootstrapTokenExpiresAtSecretKey)
				return data
			}()),
			Entry("a token bound to another machine, which authenticates nothing here", func() map[string][]byte {
				return boundCredentialData("another-machine", "foreign-token", time.Hour, "", 0)
			}()),
		)
	})

	Context("when deciding whether a token can be handed out again", func() {
		It("re-mints a token with less life left than a boot nonce plus an inspection", func() {
			putCredentialSecret(hostKey, boundTo("nearly-spent-token", 15*time.Minute))
			now := time.Now()
			Expect(r.ensureBootstrapCredentials(ctx, r.Log, machine, getPhysicalHost(hostKey), now)).To(Succeed())
			creds := readBootstrapCredentials(getSecret())
			Expect(creds.token).NotTo(Equal("nearly-spent-token"),
				"10 minutes of nonce plus 10 of inspection would outlast it: /boot must not render it")
			Expect(creds.tokenExpiresAt).To(BeTemporally("~", now.Add(auth.TokenLifetime), 2*time.Second))
		})

		It("keeps a token with more life left than that", func() {
			putCredentialSecret(hostKey, boundTo("healthy-token", 25*time.Minute))
			Expect(r.ensureBootstrapCredentials(ctx, r.Log, machine, getPhysicalHost(hostKey), time.Now())).To(Succeed())
			Expect(readBootstrapCredentials(getSecret()).token).To(Equal("healthy-token"))
		})

		It("measures the margin with the configured inspection timeout", func() {
			r.InspectionTimeout = 30 * time.Minute
			putCredentialSecret(hostKey, boundTo("thirty-five-minutes-left", 35*time.Minute))
			Expect(r.ensureBootstrapCredentials(ctx, r.Log, machine, getPhysicalHost(hostKey), time.Now())).To(Succeed())
			Expect(readBootstrapCredentials(getSecret()).token).NotTo(Equal("thirty-five-minutes-left"),
				"10 minutes of nonce plus a 30-minute inspection would outlast it")

			putCredentialSecret(hostKey, boundTo("forty-five-minutes-left", 45*time.Minute))
			Expect(r.ensureBootstrapCredentials(ctx, r.Log, machine, getPhysicalHost(hostKey), time.Now())).To(Succeed())
			Expect(readBootstrapCredentials(getSecret()).token).To(Equal("forty-five-minutes-left"))
		})

		It("mints a token that outlasts a long inspection timeout, and keeps it on the next pass", func() {
			r.InspectionTimeout = 55 * time.Minute
			now := time.Now()
			Expect(r.ensureBootstrapCredentials(ctx, r.Log, machine, getPhysicalHost(hostKey), now)).To(Succeed())
			minted := getSecret()
			Expect(readBootstrapCredentials(minted).tokenExpiresAt).To(
				BeTemporally("~", now.Add(auth.TokenLifetime+45*time.Minute), 2*time.Second),
				"the mint lengthens the token by however much the inspection timeout exceeds its default")

			Expect(r.ensureBootstrapCredentials(ctx, r.Log, machine, getPhysicalHost(hostKey), time.Now())).To(Succeed())
			Expect(getSecret().ResourceVersion).To(Equal(minted.ResourceVersion),
				"a token re-minted on every pass would strand an inspector already holding it (D-024)")
		})
	})

	Context("when a Secret the host does not own sits under its bootstrap-token name", func() {
		DescribeTable("the mint refuses to take it over and leaves it as it is",
			func(owner func() []metav1.OwnerReference) {
				squatter := unownedCredentialSecret(hostKey.Namespace, hostKey.Name, map[string][]byte{
					"operator-note":         []byte("not the manager's"),
					bootstrapTokenSecretKey: []byte("squatter-token"),
				})
				squatter.OwnerReferences = owner()
				Expect(k8sClient.Create(ctx, squatter)).To(Succeed())
				before := getSecret()

				err := r.ensureBootstrapCredentials(ctx, r.Log, machine, getPhysicalHost(hostKey), time.Now())
				Expect(err).To(HaveOccurred())

				after := getSecret()
				Expect(after.ResourceVersion).To(Equal(before.ResourceVersion))
				Expect(after.OwnerReferences).To(Equal(before.OwnerReferences), "never adopted")
				Expect(after.Data).To(Equal(before.Data))
			},
			Entry("one no controller owns", func() []metav1.OwnerReference { return nil }),
			Entry("one an earlier host of the same name controlled", func() []metav1.OwnerReference {
				return []metav1.OwnerReference{{
					APIVersion: infrav1.GroupVersion.String(), Kind: "PhysicalHost",
					Name: hostKey.Name, UID: "uid-of-a-deleted-host",
					Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true),
				}}
			}),
		)

		It("is refused by the Secret write itself, whichever path computed the write", func() {
			Expect(k8sClient.Create(ctx, unownedCredentialSecret(hostKey.Namespace, hostKey.Name, map[string][]byte{
				"operator-note": []byte("not the manager's"),
			}))).To(Succeed())
			before := getSecret()

			err := r.writeBootstrapCredentials(ctx, r.Log, getPhysicalHost(hostKey), before.DeepCopy(),
				readBootstrapCredentials(&corev1.Secret{Data: boundTo("would-be-token", time.Hour)}))
			Expect(errors.Is(err, errBootstrapSecretNotOwned)).To(BeTrue(), "got %v", err)
			after := getSecret()
			Expect(after.ResourceVersion).To(Equal(before.ResourceVersion))
			Expect(after.OwnerReferences).To(BeEmpty())
		})
	})
})
