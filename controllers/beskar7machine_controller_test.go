package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/stmcginnis/gofish/redfish"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	conditions "sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	infrastructurev1beta1 "github.com/projectbeskar/beskar7/api/v1beta1"
	internalredfish "github.com/projectbeskar/beskar7/internal/redfish"
)

var _ = Describe("Beskar7MachineReconciler factory defaulting", func() {
	It("should default RedfishClientFactory to internalredfish.NewClient when nil", func() {
		r := &Beskar7MachineReconciler{}
		Expect(r.RedfishClientFactory).To(BeNil())

		Expect(r.defaultFactory()).To(Succeed())

		Expect(r.RedfishClientFactory).NotTo(BeNil(),
			"factory must be non-nil after defaultFactory()")
	})

	It("should preserve an explicitly provided factory", func() {
		sentinel := internalredfish.RedfishClientFactory(
			func(_ context.Context, _, _, _ string, _ bool, _ []byte) (internalredfish.Client, error) {
				return internalredfish.NewMockClient(), nil
			},
		)
		r := &Beskar7MachineReconciler{RedfishClientFactory: sentinel}

		Expect(r.defaultFactory()).To(Succeed())

		// Pointer equality is not directly comparable for func types in Go; verify
		// the factory is still the one we set by calling it and checking the result type.
		client, err := r.RedfishClientFactory(ctx, "", "", "", false, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(client).To(BeAssignableToTypeOf(&internalredfish.MockClient{}))
	})
})

var _ = Describe("Beskar7Machine Controller", func() {

	// Note: the Timeout / Interval constants previously declared at this scope
	// were used by the v0.3-era PIt blocks that PR-10 converted or deleted.
	// The remaining specs in this Describe either don't need Eventually polling
	// or use literal "5s" / "100ms" timeouts inline. The block-scoped Timeout /
	// Interval constants below (around line 951) belong to the bootstrap-data
	// Describe and remain in use.

	Context("When reconciling a Beskar7Machine", func() {
		var beskar7Machine *infrastructurev1beta1.Beskar7Machine
		var physicalHost *infrastructurev1beta1.PhysicalHost
		var credentialSecret *corev1.Secret
		var reconciler *Beskar7MachineReconciler
		var testNs *corev1.Namespace

		BeforeEach(func() {
			// Create unique namespace
			testNs = &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "beskar7machine-test-",
				},
			}
			Expect(k8sClient.Create(ctx, testNs)).To(Succeed())

			// Create credential secret
			credentialSecret = &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-bmc-creds",
					Namespace: testNs.Name,
				},
				Data: map[string][]byte{
					"username": []byte("admin"),
					"password": []byte("password"),
				},
			}
			Expect(k8sClient.Create(ctx, credentialSecret)).To(Succeed())

			// Create available PhysicalHost
			physicalHost = &infrastructurev1beta1.PhysicalHost{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-host",
					Namespace: testNs.Name,
				},
				Spec: infrastructurev1beta1.PhysicalHostSpec{
					RedfishConnection: infrastructurev1beta1.RedfishConnection{
						Address:              "https://192.168.1.100",
						CredentialsSecretRef: credentialSecret.Name,
					},
				},
				Status: infrastructurev1beta1.PhysicalHostStatus{
					State: infrastructurev1beta1.StateAvailable,
					Ready: true,
				},
			}
			Expect(k8sClient.Create(ctx, physicalHost)).To(Succeed())
			Expect(k8sClient.Status().Update(ctx, physicalHost)).To(Succeed())

			// Create Beskar7Machine
			beskar7Machine = &infrastructurev1beta1.Beskar7Machine{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-machine",
					Namespace: testNs.Name,
				},
				Spec: infrastructurev1beta1.Beskar7MachineSpec{
					InspectionImageURL: "http://boot-server/ipxe/inspect.ipxe",
					TargetImageURL:     "http://boot-server/images/kairos.tar.gz",
				},
			}

			// Create reconciler
			reconciler = &Beskar7MachineReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Log:    ctrl.Log.WithName("beskar7machine-test"),
			}
		})

		AfterEach(func() {
			Expect(k8sClient.Delete(ctx, testNs)).To(Succeed())
		})

		// Regression test for the re-find bug: once a host is claimed (state
		// transitions Available -> InUse), findAndClaimOrGetAssociatedHost
		// must still return it on subsequent calls. Previously the function
		// only looked up by Spec.ProviderID (not set until inspection
		// completes) or by Status.State=Available (filtered out claimed
		// hosts), so a claimed-but-not-yet-provisioned host was invisible to
		// the controller — it would loop "No available host, requeuing"
		// indefinitely after the initial claim, never reaching
		// triggerInspection. Added a third lookup branch by
		// Spec.ConsumerRef.Name pointing back at this Beskar7Machine.
		It("Should re-find a claimed PhysicalHost via ConsumerRef on subsequent calls", func() {
			By("Claiming the host: set ConsumerRef + transition status.state to InUse")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: physicalHost.Name, Namespace: testNs.Name}, physicalHost)).To(Succeed())
			base := physicalHost.DeepCopy()
			physicalHost.Spec.ConsumerRef = &corev1.ObjectReference{
				Kind:       "Beskar7Machine",
				APIVersion: InfrastructureAPIVersion,
				Name:       beskar7Machine.Name,
				Namespace:  testNs.Name,
			}
			Expect(k8sClient.Patch(ctx, physicalHost, client.MergeFrom(base))).To(Succeed())

			// Move status into InUse (not Available) — the failure mode this
			// test guards against happens precisely when status.state != Available.
			physicalHost.Status.State = infrastructurev1beta1.StateInUse
			Expect(k8sClient.Status().Update(ctx, physicalHost)).To(Succeed())

			By("Calling findAndClaimOrGetAssociatedHost: should return our host via ConsumerRef")
			// beskar7Machine.Spec.ProviderID is unset (set later in handleReadyHost),
			// status.state is InUse so the StateAvailable index won't return it — only
			// the ConsumerRef branch can.
			got, result, err := reconciler.findAndClaimOrGetAssociatedHost(ctx, ctrl.Log.WithName("refind-test"), beskar7Machine)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}), "no requeue expected — host is already claimed by us")
			Expect(got).NotTo(BeNil(), "ConsumerRef lookup must re-find the claimed host")
			Expect(got.Name).To(Equal(physicalHost.Name))
			Expect(got.Spec.ConsumerRef).NotTo(BeNil())
			Expect(got.Spec.ConsumerRef.Name).To(Equal(beskar7Machine.Name))
		})

		// Converted from PIt "[SKIP - Hardware Testing] Should successfully claim an available PhysicalHost":
		// This no longer requires Redfish (claim path only touches Spec.ConsumerRef, no Redfish calls
		// until triggerInspection). We skip the Machine OwnerRef requirement here because this
		// controller test calls Reconcile directly without a real CAPI Machine object — the reconciler
		// returns early at "Waiting for Machine Controller to set OwnerRef" which is fine for
		// verifying the claim + no-Status-write invariant.
		It("Should set ConsumerRef on PhysicalHost spec (not status) when claiming", func() {
			By("Creating the Beskar7Machine")
			Expect(k8sClient.Create(ctx, beskar7Machine)).To(Succeed())

			machineLookupKey := types.NamespacedName{Name: beskar7Machine.Name, Namespace: beskar7Machine.Namespace}

			By("First reconcile: no ownerRef yet, controller waits — no claim, no status write")
			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: machineLookupKey})
			Expect(err).NotTo(HaveOccurred())

			// No owner Machine → should not have claimed any host yet.
			hostKey := types.NamespacedName{Name: physicalHost.Name, Namespace: physicalHost.Namespace}
			got := &infrastructurev1beta1.PhysicalHost{}
			Expect(k8sClient.Get(ctx, hostKey, got)).To(Succeed())
			Expect(got.Spec.ConsumerRef).To(BeNil(), "no claim should happen without an owner Machine")
		})

		// New envtest: proves the Beskar7Machine controller never writes to PhysicalHost.Status.
		// It verifies this for the annotation-signal path: after triggerInspection the only change
		// on PhysicalHost is the InspectionRequestAnnotation in metadata.annotations — never a
		// status subresource write. We set up a PhysicalHost with State=InUse and a matching
		// ConsumerRef, then call triggerInspection directly via the helper method.
		It("Should not write to PhysicalHost.Status from Beskar7Machine controller (BUG-1)", func() {
			By("Marking PhysicalHost as InUse with a matching ConsumerRef")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: physicalHost.Name, Namespace: testNs.Name}, physicalHost)).To(Succeed())
			base := physicalHost.DeepCopy()
			physicalHost.Spec.ConsumerRef = &corev1.ObjectReference{
				Kind:       "Beskar7Machine",
				APIVersion: InfrastructureAPIVersion,
				Name:       beskar7Machine.Name,
				Namespace:  testNs.Name,
			}
			Expect(k8sClient.Patch(ctx, physicalHost, client.MergeFrom(base))).To(Succeed())

			// Persist InUse in status
			Expect(k8sClient.Status().Update(ctx, physicalHost)).To(Succeed())

			By("Capturing PhysicalHost status before calling setInspectionRequestAnnotation")
			before := &infrastructurev1beta1.PhysicalHost{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: physicalHost.Name, Namespace: testNs.Name}, before)).To(Succeed())
			statusBefore := before.Status.DeepCopy()

			By("Calling setInspectionRequestAnnotation (the method that replaced r.Status().Update)")
			mockRf := internalredfish.NewMockClient()
			r := &Beskar7MachineReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Log:    ctrl.Log.WithName("beskar7machine-bug1-test"),
				RedfishClientFactory: func(_ context.Context, _, _, _ string, _ bool, _ []byte) (internalredfish.Client, error) {
					return mockRf, nil
				},
			}
			Expect(r.setInspectionRequestAnnotation(ctx, r.Log, physicalHost, "inspect")).To(Succeed())

			By("Verifying PhysicalHost.Status is unchanged after annotation call")
			after := &infrastructurev1beta1.PhysicalHost{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: physicalHost.Name, Namespace: testNs.Name}, after)).To(Succeed())

			// Status must be identical — no state transition, no phase change.
			Expect(after.Status.State).To(Equal(statusBefore.State),
				"Beskar7Machine controller must not write to PhysicalHost.Status.State")
			Expect(after.Status.InspectionPhase).To(Equal(statusBefore.InspectionPhase),
				"Beskar7Machine controller must not write to PhysicalHost.Status.InspectionPhase")
			Expect(after.Status.InspectionTimestamp).To(Equal(statusBefore.InspectionTimestamp),
				"Beskar7Machine controller must not write to PhysicalHost.Status.InspectionTimestamp")

			By("Verifying annotation IS set (the signal to PhysicalHost controller)")
			Expect(after.Annotations).To(HaveKeyWithValue(InspectionRequestAnnotation, "inspect"))
		})

		// "[SKIP - Hardware Testing] Should transition host to Inspecting state"
		// was deleted: the spec immediately above
		// ("Should not write to PhysicalHost.Status from Beskar7Machine controller")
		// covers the same surface — the Beskar7Machine controller signals the
		// transition via the InspectionRequestAnnotation rather than writing
		// PhysicalHost.Status itself (PR-2.1 / BUG-1). Asserting the post-
		// annotation StateInspecting transition belongs in the PhysicalHost
		// controller test (where applyInspectionRequest is exercised).

		// Rewritten from "[SKIP - Hardware Testing] Should handle inspection completion".
		// The original spec referenced the removed StateProvisioned constant. In
		// v0.4 the flow is: validateInspectionReport runs against the report,
		// passes hardware checks, and signals "inspect-complete" to PhysicalHost
		// via setInspectionRequestAnnotation. PhysicalHost owns its status
		// (PR-2.1 / BUG-1), so we assert the annotation handoff, not a
		// direct StateReady write.
		It("Should signal inspect-complete via annotation when validateInspectionReport passes hardware checks", func() {
			beskar7Machine.Namespace = testNs.Name
			beskar7Machine.Spec.HardwareRequirements = &infrastructurev1beta1.HardwareRequirements{
				MinCPUCores: 8,
				MinMemoryGB: 16,
				MinDiskGB:   100,
			}
			Expect(k8sClient.Create(ctx, beskar7Machine)).To(Succeed())

			// Build a report that comfortably satisfies the requirements.
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: physicalHost.Name, Namespace: testNs.Name}, physicalHost)).To(Succeed())
			physicalHost.Status.InspectionPhase = infrastructurev1beta1.InspectionPhaseComplete
			physicalHost.Status.InspectionReport = &infrastructurev1beta1.InspectionReport{
				Timestamp:    metav1.Now(),
				Manufacturer: "Dell Inc.",
				Model:        "PowerEdge R650",
				CPUs:         []infrastructurev1beta1.CPUInfo{{ID: "0", Cores: 18, Threads: 36}},
				Memory:       []infrastructurev1beta1.MemoryInfo{{ID: "DIMM0", Capacity: "32GB"}},
				Disks:        []infrastructurev1beta1.DiskInfo{{Name: "sda", SizeGB: 500}},
			}
			Expect(k8sClient.Status().Update(ctx, physicalHost)).To(Succeed())

			result, err := reconciler.validateInspectionReport(ctx, reconciler.Log, beskar7Machine, physicalHost)
			Expect(err).NotTo(HaveOccurred(), "passing hardware checks must not error")
			Expect(result.Requeue).To(BeTrue(), "post-inspection should requeue once to observe the new state")

			// FailureReason must NOT be set on a passing report.
			Expect(beskar7Machine.Status.FailureReason).To(BeNil(),
				"successful validation must leave FailureReason unset")

			// Annotation handoff: validateInspectionReport calls
			// setInspectionRequestAnnotation("inspect-complete") on success.
			updated := &infrastructurev1beta1.PhysicalHost{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: physicalHost.Name, Namespace: testNs.Name}, updated)).To(Succeed())
			Expect(updated.Annotations).To(HaveKeyWithValue(InspectionRequestAnnotation, "inspect-complete"),
				"validateInspectionReport must signal inspect-complete via annotation; PhysicalHost owns the status transition")
		})

		// Note: "[SKIP - Hardware Testing] Should handle no available hosts"
		// (the original PIt) is now covered by the
		// "Should return no host and no error when zero PhysicalHosts are Available"
		// spec in the race-test Describe block below. The race-test block has a
		// manager-backed cache with PhysicalHostStateIndex registered, which is
		// required by the client.MatchingFields filter that
		// findAndClaimOrGetAssociatedHost uses.

		// Converted from PIt "[SKIP - Hardware Testing] Should handle deletion and release host":
		// The deletion path no longer requires a real CAPI Machine owner — reconcileDelete is
		// called directly. We set up ProviderID + ConsumerRef manually and inject a MockClient.
		It("Should clear boot source override and power off the host before clearing ConsumerRef", func() {
			mockRf := internalredfish.NewMockClient()
			mockRf.PowerState = redfish.OnPowerState
			mockRf.BootSourceIsPXE = true

			r := &Beskar7MachineReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Log:    ctrl.Log.WithName("beskar7machine-delete-test"),
				RedfishClientFactory: func(_ context.Context, _, _, _ string, _ bool, _ []byte) (internalredfish.Client, error) {
					return mockRf, nil
				},
			}

			By("Setting ConsumerRef on PhysicalHost")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: physicalHost.Name, Namespace: testNs.Name}, physicalHost)).To(Succeed())
			base := physicalHost.DeepCopy()
			physicalHost.Spec.ConsumerRef = &corev1.ObjectReference{
				Kind:       "Beskar7Machine",
				APIVersion: InfrastructureAPIVersion,
				Name:       "delete-test-machine",
				Namespace:  testNs.Name,
			}
			Expect(k8sClient.Patch(ctx, physicalHost, client.MergeFrom(base))).To(Succeed())

			By("Creating a Beskar7Machine with ProviderID pointing at the host")
			provID := "b7://" + testNs.Name + "/" + physicalHost.Name
			b7m := &infrastructurev1beta1.Beskar7Machine{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "delete-test-machine",
					Namespace:  testNs.Name,
					Finalizers: []string{Beskar7MachineFinalizer},
				},
				Spec: infrastructurev1beta1.Beskar7MachineSpec{
					InspectionImageURL: "http://boot-server/inspect.ipxe",
					TargetImageURL:     "http://boot-server/kairos.tar.gz",
					ProviderID:         &provID,
				},
			}
			Expect(k8sClient.Create(ctx, b7m)).To(Succeed())

			By("Calling reconcileDelete directly")
			_, err := r.reconcileDelete(ctx, r.Log, b7m)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying ClearBootSourceOverride was called")
			Expect(mockRf.ClearBootSourceOverrideCalled).To(BeTrue(),
				"ClearBootSourceOverride must be called on clean release")

			By("Verifying SetPowerState(Off) was called")
			Expect(mockRf.SetPowerStateCalled).To(BeTrue(),
				"SetPowerState must be called on clean release")
			Expect(mockRf.PowerState).To(Equal(redfish.OffPowerState))

			By("Verifying ConsumerRef is nil on the host")
			hostAfter := &infrastructurev1beta1.PhysicalHost{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: physicalHost.Name, Namespace: testNs.Name}, hostAfter)).To(Succeed())
			Expect(hostAfter.Spec.ConsumerRef).To(BeNil(), "ConsumerRef must be cleared after deletion")

			By("Cleanup: remove finalizer so the machine can be GC'd")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: b7m.Name, Namespace: testNs.Name}, b7m)).To(Succeed())
			b7mBase := b7m.DeepCopy()
			b7m.Finalizers = nil
			Expect(k8sClient.Patch(ctx, b7m, client.MergeFrom(b7mBase))).To(Succeed())
		})

		It("Should skip Redfish ops when force-release annotation is set", func() {
			mockRf := internalredfish.NewMockClient()

			r := &Beskar7MachineReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Log:    ctrl.Log.WithName("beskar7machine-forcerelease-test"),
				RedfishClientFactory: func(_ context.Context, _, _, _ string, _ bool, _ []byte) (internalredfish.Client, error) {
					return mockRf, nil
				},
			}

			By("Setting ConsumerRef on PhysicalHost")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: physicalHost.Name, Namespace: testNs.Name}, physicalHost)).To(Succeed())
			base := physicalHost.DeepCopy()
			physicalHost.Spec.ConsumerRef = &corev1.ObjectReference{
				Kind:       "Beskar7Machine",
				APIVersion: InfrastructureAPIVersion,
				Name:       "force-release-machine",
				Namespace:  testNs.Name,
			}
			Expect(k8sClient.Patch(ctx, physicalHost, client.MergeFrom(base))).To(Succeed())

			By("Creating a Beskar7Machine with force-release annotation")
			provID := "b7://" + testNs.Name + "/" + physicalHost.Name
			b7m := &infrastructurev1beta1.Beskar7Machine{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "force-release-machine",
					Namespace:  testNs.Name,
					Finalizers: []string{Beskar7MachineFinalizer},
					Annotations: map[string]string{
						ForceReleaseAnnotation: "true",
					},
				},
				Spec: infrastructurev1beta1.Beskar7MachineSpec{
					InspectionImageURL: "http://boot-server/inspect.ipxe",
					TargetImageURL:     "http://boot-server/kairos.tar.gz",
					ProviderID:         &provID,
				},
			}
			Expect(k8sClient.Create(ctx, b7m)).To(Succeed())

			By("Calling reconcileDelete directly")
			_, err := r.reconcileDelete(ctx, r.Log, b7m)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying ClearBootSourceOverride was NOT called")
			Expect(mockRf.ClearBootSourceOverrideCalled).To(BeFalse(),
				"ClearBootSourceOverride must be skipped on force-release")

			By("Verifying SetPowerState was NOT called")
			Expect(mockRf.SetPowerStateCalled).To(BeFalse(),
				"SetPowerState must be skipped on force-release")

			By("Verifying ConsumerRef is still cleared on the host")
			hostAfter := &infrastructurev1beta1.PhysicalHost{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: physicalHost.Name, Namespace: testNs.Name}, hostAfter)).To(Succeed())
			Expect(hostAfter.Spec.ConsumerRef).To(BeNil(), "ConsumerRef must be cleared even on force-release")

			By("Cleanup: remove finalizer")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: b7m.Name, Namespace: testNs.Name}, b7m)).To(Succeed())
			b7mBase := b7m.DeepCopy()
			b7m.Finalizers = nil
			Expect(k8sClient.Patch(ctx, b7m, client.MergeFrom(b7mBase))).To(Succeed())
		})

		It("Should remove finalizer cleanly when the host is already gone", func() {
			r := &Beskar7MachineReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Log:    ctrl.Log.WithName("beskar7machine-hostgone-test"),
				RedfishClientFactory: func(_ context.Context, _, _, _ string, _ bool, _ []byte) (internalredfish.Client, error) {
					return internalredfish.NewMockClient(), nil
				},
			}

			By("Creating a Beskar7Machine pointing at a non-existent host")
			provID := "b7://" + testNs.Name + "/does-not-exist"
			b7m := &infrastructurev1beta1.Beskar7Machine{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "gone-host-machine",
					Namespace:  testNs.Name,
					Finalizers: []string{Beskar7MachineFinalizer},
				},
				Spec: infrastructurev1beta1.Beskar7MachineSpec{
					InspectionImageURL: "http://boot-server/inspect.ipxe",
					TargetImageURL:     "http://boot-server/kairos.tar.gz",
					ProviderID:         &provID,
				},
			}
			Expect(k8sClient.Create(ctx, b7m)).To(Succeed())

			By("Calling reconcileDelete — should not error even though host is missing")
			_, err := r.reconcileDelete(ctx, r.Log, b7m)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying finalizer was removed from the in-memory object")
			Expect(controllerutil.ContainsFinalizer(b7m, Beskar7MachineFinalizer)).To(BeFalse())

			By("Cleanup: patch finalizer list in the API server")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: b7m.Name, Namespace: testNs.Name}, b7m)).To(Succeed())
			b7mBase := b7m.DeepCopy()
			b7m.Finalizers = nil
			Expect(k8sClient.Patch(ctx, b7m, client.MergeFrom(b7mBase))).To(Succeed())
		})

		It("Should not block deletion when the BMC is unreachable", func() {
			r := &Beskar7MachineReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Log:    ctrl.Log.WithName("beskar7machine-bmcfail-test"),
				RedfishClientFactory: func(_ context.Context, _, _, _ string, _ bool, _ []byte) (internalredfish.Client, error) {
					return nil, fmt.Errorf("BMC unreachable")
				},
			}

			By("Setting ConsumerRef on PhysicalHost")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: physicalHost.Name, Namespace: testNs.Name}, physicalHost)).To(Succeed())
			base := physicalHost.DeepCopy()
			physicalHost.Spec.ConsumerRef = &corev1.ObjectReference{
				Kind:       "Beskar7Machine",
				APIVersion: InfrastructureAPIVersion,
				Name:       "bmc-fail-machine",
				Namespace:  testNs.Name,
			}
			Expect(k8sClient.Patch(ctx, physicalHost, client.MergeFrom(base))).To(Succeed())

			By("Creating a Beskar7Machine")
			provID := "b7://" + testNs.Name + "/" + physicalHost.Name
			b7m := &infrastructurev1beta1.Beskar7Machine{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "bmc-fail-machine",
					Namespace:  testNs.Name,
					Finalizers: []string{Beskar7MachineFinalizer},
				},
				Spec: infrastructurev1beta1.Beskar7MachineSpec{
					InspectionImageURL: "http://boot-server/inspect.ipxe",
					TargetImageURL:     "http://boot-server/kairos.tar.gz",
					ProviderID:         &provID,
				},
			}
			Expect(k8sClient.Create(ctx, b7m)).To(Succeed())

			By("Calling reconcileDelete — should succeed despite BMC failure")
			_, err := r.reconcileDelete(ctx, r.Log, b7m)
			Expect(err).NotTo(HaveOccurred(), "deletion must not be blocked by an unreachable BMC")

			By("Verifying ConsumerRef was still cleared on the host")
			hostAfter := &infrastructurev1beta1.PhysicalHost{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: physicalHost.Name, Namespace: testNs.Name}, hostAfter)).To(Succeed())
			Expect(hostAfter.Spec.ConsumerRef).To(BeNil())

			By("Verifying finalizer was removed from the in-memory object")
			Expect(controllerutil.ContainsFinalizer(b7m, Beskar7MachineFinalizer)).To(BeFalse())

			By("Cleanup: remove finalizer in API server")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: b7m.Name, Namespace: testNs.Name}, b7m)).To(Succeed())
			b7mBase := b7m.DeepCopy()
			b7m.Finalizers = nil
			Expect(k8sClient.Patch(ctx, b7m, client.MergeFrom(b7mBase))).To(Succeed())
		})

		// Converted from PIt "[SKIP - Hardware Testing] Should handle pause annotation".
		// pause is honored on the Beskar7Machine path (controllers/utils.go isPaused
		// is called from Reconcile at controllers/beskar7machine_controller.go:123).
		// When paused, Reconcile returns immediately with no error and no requeue,
		// and never reaches the claim path.
		It("Should skip reconciliation when the pause annotation is set", func() {
			beskar7Machine.Namespace = testNs.Name
			beskar7Machine.Annotations = map[string]string{
				clusterv1.PausedAnnotation: "true",
			}
			Expect(k8sClient.Create(ctx, beskar7Machine)).To(Succeed())

			machineLookupKey := types.NamespacedName{Name: beskar7Machine.Name, Namespace: beskar7Machine.Namespace}

			result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: machineLookupKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}), "paused reconcile must return zero Result")

			// The PhysicalHost must remain unclaimed: paused Reconcile never
			// reaches findAndClaimOrGetAssociatedHost.
			unchangedHost := &infrastructurev1beta1.PhysicalHost{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: physicalHost.Name, Namespace: testNs.Name}, unchangedHost)).To(Succeed())
			Expect(unchangedHost.Spec.ConsumerRef).To(BeNil(), "paused reconcile must not claim a host")
		})

		// Converted from "[SKIP - Hardware Testing] Should validate hardware requirements".
		// validateInspectionReport is exercised directly because the full Reconcile path
		// requires a CAPI Machine owner chain that this test doesn't set up. The helper
		// is the unit under test for BUG-8 (terminal-failure wiring).
		Context("hardware-validation terminal failures (BUG-8)", func() {
			buildMachine := func(reqs *infrastructurev1beta1.HardwareRequirements) *infrastructurev1beta1.Beskar7Machine {
				return &infrastructurev1beta1.Beskar7Machine{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "validate-target",
						Namespace: testNs.Name,
					},
					Spec: infrastructurev1beta1.Beskar7MachineSpec{
						InspectionImageURL:   "http://boot-server/ipxe/inspect.ipxe",
						TargetImageURL:       "http://boot-server/images/kairos.tar.gz",
						HardwareRequirements: reqs,
					},
				}
			}

			buildHostWithReport := func(report *infrastructurev1beta1.InspectionReport) *infrastructurev1beta1.PhysicalHost {
				return &infrastructurev1beta1.PhysicalHost{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "validate-host",
						Namespace: testNs.Name,
					},
					Status: infrastructurev1beta1.PhysicalHostStatus{
						InspectionReport: report,
					},
				}
			}

			expectTerminalFailure := func(b *infrastructurev1beta1.Beskar7Machine, expectedReason string) {
				Expect(b.Status.FailureReason).NotTo(BeNil(), "FailureReason must be set on terminal failure")
				Expect(*b.Status.FailureReason).To(Equal(expectedReason))
				Expect(b.Status.FailureMessage).NotTo(BeNil(), "FailureMessage must be set")
				Expect(*b.Status.FailureMessage).NotTo(BeEmpty())
				Expect(b.Status.Ready).To(BeFalse())
				Expect(b.Status.Phase).NotTo(BeNil())
				Expect(*b.Status.Phase).To(Equal("Failed"))
				cond := conditions.Get(b, infrastructurev1beta1.InfrastructureReadyCondition)
				Expect(cond).NotTo(BeNil(), "InfrastructureReady condition must be set")
				Expect(cond.Status).To(Equal(corev1.ConditionFalse))
				Expect(cond.Reason).To(Equal(expectedReason))
				Expect(cond.Severity).To(Equal(clusterv1.ConditionSeverityError))
			}

			It("Should mark Beskar7Machine terminally Failed when CPU cores are insufficient", func() {
				machine := buildMachine(&infrastructurev1beta1.HardwareRequirements{MinCPUCores: 16})
				host := buildHostWithReport(&infrastructurev1beta1.InspectionReport{
					CPUs: []infrastructurev1beta1.CPUInfo{{ID: "0", Cores: 4}},
				})

				result, err := reconciler.validateInspectionReport(ctx, reconciler.Log, machine, host)
				Expect(err).NotTo(HaveOccurred(), "terminal failures must NOT return an error (would requeue forever)")
				Expect(result).To(Equal(ctrl.Result{}), "terminal failures must NOT requeue")
				expectTerminalFailure(machine, infrastructurev1beta1.HardwareRequirementsNotMetReason)
				Expect(*machine.Status.FailureMessage).To(ContainSubstring("CPU cores"))
				Expect(*machine.Status.FailureMessage).To(ContainSubstring("4"))
				Expect(*machine.Status.FailureMessage).To(ContainSubstring("16"))
			})

			It("Should mark Beskar7Machine terminally Failed when memory is insufficient", func() {
				machine := buildMachine(&infrastructurev1beta1.HardwareRequirements{MinMemoryGB: 64})
				host := buildHostWithReport(&infrastructurev1beta1.InspectionReport{
					CPUs:   []infrastructurev1beta1.CPUInfo{{ID: "0", Cores: 32}},
					Memory: []infrastructurev1beta1.MemoryInfo{{ID: "DIMM0", Capacity: "16GB"}},
				})

				result, err := reconciler.validateInspectionReport(ctx, reconciler.Log, machine, host)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(ctrl.Result{}))
				expectTerminalFailure(machine, infrastructurev1beta1.HardwareRequirementsNotMetReason)
				Expect(*machine.Status.FailureMessage).To(ContainSubstring("memory"))
				Expect(*machine.Status.FailureMessage).To(ContainSubstring("16"))
				Expect(*machine.Status.FailureMessage).To(ContainSubstring("64"))
			})

			It("Should mark Beskar7Machine terminally Failed when disk space is insufficient", func() {
				machine := buildMachine(&infrastructurev1beta1.HardwareRequirements{MinDiskGB: 1000})
				host := buildHostWithReport(&infrastructurev1beta1.InspectionReport{
					CPUs:   []infrastructurev1beta1.CPUInfo{{ID: "0", Cores: 32}},
					Memory: []infrastructurev1beta1.MemoryInfo{{ID: "DIMM0", Capacity: "128GB"}},
					Disks:  []infrastructurev1beta1.DiskInfo{{Name: "sda", SizeGB: 250}},
				})

				result, err := reconciler.validateInspectionReport(ctx, reconciler.Log, machine, host)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(ctrl.Result{}))
				expectTerminalFailure(machine, infrastructurev1beta1.HardwareRequirementsNotMetReason)
				Expect(*machine.Status.FailureMessage).To(ContainSubstring("disk"))
				Expect(*machine.Status.FailureMessage).To(ContainSubstring("250"))
				Expect(*machine.Status.FailureMessage).To(ContainSubstring("1000"))
			})

			It("Should NOT clear FailureReason on a subsequent reconcile (idempotent terminality)", func() {
				machine := buildMachine(&infrastructurev1beta1.HardwareRequirements{MinCPUCores: 16})
				host := buildHostWithReport(&infrastructurev1beta1.InspectionReport{
					CPUs: []infrastructurev1beta1.CPUInfo{{ID: "0", Cores: 4}},
				})

				_, err := reconciler.validateInspectionReport(ctx, reconciler.Log, machine, host)
				Expect(err).NotTo(HaveOccurred())
				Expect(machine.Status.FailureReason).NotTo(BeNil())
				originalReason := *machine.Status.FailureReason

				// Re-run validation (simulating a subsequent reconcile). The helper
				// must overwrite-but-not-clear: same reason, no transition to nil.
				_, err = reconciler.validateInspectionReport(ctx, reconciler.Log, machine, host)
				Expect(err).NotTo(HaveOccurred())
				Expect(machine.Status.FailureReason).NotTo(BeNil(), "FailureReason must persist across reconciles")
				Expect(*machine.Status.FailureReason).To(Equal(originalReason))
			})
		})

		Context("inspection timeout terminal failure (BUG-8)", func() {
			It("Should mark Beskar7Machine terminally Failed when inspection times out", func() {
				machine := &infrastructurev1beta1.Beskar7Machine{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "timeout-target",
						Namespace: testNs.Name,
					},
					Spec: infrastructurev1beta1.Beskar7MachineSpec{
						InspectionImageURL: "http://boot-server/ipxe/inspect.ipxe",
						TargetImageURL:     "http://boot-server/images/kairos.tar.gz",
					},
				}
				// Build a host with an InspectionTimestamp older than DefaultInspectionTimeout.
				host := &infrastructurev1beta1.PhysicalHost{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "timeout-host",
						Namespace: testNs.Name,
					},
					Spec: infrastructurev1beta1.PhysicalHostSpec{
						RedfishConnection: infrastructurev1beta1.RedfishConnection{
							Address:              "https://192.168.1.100",
							CredentialsSecretRef: credentialSecret.Name,
						},
					},
				}
				// Persist the host so setInspectionRequestAnnotation can patch it.
				// Status is a subresource — set it AFTER Create or it's dropped.
				Expect(k8sClient.Create(ctx, host)).To(Succeed())
				old := metav1.NewTime(time.Now().Add(-2 * DefaultInspectionTimeout))
				host.Status.State = infrastructurev1beta1.StateInspecting
				host.Status.InspectionTimestamp = &old
				Expect(k8sClient.Status().Update(ctx, host)).To(Succeed())

				result, err := reconciler.handleInspectingHost(ctx, reconciler.Log, machine, host)
				Expect(err).NotTo(HaveOccurred(), "terminal failures must NOT return an error")
				Expect(result).To(Equal(ctrl.Result{}), "terminal failures must NOT requeue")

				// Beskar7Machine in-memory state assertions.
				Expect(machine.Status.FailureReason).NotTo(BeNil())
				Expect(*machine.Status.FailureReason).To(Equal(infrastructurev1beta1.InspectionTimedOutReason))
				Expect(machine.Status.FailureMessage).NotTo(BeNil())
				Expect(*machine.Status.FailureMessage).To(ContainSubstring("Inspection did not complete"))
				Expect(machine.Status.Ready).To(BeFalse())
				Expect(machine.Status.Phase).NotTo(BeNil())
				Expect(*machine.Status.Phase).To(Equal("Failed"))

				// PhysicalHost should have received the timeout annotation.
				patchedHost := &infrastructurev1beta1.PhysicalHost{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: host.Name, Namespace: host.Namespace}, patchedHost)).To(Succeed())
				Expect(patchedHost.Annotations[InspectionRequestAnnotation]).To(Equal("timeout"))
			})
		})
	})
})

