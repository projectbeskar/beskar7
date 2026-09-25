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
	"slices"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/stmcginnis/gofish/schemas"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
	internalredfish "github.com/projectbeskar/beskar7/internal/redfish"
)

// SEC-16: anyone allowed to patch or create a PhysicalHost could point it at an
// endpoint they control, with insecureSkipVerify, and name any same-namespace
// Secret with username/password keys as its credentials; the controller read
// the Secret and gofish sent it as Basic auth on the first request. D-030 binds
// a credentials Secret to the addresses its annotations list, and both readers
// (the PhysicalHost controller, and the Beskar7Machine controller's Redfish
// calls) check it before any Redfish client exists. Every spec here records
// what the Redfish client factory was handed, because the factory is the last
// point before the credentials go on the wire.

// sec16AttackerBMC is in TEST-NET-3, which no fixture Secret allow-lists.
const sec16AttackerBMC = "https://203.0.113.9"

// bmcCall is one Redfish client the controllers asked the factory for.
type bmcCall struct {
	address  string
	username string
	password string
	insecure bool
}

// capturingBMC is a Redfish client factory that records every call and hands
// back a healthy mock BMC.
type capturingBMC struct {
	mu    sync.Mutex
	calls []bmcCall
}

func (c *capturingBMC) factory() internalredfish.RedfishClientFactory {
	return func(_ context.Context, address, username, password string, insecure bool, _ []byte) (internalredfish.Client, error) {
		c.mu.Lock()
		c.calls = append(c.calls, bmcCall{address: address, username: username, password: password, insecure: insecure})
		c.mu.Unlock()
		return internalredfish.NewMockClient(), nil
	}
}

func (c *capturingBMC) recorded() []bmcCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.calls)
}

// sentTo returns the calls made for address.
func (c *capturingBMC) sentTo(address string) []bmcCall {
	var to []bmcCall
	for _, call := range c.recorded() {
		if call.address == address {
			to = append(to, call)
		}
	}
	return to
}

// setSecretAnnotations replaces a Secret's annotations; nil removes them all.
func setSecretAnnotations(key client.ObjectKey, annotations map[string]string) {
	secret := &corev1.Secret{}
	Expect(k8sClient.Get(ctx, key, secret)).To(Succeed())
	edited := secret.DeepCopy()
	edited.Annotations = annotations
	Expect(k8sClient.Patch(ctx, edited, client.MergeFrom(secret))).To(Succeed())
}

// bindingHost creates an unclaimed host that already carries the finalizer, so
// its first reconcile goes straight to the BMC.
func bindingHost(namespace, name, address, credentials string, insecureSkipVerify bool) client.ObjectKey {
	host := &infrav1.PhysicalHost{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Finalizers: []string{PhysicalHostFinalizer}},
		Spec: infrav1.PhysicalHostSpec{RedfishConnection: infrav1.RedfishConnection{
			Address:              address,
			CredentialsSecretRef: credentials,
			InsecureSkipVerify:   ptr.To(insecureSkipVerify),
		}},
	}
	Expect(k8sClient.Create(ctx, host)).To(Succeed())
	return client.ObjectKeyFromObject(host)
}

// expectCredentialsNotAuthorized checks the host says why its BMC is not used,
// names what to add, and leaks neither half of the credential.
func expectCredentialsNotAuthorized(host *infrav1.PhysicalHost, mentions ...string) {
	GinkgoHelper()
	cond := conditions.Get(host, infrav1.RedfishConnectionReadyCondition)
	Expect(cond).NotTo(BeNil())
	Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	Expect(cond.Reason).To(Equal(infrav1.CredentialsNotAuthorizedReason))
	for _, m := range mentions {
		Expect(cond.Message).To(ContainSubstring(m), "the condition must say what to add")
	}
	for _, text := range []string{cond.Message, host.Status.ErrorMessage} {
		Expect(text).NotTo(ContainSubstring(fixtureBMCPassword))
		Expect(text).NotTo(ContainSubstring(fixtureBMCUsername))
	}
}

