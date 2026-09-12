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
	"net/http"
	"net/http/httptest"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
	"github.com/projectbeskar/beskar7/internal/auth"
)

// Status.Bootstrap outlives a claim: a released host keeps it, and the next
// Beskar7Machine to claim the host mints its credentials into it. The boot
// nonce's consume record in it is written by the /boot handler and cleared by
// nothing, so the record a host's first boot left used to be inherited by every
// nonce minted after it. /boot took each new nonce for one it had already
// consumed and never recorded its consume, and the Beskar7Machine counted the
// nonce spent from the moment the host promoted it, so a repeated trigger
// replaced a nonce the operator's boot service may already have handed out.
// These specs run two provisioning cycles on one host through the real
// Beskar7Machine mint, PhysicalHostReconciler.Reconcile and BootHandler.
var _ = Describe("Boot nonce across two provisioning cycles of one host", func() {
	var (
		ns       *corev1.Namespace
		server   *httptest.Server
		hostR    *PhysicalHostReconciler
		machineR *Beskar7MachineReconciler
		hostKey  client.ObjectKey
	)

	BeforeEach(func() {
		ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "boot-nonce-cycles-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		credentials := bmcCredentialsSecret(ns.Name)
		Expect(k8sClient.Create(ctx, credentials)).To(Succeed())
		server = httptest.NewServer(buildBootMux(bootTestConfig()))

		hostR = &PhysicalHostReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			Log:                  ctrl.Log.WithName("boot-nonce-cycles-host"),
			Recorder:             record.NewFakeRecorder(10),
			RedfishClientFactory: reachableBMC(),
		}
		machineR = &Beskar7MachineReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			Log:                  ctrl.Log.WithName("boot-nonce-cycles-machine"),
			RedfishClientFactory: reachableBMC(),
		}

		host := &infrav1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{Name: "recycled-host", Namespace: ns.Name},
			Spec: infrav1.PhysicalHostSpec{RedfishConnection: infrav1.RedfishConnection{
				Address:              "https://mock-redfish.example.invalid:8443",
				CredentialsSecretRef: credentials.Name,
			}},
		}
		Expect(k8sClient.Create(ctx, host)).To(Succeed())
		hostKey = client.ObjectKeyFromObject(host)
		Expect(settlePhysicalHost(hostR, hostKey).Status.State).To(Equal(infrav1.StateAvailable))
	})

	AfterEach(func() {
		server.Close()
		Expect(k8sClient.Delete(ctx, ns)).To(Succeed())
	})

	// claim hands the host to a new Beskar7Machine, as a re-claim after release
	// or a MachineHealthCheck's replacement does, by setting its ConsumerRef.
	claim := func(machineName string) *infrav1.Beskar7Machine {
		machine := &infrav1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{Name: machineName, Namespace: ns.Name},
			Spec: infrav1.Beskar7MachineSpec{
				InspectionImageURL: "https://boot.example.com/inspect",
				TargetImageURL:     "https://boot.example.com/kairos.raw",
				TargetImageDigest:  bootTestDigest,
			},
		}
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())
		host := getPhysicalHost(hostKey)
		claimed := host.DeepCopy()
		claimed.Spec.ConsumerRef = &corev1.ObjectReference{
			Kind: "Beskar7Machine", APIVersion: InfrastructureAPIVersion,
			Name: machine.Name, Namespace: machine.Namespace, UID: machine.UID,
		}
		Expect(k8sClient.Patch(ctx, claimed, client.MergeFrom(host))).To(Succeed())
		Expect(settlePhysicalHost(hostR, hostKey).Status.State).To(Equal(infrav1.StateInUse))
		return machine
	}

	// secretNonce returns the boot nonce the per-host Secret holds: the one the
	// operator's boot service puts in the host's /boot URL.
	secretNonce := func() string {
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: bootstrapTokenSecretName(hostKey.Name)}, secret)).To(Succeed())
		return string(secret.Data[bootNonceSecretKey])
	}

	// startInspection runs the machine's triggerInspection on the host as
	// persisted, has the host reconciler take up what it signalled, and returns
	// the nonce the host now advertises.
	startInspection := func(machine *infrav1.Beskar7Machine) string {
		_, err := machineR.triggerInspection(ctx, machineR.Log, machine, getPhysicalHost(hostKey))
		Expect(err).NotTo(HaveOccurred())
		host := settlePhysicalHost(hostR, hostKey)
		Expect(host.Status.State).To(Equal(infrav1.StateInspecting))
		Expect(host.Annotations).NotTo(HaveKey(BootNonceAnnotation), "the host has taken up the mint")
		nonce := secretNonce()
		Expect(auth.Verify(nonce, host.Status.Bootstrap.BootNonceHash)).To(BeTrue(),
			"the host advertises the nonce the Secret holds")
		return nonce
	}

	// fetchBoot asks /boot for the host's script with nonce, and returns the
	// response with the host as persisted afterwards.
	fetchBoot := func(nonce string) (int, string, *infrav1.PhysicalHost) {
		resp := doBoot(server.URL, ns.Name, hostKey.Name, nonce)
		body := readBody(resp)
		return resp.StatusCode, body, getPhysicalHost(hostKey)
	}

	// bootTwice fetches the script with nonce, then again as a NIC retry would.
	// The first fetch must write the consume record — later than previous, the
	// record an earlier cycle left, when one is given — and the retry must get
	// the same script without writing anything. Returns the record's time.
	bootTwice := func(nonce string, previous *metav1.Time) *metav1.Time {
		unfetched := getPhysicalHost(hostKey)

		code, script, fetched := fetchBoot(nonce)
		Expect(code).To(Equal(http.StatusOK))
		Expect(script).To(ContainSubstring("beskar7.token="))
		Expect(fetched.ResourceVersion).NotTo(Equal(unfetched.ResourceVersion),
			"the first fetch of a nonce records that the nonce was consumed")
		consumedAt := fetched.Status.Bootstrap.BootNonceConsumedAt
		Expect(consumedAt).NotTo(BeNil())
		if previous != nil {
			Expect(consumedAt.After(previous.Time)).To(BeTrue(),
				"the record must describe this nonce's fetch, not the previous cycle's (recorded %s, previous %s)",
				consumedAt, previous)
		}

		code, retried, afterRetry := fetchBoot(nonce)
		Expect(code).To(Equal(http.StatusOK),
			"a retry with the same nonce within its lifetime is served the same script (contract §4.1)")
		Expect(retried).To(Equal(script))
		Expect(afterRetry.ResourceVersion).To(Equal(fetched.ResourceVersion),
			"a retry does not consume the nonce a second time")
		return consumedAt
	}

	It("records the first /boot fetch of each cycle's nonce as that nonce's consume", func() {
		By("cycle 1: the first claim mints a nonce, and the host boots with it")
		firstNonce := startInspection(claim("first-machine"))
		firstConsumedAt := bootTwice(firstNonce, nil)

		By("releasing the host, which keeps Status.Bootstrap and the consume record in it")
		releasePhysicalHost(hostKey)
		released := settlePhysicalHost(hostR, hostKey)
		Expect(released.Status.State).To(Equal(infrav1.StateAvailable))
		Expect(released.Status.Bootstrap.BootNonceConsumedAt).NotTo(BeNil())

		By("cycle 2: the next claim mints a fresh nonce")
		secondNonce := startInspection(claim("second-machine"))
		Expect(secondNonce).NotTo(Equal(firstNonce))
		code, _, _ := fetchBoot(firstNonce)
		Expect(code).To(Equal(http.StatusNotFound), "the first cycle's nonce no longer boots the host")

		// metav1.Time keeps whole seconds; a boot in the same second as the
		// first cycle's would leave a record indistinguishable from it.
		By("booting with the fresh nonce a second later than the first cycle's record")
		time.Sleep(time.Until(firstConsumedAt.Add(time.Second)))
		bootTwice(secondNonce, firstConsumedAt)
	})

	It("counts a re-claimed host's fresh nonce as unspent until it is fetched", func() {
		By("cycle 1: claim, mint, and boot")
		firstNonce := startInspection(claim("first-machine"))
		code, _, _ := fetchBoot(firstNonce)
		Expect(code).To(Equal(http.StatusOK))
		releasePhysicalHost(hostKey)
		Expect(settlePhysicalHost(hostR, hostKey).Status.State).To(Equal(infrav1.StateAvailable))

		By("cycle 2: the re-claim's nonce, promoted but not fetched yet")
		machine := claim("second-machine")
		secondNonce := startInspection(machine)
		promoted := getPhysicalHost(hostKey)
		Expect(unexpiredBootNonceHash(promoted, time.Now())).To(Equal(promoted.Status.Bootstrap.BootNonceHash),
			"a nonce nothing has fetched is not spent, whatever an earlier nonce's consume record says")

		By("triggering inspection again, which must keep the nonce the boot service may already have handed out")
		_, err := machineR.triggerInspection(ctx, machineR.Log, machine, promoted)
		Expect(err).NotTo(HaveOccurred())
		Expect(secretNonce()).To(Equal(secondNonce))
		Expect(getPhysicalHost(hostKey).Annotations).NotTo(HaveKey(BootNonceAnnotation), "no fresh nonce is advertised")

		By("fetching it, after which it counts as spent")
		code, _, fetched := fetchBoot(secondNonce)
		Expect(code).To(Equal(http.StatusOK))
		Expect(unexpiredBootNonceHash(fetched, time.Now())).To(BeEmpty(),
			"a consumed nonce is never reused: the next trigger mints a fresh one")
	})
})