var _ = Describe("When two Beskar7Machines race for the same available host", func() {
	// This spec proves that the atomic claim (field-index List + MergeFromWithOptimisticLock
	// Patch) is race-safe: exactly one machine ends up owning the host, the loser gets
	// Requeue=true and no ProviderID.
	//
	// We spin up a minimal controller-runtime manager just for this spec so that the
	// reconciler's client is backed by the informer cache. This is required because
	// client.MatchingFields only works on a cached client that has the field index
	// registered — a plain client.New does not support it for custom status fields.

	var (
		testNs    *corev1.Namespace
		mgr       ctrl.Manager
		mgrCtx    context.Context
		mgrCancel context.CancelFunc
	)

	BeforeEach(func() {
		testNs = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "race-test-"},
		}
		Expect(k8sClient.Create(ctx, testNs)).To(Succeed())

		var err error
		skipNameValidation := true
		mgr, err = ctrl.NewManager(cfg, ctrl.Options{
			Scheme: k8sClient.Scheme(),
			// Disable the metrics server and health probes — not needed in tests.
			Metrics:                metricsserver.Options{BindAddress: "0"},
			HealthProbeBindAddress: "0",
			// Multiple specs in this Describe each spin up a fresh manager and
			// register the same Beskar7Machine controller name; without this,
			// the second BeforeEach errors on "controller with name X already
			// exists" because controller-runtime's metric registry is global.
			Controller: config.Controller{SkipNameValidation: &skipNameValidation},
		})
		Expect(err).NotTo(HaveOccurred())

		noopFactory := internalredfish.RedfishClientFactory(
			func(_ context.Context, _, _, _ string, _ bool, _ []byte) (internalredfish.Client, error) {
				return internalredfish.NewMockClient(), nil
			},
		)
		reconciler := &Beskar7MachineReconciler{
			Client:               mgr.GetClient(),
			Scheme:               mgr.GetScheme(),
			Log:                  ctrl.Log.WithName("race-test"),
			RedfishClientFactory: noopFactory,
			BootstrapURLBase:     "https://test-mgr.beskar7-system.svc:8082",
		}
		// SetupWithManager registers the PhysicalHostStateIndex on the manager's
		// cache indexer and adds the Beskar7Machine controller to the manager.
		Expect(reconciler.SetupWithManager(mgr)).To(Succeed())

		mgrCtx, mgrCancel = context.WithCancel(ctx)
		go func() {
			defer GinkgoRecover()
			Expect(mgr.Start(mgrCtx)).To(Succeed())
		}()
		// Wait until the manager's cache is synced before creating test objects.
		Expect(mgr.GetCache().WaitForCacheSync(mgrCtx)).To(BeTrue())
	})

	AfterEach(func() {
		mgrCancel()
		Expect(k8sClient.Delete(ctx, testNs)).To(Succeed())
	})

	It("Should allow exactly one machine to claim the host; the other gets Requeue=true", func() {
		By("Creating one available PhysicalHost with no ConsumerRef")
		host := &infrastructurev1beta1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "race-host",
				Namespace: testNs.Name,
			},
			Spec: infrastructurev1beta1.PhysicalHostSpec{
				RedfishConnection: infrastructurev1beta1.RedfishConnection{
					Address:              "https://192.168.100.1",
					CredentialsSecretRef: "irrelevant",
				},
			},
		}
		Expect(k8sClient.Create(ctx, host)).To(Succeed())
		// Status must be set via the status subresource.
		host.Status.State = infrastructurev1beta1.StateAvailable
		Expect(k8sClient.Status().Update(ctx, host)).To(Succeed())

		By("Creating two Beskar7Machines that would each want to claim the host")
		machineA := &infrastructurev1beta1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "machine-a", Namespace: testNs.Name},
			Spec: infrastructurev1beta1.Beskar7MachineSpec{
				InspectionImageURL: "http://boot-server/inspect.ipxe",
				TargetImageURL:     "http://boot-server/kairos.tar.gz",
			},
		}
		machineB := &infrastructurev1beta1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "machine-b", Namespace: testNs.Name},
			Spec: infrastructurev1beta1.Beskar7MachineSpec{
				InspectionImageURL: "http://boot-server/inspect.ipxe",
				TargetImageURL:     "http://boot-server/kairos.tar.gz",
			},
		}
		Expect(k8sClient.Create(ctx, machineA)).To(Succeed())
		Expect(k8sClient.Create(ctx, machineB)).To(Succeed())

		// Build a reconciler that uses the manager's cached client (required for
		// MatchingFields to work via the registered PhysicalHostStateIndex).
		r := &Beskar7MachineReconciler{
			Client: mgr.GetClient(),
			Scheme: mgr.GetScheme(),
			Log:    ctrl.Log.WithName("race-test-direct"),
			RedfishClientFactory: internalredfish.RedfishClientFactory(
				func(_ context.Context, _, _, _ string, _ bool, _ []byte) (internalredfish.Client, error) {
					return internalredfish.NewMockClient(), nil
				},
			),
		}

		// Both machines have no owner Machine, so Reconcile returns early at
		// "Waiting for Machine Controller to set OwnerRef" — before it reaches the
		// claim path. To exercise the claim path directly, we call
		// findAndClaimOrGetAssociatedHost which is the method under test.
		//
		// Wait for the manager's cache to reflect the host, host status, and both machines
		// before proceeding. The cache client (mgr.GetClient()) is a read-through to the
		// informer cache; Get returns NotFound until the list-watch catches up.
		hostKey := types.NamespacedName{Name: host.Name, Namespace: testNs.Name}
		Eventually(func(g Gomega) {
			cachedHost := &infrastructurev1beta1.PhysicalHost{}
			g.Expect(mgr.GetClient().Get(ctx, hostKey, cachedHost)).To(Succeed())
			g.Expect(cachedHost.Status.State).To(Equal(infrastructurev1beta1.StateAvailable))
		}, 10*time.Second, 100*time.Millisecond).Should(Succeed())

		// Re-fetch machines through the cache so they have a valid UID (needed by ConsumerRef).
		Eventually(func(g Gomega) {
			g.Expect(mgr.GetClient().Get(ctx, types.NamespacedName{Name: machineA.Name, Namespace: testNs.Name}, machineA)).To(Succeed())
			g.Expect(mgr.GetClient().Get(ctx, types.NamespacedName{Name: machineB.Name, Namespace: testNs.Name}, machineB)).To(Succeed())
		}, 10*time.Second, 100*time.Millisecond).Should(Succeed())

		By("Calling findAndClaimOrGetAssociatedHost for machine-a then machine-b in quick succession")
		log := ctrl.Log.WithName("race-test-direct")

		claimedByA, resultA, errA := r.findAndClaimOrGetAssociatedHost(ctx, log, machineA)
		claimedByB, resultB, errB := r.findAndClaimOrGetAssociatedHost(ctx, log, machineB)

		By("Asserting exactly one machine won and neither call returned a hard error")
		// Both calls must not return a hard error.
		Expect(errA).NotTo(HaveOccurred(), "machine-a must not return a hard error")
		Expect(errB).NotTo(HaveOccurred(), "machine-b must not return a hard error")

		// In a sequential execution: machine-a wins the Patch, machine-b sees
		// ConsumerRef already set on the only host and returns (nil, {}, nil) — no
		// hosts available for it. In a truly concurrent execution (e.g. multiple
		// goroutines), the loser gets a Conflict 409 and returns Requeue=true.
		// Either outcome is correct. The invariant under test is: the host ends up
		// claimed by exactly one machine, and the second caller either gets
		// Requeue=true (Conflict path) or nil/nil (no available hosts path).
		aWon := claimedByA != nil
		bWon := claimedByB != nil
		Expect(aWon || bWon).To(BeTrue(), "at least one machine must have claimed the host")
		Expect(aWon && bWon).To(BeFalse(), "both machines must not claim the host simultaneously")

		// The winner's result must not include Requeue (it already has the host).
		if aWon {
			Expect(resultA.Requeue).To(BeFalse(), "winning machine-a must not be told to requeue")
		} else {
			Expect(resultB.Requeue).To(BeFalse(), "winning machine-b must not be told to requeue")
		}

		By("Asserting host ConsumerRef points at exactly one machine")
		Eventually(func(g Gomega) {
			updatedHost := &infrastructurev1beta1.PhysicalHost{}
			g.Expect(k8sClient.Get(ctx, hostKey, updatedHost)).To(Succeed())
			g.Expect(updatedHost.Spec.ConsumerRef).NotTo(BeNil(), "host must be claimed by one machine")
			winner := "machine-a"
			if bWon {
				winner = "machine-b"
			}
			g.Expect(updatedHost.Spec.ConsumerRef.Name).To(Equal(winner))
		}, 5*time.Second, 100*time.Millisecond).Should(Succeed())

		By("Asserting the loser has no ProviderID set")
		// ProviderID is only set after the host is claimed and boots; in this test
		// neither machine has a CAPI owner so ProviderID will not be set on either.
		// The important invariant: the losing machine has no host associated.
		if aWon {
			Expect(machineB.Spec.ProviderID).To(BeNil(), "losing machine-b must have no ProviderID")
		} else {
			Expect(machineA.Spec.ProviderID).To(BeNil(), "losing machine-a must have no ProviderID")
		}
	})

})