var _ = Describe("BMC credentials leave only for the addresses their Secret lists (SEC-16, D-030)", func() {
	var ns string

	BeforeEach(func() {
		n := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "bmc-binding-"}}
		Expect(k8sClient.Create(ctx, n)).To(Succeed())
		ns = n.Name
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
	})

	hostReconciler := func(bmc *capturingBMC) *PhysicalHostReconciler {
		return &PhysicalHostReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			Log:                  ctrl.Log.WithName("bmc-binding-host"),
			Recorder:             record.NewFakeRecorder(10),
			RedfishClientFactory: bmc.factory(),
		}
	}

	reconcileHost := func(r *PhysicalHostReconciler, key client.ObjectKey) ctrl.Result {
		GinkgoHelper()
		result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred(), "an unauthorised Secret waits for its annotations and is not retried as an error")
		return result
	}

	It("never hands an unannotated Secret's password to a host pointed at an endpoint an attacker controls", func() {
		Expect(k8sClient.Create(ctx, bmcCredentialsSecretWith(ns, "victim-bmc-credentials", nil))).To(Succeed())
		key := bindingHost(ns, "exfiltrating-host", sec16AttackerBMC, "victim-bmc-credentials", true)

		bmc := &capturingBMC{}
		result := reconcileHost(hostReconciler(bmc), key)

		Expect(bmc.recorded()).To(BeEmpty(), "the victim Secret's credentials were handed to the Redfish client for %s", sec16AttackerBMC)
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		host := getPhysicalHost(key)
		expectCredentialsNotAuthorized(host, BMCAddressesAnnotation, "203.0.113.9")
		Expect(host.Status.State).To(Equal(infrav1.StateError), "a host whose BMC cannot be used is never Available, so nothing claims it")
	})

	It("connects a host inside the Secret's CIDR, and refuses one re-pointed or created outside it", func() {
		Expect(k8sClient.Create(ctx, bmcCredentialsSecretWith(ns, "rack-credentials",
			map[string]string{BMCAddressesAnnotation: "10.0.0.0/24"}))).To(Succeed())
		bmc := &capturingBMC{}
		r := hostReconciler(bmc)

		By("connecting a host whose BMC address is inside the range")
		inside := bindingHost(ns, "rack-host", "https://10.0.0.5", "rack-credentials", false)
		reconcileHost(r, inside)
		Expect(bmc.recorded()).To(ConsistOf(bmcCall{
			address: "https://10.0.0.5", username: fixtureBMCUsername, password: fixtureBMCPassword,
		}))
		connected := getPhysicalHost(inside)
		Expect(conditions.IsTrue(connected, infrav1.RedfishConnectionReadyCondition)).To(BeTrue())
		Expect(connected.Status.State).To(Equal(infrav1.StateAvailable))

		By("re-pointing that host at an address outside the range")
		editRedfishConnection(inside, func(c *infrav1.RedfishConnection) { c.Address = sec16AttackerBMC })
		reconcileHost(r, inside)
		Expect(bmc.sentTo(sec16AttackerBMC)).To(BeEmpty(), "a patch to the address must not take the credentials with it")
		repointed := getPhysicalHost(inside)
		expectCredentialsNotAuthorized(repointed, BMCAddressesAnnotation, "203.0.113.9")
		Expect(repointed.Status.State).To(Equal(infrav1.StateError))

		By("creating a new host outside the range that names the same Secret")
		const strayAddress = "https://198.51.100.7"
		outside := bindingHost(ns, "stray-host", strayAddress, "rack-credentials", false)
		reconcileHost(r, outside)
		Expect(bmc.sentTo(strayAddress)).To(BeEmpty())
		expectCredentialsNotAuthorized(getPhysicalHost(outside), BMCAddressesAnnotation, "198.51.100.7")
	})

	DescribeTable("an allow-listed address over a transport that does not verify the BMC needs the Secret's opt-in",
		func(address string, insecureSkipVerify bool) {
			Expect(k8sClient.Create(ctx, bmcCredentialsSecretWith(ns, "rack-credentials",
				map[string]string{BMCAddressesAnnotation: "10.0.0.0/24"}))).To(Succeed())
			key := bindingHost(ns, "insecure-host", address, "rack-credentials", insecureSkipVerify)
			bmc := &capturingBMC{}
			r := hostReconciler(bmc)

			reconcileHost(r, key)
			Expect(bmc.recorded()).To(BeEmpty(), "the credentials must not travel unverified without the opt-in")
			expectCredentialsNotAuthorized(getPhysicalHost(key), BMCInsecureTransportAnnotation)

			By("opting the Secret in to insecure transport")
			setSecretAnnotations(client.ObjectKey{Namespace: ns, Name: "rack-credentials"}, map[string]string{
				BMCAddressesAnnotation:         "10.0.0.0/24",
				BMCInsecureTransportAnnotation: "true",
			})
			reconcileHost(r, key)
			Expect(bmc.recorded()).To(ConsistOf(bmcCall{
				address: address, username: fixtureBMCUsername, password: fixtureBMCPassword, insecure: insecureSkipVerify,
			}))
			Expect(conditions.IsTrue(getPhysicalHost(key), infrav1.RedfishConnectionReadyCondition)).To(BeTrue())
		},
		Entry("an http:// address", "http://10.0.0.7", false),
		Entry("an https:// address with insecureSkipVerify", "https://10.0.0.7", true),
	)

	It("leaves a Ready host Ready, and its machine Provisioned, while its Secret lacks the annotations (an upgrade from v0.8.0)", func() {
		Expect(k8sClient.Create(ctx, bmcCredentialsSecretWith(ns, "bmc-credentials", nil))).To(Succeed())
		key := provisioningHost(ns, "upgraded-host", "upgraded-machine", infrav1.StateReady, nil)

		bmc := &capturingBMC{}
		reconcileHost(hostReconciler(bmc), key)
		host := getPhysicalHost(key)
		Expect(bmc.recorded()).To(BeEmpty())
		Expect(host.Status.State).To(Equal(infrav1.StateReady), "nothing is reprovisioned for a missing annotation")
		Expect(host.Status.Ready).To(BeTrue())
		Expect(host.Status.ErrorMessage).To(BeEmpty())
		expectCredentialsNotAuthorized(host, BMCAddressesAnnotation, "mock-redfish.example.invalid")

		machine := &infrav1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "upgraded-machine", Namespace: ns},
			Spec:       infrav1.Beskar7MachineSpec{ProviderID: providerID(ns, host.Name)},
			Status: infrav1.Beskar7MachineStatus{
				Ready:          true,
				Phase:          ptr.To("Provisioned"),
				Initialization: infrav1.Beskar7MachineInitializationStatus{Provisioned: ptr.To(true)},
			},
		}
		setTrue(machine, infrav1.InfrastructureReadyCondition, infrav1.ProvisionedReason)
		machineR := &Beskar7MachineReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			Log:                  ctrl.Log.WithName("bmc-binding-upgraded-machine"),
			RedfishClientFactory: bmc.factory(),
		}
		_, err := machineR.handlePhysicalHostState(ctx, machineR.Log, machine, host)
		Expect(err).NotTo(HaveOccurred())
		Expect(isTerminallyFailed(machine)).To(BeFalse())
		Expect(machine.Status.Ready).To(BeTrue())
		Expect(ptr.Deref(machine.Status.Phase, "")).To(Equal("Provisioned"))
		Expect(bmc.recorded()).To(BeEmpty())
	})

	// The Beskar7Machine controller builds a Redfish client in four places, all
	// from the host's spec as it reads it. Each spec re-points a claimed host
	// the way a patch by an attacker would, before the host's own reconcile has
	// seen the edit, and drives one of them.
	Describe("the Beskar7Machine's Redfish calls on a claimed host re-pointed outside the list", func() {
		const machineName = "repointed-machine"

		var (
			bmc      *capturingBMC
			machineR *Beskar7MachineReconciler
			machine  *infrav1.Beskar7Machine
		)

		BeforeEach(func() {
			Expect(k8sClient.Create(ctx, bmcCredentialsSecret(ns))).To(Succeed())
			bmc = &capturingBMC{}
			machineR = &Beskar7MachineReconciler{
				Client: k8sClient, Scheme: k8sClient.Scheme(),
				Log:                  ctrl.Log.WithName("bmc-binding-machine"),
				RedfishClientFactory: bmc.factory(),
				BootstrapURLBase:     "https://example.com:8082",
			}
			machine = &infrav1.Beskar7Machine{ObjectMeta: metav1.ObjectMeta{Name: machineName, Namespace: ns}}
		})

		// repoint moves the host's address to sec16AttackerBMC and returns the
		// host as the machine then reads it.
		repoint := func(key client.ObjectKey) *infrav1.PhysicalHost {
			editRedfishConnection(key, func(c *infrav1.RedfishConnection) { c.Address = sec16AttackerBMC })
			return getPhysicalHost(key)
		}

		expectNothingSent := func() {
			GinkgoHelper()
			Expect(bmc.recorded()).To(BeEmpty(), "the Beskar7Machine handed the host's BMC credentials to the Redfish client for %s", sec16AttackerBMC)
		}

		It("does not boot the inspector through it", func() {
			key := inUseHost(ns, "claimed-host", machineName, nil)
			host := repoint(key)

			_, err := machineR.handlePhysicalHostState(ctx, machineR.Log, machine, host)
			expectNothingSent()
			Expect(err).To(HaveOccurred(), "the inspection waits for the host")
			Expect(isTerminallyFailed(machine)).To(BeFalse())
			Expect(getPhysicalHost(key).Annotations).NotTo(HaveKey(InspectionRequestAnnotation))
		})

		It("does not power it back on while it inspects", func() {
			key := provisioningHost(ns, "inspecting-host", machineName, infrav1.StateInspecting, nil)
			host := getPhysicalHost(key)
			started := metav1.NewTime(time.Now().Add(-(InspectionPowerRecheckDelay + time.Minute)))
			host.Status.InspectionTimestamp = &started
			host.Status.ObservedPowerState = string(schemas.OffPowerState)
			Expect(k8sClient.Status().Update(ctx, host)).To(Succeed())
			host = repoint(key)

			_, err := machineR.handlePhysicalHostState(ctx, machineR.Log, machine, host)
			Expect(err).NotTo(HaveOccurred())
			expectNothingSent()
			Expect(isTerminallyFailed(machine)).To(BeFalse())
		})

		It("does not clear its boot override when it reaches Ready", func() {
			key := provisioningHost(ns, "ready-host", machineName, infrav1.StateReady, nil)
			host := repoint(key)

			_, err := machineR.handlePhysicalHostState(ctx, machineR.Log, machine, host)
			Expect(err).NotTo(HaveOccurred())
			expectNothingSent()
			Expect(machine.Status.Ready).To(BeTrue(), "the boot-override clear is best effort; the machine is provisioned without it")
		})

		It("releases it without a Redfish call when the machine is deleted", func() {
			key := inUseHost(ns, "released-host", machineName, nil)
			repoint(key)

			_, err := machineR.reconcileDelete(ctx, machineR.Log, machine)
			Expect(err).NotTo(HaveOccurred())
			expectNothingSent()
			Expect(getPhysicalHost(key).Spec.ConsumerRef).To(BeNil(), "the release itself does not need the BMC")
		})
	})
})

