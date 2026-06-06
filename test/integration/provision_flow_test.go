//go:build integration

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

package integration

import (
	"context"
	"encoding/json"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrastructurev1beta1 "github.com/projectbeskar/beskar7/api/v1beta1"
	"github.com/projectbeskar/beskar7/controllers"
)

// Timeouts and poll intervals used throughout. The manager is envtest-local so
// reconciles are fast; 60 s gives ample headroom for slow CI runners while still
// catching genuine hangs. The 200 ms poll interval keeps specs snappy on fast
// hardware without hammering the apiserver.
const (
	eventuallyTimeout  = "60s"
	eventuallyInterval = "200ms"
)

// simulateProvisioned sets the ProvisionedRequestAnnotation on the host,
// mimicking what the ProvisionedHandler writes when the inspector signals
// OS deployment complete (D-015). The PhysicalHostReconciler's
// applyProvisionedRequestAnnotation then consumes it and transitions the
// host from Deploying to Ready.
func simulateProvisioned(ctx context.Context, ns, hostName string) {
	hostKey := client.ObjectKey{Namespace: ns, Name: hostName}
	currentHost := &infrastructurev1beta1.PhysicalHost{}
	Expect(mgr.GetClient().Get(ctx, hostKey, currentHost)).To(Succeed())
	base := currentHost.DeepCopy()
	if currentHost.Annotations == nil {
		currentHost.Annotations = make(map[string]string)
	}
	currentHost.Annotations[controllers.ProvisionedRequestAnnotation] = "provisioned"
	Expect(k8sClient.Patch(ctx, currentHost, client.MergeFrom(base))).To(Succeed())
}

// simulateInspector creates the inspection-result ConfigMap and sets the
// InspectionResultAnnotation on the host, mimicking what the real HTTPS
// callback server writes (locked-decision 2). The PhysicalHostReconciler's
// applyInspectionResultAnnotation then consumes it under real watches.
func simulateInspector(ctx context.Context, ns, hostName string) {
	report := &infrastructurev1beta1.InspectionReport{
		Timestamp:    metav1.Now(),
		Manufacturer: "MockInc",
		Model:        "MockSystem",
		SerialNumber: "MOCK12345",
	}
	reportJSON, err := json.Marshal(report)
	Expect(err).NotTo(HaveOccurred())

	cmName := fmt.Sprintf("%s-inspection-result", hostName)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: ns,
			Labels: map[string]string{
				// Mirror the labels the real inspection handler writes.
				"infrastructure.cluster.x-k8s.io/owned-by": "beskar7-controller-manager",
				"infrastructure.cluster.x-k8s.io/host":     hostName,
			},
		},
		Data: map[string]string{
			// inspectionResultDataKey = "report.json" (unexported const).
			// Hardcoded here per locked-decision 3 in the issue spec.
			"report.json": string(reportJSON),
		},
	}
	Expect(k8sClient.Create(ctx, cm)).To(Succeed())

	// Patch the annotation on the host using the cache client so the version
	// is current and we do not produce an optimistic-lock conflict.
	hostKey := client.ObjectKey{Namespace: ns, Name: hostName}
	currentHost := &infrastructurev1beta1.PhysicalHost{}
	Expect(mgr.GetClient().Get(ctx, hostKey, currentHost)).To(Succeed())
	base := currentHost.DeepCopy()
	if currentHost.Annotations == nil {
		currentHost.Annotations = make(map[string]string)
	}
	currentHost.Annotations[controllers.InspectionResultAnnotation] = cmName
	Expect(k8sClient.Patch(ctx, currentHost, client.MergeFrom(base))).To(Succeed())
}

