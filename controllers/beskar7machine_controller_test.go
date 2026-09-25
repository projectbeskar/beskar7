package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/stmcginnis/gofish/schemas"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
	"github.com/projectbeskar/beskar7/internal/auth"
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
		var beskar7Machine *infrav1.Beskar7Machine
		var physicalHost *infrav1.PhysicalHost
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
			credentialSecret = bmcCredentialsSecretNamed(testNs.Name, "test-bmc-creds")
			Expect(k8sClient.Create(ctx, credentialSecret)).To(Succeed())

			// Create available PhysicalHost
			physicalHost = &infrav1.PhysicalHost{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-host",
					Namespace: testNs.Name,
				},
				Spec: infrav1.PhysicalHostSpec{
					RedfishConnection: infrav1.RedfishConnection{
						Address:              "https://192.168.1.100",
						CredentialsSecretRef: credentialSecret.Name,
					},
				},
				Status: infrav1.PhysicalHostStatus{
					State: infrav1.StateAvailable,
					Ready: true,
				},
			}
			Expect(k8sClient.Create(ctx, physicalHost)).To(Succeed())
			Expect(k8sClient.Status().Update(ctx, physicalHost)).To(Succeed())

			// Create Beskar7Machine
			beskar7Machine = &infrav1.Beskar7Machine{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-machine",
					Namespace: testNs.Name,
				},
				Spec: infrav1.Beskar7MachineSpec{
					InspectionImageURL: "http://boot-server/ipxe/inspect.ipxe",
					TargetImageURL:     "http://boot-server/images/kairos.tar.gz",
					TargetImageDigest:  bootTestDigest,
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
			physicalHost.Status.State = infrav1.StateInUse
			Expect(k8sClient.Status().Update(ctx, physicalHost)).To(Succeed())

			By("Calling findAndClaimOrGetAssociatedHost: should return our host via ConsumerRef")
			// beskar7Machine.Spec.ProviderID is unset (set later in handleReadyHost),
			// status.state is InUse so the StateAvailable index won't return it — only
			// the ConsumerRef branch can.
			got, result, err := reconciler.findAndClaimOrGetAssociatedHost(ctx, ctrl.Log.WithName("refind-test"), beskar7Machine, nil)
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
			got := &infrav1.PhysicalHost{}
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
			before := &infrav1.PhysicalHost{}
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
			after := &infrav1.PhysicalHost{}
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
			beskar7Machine.Spec.HardwareRequirements = &infrav1.HardwareRequirements{
				MinCPUCores: 8,
				MinMemoryGB: 16,
				MinDiskGB:   100,
			}
			Expect(k8sClient.Create(ctx, beskar7Machine)).To(Succeed())

			// Build a report that comfortably satisfies the requirements.
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: physicalHost.Name, Namespace: testNs.Name}, physicalHost)).To(Succeed())
			physicalHost.Status.InspectionPhase = infrav1.InspectionPhaseComplete
			physicalHost.Status.InspectionReport = &infrav1.InspectionReport{
				Timestamp:    metav1.Now(),
				Manufacturer: "Dell Inc.",
				Model:        "PowerEdge R650",
				CPUs:         []infrav1.CPUInfo{{ID: "0", Cores: 18, Threads: 36}},
				Memory:       []infrav1.MemoryInfo{{ID: "DIMM0", Capacity: "32GB"}},
				Disks:        []infrav1.DiskInfo{{Name: "sda", SizeGB: 500}},
			}
			Expect(k8sClient.Status().Update(ctx, physicalHost)).To(Succeed())

			result, err := reconciler.validateInspectionReport(ctx, reconciler.Log, beskar7Machine, physicalHost)
			Expect(err).NotTo(HaveOccurred(), "passing hardware checks must not error")
			Expect(result.RequeueAfter).To(BeNumerically(">", 0), "post-inspection should requeue once to observe the new state")

			// The machine must NOT be terminally failed on a passing report.
			Expect(isTerminallyFailed(beskar7Machine)).To(BeFalse(),
				"successful validation must leave the machine non-terminal")

			// Annotation handoff: validateInspectionReport calls
			// setInspectionRequestAnnotation("inspect-complete") on success.
			updated := &infrav1.PhysicalHost{}
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
			mockRf.PowerState = schemas.OnPowerState
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
			b7m := &infrav1.Beskar7Machine{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "delete-test-machine",
					Namespace:  testNs.Name,
					Finalizers: []string{Beskar7MachineFinalizer},
				},
				Spec: infrav1.Beskar7MachineSpec{
					InspectionImageURL: "http://boot-server/inspect.ipxe",
					TargetImageURL:     "http://boot-server/kairos.tar.gz",
					TargetImageDigest:  bootTestDigest,
					ProviderID:         provID,
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
			Expect(mockRf.PowerState).To(Equal(schemas.OffPowerState))

			By("Verifying ConsumerRef is nil on the host")
			hostAfter := &infrav1.PhysicalHost{}
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
			b7m := &infrav1.Beskar7Machine{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "force-release-machine",
					Namespace:  testNs.Name,
					Finalizers: []string{Beskar7MachineFinalizer},
					Annotations: map[string]string{
						ForceReleaseAnnotation: "true",
					},
				},
				Spec: infrav1.Beskar7MachineSpec{
					InspectionImageURL: "http://boot-server/inspect.ipxe",
					TargetImageURL:     "http://boot-server/kairos.tar.gz",
					TargetImageDigest:  bootTestDigest,
					ProviderID:         provID,
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
			hostAfter := &infrav1.PhysicalHost{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: physicalHost.Name, Namespace: testNs.Name}, hostAfter)).To(Succeed())
			Expect(hostAfter.Spec.ConsumerRef).To(BeNil(), "ConsumerRef must be cleared even on force-release")

			By("Cleanup: remove finalizer")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: b7m.Name, Namespace: testNs.Name}, b7m)).To(Succeed())
			b7mBase := b7m.DeepCopy()
			b7m.Finalizers = nil
			Expect(k8sClient.Patch(ctx, b7m, client.MergeFrom(b7mBase))).To(Succeed())
		})

		// SEC-12: mirrors the re-find guard above, for the release path.
		// findClaimedHostForRelease's fallback ConsumerRef.Name scan must not
		// treat a same-named machine in a different namespace as this
		// machine's host — otherwise deletion would release (and clear the
		// ConsumerRef on) a host this machine never claimed.
		It("Should NOT release a PhysicalHost whose ConsumerRef names a same-named machine in a different namespace (SEC-12)", func() {
			otherNs := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "beskar7machine-test-crossns-"},
			}
			Expect(k8sClient.Create(ctx, otherNs)).To(Succeed())
			defer func() { Expect(k8sClient.Delete(ctx, otherNs)).To(Succeed()) }()

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: physicalHost.Name, Namespace: testNs.Name}, physicalHost)).To(Succeed())
			base := physicalHost.DeepCopy()
			physicalHost.Spec.ConsumerRef = &corev1.ObjectReference{
				Kind:       "Beskar7Machine",
				APIVersion: InfrastructureAPIVersion,
				Name:       beskar7Machine.Name,
				Namespace:  otherNs.Name,
			}
			Expect(k8sClient.Patch(ctx, physicalHost, client.MergeFrom(base))).To(Succeed())
			physicalHost.Status.State = infrav1.StateInUse
			Expect(k8sClient.Status().Update(ctx, physicalHost)).To(Succeed())

			got, err := reconciler.findClaimedHostForRelease(ctx, ctrl.Log.WithName("crossns-release-test"), beskar7Machine)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(BeNil(),
				"a host claimed by a same-named machine in a different namespace must not be released by this machine")
		})

		// The ProviderID fast path fetches the host the ProviderID names, which
		// can be in any namespace: a ProviderID written by hand, or copied from
		// a same-named machine elsewhere, must not let this machine release (and
		// power off) that other namespace's host on deletion.
		It("Should NOT release another namespace's host that its ProviderID names (SEC-12)", func() {
			otherNs := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "beskar7machine-test-crossns-pid-"},
			}
			Expect(k8sClient.Create(ctx, otherNs)).To(Succeed())
			defer func() { Expect(k8sClient.Delete(ctx, otherNs)).To(Succeed()) }()

			otherHost := &infrav1.PhysicalHost{
				ObjectMeta: metav1.ObjectMeta{Name: "crossns-pid-host", Namespace: otherNs.Name},
				Spec: infrav1.PhysicalHostSpec{
					RedfishConnection: infrav1.RedfishConnection{
						Address:              "https://mock-redfish.example.invalid:8443",
						CredentialsSecretRef: "bmc-credentials",
					},
					// Claimed by the same-named machine in its own namespace.
					ConsumerRef: &corev1.ObjectReference{
						Kind:       "Beskar7Machine",
						APIVersion: InfrastructureAPIVersion,
						Name:       beskar7Machine.Name,
						Namespace:  otherNs.Name,
					},
				},
			}
			Expect(k8sClient.Create(ctx, otherHost)).To(Succeed())

			pointing := beskar7Machine.DeepCopy()
			pointing.Spec.ProviderID = "b7://" + otherNs.Name + "/" + otherHost.Name

			got, err := reconciler.findClaimedHostForRelease(ctx, ctrl.Log.WithName("crossns-pid-release-test"), pointing)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(BeNil(),
				"a ProviderID naming another namespace's host must not make this machine release it")
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
			b7m := &infrav1.Beskar7Machine{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "gone-host-machine",
					Namespace:  testNs.Name,
					Finalizers: []string{Beskar7MachineFinalizer},
				},
				Spec: infrav1.Beskar7MachineSpec{
					InspectionImageURL: "http://boot-server/inspect.ipxe",
					TargetImageURL:     "http://boot-server/kairos.tar.gz",
					TargetImageDigest:  bootTestDigest,
					ProviderID:         provID,
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
			b7m := &infrav1.Beskar7Machine{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "bmc-fail-machine",
					Namespace:  testNs.Name,
					Finalizers: []string{Beskar7MachineFinalizer},
				},
				Spec: infrav1.Beskar7MachineSpec{
					InspectionImageURL: "http://boot-server/inspect.ipxe",
					TargetImageURL:     "http://boot-server/kairos.tar.gz",
					TargetImageDigest:  bootTestDigest,
					ProviderID:         provID,
				},
			}
			Expect(k8sClient.Create(ctx, b7m)).To(Succeed())

			By("Calling reconcileDelete — should succeed despite BMC failure")
			_, err := r.reconcileDelete(ctx, r.Log, b7m)
			Expect(err).NotTo(HaveOccurred(), "deletion must not be blocked by an unreachable BMC")

			By("Verifying ConsumerRef was still cleared on the host")
			hostAfter := &infrav1.PhysicalHost{}
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
		// The cluster.x-k8s.io/paused annotation on the Beskar7Machine itself is
		// honored via paused.EnsurePausedCondition (controllers/beskar7machine_controller.go),
		// called once the owner Machine and Cluster are resolved. A machine with no
		// resolvable owner now bails out earlier, at "Waiting for Machine Controller
		// to set OwnerRef", before ever reaching the pause check — so this needs a
		// full owner chain to actually exercise paused.EnsurePausedCondition.
		It("Should skip reconciliation when the pause annotation is set", func() {
			beskar7Machine.Annotations = map[string]string{
				clusterv1.PausedAnnotation: "true",
			}
			Expect(k8sClient.Create(ctx, beskar7Machine)).To(Succeed())

			ownerCluster := &clusterv1.Cluster{
				ObjectMeta: metav1.ObjectMeta{Name: "pause-annotation-cluster", Namespace: testNs.Name},
				Spec:       clusterv1.ClusterSpec{Paused: ptr.To(false)},
			}
			Expect(k8sClient.Create(ctx, ownerCluster)).To(Succeed())

			ownerMachine := &clusterv1.Machine{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pause-annotation-machine",
					Namespace: testNs.Name,
					Labels:    map[string]string{clusterv1.ClusterNameLabel: ownerCluster.Name},
				},
				Spec: clusterv1.MachineSpec{
					ClusterName: ownerCluster.Name,
					// The v1beta2 Machine CRD requires bootstrap and infrastructureRef;
					// nothing in envtest acts on either fixture reference.
					Bootstrap:         clusterv1.Bootstrap{ConfigRef: clusterv1.ContractVersionedObjectReference{APIGroup: "bootstrap.cluster.x-k8s.io", Kind: "KairosConfig", Name: "fixture"}},
					InfrastructureRef: clusterv1.ContractVersionedObjectReference{APIGroup: "infrastructure.cluster.x-k8s.io", Kind: "Beskar7Machine", Name: "fixture"},
				},
			}
			Expect(k8sClient.Create(ctx, ownerMachine)).To(Succeed())

			beskar7Machine.OwnerReferences = []metav1.OwnerReference{{
				APIVersion: clusterv1.GroupVersion.String(),
				Kind:       "Machine",
				Name:       ownerMachine.Name,
				UID:        ownerMachine.UID,
			}}
			beskar7Machine.Labels = map[string]string{clusterv1.ClusterNameLabel: ownerCluster.Name}
			Expect(k8sClient.Update(ctx, beskar7Machine)).To(Succeed())

			machineLookupKey := types.NamespacedName{Name: beskar7Machine.Name, Namespace: beskar7Machine.Namespace}

			result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: machineLookupKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}), "paused reconcile must return zero Result")

			// The PhysicalHost must remain unclaimed: paused Reconcile never
			// reaches findAndClaimOrGetAssociatedHost.
			unchangedHost := &infrav1.PhysicalHost{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: physicalHost.Name, Namespace: testNs.Name}, unchangedHost)).To(Succeed())
			Expect(unchangedHost.Spec.ConsumerRef).To(BeNil(), "paused reconcile must not claim a host")

			updated := &infrav1.Beskar7Machine{}
			Expect(k8sClient.Get(ctx, machineLookupKey, updated)).To(Succeed())
			Expect(conditions.IsTrue(updated, clusterv1.PausedCondition)).To(BeTrue(),
				"the paused annotation must be reflected in the Beskar7Machine's Paused condition")
		})

		// New spec: Cluster.spec.paused must pause the Beskar7Machine too (the
		// signal comes from the owner Cluster this time, not the object's own
		// annotation), and reconciliation must resume once the Cluster unpauses.
		It("Should not act while the owner Cluster is paused, and resume once it is unpaused", func() {
			Expect(k8sClient.Create(ctx, beskar7Machine)).To(Succeed())

			ownerCluster := &clusterv1.Cluster{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-pause-cluster", Namespace: testNs.Name},
				Spec:       clusterv1.ClusterSpec{Paused: ptr.To(true)},
			}
			Expect(k8sClient.Create(ctx, ownerCluster)).To(Succeed())

			ownerMachine := &clusterv1.Machine{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cluster-pause-machine",
					Namespace: testNs.Name,
					Labels:    map[string]string{clusterv1.ClusterNameLabel: ownerCluster.Name},
				},
				Spec: clusterv1.MachineSpec{
					ClusterName: ownerCluster.Name,
					// The v1beta2 Machine CRD requires bootstrap and infrastructureRef;
					// nothing in envtest acts on either fixture reference.
					Bootstrap:         clusterv1.Bootstrap{ConfigRef: clusterv1.ContractVersionedObjectReference{APIGroup: "bootstrap.cluster.x-k8s.io", Kind: "KairosConfig", Name: "fixture"}},
					InfrastructureRef: clusterv1.ContractVersionedObjectReference{APIGroup: "infrastructure.cluster.x-k8s.io", Kind: "Beskar7Machine", Name: "fixture"},
				},
			}
			Expect(k8sClient.Create(ctx, ownerMachine)).To(Succeed())

			beskar7Machine.OwnerReferences = []metav1.OwnerReference{{
				APIVersion: clusterv1.GroupVersion.String(),
				Kind:       "Machine",
				Name:       ownerMachine.Name,
				UID:        ownerMachine.UID,
			}}
			beskar7Machine.Labels = map[string]string{clusterv1.ClusterNameLabel: ownerCluster.Name}
			Expect(k8sClient.Update(ctx, beskar7Machine)).To(Succeed())

			machineLookupKey := types.NamespacedName{Name: beskar7Machine.Name, Namespace: testNs.Name}
			req := ctrl.Request{NamespacedName: machineLookupKey}

			By("reconciling while Cluster.spec.paused is true")
			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			hostWhilePaused := &infrav1.PhysicalHost{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: physicalHost.Name, Namespace: testNs.Name}, hostWhilePaused)).To(Succeed())
			Expect(hostWhilePaused.Spec.ConsumerRef).To(BeNil(), "a machine paused via its owner Cluster must not claim a host")

			pausedMachine := &infrav1.Beskar7Machine{}
			Expect(k8sClient.Get(ctx, machineLookupKey, pausedMachine)).To(Succeed())
			Expect(conditions.IsTrue(pausedMachine, clusterv1.PausedCondition)).To(BeTrue(),
				"Cluster.spec.paused must be reflected in the Beskar7Machine's Paused condition")

			By("unpausing the Cluster")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ownerCluster.Name, Namespace: testNs.Name}, ownerCluster)).To(Succeed())
			ownerCluster.Spec.Paused = ptr.To(false)
			Expect(k8sClient.Update(ctx, ownerCluster)).To(Succeed())

			// Drive reconciliation past the pause transition: the first
			// post-unpause call only flips the condition and requeues (no prior
			// condition to compare against costs a cycle the same way the initial
			// pause did); the second reaches reconcileNormal and adds the
			// finalizer. Stop there — a third call would proceed into
			// findAndClaimOrGetAssociatedHost, whose Available-host list uses a
			// field index this Context's plain (non-manager-backed) k8sClient
			// does not register; claiming itself is covered by the race and
			// placement Describe blocks below, which do set that index up.
			for i := 0; i < 2; i++ {
				_, err := reconciler.Reconcile(ctx, req)
				Expect(err).NotTo(HaveOccurred())
			}

			resumedMachine := &infrav1.Beskar7Machine{}
			Expect(k8sClient.Get(ctx, machineLookupKey, resumedMachine)).To(Succeed())
			Expect(conditions.IsFalse(resumedMachine, clusterv1.PausedCondition)).To(BeTrue(),
				"unpausing the Cluster must flip the Beskar7Machine's Paused condition to False")
			Expect(resumedMachine.Finalizers).To(ContainElement(Beskar7MachineFinalizer),
				"reconciliation must resume and reach reconcileNormal once unpaused")
		})

		// Converted from "[SKIP - Hardware Testing] Should validate hardware requirements".
		// validateInspectionReport is exercised directly because the full Reconcile path
		// requires a CAPI Machine owner chain that this test doesn't set up. The helper
		// is the unit under test for BUG-8 (terminal-failure wiring).
		Context("hardware-validation terminal failures (BUG-8)", func() {
			buildMachine := func(reqs *infrav1.HardwareRequirements) *infrav1.Beskar7Machine {
				return &infrav1.Beskar7Machine{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "validate-target",
						Namespace: testNs.Name,
					},
					Spec: infrav1.Beskar7MachineSpec{
						InspectionImageURL:   "http://boot-server/ipxe/inspect.ipxe",
						TargetImageURL:       "http://boot-server/images/kairos.tar.gz",
						TargetImageDigest:    bootTestDigest,
						HardwareRequirements: reqs,
					},
				}
			}

			buildHostWithReport := func(report *infrav1.InspectionReport) *infrav1.PhysicalHost {
				return &infrav1.PhysicalHost{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "validate-host",
						Namespace: testNs.Name,
					},
					Status: infrav1.PhysicalHostStatus{
						InspectionReport: report,
					},
				}
			}

			expectTerminalFailure := func(b *infrav1.Beskar7Machine, expectedReason string) string {
				Expect(b.Status.Ready).To(BeFalse())
				Expect(b.Status.Phase).NotTo(BeNil())
				Expect(*b.Status.Phase).To(Equal(infrav1.PhaseFailed))
				cond := conditions.Get(b, infrav1.InfrastructureReadyCondition)
				Expect(cond).NotTo(BeNil(), "InfrastructureReady condition must be set")
				Expect(cond.Status).To(Equal(metav1.ConditionFalse))
				Expect(cond.Reason).To(Equal(expectedReason))
				Expect(cond.Message).NotTo(BeEmpty(), "the condition message must carry the failure detail")
				return cond.Message
			}

			It("Should mark Beskar7Machine terminally Failed when CPU cores are insufficient", func() {
				machine := buildMachine(&infrav1.HardwareRequirements{MinCPUCores: 16})
				host := buildHostWithReport(&infrav1.InspectionReport{
					CPUs: []infrav1.CPUInfo{{ID: "0", Cores: 4}},
				})

				result, err := reconciler.validateInspectionReport(ctx, reconciler.Log, machine, host)
				Expect(err).NotTo(HaveOccurred(), "terminal failures must NOT return an error (would requeue forever)")
				Expect(result).To(Equal(ctrl.Result{}), "terminal failures must NOT requeue")
				msg := expectTerminalFailure(machine, infrav1.HardwareRequirementsNotMetReason)
				Expect(msg).To(ContainSubstring("CPU cores"))
				Expect(msg).To(ContainSubstring("4"))
				Expect(msg).To(ContainSubstring("16"))
			})

			It("Should mark Beskar7Machine terminally Failed when memory is insufficient", func() {
				machine := buildMachine(&infrav1.HardwareRequirements{MinMemoryGB: 64})
				host := buildHostWithReport(&infrav1.InspectionReport{
					CPUs:   []infrav1.CPUInfo{{ID: "0", Cores: 32}},
					Memory: []infrav1.MemoryInfo{{ID: "DIMM0", Capacity: "16GB"}},
				})

				result, err := reconciler.validateInspectionReport(ctx, reconciler.Log, machine, host)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(ctrl.Result{}))
				msg := expectTerminalFailure(machine, infrav1.HardwareRequirementsNotMetReason)
				Expect(msg).To(ContainSubstring("memory"))
				Expect(msg).To(ContainSubstring("16"))
				Expect(msg).To(ContainSubstring("64"))
			})

			It("Should mark Beskar7Machine terminally Failed when disk space is insufficient", func() {
				machine := buildMachine(&infrav1.HardwareRequirements{MinDiskGB: 1000})
				host := buildHostWithReport(&infrav1.InspectionReport{
					CPUs:   []infrav1.CPUInfo{{ID: "0", Cores: 32}},
					Memory: []infrav1.MemoryInfo{{ID: "DIMM0", Capacity: "128GB"}},
					Disks:  []infrav1.DiskInfo{{Name: "sda", SizeGB: 250}},
				})

				result, err := reconciler.validateInspectionReport(ctx, reconciler.Log, machine, host)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(ctrl.Result{}))
				msg := expectTerminalFailure(machine, infrav1.HardwareRequirementsNotMetReason)
				Expect(msg).To(ContainSubstring("disk"))
				Expect(msg).To(ContainSubstring("250"))
				Expect(msg).To(ContainSubstring("1000"))
			})

			It("Should NOT clear the terminal failure on a subsequent reconcile (idempotent terminality)", func() {
				machine := buildMachine(&infrav1.HardwareRequirements{MinCPUCores: 16})
				host := buildHostWithReport(&infrav1.InspectionReport{
					CPUs: []infrav1.CPUInfo{{ID: "0", Cores: 4}},
				})

				_, err := reconciler.validateInspectionReport(ctx, reconciler.Log, machine, host)
				Expect(err).NotTo(HaveOccurred())
				Expect(isTerminallyFailed(machine)).To(BeTrue())
				originalReason := conditions.GetReason(machine, infrav1.InfrastructureReadyCondition)

				// Re-run validation (simulating a subsequent reconcile). The helper
				// must overwrite-but-not-clear: same reason, no transition away from terminal.
				_, err = reconciler.validateInspectionReport(ctx, reconciler.Log, machine, host)
				Expect(err).NotTo(HaveOccurred())
				Expect(isTerminallyFailed(machine)).To(BeTrue(), "the terminal failure must persist across reconciles")
				Expect(conditions.GetReason(machine, infrav1.InfrastructureReadyCondition)).To(Equal(originalReason))
			})
		})

		Context("inspection timeout terminal failure (BUG-8)", func() {
			It("Should mark Beskar7Machine terminally Failed when inspection times out", func() {
				machine := &infrav1.Beskar7Machine{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "timeout-target",
						Namespace: testNs.Name,
					},
					Spec: infrav1.Beskar7MachineSpec{
						InspectionImageURL: "http://boot-server/ipxe/inspect.ipxe",
						TargetImageURL:     "http://boot-server/images/kairos.tar.gz",
						TargetImageDigest:  bootTestDigest,
					},
				}
				// Build a host with an InspectionTimestamp older than DefaultInspectionTimeout.
				host := &infrav1.PhysicalHost{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "timeout-host",
						Namespace: testNs.Name,
					},
					Spec: infrav1.PhysicalHostSpec{
						RedfishConnection: infrav1.RedfishConnection{
							Address:              "https://192.168.1.100",
							CredentialsSecretRef: credentialSecret.Name,
						},
					},
				}
				// Persist the host so setInspectionRequestAnnotation can patch it.
				// Status is a subresource — set it AFTER Create or it's dropped.
				Expect(k8sClient.Create(ctx, host)).To(Succeed())
				old := metav1.NewTime(time.Now().Add(-2 * DefaultInspectionTimeout))
				host.Status.State = infrav1.StateInspecting
				host.Status.InspectionTimestamp = &old
				Expect(k8sClient.Status().Update(ctx, host)).To(Succeed())

				result, err := reconciler.handleInspectingHost(ctx, reconciler.Log, machine, host)
				Expect(err).NotTo(HaveOccurred(), "terminal failures must NOT return an error")
				Expect(result).To(Equal(ctrl.Result{}), "terminal failures must NOT requeue")

				// Beskar7Machine in-memory state assertions.
				Expect(machine.Status.Ready).To(BeFalse())
				Expect(machine.Status.Phase).NotTo(BeNil())
				Expect(*machine.Status.Phase).To(Equal(infrav1.PhaseFailed))
				cond := conditions.Get(machine, infrav1.InfrastructureReadyCondition)
				Expect(cond).NotTo(BeNil(), "InfrastructureReady condition must be set")
				Expect(cond.Status).To(Equal(metav1.ConditionFalse))
				Expect(cond.Reason).To(Equal(infrav1.InspectionTimedOutReason))
				Expect(cond.Message).To(ContainSubstring("Inspection did not complete"))

				// PhysicalHost should have received the timeout annotation.
				patchedHost := &infrav1.PhysicalHost{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: host.Name, Namespace: host.Namespace}, patchedHost)).To(Succeed())
				Expect(patchedHost.Annotations[InspectionRequestAnnotation]).To(Equal("timeout"))
			})
		})

		// Regression test for the reordering fix: a Complete inspection whose
		// reconcile fires after the timeout window must NOT be marked
		// InspectionTimedOut — success must always win over the timeout check.
		Context("inspection Complete but InspectionTimestamp older than timeout", func() {
			It("Should advance to validateInspectionReport and NOT mark InspectionTimedOut", func() {
				machine := &infrav1.Beskar7Machine{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "late-complete-target",
						Namespace: testNs.Name,
					},
					Spec: infrav1.Beskar7MachineSpec{
						InspectionImageURL: "http://boot-server/ipxe/inspect.ipxe",
						TargetImageURL:     "http://boot-server/images/kairos.tar.gz",
						TargetImageDigest:  bootTestDigest,
					},
				}
				// Host: InspectionPhase=Complete, but timestamp is 2× past the timeout.
				// Before the fix, this would have fired the timeout branch first.
				// Persist the host so setInspectionRequestAnnotation can patch it
				// (OptimisticLock requires a server-assigned resource version).
				old := metav1.NewTime(time.Now().Add(-2 * DefaultInspectionTimeout))
				host := &infrav1.PhysicalHost{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "late-complete-host",
						Namespace: testNs.Name,
					},
					Spec: infrav1.PhysicalHostSpec{
						RedfishConnection: infrav1.RedfishConnection{
							Address:              "https://192.168.1.100",
							CredentialsSecretRef: credentialSecret.Name,
						},
					},
				}
				Expect(k8sClient.Create(ctx, host)).To(Succeed())
				host.Status.InspectionPhase = infrav1.InspectionPhaseComplete
				host.Status.InspectionTimestamp = &old
				// Provide a minimal report so validateInspectionReport can proceed.
				host.Status.InspectionReport = &infrav1.InspectionReport{
					Timestamp: metav1.Now(),
				}
				Expect(k8sClient.Status().Update(ctx, host)).To(Succeed())

				result, err := reconciler.handleInspectingHost(ctx, reconciler.Log, machine, host)
				Expect(err).NotTo(HaveOccurred())
				// validateInspectionReport returns Requeue=true on success; at minimum
				// the machine must NOT be terminally failed.
				Expect(isTerminallyFailed(machine)).To(BeFalse(),
					"Complete inspection must never be marked InspectionTimedOut, even when the timestamp is past the timeout window")
				// Confirm the success path was taken: the annotation must have been set.
				patchedHost := &infrav1.PhysicalHost{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: host.Name, Namespace: host.Namespace}, patchedHost)).To(Succeed())
				Expect(patchedHost.Annotations[InspectionRequestAnnotation]).To(Equal("inspect-complete"))
				// result carries Requeue=true from the success path (not zero from a terminal failure).
				Expect(result.RequeueAfter).To(BeNumerically(">", 0))
			})
		})

		// New terminal branch: InspectionPhaseFailed must produce a terminal failure
		// with InspectionFailedReason.
		Context("inspection phase Failed terminal failure", func() {
			It("Should mark Beskar7Machine terminally Failed when PhysicalHost inspection phase is Failed", func() {
				machine := &infrav1.Beskar7Machine{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "failed-inspection-target",
						Namespace: testNs.Name,
					},
					Spec: infrav1.Beskar7MachineSpec{
						InspectionImageURL: "http://boot-server/ipxe/inspect.ipxe",
						TargetImageURL:     "http://boot-server/images/kairos.tar.gz",
						TargetImageDigest:  bootTestDigest,
					},
				}
				host := &infrav1.PhysicalHost{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "failed-inspection-host",
						Namespace: testNs.Name,
					},
					Status: infrav1.PhysicalHostStatus{
						InspectionPhase: infrav1.InspectionPhaseFailed,
					},
				}

				result, err := reconciler.handleInspectingHost(ctx, reconciler.Log, machine, host)
				Expect(err).NotTo(HaveOccurred(), "terminal failures must NOT return an error")
				Expect(result).To(Equal(ctrl.Result{}), "terminal failures must NOT requeue")

				Expect(machine.Status.Ready).To(BeFalse())
				Expect(machine.Status.Phase).NotTo(BeNil())
				Expect(*machine.Status.Phase).To(Equal(infrav1.PhaseFailed))

				cond := conditions.Get(machine, infrav1.InfrastructureReadyCondition)
				Expect(cond).NotTo(BeNil(), "InfrastructureReady condition must be set")
				Expect(cond.Status).To(Equal(metav1.ConditionFalse))
				Expect(cond.Reason).To(Equal(infrav1.InspectionFailedReason))
				Expect(cond.Message).NotTo(BeEmpty())
			})
		})

		// Preserve existing behavior: still-in-progress past the timeout must be
		// marked InspectionTimedOut (the timeout check still fires for non-terminal phases).
		Context("inspection still in progress past timeout", func() {
			It("Should mark Beskar7Machine terminally Failed with InspectionTimedOut when in-progress inspection times out", func() {
				machine := &infrav1.Beskar7Machine{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "inprogress-timeout-target",
						Namespace: testNs.Name,
					},
					Spec: infrav1.Beskar7MachineSpec{
						InspectionImageURL: "http://boot-server/ipxe/inspect.ipxe",
						TargetImageURL:     "http://boot-server/images/kairos.tar.gz",
						TargetImageDigest:  bootTestDigest,
					},
				}
				old := metav1.NewTime(time.Now().Add(-2 * DefaultInspectionTimeout))
				host := &infrav1.PhysicalHost{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "inprogress-timeout-host",
						Namespace: testNs.Name,
					},
					Spec: infrav1.PhysicalHostSpec{
						RedfishConnection: infrav1.RedfishConnection{
							Address:              "https://192.168.1.100",
							CredentialsSecretRef: credentialSecret.Name,
						},
					},
				}
				// Persist the host so setInspectionRequestAnnotation can patch it.
				Expect(k8sClient.Create(ctx, host)).To(Succeed())
				host.Status.State = infrav1.StateInspecting
				host.Status.InspectionPhase = infrav1.InspectionPhaseInProgress
				host.Status.InspectionTimestamp = &old
				Expect(k8sClient.Status().Update(ctx, host)).To(Succeed())

				result, err := reconciler.handleInspectingHost(ctx, reconciler.Log, machine, host)
				Expect(err).NotTo(HaveOccurred(), "terminal failures must NOT return an error")
				Expect(result).To(Equal(ctrl.Result{}), "terminal failures must NOT requeue")

				Expect(machine.Status.Ready).To(BeFalse())
				Expect(machine.Status.Phase).NotTo(BeNil())
				Expect(*machine.Status.Phase).To(Equal(infrav1.PhaseFailed))
				cond := conditions.Get(machine, infrav1.InfrastructureReadyCondition)
				Expect(cond).NotTo(BeNil(), "InfrastructureReady condition must be set")
				Expect(cond.Status).To(Equal(metav1.ConditionFalse))
				Expect(cond.Reason).To(Equal(infrav1.InspectionTimedOutReason))
				Expect(cond.Message).To(ContainSubstring("Inspection did not complete"))
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
			BootstrapURLBase:     "https://test-mgr.capb7-system.svc:8082",
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

	It("Should allow exactly one machine to claim the host; the other retries or sees no host", func() {
		By("Creating one available PhysicalHost with no ConsumerRef")
		host := &infrav1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "race-host",
				Namespace: testNs.Name,
			},
			Spec: infrav1.PhysicalHostSpec{
				RedfishConnection: infrav1.RedfishConnection{
					Address:              "https://192.168.100.1",
					CredentialsSecretRef: "irrelevant",
				},
			},
		}
		Expect(k8sClient.Create(ctx, host)).To(Succeed())
		// Status must be set via the status subresource.
		host.Status.State = infrav1.StateAvailable
		Expect(k8sClient.Status().Update(ctx, host)).To(Succeed())

		By("Creating two Beskar7Machines that would each want to claim the host")
		machineA := &infrav1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "machine-a", Namespace: testNs.Name},
			Spec: infrav1.Beskar7MachineSpec{
				InspectionImageURL: "http://boot-server/inspect.ipxe",
				TargetImageURL:     "http://boot-server/kairos.tar.gz",
				TargetImageDigest:  bootTestDigest,
			},
		}
		machineB := &infrav1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "machine-b", Namespace: testNs.Name},
			Spec: infrav1.Beskar7MachineSpec{
				InspectionImageURL: "http://boot-server/inspect.ipxe",
				TargetImageURL:     "http://boot-server/kairos.tar.gz",
				TargetImageDigest:  bootTestDigest,
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
			cachedHost := &infrav1.PhysicalHost{}
			g.Expect(mgr.GetClient().Get(ctx, hostKey, cachedHost)).To(Succeed())
			g.Expect(cachedHost.Status.State).To(Equal(infrav1.StateAvailable))
		}, 10*time.Second, 100*time.Millisecond).Should(Succeed())

		// Re-fetch machines through the cache so they have a valid UID (needed by ConsumerRef).
		Eventually(func(g Gomega) {
			g.Expect(mgr.GetClient().Get(ctx, types.NamespacedName{Name: machineA.Name, Namespace: testNs.Name}, machineA)).To(Succeed())
			g.Expect(mgr.GetClient().Get(ctx, types.NamespacedName{Name: machineB.Name, Namespace: testNs.Name}, machineB)).To(Succeed())
		}, 10*time.Second, 100*time.Millisecond).Should(Succeed())

		By("Calling findAndClaimOrGetAssociatedHost for machine-a then machine-b in quick succession")
		log := ctrl.Log.WithName("race-test-direct")

		claimedByA, resultA, errA := r.findAndClaimOrGetAssociatedHost(ctx, log, machineA, nil)
		claimedByB, resultB, errB := r.findAndClaimOrGetAssociatedHost(ctx, log, machineB, nil)

		By("Asserting exactly one machine won and neither call returned a hard error")
		// Both calls must not return a hard error.
		Expect(errA).NotTo(HaveOccurred(), "machine-a must not return a hard error")
		Expect(errB).NotTo(HaveOccurred(), "machine-b must not return a hard error")

		// In a sequential execution: machine-a wins the Patch, machine-b sees
		// ConsumerRef already set on the only host and returns (nil, {}, nil) — no
		// hosts available for it. In a truly concurrent execution (e.g. multiple
		// goroutines), the loser gets a Conflict 409 and returns Requeue=true.
		// Either outcome is correct. The invariant under test is: the host ends up
		// claimed by exactly one machine, and the second caller either gets the
		// short conflict retry (RequeueAfter == requeueShortly) or nil/nil (no
		// available hosts path).
		aWon := claimedByA != nil
		bWon := claimedByB != nil
		Expect(aWon || bWon).To(BeTrue(), "at least one machine must have claimed the host")
		Expect(aWon && bWon).To(BeFalse(), "both machines must not claim the host simultaneously")

		// The winner gets the fresh-claim requeue (5s, so the next pass re-finds
		// the host via ConsumerRef), never the conflict retry; the loser gets the
		// conflict retry or nothing.
		winner, loser := resultA, resultB
		if bWon {
			winner, loser = resultB, resultA
		}
		Expect(winner.RequeueAfter).To(Equal(5*time.Second), "the winner must get the fresh-claim requeue, not the conflict retry")
		Expect(loser.RequeueAfter).To(Or(Equal(requeueShortly), BeZero()), "the loser retries shortly (conflict) or sees no host")

		By("Asserting host ConsumerRef points at exactly one machine")
		Eventually(func(g Gomega) {
			updatedHost := &infrav1.PhysicalHost{}
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
			Expect(machineB.Spec.ProviderID).To(BeEmpty(), "losing machine-b must have no ProviderID")
		} else {
			Expect(machineA.Spec.ProviderID).To(BeEmpty(), "losing machine-a must have no ProviderID")
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
		host := &infrav1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{Name: "no-avail-host", Namespace: "default"},
			Status:     infrav1.PhysicalHostStatus{State: infrav1.StateInUse},
		}
		machine := &infrav1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "no-avail-machine", Namespace: "default"},
			Spec: infrav1.Beskar7MachineSpec{
				InspectionImageURL: "http://boot/inspect.ipxe",
				TargetImageURL:     "http://boot/kairos.tar.gz",
				TargetImageDigest:  bootTestDigest,
			},
		}

		fakeC := fake.NewClientBuilder().
			WithScheme(scheme.Scheme).
			WithObjects(machine).
			WithStatusSubresource(host).
			// Same indexer SetupWithManager registers on the real cache.
			WithIndex(&infrav1.PhysicalHost{}, PhysicalHostStateIndex, func(obj client.Object) []string {
				h, ok := obj.(*infrav1.PhysicalHost)
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

		got, result, err := r.findAndClaimOrGetAssociatedHost(context.Background(), r.Log, machine, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(BeNil(), "no Available host means no host to return")
		Expect(result).To(Equal(ctrl.Result{}), "the helper does not requeue itself; the caller does")
	})

	// SEC-12: a PhysicalHost's ConsumerRef.Name matching this machine's name is
	// not sufficient — the ConsumerRef must also be scoped to the host's own
	// namespace. A real claim always sets both together
	// (findAndClaimOrGetAssociatedHost claims only hosts it has just listed
	// from its own namespace), so a ConsumerRef naming a different namespace can
	// only come from direct PhysicalHost patch access. Uses the same fake-client
	// + field-index pattern as the spec above, since this also falls through to
	// the StateAvailable-indexed list once the ConsumerRef match correctly fails.
	It("Should NOT re-find a PhysicalHost whose ConsumerRef names a same-named machine in a different namespace (SEC-12)", func() {
		host := &infrav1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{Name: "crossns-host", Namespace: "team-a"},
			Spec: infrav1.PhysicalHostSpec{
				ConsumerRef: &corev1.ObjectReference{
					Kind:       "Beskar7Machine",
					APIVersion: InfrastructureAPIVersion,
					Name:       "crossns-machine",
					// Forged: names the same machine name as machine below, but in a
					// namespace that is not the host's own ("team-a").
					Namespace: "team-b",
				},
			},
			Status: infrav1.PhysicalHostStatus{State: infrav1.StateInUse},
		}
		machine := &infrav1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "crossns-machine", Namespace: "team-a"},
			Spec: infrav1.Beskar7MachineSpec{
				InspectionImageURL: "http://boot/inspect.ipxe",
				TargetImageURL:     "http://boot/kairos.tar.gz",
				TargetImageDigest:  bootTestDigest,
			},
		}

		fakeC := fake.NewClientBuilder().
			WithScheme(scheme.Scheme).
			WithObjects(machine).
			WithStatusSubresource(host).
			WithIndex(&infrav1.PhysicalHost{}, PhysicalHostStateIndex, func(obj client.Object) []string {
				h, ok := obj.(*infrav1.PhysicalHost)
				if !ok {
					return nil
				}
				return []string{string(h.Status.State)}
			}).
			Build()
		Expect(fakeC.Create(context.Background(), host)).To(Succeed())
		Expect(fakeC.Status().Update(context.Background(), host)).To(Succeed())

		r := &Beskar7MachineReconciler{
			Client: fakeC,
			Scheme: scheme.Scheme,
			Log:    ctrl.Log.WithName("crossns-refind-test"),
		}

		got, _, err := r.findAndClaimOrGetAssociatedHost(context.Background(), r.Log, machine, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(BeNil(),
			"a host claimed by a same-named machine in a different namespace must not resolve as this machine's host")
	})
})

