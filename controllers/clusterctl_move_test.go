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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
	internalredfish "github.com/projectbeskar/beskar7/internal/redfish"
)

// clusterctl move only ever Creates objects on the target
// (cmd/clusterctl/client/cluster/mover.go createTargetObject), and Create
// drops .status for any resource with a status subresource — the API server
// ignores whatever the request body carries there. A moved PhysicalHost
// therefore always lands at State "" even when it was Ready, and its
// Beskar7Machine's Spec.ProviderID — Spec is never dropped — still names it.
// Left alone, the claimed-host branch of PhysicalHostReconciler.reconcileNormal
// reads State "" as a fresh claim and moves the host to InUse, and
// Beskar7Machine's InUse case boots the inspector again on hardware that is
// already serving (MOVE-1). These specs replay the mover's own object graph
// and ordering — Cluster paused first, then the PhysicalHost and the secrets
// it and its Machine need, then the owner Machine, then the Beskar7Machine
// last, exactly the order clusterctl's owner-chain sequencing produces — and
// pin that the pair comes back exactly as it was, D-028's fix.

// minimalImageSpec fills the three fields the Beskar7Machine CRD requires
// (inspectionImageURL, targetImageURL, targetImageDigest) with valid-shaped
// placeholders. None of these specs ever reaches triggerInspection in this
// file — either adoption short-circuits it or the test asserts it is never
// called — so the values themselves are never read; only their shape must
// satisfy the CRD's validation patterns.
func minimalImageSpec() infrav1.Beskar7MachineSpec {
	return infrav1.Beskar7MachineSpec{
		InspectionImageURL: "http://boot/inspect.ipxe",
		TargetImageURL:     "http://boot/kairos.raw",
		TargetImageDigest:  bootTestDigest,
	}
}

// deleteForMove strips obj's finalizers (if any) and deletes it, standing in
// for the mover's own deleteSourceObject: no controller is watching these
// objects in this test to react to a normal graceful delete, so a finalizer
// would otherwise hang it forever.
func deleteForMove(obj client.Object) {
	if len(obj.GetFinalizers()) > 0 {
		base, ok := obj.DeepCopyObject().(client.Object)
		Expect(ok).To(BeTrue())
		obj.SetFinalizers(nil)
		Expect(k8sClient.Patch(ctx, obj, client.MergeFrom(base))).To(Succeed())
	}
	Expect(k8sClient.Delete(ctx, obj)).To(Succeed())
}