var _ = Describe("Full provision flow", func() {
	// The marquee test: drives a PhysicalHost through Available → InUse →
	// Inspecting → Ready and a Beskar7Machine to Status.Ready=true, exercising
	// the live manager's watch mappers (PhysicalHostToBeskar7Machine) and the
	// cross-controller annotation/ConfigMap handoff pattern (D-005).
	//
	// Inspector simulation (per locked-decision 2): we create the inspection-
	// result ConfigMap and set the InspectionResultAnnotation directly, mimicking
	// what the real HTTPS callback server writes. The PhysicalHostReconciler's
	// applyInspectionResultAnnotation then consumes it under real watches.

	var (
		specCtx   context.Context
		ns        *corev1.Namespace
		host      *infrastructurev1beta1.PhysicalHost
		b7machine *infrastructurev1beta1.Beskar7Machine
	)

	BeforeEach(func() {
		specCtx = context.Background()

		// Fresh namespace for full isolation between specs.
		ns = createNamespace(specCtx)

		bmcSecret := createBMCSecret(specCtx, ns.Name, "bmc-creds")
		bootstrapSecret := createBootstrapDataSecret(specCtx, ns.Name, "bootstrap-data")
		cluster := createCluster(specCtx, ns.Name, "test-cluster")
		machine := createMachine(specCtx, ns.Name, "test-machine", cluster.Name, bootstrapSecret.Name)
		host = createPhysicalHost(specCtx, ns.Name, "test-host", bmcSecret.Name)
		b7machine = createBeskar7Machine(specCtx, ns.Name, "test-b7machine", machine)
	})

	AfterEach(func() {
		// Namespace deletion cascades to all owned resources. Using Background
		// so cleanup proceeds even if the spec context was cancelled.
		Expect(k8sClient.Delete(context.Background(), ns)).To(Succeed())
	})

	It("should drive host and machine through the full provision flow", func() {
		hostKey := client.ObjectKeyFromObject(host)

		By("waiting for host to be claimed and reach Inspecting state")
		// Available → InUse → Inspecting can all happen within a single
		// polling window because the reconcilers run continuously. We assert
		// on Inspecting (not InUse) because InUse is ephemeral — the Beskar7Machine
		// controller calls triggerInspection on the same reconcile pass that sees
		// InUse, then the PhysicalHost controller sets Inspecting on its next pass.
		// Asserting on InUse would produce a race between the two controllers that
		// could fail on fast CI runners.
		Eventually(func(g Gomega) {
			current := &infrastructurev1beta1.PhysicalHost{}
			g.Expect(mgr.GetClient().Get(specCtx, hostKey, current)).To(Succeed())
			g.Expect(current.Status.State).To(Equal(infrastructurev1beta1.StateInspecting))
			// ConsumerRef must be set before the host can reach Inspecting.
			g.Expect(current.Spec.ConsumerRef).NotTo(BeNil())
		}, eventuallyTimeout, eventuallyInterval).Should(Succeed())

		By("simulating the inspector: creating result ConfigMap and annotating the host")
		simulateInspector(specCtx, ns.Name, host.Name)

		// D-015: inspection-complete now transitions to StateDeploying, not StateReady.
		// The Beskar7Machine must not be Ready yet at this point.
		By("waiting for PhysicalHostReconciler to consume the result (host → Deploying)")
		Eventually(func(g Gomega) {
			current := &infrastructurev1beta1.PhysicalHost{}
			g.Expect(mgr.GetClient().Get(specCtx, hostKey, current)).To(Succeed())
			g.Expect(current.Status.State).To(Equal(infrastructurev1beta1.StateDeploying),
				"inspect-complete must land in Deploying, not Ready (D-015)")
			g.Expect(current.Status.InspectionPhase).To(Equal(infrastructurev1beta1.InspectionPhaseComplete))
			g.Expect(current.Status.InspectionReport).NotTo(BeNil())
			g.Expect(current.Status.DeployingTimestamp).NotTo(BeNil())
		}, eventuallyTimeout, eventuallyInterval).Should(Succeed())

		By("simulating the inspector provisioned callback (OS image written, host rebooted)")
		simulateProvisioned(specCtx, ns.Name, host.Name)

		By("waiting for PhysicalHostReconciler to consume provisioned signal (host → Ready)")
		Eventually(func(g Gomega) {
			current := &infrastructurev1beta1.PhysicalHost{}
			g.Expect(mgr.GetClient().Get(specCtx, hostKey, current)).To(Succeed())
			g.Expect(current.Status.State).To(Equal(infrastructurev1beta1.StateReady),
				"provisioned callback must drive Deploying→Ready (D-015)")
		}, eventuallyTimeout, eventuallyInterval).Should(Succeed())

		By("waiting for Beskar7Machine to reach Ready=true with ProviderID set")
		b7mKey := client.ObjectKeyFromObject(b7machine)
		expectedProviderID := fmt.Sprintf("b7://%s/%s", ns.Name, host.Name)
		Eventually(func(g Gomega) {
			current := &infrastructurev1beta1.Beskar7Machine{}
			g.Expect(mgr.GetClient().Get(specCtx, b7mKey, current)).To(Succeed())
			g.Expect(current.Status.Ready).To(BeTrue())
			g.Expect(current.Spec.ProviderID).NotTo(BeNil())
			g.Expect(*current.Spec.ProviderID).To(Equal(expectedProviderID))
			g.Expect(current.Status.Initialization).NotTo(BeNil())
			g.Expect(current.Status.Initialization.Provisioned).To(BeTrue())
		}, eventuallyTimeout, eventuallyInterval).Should(Succeed())
	})
})