// Converted from PIt "[SKIP - Hardware Testing] Should handle no available hosts".
// Uses controller-runtime's fake client with the PhysicalHostStateIndex field
// indexer registered. This is lighter than the manager-backed setup the race
// test uses, and importantly it avoids the controller-name collision that
// happens when multiple managers register the same controller in the same
// process.
var _ = Describe("findAndClaimOrGetAssociatedHost with no Available hosts", func() {
	It("Should return no host and no error when zero PhysicalHosts are Available", func() {
		host := &infrastructurev1beta1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{Name: "no-avail-host", Namespace: "default"},
			Status:     infrastructurev1beta1.PhysicalHostStatus{State: infrastructurev1beta1.StateInUse},
		}
		machine := &infrastructurev1beta1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "no-avail-machine", Namespace: "default"},
			Spec: infrastructurev1beta1.Beskar7MachineSpec{
				InspectionImageURL: "http://boot/inspect.ipxe",
				TargetImageURL:     "http://boot/kairos.tar.gz",
			},
		}

		fakeC := fake.NewClientBuilder().
			WithScheme(scheme.Scheme).
			WithObjects(machine).
			WithStatusSubresource(host).
			// Same indexer SetupWithManager registers on the real cache.
			WithIndex(&infrastructurev1beta1.PhysicalHost{}, PhysicalHostStateIndex, func(obj client.Object) []string {
				h, ok := obj.(*infrastructurev1beta1.PhysicalHost)
				if !ok {
					return nil
				}
				return []string{string(h.Status.State)}
			}).
			Build()
		// Add the host with its status set; fake client requires the status
		// to be applied via the status subresource path.
		Expect(fakeC.Create(context.Background(), host)).To(Succeed())
		Expect(fakeC.Status().Update(context.Background(), host)).To(Succeed())

		r := &Beskar7MachineReconciler{
			Client: fakeC,
			Scheme: scheme.Scheme,
			Log:    ctrl.Log.WithName("no-avail-test"),
		}

		got, result, err := r.findAndClaimOrGetAssociatedHost(context.Background(), r.Log, machine)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(BeNil(), "no Available host means no host to return")
		Expect(result).To(Equal(ctrl.Result{}), "the helper does not requeue itself; the caller does")
	})
})