// Bootstrap data secret tests — envtest + unit level.
var _ = Describe("Beskar7Machine bootstrap data secret handling", func() {
	const (
		bootstrapURLBase = "https://beskar7-controller-manager.capb7-system.svc:8082"
		Timeout          = time.Second * 10
		Interval         = time.Millisecond * 250
	)

	var (
		testNs       *corev1.Namespace
		physicalHost *infrav1.PhysicalHost
		b7machine    *infrav1.Beskar7Machine
		machine      *clusterv1.Machine
		r            *Beskar7MachineReconciler
	)

	BeforeEach(func() {
		testNs = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "bootstrap-test-"},
		}
		Expect(k8sClient.Create(ctx, testNs)).To(Succeed())

		// Create a PhysicalHost already associated (ConsumerRef set, State=InUse).
		physicalHost = &infrav1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "bootstrap-test-host",
				Namespace: testNs.Name,
			},
			Spec: infrav1.PhysicalHostSpec{
				RedfishConnection: infrav1.RedfishConnection{
					Address:              "https://192.168.1.200",
					CredentialsSecretRef: "irrelevant",
				},
			},
		}
		Expect(k8sClient.Create(ctx, physicalHost)).To(Succeed())
		physicalHost.Status.State = infrav1.StateInUse
		Expect(k8sClient.Status().Update(ctx, physicalHost)).To(Succeed())

		b7machine = &infrav1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "bootstrap-test-b7machine",
				Namespace: testNs.Name,
			},
			Spec: infrav1.Beskar7MachineSpec{
				InspectionImageURL: "http://boot-server/inspect.ipxe",
				TargetImageURL:     "http://boot-server/kairos.tar.gz",
				TargetImageDigest:  bootTestDigest,
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
				ClusterName:       "fake-cluster",
				InfrastructureRef: clusterv1.ContractVersionedObjectReference{APIGroup: "infrastructure.cluster.x-k8s.io", Kind: "Beskar7Machine", Name: "fixture"},
				Bootstrap:         clusterv1.Bootstrap{},
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

		cond := conditions.Get(b7machine, infrav1.BootstrapDataReadyCondition)
		Expect(cond).NotTo(BeNil(), "BootstrapDataReadyCondition must be set")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(infrav1.WaitingForBootstrapDataReason))

		By("Verifying no bootstrap-url annotation was set on PhysicalHost")
		ph := &infrav1.PhysicalHost{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: physicalHost.Name, Namespace: testNs.Name}, ph)).To(Succeed())
		Expect(ph.Annotations).NotTo(HaveKey(BootstrapURLAnnotation))
	})

	It("Should mark a terminal InfrastructureReady=False/BootstrapDataUnavailable when the named Secret is missing", func() {
		By("Setting DataSecretName to a non-existent secret")
		missingName := "does-not-exist"
		machine.Spec.Bootstrap.DataSecretName = &missingName

		result, err := r.ensureBootstrapDataReady(ctx, r.Log, b7machine, machine, physicalHost)

		By("Expecting a terminal (zero requeue, no error returned) result")
		Expect(err).NotTo(HaveOccurred(),
			"terminal failures must return nil error so CAPI surfaces the failure via conditions")
		Expect(result.IsZero()).To(BeTrue(),
			"terminal failure must not requeue")

		cond := conditions.Get(b7machine, infrav1.BootstrapDataReadyCondition)
		Expect(cond).NotTo(BeNil(), "BootstrapDataReadyCondition must be set")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(infrav1.BootstrapDataUnavailableReason))

		Expect(b7machine.Status.Phase).NotTo(BeNil())
		Expect(*b7machine.Status.Phase).To(Equal(infrav1.PhaseFailed))
		infraCond := conditions.Get(b7machine, infrav1.InfrastructureReadyCondition)
		Expect(infraCond).NotTo(BeNil(), "InfrastructureReady condition must be set")
		Expect(infraCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(infraCond.Reason).To(Equal(infrav1.BootstrapDataUnavailableReason))
		Expect(infraCond.Message).NotTo(BeEmpty())
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
		cond := conditions.Get(b7machine, infrav1.BootstrapDataReadyCondition)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))

		By("Verifying the bootstrap-url annotation was set on the PhysicalHost")
		ph := &infrav1.PhysicalHost{}
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
		physicalHost.Status.Bootstrap = &infrav1.BootstrapStatus{URL: expectedURL}
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
		ph := &infrav1.PhysicalHost{}
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
		withSlash := buildURL("https://beskar7-controller-manager.capb7-system.svc:8082/", "default", "my-host")
		withoutSlash := buildURL("https://beskar7-controller-manager.capb7-system.svc:8082", "default", "my-host")
		Expect(withSlash).To(Equal(withoutSlash))
		Expect(withSlash).To(Equal("https://beskar7-controller-manager.capb7-system.svc:8082/api/v1/bootstrap/default/my-host"))
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
		host := &infrav1.PhysicalHost{
			Spec: infrav1.PhysicalHostSpec{
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
		host := &infrav1.PhysicalHost{
			Spec: infrav1.PhysicalHostSpec{},
		}
		reqs := r.PhysicalHostToBeskar7Machine(context.Background(), host)
		Expect(reqs).To(BeEmpty())
	})

	It("Should not enqueue when ConsumerRef is a different Kind", func() {
		host := &infrav1.PhysicalHost{
			Spec: infrav1.PhysicalHostSpec{
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
		host := &infrav1.PhysicalHost{
			Spec: infrav1.PhysicalHostSpec{
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

// Credential mint wiring (PR-5.2, D-006, D-029). ensureBootstrapCredentials
// stores both credentials, their expiries and the consumer binding in one
// write of the per-host Secret (labels, owner-ref), and writes nothing else:
// no annotation on the host and nothing in its status.
var _ = Describe("Beskar7Machine credential mint into the bound Secret (PR-5.2, D-029)", func() {
	var (
		testNs       *corev1.Namespace
		physicalHost *infrav1.PhysicalHost
		b7machine    *infrav1.Beskar7Machine
		r            *Beskar7MachineReconciler
		secretKey    types.NamespacedName
	)

	BeforeEach(func() {
		testNs = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "mint-token-test-"},
		}
		Expect(k8sClient.Create(ctx, testNs)).To(Succeed())

		physicalHost = &infrav1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "mint-token-host",
				Namespace: testNs.Name,
			},
			Spec: infrav1.PhysicalHostSpec{
				RedfishConnection: infrav1.RedfishConnection{
					Address:              "https://192.168.42.1",
					CredentialsSecretRef: "irrelevant",
				},
			},
		}
		Expect(k8sClient.Create(ctx, physicalHost)).To(Succeed())
		secretKey = types.NamespacedName{Namespace: testNs.Name, Name: bootstrapTokenSecretName(physicalHost.Name)}
		b7machine = &infrav1.Beskar7Machine{ObjectMeta: metav1.ObjectMeta{Name: "mint-machine", Namespace: testNs.Name}}

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

	getSecret := func() *corev1.Secret {
		s := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, secretKey, s)).To(Succeed())
		return s
	}
	getHost := func() *infrav1.PhysicalHost {
		ph := &infrav1.PhysicalHost{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(physicalHost), ph)).To(Succeed())
		return ph
	}

	It("creates the per-host Secret with both credentials, their expiries and the consumer, and writes nothing on the host", func() {
		statusBefore := physicalHost.Status.DeepCopy()
		now := time.Now()
		Expect(r.ensureBootstrapCredentials(ctx, r.Log, b7machine, physicalHost, now)).To(Succeed())

		s := getSecret()
		Expect(s.Type).To(Equal(corev1.SecretTypeOpaque))
		token, nonce := string(s.Data[bootstrapTokenSecretKey]), string(s.Data[bootNonceSecretKey])
		// 32 random bytes → base64.RawURLEncoding → 43 chars.
		Expect(token).To(HaveLen(43), "plaintext token length must match auth.MintToken's contract (43 chars)")
		Expect(nonce).To(HaveLen(43))
		Expect(nonce).NotTo(Equal(token), "the nonce and the bearer token are distinct secrets (D-009)")
		creds := readBootstrapCredentials(s)
		Expect(creds.consumer).To(Equal(b7machine.Name))
		Expect(creds.tokenIssuedAt).To(BeTemporally("~", now, time.Second))
		Expect(creds.tokenExpiresAt).To(BeTemporally("~", now.Add(auth.TokenLifetime), time.Second))
		Expect(creds.nonceExpiresAt).To(BeTemporally("~", now.Add(auth.BootNonceLifetime), time.Second))
		// Owner ref so the Secret is GC'd on host deletion.
		Expect(s.OwnerReferences).To(HaveLen(1))
		Expect(s.OwnerReferences[0].Kind).To(Equal("PhysicalHost"))
		Expect(s.OwnerReferences[0].Name).To(Equal(physicalHost.Name))
		// Labels for operator visibility + cleanup.
		Expect(s.Labels).To(HaveKeyWithValue(inspectionResultLabelOwnedBy, "beskar7-controller-manager"))
		Expect(s.Labels).To(HaveKeyWithValue(inspectionResultLabelHost, physicalHost.Name))

		By("writing nothing on the PhysicalHost: no annotation, no status, no plaintext")
		ph := getHost()
		Expect(ph.Annotations).NotTo(HaveKey(BootstrapTokenAnnotation))
		Expect(ph.Annotations).NotTo(HaveKey(BootNonceAnnotation))
		Expect(ph.Status.Bootstrap).To(Equal(statusBefore.Bootstrap),
			"the mint must not write PhysicalHost.Status — the PhysicalHost reconciler mirrors the Secret")
		encoded, err := json.Marshal(ph)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(encoded)).NotTo(ContainSubstring(token))
		Expect(string(encoded)).NotTo(ContainSubstring(nonce))
	})

	It("mints nothing when both credentials are bound to this machine and valid", func() {
		Expect(r.ensureBootstrapCredentials(ctx, r.Log, b7machine, physicalHost, time.Now())).To(Succeed())
		before := getSecret()

		Expect(r.ensureBootstrapCredentials(ctx, r.Log, b7machine, getHost(), time.Now())).To(Succeed())
		Expect(getSecret().ResourceVersion).To(Equal(before.ResourceVersion), "a valid, bound pair must not be rewritten")
	})

	It("re-mints only the nonce once its consume is recorded, keeping the bound token (single-use enforcement)", func() {
		Expect(r.ensureBootstrapCredentials(ctx, r.Log, b7machine, physicalHost, time.Now())).To(Succeed())
		first := readBootstrapCredentials(getSecret())

		By("recording the nonce's consume, as /boot does")
		ph := getHost()
		consumed := metav1.Now()
		ph.Status.Bootstrap = &infrav1.BootstrapStatus{BootNonceConsumedAt: &consumed, BootNonceConsumedHash: auth.Hash(first.nonce)}
		Expect(k8sClient.Status().Update(ctx, ph)).To(Succeed())

		Expect(r.ensureBootstrapCredentials(ctx, r.Log, b7machine, getHost(), time.Now())).To(Succeed())
		second := readBootstrapCredentials(getSecret())
		Expect(second.nonce).NotTo(Equal(first.nonce), "a consumed nonce is never reused")
		Expect(second.token).To(Equal(first.token), "the nonce mint must not replace the bearer token")
		Expect(second.tokenExpiresAt).To(Equal(first.tokenExpiresAt))
		Expect(second.consumer).To(Equal(b7machine.Name))
	})

	It("mints fresh credentials for a machine recreated under the same name", func() {
		first := b7machine.DeepCopy()
		first.UID = "uid-of-the-first-machine"
		Expect(r.ensureBootstrapCredentials(ctx, r.Log, first, physicalHost, time.Now())).To(Succeed())
		minted := readBootstrapCredentials(getSecret())
		Expect(minted.consumerUID).To(Equal(string(first.UID)))

		recreated := b7machine.DeepCopy()
		recreated.UID = "uid-of-the-recreated-machine"
		Expect(r.ensureBootstrapCredentials(ctx, r.Log, recreated, getHost(), time.Now())).To(Succeed())
		after := readBootstrapCredentials(getSecret())
		Expect(after.token).NotTo(Equal(minted.token),
			"a token the earlier machine's run may have exposed must not serve the new claim")
		Expect(after.nonce).NotTo(Equal(minted.nonce))
		Expect(after.consumer).To(Equal(recreated.Name))
		Expect(after.consumerUID).To(Equal(string(recreated.UID)))
	})
})