var _ = Describe("clusterctl move: a Ready PhysicalHost / Provisioned Beskar7Machine pair", func() {
	var ns string

	BeforeEach(func() {
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "clusterctl-move-"}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		ns = nsObj.Name
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
	})

	It("adopts Ready without re-inspecting, and the machine never touches Redfish boot or mints new credentials", func() {
		const (
			clusterName = "move-cluster"
			hostName    = "move-host"
			machineName = "move-machine"
		)

		By("seeding a fully provisioned pair, exactly what clusterctl move requires to start (mover.go's nodeRef precondition)")
		cluster := &clusterv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns},
			Spec:       clusterv1.ClusterSpec{Paused: ptr.To(false)},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

		bootstrapDataSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: machineName + "-bootstrap", Namespace: ns},
			Data:       map[string][]byte{"value": []byte("#cloud-config\n")},
		}
		Expect(k8sClient.Create(ctx, bootstrapDataSecret)).To(Succeed())

		machine := &clusterv1.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name: machineName, Namespace: ns,
				Labels: map[string]string{clusterv1.ClusterNameLabel: clusterName},
			},
			Spec: clusterv1.MachineSpec{
				ClusterName: clusterName,
				InfrastructureRef: clusterv1.ContractVersionedObjectReference{
					APIGroup: infrav1.GroupVersion.Group, Kind: "Beskar7Machine", Name: machineName,
				},
				Bootstrap: clusterv1.Bootstrap{DataSecretName: ptr.To(bootstrapDataSecret.Name)},
			},
		}
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())

		credsSecret := bmcCredentialsSecret(ns)
		Expect(k8sClient.Create(ctx, credsSecret)).To(Succeed())

		host := claimedPhysicalHost(ns, hostName, machineName)
		host.Finalizers = []string{PhysicalHostFinalizer}
		Expect(k8sClient.Create(ctx, host)).To(Succeed())
		host.Status.State = infrav1.StateReady
		host.Status.Ready = true
		setTrue(host, infrav1.RedfishConnectionReadyCondition, infrav1.RedfishConnectedReason)
		setTrue(host, infrav1.HostInspectedCondition, infrav1.HostInspectedReason)
		Expect(k8sClient.Status().Update(ctx, host)).To(Succeed())

		tokenSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: bootstrapTokenSecretName(hostName), Namespace: ns},
			Type:       corev1.SecretTypeOpaque,
			Data: map[string][]byte{
				bootstrapTokenSecretKey: []byte("s3cr3t-token"),
				bootNonceSecretKey:      []byte("s3cr3t-nonce"),
			},
		}
		Expect(controllerutil.SetControllerReference(host, tokenSecret, k8sClient.Scheme())).To(Succeed())
		Expect(k8sClient.Create(ctx, tokenSecret)).To(Succeed())

		b7m := &infrav1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name: machineName, Namespace: ns,
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
				ProviderID:         providerID(ns, hostName),
			},
		}
		b7m.Finalizers = []string{Beskar7MachineFinalizer}
		Expect(k8sClient.Create(ctx, b7m)).To(Succeed())
		b7m.Status.Ready = true
		b7m.Status.Phase = ptr.To("Provisioned")
		b7m.Status.Initialization.Provisioned = ptr.To(true)
		setTrue(b7m, infrav1.InfrastructureReadyCondition, infrav1.ProvisionedReason)
		setTrue(b7m, infrav1.PhysicalHostAssociatedCondition, infrav1.PhysicalHostAssociatedReason)
		setTrue(b7m, infrav1.BootstrapDataReadyCondition, infrav1.BootstrapDataReadyReason)
		Expect(k8sClient.Status().Update(ctx, b7m)).To(Succeed())

		By("snapshotting the source objects, then deleting them the way the mover clears the source namespace")
		hostSnapshot := host.DeepCopy()
		b7mSnapshot := b7m.DeepCopy()
		machineSnapshot := machine.DeepCopy()
		clusterSnapshot := cluster.DeepCopy()
		tokenSecretSnapshot := tokenSecret.DeepCopy()
		bootstrapDataSecretSnapshot := bootstrapDataSecret.DeepCopy()

		deleteForMove(cluster)
		deleteForMove(host)
		deleteForMove(b7m)
		deleteForMove(machine)
		Expect(k8sClient.Delete(ctx, tokenSecret)).To(Succeed())
		Expect(k8sClient.Delete(ctx, credsSecret)).To(Succeed())
		Expect(k8sClient.Delete(ctx, bootstrapDataSecret)).To(Succeed())

		By("recreating the Cluster, paused, first — the mover's own first step")
		newCluster := clusterSnapshot.DeepCopy()
		newCluster.ResourceVersion = ""
		newCluster.UID = ""
		newCluster.Spec.Paused = ptr.To(true)
		Expect(k8sClient.Create(ctx, newCluster)).To(Succeed())
		Expect(newCluster.Status.Conditions).To(BeEmpty(), "Create drops .status")

		By("recreating the PhysicalHost and the secrets it and its Machine need")
		newHost := hostSnapshot.DeepCopy()
		newHost.ResourceVersion = ""
		newHost.UID = ""
		newHost.Finalizers = []string{PhysicalHostFinalizer}
		Expect(k8sClient.Create(ctx, newHost)).To(Succeed())
		Expect(newHost.Status.State).To(BeEmpty(), "the mover only Creates; status is dropped even though the source was Ready")
		Expect(newHost.Status.Ready).To(BeFalse())
		Expect(newHost.Spec.ConsumerRef).NotTo(BeNil(), "Spec, unlike Status, is never dropped by a move")

		newCreds := bmcCredentialsSecret(ns)
		Expect(k8sClient.Create(ctx, newCreds)).To(Succeed())

		newTokenSecret := tokenSecretSnapshot.DeepCopy()
		newTokenSecret.ResourceVersion = ""
		newTokenSecret.UID = ""
		newTokenSecret.OwnerReferences = nil
		Expect(controllerutil.SetControllerReference(newHost, newTokenSecret, k8sClient.Scheme())).To(Succeed())
		Expect(k8sClient.Create(ctx, newTokenSecret)).To(Succeed())

		newBootstrapDataSecret := bootstrapDataSecretSnapshot.DeepCopy()
		newBootstrapDataSecret.ResourceVersion = ""
		newBootstrapDataSecret.UID = ""
		Expect(k8sClient.Create(ctx, newBootstrapDataSecret)).To(Succeed())

		mockRf := internalredfish.NewMockClient()
		hostReconciler := &PhysicalHostReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			Log:      ctrl.Log.WithName("move-host"),
			Recorder: record.NewFakeRecorder(10),
			RedfishClientFactory: func(context.Context, string, string, string, bool, []byte) (internalredfish.Client, error) {
				return mockRf, nil
			},
		}

		By("reconciling the host before its Beskar7Machine has landed — the mover creates it several groups later")
		_, err := hostReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(newHost)})
		Expect(err).NotTo(HaveOccurred())
		notYetAdopted := getPhysicalHost(client.ObjectKeyFromObject(newHost))
		Expect(notYetAdopted.Status.State).To(Equal(infrav1.StateInUse),
			"claimed, but its consumer does not exist yet, so this cannot be recognised as an adoption")

		By("recreating the owner Machine, then the Beskar7Machine, in that order")
		newMachine := machineSnapshot.DeepCopy()
		newMachine.ResourceVersion = ""
		newMachine.UID = ""
		Expect(k8sClient.Create(ctx, newMachine)).To(Succeed())

		newB7M := b7mSnapshot.DeepCopy()
		newB7M.ResourceVersion = ""
		newB7M.UID = ""
		newB7M.Finalizers = []string{Beskar7MachineFinalizer}
		newB7M.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: clusterv1.GroupVersion.String(), Kind: "Machine",
			Name: newMachine.Name, UID: newMachine.UID,
		}}
		Expect(k8sClient.Create(ctx, newB7M)).To(Succeed())
		Expect(newB7M.Status.Ready).To(BeFalse(), "Create drops .status")
		Expect(ptr.Deref(newB7M.Status.Initialization.Provisioned, false)).To(BeFalse())
		Expect(newB7M.Spec.ProviderID).To(Equal(providerID(ns, hostName)), "Spec.ProviderID is never dropped by a move")

		By("reconciling the host again now that its consumer has landed")
		_, err = hostReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(newHost)})
		Expect(err).NotTo(HaveOccurred())
		adopted := getPhysicalHost(client.ObjectKeyFromObject(newHost))
		Expect(adopted.Status.State).To(Equal(infrav1.StateReady))
		Expect(adopted.Status.Ready).To(BeTrue())
		Expect(adopted.Status.ErrorMessage).To(BeEmpty())
		Expect(mockRf.SetPowerStateCalled).To(BeFalse(), "adoption never touches the BMC")
		Expect(mockRf.SetBootSourcePXECalled).To(BeFalse())

		By("unpausing the target Cluster and reconciling the Beskar7Machine")
		base := newCluster.DeepCopy()
		newCluster.Spec.Paused = ptr.To(false)
		Expect(k8sClient.Patch(ctx, newCluster, client.MergeFrom(base))).To(Succeed())

		machineReconciler := &Beskar7MachineReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			Log: ctrl.Log.WithName("move-machine"),
			RedfishClientFactory: func(context.Context, string, string, string, bool, []byte) (internalredfish.Client, error) {
				return mockRf, nil
			},
			BootstrapURLBase: "https://example.com:8082",
		}
		// The first pass only publishes the Paused condition:
		// paused.EnsurePausedCondition requeues once, on any object that does
		// not carry the condition yet, before it ever evaluates isPaused — the
		// same "first pass only adds the finalizer" shape the rest of this
		// suite works around. The second pass does the real work.
		_, err = machineReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(newB7M)})
		Expect(err).NotTo(HaveOccurred())
		_, err = machineReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(newB7M)})
		Expect(err).NotTo(HaveOccurred())

		finalMachine := &infrav1.Beskar7Machine{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(newB7M), finalMachine)).To(Succeed())
		Expect(finalMachine.Status.Ready).To(BeTrue())
		Expect(ptr.Deref(finalMachine.Status.Phase, "")).To(Equal("Provisioned"))
		Expect(ptr.Deref(finalMachine.Status.Initialization.Provisioned, false)).To(BeTrue())
		Expect(conditions.GetReason(finalMachine, infrav1.InfrastructureReadyCondition)).To(Equal(infrav1.ProvisionedReason))
		Expect(finalMachine.Spec.ProviderID).To(Equal(providerID(ns, hostName)))

		Expect(mockRf.SetBootSourcePXECalled).To(BeFalse(),
			"the machine must never boot the inspector again on a host it already holds by ProviderID")
		Expect(mockRf.SetPowerStateCalled).To(BeFalse(), "no power operation is needed to reassert Ready")

		finalHost := getPhysicalHost(client.ObjectKeyFromObject(newHost))
		Expect(finalHost.Annotations).NotTo(HaveKey(InspectionRequestAnnotation), "no inspection was ever requested")
		Expect(finalHost.Annotations).NotTo(HaveKey(BootstrapTokenAnnotation), "no fresh bearer token was minted")
		Expect(finalHost.Annotations).NotTo(HaveKey(BootNonceAnnotation), "no fresh boot nonce was minted")
	})

	It("adopts Ready even when the credentials Secret has not been created yet", func() {
		host := claimedPhysicalHost(ns, "late-creds-host", "late-creds-machine")
		host.Finalizers = []string{PhysicalHostFinalizer}
		Expect(k8sClient.Create(ctx, host)).To(Succeed())
		// No Status().Update: this host is exactly as a move leaves it — claimed, State "".

		b7m := &infrav1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "late-creds-machine", Namespace: ns},
			Spec: func() infrav1.Beskar7MachineSpec {
				s := minimalImageSpec()
				s.ProviderID = providerID(ns, host.Name)
				return s
			}(),
		}
		Expect(k8sClient.Create(ctx, b7m)).To(Succeed())

		hostReconciler := &PhysicalHostReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			Log:                  ctrl.Log.WithName("move-late-creds"),
			Recorder:             record.NewFakeRecorder(10),
			RedfishClientFactory: failingBMC(errors.New("must not be reached: adoption needs no BMC")),
		}
		_, err := hostReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(host)})
		Expect(err).To(HaveOccurred(), "the missing credentials Secret still errors, for the workqueue's backoff")

		adopted := getPhysicalHost(client.ObjectKeyFromObject(host))
		Expect(adopted.Status.State).To(Equal(infrav1.StateReady), "adoption must not wait on credentials that have not arrived yet")
		Expect(adopted.Status.Ready).To(BeTrue())
		Expect(conditions.GetReason(adopted, infrav1.RedfishConnectionReadyCondition)).To(Equal(infrav1.MissingCredentialsReason))
	})

	It("never adopts a ProviderID that names a different host", func() {
		host := claimedPhysicalHost(ns, "actual-host", "elsewhere-machine")
		host.Finalizers = []string{PhysicalHostFinalizer}
		Expect(k8sClient.Create(ctx, host)).To(Succeed())

		b7m := &infrav1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "elsewhere-machine", Namespace: ns},
			Spec: func() infrav1.Beskar7MachineSpec {
				s := minimalImageSpec()
				s.ProviderID = providerID(ns, "some-other-host")
				return s
			}(),
		}
		Expect(k8sClient.Create(ctx, b7m)).To(Succeed())

		hostReconciler := &PhysicalHostReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			Log:      ctrl.Log.WithName("move-wrong-host"),
			Recorder: record.NewFakeRecorder(10),
			RedfishClientFactory: func(context.Context, string, string, string, bool, []byte) (internalredfish.Client, error) {
				return internalredfish.NewMockClient(), nil
			},
		}
		Expect(k8sClient.Create(ctx, bmcCredentialsSecret(ns))).To(Succeed())
		_, err := hostReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(host)})
		Expect(err).NotTo(HaveOccurred())

		notAdopted := getPhysicalHost(client.ObjectKeyFromObject(host))
		Expect(notAdopted.Status.State).To(Equal(infrav1.StateInUse), "the consumer's ProviderID names a different host entirely")
	})

	It("never adopts a cross-namespace consumerRef", func() {
		otherNsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "clusterctl-move-other-"}}
		Expect(k8sClient.Create(ctx, otherNsObj)).To(Succeed())
		defer func() { Expect(k8sClient.Delete(ctx, otherNsObj)).To(Succeed()) }()

		host := &infrav1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{Name: "xns-host", Namespace: ns, Finalizers: []string{PhysicalHostFinalizer}},
			Spec: infrav1.PhysicalHostSpec{
				RedfishConnection: infrav1.RedfishConnection{
					Address: "https://mock-redfish.example.invalid:8443", CredentialsSecretRef: "bmc-credentials",
				},
				ConsumerRef: &corev1.ObjectReference{
					Kind: "Beskar7Machine", Name: "xns-machine", Namespace: otherNsObj.Name,
					APIVersion: infrav1.GroupVersion.String(),
				},
			},
		}
		Expect(k8sClient.Create(ctx, host)).To(Succeed())
		Expect(k8sClient.Create(ctx, bmcCredentialsSecret(ns))).To(Succeed())

		// A Beskar7Machine of the same name in the OTHER namespace, with a
		// ProviderID that (deliberately, for this test) names the host in ns —
		// it must never be consulted, because the consumerRef itself is rejected
		// before the lookup.
		xnsMachine := &infrav1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "xns-machine", Namespace: otherNsObj.Name},
			Spec: func() infrav1.Beskar7MachineSpec {
				s := minimalImageSpec()
				s.ProviderID = providerID(ns, "xns-host")
				return s
			}(),
		}
		Expect(k8sClient.Create(ctx, xnsMachine)).To(Succeed())

		hostReconciler := &PhysicalHostReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			Log:      ctrl.Log.WithName("move-xns"),
			Recorder: record.NewFakeRecorder(10),
			RedfishClientFactory: func(context.Context, string, string, string, bool, []byte) (internalredfish.Client, error) {
				return internalredfish.NewMockClient(), nil
			},
		}
		_, err := hostReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(host)})
		Expect(err).NotTo(HaveOccurred())

		notAdopted := getPhysicalHost(client.ObjectKeyFromObject(host))
		Expect(notAdopted.Status.State).To(Equal(infrav1.StateInUse), "a cross-namespace consumerRef is never legitimate (SEC-12)")
	})

	It("leaves an already-Deploying host untouched even though its consumer's ProviderID matches", func() {
		// Defends the state guard in adoptProvisionedClaim: Inspecting,
		// Deploying, Ready and the run's own Error are driven exclusively by the
		// annotation handlers and must never be second-guessed here, even when
		// the ProviderID would otherwise line up (a coincidence that cannot
		// happen for real — ProviderID is set only once a host reaches Ready —
		// but the guard must hold regardless of why the coincidence occurred).
		Expect(k8sClient.Create(ctx, bmcCredentialsSecret(ns))).To(Succeed())
		key := provisioningHost(ns, "deploying-host", "deploying-machine", infrav1.StateDeploying, nil)
		before := getPhysicalHost(key)

		b7m := &infrav1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "deploying-machine", Namespace: ns},
			Spec: func() infrav1.Beskar7MachineSpec {
				s := minimalImageSpec()
				s.ProviderID = providerID(ns, "deploying-host")
				return s
			}(),
		}
		Expect(k8sClient.Create(ctx, b7m)).To(Succeed())

		hostReconciler := &PhysicalHostReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			Log:      ctrl.Log.WithName("move-deploying-guard"),
			Recorder: record.NewFakeRecorder(10),
			RedfishClientFactory: func(context.Context, string, string, string, bool, []byte) (internalredfish.Client, error) {
				return internalredfish.NewMockClient(), nil
			},
		}
		_, err := hostReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		after := getPhysicalHost(key)
		Expect(after.Status.State).To(Equal(infrav1.StateDeploying), "Deploying is driven by the annotation handlers, not adoption")
		Expect(after.Status.DeployingTimestamp).To(Equal(before.Status.DeployingTimestamp))
	})
})