// Bootstrap data secret tests — envtest + unit level.
var _ = Describe("Beskar7Machine bootstrap data secret handling", func() {
	const (
		bootstrapURLBase = "https://beskar7-controller-manager.beskar7-system.svc:8082"
		Timeout          = time.Second * 10
		Interval         = time.Millisecond * 250
	)

	var (
		testNs       *corev1.Namespace
		physicalHost *infrastructurev1beta1.PhysicalHost
		b7machine    *infrastructurev1beta1.Beskar7Machine
		machine      *clusterv1.Machine
		r            *Beskar7MachineReconciler
	)

	BeforeEach(func() {
		testNs = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "bootstrap-test-"},
		}
		Expect(k8sClient.Create(ctx, testNs)).To(Succeed())

		// Create a PhysicalHost already associated (ConsumerRef set, State=InUse).
		physicalHost = &infrastructurev1beta1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "bootstrap-test-host",
				Namespace: testNs.Name,
			},
			Spec: infrastructurev1beta1.PhysicalHostSpec{
				RedfishConnection: infrastructurev1beta1.RedfishConnection{
					Address:              "https://192.168.1.200",
					CredentialsSecretRef: "irrelevant",
				},
			},
		}
		Expect(k8sClient.Create(ctx, physicalHost)).To(Succeed())
		physicalHost.Status.State = infrastructurev1beta1.StateInUse
		Expect(k8sClient.Status().Update(ctx, physicalHost)).To(Succeed())

		b7machine = &infrastructurev1beta1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "bootstrap-test-b7machine",
				Namespace: testNs.Name,
			},
			Spec: infrastructurev1beta1.Beskar7MachineSpec{
				InspectionImageURL: "http://boot-server/inspect.ipxe",
				TargetImageURL:     "http://boot-server/kairos.tar.gz",
			},
		}
		Expect(k8sClient.Create(ctx, b7machine)).To(Succeed())

		// Build a minimal CAPI Machine (no owner cluster; we only need Spec.Bootstrap).
		machine = &clusterv1.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "bootstrap-test-machine",
				Namespace: testNs.Name,
			},
			Spec: clusterv1.MachineSpec{
				ClusterName: "fake-cluster",
				Bootstrap:   clusterv1.Bootstrap{},
			},
		}

		r = &Beskar7MachineReconciler{
			Client:           k8sClient,
			Scheme:           k8sClient.Scheme(),
			Log:              ctrl.Log.WithName("bootstrap-test"),
			BootstrapURLBase: bootstrapURLBase,
			RedfishClientFactory: func(_ context.Context, _, _, _ string, _ bool, _ []byte) (internalredfish.Client, error) {
				return internalredfish.NewMockClient(), nil
			},
		}
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, testNs)).To(Succeed())
	})

	It("Should mark BootstrapDataReadyCondition=False/WaitingForBootstrapData when DataSecretName is nil", func() {
		By("Calling ensureBootstrapDataReady with no DataSecretName")
		// machine.Spec.Bootstrap.DataSecretName is nil (zero value)
		result, err := r.ensureBootstrapDataReady(ctx, r.Log, b7machine, machine, physicalHost)

		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(30*time.Second),
			"should requeue after 30s while waiting for bootstrap data secret name")

		cond := conditions.Get(b7machine, infrastructurev1beta1.BootstrapDataReadyCondition)
		Expect(cond).NotTo(BeNil(), "BootstrapDataReadyCondition must be set")
		Expect(cond.Status).To(Equal(corev1.ConditionFalse))
		Expect(cond.Reason).To(Equal(infrastructurev1beta1.WaitingForBootstrapDataReason))

		By("Verifying no bootstrap-url annotation was set on PhysicalHost")
		ph := &infrastructurev1beta1.PhysicalHost{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: physicalHost.Name, Namespace: testNs.Name}, ph)).To(Succeed())
		Expect(ph.Annotations).NotTo(HaveKey(BootstrapURLAnnotation))
	})

	It("Should set FailureReason=BootstrapDataUnavailable when the named Secret is missing", func() {
		By("Setting DataSecretName to a non-existent secret")
		missingName := "does-not-exist"
		machine.Spec.Bootstrap.DataSecretName = &missingName

		result, err := r.ensureBootstrapDataReady(ctx, r.Log, b7machine, machine, physicalHost)

		By("Expecting a terminal (zero requeue, no error returned) result")
		Expect(err).NotTo(HaveOccurred(),
			"terminal failures must return nil error so CAPI surfaces FailureReason/FailureMessage")
		Expect(result.IsZero()).To(BeTrue(),
			"terminal failure must not requeue")

		cond := conditions.Get(b7machine, infrastructurev1beta1.BootstrapDataReadyCondition)
		Expect(cond).NotTo(BeNil(), "BootstrapDataReadyCondition must be set")
		Expect(cond.Status).To(Equal(corev1.ConditionFalse))
		Expect(cond.Reason).To(Equal(infrastructurev1beta1.BootstrapDataUnavailableReason))

		Expect(b7machine.Status.FailureReason).NotTo(BeNil(), "FailureReason must be set")
		Expect(*b7machine.Status.FailureReason).To(Equal(infrastructurev1beta1.BootstrapDataUnavailableReason))
		Expect(b7machine.Status.FailureMessage).NotTo(BeNil(), "FailureMessage must be non-empty")
		Expect(*b7machine.Status.FailureMessage).NotTo(BeEmpty())
	})

	It("Should set BootstrapDataReadyCondition=True and annotate PhysicalHost when Secret exists", func() {
		By("Creating the bootstrap data secret")
		secretName := "real-bootstrap-secret"
		bootstrapSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secretName,
				Namespace: testNs.Name,
			},
			Data: map[string][]byte{
				"value": []byte("#cloud-config\nhostname: test-node"),
			},
		}
		Expect(k8sClient.Create(ctx, bootstrapSecret)).To(Succeed())

		By("Setting DataSecretName on the Machine")
		machine.Spec.Bootstrap.DataSecretName = &secretName

		result, err := r.ensureBootstrapDataReady(ctx, r.Log, b7machine, machine, physicalHost)

		Expect(err).NotTo(HaveOccurred())
		Expect(result.IsZero()).To(BeTrue(), "should return empty result when bootstrap data is ready")

		By("Verifying BootstrapDataReadyCondition=True")
		cond := conditions.Get(b7machine, infrastructurev1beta1.BootstrapDataReadyCondition)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(corev1.ConditionTrue))

		By("Verifying the bootstrap-url annotation was set on the PhysicalHost")
		ph := &infrastructurev1beta1.PhysicalHost{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: physicalHost.Name, Namespace: testNs.Name}, ph)).To(Succeed())
		expectedURL := fmt.Sprintf("%s/api/v1/bootstrap/%s/%s",
			bootstrapURLBase, physicalHost.Namespace, physicalHost.Name)
		Expect(ph.Annotations).To(HaveKeyWithValue(BootstrapURLAnnotation, expectedURL))
	})

	It("Should not re-annotate PhysicalHost when Status.Bootstrap.URL already matches", func() {
		By("Pre-seeding Status.Bootstrap.URL to the expected value")
		expectedURL := fmt.Sprintf("%s/api/v1/bootstrap/%s/%s",
			bootstrapURLBase, physicalHost.Namespace, physicalHost.Name)

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: physicalHost.Name, Namespace: testNs.Name}, physicalHost)).To(Succeed())
		physicalHost.Status.Bootstrap = &infrastructurev1beta1.BootstrapStatus{URL: expectedURL}
		Expect(k8sClient.Status().Update(ctx, physicalHost)).To(Succeed())

		By("Creating the bootstrap secret")
		secretName := "bootstrap-idempotent-secret"
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: testNs.Name},
			Data:       map[string][]byte{"value": []byte("data")},
		})).To(Succeed())
		machine.Spec.Bootstrap.DataSecretName = &secretName

		By("Calling ensureBootstrapDataReady")
		result, err := r.ensureBootstrapDataReady(ctx, r.Log, b7machine, machine, physicalHost)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.IsZero()).To(BeTrue())

		By("Verifying no bootstrap-url annotation was added (already up to date)")
		ph := &infrastructurev1beta1.PhysicalHost{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: physicalHost.Name, Namespace: testNs.Name}, ph)).To(Succeed())
		Expect(ph.Annotations).NotTo(HaveKey(BootstrapURLAnnotation),
			"annotation must not be re-set when Status.Bootstrap.URL already matches")
	})
})