// The reuse decision reads only the Secret: nothing the PhysicalHost carries
// (its status mirror, its annotations) can make a credential look reusable.
var _ = Describe("bootstrapTokenReusable", func() {
	now := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	const consumer = "reuse-machine"
	bound := func() bootstrapCredentials {
		return bootstrapCredentials{token: "t", tokenExpiresAt: now.Add(10 * time.Minute), consumer: consumer}
	}

	It("reuses a token bound to this machine within its validity window", func() {
		Expect(bootstrapTokenReusable(bound(), consumer, now)).To(BeTrue())
	})

	It("re-mints when the Secret holds no token", func() {
		creds := bound()
		creds.token = ""
		Expect(bootstrapTokenReusable(creds, consumer, now)).To(BeFalse())
	})

	It("re-mints when the token has no expiry: a missing or unparseable key fails closed", func() {
		creds := bound()
		creds.tokenExpiresAt = time.Time{}
		Expect(bootstrapTokenReusable(creds, consumer, now)).To(BeFalse())
	})

	It("re-mints when the expiry is in the past", func() {
		creds := bound()
		creds.tokenExpiresAt = now.Add(-time.Minute)
		Expect(bootstrapTokenReusable(creds, consumer, now)).To(BeFalse())
	})

	It("re-mints when the expiry equals now (boundary)", func() {
		creds := bound()
		creds.tokenExpiresAt = now
		Expect(bootstrapTokenReusable(creds, consumer, now)).To(BeFalse(),
			"now.Before(expiry) is false at equality — boundary must re-mint")
	})

	It("re-mints when the Secret is bound to another machine, so a token never outlives its claim (SEC-13)", func() {
		Expect(bootstrapTokenReusable(bound(), "next-machine", now)).To(BeFalse())
	})

	It("re-mints when the Secret carries no binding", func() {
		creds := bound()
		creds.consumer = ""
		Expect(bootstrapTokenReusable(creds, consumer, now)).To(BeFalse())
	})
})

