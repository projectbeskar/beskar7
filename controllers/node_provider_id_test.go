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
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/controllers/clustercache"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
)

const (
	nodeProviderIDTestNamespace = "npid"
	nodeProviderIDTestCluster   = "npid-cluster"
	nodeProviderIDTestMachine   = "npid-workers-abc12"
	nodeProviderIDTestHost      = "npid-host"
	foreignProviderID           = "k3s://npid-workers-abc12"
)

// nodeProviderIDFixture is a Beskar7Machine with its owner Machine and Cluster.
type nodeProviderIDFixture struct {
	cluster   *clusterv1.Cluster
	machine   *clusterv1.Machine
	b7machine *infrav1.Beskar7Machine
}

func newNodeProviderIDFixture(namespace string) nodeProviderIDFixture {
	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeProviderIDTestCluster, Namespace: namespace,
			Annotations: map[string]string{NodeProviderIDAnnotation: NodeProviderIDAnnotationValue},
		},
		Spec: clusterv1.ClusterSpec{
			Paused:            ptr.To(false),
			InfrastructureRef: beskar7ClusterRef(nodeProviderIDTestCluster),
		},
	}
	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeProviderIDTestMachine, Namespace: namespace, UID: "npid-machine-uid",
			Labels: map[string]string{clusterv1.ClusterNameLabel: nodeProviderIDTestCluster},
		},
		Spec: clusterv1.MachineSpec{
			ClusterName:       nodeProviderIDTestCluster,
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{APIGroup: infrav1.GroupVersion.Group, Kind: "Beskar7Machine", Name: nodeProviderIDTestMachine},
			Bootstrap:         clusterv1.Bootstrap{DataSecretName: ptr.To("npid-bootstrap")},
		},
	}
	b7machine := &infrav1.Beskar7Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeProviderIDTestMachine, Namespace: namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: clusterv1.GroupVersion.String(), Kind: "Machine",
				Name: machine.Name, UID: machine.UID,
			}},
		},
		Spec: infrav1.Beskar7MachineSpec{ProviderID: providerID(namespace, nodeProviderIDTestHost)},
	}
	return nodeProviderIDFixture{cluster: cluster, machine: machine, b7machine: b7machine}
}

func beskar7ClusterRef(name string) clusterv1.ContractVersionedObjectReference {
	return clusterv1.ContractVersionedObjectReference{APIGroup: infrav1.GroupVersion.Group, Kind: beskar7ClusterKind, Name: name}
}

// optOut turns the fixture's Cluster into one that has not opted in.
func (f nodeProviderIDFixture) optOut() {
	delete(f.cluster.Annotations, NodeProviderIDAnnotation)
}

func (f nodeProviderIDFixture) clusterKey() client.ObjectKey {
	return client.ObjectKeyFromObject(f.cluster)
}

func nodeProviderIDScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := newSchemeForTest(t)
	if err := clusterv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add cluster-api to scheme: %v", err)
	}
	return scheme
}

func registeredNode(providerID string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: nodeProviderIDTestMachine},
		Spec:       corev1.NodeSpec{ProviderID: providerID},
	}
}

// nodeProviderIDCase is one reconcileNodeProviderID run against fake clients.
type nodeProviderIDCase struct {
	fixture  nodeProviderIDFixture
	workload client.Client
	cache    clustercache.ClusterCache
}

func (c nodeProviderIDCase) run(t *testing.T) (ctrl.Result, error) {
	t.Helper()
	management := fake.NewClientBuilder().WithScheme(nodeProviderIDScheme(t)).
		WithObjects(c.fixture.cluster, c.fixture.machine).Build()
	r := &Beskar7MachineReconciler{Client: management, ClusterCache: c.cache, Log: ctrl.Log.WithName("npid")}
	return r.reconcileNodeProviderID(context.Background(), r.Log, c.fixture.b7machine)
}

func workloadWith(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(nodeProviderIDScheme(t)).WithObjects(objs...).Build()
}

func nodeProviderIDCondition(b7m *infrav1.Beskar7Machine) *metav1.Condition {
	return conditions.Get(b7m, infrav1.NodeProviderIDReadyCondition)
}

func TestReconcileNodeProviderID_SetsEmptyProviderID(t *testing.T) {
	g := NewWithT(t)
	f := newNodeProviderIDFixture(nodeProviderIDTestNamespace)
	workload := workloadWith(t, registeredNode(""))

	result, err := nodeProviderIDCase{fixture: f, workload: workload, cache: clustercache.NewFakeClusterCache(workload, f.clusterKey())}.run(t)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())
	node := &corev1.Node{}
	g.Expect(workload.Get(context.Background(), client.ObjectKey{Name: nodeProviderIDTestMachine}, node)).To(Succeed())
	g.Expect(node.Spec.ProviderID).To(Equal(f.b7machine.Spec.ProviderID))
	cond := nodeProviderIDCondition(f.b7machine)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(infrav1.NodeProviderIDSetReason))
}

