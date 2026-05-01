package controllers

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	conditions "sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

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
			func(_ context.Context, _, _, _ string, _ bool) (internalredfish.Client, error) {
				return internalredfish.NewMockClient(), nil
			},
		)
		r := &Beskar7MachineReconciler{RedfishClientFactory: sentinel}

		Expect(r.defaultFactory()).To(Succeed())

		// Pointer equality is not directly comparable for func types in Go; verify
		// the factory is still the one we set by calling it and checking the result type.
		client, err := r.RedfishClientFactory(ctx, "", "", "", false)
		Expect(err).NotTo(HaveOccurred())
		Expect(client).To(BeAssignableToTypeOf(&internalredfish.MockClient{}))
	})
})

var _ = Describe("Beskar7Machine Controller", func() {

	const (
		Timeout  = time.Second * 10
		Interval = time.Millisecond * 250
	)

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

		PIt("[SKIP - Hardware Testing] Should successfully claim an available PhysicalHost", func() {
			By("Creating the Beskar7Machine")
			Expect(k8sClient.Create(ctx, beskar7Machine)).To(Succeed())

			machineLookupKey := types.NamespacedName{Name: beskar7Machine.Name, Namespace: beskar7Machine.Namespace}

			By("Reconciling to claim host")
			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: machineLookupKey})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying PhysicalHost was claimed")
			Eventually(func(g Gomega) {
				claimedHost := &infrastructurev1beta1.PhysicalHost{}
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: physicalHost.Name, Namespace: physicalHost.Namespace}, claimedHost)).To(Succeed())
				g.Expect(claimedHost.Spec.ConsumerRef).NotTo(BeNil())
				g.Expect(claimedHost.Spec.ConsumerRef.Name).To(Equal(beskar7Machine.Name))
				g.Expect(claimedHost.Status.State).To(Equal(infrastructurev1beta1.StateInUse))
			}, Timeout, Interval).Should(Succeed())

			By("Verifying Machine status updated")
			Eventually(func(g Gomega) {
				updatedMachine := &infrastructurev1beta1.Beskar7Machine{}
				g.Expect(k8sClient.Get(ctx, machineLookupKey, updatedMachine)).To(Succeed())
				g.Expect(updatedMachine.Status.Ready).To(BeFalse()) // Not ready yet, still provisioning
			}, Timeout, Interval).Should(Succeed())
		})

		PIt("[SKIP - Hardware Testing] Should transition host to Inspecting state", func() {
			By("Creating and claiming machine")
			Expect(k8sClient.Create(ctx, beskar7Machine)).To(Succeed())

			machineLookupKey := types.NamespacedName{Name: beskar7Machine.Name, Namespace: beskar7Machine.Namespace}

			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: machineLookupKey})
			Expect(err).NotTo(HaveOccurred())

			By("Reconciling again to start inspection")
			_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: machineLookupKey})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying host transitioned to Inspecting")
			Eventually(func(g Gomega) {
				inspectingHost := &infrastructurev1beta1.PhysicalHost{}
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: physicalHost.Name, Namespace: physicalHost.Namespace}, inspectingHost)).To(Succeed())
				g.Expect(inspectingHost.Status.State).To(Equal(infrastructurev1beta1.StateInspecting))
				g.Expect(inspectingHost.Status.InspectionPhase).To(Equal(infrastructurev1beta1.InspectionPending))
			}, Timeout, Interval).Should(Succeed())
		})

		PIt("[SKIP - Hardware Testing] Should handle inspection completion", func() {
			By("Creating and claiming machine")
			Expect(k8sClient.Create(ctx, beskar7Machine)).To(Succeed())

			machineLookupKey := types.NamespacedName{Name: beskar7Machine.Name, Namespace: beskar7Machine.Namespace}

			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: machineLookupKey})
			Expect(err).NotTo(HaveOccurred())

			By("Simulating inspection complete")
			inspectedHost := &infrastructurev1beta1.PhysicalHost{}
			hostKey := types.NamespacedName{Name: physicalHost.Name, Namespace: physicalHost.Namespace}
			Expect(k8sClient.Get(ctx, hostKey, inspectedHost)).To(Succeed())

			inspectedHost.Status.InspectionPhase = infrastructurev1beta1.InspectionComplete
			inspectedHost.Status.InspectionReport = &infrastructurev1beta1.InspectionReport{
				Timestamp:    metav1.Now(),
				Manufacturer: "Dell Inc.",
				Model:        "PowerEdge R750",
				SerialNumber: "ABC123",
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
			Expect(k8sClient.Status().Update(ctx, inspectedHost)).To(Succeed())

			By("Reconciling after inspection complete")
			_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: machineLookupKey})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying machine marked as provisioned")
			Eventually(func(g Gomega) {
				provisionedMachine := &infrastructurev1beta1.Beskar7Machine{}
				g.Expect(k8sClient.Get(ctx, machineLookupKey, provisionedMachine)).To(Succeed())
				g.Expect(provisionedMachine.Status.Ready).To(BeTrue())
				g.Expect(conditions.IsTrue(provisionedMachine, infrastructurev1beta1.InfrastructureReadyCondition)).To(BeTrue())
			}, Timeout, Interval).Should(Succeed())

			By("Verifying host transitioned to Provisioned")
			Eventually(func(g Gomega) {
				provisionedHost := &infrastructurev1beta1.PhysicalHost{}
				g.Expect(k8sClient.Get(ctx, hostKey, provisionedHost)).To(Succeed())
				g.Expect(provisionedHost.Status.State).To(Equal(infrastructurev1beta1.StateProvisioned))
			}, Timeout, Interval).Should(Succeed())
		})

		PIt("[SKIP - Hardware Testing] Should handle no available hosts", func() {
			By("Making all hosts unavailable")
			unavailableHost := physicalHost.DeepCopy()
			unavailableHost.Status.State = infrastructurev1beta1.StateInUse
			unavailableHost.Status.Ready = false
			Expect(k8sClient.Status().Update(ctx, unavailableHost)).To(Succeed())

			By("Creating machine when no hosts available")
			Expect(k8sClient.Create(ctx, beskar7Machine)).To(Succeed())

			machineLookupKey := types.NamespacedName{Name: beskar7Machine.Name, Namespace: beskar7Machine.Namespace}

			By("Reconciling should requeue")
			result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: machineLookupKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			By("Verifying condition reflects no hosts available")
			Eventually(func(g Gomega) {
				waitingMachine := &infrastructurev1beta1.Beskar7Machine{}
				g.Expect(k8sClient.Get(ctx, machineLookupKey, waitingMachine)).To(Succeed())
				cond := conditions.Get(waitingMachine, infrastructurev1beta1.MachineProvisionedCondition)
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(Equal(corev1.ConditionFalse))
				g.Expect(cond.Reason).To(Equal(infrastructurev1beta1.WaitingForPhysicalHostReason))
			}, Timeout, Interval).Should(Succeed())
		})

		PIt("[SKIP - Hardware Testing] Should handle deletion and release host", func() {
			By("Creating and provisioning machine")
			Expect(k8sClient.Create(ctx, beskar7Machine)).To(Succeed())

			machineLookupKey := types.NamespacedName{Name: beskar7Machine.Name, Namespace: beskar7Machine.Namespace}

			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: machineLookupKey})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying host was claimed")
			claimedHost := &infrastructurev1beta1.PhysicalHost{}
			hostKey := types.NamespacedName{Name: physicalHost.Name, Namespace: physicalHost.Namespace}
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, hostKey, claimedHost)).To(Succeed())
				g.Expect(claimedHost.Spec.ConsumerRef).NotTo(BeNil())
			}, Timeout, Interval).Should(Succeed())

			By("Deleting machine")
			Expect(k8sClient.Delete(ctx, beskar7Machine)).To(Succeed())

			_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: machineLookupKey})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying host was released")
			Eventually(func(g Gomega) {
				releasedHost := &infrastructurev1beta1.PhysicalHost{}
				g.Expect(k8sClient.Get(ctx, hostKey, releasedHost)).To(Succeed())
				g.Expect(releasedHost.Spec.ConsumerRef).To(BeNil())
				g.Expect(releasedHost.Status.State).To(Equal(infrastructurev1beta1.StateAvailable))
			}, Timeout, Interval).Should(Succeed())

			By("Verifying machine is eventually deleted")
			Eventually(func() bool {
				deletedMachine := &infrastructurev1beta1.Beskar7Machine{}
				err := k8sClient.Get(ctx, machineLookupKey, deletedMachine)
				return client.IgnoreNotFound(err) == nil
			}, Timeout*2, Interval).Should(BeTrue())
		})

		PIt("[SKIP - Hardware Testing] Should handle pause annotation", func() {
			By("Creating paused machine")
			beskar7Machine.Annotations = map[string]string{
				clusterv1.PausedAnnotation: "true",
			}
			Expect(k8sClient.Create(ctx, beskar7Machine)).To(Succeed())

			machineLookupKey := types.NamespacedName{Name: beskar7Machine.Name, Namespace: beskar7Machine.Namespace}

			By("Reconciling paused machine")
			result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: machineLookupKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))

			By("Verifying no host was claimed")
			unchangedHost := &infrastructurev1beta1.PhysicalHost{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: physicalHost.Name, Namespace: physicalHost.Namespace}, unchangedHost)).To(Succeed())
			Expect(unchangedHost.Spec.ConsumerRef).To(BeNil())
			Expect(unchangedHost.Status.State).To(Equal(infrastructurev1beta1.StateAvailable))
		})

		PIt("[SKIP - Hardware Testing] Should validate hardware requirements", func() {
			By("Creating machine with hardware requirements")
			beskar7Machine.Spec.HardwareRequirements = &infrastructurev1beta1.HardwareRequirements{
				MinCPUCores: 32,
				MinMemoryGB: 64,
				MinDiskGB:   1000,
			}
			Expect(k8sClient.Create(ctx, beskar7Machine)).To(Succeed())

			machineLookupKey := types.NamespacedName{Name: beskar7Machine.Name, Namespace: beskar7Machine.Namespace}

			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: machineLookupKey})
			Expect(err).NotTo(HaveOccurred())

			By("Simulating inspection with insufficient hardware")
			inspectedHost := &infrastructurev1beta1.PhysicalHost{}
			hostKey := types.NamespacedName{Name: physicalHost.Name, Namespace: physicalHost.Namespace}
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, hostKey, inspectedHost)).To(Succeed())
				g.Expect(inspectedHost.Spec.ConsumerRef).NotTo(BeNil())
			}, Timeout, Interval).Should(Succeed())

			inspectedHost.Status.InspectionPhase = infrastructurev1beta1.InspectionComplete
			inspectedHost.Status.InspectionReport = &infrastructurev1beta1.InspectionReport{
				Timestamp:    metav1.Now(),
				Manufacturer: "Dell Inc.",
				Model:        "PowerEdge R650",
				SerialNumber: "XYZ789",
				CPUs: []infrastructurev1beta1.CPUInfo{
					{
						ID:    "0",
						Cores: 16, // Less than required 32
					},
				},
				Memory: []infrastructurev1beta1.MemoryInfo{
					{
						ID:       "DIMM0",
						Capacity: "32GB", // Less than required 64GB
					},
				},
			}
			Expect(k8sClient.Status().Update(ctx, inspectedHost)).To(Succeed())

			By("Reconciling after inspection")
			_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: machineLookupKey})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying validation failure condition")
			Eventually(func(g Gomega) {
				failedMachine := &infrastructurev1beta1.Beskar7Machine{}
				g.Expect(k8sClient.Get(ctx, machineLookupKey, failedMachine)).To(Succeed())
				cond := conditions.Get(failedMachine, infrastructurev1beta1.MachineProvisionedCondition)
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(Equal(corev1.ConditionFalse))
				g.Expect(cond.Reason).To(ContainSubstring("Validation"))
			}, Timeout, Interval).Should(Succeed())
		})
	})
})
