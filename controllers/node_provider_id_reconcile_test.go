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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/controllers/clustercache"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/kubeconfig"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
	internalredfish "github.com/projectbeskar/beskar7/internal/redfish"
)

const (
	npidReconcileCluster = "npid-reconcile-cluster"
	npidReconcileHost    = "npid-reconcile-host"

	// npidEventually bounds a ClusterCache connect, which is asynchronous to the reconcile under test.
	npidEventually = 20 * time.Second
	npidPoll       = 250 * time.Millisecond
	// npidConsistently is how long an opted-out cluster is watched for a connection that must never come.
	npidConsistently = 2 * time.Second
)

// npidReconcileEnv is one namespace with a Ready host, its machine, and a manager running the real ClusterCache.
type npidReconcileEnv struct {
	ns          string
	cluster     *clusterv1.Cluster
	b7cluster   *infrav1.Beskar7Cluster
	b7machine   client.ObjectKey
	node        *corev1.Node
	cache       clustercache.ClusterCache
	clusterRec  *Beskar7ClusterReconciler
	machineRec  *Beskar7MachineReconciler
	stopManager context.CancelFunc
}

func newNPIDReconcileEnv(setNodeProviderID bool) *npidReconcileEnv {
	env := &npidReconcileEnv{stopManager: func() {}}
	nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "npid-reconcile-"}}
	Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
	env.ns = nsObj.Name
	env.startClusterCache()
	env.seedCluster(setNodeProviderID)
	env.seedReadyMachine()
	return env
}

// startClusterCache runs the production ClusterCache setup, filter included, in its own manager.
func (env *npidReconcileEnv) startClusterCache() {
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  k8sClient.Scheme(),
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	Expect(err).NotTo(HaveOccurred())
	mgrCtx, stop := context.WithCancel(ctx)
	env.stopManager = stop
	env.cache, err = clustercache.SetupWithManager(mgrCtx, mgr, clustercache.Options{
		SecretClient:  mgr.GetClient(),
		Client:        clustercache.ClientOptions{UserAgent: "beskar7-npid-test"},
		ClusterFilter: NodeProviderIDClusterFilter,
	}, controller.Options{SkipNameValidation: ptr.To(true)})
	Expect(err).NotTo(HaveOccurred())
	go func() {
		defer GinkgoRecover()
		Expect(mgr.Start(mgrCtx)).To(Succeed())
	}()
}

func (env *npidReconcileEnv) seedCluster(setNodeProviderID bool) {
	env.cluster = &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: npidReconcileCluster, Namespace: env.ns},
		Spec: clusterv1.ClusterSpec{
			Paused:            ptr.To(false),
			InfrastructureRef: beskar7ClusterRef(npidReconcileCluster),
		},
	}
	Expect(k8sClient.Create(ctx, env.cluster)).To(Succeed())
	// ClusterCache connects only once the infrastructure is provisioned.
	env.cluster.Status.Initialization.InfrastructureProvisioned = ptr.To(true)
	Expect(k8sClient.Status().Update(ctx, env.cluster)).To(Succeed())

	Expect(k8sClient.Create(ctx, kubeconfig.GenerateSecret(env.cluster, kubeconfig.FromEnvTestConfig(cfg, env.cluster)))).To(Succeed())

	env.b7cluster = &infrav1.Beskar7Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: npidReconcileCluster, Namespace: env.ns,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: clusterv1.GroupVersion.String(), Kind: "Cluster",
				Name: env.cluster.Name, UID: env.cluster.UID,
			}},
		},
		Spec: infrav1.Beskar7ClusterSpec{SetNodeProviderID: setNodeProviderID},
	}
	Expect(k8sClient.Create(ctx, env.b7cluster)).To(Succeed())
	env.clusterRec = &Beskar7ClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Log: ctrl.Log.WithName("npid-cluster")}
}