var _ = Describe("Beskar7Machine InUse case: a machine that already holds its host by ProviderID", func() {
	DescribeTable("whether it re-inspects",
		func(withProviderID bool) {
			nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "move-inuse-"}}
			Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
			defer func() { Expect(k8sClient.Delete(ctx, nsObj)).To(Succeed()) }()
			Expect(k8sClient.Create(ctx, bmcCredentialsSecret(nsObj.Name))).To(Succeed())

			hostKey := inUseHost(nsObj.Name, "inuse-host", "held-machine", nil)
			host := getPhysicalHost(hostKey)

			machine := &infrav1.Beskar7Machine{ObjectMeta: metav1.ObjectMeta{Name: "held-machine", Namespace: nsObj.Name}}
			if withProviderID {
				machine.Spec.ProviderID = providerID(nsObj.Name, host.Name)
			}

			mockRf := internalredfish.NewMockClient()
			r := &Beskar7MachineReconciler{
				Client: k8sClient, Scheme: k8sClient.Scheme(),
				Log: ctrl.Log.WithName("move-inuse"),
				RedfishClientFactory: func(context.Context, string, string, string, bool, []byte) (internalredfish.Client, error) {
					return mockRf, nil
				},
			}
			result, err := r.handlePhysicalHostState(ctx, r.Log, machine, host)
			Expect(err).NotTo(HaveOccurred())

			if withProviderID {
				Expect(mockRf.SetBootSourcePXECalled).To(BeFalse(), "already held by ProviderID: never re-inspect")
				Expect(conditions.GetReason(machine, infrav1.InfrastructureReadyCondition)).To(Equal(infrav1.WaitingForHostAdoptionReason))
				Expect(result.RequeueAfter).To(Equal(30 * time.Second))
				Expect(ptr.Deref(machine.Status.Phase, "")).NotTo(Equal("Inspecting"))
			} else {
				Expect(mockRf.SetBootSourcePXECalled).To(BeTrue(), "no ProviderID yet: a fresh claim, inspection proceeds as always")
				Expect(conditions.GetReason(machine, infrav1.InfrastructureReadyCondition)).To(Equal(infrav1.PhysicalHostNotReadyReason))
				Expect(ptr.Deref(machine.Status.Phase, "")).To(Equal("Inspecting"))
			}
		},
		Entry("with a ProviderID already set: waits for the host to adopt", true),
		Entry("without a ProviderID: unchanged behaviour, triggers inspection", false),
	)
})