// An upgrade from v0.8.0 finds every credentials Secret without the D-030
// annotations. A claim that has not started inspecting yet must wait rather
// than fail its machine, and the operator's annotation alone must be enough to
// resume it: the host is woken through its Secret watch (SecretToPhysicalHosts),
// not by a timer, and nothing else touches the machine.
var _ = Describe("Upgrading with an unannotated BMC credentials Secret, both controllers under a running manager", func() {
	var (
		testNs    *corev1.Namespace
		bmc       *capturingBMC
		mgrCancel context.CancelFunc
	)

	BeforeEach(func() {
		testNs = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "bmc-binding-mgr-"}}
		Expect(k8sClient.Create(ctx, testNs)).To(Succeed())
		bmc = &capturingBMC{}
	})

	AfterEach(func() {
		if mgrCancel != nil {
			mgrCancel()
		}
		Expect(k8sClient.Delete(ctx, testNs)).To(Succeed())
	})

	startManager := func() {
		skipNameValidation := true
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:                 k8sClient.Scheme(),
			Metrics:                metricsserver.Options{BindAddress: "0"},
			HealthProbeBindAddress: "0",
			// envtest never finishes deleting namespaces, so a cluster-wide cache
			// would hand these controllers every other spec's leftovers.
			Cache:      cache.Options{DefaultNamespaces: map[string]cache.Config{testNs.Name: {}}},
			Controller: config.Controller{SkipNameValidation: &skipNameValidation},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect((&PhysicalHostReconciler{
			Client:               mgr.GetClient(),
			Scheme:               mgr.GetScheme(),
			Log:                  ctrl.Log.WithName("bmc-binding-mgr-host"),
			Recorder:             record.NewFakeRecorder(100),
			RedfishClientFactory: bmc.factory(),
		}).SetupWithManager(mgr)).To(Succeed())
		Expect((&Beskar7MachineReconciler{
			Client:               mgr.GetClient(),
			Scheme:               mgr.GetScheme(),
			Log:                  ctrl.Log.WithName("bmc-binding-mgr-machine"),
			RedfishClientFactory: bmc.factory(),
			BootstrapURLBase:     "https://example.com:8082",
		}).SetupWithManager(mgr)).To(Succeed())

		var mgrCtx context.Context
		mgrCtx, mgrCancel = context.WithCancel(ctx)
		go func() {
			defer GinkgoRecover()
			Expect(mgr.Start(mgrCtx)).To(Succeed())
		}()
		Expect(mgr.GetCache().WaitForCacheSync(mgrCtx)).To(BeTrue())
	}

	It("parks a claimed InUse host's machine until the Secret is annotated, then carries on", func() {
		ns := testNs.Name
		const clusterName = "upgrade-cluster"

		By("seeding what v0.8.0 left behind: a claim whose host is InUse and whose Secret has no annotations")
		Expect(k8sClient.Create(ctx, &clusterv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns},
			Spec:       clusterv1.ClusterSpec{Paused: ptr.To(false)},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "upgrade-bootstrap", Namespace: ns},
			Data:       map[string][]byte{"value": []byte("#cloud-config\n")},
		})).To(Succeed())
		machine := &clusterv1.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name: "upgrade-machine", Namespace: ns,
				Labels: map[string]string{clusterv1.ClusterNameLabel: clusterName},
			},
			Spec: clusterv1.MachineSpec{
				ClusterName: clusterName,
				InfrastructureRef: clusterv1.ContractVersionedObjectReference{
					APIGroup: infrav1.GroupVersion.Group, Kind: "Beskar7Machine", Name: "upgrade-machine",
				},
				Bootstrap: clusterv1.Bootstrap{DataSecretName: ptr.To("upgrade-bootstrap")},
			},
		}
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())
		credentials := bmcCredentialsSecretWith(ns, "bmc-credentials", nil)
		Expect(k8sClient.Create(ctx, credentials)).To(Succeed())

		b7m := &infrav1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name: "upgrade-machine", Namespace: ns,
				Labels: map[string]string{clusterv1.ClusterNameLabel: clusterName},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: clusterv1.GroupVersion.String(), Kind: "Machine",
					Name: machine.Name, UID: machine.UID,
				}},
			},
			Spec: infrav1.Beskar7MachineSpec{
				InspectionImageURL: "http://boot/inspect.ipxe",
				TargetImageURL:     "http://boot/kairos.raw",
				TargetImageDigest:  bootTestDigest,
			},
		}
		Expect(k8sClient.Create(ctx, b7m)).To(Succeed())
		machineKey := client.ObjectKeyFromObject(b7m)
		hostKey := inUseHost(ns, "upgrade-host", b7m.Name, nil)

		By("starting the upgraded controllers")
		startManager()

		By("waiting for the machine to report that it waits for its host")
		Eventually(func(g Gomega) {
			got := &infrav1.Beskar7Machine{}
			g.Expect(k8sClient.Get(ctx, machineKey, got)).To(Succeed())
			g.Expect(conditions.GetReason(got, infrav1.InfrastructureReadyCondition)).To(Equal(infrav1.WaitingForBMCReason))
			g.Expect(conditions.GetMessage(got, infrav1.InfrastructureReadyCondition)).To(ContainSubstring(BMCAddressesAnnotation),
				"the machine quotes the host, so it says what the operator has to add")
		}, 30*time.Second, 200*time.Millisecond).Should(Succeed())
		host := getPhysicalHost(hostKey)
		Expect(host.Status.State).To(Equal(infrav1.StateError))
		expectCredentialsNotAuthorized(host, BMCAddressesAnnotation)

		By("checking the machine keeps waiting, not Failed, and nothing reaches the BMC")
		Consistently(func(g Gomega) {
			got := &infrav1.Beskar7Machine{}
			g.Expect(k8sClient.Get(ctx, machineKey, got)).To(Succeed())
			g.Expect(isTerminallyFailed(got)).To(BeFalse())
			g.Expect(bmc.recorded()).To(BeEmpty())
		}, 3*time.Second, 200*time.Millisecond).Should(Succeed())

		By("annotating the Secret, and waiting for the machine to boot the inspector by itself")
		fixture := bmcCredentialsSecret(ns)
		setSecretAnnotations(client.ObjectKeyFromObject(credentials), fixture.Annotations)
		// Well inside the host's own requeue for this condition, and the
		// machine's: only the Secret watch can wake the host this soon.
		Eventually(func(g Gomega) {
			got := &infrav1.Beskar7Machine{}
			g.Expect(k8sClient.Get(ctx, machineKey, got)).To(Succeed())
			g.Expect(isTerminallyFailed(got)).To(BeFalse())
			g.Expect(ptr.Deref(got.Status.Phase, "")).To(Equal("Inspecting"))

			h := &infrav1.PhysicalHost{}
			g.Expect(k8sClient.Get(ctx, hostKey, h)).To(Succeed())
			g.Expect(h.Status.State).To(Equal(infrav1.StateInspecting))
		}, 20*time.Second, 200*time.Millisecond).Should(Succeed())
		Expect(bmc.sentTo(host.Spec.RedfishConnection.Address)).NotTo(BeEmpty())
	})
})