func TestReconcileNodeProviderID_EqualProviderIDNeedsNoPatch(t *testing.T) {
	g := NewWithT(t)
	f := newNodeProviderIDFixture(nodeProviderIDTestNamespace)
	patched := false
	workload := interceptor.NewClient(workloadWith(t, registeredNode(f.b7machine.Spec.ProviderID)).(client.WithWatch), interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			patched = true
			return c.Patch(ctx, obj, patch, opts...)
		},
	})

	result, err := nodeProviderIDCase{fixture: f, workload: workload, cache: clustercache.NewFakeClusterCache(workload, f.clusterKey())}.run(t)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())
	g.Expect(patched).To(BeFalse())
	g.Expect(nodeProviderIDCondition(f.b7machine).Reason).To(Equal(infrav1.NodeProviderIDSetReason))
}

func TestReconcileNodeProviderID_MismatchIsReportedNotOverwritten(t *testing.T) {
	g := NewWithT(t)
	f := newNodeProviderIDFixture(nodeProviderIDTestNamespace)
	workload := workloadWith(t, registeredNode(foreignProviderID))

	result, err := nodeProviderIDCase{fixture: f, workload: workload, cache: clustercache.NewFakeClusterCache(workload, f.clusterKey())}.run(t)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(nodeProviderIDMismatchRequeue))
	node := &corev1.Node{}
	g.Expect(workload.Get(context.Background(), client.ObjectKey{Name: nodeProviderIDTestMachine}, node)).To(Succeed())
	g.Expect(node.Spec.ProviderID).To(Equal(foreignProviderID))
	cond := nodeProviderIDCondition(f.b7machine)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(infrav1.NodeProviderIDMismatchReason))
	g.Expect(cond.Message).To(ContainSubstring(foreignProviderID))
	g.Expect(cond.Message).To(ContainSubstring(f.b7machine.Spec.ProviderID))
}

func TestReconcileNodeProviderID_NodeNotFound(t *testing.T) {
	g := NewWithT(t)
	f := newNodeProviderIDFixture(nodeProviderIDTestNamespace)
	workload := workloadWith(t)

	result, err := nodeProviderIDCase{fixture: f, workload: workload, cache: clustercache.NewFakeClusterCache(workload, f.clusterKey())}.run(t)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(nodeNotFoundRequeue))
	g.Expect(nodeProviderIDCondition(f.b7machine).Reason).To(Equal(infrav1.NodeNotFoundReason))
}

func TestReconcileNodeProviderID_WorkloadClusterUnreachable(t *testing.T) {
	g := NewWithT(t)
	f := newNodeProviderIDFixture(nodeProviderIDTestNamespace)

	result, err := nodeProviderIDCase{fixture: f, cache: clustercache.NewFakeEmptyClusterCache()}.run(t)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(workloadClusterUnreachableRequeue))
	g.Expect(nodeProviderIDCondition(f.b7machine).Reason).To(Equal(infrav1.WorkloadClusterUnreachableReason))
}

func TestReconcileNodeProviderID_NodeRefSkipsWorkloadCluster(t *testing.T) {
	g := NewWithT(t)
	f := newNodeProviderIDFixture(nodeProviderIDTestNamespace)
	f.machine.Status.NodeRef = clusterv1.MachineNodeReference{Name: nodeProviderIDTestMachine}

	// An empty cache would report the cluster unreachable if it were asked.
	result, err := nodeProviderIDCase{fixture: f, cache: clustercache.NewFakeEmptyClusterCache()}.run(t)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())
	g.Expect(nodeProviderIDCondition(f.b7machine).Reason).To(Equal(infrav1.NodeProviderIDSetReason))
}

func TestReconcileNodeProviderID_NoClusterCacheIsNoop(t *testing.T) {
	g := NewWithT(t)
	f := newNodeProviderIDFixture(nodeProviderIDTestNamespace)

	result, err := nodeProviderIDCase{fixture: f}.run(t)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())
	g.Expect(nodeProviderIDCondition(f.b7machine)).To(BeNil())
}

func TestReconcileNodeProviderID_OptedOutNeverAsksClusterCache(t *testing.T) {
	g := NewWithT(t)
	f := newNodeProviderIDFixture(nodeProviderIDTestNamespace)
	f.optOut()

	// An empty cache would report the cluster unreachable if it were asked.
	result, err := nodeProviderIDCase{fixture: f, cache: clustercache.NewFakeEmptyClusterCache()}.run(t)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())
	g.Expect(nodeProviderIDCondition(f.b7machine)).To(BeNil())
}