var _ = Describe("Beskar7Machine reconcileDelete: the clusterctl delete-for-move annotation", func() {
	DescribeTable("whether the source side touches the hardware",
		func(movingAway bool) {
			nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "move-delete-"}}
			Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
			defer func() { Expect(k8sClient.Delete(ctx, nsObj)).To(Succeed()) }()
			Expect(k8sClient.Create(ctx, bmcCredentialsSecret(nsObj.Name))).To(Succeed())

			host := claimedPhysicalHost(nsObj.Name, "move-delete-host", "move-delete-machine")
			host.Finalizers = []string{PhysicalHostFinalizer}
			Expect(k8sClient.Create(ctx, host)).To(Succeed())
			host.Status.State = infrav1.StateReady
			host.Status.Ready = true
			Expect(k8sClient.Status().Update(ctx, host)).To(Succeed())

			b7m := &infrav1.Beskar7Machine{
				ObjectMeta: metav1.ObjectMeta{Name: "move-delete-machine", Namespace: nsObj.Name},
				Spec: func() infrav1.Beskar7MachineSpec {
					s := minimalImageSpec()
					s.ProviderID = providerID(nsObj.Name, host.Name)
					return s
				}(),
			}
			if movingAway {
				b7m.Annotations = map[string]string{clusterctlDeleteForMoveAnnotation: ""}
			}

			mockRf := internalredfish.NewMockClient()
			r := &Beskar7MachineReconciler{
				Client: k8sClient, Scheme: k8sClient.Scheme(),
				Log: ctrl.Log.WithName("move-delete"),
				RedfishClientFactory: func(context.Context, string, string, string, bool, []byte) (internalredfish.Client, error) {
					return mockRf, nil
				},
			}
			_, err := r.reconcileDelete(ctx, r.Log, b7m)
			Expect(err).NotTo(HaveOccurred())
			Expect(controllerutil.ContainsFinalizer(b7m, Beskar7MachineFinalizer)).To(BeFalse(), "the finalizer is always dropped so deletion completes")

			after := getPhysicalHost(client.ObjectKeyFromObject(host))
			if movingAway {
				Expect(mockRf.SetPowerStateCalled).To(BeFalse(), "the source side of a move must not power the host off")
				Expect(mockRf.ClearBootSourceOverrideCalled).To(BeFalse())
				Expect(after.Spec.ConsumerRef).NotTo(BeNil(), "the claim must survive on the source; the target now owns it")
			} else {
				Expect(mockRf.SetPowerStateCalled).To(BeTrue(), "an ordinary delete still releases the host over Redfish")
				Expect(after.Spec.ConsumerRef).To(BeNil(), "an ordinary delete still releases the claim")
			}
		},
		Entry("carrying the annotation: leaves the hardware alone", true),
		Entry("without the annotation: releases as always", false),
	)
})