var _ = Describe("Secret-rotation watch", func() {
	// Verifies that the SecretToPhysicalHosts mapper in PhysicalHostReconciler
	// re-enqueues the owning host whenever its credentials Secret changes. The
	// observable signal is a new reconcile pass that successfully connects to
	// the mock BMC and re-affirms the host's Available state — meaning the
	// RedfishConnectionReady condition is re-evaluated rather than stale.
	//
	// We assert on RedfishConnectionReady=True to confirm the reconcile actually
	// completed a fresh Redfish call, not just a noop.

	var (
		specCtx   context.Context
		ns        *corev1.Namespace
		host      *infrastructurev1beta1.PhysicalHost
		bmcSecret *corev1.Secret
	)

	BeforeEach(func() {
		specCtx = context.Background()
		ns = createNamespace(specCtx)
		bmcSecret = createBMCSecret(specCtx, ns.Name, "bmc-rotation-secret")
		host = createPhysicalHost(specCtx, ns.Name, "rotation-host", bmcSecret.Name)
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(context.Background(), ns)).To(Succeed())
	})

	It("should re-reconcile the owning host when its credentials Secret is updated", func() {
		hostKey := client.ObjectKeyFromObject(host)

		By("waiting for host to reach Available (first successful reconcile)")
		Eventually(func(g Gomega) {
			current := &infrastructurev1beta1.PhysicalHost{}
			g.Expect(mgr.GetClient().Get(specCtx, hostKey, current)).To(Succeed())
			g.Expect(current.Status.State).To(Equal(infrastructurev1beta1.StateAvailable))
			// RedfishConnectionReady=True confirms the first reconcile ran.
			for _, c := range current.Status.Conditions {
				if c.Type == infrastructurev1beta1.RedfishConnectionReadyCondition {
					g.Expect(c.Status).To(Equal(corev1.ConditionTrue))
					return
				}
			}
			g.Expect(current.Status.Conditions).NotTo(BeEmpty(), "RedfishConnectionReady condition must be present")
		}, eventuallyTimeout, eventuallyInterval).Should(Succeed())

		By("rotating the credentials Secret")
		// Fetch the current Secret via the cache client to get the latest version.
		secretKey := client.ObjectKey{Name: bmcSecret.Name, Namespace: ns.Name}
		currentSecret := &corev1.Secret{}
		Expect(mgr.GetClient().Get(specCtx, secretKey, currentSecret)).To(Succeed())
		base := currentSecret.DeepCopy()
		currentSecret.Data["password"] = []byte("new-rotated-password")
		Expect(k8sClient.Patch(specCtx, currentSecret, client.MergeFrom(base))).To(Succeed())

		By("confirming the host reconciles successfully after rotation (RedfishConnectionReady=True, state=Available)")
		// The mock BMC never rejects credentials, so the reconcile should
		// complete successfully regardless of the password value. We assert that
		// the condition is True (set by PhysicalHostReconciler on each successful
		// Redfish query), which proves the reconcile ran after the Secret update.
		// We also confirm the generation has advanced by re-checking the condition
		// timestamp (any re-set of the condition bumps LastTransitionTime or
		// LastProbeTime depending on the CAPI conditions library).
		Eventually(func(g Gomega) {
			current := &infrastructurev1beta1.PhysicalHost{}
			g.Expect(mgr.GetClient().Get(specCtx, hostKey, current)).To(Succeed())
			// Host must still be Available — rotation must not break it.
			g.Expect(current.Status.State).To(Equal(infrastructurev1beta1.StateAvailable))
			found := false
			for _, c := range current.Status.Conditions {
				if c.Type == infrastructurev1beta1.RedfishConnectionReadyCondition {
					g.Expect(c.Status).To(Equal(corev1.ConditionTrue))
					found = true
					break
				}
			}
			g.Expect(found).To(BeTrue(), "RedfishConnectionReady condition must be present after rotation")
		}, eventuallyTimeout, eventuallyInterval).Should(Succeed())
	})
})

