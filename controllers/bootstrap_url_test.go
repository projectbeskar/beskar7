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
	"fmt"

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

// PhysicalHost.Status.Bootstrap.URL used to be copied from an annotation. The
// Beskar7Machine controller computed the URL and left it in
// infrastructure.cluster.x-k8s.io/bootstrap-url, and the PhysicalHost reconciler
// copied whatever the annotation held, with no check on who wrote it: the one
// annotation the reconciler turned into status without a binding (D-034,
// D-037). The URL depends only on --bootstrap-url-base and the host's name, so
// the reconciler now derives it itself and the annotation is retired. These
// specs pin the three things that follow: the legitimate flow still publishes
// the URL, an annotation changes nothing, and the URL's timing and lifetime are
// the ones status.bootstrap always had.

var _ = Describe("PhysicalHost.Status.Bootstrap.URL is derived by the reconciler, not copied from an annotation", func() {
	const callbackBase = "https://callback.example.com:8082"

	var (
		ns       *corev1.Namespace
		hostR    *PhysicalHostReconciler
		machineR *Beskar7MachineReconciler
	)

	BeforeEach(func() {
		ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "bootstrap-url-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		Expect(k8sClient.Create(ctx, bmcCredentialsSecret(ns.Name))).To(Succeed())
		hostR = &PhysicalHostReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			Log:                  ctrl.Log.WithName("bootstrap-url-host"),
			Recorder:             record.NewFakeRecorder(100),
			RedfishClientFactory: reachableBMC(),
			BootstrapURLBase:     callbackBase,
		}
		machineR = &Beskar7MachineReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			Log:                  ctrl.Log.WithName("bootstrap-url-machine"),
			RedfishClientFactory: reachableBMC(),
		}
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, ns)).To(Succeed())
	})

	expectedURL := func(key client.ObjectKey) string {
		return fmt.Sprintf("%s/api/v1/bootstrap/%s/%s", callbackBase, key.Namespace, key.Name)
	}

	// claimedUnminted creates a claimed InUse host whose machine has not minted
	// its credentials yet, and returns it with the machine, the host as it stands
	// before the machine's first pass.
	claimedUnminted := func(name string) (client.ObjectKey, *infrav1.Beskar7Machine) {
		machine, _ := consumerWithBootstrapData(ns.Name, name+"-machine", "BOOTSTRAP-URL-"+name)
		return provisioningHost(ns.Name, name, machine.Name, infrav1.StateInUse, nil), machine
	}

	// mint has the machine mint the host's credentials through triggerInspection,
	// as its first pass over a claimed host does.
	mint := func(key client.ObjectKey, machine *infrav1.Beskar7Machine) {
		_, err := machineR.triggerInspection(ctx, machineR.Log, machine, getPhysicalHost(key))
		Expect(err).NotTo(HaveOccurred())
	}

	It("publishes the URL together with the credentials' hashes once the machine has minted them, and not before", func() {
		key, machine := claimedUnminted("url-flow-host")

		By("a claimed host whose machine has minted nothing yet: no URL, no hashes")
		Expect(settlePhysicalHost(hostR, key).Status.Bootstrap).To(BeNil())

		By("the machine minting the credentials, and the host taking them up")
		mint(key, machine)
		host := settlePhysicalHost(hostR, key)

		Expect(host.Status.Bootstrap).NotTo(BeNil())
		Expect(host.Status.Bootstrap.URL).To(Equal(expectedURL(key)))
		Expect(host.Status.Bootstrap.TokenHash).To(Equal(auth.Hash(callbackTokenOf(key))),
			"the URL and the token hash appear in the same write, which is what cmd/mock-inspector waits for")
		Expect(host.Annotations).NotTo(HaveKey(BootstrapURLAnnotation), "nothing signals the URL through an annotation")
	})

	It("follows --bootstrap-url-base, whatever the trailing slash", func() {
		key, machine := claimedUnminted("url-base-host")
		mint(key, machine)
		Expect(settlePhysicalHost(hostR, key).Status.Bootstrap.URL).To(Equal(expectedURL(key)))

		hostR.BootstrapURLBase = "https://boot.example.net/"
		Expect(settlePhysicalHost(hostR, key).Status.Bootstrap.URL).To(
			Equal(fmt.Sprintf("https://boot.example.net/api/v1/bootstrap/%s/%s", key.Namespace, key.Name)))
	})

	It("keeps the URL when the host is released, as the status always did", func() {
		key, machine := claimedUnminted("url-release-host")
		mint(key, machine)
		Expect(settlePhysicalHost(hostR, key).Status.Bootstrap.URL).To(Equal(expectedURL(key)))

		releasePhysicalHost(key)
		released := settlePhysicalHost(hostR, key)
		Expect(released.Spec.ConsumerRef).To(BeNil())
		Expect(released.Status.Bootstrap).NotTo(BeNil())
		Expect(released.Status.Bootstrap.URL).To(Equal(expectedURL(key)))
	})

	DescribeTable("a hand-set bootstrap-url annotation changes nothing and is removed",
		func(setup func(key client.ObjectKey, machine *infrav1.Beskar7Machine) string) {
			key, machine := claimedUnminted("url-forged-host")
			want := setup(key, machine)

			annotateHost(key, BootstrapURLAnnotation, "https://attacker.example.com/api/v1/bootstrap/other/host")
			host := settlePhysicalHost(hostR, key)

			Expect(host.Annotations).NotTo(HaveKey(BootstrapURLAnnotation), "removed on sight")
			if want == "" {
				Expect(host.Status.Bootstrap).To(BeNil(), "an annotation is never promoted into status")
				return
			}
			Expect(host.Status.Bootstrap).NotTo(BeNil())
			Expect(host.Status.Bootstrap.URL).To(Equal(want), "the URL stays the one the reconciler derives")
		},
		Entry("on a claimed host before its machine has minted anything", func(client.ObjectKey, *infrav1.Beskar7Machine) string {
			return ""
		}),
		Entry("on a host that already shows its URL", func(key client.ObjectKey, machine *infrav1.Beskar7Machine) string {
			mint(key, machine)
			Expect(settlePhysicalHost(hostR, key).Status.Bootstrap.URL).To(Equal(expectedURL(key)))
			return expectedURL(key)
		}),
	)

	It("removes a stale annotation an older manager left, even one carrying the right value, and derives the URL itself", func() {
		key, machine := claimedUnminted("url-stale-host")
		mint(key, machine)
		annotateHost(key, BootstrapURLAnnotation, expectedURL(key))

		host := settlePhysicalHost(hostR, key)

		Expect(host.Annotations).NotTo(HaveKey(BootstrapURLAnnotation))
		Expect(host.Status.Bootstrap.URL).To(Equal(expectedURL(key)))
	})
})

