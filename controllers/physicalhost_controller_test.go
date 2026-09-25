package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/stmcginnis/gofish/schemas"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
	"github.com/projectbeskar/beskar7/internal/auth"
	internalredfish "github.com/projectbeskar/beskar7/internal/redfish"
)

var _ = Describe("PhysicalHostReconciler factory defaulting", func() {
	It("should default RedfishClientFactory to internalredfish.NewClient when nil", func() {
		r := &PhysicalHostReconciler{}
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
		r := &PhysicalHostReconciler{RedfishClientFactory: sentinel}

		Expect(r.defaultFactory()).To(Succeed())

		// Pointer equality is not directly comparable for func types in Go; verify
		// the factory is still the one we set by calling it and checking the result type.
		client, err := r.RedfishClientFactory(ctx, "", "", "", false, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(client).To(BeAssignableToTypeOf(&internalredfish.MockClient{}))
	})
})

var _ = Describe("PhysicalHost Controller", func() {

	const (
		Timeout  = time.Second * 10
		Interval = time.Millisecond * 250
	)

	// Helper function to reconcile with timeout context
	reconcileWithTimeout := func(reconciler *PhysicalHostReconciler, phLookupKey types.NamespacedName) (ctrl.Result, error) {
		reconcileCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return reconciler.Reconcile(reconcileCtx, ctrl.Request{NamespacedName: phLookupKey})
	}

	Context("When reconciling a PhysicalHost", func() {
		var physicalHost *infrav1.PhysicalHost
		var credentialSecret *corev1.Secret
		var mockRfClient *internalredfish.MockClient
		var reconciler *PhysicalHostReconciler
		var testNs *corev1.Namespace

		BeforeEach(func() {
			// Create a unique namespace for this test
			testNs = &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "physicalhost-test-",
				},
			}
			Expect(k8sClient.Create(ctx, testNs)).To(Succeed())

			// Create the credential secret
			credentialSecret = bmcCredentialsSecretNamed(testNs.Name, "test-redfish-credentials")
			Expect(k8sClient.Create(ctx, credentialSecret)).To(Succeed())

			// Define the PhysicalHost resource
			physicalHost = &infrav1.PhysicalHost{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-physicalhost",
					Namespace: testNs.Name,
				},
				Spec: infrav1.PhysicalHostSpec{
					RedfishConnection: infrav1.RedfishConnection{
						Address:              "https://redfish-mock.example.com",
						CredentialsSecretRef: credentialSecret.Name,
					},
				},
			}

			// Create Mock Redfish Client
			mockRfClient = internalredfish.NewMockClient()

			// Create the reconciler instance for the test
			reconciler = &PhysicalHostReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Log:      ctrl.Log.WithName("physicalhost-test"),
				Recorder: record.NewFakeRecorder(100),
				RedfishClientFactory: func(ctx context.Context, address, username, password string, insecure bool, caBundle []byte) (internalredfish.Client, error) {
					return mockRfClient, nil
				},
			}
		})

		AfterEach(func() {
			// Clean up the namespace
			Expect(k8sClient.Delete(ctx, testNs)).To(Succeed())
		})

		It("Should successfully reconcile and become Available", func() {
			By("Creating the PhysicalHost resource")
			Expect(k8sClient.Create(ctx, physicalHost)).To(Succeed())

			phLookupKey := types.NamespacedName{Name: physicalHost.Name, Namespace: physicalHost.Namespace}

			By("Reconciling to add finalizer")
			_, err := reconcileWithTimeout(reconciler, phLookupKey)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				createdPh := &infrav1.PhysicalHost{}
				g.Expect(k8sClient.Get(ctx, phLookupKey, createdPh)).To(Succeed())
				g.Expect(createdPh.Finalizers).To(ContainElement(PhysicalHostFinalizer))
			}, Timeout, Interval).Should(Succeed())

			By("Reconciling again to transition to Available")
			_, err = reconcileWithTimeout(reconciler, phLookupKey)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				createdPh := &infrav1.PhysicalHost{}
				g.Expect(k8sClient.Get(ctx, phLookupKey, createdPh)).To(Succeed())
				g.Expect(createdPh.Status.State).To(Equal(infrav1.StateAvailable))
				g.Expect(createdPh.Status.ObservedPowerState).To(Equal(string(schemas.OffPowerState)))
				g.Expect(createdPh.Status.HardwareDetails).NotTo(BeNil())
				g.Expect(conditions.IsTrue(createdPh, infrav1.RedfishConnectionReadyCondition)).To(BeTrue())
				g.Expect(conditions.IsTrue(createdPh, infrav1.HostAvailableCondition)).To(BeTrue())
			}, Timeout, Interval).Should(Succeed())

			// Verify mock client methods were called
			Expect(mockRfClient.GetSystemInfoCalled).To(BeTrue())
			Expect(mockRfClient.GetPowerStateCalled).To(BeTrue())
		})

		// Converted from PIt: verifies that the patch-helper deferred finalizer add and
		// remove are idempotent and do not cause spurious API conflicts.
		It("Should add finalizer via patch on first reconcile and remove it on delete", func() {
			By("Creating the PhysicalHost resource")
			Expect(k8sClient.Create(ctx, physicalHost)).To(Succeed())

			phLookupKey := types.NamespacedName{Name: physicalHost.Name, Namespace: physicalHost.Namespace}

			By("First reconcile adds finalizer through deferred patch")
			_, err := reconcileWithTimeout(reconciler, phLookupKey)
			Expect(err).NotTo(HaveOccurred())

			ph := &infrav1.PhysicalHost{}
			Expect(k8sClient.Get(ctx, phLookupKey, ph)).To(Succeed())
			Expect(ph.Finalizers).To(ContainElement(PhysicalHostFinalizer),
				"finalizer must be present after first reconcile")

			By("Second reconcile (status update) is idempotent — no conflict expected")
			_, err = reconcileWithTimeout(reconciler, phLookupKey)
			Expect(err).NotTo(HaveOccurred())

			ph2 := &infrav1.PhysicalHost{}
			Expect(k8sClient.Get(ctx, phLookupKey, ph2)).To(Succeed())
			Expect(ph2.Finalizers).To(ContainElement(PhysicalHostFinalizer),
				"finalizer must still be present after second reconcile")
			Expect(ph2.Status.State).To(Equal(infrav1.StateAvailable),
				"status must be persisted by the deferred patch")

			By("Deleting the PhysicalHost")
			Expect(k8sClient.Delete(ctx, physicalHost)).To(Succeed())

			By("Reconciling to handle deletion — finalizer removed via patch")
			_, err = reconcileWithTimeout(reconciler, phLookupKey)
			Expect(err).NotTo(HaveOccurred())

			By("Ensuring PhysicalHost is eventually deleted (finalizer gone)")
			Eventually(func() bool {
				ph := &infrav1.PhysicalHost{}
				errGet := k8sClient.Get(ctx, phLookupKey, ph)
				return client.IgnoreNotFound(errGet) == nil
			}, Timeout*2, Interval).Should(BeTrue())
		})

		// Converted from PIt "[SKIP - Hardware Testing] Should handle inspection phase transitions":
		// The original test directly mutated PhysicalHost.Status which is no longer valid.
		// This version verifies the annotation-based inspection signalling introduced by PR-2.1:
		// when the InspectionRequestAnnotation is set to "inspect", the reconciler transitions
		// state and clears the annotation; "inspect-complete" transitions to StateDeploying (D-015).
		It("Should apply inspection-request annotation and drive state transitions", func() {
			By("Creating the PhysicalHost resource and making it Available")
			Expect(k8sClient.Create(ctx, physicalHost)).To(Succeed())

			phLookupKey := types.NamespacedName{Name: physicalHost.Name, Namespace: physicalHost.Namespace}

			// Two reconciles: first adds finalizer, second drives to Available.
			_, err := reconcileWithTimeout(reconciler, phLookupKey)
			Expect(err).NotTo(HaveOccurred())
			_, err = reconcileWithTimeout(reconciler, phLookupKey)
			Expect(err).NotTo(HaveOccurred())

			ph := &infrav1.PhysicalHost{}
			Expect(k8sClient.Get(ctx, phLookupKey, ph)).To(Succeed())
			Expect(ph.Status.State).To(Equal(infrav1.StateAvailable))

			By("Setting ConsumerRef and inspect annotation (as Beskar7Machine controller would)")
			phPatch := ph.DeepCopy()
			if phPatch.Annotations == nil {
				phPatch.Annotations = map[string]string{}
			}
			phPatch.Annotations[InspectionRequestAnnotation] = "inspect"
			phPatch.Spec.ConsumerRef = &corev1.ObjectReference{
				Kind:       "Beskar7Machine",
				APIVersion: InfrastructureAPIVersion,
				Name:       "test-machine",
				Namespace:  ph.Namespace,
			}
			Expect(k8sClient.Patch(ctx, phPatch, client.MergeFrom(ph))).To(Succeed())

			By("Reconciling — controller should consume annotation and transition to Inspecting")
			_, err = reconcileWithTimeout(reconciler, phLookupKey)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				got := &infrav1.PhysicalHost{}
				g.Expect(k8sClient.Get(ctx, phLookupKey, got)).To(Succeed())
				g.Expect(got.Status.State).To(Equal(infrav1.StateInspecting))
				g.Expect(got.Status.InspectionPhase).To(Equal(infrav1.InspectionPhaseBooting))
				g.Expect(got.Status.InspectionTimestamp).NotTo(BeNil())
				// The claim and the inspect request arrived in one patch, so the host
				// went Available -> Inspecting without ever being InUse. HostAvailable
				// must still read False: it follows the claim, not the InUse edge.
				g.Expect(conditions.IsFalse(got, infrav1.HostAvailableCondition)).To(BeTrue(),
					"a claimed host must not advertise HostAvailable=True, even one that skipped InUse")
				// Annotation must be cleared so it is not acted on again.
				g.Expect(got.Annotations).NotTo(HaveKey(InspectionRequestAnnotation))
			}, Timeout, Interval).Should(Succeed())

			By("Setting inspect-complete annotation (as Beskar7Machine controller would after validation)")
			ph2 := &infrav1.PhysicalHost{}
			Expect(k8sClient.Get(ctx, phLookupKey, ph2)).To(Succeed())
			ph2Patch := ph2.DeepCopy()
			if ph2Patch.Annotations == nil {
				ph2Patch.Annotations = map[string]string{}
			}
			ph2Patch.Annotations[InspectionRequestAnnotation] = "inspect-complete"
			Expect(k8sClient.Patch(ctx, ph2Patch, client.MergeFrom(ph2))).To(Succeed())

			// D-015: inspect-complete now transitions to StateDeploying, not StateReady.
			// StateReady is only reached after the provisioned callback.
			By("Reconciling — controller should transition to StateDeploying (D-015)")
			_, err = reconcileWithTimeout(reconciler, phLookupKey)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				got := &infrav1.PhysicalHost{}
				g.Expect(k8sClient.Get(ctx, phLookupKey, got)).To(Succeed())
				g.Expect(got.Status.State).To(Equal(infrav1.StateDeploying),
					"inspect-complete must land in Deploying, not Ready (D-015)")
				g.Expect(conditions.IsTrue(got, infrav1.HostInspectedCondition)).To(BeTrue())
				g.Expect(got.Annotations).NotTo(HaveKey(InspectionRequestAnnotation))
				g.Expect(got.Status.DeployingTimestamp).NotTo(BeNil(),
					"DeployingTimestamp must be set on Deploying entry")
			}, Timeout, Interval).Should(Succeed())
		})

		// A PhysicalHost must be reusable. Before this, InspectionTimestamp and
		// DeployingTimestamp were only ever set (guarded by `== nil`) and never
		// cleared, so a released host kept the previous run's clock. The next
		// Beskar7Machine to claim it computed time.Since(InspectionTimestamp)
		// against that stale value and was marked terminally failed with
		// InspectionTimedOut within seconds — breaking MachineDeployment replica
		// replacement, cluster rebuild on the same hardware, and MHC remediation.
		It("Should clear the previous run's state when released, so the host can be reused", func() {
			Expect(k8sClient.Create(ctx, physicalHost)).To(Succeed())
			phLookupKey := types.NamespacedName{Name: physicalHost.Name, Namespace: physicalHost.Namespace}

			_, err := reconcileWithTimeout(reconciler, phLookupKey)
			Expect(err).NotTo(HaveOccurred())
			_, err = reconcileWithTimeout(reconciler, phLookupKey)
			Expect(err).NotTo(HaveOccurred())

			By("Driving a full provisioning run: claim -> Inspecting -> Deploying")
			ph := &infrav1.PhysicalHost{}
			Expect(k8sClient.Get(ctx, phLookupKey, ph)).To(Succeed())
			phPatch := ph.DeepCopy()
			if phPatch.Annotations == nil {
				phPatch.Annotations = map[string]string{}
			}
			phPatch.Annotations[InspectionRequestAnnotation] = "inspect"
			phPatch.Spec.ConsumerRef = &corev1.ObjectReference{
				Kind:       "Beskar7Machine",
				APIVersion: InfrastructureAPIVersion,
				Name:       "first-consumer",
				Namespace:  ph.Namespace,
			}
			Expect(k8sClient.Patch(ctx, phPatch, client.MergeFrom(ph))).To(Succeed())
			_, err = reconcileWithTimeout(reconciler, phLookupKey)
			Expect(err).NotTo(HaveOccurred())

			ph2 := &infrav1.PhysicalHost{}
			Expect(k8sClient.Get(ctx, phLookupKey, ph2)).To(Succeed())
			ph2Patch := ph2.DeepCopy()
			if ph2Patch.Annotations == nil {
				ph2Patch.Annotations = map[string]string{}
			}
			ph2Patch.Annotations[InspectionRequestAnnotation] = "inspect-complete"
			Expect(k8sClient.Patch(ctx, ph2Patch, client.MergeFrom(ph2))).To(Succeed())
			_, err = reconcileWithTimeout(reconciler, phLookupKey)
			Expect(err).NotTo(HaveOccurred())

			By("Confirming the run really did leave state behind")
			Eventually(func(g Gomega) {
				got := &infrav1.PhysicalHost{}
				g.Expect(k8sClient.Get(ctx, phLookupKey, got)).To(Succeed())
				g.Expect(got.Status.State).To(Equal(infrav1.StateDeploying))
				g.Expect(got.Status.InspectionTimestamp).NotTo(BeNil())
				g.Expect(got.Status.DeployingTimestamp).NotTo(BeNil())
			}, Timeout, Interval).Should(Succeed())

			By("Releasing the host, as Beskar7Machine does on delete: ConsumerRef = nil")
			released := &infrav1.PhysicalHost{}
			Expect(k8sClient.Get(ctx, phLookupKey, released)).To(Succeed())
			relPatch := released.DeepCopy()
			relPatch.Spec.ConsumerRef = nil
			Expect(k8sClient.Patch(ctx, relPatch, client.MergeFrom(released))).To(Succeed())

			_, err = reconcileWithTimeout(reconciler, phLookupKey)
			Expect(err).NotTo(HaveOccurred())

			By("The released host must carry NOTHING from the previous run")
			Eventually(func(g Gomega) {
				got := &infrav1.PhysicalHost{}
				g.Expect(k8sClient.Get(ctx, phLookupKey, got)).To(Succeed())
				g.Expect(got.Status.State).To(Equal(infrav1.StateAvailable))

				// The regression: these two drive the inspection and deployment
				// timeouts in the Beskar7Machine controller. A stale value fails the
				// next consumer instantly.
				g.Expect(got.Status.InspectionTimestamp).To(BeNil(),
					"InspectionTimestamp must not survive release — the next consumer's inspection timeout is measured from it")
				g.Expect(got.Status.DeployingTimestamp).To(BeNil(),
					"DeployingTimestamp must not survive release — the next consumer's deployment timeout is measured from it")

				g.Expect(got.Status.InspectionPhase).To(BeEmpty(),
					"InspectionPhase describes the finished run")
				g.Expect(conditions.IsTrue(got, infrav1.HostInspectedCondition)).To(BeFalse(),
					"HostInspected describes the finished run, not the hardware")
				g.Expect(conditions.IsTrue(got, infrav1.HostAvailableCondition)).To(BeTrue(),
					"a released host is available again")
			}, Timeout, Interval).Should(Succeed())

			By("A second consumer can claim it and start a fresh inspection")
			reclaim := &infrav1.PhysicalHost{}
			Expect(k8sClient.Get(ctx, phLookupKey, reclaim)).To(Succeed())
			reclaimPatch := reclaim.DeepCopy()
			if reclaimPatch.Annotations == nil {
				reclaimPatch.Annotations = map[string]string{}
			}
			reclaimPatch.Annotations[InspectionRequestAnnotation] = "inspect"
			reclaimPatch.Spec.ConsumerRef = &corev1.ObjectReference{
				Kind:       "Beskar7Machine",
				APIVersion: InfrastructureAPIVersion,
				Name:       "second-consumer",
				Namespace:  reclaim.Namespace,
			}
			Expect(k8sClient.Patch(ctx, reclaimPatch, client.MergeFrom(reclaim))).To(Succeed())
			_, err = reconcileWithTimeout(reconciler, phLookupKey)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				got := &infrav1.PhysicalHost{}
				g.Expect(k8sClient.Get(ctx, phLookupKey, got)).To(Succeed())
				g.Expect(got.Status.State).To(Equal(infrav1.StateInspecting))
				g.Expect(got.Status.InspectionTimestamp).NotTo(BeNil())
				// The whole point: the second consumer's clock starts now, not when
				// the first consumer's run began.
				g.Expect(got.Status.InspectionTimestamp.Time).To(BeTemporally("~", time.Now(), 2*time.Minute),
					"the reclaimed host's inspection clock must start at the new claim")
			}, Timeout, Interval).Should(Succeed())
		})

		// HostAvailable is a condition, not an event: it has to read False for
		// as long as a consumer holds the host and True again once released.
		// The controller used to set it True on the transition into Available
		// and never flip it back, so a host that was InUse, Inspecting,
		// Deploying or Ready kept advertising HostAvailable=True.
		It("Should hold HostAvailable=False while the host is claimed and restore True on release", func() {
			By("Creating the PhysicalHost resource and making it Available")
			Expect(k8sClient.Create(ctx, physicalHost)).To(Succeed())

			phLookupKey := types.NamespacedName{Name: physicalHost.Name, Namespace: physicalHost.Namespace}

			_, err := reconcileWithTimeout(reconciler, phLookupKey)
			Expect(err).NotTo(HaveOccurred())
			_, err = reconcileWithTimeout(reconciler, phLookupKey)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				got := &infrav1.PhysicalHost{}
				g.Expect(k8sClient.Get(ctx, phLookupKey, got)).To(Succeed())
				g.Expect(got.Status.State).To(Equal(infrav1.StateAvailable))
				g.Expect(conditions.IsTrue(got, infrav1.HostAvailableCondition)).To(BeTrue())
			}, Timeout, Interval).Should(Succeed())

			By("Claiming the host with a bare ConsumerRef, so it lands in InUse")
			ph := &infrav1.PhysicalHost{}
			Expect(k8sClient.Get(ctx, phLookupKey, ph)).To(Succeed())
			claim := ph.DeepCopy()
			claim.Spec.ConsumerRef = &corev1.ObjectReference{
				Kind:       "Beskar7Machine",
				APIVersion: InfrastructureAPIVersion,
				Name:       "claimant",
				Namespace:  ph.Namespace,
			}
			Expect(k8sClient.Patch(ctx, claim, client.MergeFrom(ph))).To(Succeed())
			_, err = reconcileWithTimeout(reconciler, phLookupKey)
			Expect(err).NotTo(HaveOccurred())

			var claimedAt time.Time
			Eventually(func(g Gomega) {
				got := &infrav1.PhysicalHost{}
				g.Expect(k8sClient.Get(ctx, phLookupKey, got)).To(Succeed())
				g.Expect(got.Status.State).To(Equal(infrav1.StateInUse))
				cond := conditions.Get(got, infrav1.HostAvailableCondition)
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(Equal(metav1.ConditionFalse),
					"a claimed host must not advertise HostAvailable=True")
				g.Expect(cond.Reason).To(Equal(infrav1.HostClaimedReason))
				g.Expect(cond.Message).To(ContainSubstring("claimant"))
				claimedAt = cond.LastTransitionTime.Time
			}, Timeout, Interval).Should(Succeed())

			By("Reconciling again while still claimed: the condition is re-asserted, not re-transitioned")
			_, err = reconcileWithTimeout(reconciler, phLookupKey)
			Expect(err).NotTo(HaveOccurred())

			stillClaimed := &infrav1.PhysicalHost{}
			Expect(k8sClient.Get(ctx, phLookupKey, stillClaimed)).To(Succeed())
			cond := conditions.Get(stillClaimed, infrav1.HostAvailableCondition)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.LastTransitionTime.Time).To(BeTemporally("==", claimedAt),
				"re-asserting an unchanged status must not move lastTransitionTime")

			By("Releasing the host: ConsumerRef = nil")
			released := &infrav1.PhysicalHost{}
			Expect(k8sClient.Get(ctx, phLookupKey, released)).To(Succeed())
			relPatch := released.DeepCopy()
			relPatch.Spec.ConsumerRef = nil
			Expect(k8sClient.Patch(ctx, relPatch, client.MergeFrom(released))).To(Succeed())
			_, err = reconcileWithTimeout(reconciler, phLookupKey)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				got := &infrav1.PhysicalHost{}
				g.Expect(k8sClient.Get(ctx, phLookupKey, got)).To(Succeed())
				g.Expect(got.Status.State).To(Equal(infrav1.StateAvailable))
				cond := conditions.Get(got, infrav1.HostAvailableCondition)
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(Equal(metav1.ConditionTrue),
					"a released host is available again")
				g.Expect(cond.Reason).To(Equal(infrav1.HostAvailableReason))
			}, Timeout, Interval).Should(Succeed())
		})

		It("Should handle deletion gracefully", func() {
			By("Creating the PhysicalHost resource")
			Expect(k8sClient.Create(ctx, physicalHost)).To(Succeed())

			phLookupKey := types.NamespacedName{Name: physicalHost.Name, Namespace: physicalHost.Namespace}

			By("Making host Available")
			_, err := reconcileWithTimeout(reconciler, phLookupKey)
			Expect(err).NotTo(HaveOccurred())
			_, err = reconcileWithTimeout(reconciler, phLookupKey)
			Expect(err).NotTo(HaveOccurred())

			By("Deleting the PhysicalHost")
			Expect(k8sClient.Delete(ctx, physicalHost)).To(Succeed())

			By("Reconciling to handle deletion")
			_, err = reconcileWithTimeout(reconciler, phLookupKey)
			Expect(err).NotTo(HaveOccurred())

			By("Ensuring PhysicalHost is eventually deleted")
			Eventually(func() bool {
				ph := &infrav1.PhysicalHost{}
				errGet := k8sClient.Get(ctx, phLookupKey, ph)
				return client.IgnoreNotFound(errGet) == nil
			}, Timeout*2, Interval).Should(BeTrue())
		})

		It("Should hand a non-network Redfish failure to the workqueue's exponential backoff", func() {
			// A plain error is not a network-level failure (see
			// internalredfish.IsTransientConnectionError); that path returns a
			// flat RequeueAfter instead and is covered in
			// physicalhost_transient_retry_test.go.
			By("Creating reconciler that fails connection")
			failedReconciler := &PhysicalHostReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Log:      ctrl.Log.WithName("physicalhost-test-failed"),
				Recorder: record.NewFakeRecorder(100),
				RedfishClientFactory: func(ctx context.Context, address, username, password string, insecure bool, caBundle []byte) (internalredfish.Client, error) {
					return nil, fmt.Errorf("connection timeout")
				},
			}

			failedPh := physicalHost.DeepCopy()
			failedPh.Name = "failed-connection"
			Expect(k8sClient.Create(ctx, failedPh)).To(Succeed())

			phLookupKey := types.NamespacedName{Name: failedPh.Name, Namespace: failedPh.Namespace}

			By("Reconciling with connection failure")
			_, err := failedReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: phLookupKey})
			Expect(err).NotTo(HaveOccurred()) // First reconcile adds finalizer

			result, err := failedReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: phLookupKey})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("connection timeout"))

			By("Asserting error paths defer requeue cadence to the workqueue rate-limiter (BUG-14)")
			// The error returned alongside Result{} signals controller-runtime to
			// requeue via the configured exponential rate-limiter (set in
			// SetupWithManager). An explicit RequeueAfter here would override the
			// rate-limiter and pin a misconfigured BMC to a fixed 60s ping forever.
			Expect(result.RequeueAfter).To(BeZero(), "non-network Redfish failure path must not set RequeueAfter; the workqueue rate-limiter governs the retry cadence")

			By("Checking error conditions")
			Eventually(func(g Gomega) {
				Expect(k8sClient.Get(ctx, phLookupKey, failedPh)).To(Succeed())
				cond := conditions.Get(failedPh, infrav1.RedfishConnectionReadyCondition)
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
				g.Expect(failedPh.Status.State).To(Equal(infrav1.StateError))
			}, Timeout, Interval).Should(Succeed())
		})

		It("Should reject InsecureSkipVerify=true combined with CABundleSecretRef terminally", func() {
			By("Creating a host with the conflicting TLS configuration")
			insecure := true
			conflictPh := physicalHost.DeepCopy()
			conflictPh.Name = "tls-conflict-host"
			conflictPh.Spec.RedfishConnection.InsecureSkipVerify = &insecure
			conflictPh.Spec.RedfishConnection.CABundleSecretRef = "irrelevant-bundle"

			// Build a reconciler whose factory would panic if invoked — we want to
			// prove the conflict gate fires BEFORE the factory.
			factoryCalled := false
			conflictReconciler := &PhysicalHostReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Log:      ctrl.Log.WithName("physicalhost-test-tls-conflict"),
				Recorder: record.NewFakeRecorder(100),
				RedfishClientFactory: func(_ context.Context, _, _, _ string, _ bool, _ []byte) (internalredfish.Client, error) {
					factoryCalled = true
					return mockRfClient, nil
				},
			}

			Expect(k8sClient.Create(ctx, conflictPh)).To(Succeed())
			phLookupKey := types.NamespacedName{Name: conflictPh.Name, Namespace: conflictPh.Namespace}

			By("Reconciling — first pass adds finalizer, second fires the conflict gate")
			_, err := conflictReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: phLookupKey})
			Expect(err).NotTo(HaveOccurred())

			result, err := conflictReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: phLookupKey})
			Expect(err).NotTo(HaveOccurred(), "conflict must be terminal — no requeue-with-error")
			Expect(result.RequeueAfter).To(BeNumerically(">", 0), "conflict should set a long RequeueAfter, not return Result{}")
			Expect(factoryCalled).To(BeFalse(), "factory must not be called when TLS configuration is invalid")

			By("Verifying the InsecureCABundleConflict condition + Error state are set")
			Eventually(func(g Gomega) {
				got := &infrav1.PhysicalHost{}
				g.Expect(k8sClient.Get(ctx, phLookupKey, got)).To(Succeed())
				cond := conditions.Get(got, infrav1.RedfishConnectionReadyCondition)
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
				g.Expect(cond.Reason).To(Equal(infrav1.InsecureCABundleConflictReason))
				g.Expect(got.Status.State).To(Equal(infrav1.StateError))
				g.Expect(got.Status.ErrorMessage).To(ContainSubstring("mutually exclusive"))
			}, Timeout, Interval).Should(Succeed())
		})

		It("Should fetch and pass the CA bundle to the factory when CABundleSecretRef is set", func() {
			By("Creating a CA bundle Secret")
			const expectedBundle = "-----BEGIN CERTIFICATE-----\nFAKE\n-----END CERTIFICATE-----\n"
			caSecret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ca-bundle",
					Namespace: testNs.Name,
				},
				Data: map[string][]byte{
					"ca.crt": []byte(expectedBundle),
				},
			}
			Expect(k8sClient.Create(ctx, caSecret)).To(Succeed())

			By("Creating a host that references the CA bundle")
			withBundlePh := physicalHost.DeepCopy()
			withBundlePh.Name = "with-ca-bundle-host"
			withBundlePh.Spec.RedfishConnection.CABundleSecretRef = caSecret.Name

			var observedBundle []byte
			withBundleReconciler := &PhysicalHostReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Log:      ctrl.Log.WithName("physicalhost-test-with-bundle"),
				Recorder: record.NewFakeRecorder(100),
				RedfishClientFactory: func(_ context.Context, _, _, _ string, _ bool, caBundle []byte) (internalredfish.Client, error) {
					// Capture for assertion. The factory is invoked once per reconcile;
					// the latest call wins, which is what we want.
					observedBundle = caBundle
					return mockRfClient, nil
				},
			}

			Expect(k8sClient.Create(ctx, withBundlePh)).To(Succeed())
			phLookupKey := types.NamespacedName{Name: withBundlePh.Name, Namespace: withBundlePh.Namespace}

			_, err := withBundleReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: phLookupKey})
			Expect(err).NotTo(HaveOccurred())
			_, err = withBundleReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: phLookupKey})
			Expect(err).NotTo(HaveOccurred())

			Expect(string(observedBundle)).To(Equal(expectedBundle),
				"factory must receive the bundle bytes from the referenced Secret")
		})

		It("Should fail with CABundleFetchFailed when the referenced bundle Secret is missing", func() {
			By("Creating a host that references a non-existent CA bundle")
			missingBundlePh := physicalHost.DeepCopy()
			missingBundlePh.Name = "missing-ca-bundle-host"
			missingBundlePh.Spec.RedfishConnection.CABundleSecretRef = "ghost-bundle"

			factoryCalled := false
			missingBundleReconciler := &PhysicalHostReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Log:      ctrl.Log.WithName("physicalhost-test-missing-bundle"),
				Recorder: record.NewFakeRecorder(100),
				RedfishClientFactory: func(_ context.Context, _, _, _ string, _ bool, _ []byte) (internalredfish.Client, error) {
					factoryCalled = true
					return mockRfClient, nil
				},
			}

			Expect(k8sClient.Create(ctx, missingBundlePh)).To(Succeed())
			phLookupKey := types.NamespacedName{Name: missingBundlePh.Name, Namespace: missingBundlePh.Namespace}

			_, err := missingBundleReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: phLookupKey})
			Expect(err).NotTo(HaveOccurred()) // first pass adds finalizer

			_, err = missingBundleReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: phLookupKey})
			Expect(err).To(HaveOccurred(), "missing CA bundle should error so the reconcile can be retried")
			Expect(factoryCalled).To(BeFalse(), "factory must not be called when CA bundle fetch fails")

			Eventually(func(g Gomega) {
				got := &infrav1.PhysicalHost{}
				g.Expect(k8sClient.Get(ctx, phLookupKey, got)).To(Succeed())
				cond := conditions.Get(got, infrav1.RedfishConnectionReadyCondition)
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
				g.Expect(cond.Reason).To(Equal(infrav1.CABundleFetchFailedReason))
				g.Expect(got.Status.State).To(Equal(infrav1.StateError))
			}, Timeout, Interval).Should(Succeed())
		})

		It("Should handle power operations", func() {
			By("Creating PhysicalHost")
			Expect(k8sClient.Create(ctx, physicalHost)).To(Succeed())

			phLookupKey := types.NamespacedName{Name: physicalHost.Name, Namespace: physicalHost.Namespace}

			By("Making host Available")
			_, err := reconcileWithTimeout(reconciler, phLookupKey)
			Expect(err).NotTo(HaveOccurred())
			_, err = reconcileWithTimeout(reconciler, phLookupKey)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying power state is tracked")
			Eventually(func(g Gomega) {
				ph := &infrav1.PhysicalHost{}
				g.Expect(k8sClient.Get(ctx, phLookupKey, ph)).To(Succeed())
				g.Expect(ph.Status.ObservedPowerState).To(Equal(string(schemas.OffPowerState)))
			}, Timeout, Interval).Should(Succeed())

			By("Simulating power on")
			mockRfClient.PowerState = schemas.OnPowerState
			_, err = reconcileWithTimeout(reconciler, phLookupKey)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				ph := &infrav1.PhysicalHost{}
				g.Expect(k8sClient.Get(ctx, phLookupKey, ph)).To(Succeed())
				g.Expect(ph.Status.ObservedPowerState).To(Equal(string(schemas.OnPowerState)))
			}, Timeout, Interval).Should(Succeed())
		})

		It("Should consume inspection-result annotation, persist InspectionReport to Status, delete the ConfigMap, and clear the annotation", func() {
			By("Creating the PhysicalHost and making it Available, with a ConsumerRef so we don't transition back to Available")
			Expect(k8sClient.Create(ctx, physicalHost)).To(Succeed())

			phLookupKey := types.NamespacedName{Name: physicalHost.Name, Namespace: physicalHost.Namespace}
			_, err := reconcileWithTimeout(reconciler, phLookupKey)
			Expect(err).NotTo(HaveOccurred())
			_, err = reconcileWithTimeout(reconciler, phLookupKey)
			Expect(err).NotTo(HaveOccurred())

			By("Creating the inspection-result ConfigMap that the handler would have written")
			report := &infrav1.InspectionReport{
				Timestamp:    metav1.Now(),
				Manufacturer: "Acme",
				Model:        "Test-1000",
				SerialNumber: "SN-RESULT",
				CPUs: []infrav1.CPUInfo{
					{ID: "cpu0", Cores: 16},
				},
			}
			body, err := json.Marshal(report)
			Expect(err).NotTo(HaveOccurred())

			cmName := inspectionResultConfigMapName(physicalHost.Name)
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      cmName,
					Namespace: physicalHost.Namespace,
				},
				Data: map[string]string{
					inspectionResultDataKey: string(body),
				},
			}
			Expect(k8sClient.Create(ctx, cm)).To(Succeed())

			By("Setting the inspection-result annotation pointing at the ConfigMap")
			ph := &infrav1.PhysicalHost{}
			Expect(k8sClient.Get(ctx, phLookupKey, ph)).To(Succeed())
			phPatch := ph.DeepCopy()
			if phPatch.Annotations == nil {
				phPatch.Annotations = map[string]string{}
			}
			phPatch.Annotations[InspectionResultAnnotation] = cmName
			Expect(k8sClient.Patch(ctx, phPatch, client.MergeFrom(ph))).To(Succeed())

			By("Reconciling — controller should consume the result, persist to Status, and delete the CM")
			_, err = reconcileWithTimeout(reconciler, phLookupKey)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				got := &infrav1.PhysicalHost{}
				g.Expect(k8sClient.Get(ctx, phLookupKey, got)).To(Succeed())
				g.Expect(got.Status.InspectionReport).NotTo(BeNil(), "Status.InspectionReport must be set")
				g.Expect(got.Status.InspectionReport.Manufacturer).To(Equal("Acme"))
				g.Expect(got.Status.InspectionReport.Model).To(Equal("Test-1000"))
				g.Expect(got.Status.InspectionReport.SerialNumber).To(Equal("SN-RESULT"))
				g.Expect(got.Status.InspectionPhase).To(Equal(infrav1.InspectionPhaseComplete))
				g.Expect(conditions.IsTrue(got, infrav1.HostInspectedCondition)).To(BeTrue(),
					"HostInspectedCondition must be True after consuming the report")
				// Annotation cleared.
				g.Expect(got.Annotations).NotTo(HaveKey(InspectionResultAnnotation),
					"inspection-result annotation must be removed after consumption")
			}, Timeout, Interval).Should(Succeed())

			By("Verifying the inspection-result ConfigMap was deleted (one-shot consumption)")
			Eventually(func(g Gomega) {
				got := &corev1.ConfigMap{}
				err := k8sClient.Get(ctx, types.NamespacedName{Namespace: physicalHost.Namespace, Name: cmName}, got)
				g.Expect(client.IgnoreNotFound(err)).To(Succeed())
				g.Expect(err).NotTo(BeNil(), "ConfigMap must be gone")
			}, Timeout, Interval).Should(Succeed())
		})

		It("Should consume bootstrap-url annotation, persist to Status.Bootstrap.URL, and clear the annotation", func() {
			By("Creating the PhysicalHost and making it Available")
			Expect(k8sClient.Create(ctx, physicalHost)).To(Succeed())

			phLookupKey := types.NamespacedName{Name: physicalHost.Name, Namespace: physicalHost.Namespace}

			// Two reconciles: first adds finalizer, second drives to Available.
			_, err := reconcileWithTimeout(reconciler, phLookupKey)
			Expect(err).NotTo(HaveOccurred())
			_, err = reconcileWithTimeout(reconciler, phLookupKey)
			Expect(err).NotTo(HaveOccurred())

			ph := &infrav1.PhysicalHost{}
			Expect(k8sClient.Get(ctx, phLookupKey, ph)).To(Succeed())
			Expect(ph.Status.State).To(Equal(infrav1.StateAvailable))

			By("Setting the bootstrap-url annotation (as Beskar7Machine controller would)")
			const expectedURL = "https://beskar7-controller-manager.capb7-system.svc:8082/api/v1/bootstrap/default/test-physicalhost"
			phPatch := ph.DeepCopy()
			if phPatch.Annotations == nil {
				phPatch.Annotations = map[string]string{}
			}
			phPatch.Annotations[BootstrapURLAnnotation] = expectedURL
			Expect(k8sClient.Patch(ctx, phPatch, client.MergeFrom(ph))).To(Succeed())

			By("Reconciling — controller should consume annotation and persist to Status.Bootstrap.URL")
			_, err = reconcileWithTimeout(reconciler, phLookupKey)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				got := &infrav1.PhysicalHost{}
				g.Expect(k8sClient.Get(ctx, phLookupKey, got)).To(Succeed())
				g.Expect(got.Status.Bootstrap).NotTo(BeNil(), "Status.Bootstrap must be initialized")
				g.Expect(got.Status.Bootstrap.URL).To(Equal(expectedURL),
					"Status.Bootstrap.URL must equal the annotation value")
				// Annotation must be cleared so it is not acted on again.
				g.Expect(got.Annotations).NotTo(HaveKey(BootstrapURLAnnotation),
					"bootstrap-url annotation must be removed after consumption")
			}, Timeout, Interval).Should(Succeed())
		})
	})

	Describe("PhysicalHost pause functionality", func() {
		var physicalHost *infrav1.PhysicalHost
		var credentialSecret *corev1.Secret
		var mockRfClient *internalredfish.MockClient
		var reconciler *PhysicalHostReconciler
		var testNs *corev1.Namespace

		BeforeEach(func() {
			testNs = &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "physicalhost-pause-test-",
				},
			}
			Expect(k8sClient.Create(ctx, testNs)).To(Succeed())

			credentialSecret = bmcCredentialsSecretNamed(testNs.Name, "test-redfish-credentials-pause")
			Expect(k8sClient.Create(ctx, credentialSecret)).To(Succeed())

			physicalHost = &infrav1.PhysicalHost{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-physicalhost-pause",
					Namespace: testNs.Name,
				},
				Spec: infrav1.PhysicalHostSpec{
					RedfishConnection: infrav1.RedfishConnection{
						Address:              "https://redfish-pause.example.com",
						CredentialsSecretRef: credentialSecret.Name,
					},
				},
			}

			mockRfClient = internalredfish.NewMockClient()

			reconciler = &PhysicalHostReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Log:      ctrl.Log.WithName("physicalhost-test-pause"),
				Recorder: record.NewFakeRecorder(100),
				RedfishClientFactory: func(ctx context.Context, address, username, password string, insecure bool, caBundle []byte) (internalredfish.Client, error) {
					return mockRfClient, nil
				},
			}
		})

		AfterEach(func() {
			Expect(k8sClient.Delete(ctx, testNs)).To(Succeed())
		})

		// PhysicalHostReconciler honors the cluster.x-k8s.io/paused annotation on
		// the resource itself (GAP-1 / #87). PhysicalHost is standalone inventory
		// with no owner Cluster, so only the resource-level annotation applies —
		// there is no cluster-pause to inherit. A paused host is left completely
		// untouched: no finalizer add, no Redfish I/O, no status write.
		It("Should skip reconciliation when paused", func() {
			By("Creating paused PhysicalHost")
			physicalHost.Annotations = map[string]string{
				clusterv1.PausedAnnotation: "true",
			}
			Expect(k8sClient.Create(ctx, physicalHost)).To(Succeed())

			phLookupKey := types.NamespacedName{Name: physicalHost.Name, Namespace: physicalHost.Namespace}

			By("Reconciling paused host")
			result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: phLookupKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))

			By("Verifying no Redfish calls were made")
			Expect(mockRfClient.GetSystemInfoCalled).To(BeFalse())
			Expect(mockRfClient.GetPowerStateCalled).To(BeFalse())
		})

		It("Should resume when pause annotation is removed", func() {
			By("Creating paused PhysicalHost")
			physicalHost.Annotations = map[string]string{
				clusterv1.PausedAnnotation: "true",
			}
			Expect(k8sClient.Create(ctx, physicalHost)).To(Succeed())

			phLookupKey := types.NamespacedName{Name: physicalHost.Name, Namespace: physicalHost.Namespace}

			By("Verifying paused state")
			result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: phLookupKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))

			By("Removing pause annotation")
			pausedPh := &infrav1.PhysicalHost{}
			Expect(k8sClient.Get(ctx, phLookupKey, pausedPh)).To(Succeed())
			delete(pausedPh.Annotations, clusterv1.PausedAnnotation)
			Expect(k8sClient.Update(ctx, pausedPh)).To(Succeed())

			By("Reconciling resumed host")
			result, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: phLookupKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			Eventually(func(g Gomega) {
				resumedPh := &infrav1.PhysicalHost{}
				g.Expect(k8sClient.Get(ctx, phLookupKey, resumedPh)).To(Succeed())
				g.Expect(resumedPh.Finalizers).To(ContainElement(PhysicalHostFinalizer))
			}, time.Second*10, time.Millisecond*250).Should(Succeed())
		})
	})
})