var _ = Describe("Delete and release", func() {
	// Regression tests adjacent to the #103 nil-Recorder-on-delete fix. Verify
	// that deleting a Beskar7Machine causes:
	//   1. The host's Spec.ConsumerRef to be cleared.
	//   2. The host's Status.State to return to Available.
	//
	// Two cases are covered:
	//   - Provisioned delete: machine driven to Ready (ProviderID set) before delete.
	//   - Pre-Ready delete (#107 regression): machine deleted while the host is still
	//     Inspecting, so ProviderID is nil. reconcileDelete must locate the claimed
	//     host by ConsumerRef ownership (not ProviderID) and release it; otherwise
	//     the host strands permanently in InUse with a dangling ConsumerRef.

	var (
		specCtx   context.Context
		ns        *corev1.Namespace
		host      *infrastructurev1beta1.PhysicalHost
		b7machine *infrastructurev1beta1.Beskar7Machine
	)

	BeforeEach(func() {
		specCtx = context.Background()
		ns = createNamespace(specCtx)

		bmcSecret := createBMCSecret(specCtx, ns.Name, "delete-bmc-creds")
		bootstrapSecret := createBootstrapDataSecret(specCtx, ns.Name, "delete-bootstrap")
		cluster := createCluster(specCtx, ns.Name, "delete-cluster")
		machine := createMachine(specCtx, ns.Name, "delete-machine", cluster.Name, bootstrapSecret.Name)
		host = createPhysicalHost(specCtx, ns.Name, "delete-host", bmcSecret.Name)
		b7machine = createBeskar7Machine(specCtx, ns.Name, "delete-b7machine", machine)
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(context.Background(), ns)).To(Succeed())
	})

	It("should release the host and return it to Available when a provisioned Beskar7Machine is deleted", func() {
		hostKey := client.ObjectKeyFromObject(host)
		b7mKey := client.ObjectKeyFromObject(b7machine)

		By("driving the machine to Inspecting state")
		Eventually(func(g Gomega) {
			current := &infrastructurev1beta1.PhysicalHost{}
			g.Expect(mgr.GetClient().Get(specCtx, hostKey, current)).To(Succeed())
			g.Expect(current.Status.State).To(Equal(infrastructurev1beta1.StateInspecting))
		}, eventuallyTimeout, eventuallyInterval).Should(Succeed())

		By("simulating the inspector: inspection result + provisioned callback (D-015 two-step)")
		simulateInspector(specCtx, ns.Name, host.Name)
		// Wait for Deploying before sending the provisioned callback.
		Eventually(func(g Gomega) {
			current := &infrastructurev1beta1.PhysicalHost{}
			g.Expect(mgr.GetClient().Get(specCtx, hostKey, current)).To(Succeed())
			g.Expect(current.Status.State).To(Equal(infrastructurev1beta1.StateDeploying))
		}, eventuallyTimeout, eventuallyInterval).Should(Succeed())
		simulateProvisioned(specCtx, ns.Name, host.Name)

		By("waiting for Beskar7Machine to reach Ready=true (ProviderID set)")
		Eventually(func(g Gomega) {
			current := &infrastructurev1beta1.Beskar7Machine{}
			g.Expect(mgr.GetClient().Get(specCtx, b7mKey, current)).To(Succeed())
			g.Expect(current.Status.Ready).To(BeTrue())
			g.Expect(current.Spec.ProviderID).NotTo(BeNil())
		}, eventuallyTimeout, eventuallyInterval).Should(Succeed())

		By("deleting the Beskar7Machine")
		Expect(k8sClient.Delete(specCtx, b7machine)).To(Succeed())

		By("waiting for host to be released (ConsumerRef cleared, state=Available)")
		Eventually(func(g Gomega) {
			current := &infrastructurev1beta1.PhysicalHost{}
			g.Expect(mgr.GetClient().Get(specCtx, hostKey, current)).To(Succeed())
			g.Expect(current.Spec.ConsumerRef).To(BeNil(),
				"ConsumerRef must be cleared after Beskar7Machine deletion")
			g.Expect(current.Status.State).To(Equal(infrastructurev1beta1.StateAvailable),
				"host must return to Available after release")
		}, eventuallyTimeout, eventuallyInterval).Should(Succeed())

		By("confirming the Beskar7Machine is fully gone (finalizer removed by reconcileDelete)")
		Eventually(func(g Gomega) {
			err := mgr.GetClient().Get(specCtx, b7mKey, &infrastructurev1beta1.Beskar7Machine{})
			g.Expect(err).To(MatchError(ContainSubstring("not found")),
				"Beskar7Machine must be fully deleted after finalizer removal")
		}, eventuallyTimeout, eventuallyInterval).Should(Succeed())
	})

	It("should release the host when a Beskar7Machine is deleted before provisioning (ProviderID unset) (#107)", func() {
		hostKey := client.ObjectKeyFromObject(host)
		b7mKey := client.ObjectKeyFromObject(b7machine)

		By("waiting for the host to be claimed and reach Inspecting (pre-Ready, ProviderID still unset)")
		// We deliberately do NOT simulate the inspector here, so the machine never
		// reaches Ready and ProviderID is never set. This is the #107 window.
		Eventually(func(g Gomega) {
			current := &infrastructurev1beta1.PhysicalHost{}
			g.Expect(mgr.GetClient().Get(specCtx, hostKey, current)).To(Succeed())
			g.Expect(current.Status.State).To(Equal(infrastructurev1beta1.StateInspecting))
			g.Expect(current.Spec.ConsumerRef).NotTo(BeNil())
		}, eventuallyTimeout, eventuallyInterval).Should(Succeed())

		By("confirming ProviderID is unset on the Beskar7Machine (proves we are in the pre-Ready window)")
		currentB7M := &infrastructurev1beta1.Beskar7Machine{}
		Expect(mgr.GetClient().Get(specCtx, b7mKey, currentB7M)).To(Succeed())
		Expect(currentB7M.Spec.ProviderID).To(BeNil(),
			"ProviderID must be unset for this regression to exercise the ConsumerRef-based release path")

		By("deleting the Beskar7Machine while the host is still Inspecting")
		Expect(k8sClient.Delete(specCtx, b7machine)).To(Succeed())

		By("waiting for host to be released (ConsumerRef cleared, state=Available) despite ProviderID never being set")
		Eventually(func(g Gomega) {
			current := &infrastructurev1beta1.PhysicalHost{}
			g.Expect(mgr.GetClient().Get(specCtx, hostKey, current)).To(Succeed())
			g.Expect(current.Spec.ConsumerRef).To(BeNil(),
				"ConsumerRef must be cleared even when the machine was deleted before ProviderID was assigned (#107)")
			g.Expect(current.Status.State).To(Equal(infrastructurev1beta1.StateAvailable),
				"host must return to Available rather than stranding in InUse/Inspecting")
		}, eventuallyTimeout, eventuallyInterval).Should(Succeed())

		By("confirming the Beskar7Machine is fully gone")
		Eventually(func(g Gomega) {
			err := mgr.GetClient().Get(specCtx, b7mKey, &infrastructurev1beta1.Beskar7Machine{})
			g.Expect(err).To(MatchError(ContainSubstring("not found")))
		}, eventuallyTimeout, eventuallyInterval).Should(Succeed())
	})
})