func TestReconcileNodeProviderID_OptingOutRemovesCondition(t *testing.T) {
	g := NewWithT(t)
	f := newNodeProviderIDFixture(nodeProviderIDTestNamespace)
	f.optOut()
	setFalse(f.b7machine, infrav1.NodeProviderIDReadyCondition, infrav1.NodeNotFoundReason, "left over")

	result, err := nodeProviderIDCase{fixture: f, cache: clustercache.NewFakeEmptyClusterCache()}.run(t)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())
	g.Expect(nodeProviderIDCondition(f.b7machine)).To(BeNil())
}

func TestReconcileNodeProviderID_OptedOutWithNodeRefGetsNoCondition(t *testing.T) {
	g := NewWithT(t)
	f := newNodeProviderIDFixture(nodeProviderIDTestNamespace)
	f.optOut()
	f.machine.Status.NodeRef = clusterv1.MachineNodeReference{Name: nodeProviderIDTestMachine}

	_, err := nodeProviderIDCase{fixture: f, cache: clustercache.NewFakeEmptyClusterCache()}.run(t)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(nodeProviderIDCondition(f.b7machine)).To(BeNil())
}

func TestReconcileNodeProviderID_PatchRejectionIsAnError(t *testing.T) {
	g := NewWithT(t)
	f := newNodeProviderIDFixture(nodeProviderIDTestNamespace)
	workload := interceptor.NewClient(workloadWith(t, registeredNode("")).(client.WithWatch), interceptor.Funcs{
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			return apierrors.NewInvalid(schema.GroupKind{Kind: "Node"}, nodeProviderIDTestMachine, nil)
		},
	})

	_, err := nodeProviderIDCase{fixture: f, workload: workload, cache: clustercache.NewFakeClusterCache(workload, f.clusterKey())}.run(t)

	g.Expect(err).To(HaveOccurred())
	g.Expect(nodeProviderIDCondition(f.b7machine)).To(BeNil())
}

// The suite's API server stands in for the workload cluster, so the Node immutability rules are real.
var _ = Describe("Node providerID on the workload cluster (#297)", func() {
	var (
		testNs *corev1.Namespace
		f      nodeProviderIDFixture
		node   *corev1.Node
		r      *Beskar7MachineReconciler
	)

	BeforeEach(func() {
		testNs = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "node-provider-id-"}}
		Expect(k8sClient.Create(ctx, testNs)).To(Succeed())
		f = newNodeProviderIDFixture(testNs.Name)
		// Nodes are cluster-scoped, so each spec names its own.
		f.machine.Name = nodeProviderIDTestMachine + "-" + rand.String(5)
		f.machine.UID = ""
		Expect(k8sClient.Create(ctx, f.cluster)).To(Succeed())
		Expect(k8sClient.Create(ctx, f.machine)).To(Succeed())
		f.b7machine.OwnerReferences[0].Name = f.machine.Name
		f.b7machine.OwnerReferences[0].UID = f.machine.UID
		node = &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: f.machine.Name}}
		r = &Beskar7MachineReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Log: ctrl.Log.WithName("node-provider-id"),
			ClusterCache: clustercache.NewFakeClusterCache(k8sClient, f.clusterKey()),
		}
	})

	AfterEach(func() {
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, node))).To(Succeed())
		Expect(k8sClient.Delete(ctx, testNs)).To(Succeed())
	})

	It("sets the providerID of a Node registered without one", func() {
		Expect(k8sClient.Create(ctx, node)).To(Succeed())

		result, err := r.reconcileNodeProviderID(ctx, r.Log, f.b7machine)

		Expect(err).NotTo(HaveOccurred())
		Expect(result.IsZero()).To(BeTrue())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(node), node)).To(Succeed())
		Expect(node.Spec.ProviderID).To(Equal(f.b7machine.Spec.ProviderID))
		Expect(conditions.IsTrue(f.b7machine, infrav1.NodeProviderIDReadyCondition)).To(BeTrue())
	})

	It("never overwrites a providerID set after the read", func() {
		node.Spec.ProviderID = foreignProviderID
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		base, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
		Expect(err).NotTo(HaveOccurred())
		// The read sees the Node as it was before another writer set its providerID.
		stale := interceptor.NewClient(base, interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if err := c.Get(ctx, key, obj, opts...); err != nil {
					return err
				}
				if n, ok := obj.(*corev1.Node); ok {
					n.Spec.ProviderID = ""
				}
				return nil
			},
		})
		r.ClusterCache = clustercache.NewFakeClusterCache(stale, f.clusterKey())

		_, err = r.reconcileNodeProviderID(ctx, r.Log, f.b7machine)

		Expect(err).To(HaveOccurred())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(node), node)).To(Succeed())
		Expect(node.Spec.ProviderID).To(Equal(foreignProviderID))
	})
})