// validateAndDefault tests — pure unit, no envtest needed.
var _ = Describe("Beskar7MachineReconciler.validateAndDefault", func() {
	It("should fail when BootstrapURLBase is empty", func() {
		r := &Beskar7MachineReconciler{
			RedfishClientFactory: func(_ context.Context, _, _, _ string, _ bool, _ []byte) (internalredfish.Client, error) {
				return internalredfish.NewMockClient(), nil
			},
		}
		Expect(r.validateAndDefault()).To(MatchError(ContainSubstring("BootstrapURLBase is empty")))
	})

	It("should succeed when BootstrapURLBase is set", func() {
		r := &Beskar7MachineReconciler{
			BootstrapURLBase: "https://example.com:8082",
			RedfishClientFactory: func(_ context.Context, _, _, _ string, _ bool, _ []byte) (internalredfish.Client, error) {
				return internalredfish.NewMockClient(), nil
			},
		}
		Expect(r.validateAndDefault()).To(Succeed())
	})
})

// bootstrapURL formatting — pure unit tests for trailing-slash correctness.
var _ = Describe("Bootstrap URL formatting", func() {
	buildURL := func(base, ns, name string) string {
		return fmt.Sprintf("%s/api/v1/bootstrap/%s/%s",
			strings.TrimRight(base, "/"), ns, name)
	}

	It("should produce the same URL with and without a trailing slash in base", func() {
		withSlash := buildURL("https://beskar7-controller-manager.beskar7-system.svc:8082/", "default", "my-host")
		withoutSlash := buildURL("https://beskar7-controller-manager.beskar7-system.svc:8082", "default", "my-host")
		Expect(withSlash).To(Equal(withoutSlash))
		Expect(withSlash).To(Equal("https://beskar7-controller-manager.beskar7-system.svc:8082/api/v1/bootstrap/default/my-host"))
	})

	It("should encode namespace and name correctly in path segments", func() {
		url := buildURL("https://mgr:8082", "my-ns", "host-01")
		Expect(url).To(Equal("https://mgr:8082/api/v1/bootstrap/my-ns/host-01"))
	})
})

