package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/stmcginnis/gofish/redfish"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	conditions "sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrastructurev1beta1 "github.com/projectbeskar/beskar7/api/v1beta1"
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
		var physicalHost *infrastructurev1beta1.PhysicalHost
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
			credentialSecret = &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-redfish-credentials",
					Namespace: testNs.Name,
				},
				Data: map[string][]byte{
					"username": []byte("testuser"),
					"password": []byte("testpass"),
				},
			}
			Expect(k8sClient.Create(ctx, credentialSecret)).To(Succeed())

			// Define the PhysicalHost resource
			physicalHost = &infrastructurev1beta1.PhysicalHost{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-physicalhost",
					Namespace: testNs.Name,
				},
				Spec: infrastructurev1beta1.PhysicalHostSpec{
					RedfishConnection: infrastructurev1beta1.RedfishConnection{
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
				createdPh := &infrastructurev1beta1.PhysicalHost{}
				g.Expect(k8sClient.Get(ctx, phLookupKey, createdPh)).To(Succeed())
				g.Expect(createdPh.Finalizers).To(ContainElement(PhysicalHostFinalizer))
			}, Timeout, Interval).Should(Succeed())

			By("Reconciling again to transition to Available")
			_, err = reconcileWithTimeout(reconciler, phLookupKey)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				createdPh := &infrastructurev1beta1.PhysicalHost{}
				g.Expect(k8sClient.Get(ctx, phLookupKey, createdPh)).To(Succeed())
				g.Expect(createdPh.Status.State).To(Equal(infrastructurev1beta1.StateAvailable))
				g.Expect(createdPh.Status.ObservedPowerState).To(Equal(string(redfish.OffPowerState)))
				g.Expect(createdPh.Status.HardwareDetails).NotTo(BeNil())
				g.Expect(conditions.IsTrue(createdPh, infrastructurev1beta1.RedfishConnectionReadyCondition)).To(BeTrue())
				g.Expect(conditions.IsTrue(createdPh, infrastructurev1beta1.HostAvailableCondition)).To(BeTrue())
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

			ph := &infrastructurev1beta1.PhysicalHost{}
			Expect(k8sClient.Get(ctx, phLookupKey, ph)).To(Succeed())
			Expect(ph.Finalizers).To(ContainElement(PhysicalHostFinalizer),
				"finalizer must be present after first reconcile")

			By("Second reconcile (status update) is idempotent — no conflict expected")
			_, err = reconcileWithTimeout(reconciler, phLookupKey)
			Expect(err).NotTo(HaveOccurred())

			ph2 := &infrastructurev1beta1.PhysicalHost{}
			Expect(k8sClient.Get(ctx, phLookupKey, ph2)).To(Succeed())
			Expect(ph2.Finalizers).To(ContainElement(PhysicalHostFinalizer),
				"finalizer must still be present after second reconcile")
			Expect(ph2.Status.State).To(Equal(infrastructurev1beta1.StateAvailable),
				"status must be persisted by the deferred patch")

			By("Deleting the PhysicalHost")
			Expect(k8sClient.Delete(ctx, physicalHost)).To(Succeed())

			By("Reconciling to handle deletion — finalizer removed via patch")
			_, err = reconcileWithTimeout(reconciler, phLookupKey)
			Expect(err).NotTo(HaveOccurred())

			By("Ensuring PhysicalHost is eventually deleted (finalizer gone)")
			Eventually(func() bool {
				ph := &infrastructurev1beta1.PhysicalHost{}
				errGet := k8sClient.Get(ctx, phLookupKey, ph)
				return client.IgnoreNotFound(errGet) == nil
			}, Timeout*2, Interval).Should(BeTrue())
		})

		// Converted from PIt "[SKIP - Hardware Testing] Should handle inspection phase transitions":
		// The original test directly mutated PhysicalHost.Status which is no longer valid.
		// This version verifies the annotation-based inspection signalling introduced by PR-2.1:
		// when the InspectionRequestAnnotation is set to "inspect", the reconciler transitions
		// state and clears the annotation; "inspect-complete" transitions to StateReady.
		It("Should apply inspection-request annotation and drive state transitions", func() {
			By("Creating the PhysicalHost resource and making it Available")
			Expect(k8sClient.Create(ctx, physicalHost)).To(Succeed())

			phLookupKey := types.NamespacedName{Name: physicalHost.Name, Namespace: physicalHost.Namespace}

			// Two reconciles: first adds finalizer, second drives to Available.
			_, err := reconcileWithTimeout(reconciler, phLookupKey)
			Expect(err).NotTo(HaveOccurred())
			_, err = reconcileWithTimeout(reconciler, phLookupKey)
			Expect(err).NotTo(HaveOccurred())

			ph := &infrastructurev1beta1.PhysicalHost{}
			Expect(k8sClient.Get(ctx, phLookupKey, ph)).To(Succeed())
			Expect(ph.Status.State).To(Equal(infrastructurev1beta1.StateAvailable))

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
				got := &infrastructurev1beta1.PhysicalHost{}
				g.Expect(k8sClient.Get(ctx, phLookupKey, got)).To(Succeed())
				g.Expect(got.Status.State).To(Equal(infrastructurev1beta1.StateInspecting))
				g.Expect(got.Status.InspectionPhase).To(Equal(infrastructurev1beta1.InspectionPhaseBooting))
				g.Expect(got.Status.InspectionTimestamp).NotTo(BeNil())
				// Annotation must be cleared so it is not acted on again.
				g.Expect(got.Annotations).NotTo(HaveKey(InspectionRequestAnnotation))
			}, Timeout, Interval).Should(Succeed())

			By("Setting inspect-complete annotation (as Beskar7Machine controller would after validation)")
			ph2 := &infrastructurev1beta1.PhysicalHost{}
			Expect(k8sClient.Get(ctx, phLookupKey, ph2)).To(Succeed())
			ph2Patch := ph2.DeepCopy()
			if ph2Patch.Annotations == nil {
				ph2Patch.Annotations = map[string]string{}
			}
			ph2Patch.Annotations[InspectionRequestAnnotation] = "inspect-complete"
			Expect(k8sClient.Patch(ctx, ph2Patch, client.MergeFrom(ph2))).To(Succeed())

			By("Reconciling — controller should transition to StateReady")
			_, err = reconcileWithTimeout(reconciler, phLookupKey)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				got := &infrastructurev1beta1.PhysicalHost{}
				g.Expect(k8sClient.Get(ctx, phLookupKey, got)).To(Succeed())
				g.Expect(got.Status.State).To(Equal(infrastructurev1beta1.StateReady))
				g.Expect(conditions.IsTrue(got, infrastructurev1beta1.HostInspectedCondition)).To(BeTrue())
				g.Expect(got.Annotations).NotTo(HaveKey(InspectionRequestAnnotation))
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
				ph := &infrastructurev1beta1.PhysicalHost{}
				errGet := k8sClient.Get(ctx, phLookupKey, ph)
				return client.IgnoreNotFound(errGet) == nil
			}, Timeout*2, Interval).Should(BeTrue())
		})

		It("Should handle Redfish connection failure", func() {
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

			_, err = failedReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: phLookupKey})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("connection timeout"))

			By("Checking error conditions")
			Eventually(func(g Gomega) {
				Expect(k8sClient.Get(ctx, phLookupKey, failedPh)).To(Succeed())
				cond := conditions.Get(failedPh, infrastructurev1beta1.RedfishConnectionReadyCondition)
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(Equal(corev1.ConditionFalse))
				g.Expect(failedPh.Status.State).To(Equal(infrastructurev1beta1.StateError))
			}, Timeout, Interval).Should(Succeed())
		})

		It("Should reject InsecureSkipVerify=true combined with CABundleSecretRef terminally", func() {
			By("Creating a host with the conflicting TLS configuration")
			insecure := true
			conflictPh := physicalHost.DeepCopy()
			conflictPh.Name = "tls-conflict-host"
			conflictPh.Spec.RedfishConnection.InsecureSkipVerify = &insecure
			conflictPh.Spec.RedfishConnection.CABundleSecretRef = &corev1.LocalObjectReference{
				Name: "irrelevant-bundle",
			}

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
				got := &infrastructurev1beta1.PhysicalHost{}
				g.Expect(k8sClient.Get(ctx, phLookupKey, got)).To(Succeed())
				cond := conditions.Get(got, infrastructurev1beta1.RedfishConnectionReadyCondition)
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(Equal(corev1.ConditionFalse))
				g.Expect(cond.Reason).To(Equal(infrastructurev1beta1.InsecureCABundleConflictReason))
				g.Expect(got.Status.State).To(Equal(infrastructurev1beta1.StateError))
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
			withBundlePh.Spec.RedfishConnection.CABundleSecretRef = &corev1.LocalObjectReference{
				Name: caSecret.Name,
			}

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
			missingBundlePh.Spec.RedfishConnection.CABundleSecretRef = &corev1.LocalObjectReference{
				Name: "ghost-bundle",
			}

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
				got := &infrastructurev1beta1.PhysicalHost{}
				g.Expect(k8sClient.Get(ctx, phLookupKey, got)).To(Succeed())
				cond := conditions.Get(got, infrastructurev1beta1.RedfishConnectionReadyCondition)
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(Equal(corev1.ConditionFalse))
				g.Expect(cond.Reason).To(Equal(infrastructurev1beta1.CABundleFetchFailedReason))
				g.Expect(got.Status.State).To(Equal(infrastructurev1beta1.StateError))
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
				ph := &infrastructurev1beta1.PhysicalHost{}
				g.Expect(k8sClient.Get(ctx, phLookupKey, ph)).To(Succeed())
				g.Expect(ph.Status.ObservedPowerState).To(Equal(string(redfish.OffPowerState)))
			}, Timeout, Interval).Should(Succeed())

			By("Simulating power on")
			mockRfClient.PowerState = redfish.OnPowerState
			_, err = reconcileWithTimeout(reconciler, phLookupKey)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				ph := &infrastructurev1beta1.PhysicalHost{}
				g.Expect(k8sClient.Get(ctx, phLookupKey, ph)).To(Succeed())
				g.Expect(ph.Status.ObservedPowerState).To(Equal(string(redfish.OnPowerState)))
			}, Timeout, Interval).Should(Succeed())
		})

		It("Should consume bootstrap-token annotation, persist hash+lifetime to Status.Bootstrap, and clear the annotation", func() {
			By("Creating the PhysicalHost and making it Available")
			Expect(k8sClient.Create(ctx, physicalHost)).To(Succeed())

			phLookupKey := types.NamespacedName{Name: physicalHost.Name, Namespace: physicalHost.Namespace}
			_, err := reconcileWithTimeout(reconciler, phLookupKey)
			Expect(err).NotTo(HaveOccurred())
			_, err = reconcileWithTimeout(reconciler, phLookupKey)
			Expect(err).NotTo(HaveOccurred())

			ph := &infrastructurev1beta1.PhysicalHost{}
			Expect(k8sClient.Get(ctx, phLookupKey, ph)).To(Succeed())
			Expect(ph.Status.State).To(Equal(infrastructurev1beta1.StateAvailable))

			By("Setting the bootstrap-token annotation (as Beskar7Machine controller would)")
			fakeHash := "deadbeef0123456789abcdef0123456789abcdef0123456789abcdef01234567"
			now := metav1.Now()
			expiresAt := metav1.NewTime(now.Add(30 * time.Minute))
			value := BootstrapTokenAnnotationValue{
				Hash:      fakeHash,
				IssuedAt:  now,
				ExpiresAt: expiresAt,
			}
			encoded, err := json.Marshal(value)
			Expect(err).NotTo(HaveOccurred())

			phPatch := ph.DeepCopy()
			if phPatch.Annotations == nil {
				phPatch.Annotations = map[string]string{}
			}
			phPatch.Annotations[BootstrapTokenAnnotation] = string(encoded)
			Expect(k8sClient.Patch(ctx, phPatch, client.MergeFrom(ph))).To(Succeed())

			By("Reconciling — controller should consume annotation and persist to Status.Bootstrap")
			_, err = reconcileWithTimeout(reconciler, phLookupKey)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				got := &infrastructurev1beta1.PhysicalHost{}
				g.Expect(k8sClient.Get(ctx, phLookupKey, got)).To(Succeed())
				g.Expect(got.Status.Bootstrap).NotTo(BeNil(), "Status.Bootstrap must be initialized")
				g.Expect(got.Status.Bootstrap.TokenHash).To(Equal(fakeHash))
				g.Expect(got.Status.Bootstrap.IssuedAt).NotTo(BeNil())
				g.Expect(got.Status.Bootstrap.ExpiresAt).NotTo(BeNil())
				// Allow some skew between encoded and decoded times due to status round-trip.
				g.Expect(got.Status.Bootstrap.ExpiresAt.Time.After(got.Status.Bootstrap.IssuedAt.Time)).To(BeTrue(),
					"ExpiresAt must be after IssuedAt")
				g.Expect(got.Annotations).NotTo(HaveKey(BootstrapTokenAnnotation),
					"bootstrap-token annotation must be removed after consumption")
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
			report := &infrastructurev1beta1.InspectionReport{
				Timestamp:    metav1.Now(),
				Manufacturer: "Acme",
				Model:        "Test-1000",
				SerialNumber: "SN-RESULT",
				CPUs: []infrastructurev1beta1.CPUInfo{
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
			ph := &infrastructurev1beta1.PhysicalHost{}
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
				got := &infrastructurev1beta1.PhysicalHost{}
				g.Expect(k8sClient.Get(ctx, phLookupKey, got)).To(Succeed())
				g.Expect(got.Status.InspectionReport).NotTo(BeNil(), "Status.InspectionReport must be set")
				g.Expect(got.Status.InspectionReport.Manufacturer).To(Equal("Acme"))
				g.Expect(got.Status.InspectionReport.Model).To(Equal("Test-1000"))
				g.Expect(got.Status.InspectionReport.SerialNumber).To(Equal("SN-RESULT"))
				g.Expect(got.Status.InspectionPhase).To(Equal(infrastructurev1beta1.InspectionPhaseComplete))
				g.Expect(conditions.IsTrue(got, infrastructurev1beta1.HostInspectedCondition)).To(BeTrue(),
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

			ph := &infrastructurev1beta1.PhysicalHost{}
			Expect(k8sClient.Get(ctx, phLookupKey, ph)).To(Succeed())
			Expect(ph.Status.State).To(Equal(infrastructurev1beta1.StateAvailable))

			By("Setting the bootstrap-url annotation (as Beskar7Machine controller would)")
			const expectedURL = "https://beskar7-controller-manager.beskar7-system.svc:8082/api/v1/bootstrap/default/test-physicalhost"
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
				got := &infrastructurev1beta1.PhysicalHost{}
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
		var physicalHost *infrastructurev1beta1.PhysicalHost
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

			credentialSecret = &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-redfish-credentials-pause",
					Namespace: testNs.Name,
				},
				Data: map[string][]byte{
					"username": []byte("testuser"),
					"password": []byte("testpass"),
				},
			}
			Expect(k8sClient.Create(ctx, credentialSecret)).To(Succeed())

			physicalHost = &infrastructurev1beta1.PhysicalHost{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-physicalhost-pause",
					Namespace: testNs.Name,
				},
				Spec: infrastructurev1beta1.PhysicalHostSpec{
					RedfishConnection: infrastructurev1beta1.RedfishConnection{
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

		PIt("[SKIP - Pause Not Implemented] Should skip reconciliation when paused", func() {
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

		PIt("[SKIP - Pause Not Implemented] Should resume when pause annotation is removed", func() {
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
			pausedPh := &infrastructurev1beta1.PhysicalHost{}
			Expect(k8sClient.Get(ctx, phLookupKey, pausedPh)).To(Succeed())
			delete(pausedPh.Annotations, clusterv1.PausedAnnotation)
			Expect(k8sClient.Update(ctx, pausedPh)).To(Succeed())

			By("Reconciling resumed host")
			result, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: phLookupKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Requeue).To(BeTrue())

			Eventually(func(g Gomega) {
				resumedPh := &infrastructurev1beta1.PhysicalHost{}
				g.Expect(k8sClient.Get(ctx, phLookupKey, resumedPh)).To(Succeed())
				g.Expect(resumedPh.Finalizers).To(ContainElement(PhysicalHostFinalizer))
			}, time.Second*10, time.Millisecond*250).Should(Succeed())
		})
	})
})