// The retired credential annotations (D-029) are removed on sight and never
// read. Pure unit; no I/O.
var _ = Describe("dropRetiredCredentialAnnotations", func() {
	r := &PhysicalHostReconciler{Log: ctrl.Log.WithName("retired-annotations-test")}

	It("removes both keys, whatever they carry, and leaves status and every other annotation alone", func() {
		ph := &infrav1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				BootstrapTokenAnnotation:    `{"hash":"` + strings.Repeat("a", 64) + `"}`,
				BootNonceAnnotation:         "not-even-json",
				InspectionRequestAnnotation: "inspect",
			}},
		}
		r.dropRetiredCredentialAnnotations(r.Log, ph)
		Expect(ph.Annotations).To(Equal(map[string]string{InspectionRequestAnnotation: "inspect"}))
		Expect(ph.Status.Bootstrap).To(BeNil(), "an annotation is never promoted into status")
	})

	It("is a no-op when neither annotation is present", func() {
		ph := &infrav1.PhysicalHost{}
		r.dropRetiredCredentialAnnotations(r.Log, ph)
		Expect(ph.Annotations).To(BeNil())
	})
})

// mirrorBootstrapCredentials copies the Secret's hashes and lifetimes into
// status for operators; nothing authenticates against the copy (D-029). Pure
// unit against a fake client.
var _ = Describe("mirrorBootstrapCredentials", func() {
	const ns, hostName = "mirror-ns", "mirror-host"
	var (
		now        time.Time
		consumedAt metav1.Time
	)
	BeforeEach(func() {
		now = time.Now().Truncate(time.Second)
		consumedAt = metav1.NewTime(now.Add(-time.Minute))
	})

	mirror := func(ph *infrav1.PhysicalHost, objs ...client.Object) {
		r := &PhysicalHostReconciler{
			Client: fake.NewClientBuilder().WithScheme(k8sClient.Scheme()).WithObjects(objs...).Build(),
			Log:    ctrl.Log.WithName("mirror-test"),
		}
		r.mirrorBootstrapCredentials(ctx, r.Log, ph)
	}
	hostWithStatus := func() *infrav1.PhysicalHost {
		return &infrav1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{Name: hostName, Namespace: ns},
			Status: infrav1.PhysicalHostStatus{Bootstrap: &infrav1.BootstrapStatus{
				URL:                   "https://callback.example.com/api/v1/bootstrap/mirror-ns/mirror-host",
				TokenHash:             auth.Hash("older-token"),
				BootNonceHash:         auth.Hash("older-nonce"),
				BootNonceConsumedAt:   &consumedAt,
				BootNonceConsumedHash: auth.Hash("older-nonce"),
			}},
		}
	}

	It("mirrors the bound Secret's hashes and lifetimes, and leaves the URL and the consume record alone", func() {
		ph := hostWithStatus()
		data := boundCredentialData("mirror-machine", "token", time.Hour, "nonce", 10*time.Minute)
		data[bootstrapTokenIssuedAtSecretKey] = credentialTime(now)
		mirror(ph, credentialSecret(ns, hostName, data))

		bs := ph.Status.Bootstrap
		Expect(bs.TokenHash).To(Equal(auth.Hash("token")))
		Expect(bs.IssuedAt.Time).To(BeTemporally("==", now))
		Expect(bs.ExpiresAt.Time).To(BeTemporally("~", now.Add(time.Hour), 2*time.Second))
		Expect(bs.BootNonceHash).To(Equal(auth.Hash("nonce")))
		Expect(bs.BootNonceExpiresAt.Time).To(BeTemporally("~", now.Add(10*time.Minute), 2*time.Second))
		Expect(bs.URL).To(Equal("https://callback.example.com/api/v1/bootstrap/mirror-ns/mirror-host"))
		Expect(bs.BootNonceConsumedAt).To(Equal(&consumedAt), "the consume record is the /boot handler's (D-010)")
		Expect(bs.BootNonceConsumedHash).To(Equal(auth.Hash("older-nonce")))
	})

	It("mirrors a missing or malformed expiry as absent, as the verifier reads it", func() {
		ph := hostWithStatus()
		data := boundCredentialData("mirror-machine", "token", time.Hour, "nonce", time.Minute)
		data[bootstrapTokenExpiresAtSecretKey] = []byte("next tuesday")
		delete(data, bootNonceExpiresAtSecretKey)
		mirror(ph, credentialSecret(ns, hostName, data))
		Expect(ph.Status.Bootstrap.TokenHash).To(Equal(auth.Hash("token")))
		Expect(ph.Status.Bootstrap.ExpiresAt).To(BeNil())
		Expect(ph.Status.Bootstrap.BootNonceExpiresAt).To(BeNil())
	})

	It("leaves status alone for a Secret written before the consumer binding (the upgrade backfill reads it)", func() {
		ph := hostWithStatus()
		before := ph.Status.DeepCopy()
		mirror(ph, credentialSecret(ns, hostName, map[string][]byte{bootstrapTokenSecretKey: []byte("token")}))
		Expect(ph.Status).To(Equal(*before))
	})

	It("clears the mirrored credentials when the Secret is gone, keeping the URL and the consume record", func() {
		ph := hostWithStatus()
		mirror(ph)
		bs := ph.Status.Bootstrap
		Expect(bs.TokenHash).To(BeEmpty())
		Expect(bs.BootNonceHash).To(BeEmpty())
		Expect(bs.ExpiresAt).To(BeNil())
		Expect(bs.URL).NotTo(BeEmpty())
		Expect(bs.BootNonceConsumedHash).To(Equal(auth.Hash("older-nonce")))
	})

	It("writes nothing on a host that has no Status.Bootstrap and no Secret", func() {
		ph := &infrav1.PhysicalHost{ObjectMeta: metav1.ObjectMeta{Name: hostName, Namespace: ns}}
		mirror(ph)
		Expect(ph.Status.Bootstrap).To(BeNil())
	})
})