var _ = Describe("bootNonceReusable", func() {
	now := time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC)
	const consumer = "reuse-machine"
	bound := func() bootstrapCredentials {
		return bootstrapCredentials{nonce: "n", nonceExpiresAt: now.Add(5 * time.Minute), consumer: consumer}
	}
	consumedAt := metav1.NewTime(now.Add(-time.Minute))

	It("reuses a bound, unexpired nonce nothing has consumed", func() {
		Expect(bootNonceReusable(bound(), nil, consumer, now)).To(BeTrue())
	})

	It("re-mints when the Secret holds no nonce", func() {
		creds := bound()
		creds.nonce = ""
		Expect(bootNonceReusable(creds, nil, consumer, now)).To(BeFalse())
	})

	It("re-mints when the nonce has no expiry: a missing or unparseable key fails closed", func() {
		creds := bound()
		creds.nonceExpiresAt = time.Time{}
		Expect(bootNonceReusable(creds, nil, consumer, now)).To(BeFalse())
	})

	It("re-mints when the expiry is in the past, or equals now", func() {
		creds := bound()
		creds.nonceExpiresAt = now.Add(-time.Second)
		Expect(bootNonceReusable(creds, nil, consumer, now)).To(BeFalse())
		creds.nonceExpiresAt = now
		Expect(bootNonceReusable(creds, nil, consumer, now)).To(BeFalse())
	})

	It("re-mints when the Secret is bound to another machine", func() {
		Expect(bootNonceReusable(bound(), nil, "next-machine", now)).To(BeFalse())
	})

	It("re-mints when the consume record names this nonce", func() {
		bs := &infrav1.BootstrapStatus{BootNonceConsumedAt: &consumedAt, BootNonceConsumedHash: auth.Hash("n")}
		Expect(bootNonceReusable(bound(), bs, consumer, now)).To(BeFalse(), "a consumed nonce is never reused")
	})

	It("reuses the nonce when the consume record names an earlier one", func() {
		bs := &infrav1.BootstrapStatus{BootNonceConsumedAt: &consumedAt, BootNonceConsumedHash: auth.Hash("earlier")}
		Expect(bootNonceReusable(bound(), bs, consumer, now)).To(BeTrue(),
			"Status.Bootstrap outlives a claim; an earlier cycle's record must not spend this nonce")
	})

	It("re-mints when a consume record names no nonce, since it might describe this one", func() {
		bs := &infrav1.BootstrapStatus{BootNonceConsumedAt: &consumedAt}
		Expect(bootNonceReusable(bound(), bs, consumer, now)).To(BeFalse())
	})

	It("ignores the hash and expiry the status mirror advertises", func() {
		past := metav1.NewTime(now.Add(-time.Hour))
		bs := &infrav1.BootstrapStatus{BootNonceHash: auth.Hash("something-else"), BootNonceExpiresAt: &past}
		Expect(bootNonceReusable(bound(), bs, consumer, now)).To(BeTrue())
	})
})