// PhysicalHostToBeskar7Machine mapping — pure unit tests; no envtest required.
var _ = Describe("PhysicalHostToBeskar7Machine mapping", func() {
	var r *Beskar7MachineReconciler

	BeforeEach(func() {
		r = &Beskar7MachineReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			Log:    ctrl.Log.WithName("mapping-test"),
		}
	})

	It("Should enqueue Beskar7Machine when host has matching ConsumerRef", func() {
		host := &infrastructurev1beta1.PhysicalHost{
			Spec: infrastructurev1beta1.PhysicalHostSpec{
				ConsumerRef: &corev1.ObjectReference{
					Kind:       "Beskar7Machine",
					APIVersion: InfrastructureAPIVersion,
					Name:       "my-machine",
					Namespace:  "my-ns",
				},
			},
		}
		reqs := r.PhysicalHostToBeskar7Machine(context.Background(), host)
		Expect(reqs).To(HaveLen(1))
		Expect(reqs[0].NamespacedName).To(Equal(types.NamespacedName{Namespace: "my-ns", Name: "my-machine"}))
	})

	It("Should not enqueue when host has no ConsumerRef", func() {
		host := &infrastructurev1beta1.PhysicalHost{
			Spec: infrastructurev1beta1.PhysicalHostSpec{},
		}
		reqs := r.PhysicalHostToBeskar7Machine(context.Background(), host)
		Expect(reqs).To(BeEmpty())
	})

	It("Should not enqueue when ConsumerRef is a different Kind", func() {
		host := &infrastructurev1beta1.PhysicalHost{
			Spec: infrastructurev1beta1.PhysicalHostSpec{
				ConsumerRef: &corev1.ObjectReference{
					Kind:       "SomeOtherKind",
					APIVersion: InfrastructureAPIVersion,
					Name:       "some-object",
					Namespace:  "my-ns",
				},
			},
		}
		reqs := r.PhysicalHostToBeskar7Machine(context.Background(), host)
		Expect(reqs).To(BeEmpty())
	})

	It("Should not enqueue when ConsumerRef APIVersion does not match", func() {
		host := &infrastructurev1beta1.PhysicalHost{
			Spec: infrastructurev1beta1.PhysicalHostSpec{
				ConsumerRef: &corev1.ObjectReference{
					Kind:       "Beskar7Machine",
					APIVersion: "some.other.api/v1",
					Name:       "my-machine",
					Namespace:  "my-ns",
				},
			},
		}
		reqs := r.PhysicalHostToBeskar7Machine(context.Background(), host)
		Expect(reqs).To(BeEmpty())
	})
})