// seedReadyMachine claims a Ready host for a Beskar7Machine and registers its Node without a providerID.
func (env *npidReconcileEnv) seedReadyMachine() {
	machineName := "npid-reconcile-" + rand.String(5)

	bootstrapData := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: machineName + "-bootstrap", Namespace: env.ns},
		Data:       map[string][]byte{"value": []byte("#cloud-config\n")},
	}
	Expect(k8sClient.Create(ctx, bootstrapData)).To(Succeed())

	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name: machineName, Namespace: env.ns,
			Labels: map[string]string{clusterv1.ClusterNameLabel: env.cluster.Name},
		},
		Spec: clusterv1.MachineSpec{
			ClusterName: env.cluster.Name,
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: infrav1.GroupVersion.Group, Kind: "Beskar7Machine", Name: machineName,
			},
			Bootstrap: clusterv1.Bootstrap{DataSecretName: ptr.To(bootstrapData.Name)},
		},
	}
	Expect(k8sClient.Create(ctx, machine)).To(Succeed())

	Expect(k8sClient.Create(ctx, bmcCredentialsSecret(env.ns))).To(Succeed())
	host := claimedPhysicalHost(env.ns, npidReconcileHost, machineName)
	Expect(k8sClient.Create(ctx, host)).To(Succeed())
	host.Status.State = infrav1.StateReady
	host.Status.Ready = true
	setTrue(host, infrav1.RedfishConnectionReadyCondition, infrav1.RedfishConnectedReason)
	setTrue(host, infrav1.HostInspectedCondition, infrav1.HostInspectedReason)
	Expect(k8sClient.Status().Update(ctx, host)).To(Succeed())
	Expect(k8sClient.Create(ctx, credentialSecret(host,
		boundCredentialData(machineName, "npid-token", 10*time.Minute, "npid-nonce", 5*time.Minute)))).To(Succeed())

	b7m := &infrav1.Beskar7Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name: machineName, Namespace: env.ns,
			Labels:     map[string]string{clusterv1.ClusterNameLabel: env.cluster.Name},
			Finalizers: []string{Beskar7MachineFinalizer},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: clusterv1.GroupVersion.String(), Kind: "Machine",
				Name: machine.Name, UID: machine.UID,
			}},
		},
		Spec: minimalImageSpec(),
	}
	Expect(k8sClient.Create(ctx, b7m)).To(Succeed())
	env.b7machine = client.ObjectKeyFromObject(b7m)

	// The suite's API server is the workload cluster too, and Nodes are cluster-scoped.
	env.node = &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: machineName}}
	Expect(k8sClient.Create(ctx, env.node)).To(Succeed())

	mockRf := internalredfish.NewMockClient()
	env.machineRec = &Beskar7MachineReconciler{
		Client: k8sClient, Scheme: k8sClient.Scheme(), Log: ctrl.Log.WithName("npid-machine"),
		ClusterCache: env.cache,
		RedfishClientFactory: func(context.Context, string, string, string, bool, []byte) (internalredfish.Client, error) {
			return mockRf, nil
		},
	}
}

func (env *npidReconcileEnv) teardown() {
	if env == nil {
		return
	}
	env.stopManager()
	if env.node != nil {
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, env.node))).To(Succeed())
	}
	Expect(k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: env.ns}})).To(Succeed())
}

// reconcileCluster drives the real Beskar7Cluster reconciler until the Cluster annotation matches want.
func (env *npidReconcileEnv) reconcileCluster(want bool) {
	Eventually(func(g Gomega) {
		_, err := env.clusterRec.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(env.b7cluster)})
		g.Expect(err).NotTo(HaveOccurred())
		cluster := &clusterv1.Cluster{}
		g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(env.cluster), cluster)).To(Succeed())
		g.Expect(NodeProviderIDClusterFilter(cluster)).To(Equal(want))
	}, npidEventually, npidPoll).Should(Succeed())
}