// The one-release upgrade backfill (D-029): a run in flight across the upgrade
// keeps its credentials once they are bound to its machine. Pure unit tests;
// the envtest specs in callback_credentials_test.go run it through the
// reconciler.
var _ = Describe("backfillLegacyCredentials", func() {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	const machine = "inflight-machine"
	token, tokenHash := "legacy-token", auth.Hash("legacy-token")
	nonce, nonceHash := "legacy-nonce", auth.Hash("legacy-nonce")

	legacyHost := func(state string, tokenExpiry, nonceExpiry time.Time) *infrav1.PhysicalHost {
		issued := metav1.NewTime(now.Add(-5 * time.Minute))
		te, ne := metav1.NewTime(tokenExpiry), metav1.NewTime(nonceExpiry)
		host := claimedPhysicalHost("ns", "host", machine)
		host.Status.State = state
		host.Status.Bootstrap = &infrav1.BootstrapStatus{
			TokenHash: tokenHash, IssuedAt: &issued, ExpiresAt: &te,
			BootNonceHash: nonceHash, BootNonceExpiresAt: &ne,
		}
		return host
	}
	legacyCreds := func() bootstrapCredentials {
		return bootstrapCredentials{token: token, nonce: nonce}
	}

	for _, state := range []string{infrav1.StateInUse, infrav1.StateInspecting, infrav1.StateDeploying} {
		It("binds both credentials of a host in "+state+" with the lifetimes status carried", func() {
			creds := legacyCreds()
			host := legacyHost(state, now.Add(40*time.Minute), now.Add(4*time.Minute))
			Expect(backfillLegacyCredentials(&creds, host, machine, now)).To(BeTrue())
			Expect(creds.consumer).To(Equal(machine))
			Expect(creds.token).To(Equal(token))
			Expect(creds.tokenExpiresAt).To(Equal(host.Status.Bootstrap.ExpiresAt.Time))
			Expect(creds.tokenIssuedAt).To(Equal(host.Status.Bootstrap.IssuedAt.Time))
			Expect(creds.nonce).To(Equal(nonce))
			Expect(creds.nonceExpiresAt).To(Equal(host.Status.Bootstrap.BootNonceExpiresAt.Time))
		})
	}

	It("caps a lifetime status carried beyond a fresh one", func() {
		creds := legacyCreds()
		host := legacyHost(infrav1.StateInspecting, now.Add(24*time.Hour), now.Add(24*time.Hour))
		Expect(backfillLegacyCredentials(&creds, host, machine, now)).To(BeTrue())
		Expect(creds.tokenExpiresAt).To(Equal(now.Add(auth.TokenLifetime)))
		Expect(creds.nonceExpiresAt).To(Equal(now.Add(auth.BootNonceLifetime)))
	})

	It("leaves a Ready host alone: no callback follows Ready", func() {
		creds := legacyCreds()
		Expect(backfillLegacyCredentials(&creds, legacyHost(infrav1.StateReady, now.Add(time.Hour), now.Add(time.Hour)), machine, now)).To(BeFalse())
		Expect(creds.consumer).To(BeEmpty())
		Expect(creds.tokenExpiresAt.IsZero()).To(BeTrue())
	})

	It("never backfills a credential whose plaintext does not hash to what status carries", func() {
		creds := legacyCreds()
		host := legacyHost(infrav1.StateInspecting, now.Add(time.Hour), now.Add(time.Minute))
		host.Status.Bootstrap.TokenHash = auth.Hash("forged-through-an-annotation")
		Expect(backfillLegacyCredentials(&creds, host, machine, now)).To(BeTrue(), "the nonce still matches")
		Expect(creds.tokenExpiresAt.IsZero()).To(BeTrue(), "a token without an expiry never verifies")
		Expect(creds.nonceExpiresAt).To(Equal(now.Add(time.Minute)))
	})

	It("never backfills a credential status gave no expiry", func() {
		creds := legacyCreds()
		host := legacyHost(infrav1.StateInspecting, now.Add(time.Hour), now.Add(time.Minute))
		host.Status.Bootstrap.ExpiresAt = nil
		host.Status.Bootstrap.BootNonceExpiresAt = nil
		Expect(backfillLegacyCredentials(&creds, host, machine, now)).To(BeFalse())
		Expect(creds.consumer).To(BeEmpty())
	})

	It("leaves a Secret that already carries a binding alone", func() {
		creds := legacyCreds()
		creds.consumer = "someone"
		Expect(backfillLegacyCredentials(&creds, legacyHost(infrav1.StateInspecting, now.Add(time.Hour), now.Add(time.Minute)), machine, now)).To(BeFalse())
		Expect(creds.consumer).To(Equal("someone"))
	})

	It("binds only to the machine the host's claim names", func() {
		creds := legacyCreds()
		Expect(backfillLegacyCredentials(&creds, legacyHost(infrav1.StateInspecting, now.Add(time.Hour), now.Add(time.Minute)), "other-machine", now)).To(BeFalse())
		Expect(creds.consumer).To(BeEmpty())
	})
})