var _ = Describe("PhysicalHostReconciler's Beskar7Machine watch", func() {
	It("maps a Beskar7Machine to the PhysicalHost its ProviderID names, same-namespace only", func() {
		r := &PhysicalHostReconciler{Log: ctrl.Log.WithName("move-watch-mapper")}

		named := &infrav1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: "ns-a"},
			Spec:       infrav1.Beskar7MachineSpec{ProviderID: providerID("ns-a", "host-1")},
		}
		Expect(r.Beskar7MachineToPhysicalHost(ctx, named)).To(ConsistOf(
			reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns-a", Name: "host-1"}}))

		empty := &infrav1.Beskar7Machine{ObjectMeta: metav1.ObjectMeta{Name: "m2", Namespace: "ns-a"}}
		Expect(r.Beskar7MachineToPhysicalHost(ctx, empty)).To(BeEmpty(), "no ProviderID: nothing to map")

		cross := &infrav1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "m3", Namespace: "ns-a"},
			Spec:       infrav1.Beskar7MachineSpec{ProviderID: providerID("ns-b", "host-1")},
		}
		Expect(r.Beskar7MachineToPhysicalHost(ctx, cross)).To(BeEmpty(), "a ProviderID naming a different namespace than the machine's own is never produced by this controller")

		notAMachine := &corev1.Secret{}
		Expect(r.Beskar7MachineToPhysicalHost(ctx, notAMachine)).To(BeEmpty())
	})

	It("admits only Create-with-ProviderID and an Update that changes ProviderID", func() {
		pred := beskar7MachineProviderIDLanded()
		withID := &infrav1.Beskar7Machine{Spec: infrav1.Beskar7MachineSpec{ProviderID: "b7://ns/host"}}
		otherID := &infrav1.Beskar7Machine{Spec: infrav1.Beskar7MachineSpec{ProviderID: "b7://ns/other-host"}}
		withoutID := &infrav1.Beskar7Machine{}

		Expect(pred.Create(event.CreateEvent{Object: withID})).To(BeTrue())
		Expect(pred.Create(event.CreateEvent{Object: withoutID})).To(BeFalse())

		Expect(pred.Update(event.UpdateEvent{ObjectOld: withoutID, ObjectNew: withID})).To(BeTrue(), "ProviderID just landed")
		Expect(pred.Update(event.UpdateEvent{ObjectOld: withID, ObjectNew: otherID})).To(BeTrue(), "ProviderID changed")
		Expect(pred.Update(event.UpdateEvent{ObjectOld: withID, ObjectNew: withID})).To(BeFalse(), "no change: status churn or an unrelated spec edit")

		Expect(pred.Delete(event.DeleteEvent{Object: withID})).To(BeFalse())
		Expect(pred.Generic(event.GenericEvent{Object: withID})).To(BeFalse())
	})

	It("wakes the host as soon as its consumer's Beskar7Machine is created, without a manual nudge", func() {
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "move-watch-live-"}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		defer func() { Expect(k8sClient.Delete(ctx, nsObj)).To(Succeed()) }()
		Expect(k8sClient.Create(ctx, bmcCredentialsSecret(nsObj.Name))).To(Succeed())

		host := claimedPhysicalHost(nsObj.Name, "watch-live-host", "watch-live-machine")
		host.Finalizers = []string{PhysicalHostFinalizer}
		Expect(k8sClient.Create(ctx, host)).To(Succeed())
		// Left at State "" deliberately: the point under test is the watch, not
		// the state the host starts from.

		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:                 k8sClient.Scheme(),
			Metrics:                metricsserver.Options{BindAddress: "0"},
			HealthProbeBindAddress: "0",
			Cache:                  cache.Options{DefaultNamespaces: map[string]cache.Config{nsObj.Name: {}}},
			Controller:             config.Controller{SkipNameValidation: ptr.To(true)},
		})
		Expect(err).NotTo(HaveOccurred())

		Expect((&PhysicalHostReconciler{
			Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
			Log:      ctrl.Log.WithName("move-watch-live-host"),
			Recorder: record.NewFakeRecorder(100),
			RedfishClientFactory: func(context.Context, string, string, string, bool, []byte) (internalredfish.Client, error) {
				return internalredfish.NewMockClient(), nil
			},
		}).SetupWithManager(mgr)).To(Succeed())

		mgrCtx, mgrCancel := context.WithCancel(ctx)
		defer mgrCancel()
		go func() {
			defer GinkgoRecover()
			Expect(mgr.Start(mgrCtx)).To(Succeed())
		}()
		Expect(mgr.GetCache().WaitForCacheSync(mgrCtx)).To(BeTrue())

		By("letting the host settle at InUse with no consumer yet")
		Eventually(func(g Gomega) {
			h := getPhysicalHost(client.ObjectKeyFromObject(host))
			g.Expect(h.Status.State).To(Equal(infrav1.StateInUse))
		}, 10*time.Second, 100*time.Millisecond).Should(Succeed())

		By("creating the Beskar7Machine that already holds this host by ProviderID, with no host reconcile triggered by hand")
		b7mSpec := minimalImageSpec()
		b7mSpec.ProviderID = providerID(nsObj.Name, host.Name)
		b7m := &infrav1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "watch-live-machine", Namespace: nsObj.Name},
			Spec:       b7mSpec,
		}
		Expect(k8sClient.Create(ctx, b7m)).To(Succeed())

		Eventually(func(g Gomega) {
			h := getPhysicalHost(client.ObjectKeyFromObject(host))
			g.Expect(h.Status.State).To(Equal(infrav1.StateReady))
		}, 10*time.Second, 100*time.Millisecond).Should(Succeed())
	})
})
