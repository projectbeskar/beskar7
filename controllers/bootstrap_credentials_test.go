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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
	"github.com/projectbeskar/beskar7/internal/auth"
)

// Pure unit specs for the per-host bootstrap-token Secret (D-029); the
// envtest specs in callback_credentials_test.go drive it end to end.
var _ = Describe("bootstrap-token Secret credentials", func() {
	It("round-trips through the Secret, leaving keys it does not own alone", func() {
		issued := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
		want := bootstrapCredentials{
			token: "t", tokenIssuedAt: issued, tokenExpiresAt: issued.Add(time.Hour),
			nonce: "n", nonceExpiresAt: issued.Add(10 * time.Minute),
			consumer: "machine",
		}
		secret := &corev1.Secret{Data: map[string][]byte{"operator-note": []byte("kept")}}
		want.writeTo(secret)
		Expect(readBootstrapCredentials(secret)).To(Equal(want))
		Expect(secret.Data).To(HaveKeyWithValue("operator-note", []byte("kept")))
		Expect(secret.Data).To(HaveKeyWithValue(bootstrapTokenExpiresAtSecretKey, []byte("2026-09-25T13:00:00Z")))

		By("removing the key of a field that is empty")
		want.nonce, want.nonceExpiresAt = "", time.Time{}
		want.writeTo(secret)
		Expect(secret.Data).NotTo(HaveKey(bootNonceSecretKey))
		Expect(secret.Data).NotTo(HaveKey(bootNonceExpiresAtSecretKey))
	})

	It("reads a missing or malformed expiry as absent, so the credential never validates", func() {
		now := time.Now()
		for _, raw := range [][]byte{nil, []byte(""), []byte("soon"), []byte("2026-13-45T99:00:00Z"), []byte("1790000000")} {
			secret := &corev1.Secret{Data: map[string][]byte{
				bootstrapTokenSecretKey: []byte("t"),
				bootNonceSecretKey:      []byte("n"),
			}}
			if raw != nil {
				secret.Data[bootstrapTokenExpiresAtSecretKey] = raw
				secret.Data[bootNonceExpiresAtSecretKey] = raw
			}
			creds := readBootstrapCredentials(secret)
			Expect(creds.tokenValid(now)).To(BeFalse(), "token expiry %q", raw)
			Expect(creds.nonceValid(now)).To(BeFalse(), "nonce expiry %q", raw)
			Expect(verifyBootNonce("n", creds, now)).To(BeFalse(), "nonce expiry %q", raw)
		}
	})

	It("verifies only the Secret's own nonce, and only before its expiry", func() {
		now := time.Now()
		creds := bootstrapCredentials{nonce: "the-nonce", nonceExpiresAt: now.Add(time.Minute)}
		Expect(verifyBootNonce("the-nonce", creds, now)).To(BeTrue())
		Expect(verifyBootNonce("another-nonce", creds, now)).To(BeFalse())
		Expect(verifyBootNonce("", creds, now)).To(BeFalse())
		Expect(verifyBootNonce("the-nonce", creds, now.Add(time.Minute))).To(BeFalse(), "expired at equality")
	})

	Describe("the consume record's client (D-031)", func() {
		consumedAt := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
		record := func(clientHash string) *infrav1.BootstrapStatus {
			at := metav1.NewTime(consumedAt)
			return &infrav1.BootstrapStatus{
				BootNonceConsumedAt: &at, BootNonceConsumedHash: auth.Hash("the-nonce"),
				BootNonceConsumedClientHash: clientHash,
			}
		}

		It("identifies the client with a hash keyed by the nonce, so status reveals nothing about the address", func() {
			h := bootNonceClientHash("the-nonce", "192.0.2.10")
			Expect(h).To(HaveLen(64))
			Expect(h).NotTo(Equal(auth.Hash("192.0.2.10")), "an unkeyed hash of an IPv4 address is reversed by enumeration")
			Expect(bootNonceClientHash("another-nonce", "192.0.2.10")).NotTo(Equal(h))
			Expect(bootNonceClientHash("the-nonce", "192.0.2.20")).NotTo(Equal(h))
			Expect(bootNonceClientHash("the-nonce", "2001:db8:0:0::1")).To(Equal(bootNonceClientHash("the-nonce", "2001:db8::1")),
				"one address written two ways is one client")
		})

		It("re-serves only the consuming client, and only before two minutes have passed", func() {
			client := bootNonceClientHash("the-nonce", "192.0.2.10")
			bs := record(client)
			Expect(bootNonceRetryAllowed(bs, client, consumedAt)).To(BeTrue())
			Expect(bootNonceRetryAllowed(bs, client, consumedAt.Add(2*time.Minute-time.Second))).To(BeTrue())
			Expect(bootNonceRetryAllowed(bs, client, consumedAt.Add(2*time.Minute))).To(BeFalse(), "boundary: the window has closed")
			Expect(bootNonceRetryAllowed(bs, bootNonceClientHash("the-nonce", "192.0.2.20"), consumedAt)).To(BeFalse())
		})

		It("re-serves nobody from a record that names no client, or from no record", func() {
			Expect(bootNonceRetryAllowed(record(""), "", consumedAt)).To(BeFalse())
			Expect(bootNonceRetryAllowed(record(""), bootNonceClientHash("the-nonce", "192.0.2.10"), consumedAt)).To(BeFalse())
			Expect(bootNonceRetryAllowed(nil, bootNonceClientHash("the-nonce", "192.0.2.10"), consumedAt)).To(BeFalse())
		})
	})

	Describe("boundBootstrapCredentials", func() {
		const ns, hostName, machine = "bound-ns", "bound-host", "bound-machine"
		const hostUID = types.UID("bound-host-uid")
		claimed := func(consumer, consumerNamespace string) *infrav1.PhysicalHost {
			host := &infrav1.PhysicalHost{ObjectMeta: metav1.ObjectMeta{Name: hostName, Namespace: ns, UID: hostUID}}
			if consumer != "" {
				host.Spec.ConsumerRef = &corev1.ObjectReference{
					Kind: "Beskar7Machine", APIVersion: InfrastructureAPIVersion,
					Name: consumer, Namespace: consumerNamespace,
				}
			}
			return host
		}
		reader := func(objs ...client.Object) client.Reader {
			return fake.NewClientBuilder().WithScheme(k8sClient.Scheme()).WithObjects(objs...).Build()
		}
		bound := func() map[string][]byte {
			return boundCredentialData(machine, "t", time.Hour, "n", time.Minute)
		}
		// ownedBy returns the host as another object of the same name: one
		// deleted and recreated, or recreated by clusterctl move.
		ownedBy := func(uid types.UID) *infrav1.PhysicalHost {
			host := claimed(machine, ns)
			host.UID = uid
			return host
		}
		secretFor := func(kind string) []client.Object {
			switch kind {
			case "bound":
				return []client.Object{credentialSecret(claimed(machine, ns), bound())}
			case "unbound":
				return []client.Object{credentialSecret(claimed(machine, ns), map[string][]byte{bootstrapTokenSecretKey: []byte("t")})}
			case "unowned":
				return []client.Object{unownedCredentialSecret(ns, hostName, bound())}
			case "controlled by another host of the name":
				return []client.Object{credentialSecret(ownedBy("uid-of-a-deleted-host"), bound())}
			case "owned, but not as its controller":
				secret := unownedCredentialSecret(ns, hostName, bound())
				Expect(controllerutil.SetOwnerReference(claimed(machine, ns), secret, k8sClient.Scheme())).To(Succeed())
				return []client.Object{secret}
			}
			return nil
		}

		It("returns the credentials and the consumer when the host's claim names the machine they are bound to", func() {
			creds, key, err := boundBootstrapCredentials(ctx, reader(secretFor("bound")...), claimed(machine, ns))
			Expect(err).NotTo(HaveOccurred())
			Expect(key).To(Equal(types.NamespacedName{Namespace: ns, Name: machine}))
			Expect(creds.token).To(Equal("t"))
			Expect(creds.nonce).To(Equal("n"))
		})

		DescribeTable("refuses",
			func(consumer, consumerNamespace, secret string) {
				_, _, err := boundBootstrapCredentials(ctx, reader(secretFor(secret)...), claimed(consumer, consumerNamespace))
				Expect(err).To(HaveOccurred())
			},
			Entry("a host nobody claims", "", "", "bound"),
			Entry("a claim naming another machine", "other-machine", ns, "bound"),
			Entry("a claim naming another namespace (SEC-12)", machine, "other-ns", "bound"),
			Entry("a host without a Secret", machine, ns, "none"),
			Entry("a Secret without a binding", machine, ns, "unbound"),
			// D-031: the name is deterministic, so anyone who can create a
			// Secret in the namespace can put one there before the manager does.
			Entry("a Secret no controller owns", machine, ns, "unowned"),
			Entry("a Secret controlled by an earlier host of the same name", machine, ns, "controlled by another host of the name"),
			Entry("a Secret that names the host as an owner but not as its controller", machine, ns, "owned, but not as its controller"),
		)

		It("accepts the Secret once clusterctl move has rewritten its owner to the moved host, and only then", func() {
			moved := ownedBy("uid-on-the-target-cluster")
			_, _, err := boundBootstrapCredentials(ctx, reader(credentialSecret(moved, bound())), moved)
			Expect(err).NotTo(HaveOccurred(), "the mover rewrites ownerRef UIDs to the recreated owner's")

			_, _, err = boundBootstrapCredentials(ctx, reader(credentialSecret(claimed(machine, ns), bound())), moved)
			Expect(err).To(HaveOccurred(), "an ownerRef still naming the source host's UID names another object")
		})

		It("refuses a host without a UID, whatever the Secret's owner says", func() {
			host := ownedBy("")
			_, _, err := boundBootstrapCredentials(ctx, reader(credentialSecret(host, bound())), host)
			Expect(err).To(HaveOccurred())
		})
	})
})