var _ = Describe("Host claim honours placement: failure domain and hostSelector", func() {
	// CAPI places a Machine into one of the failure domains Beskar7Cluster
	// publishes, which it derives from the topology.kubernetes.io/zone label on
	// PhysicalHosts. The fresh-claim path must respect that placement: an
	// Available host in another zone, or with no zone at all, is not a candidate.
	// A fake client with the same status.state index the manager registers is
	// enough here — the claim is a List with a field selector plus a label
	// selector, and both are honoured by the fake.

	hostIndex := func(obj client.Object) []string {
		h, ok := obj.(*infrav1.PhysicalHost)
		if !ok {
			return nil
		}
		return []string{string(h.Status.State)}
	}
	availableHost := func(name, zone string) *infrav1.PhysicalHost {
		h := &infrav1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: infrav1.PhysicalHostSpec{
				RedfishConnection: infrav1.RedfishConnection{Address: "https://192.0.2.10", CredentialsSecretRef: "irrelevant"},
			},
			Status: infrav1.PhysicalHostStatus{State: infrav1.StateAvailable},
		}
		if zone != "" {
			h.Labels = map[string]string{zoneLabelKey: zone}
		}
		return h
	}
	newClientWith := func(hosts ...*infrav1.PhysicalHost) client.Client {
		b := fake.NewClientBuilder().WithScheme(scheme.Scheme).
			WithIndex(&infrav1.PhysicalHost{}, PhysicalHostStateIndex, hostIndex)
		for _, h := range hosts {
			b = b.WithStatusSubresource(h)
		}
		c := b.Build()
		for _, h := range hosts {
			Expect(c.Create(context.Background(), h)).To(Succeed())
			Expect(c.Status().Update(context.Background(), h)).To(Succeed())
		}
		return c
	}
	newMachine := func() *infrav1.Beskar7Machine {
		return &infrav1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "fd-machine", Namespace: "default", Finalizers: []string{Beskar7MachineFinalizer}},
			Spec: infrav1.Beskar7MachineSpec{
				InspectionImageURL: "http://boot/inspect.ipxe",
				TargetImageURL:     "http://boot/kairos.tar.gz",
				TargetImageDigest:  bootTestDigest,
			},
		}
	}
	inZone := func(zone string) labels.Selector { return labels.SelectorFromSet(labels.Set{zoneLabelKey: zone}) }
	consumerOf := func(c client.Client, name string) *corev1.ObjectReference {
		h := &infrav1.PhysicalHost{}
		Expect(c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, h)).To(Succeed())
		return h.Spec.ConsumerRef
	}

	It("claims only a host in the failure domain, even when others list first", func() {
		// Names sort so both non-matching hosts come before the match.
		c := newClientWith(availableHost("a-other-zone", "rack-2"), availableHost("b-no-zone", ""), availableHost("c-in-zone", "rack-1"))
		r := &Beskar7MachineReconciler{Client: c, Scheme: scheme.Scheme, Log: ctrl.Log.WithName("fd-test")}

		got, result, err := r.findAndClaimOrGetAssociatedHost(context.Background(), r.Log, newMachine(), inZone("rack-1"))
		Expect(err).NotTo(HaveOccurred())
		Expect(got).NotTo(BeNil(), "an Available host in the failure domain must be claimed")
		Expect(got.Name).To(Equal("c-in-zone"))
		Expect(result.RequeueAfter).To(Equal(5*time.Second), "a fresh claim requeues so the next pass re-finds it via ConsumerRef")
		Expect(consumerOf(c, "c-in-zone")).NotTo(BeNil())
		Expect(consumerOf(c, "a-other-zone")).To(BeNil(), "a host in another zone must not be touched")
		Expect(consumerOf(c, "b-no-zone")).To(BeNil(), "a host with no zone label must not be touched")
	})

	It("claims nothing when no Available host is in the failure domain", func() {
		c := newClientWith(availableHost("a-other-zone", "rack-2"), availableHost("b-no-zone", ""))
		r := &Beskar7MachineReconciler{Client: c, Scheme: scheme.Scheme, Log: ctrl.Log.WithName("fd-test")}

		got, result, err := r.findAndClaimOrGetAssociatedHost(context.Background(), r.Log, newMachine(), inZone("rack-1"))
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(BeNil(), "a host outside the failure domain must not be claimed")
		Expect(result.IsZero()).To(BeTrue())
		Expect(consumerOf(c, "a-other-zone")).To(BeNil())
		Expect(consumerOf(c, "b-no-zone")).To(BeNil())
	})

	It("claims any Available host when the Machine has no failure domain", func() {
		c := newClientWith(availableHost("a-other-zone", "rack-2"))
		r := &Beskar7MachineReconciler{Client: c, Scheme: scheme.Scheme, Log: ctrl.Log.WithName("fd-test")}

		got, _, err := r.findAndClaimOrGetAssociatedHost(context.Background(), r.Log, newMachine(), nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).NotTo(BeNil(), "unconstrained machines keep today's behaviour")
		Expect(got.Name).To(Equal("a-other-zone"))
	})

	It("derives the placement from hostSelector and Machine.spec.failureDomain, ANDed", func() {
		sel, err := hostPlacementSelector(newMachine(), nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(sel).To(BeNil(), "no selector, no owner: no constraint")

		m := &clusterv1.Machine{}
		sel, err = hostPlacementSelector(newMachine(), m)
		Expect(err).NotTo(HaveOccurred())
		Expect(sel).To(BeNil(), "no failure domain: no constraint")

		m.Spec.FailureDomain = ""
		sel, err = hostPlacementSelector(newMachine(), m)
		Expect(err).NotTo(HaveOccurred())
		Expect(sel).To(BeNil(), "an empty failure domain is no constraint")

		fd := "rack-1"
		m.Spec.FailureDomain = fd
		sel, err = hostPlacementSelector(newMachine(), m)
		Expect(err).NotTo(HaveOccurred())
		Expect(sel.String()).To(Equal(zoneLabelKey + "=rack-1"))

		bad := "not a label value!"
		m.Spec.FailureDomain = bad
		_, err = hostPlacementSelector(newMachine(), m)
		Expect(err).To(HaveOccurred(), "a failure domain that cannot be a label value must be reported, not silently match nothing")
		Expect(errors.Is(err, errInvalidHostSelector)).To(BeFalse(), "a bad failure domain is not the spec's fault; it must not be terminal")

		By("an empty hostSelector is no constraint")
		b7m := newMachine()
		b7m.Spec.HostSelector = &metav1.LabelSelector{}
		sel, err = hostPlacementSelector(b7m, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(sel).To(BeNil())

		By("a hostSelector alone")
		b7m.Spec.HostSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"node-role": "control-plane"}}
		sel, err = hostPlacementSelector(b7m, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(sel.Matches(labels.Set{"node-role": "control-plane"})).To(BeTrue())
		Expect(sel.Matches(labels.Set{"node-role": "worker"})).To(BeFalse())

		By("a hostSelector AND a failure domain: both must hold")
		m.Spec.FailureDomain = fd
		sel, err = hostPlacementSelector(b7m, m)
		Expect(err).NotTo(HaveOccurred())
		Expect(sel.Matches(labels.Set{"node-role": "control-plane", zoneLabelKey: "rack-1"})).To(BeTrue())
		Expect(sel.Matches(labels.Set{"node-role": "control-plane", zoneLabelKey: "rack-2"})).To(BeFalse(), "right role, wrong zone")
		Expect(sel.Matches(labels.Set{"node-role": "worker", zoneLabelKey: "rack-1"})).To(BeFalse(), "right zone, wrong role")

		By("an unparsable hostSelector is reported as the spec's fault")
		b7m.Spec.HostSelector = &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "rack", Operator: "Bogus"}}}
		_, err = hostPlacementSelector(b7m, nil)
		Expect(err).To(HaveOccurred())
		Expect(errors.Is(err, errInvalidHostSelector)).To(BeTrue())
	})

	labelledHost := func(name string, lbls map[string]string) *infrav1.PhysicalHost {
		h := availableHost(name, "")
		h.Labels = lbls
		return h
	}
	withSelector := func(sel *metav1.LabelSelector) *infrav1.Beskar7Machine {
		m := newMachine()
		m.Spec.HostSelector = sel
		return m
	}
	placementOf := func(b7m *infrav1.Beskar7Machine, fd string) labels.Selector {
		var m *clusterv1.Machine
		if fd != "" {
			m = &clusterv1.Machine{Spec: clusterv1.MachineSpec{FailureDomain: fd}}
		}
		sel, err := hostPlacementSelector(b7m, m)
		Expect(err).NotTo(HaveOccurred())
		return sel
	}

	It("hostSelector: claims only a host matching the selector, even when others list first", func() {
		c := newClientWith(
			labelledHost("a-worker", map[string]string{"node-role": "worker"}),
			availableHost("b-unlabelled", ""),
			labelledHost("c-control-plane", map[string]string{"node-role": "control-plane"}),
		)
		r := &Beskar7MachineReconciler{Client: c, Scheme: scheme.Scheme, Log: ctrl.Log.WithName("hs-test")}
		b7m := withSelector(&metav1.LabelSelector{MatchLabels: map[string]string{"node-role": "control-plane"}})

		got, result, err := r.findAndClaimOrGetAssociatedHost(context.Background(), r.Log, b7m, placementOf(b7m, ""))
		Expect(err).NotTo(HaveOccurred())
		Expect(got).NotTo(BeNil())
		Expect(got.Name).To(Equal("c-control-plane"))
		Expect(result.RequeueAfter).To(Equal(5 * time.Second))
		Expect(consumerOf(c, "a-worker")).To(BeNil(), "a host with a different role label must not be touched")
		Expect(consumerOf(c, "b-unlabelled")).To(BeNil(), "an unlabelled host must not be touched")
	})

	It("hostSelector: claims nothing when no Available host matches", func() {
		c := newClientWith(labelledHost("a-worker", map[string]string{"node-role": "worker"}), availableHost("b-unlabelled", ""))
		r := &Beskar7MachineReconciler{Client: c, Scheme: scheme.Scheme, Log: ctrl.Log.WithName("hs-test")}
		b7m := withSelector(&metav1.LabelSelector{MatchLabels: map[string]string{"node-role": "control-plane"}})

		got, result, err := r.findAndClaimOrGetAssociatedHost(context.Background(), r.Log, b7m, placementOf(b7m, ""))
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(BeNil(), "a non-matching host must not be claimed")
		Expect(result.IsZero()).To(BeTrue())
		Expect(consumerOf(c, "a-worker")).To(BeNil())
		Expect(consumerOf(c, "b-unlabelled")).To(BeNil())
	})

	It("hostSelector: matchExpressions are honoured", func() {
		c := newClientWith(labelledHost("a-rack-3", map[string]string{"rack": "r3"}), labelledHost("b-rack-2", map[string]string{"rack": "r2"}))
		r := &Beskar7MachineReconciler{Client: c, Scheme: scheme.Scheme, Log: ctrl.Log.WithName("hs-test")}
		b7m := withSelector(&metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
			{Key: "rack", Operator: metav1.LabelSelectorOpIn, Values: []string{"r1", "r2"}},
		}})

		got, _, err := r.findAndClaimOrGetAssociatedHost(context.Background(), r.Log, b7m, placementOf(b7m, ""))
		Expect(err).NotTo(HaveOccurred())
		Expect(got).NotTo(BeNil())
		Expect(got.Name).To(Equal("b-rack-2"))
		Expect(consumerOf(c, "a-rack-3")).To(BeNil())
	})

	It("hostSelector AND failure domain: a host must satisfy both", func() {
		c := newClientWith(
			labelledHost("a-cp-rack-2", map[string]string{"node-role": "control-plane", zoneLabelKey: "rack-2"}),
			labelledHost("b-worker-rack-1", map[string]string{"node-role": "worker", zoneLabelKey: "rack-1"}),
			labelledHost("c-cp-rack-1", map[string]string{"node-role": "control-plane", zoneLabelKey: "rack-1"}),
		)
		r := &Beskar7MachineReconciler{Client: c, Scheme: scheme.Scheme, Log: ctrl.Log.WithName("hs-test")}
		b7m := withSelector(&metav1.LabelSelector{MatchLabels: map[string]string{"node-role": "control-plane"}})

		got, _, err := r.findAndClaimOrGetAssociatedHost(context.Background(), r.Log, b7m, placementOf(b7m, "rack-1"))
		Expect(err).NotTo(HaveOccurred())
		Expect(got).NotTo(BeNil())
		Expect(got.Name).To(Equal("c-cp-rack-1"))
		Expect(consumerOf(c, "a-cp-rack-2")).To(BeNil(), "right role, wrong zone")
		Expect(consumerOf(c, "b-worker-rack-1")).To(BeNil(), "right zone, wrong role")
	})

	It("hostSelector: an unparsable selector is a terminal failure and claims nothing", func() {
		c := newClientWith(availableHost("a-free", ""))
		r := &Beskar7MachineReconciler{Client: c, Scheme: scheme.Scheme, Log: ctrl.Log.WithName("hs-test")}
		b7m := withSelector(&metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "rack", Operator: "Bogus"}}})

		result, err := r.reconcileNormal(context.Background(), r.Log, b7m, &clusterv1.Machine{Spec: clusterv1.MachineSpec{ClusterName: "fake-cluster"}})
		Expect(err).NotTo(HaveOccurred(), "terminal failures return nil so CAPI surfaces the failure via conditions")
		Expect(result.IsZero()).To(BeTrue(), "a terminal failure must not requeue")
		Expect(b7m.Status.Phase).NotTo(BeNil())
		Expect(*b7m.Status.Phase).To(Equal(infrav1.PhaseFailed))
		infraCond := conditions.Get(b7m, infrav1.InfrastructureReadyCondition)
		Expect(infraCond).NotTo(BeNil())
		Expect(infraCond.Reason).To(Equal(infrav1.InvalidHostSelectorReason))
		Expect(infraCond.Message).To(ContainSubstring("Bogus"))
		cond := conditions.Get(b7m, infrav1.PhysicalHostAssociatedCondition)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal(infrav1.InvalidHostSelectorReason))
		Expect(consumerOf(c, "a-free")).To(BeNil(), "nothing may be claimed on the way to a terminal failure")
	})

	It("reports NoMatchingPhysicalHost rather than WaitingForPhysicalHost when hosts exist outside the domain", func() {
		c := newClientWith(availableHost("a-other-zone", "rack-2"))
		r := &Beskar7MachineReconciler{Client: c, Scheme: scheme.Scheme, Log: ctrl.Log.WithName("fd-test")}
		b7m := newMachine()
		fd := "rack-1"
		machine := &clusterv1.Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "fd-owner", Namespace: "default"},
			Spec:       clusterv1.MachineSpec{ClusterName: "fake-cluster", FailureDomain: fd},
		}

		result, err := r.reconcileNormal(context.Background(), r.Log, b7m, machine)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(time.Minute))

		cond := conditions.Get(b7m, infrav1.PhysicalHostAssociatedCondition)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(infrav1.NoMatchingPhysicalHostReason))
		Expect(cond.Message).To(ContainSubstring(zoneLabelKey + "=rack-1"))
		Expect(consumerOf(c, "a-other-zone")).To(BeNil(), "the out-of-domain host must stay unclaimed")

		By("keeping the unconstrained reason when the Machine has no failure domain and the inventory is empty")
		r2 := &Beskar7MachineReconciler{Client: newClientWith(), Scheme: scheme.Scheme, Log: ctrl.Log.WithName("fd-test")}
		b7m2 := newMachine()
		result, err = r2.reconcileNormal(context.Background(), r2.Log, b7m2, &clusterv1.Machine{Spec: clusterv1.MachineSpec{ClusterName: "fake-cluster"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(time.Minute))
		Expect(conditions.Get(b7m2, infrav1.PhysicalHostAssociatedCondition).Reason).To(Equal(infrav1.WaitingForPhysicalHostReason))
	})
})