// Mint-on-inspection wiring test (PR-5.2 / D-006). Proves that triggerInspection's
// token-minting helper:
//  1. Stores the plaintext in a per-host Secret (correct labels, owner-ref).
//  2. Sets the bootstrap-token annotation with hash + lifetime (no plaintext).
//  3. Does NOT write to PhysicalHost.Status.
var _ = Describe("Beskar7Machine mint-and-store bootstrap token (PR-5.2)", func() {
	var (
		testNs       *corev1.Namespace
		physicalHost *infrastructurev1beta1.PhysicalHost
		r            *Beskar7MachineReconciler
	)

	BeforeEach(func() {
		testNs = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "mint-token-test-"},
		}
		Expect(k8sClient.Create(ctx, testNs)).To(Succeed())

		physicalHost = &infrastructurev1beta1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "mint-token-host",
				Namespace: testNs.Name,
			},
			Spec: infrastructurev1beta1.PhysicalHostSpec{
				RedfishConnection: infrastructurev1beta1.RedfishConnection{
					Address:              "https://192.168.42.1",
					CredentialsSecretRef: "irrelevant",
				},
			},
		}
		Expect(k8sClient.Create(ctx, physicalHost)).To(Succeed())

		r = &Beskar7MachineReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			Log:    ctrl.Log.WithName("mint-token-test"),
			RedfishClientFactory: func(_ context.Context, _, _, _ string, _ bool, _ []byte) (internalredfish.Client, error) {
				return internalredfish.NewMockClient(), nil
			},
			BootstrapURLBase: "https://test.svc:8082",
		}
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, testNs)).To(Succeed())
	})

	It("creates a per-host Secret with plaintext-token + sets the bootstrap-token annotation", func() {
		By("Calling mintAndStoreBootstrapToken directly")
		statusBefore := physicalHost.Status.DeepCopy()
		Expect(r.mintAndStoreBootstrapToken(ctx, r.Log, physicalHost)).To(Succeed())

		By("Verifying the per-host Secret was created with plaintext-token data and PhysicalHost owner reference")
		secretName := bootstrapTokenSecretName(physicalHost.Name)
		Eventually(func(g Gomega) {
			s := &corev1.Secret{}
			g.Expect(k8sClient.Get(ctx,
				types.NamespacedName{Namespace: physicalHost.Namespace, Name: secretName}, s)).To(Succeed())
			g.Expect(s.Type).To(Equal(corev1.SecretTypeOpaque))
			g.Expect(s.Data).To(HaveKey("plaintext-token"))
			g.Expect(s.Data["plaintext-token"]).NotTo(BeEmpty(),
				"plaintext-token must be non-empty")
			// 32 random bytes → base64.RawURLEncoding → 43 chars.
			g.Expect(len(s.Data["plaintext-token"])).To(Equal(43),
				"plaintext token length must match auth.MintToken's contract (43 chars)")
			// Owner ref so Secret is GC'd on host deletion.
			g.Expect(s.OwnerReferences).NotTo(BeEmpty())
			g.Expect(s.OwnerReferences[0].Kind).To(Equal("PhysicalHost"))
			g.Expect(s.OwnerReferences[0].Name).To(Equal(physicalHost.Name))
			// Labels for operator visibility + cleanup.
			g.Expect(s.Labels).To(HaveKeyWithValue(inspectionResultLabelOwnedBy, "beskar7-controller-manager"))
			g.Expect(s.Labels).To(HaveKeyWithValue(inspectionResultLabelHost, physicalHost.Name))
		}, time.Second*5, time.Millisecond*100).Should(Succeed())

		By("Verifying the bootstrap-token annotation was set with a JSON-encoded {hash, issuedAt, expiresAt}")
		ph := &infrastructurev1beta1.PhysicalHost{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: physicalHost.Namespace, Name: physicalHost.Name}, ph)).To(Succeed())
		Expect(ph.Annotations).To(HaveKey(BootstrapTokenAnnotation))

		// Decode and validate the annotation.
		raw := ph.Annotations[BootstrapTokenAnnotation]
		Expect(raw).NotTo(BeEmpty())
		var value BootstrapTokenAnnotationValue
		Expect(json.Unmarshal([]byte(raw), &value)).To(Succeed())
		Expect(value.Hash).To(HaveLen(64), "hash must be hex-encoded sha256 (64 chars)")
		Expect(value.ExpiresAt.Time.After(value.IssuedAt.Time)).To(BeTrue(),
			"expiresAt must be after issuedAt")
		Expect(value.ExpiresAt.Time.Sub(value.IssuedAt.Time)).To(BeNumerically("~", 30*time.Minute, time.Second),
			"lifetime must match auth.TokenLifetime (30 min)")

		// Annotation must NOT contain the plaintext.
		Expect(strings.Contains(raw, string(getSecretPlaintext(ctx, physicalHost.Namespace, secretName)))).To(BeFalse(),
			"bootstrap-token annotation must not echo the plaintext")

		By("Verifying PhysicalHost.Status was not written by the mint helper (BUG-1 invariant)")
		Expect(ph.Status.Bootstrap).To(Equal(statusBefore.Bootstrap),
			"mint helper must not write to PhysicalHost.Status — that is the PhysicalHost reconciler's job via the annotation")
	})
})