// validateAndDefault tests — pure unit, no envtest needed.
var _ = Describe("PhysicalHostReconciler.validateAndDefault", func() {
	It("should fail when BootstrapURLBase is empty, so a host is never published without a URL", func() {
		r := &PhysicalHostReconciler{RedfishClientFactory: reachableBMC()}
		Expect(r.validateAndDefault()).To(MatchError(ContainSubstring("BootstrapURLBase is empty")))
	})

	It("should succeed when BootstrapURLBase is set", func() {
		r := &PhysicalHostReconciler{RedfishClientFactory: reachableBMC(), BootstrapURLBase: "https://example.com:8082"}
		Expect(r.validateAndDefault()).To(Succeed())
	})
})

var _ = Describe("bootstrapURL", func() {
	host := &infrav1.PhysicalHost{ObjectMeta: metav1.ObjectMeta{Name: "host-01", Namespace: "my-ns"}}

	It("is the same with and without a trailing slash on the base", func() {
		withSlash := bootstrapURL("https://beskar7-controller-manager.capb7-system.svc:8082/", host)
		withoutSlash := bootstrapURL("https://beskar7-controller-manager.capb7-system.svc:8082", host)
		Expect(withSlash).To(Equal(withoutSlash))
		Expect(withSlash).To(Equal("https://beskar7-controller-manager.capb7-system.svc:8082/api/v1/bootstrap/my-ns/host-01"))
	})

	It("names the route the callback server serves bootstrap data on", func() {
		Expect(bootstrapURL("https://mgr:8082", host)).To(Equal("https://mgr:8082/api/v1/bootstrap/my-ns/host-01"))
	})
})