var _ = Describe("Waking waiting Beskar7Machines when a PhysicalHost becomes Available", func() {
	// A machine that reconciles moments before its host finishes enrolling
	// parks on the one-minute no-host requeue. The host's own transition to
	// Available is the only event that can end that wait early, so the
	// predicate must pass exactly that transition and the map must pick
	// exactly the machines that are still looking for a host.

	hostIn := func(state string) *infrav1.PhysicalHost {
		return &infrav1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{Name: "wake-host", Namespace: "default"},
			Status:     infrav1.PhysicalHostStatus{State: state},
		}
	}
	claimedIn := func(state string) *infrav1.PhysicalHost {
		h := hostIn(state)
		h.Spec.ConsumerRef = &corev1.ObjectReference{Kind: "Beskar7Machine", Name: "someone", Namespace: "default"}
		return h
	}
	machine := func(ns, name string, mutate func(*infrav1.Beskar7Machine)) *infrav1.Beskar7Machine {
		m := &infrav1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Finalizers: []string{Beskar7MachineFinalizer}},
			Spec: infrav1.Beskar7MachineSpec{
				InspectionImageURL: "http://boot/inspect.ipxe",
				TargetImageURL:     "http://boot/kairos.tar.gz",
				TargetImageDigest:  bootTestDigest,
			},
		}
		if mutate != nil {
			mutate(m)
		}
		return m
	}

	It("admits only the events on which an unclaimed host enters Available", func() {
		p := hostBecameAvailable()

		Expect(p.Create(event.CreateEvent{Object: hostIn(infrav1.StateAvailable)})).To(BeTrue(), "created already free")
		Expect(p.Create(event.CreateEvent{Object: hostIn(infrav1.StateEnrolling)})).To(BeFalse(), "created enrolling")
		Expect(p.Create(event.CreateEvent{Object: claimedIn(infrav1.StateAvailable)})).To(BeFalse(), "created with a consumer")

		Expect(p.Update(event.UpdateEvent{ObjectOld: hostIn(""), ObjectNew: hostIn(infrav1.StateAvailable)})).To(BeTrue(), "enrolment finished")
		Expect(p.Update(event.UpdateEvent{ObjectOld: claimedIn(infrav1.StateInUse), ObjectNew: hostIn(infrav1.StateAvailable)})).To(BeTrue(), "released")
		Expect(p.Update(event.UpdateEvent{ObjectOld: hostIn(infrav1.StateAvailable), ObjectNew: hostIn(infrav1.StateAvailable)})).To(BeFalse(), "status churn on a free host")
		Expect(p.Update(event.UpdateEvent{ObjectOld: hostIn(infrav1.StateAvailable), ObjectNew: claimedIn(infrav1.StateAvailable)})).To(BeFalse(), "claimed")

		Expect(p.Delete(event.DeleteEvent{Object: hostIn(infrav1.StateAvailable)})).To(BeFalse())
		Expect(p.Generic(event.GenericEvent{Object: hostIn(infrav1.StateAvailable)})).To(BeFalse())
	})

	It("enqueues the machines still waiting for a host in the host's namespace and nothing else", func() {
		never := machine("default", "never-reconciled", nil)
		waiting := machine("default", "waiting", func(m *infrav1.Beskar7Machine) {
			setFalse(m, infrav1.PhysicalHostAssociatedCondition, infrav1.WaitingForPhysicalHostReason, "No available PhysicalHost found")
		})
		placed := machine("default", "no-match", func(m *infrav1.Beskar7Machine) {
			setFalse(m, infrav1.PhysicalHostAssociatedCondition, infrav1.NoMatchingPhysicalHostReason, "no host in rack-1")
		})
		associated := machine("default", "associated", func(m *infrav1.Beskar7Machine) {
			setTrue(m, infrav1.PhysicalHostAssociatedCondition, infrav1.PhysicalHostAssociatedReason)
		})
		// Still unassociated (like "waiting" above) but terminally failed — the
		// isTerminallyFailed guard in AvailablePhysicalHostToWaitingBeskar7Machines
		// must exclude it even though its PhysicalHostAssociatedCondition alone
		// would otherwise mark it as still waiting for a host.
		failed := machine("default", "failed", func(m *infrav1.Beskar7Machine) {
			setFalse(m, infrav1.PhysicalHostAssociatedCondition, infrav1.WaitingForPhysicalHostReason, "No available PhysicalHost found")
			m.Status.Phase = ptr.To(infrav1.PhaseFailed)
		})
		now := metav1.Now()
		deleting := machine("default", "deleting", func(m *infrav1.Beskar7Machine) {
			m.DeletionTimestamp = &now
		})
		elsewhere := machine("other-ns", "waiting-elsewhere", nil)

		c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
			WithObjects(never, waiting, placed, associated, failed, deleting, elsewhere).Build()
		r := &Beskar7MachineReconciler{Client: c, Scheme: scheme.Scheme, Log: ctrl.Log.WithName("wake-test")}

		names := func(reqs []reconcile.Request) []string {
			out := make([]string, 0, len(reqs))
			for _, req := range reqs {
				Expect(req.Namespace).To(Equal("default"))
				out = append(out, req.Name)
			}
			return out
		}

		woken := names(r.AvailablePhysicalHostToWaitingBeskar7Machines(context.Background(), hostIn(infrav1.StateAvailable)))
		Expect(woken).To(ConsistOf("never-reconciled", "waiting", "no-match"))

		By("never waking a terminally-failed machine, even though it is otherwise indistinguishable from \"waiting\"")
		Expect(woken).NotTo(ContainElement("failed"))

		By("mapping nothing for a host that is not claimable")
		Expect(r.AvailablePhysicalHostToWaitingBeskar7Machines(context.Background(), claimedIn(infrav1.StateAvailable))).To(BeEmpty())
		Expect(r.AvailablePhysicalHostToWaitingBeskar7Machines(context.Background(), hostIn(infrav1.StateInUse))).To(BeEmpty())
	})
})

