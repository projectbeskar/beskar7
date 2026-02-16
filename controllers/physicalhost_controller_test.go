package controllers

import (
	"context"
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
				RedfishClientFactory: func(ctx context.Context, address, username, password string, insecure bool) (internalredfish.Client, error) {
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
				RedfishClientFactory: func(ctx context.Context, address, username, password string, insecure bool) (internalredfish.Client, error) {
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

		PIt("[SKIP - Hardware Testing] Should handle inspection phase transitions", func() {
			By("Creating PhysicalHost")
			Expect(k8sClient.Create(ctx, physicalHost)).To(Succeed())

			phLookupKey := types.NamespacedName{Name: physicalHost.Name, Namespace: physicalHost.Namespace}

			By("Making host Available")
			_, err := reconcileWithTimeout(reconciler, phLookupKey)
			Expect(err).NotTo(HaveOccurred())
			_, err = reconcileWithTimeout(reconciler, phLookupKey)
			Expect(err).NotTo(HaveOccurred())

			By("Setting inspection phase")
			ph := &infrastructurev1beta1.PhysicalHost{}
			Expect(k8sClient.Get(ctx, phLookupKey, ph)).To(Succeed())
			ph.Status.InspectionPhase = infrastructurev1beta1.InspectionPending
			Expect(k8sClient.Status().Update(ctx, ph)).To(Succeed())

			By("Simulating inspection in progress")
			ph.Status.InspectionPhase = infrastructurev1beta1.InspectionInProgress
			Expect(k8sClient.Status().Update(ctx, ph)).To(Succeed())

			By("Simulating inspection complete with report")
			ph.Status.InspectionPhase = infrastructurev1beta1.InspectionComplete
			ph.Status.InspectionReport = &infrastructurev1beta1.InspectionReport{
				Timestamp:    metav1.Now(),
				Manufacturer: "Dell Inc.",
				Model:        "PowerEdge R750",
				SerialNumber: "TEST123",
				CPUs: []infrastructurev1beta1.CPUInfo{
					{
						ID:        "0",
						Vendor:    "Intel",
						Model:     "Xeon Gold 6254",
						Cores:     18,
						Threads:   36,
						Frequency: "3.1GHz",
					},
				},
				Memory: []infrastructurev1beta1.MemoryInfo{
					{
						ID:       "DIMM0",
						Type:     "DDR4",
						Capacity: "32GB",
						Speed:    "3200MHz",
					},
				},
			}
			Expect(k8sClient.Status().Update(ctx, ph)).To(Succeed())

			By("Verifying inspection report is stored")
			Eventually(func(g Gomega) {
				updatedPh := &infrastructurev1beta1.PhysicalHost{}
				g.Expect(k8sClient.Get(ctx, phLookupKey, updatedPh)).To(Succeed())
				g.Expect(updatedPh.Status.InspectionPhase).To(Equal(infrastructurev1beta1.InspectionComplete))
				g.Expect(updatedPh.Status.InspectionReport).NotTo(BeNil())
				g.Expect(updatedPh.Status.InspectionReport.Manufacturer).To(Equal("Dell Inc."))
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
				RedfishClientFactory: func(ctx context.Context, address, username, password string, insecure bool) (internalredfish.Client, error) {
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