// getSecretPlaintext reads the per-host bootstrap-token Secret and returns its
// plaintext-token value. Used only by the mint-and-store test to verify that
// the value is not echoed elsewhere; not used in production code.
func getSecretPlaintext(ctx context.Context, ns, name string) []byte {
	s := &corev1.Secret{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, s)).To(Succeed())
	return s.Data["plaintext-token"]
}

// bootstrapTokenStillValid no-re-mint guard tests (PR-5.3). Pure unit tests:
// no envtest needed because the helper does not perform I/O.
var _ = Describe("bootstrapTokenStillValid (no-re-mint guard)", func() {
	now := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)

	It("returns false when host is nil", func() {
		Expect(bootstrapTokenStillValid(nil, now)).To(BeFalse())
	})

	It("returns false when Status.Bootstrap is nil", func() {
		ph := &infrastructurev1beta1.PhysicalHost{}
		Expect(bootstrapTokenStillValid(ph, now)).To(BeFalse())
	})

	It("returns false when TokenHash is empty", func() {
		exp := metav1.NewTime(now.Add(10 * time.Minute))
		ph := &infrastructurev1beta1.PhysicalHost{
			Status: infrastructurev1beta1.PhysicalHostStatus{
				Bootstrap: &infrastructurev1beta1.BootstrapStatus{
					TokenHash: "",
					ExpiresAt: &exp,
				},
			},
		}
		Expect(bootstrapTokenStillValid(ph, now)).To(BeFalse())
	})

	It("returns false when ExpiresAt is nil", func() {
		ph := &infrastructurev1beta1.PhysicalHost{
			Status: infrastructurev1beta1.PhysicalHostStatus{
				Bootstrap: &infrastructurev1beta1.BootstrapStatus{
					TokenHash: "deadbeef",
					ExpiresAt: nil,
				},
			},
		}
		Expect(bootstrapTokenStillValid(ph, now)).To(BeFalse())
	})

	It("returns false when ExpiresAt is in the past", func() {
		exp := metav1.NewTime(now.Add(-1 * time.Minute))
		ph := &infrastructurev1beta1.PhysicalHost{
			Status: infrastructurev1beta1.PhysicalHostStatus{
				Bootstrap: &infrastructurev1beta1.BootstrapStatus{
					TokenHash: "deadbeef",
					ExpiresAt: &exp,
				},
			},
		}
		Expect(bootstrapTokenStillValid(ph, now)).To(BeFalse())
	})

	It("returns false when ExpiresAt equals now (boundary: must re-mint)", func() {
		exp := metav1.NewTime(now)
		ph := &infrastructurev1beta1.PhysicalHost{
			Status: infrastructurev1beta1.PhysicalHostStatus{
				Bootstrap: &infrastructurev1beta1.BootstrapStatus{
					TokenHash: "deadbeef",
					ExpiresAt: &exp,
				},
			},
		}
		Expect(bootstrapTokenStillValid(ph, now)).To(BeFalse(),
			"now.Before(ExpiresAt) is false at equality — boundary must re-mint")
	})

	It("returns true when token is still within the validity window", func() {
		exp := metav1.NewTime(now.Add(10 * time.Minute))
		ph := &infrastructurev1beta1.PhysicalHost{
			Status: infrastructurev1beta1.PhysicalHostStatus{
				Bootstrap: &infrastructurev1beta1.BootstrapStatus{
					TokenHash: "deadbeef",
					ExpiresAt: &exp,
				},
			},
		}
		Expect(bootstrapTokenStillValid(ph, now)).To(BeTrue())
	})

	// Regression test for the mint race: when a previous Beskar7Machine
	// reconcile has set the BootstrapTokenAnnotation but the PhysicalHost
	// controller has not yet promoted it to Status, the second reconcile
	// must NOT re-mint. Otherwise the Secret gets overwritten with a new
	// plaintext whose hash doesn't match the one Status will eventually
	// carry, and every inspector bearer-auth attempt 401s. See the layer
	// 5 hardening notes in this PR's body.
	It("returns true when a pending annotation has a non-expired hash (mint-race guard)", func() {
		exp := metav1.NewTime(now.Add(10 * time.Minute))
		annoBytes, _ := json.Marshal(BootstrapTokenAnnotationValue{
			Hash:      "deadbeef",
			IssuedAt:  metav1.NewTime(now.Add(-30 * time.Second)),
			ExpiresAt: exp,
		})
		ph := &infrastructurev1beta1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{BootstrapTokenAnnotation: string(annoBytes)},
			},
			// Status.Bootstrap is intentionally empty — simulates the
			// window where the PhysicalHost reconciler has not yet
			// consumed the annotation.
		}
		Expect(bootstrapTokenStillValid(ph, now)).To(BeTrue(),
			"pending annotation must count as a valid in-flight token")
	})

	It("returns false when pending annotation is expired (mint a fresh one)", func() {
		exp := metav1.NewTime(now.Add(-1 * time.Minute))
		annoBytes, _ := json.Marshal(BootstrapTokenAnnotationValue{
			Hash:      "deadbeef",
			IssuedAt:  metav1.NewTime(now.Add(-31 * time.Minute)),
			ExpiresAt: exp,
		})
		ph := &infrastructurev1beta1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{BootstrapTokenAnnotation: string(annoBytes)},
			},
		}
		Expect(bootstrapTokenStillValid(ph, now)).To(BeFalse(),
			"expired pending annotation must NOT block a fresh mint")
	})
})