// reconcileMachine runs one real Beskar7Machine reconcile and returns the stored machine.
func (env *npidReconcileEnv) reconcileMachine(g Gomega) *infrav1.Beskar7Machine {
	_, err := env.machineRec.Reconcile(ctx, ctrl.Request{NamespacedName: env.b7machine})
	g.Expect(err).NotTo(HaveOccurred())
	b7m := &infrav1.Beskar7Machine{}
	g.Expect(k8sClient.Get(ctx, env.b7machine, b7m)).To(Succeed())
	return b7m
}

func (env *npidReconcileEnv) setSwitch(on bool) {
	b7cluster := &infrav1.Beskar7Cluster{}
	Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(env.b7cluster), b7cluster)).To(Succeed())
	before := b7cluster.DeepCopy()
	b7cluster.Spec.SetNodeProviderID = on
	Expect(k8sClient.Patch(ctx, b7cluster, client.MergeFrom(before))).To(Succeed())
}

func (env *npidReconcileEnv) nodeProviderID(g Gomega) string {
	node := &corev1.Node{}
	g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(env.node), node)).To(Succeed())
	return node.Spec.ProviderID
}

func (env *npidReconcileEnv) connected() bool {
	_, err := env.cache.GetUncachedClient(ctx, client.ObjectKeyFromObject(env.cluster))
	return err == nil
}

// expectReadyAndUntouched asserts the machine is Ready, carries no NodeProviderIDReady, and the cluster stays unconnected.
func (env *npidReconcileEnv) expectReadyAndUntouched() {
	Eventually(func(g Gomega) {
		b7m := env.reconcileMachine(g)
		g.Expect(b7m.Status.Ready).To(BeTrue())
		g.Expect(conditions.Has(b7m, infrav1.NodeProviderIDReadyCondition)).To(BeFalse())
	}, npidEventually, npidPoll).Should(Succeed())
	Consistently(func(g Gomega) {
		g.Expect(env.connected()).To(BeFalse(), "ClusterCache must never connect to a cluster that did not opt in")
		g.Expect(env.nodeProviderID(g)).To(BeEmpty())
	}, npidConsistently, npidPoll).Should(Succeed())
}

// expectNodeLinked asserts the real reconcile patched the Node and reported it.
func (env *npidReconcileEnv) expectNodeLinked() {
	want := providerID(env.ns, npidReconcileHost)
	Eventually(func(g Gomega) {
		b7m := env.reconcileMachine(g)
		g.Expect(env.nodeProviderID(g)).To(Equal(want))
		g.Expect(conditions.IsTrue(b7m, infrav1.NodeProviderIDReadyCondition)).To(BeTrue())
	}, npidEventually, npidPoll).Should(Succeed())
}

var _ = Describe("Node providerID through Reconcile with the real ClusterCache (#297)", func() {
	var env *npidReconcileEnv

	AfterEach(func() {
		env.teardown()
		env = nil
	})

	It("patches the Node of a cluster that opted in", func() {
		env = newNPIDReconcileEnv(true)
		env.reconcileCluster(true)

		env.expectNodeLinked()
	})

	It("never connects to a cluster that did not opt in, and sets no condition", func() {
		env = newNPIDReconcileEnv(false)
		env.reconcileCluster(false)

		env.expectReadyAndUntouched()
	})

	It("connects once the switch is turned on for a running cluster, and lets go when it is turned off", func() {
		env = newNPIDReconcileEnv(false)
		env.reconcileCluster(false)
		env.expectReadyAndUntouched()

		By("turning the switch on, with no restart and no other change to the Cluster")
		env.setSwitch(true)
		env.reconcileCluster(true)
		env.expectNodeLinked()

		By("turning it off again")
		env.setSwitch(false)
		env.reconcileCluster(false)
		Eventually(func(g Gomega) {
			b7m := env.reconcileMachine(g)
			g.Expect(conditions.Has(b7m, infrav1.NodeProviderIDReadyCondition)).To(BeFalse())
			g.Expect(env.connected()).To(BeFalse())
		}, npidEventually, npidPoll).Should(Succeed())
	})
})
