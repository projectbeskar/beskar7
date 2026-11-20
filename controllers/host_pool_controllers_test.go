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
	"net/http"
	"net/http/httptest"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	infrav1 "github.com/beskar7/beskar7/api/v1beta2"
)

const (
	poolSpecTimeout  = 30 * time.Second
	poolSpecInterval = 100 * time.Millisecond
	poolSpecCluster  = "pool-cluster"
	poolSpecGrant    = "beskar7-pool-spec"
)

// The pool controllers under a running manager with --enable-host-pools: the
// claim, revocation and recovery are driven by the watches alone, never by a
// reconcile called from the spec.
var _ = Describe("PhysicalHostPool claims under a running manager", func() {
	var (
		hostNs, tenantNs *corev1.Namespace
		pool             *infrav1.PhysicalHostPool
		mgrCancel        context.CancelFunc
	)

	BeforeEach(func() {
		hostNs = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "pool-spec-hosts-"}}
		Expect(k8sClient.Create(ctx, hostNs)).To(Succeed())
		tenantNs = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "pool-spec-tenant-"}}
		Expect(k8sClient.Create(ctx, tenantNs)).To(Succeed())
		setNamespaceGrant(tenantNs, true)
		Expect(k8sClient.Create(ctx, bmcCredentialsSecret(hostNs.Name))).To(Succeed())

		pool = &infrav1.PhysicalHostPool{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "pool-spec-"},
			Spec: infrav1.PhysicalHostPoolSpec{
				HostNamespaces:    []string{hostNs.Name},
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{poolSpecGrant: tenantNs.Name}},
			},
		}
		Expect(k8sClient.Create(ctx, pool)).To(Succeed())

		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:                 k8sClient.Scheme(),
			Metrics:                metricsserver.Options{BindAddress: "0"},
			HealthProbeBindAddress: "0",
			// envtest never finishes deleting namespaces, so a cluster-wide cache
			// would hand these controllers every other spec's leftovers.
			Cache: cache.Options{DefaultNamespaces: map[string]cache.Config{
				hostNs.Name: {}, tenantNs.Name: {},
			}},
			Controller: config.Controller{SkipNameValidation: ptr.To(true)},
		})
		Expect(err).NotTo(HaveOccurred())

		bmc := (&bmcGate{reachable: true}).factory()
		Expect((&PhysicalHostReconciler{
			Client:               mgr.GetClient(),
			Scheme:               mgr.GetScheme(),
			Log:                  ctrl.Log.WithName("pool-spec-host"),
			Recorder:             events.NewFakeRecorder(100),
			RedfishClientFactory: bmc,
			BootstrapURLBase:     "https://example.com:8082",
			CleaningImageURL:     fixtureCleaningImageURL,
			CleaningTimeout:      DefaultCleaningTimeout,
			HostPools:            true,
		}).SetupWithManager(mgr)).To(Succeed())
		Expect((&Beskar7MachineReconciler{
			Client:               mgr.GetClient(),
			Scheme:               mgr.GetScheme(),
			Log:                  ctrl.Log.WithName("pool-spec-machine"),
			RedfishClientFactory: bmc,
			HostPools:            true,
		}).SetupWithManager(mgr)).To(Succeed())
		Expect((&PhysicalHostPoolReconciler{Client: mgr.GetClient()}).SetupWithManager(mgr)).To(Succeed())

		var mgrCtx context.Context
		mgrCtx, mgrCancel = context.WithCancel(ctx)
		go func() {
			defer GinkgoRecover()
			Expect(mgr.Start(mgrCtx)).To(Succeed())
		}()
		Expect(mgr.GetCache().WaitForCacheSync(mgrCtx)).To(BeTrue())

		Expect(k8sClient.Create(ctx, &clusterv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: poolSpecCluster, Namespace: tenantNs.Name},
			Spec:       clusterv1.ClusterSpec{Paused: ptr.To(false)},
		})).To(Succeed())
	})

	AfterEach(func() {
		mgrCancel()
		// The pool's finalizer waits for claims nobody releases here.
		current := &infrav1.PhysicalHostPool{}
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(pool), current); err == nil {
			current.Finalizers = nil
			Expect(k8sClient.Update(ctx, current)).To(Succeed())
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, current))).To(Succeed())
		}
		Expect(k8sClient.Delete(ctx, hostNs)).To(Succeed())
		Expect(k8sClient.Delete(ctx, tenantNs)).To(Succeed())
	})

	// wipedPoolHost creates a pool member and takes it through the wipe a new
	// Metadata host gets, so it is Available with HostCleaned=True.
	wipedPoolHost := func(name, zone string) client.ObjectKey {
		labels := map[string]string{poolLabelKey(pool.Name): ""}
		if zone != "" {
			labels[zoneLabelKey] = zone
		}
		host := &infrav1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: hostNs.Name, Labels: labels},
			Spec: infrav1.PhysicalHostSpec{
				RedfishConnection: infrav1.RedfishConnection{
					Address:              "https://mock-redfish.example.invalid:8443",
					CredentialsSecretRef: "bmc-credentials",
				},
				Cleaning: &infrav1.CleaningSpec{Mode: infrav1.CleaningModeMetadata},
			},
		}
		Expect(k8sClient.Create(ctx, host)).To(Succeed())
		key := client.ObjectKeyFromObject(host)
		Eventually(func(g Gomega) {
			g.Expect(getPhysicalHost(key).CleaningStartedAt()).NotTo(BeNil())
		}, poolSpecTimeout, poolSpecInterval).Should(Succeed())
		bindCleaningReport(key, CleanedRequestAnnotation, cleanedRequestValue)
		Eventually(func(g Gomega) {
			h := getPhysicalHost(key)
			g.Expect(h.Status.State).To(Equal(infrav1.StateAvailable))
			g.Expect(conditions.IsTrue(h, infrav1.HostCleanedCondition)).To(BeTrue())
		}, poolSpecTimeout, poolSpecInterval).Should(Succeed())
		return key
	}

	// poolMachine creates a Beskar7Machine in the tenant namespace that claims
	// through the pool, with the CAPI Machine and bootstrap data it waits for.
	poolMachine := func(name, failureDomain string) client.ObjectKey {
		ns := tenantNs.Name
		dataSecret := name + "-bootstrap"
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: dataSecret, Namespace: ns},
			Data:       map[string][]byte{bootstrapDataSecretKey: []byte("#cloud-config\n")},
		})).To(Succeed())
		owner := &clusterv1.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: ns,
				Labels: map[string]string{clusterv1.ClusterNameLabel: poolSpecCluster},
			},
			Spec: clusterv1.MachineSpec{
				ClusterName: poolSpecCluster,
				InfrastructureRef: clusterv1.ContractVersionedObjectReference{
					APIGroup: infrav1.GroupVersion.Group, Kind: "Beskar7Machine", Name: name,
				},
				Bootstrap:     clusterv1.Bootstrap{DataSecretName: ptr.To(dataSecret)},
				FailureDomain: failureDomain,
			},
		}
		Expect(k8sClient.Create(ctx, owner)).To(Succeed())
		machine := testPoolMachine(name, ns, pool.Name)
		machine.UID = ""
		machine.Labels = map[string]string{clusterv1.ClusterNameLabel: poolSpecCluster}
		machine.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: clusterv1.GroupVersion.String(), Kind: "Machine", Name: owner.Name, UID: owner.UID,
		}}
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())
		return client.ObjectKeyFromObject(machine)
	}

	getMachine := func(key client.ObjectKey) *infrav1.Beskar7Machine {
		m := &infrav1.Beskar7Machine{}
		Expect(k8sClient.Get(ctx, key, m)).To(Succeed())
		return m
	}

	getPool := func() *infrav1.PhysicalHostPool {
		p := &infrav1.PhysicalHostPool{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pool), p)).To(Succeed())
		return p
	}

	// claimValid waits for the machine's PoolClaimValid and the pool's ClaimsValid to reach status.
	claimValid := func(machineKey client.ObjectKey, status metav1.ConditionStatus) {
		Eventually(func(g Gomega) {
			m := getMachine(machineKey)
			g.Expect(conditions.Get(m, infrav1.PoolClaimValidCondition)).NotTo(BeNil())
			g.Expect(conditions.Get(m, infrav1.PoolClaimValidCondition).Status).To(Equal(status))
			p := getPool()
			g.Expect(conditions.Get(p, infrav1.ClaimsValidCondition)).NotTo(BeNil())
			g.Expect(conditions.Get(p, infrav1.ClaimsValidCondition).Status).To(Equal(status))
			if status == metav1.ConditionFalse {
				g.Expect(conditions.GetReason(m, infrav1.PoolClaimValidCondition)).To(Equal(infrav1.PoolGrantRevokedReason))
				g.Expect(conditions.IsTrue(m, infrav1.InfrastructureReadyCondition)).To(BeFalse())
			}
		}, poolSpecTimeout, poolSpecInterval).Should(Succeed())
	}

	// claimedHost waits until the machine holds hostKey through the pool, booted into the inspector.
	claimedHost := func(machineKey, hostKey client.ObjectKey) {
		Eventually(func(g Gomega) {
			h := getPhysicalHost(hostKey)
			g.Expect(h.Spec.ConsumerRef).NotTo(BeNil())
			g.Expect(h.Spec.ConsumerRef.Name).To(Equal(machineKey.Name))
			g.Expect(h.Spec.ConsumerPool).To(Equal(pool.Name))
			g.Expect(h.Status.State).To(Equal(infrav1.StateInspecting))
		}, poolSpecTimeout, poolSpecInterval).Should(Succeed())
	}

	It("revokes and restores a claim through the Namespace and pool watches", func() {
		hostKey := wipedPoolHost("pool-host", "")
		machineKey := poolMachine("pool-m1", "")
		claimedHost(machineKey, hostKey)
		claimValid(machineKey, metav1.ConditionTrue)
		Expect(getPool().Status.Claimed).To(Equal(int32(1)))

		By("removing the namespace label the pool grants on")
		setNamespaceGrant(tenantNs, false)
		claimValid(machineKey, metav1.ConditionFalse)
		Expect(getPhysicalHost(hostKey).Spec.ConsumerRef).NotTo(BeNil(), "a revoked claim is held, not released")

		By("refusing /boot while the claim is revoked")
		boot := func() int {
			nonce := string(getCredentialSecret(hostKey).Data[bootNonceSecretKey])
			handler := &BootHandler{Client: k8sClient, Log: ctrl.Log.WithName("pool-spec-boot"),
				Config: bootTestConfig(), HostPools: true}
			req := httptest.NewRequest(http.MethodGet, "/api/v1/boot/"+hostKey.Namespace+"/"+hostKey.Name+"/"+nonce, nil)
			req.SetPathValue("namespace", hostKey.Namespace)
			req.SetPathValue("hostName", hostKey.Name)
			req.SetPathValue("nonce", nonce)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			return w.Code
		}
		Expect(boot()).To(Equal(http.StatusNotFound))

		By("restoring the label")
		setNamespaceGrant(tenantNs, true)
		claimValid(machineKey, metav1.ConditionTrue)
		Expect(boot()).To(Equal(http.StatusOK), "the same nonce boots once the grant is back")

		By("changing the pool's selector so it no longer grants the namespace")
		current := getPool()
		current.Spec.NamespaceSelector = &metav1.LabelSelector{MatchLabels: map[string]string{poolSpecGrant: "someone-else"}}
		Expect(k8sClient.Update(ctx, current)).To(Succeed())
		claimValid(machineKey, metav1.ConditionFalse)

		By("granting the namespace again")
		current = getPool()
		current.Spec.NamespaceSelector = &metav1.LabelSelector{MatchLabels: map[string]string{poolSpecGrant: tenantNs.Name}}
		Expect(k8sClient.Update(ctx, current)).To(Succeed())
		claimValid(machineKey, metav1.ConditionTrue)
	})

	It("claims only a pool member in the Machine's failure domain", func() {
		wipedPoolHost("pool-host-rack-1", "rack-1")
		inDomain := wipedPoolHost("pool-host-rack-2", "rack-2")
		machineKey := poolMachine("pool-m-rack-2", "rack-2")
		claimedHost(machineKey, inDomain)
		Expect(getPhysicalHost(client.ObjectKey{Namespace: hostNs.Name, Name: "pool-host-rack-1"}).Spec.ConsumerRef).To(BeNil())
	})

	It("takes no new claim through a pool being deleted", func() {
		held := wipedPoolHost("pool-host-held", "")
		spare := wipedPoolHost("pool-host-spare", "")
		claimedHost(poolMachine("pool-m-first", ""), held)

		Expect(k8sClient.Delete(ctx, getPool())).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(getPool().DeletionTimestamp.IsZero()).To(BeFalse())
		}, poolSpecTimeout, poolSpecInterval).Should(Succeed())

		late := poolMachine("pool-m-late", "")
		Eventually(func(g Gomega) {
			m := getMachine(late)
			g.Expect(conditions.GetReason(m, infrav1.PhysicalHostAssociatedCondition)).To(Equal(infrav1.PoolNotAvailableReason))
		}, poolSpecTimeout, poolSpecInterval).Should(Succeed())
		Consistently(func() *corev1.ObjectReference {
			return getPhysicalHost(spare).Spec.ConsumerRef
		}, "2s", poolSpecInterval).Should(BeNil())
		Expect(getPool().Finalizers).To(ContainElement(infrav1.PhysicalHostPoolFinalizer), "the pool waits for its claim")
	})

	// clusterctl move recreates the machine with a new UID and the host with no
	// status; the machine's providerID is what proves the claim on the host side.
	It("adopts a moved pool claim on the PhysicalHost side", func() {
		const hostName = "pool-host-moved"
		machine := testPoolMachine("pool-m-moved", tenantNs.Name, pool.Name)
		machine.UID = ""
		machine.Spec.ProviderID = providerID(hostNs.Name, hostName)
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())

		host := claimedPhysicalHost(hostNs.Name, hostName, machine.Name)
		host.Labels = map[string]string{poolLabelKey(pool.Name): ""}
		host.Spec.ConsumerRef.Namespace = tenantNs.Name
		host.Spec.ConsumerRef.UID = "uid-before-move"
		host.Spec.ConsumerPool = pool.Name
		Expect(k8sClient.Create(ctx, host)).To(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(getPhysicalHost(client.ObjectKeyFromObject(host)).Status.State).To(Equal(infrav1.StateReady))
		}, poolSpecTimeout, poolSpecInterval).Should(Succeed())
	})
})

// setNamespaceGrant adds or removes the label the spec's pool grants on.
func setNamespaceGrant(ns *corev1.Namespace, granted bool) {
	current := &corev1.Namespace{}
	Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(ns), current)).To(Succeed())
	if current.Labels == nil {
		current.Labels = map[string]string{}
	}
	if granted {
		current.Labels[poolSpecGrant] = ns.Name
	} else {
		delete(current.Labels, poolSpecGrant)
	}
	Expect(k8sClient.Update(ctx, current)).To(Succeed())
}