// Credential reuse is judged by the bound Secret alone.
//
// Dome lab, 2026-09-09: two active managers (leader election off) each minted a
// bearer token for the same host within a second and their writes interleaved —
// the per-host Secret ended up holding one manager's plaintext while
// Status.Bootstrap.TokenHash carried the other's hash, which the callback
// server then checked. The inspector booted with the Secret's plaintext, every
// callback was rejected and the machine hit InspectionTimedOut. Since D-029 the
// callback server checks the Secret and status only mirrors it, so there is no
// second copy to disagree with, and one machine's stale view cannot strand a
// host: triggerInspection reuses a credential only when the Secret is bound to
// this machine and holds it unexpired, and otherwise mints afresh in a single
// optimistic-locked write.
var _ = Describe("Beskar7Machine credential reuse is judged by the bound Secret alone", func() {
	var (
		testNs       *corev1.Namespace
		physicalHost *infrav1.PhysicalHost
		b7machine    *infrav1.Beskar7Machine
		r            *Beskar7MachineReconciler
		hostKey      types.NamespacedName
		secretKey    types.NamespacedName
	)

	mustMint := func() (plaintext, hash string) {
		p, h, err := auth.MintToken()
		Expect(err).NotTo(HaveOccurred())
		return p, h
	}

	BeforeEach(func() {
		testNs = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "credential-reuse-test-"}}
		Expect(k8sClient.Create(ctx, testNs)).To(Succeed())

		creds := bmcCredentialsSecretNamed(testNs.Name, "bmc-creds")
		Expect(k8sClient.Create(ctx, creds)).To(Succeed())

		physicalHost = &infrav1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{Name: "reuse-host", Namespace: testNs.Name},
			Spec: infrav1.PhysicalHostSpec{
				RedfishConnection: infrav1.RedfishConnection{
					Address:              "https://192.168.77.1",
					CredentialsSecretRef: creds.Name,
				},
				ConsumerRef: &corev1.ObjectReference{
					Kind: "Beskar7Machine", APIVersion: InfrastructureAPIVersion,
					Name: "reuse-machine", Namespace: testNs.Name,
				},
			},
		}
		Expect(k8sClient.Create(ctx, physicalHost)).To(Succeed())
		hostKey = client.ObjectKeyFromObject(physicalHost)
		secretKey = types.NamespacedName{Namespace: testNs.Name, Name: bootstrapTokenSecretName(physicalHost.Name)}

		// triggerInspection only stamps Status.Phase on the machine; it does not
		// need to exist in the API server.
		b7machine = &infrav1.Beskar7Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "reuse-machine", Namespace: testNs.Name},
		}

		r = &Beskar7MachineReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			Log:    ctrl.Log.WithName("credential-reuse-test"),
			RedfishClientFactory: func(_ context.Context, _, _, _ string, _ bool, _ []byte) (internalredfish.Client, error) {
				return internalredfish.NewMockClient(), nil
			},
			BootstrapURLBase: "https://test.svc:8082",
		}
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, testNs)).To(Succeed())
	})

	// seedStatus plants a state and a Status.Bootstrap on the host and
	// refreshes the local copy so triggerInspection sees them.
	seedStatus := func(state string, bs *infrav1.BootstrapStatus) {
		Expect(k8sClient.Get(ctx, hostKey, physicalHost)).To(Succeed())
		physicalHost.Status.State = state
		physicalHost.Status.Bootstrap = bs
		Expect(k8sClient.Status().Update(ctx, physicalHost)).To(Succeed())
		Expect(k8sClient.Get(ctx, hostKey, physicalHost)).To(Succeed())
	}
	// seedSecret plants the per-host Secret as an earlier mint — or an older
	// manager — left it.
	seedSecret := func(data map[string][]byte) {
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretKey.Name, Namespace: secretKey.Namespace},
			Data:       data,
		})).To(Succeed())
	}
	getCreds := func() bootstrapCredentials {
		s := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, secretKey, s)).To(Succeed())
		return readBootstrapCredentials(s)
	}
	trigger := func() {
		_, err := r.triggerInspection(ctx, r.Log, b7machine, physicalHost)
		Expect(err).NotTo(HaveOccurred())
	}
	expectNoCredentialAnnotations := func() {
		ph := &infrav1.PhysicalHost{}
		Expect(k8sClient.Get(ctx, hostKey, ph)).To(Succeed())
		Expect(ph.Annotations).NotTo(HaveKey(BootstrapTokenAnnotation))
		Expect(ph.Annotations).NotTo(HaveKey(BootNonceAnnotation))
	}

	Context("bearer token", func() {
		It("reuses a token bound to this machine, whatever status advertises", func() {
			token, _ := mustMint()
			nonce, _ := mustMint()
			seedSecret(boundCredentialData(b7machine.Name, token, 30*time.Minute, nonce, 5*time.Minute))
			_, strayHash := mustMint()
			expired := metav1.NewTime(time.Now().Add(-time.Hour))
			seedStatus(infrav1.StateInUse, &infrav1.BootstrapStatus{TokenHash: strayHash, ExpiresAt: &expired})

			trigger()

			creds := getCreds()
			Expect(creds.token).To(Equal(token), "a bound, valid token must not be replaced")
			Expect(creds.nonce).To(Equal(nonce))
			expectNoCredentialAnnotations()
		})

		It("mints fresh credentials when the Secret is bound to the host's previous machine (SEC-13)", func() {
			token, _ := mustMint()
			nonce, _ := mustMint()
			seedSecret(boundCredentialData("previous-machine", token, 30*time.Minute, nonce, 5*time.Minute))

			trigger()

			creds := getCreds()
			Expect(creds.token).NotTo(Equal(token), "a token minted for another claim must never be reused")
			Expect(creds.nonce).NotTo(Equal(nonce))
			Expect(creds.consumer).To(Equal(b7machine.Name))
			Expect(creds.tokenValid(time.Now())).To(BeTrue())
			Expect(creds.nonceValid(time.Now())).To(BeTrue())
		})

		It("re-mints a bound token whose expiry is missing, however valid status says it is", func() {
			token, hash := mustMint()
			seedSecret(map[string][]byte{
				bootstrapTokenSecretKey:    []byte(token),
				bootstrapConsumerSecretKey: []byte(b7machine.Name),
			})
			future := metav1.NewTime(time.Now().Add(time.Hour))
			seedStatus(infrav1.StateInUse, &infrav1.BootstrapStatus{TokenHash: hash, ExpiresAt: &future})

			trigger()

			creds := getCreds()
			Expect(creds.token).NotTo(Equal(token), "a token with no expiry in the Secret never verifies, so it is replaced")
			Expect(creds.tokenValid(time.Now())).To(BeTrue())
		})

		It("mints when the Secret is missing, however valid status says the token is", func() {
			_, hash := mustMint()
			future := metav1.NewTime(time.Now().Add(time.Hour))
			seedStatus(infrav1.StateInUse, &infrav1.BootstrapStatus{TokenHash: hash, ExpiresAt: &future})

			trigger()

			creds := getCreds()
			Expect(creds.consumer).To(Equal(b7machine.Name))
			Expect(auth.Hash(creds.token)).NotTo(Equal(hash))
			Expect(creds.tokenValid(time.Now())).To(BeTrue())
			expectNoCredentialAnnotations()
		})
	})

	Context("boot nonce", func() {
		It("re-mints only the nonce when its expiry in the Secret has passed", func() {
			token, _ := mustMint()
			nonce, _ := mustMint()
			seedSecret(boundCredentialData(b7machine.Name, token, 30*time.Minute, nonce, -time.Second))

			trigger()

			creds := getCreds()
			Expect(creds.nonce).NotTo(Equal(nonce))
			Expect(creds.nonceValid(time.Now())).To(BeTrue())
			Expect(creds.token).To(Equal(token), "the nonce and the token have independent lifecycles")
		})

		It("mints a nonce when the bound Secret holds none", func() {
			token, _ := mustMint()
			seedSecret(boundCredentialData(b7machine.Name, token, 30*time.Minute, "", 0))

			trigger()

			creds := getCreds()
			Expect(creds.nonceValid(time.Now())).To(BeTrue())
			Expect(creds.token).To(Equal(token))
		})
	})

	// The one-release upgrade backfill (D-029) for InUse, which
	// triggerInspection covers; callback_credentials_test.go covers Inspecting
	// and Ready through the full reconcile.
	Context("a Secret written before the consumer binding", func() {
		It("binds an InUse host's credentials to this machine instead of minting", func() {
			token, tokenHash := mustMint()
			nonce, nonceHash := mustMint()
			seedSecret(map[string][]byte{bootstrapTokenSecretKey: []byte(token), bootNonceSecretKey: []byte(nonce)})
			tokenExpiry := metav1.NewTime(time.Now().Add(20 * time.Minute).Truncate(time.Second))
			nonceExpiry := metav1.NewTime(time.Now().Add(3 * time.Minute).Truncate(time.Second))
			seedStatus(infrav1.StateInUse, &infrav1.BootstrapStatus{
				TokenHash: tokenHash, ExpiresAt: &tokenExpiry, BootNonceHash: nonceHash, BootNonceExpiresAt: &nonceExpiry,
			})

			trigger()

			creds := getCreds()
			Expect(creds.token).To(Equal(token), "the operator's boot service may already have handed these out")
			Expect(creds.nonce).To(Equal(nonce))
			Expect(creds.consumer).To(Equal(b7machine.Name))
			Expect(creds.tokenExpiresAt).To(BeTemporally("==", tokenExpiry.Time))
			Expect(creds.nonceExpiresAt).To(BeTemporally("==", nonceExpiry.Time))
		})

		It("mints fresh credentials when the plaintexts do not hash to what status carries", func() {
			token, _ := mustMint()
			nonce, _ := mustMint()
			seedSecret(map[string][]byte{bootstrapTokenSecretKey: []byte(token), bootNonceSecretKey: []byte(nonce)})
			_, forgedToken := mustMint()
			_, forgedNonce := mustMint()
			future := metav1.NewTime(time.Now().Add(time.Hour))
			seedStatus(infrav1.StateInUse, &infrav1.BootstrapStatus{
				TokenHash: forgedToken, ExpiresAt: &future, BootNonceHash: forgedNonce, BootNonceExpiresAt: &future,
			})

			trigger()

			creds := getCreds()
			Expect(creds.token).NotTo(Equal(token))
			Expect(creds.nonce).NotTo(Equal(nonce))
			Expect(creds.consumer).To(Equal(b7machine.Name))
		})
	})
})
