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

	. "github.com/onsi/gomega"

	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/controllers/clustercache"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"

	infrav1 "github.com/beskar7/beskar7/api/v1beta2"
)

func TestNodeProviderIDCandidate(t *testing.T) {
	g := NewWithT(t)
	p := nodeProviderIDCandidate()
	notReady := &infrav1.Beskar7Machine{Spec: infrav1.Beskar7MachineSpec{ProviderID: "b7://ns/host"}}
	ready := notReady.DeepCopy()
	ready.Status.Ready = true
	readyLinked := ready.DeepCopy()
	setTrue(readyLinked, infrav1.NodeProviderIDReadyCondition, infrav1.NodeProviderIDSetReason)
	leftover := notReady.DeepCopy()
	setTrue(leftover, infrav1.NodeProviderIDReadyCondition, infrav1.NodeProviderIDSetReason)

	g.Expect(p.Create(event.CreateEvent{Object: ready})).To(BeTrue(), "a Ready machine")
	g.Expect(p.Create(event.CreateEvent{Object: leftover})).To(BeTrue(), "a condition to clear")
	g.Expect(p.Create(event.CreateEvent{Object: notReady})).To(BeFalse())
	g.Expect(p.Update(event.UpdateEvent{ObjectOld: notReady, ObjectNew: ready})).To(BeTrue(), "became Ready")
	g.Expect(p.Update(event.UpdateEvent{ObjectOld: ready, ObjectNew: readyLinked})).To(BeFalse(), "own status write")
	g.Expect(p.Delete(event.DeleteEvent{Object: ready})).To(BeFalse())

	pausedReady := ready.DeepCopy()
	pausedReady.Annotations = map[string]string{clusterv1.PausedAnnotation: "true"}
	g.Expect(p.Update(event.UpdateEvent{ObjectOld: pausedReady, ObjectNew: ready})).To(BeTrue(), "unpaused")
	g.Expect(p.Update(event.UpdateEvent{ObjectOld: ready, ObjectNew: pausedReady})).To(BeTrue(), "paused")
}

func TestNodeProviderIDClusterChanged(t *testing.T) {
	g := NewWithT(t)
	p := nodeProviderIDClusterChanged(nodeProviderIDScheme(t), ctrl.Log.WithName("npid-predicate"))
	running := clusterWith(beskar7ClusterRef("x"), nil)
	running.Spec.Paused = ptr.To(false)
	pausedSpec := running.DeepCopy()
	pausedSpec.Spec.Paused = ptr.To(true)
	pausedAnnotation := running.DeepCopy()
	pausedAnnotation.Annotations = map[string]string{clusterv1.PausedAnnotation: "true"}
	optedIn := running.DeepCopy()
	optedIn.Annotations = map[string]string{NodeProviderIDAnnotation: NodeProviderIDAnnotationValue}
	statusOnly := running.DeepCopy()
	statusOnly.Status.Phase = "Provisioned"

	g.Expect(p.Update(event.UpdateEvent{ObjectOld: pausedSpec, ObjectNew: running})).To(BeTrue(), "spec.paused unpaused")
	g.Expect(p.Update(event.UpdateEvent{ObjectOld: running, ObjectNew: pausedSpec})).To(BeTrue(), "spec.paused paused")
	unpausedByAnnotation := event.UpdateEvent{ObjectOld: pausedAnnotation, ObjectNew: running}
	g.Expect(p.Update(unpausedByAnnotation)).To(BeTrue(), "annotation unpaused")
	g.Expect(p.Update(event.UpdateEvent{ObjectOld: running, ObjectNew: optedIn})).To(BeTrue(), "opted in")
	g.Expect(p.Update(event.UpdateEvent{ObjectOld: running, ObjectNew: statusOnly})).To(BeFalse(), "status churn")
	g.Expect(p.Create(event.CreateEvent{Object: running})).To(BeFalse(), "creates are not transitions")
	g.Expect(p.Delete(event.DeleteEvent{Object: running})).To(BeFalse())
}

// A machine deleted while the workload call runs is gone, which is not a reconcile error.
func TestNodeProviderIDReconcileMachineDeletedMidReconcile(t *testing.T) {
	g := NewWithT(t)
	f := newNodeProviderIDFixture(nodeProviderIDTestNamespace)
	management := fake.NewClientBuilder().WithScheme(nodeProviderIDScheme(t)).
		WithObjects(f.cluster, f.b7cluster, f.machine, f.b7machine).
		WithStatusSubresource(&infrav1.Beskar7Machine{}).Build()
	workload := interceptor.NewClient(workloadWith(t, registeredNode("")).(client.WithWatch), interceptor.Funcs{
		Get: func(
			ctx context.Context,
			c client.WithWatch,
			key client.ObjectKey,
			obj client.Object,
			opts ...client.GetOption,
		) error {
			// The Beskar7Machine controller removes the last finalizer meanwhile.
			if err := management.Delete(ctx, f.b7machine.DeepCopy()); err != nil {
				return err
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	r := &NodeProviderIDReconciler{
		Client:       management,
		Log:          ctrl.Log.WithName("npid-controller"),
		ClusterCache: clustercache.NewFakeClusterCache(workload, f.clusterKey()),
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.b7machine)})

	g.Expect(err).NotTo(HaveOccurred())
}

// Reconcile writes NodeProviderIDReady and leaves the conditions the Beskar7Machine controller owns as they are.
func TestNodeProviderIDReconcileKeepsOtherConditions(t *testing.T) {
	g := NewWithT(t)
	f := newNodeProviderIDFixture(nodeProviderIDTestNamespace)
	setTrue(f.b7machine, infrav1.InfrastructureReadyCondition, infrav1.ProvisionedReason)
	management := fake.NewClientBuilder().WithScheme(nodeProviderIDScheme(t)).
		WithObjects(f.cluster, f.b7cluster, f.machine, f.b7machine).
		WithStatusSubresource(&infrav1.Beskar7Machine{}).Build()
	workload := workloadWith(t, registeredNode(""))
	r := &NodeProviderIDReconciler{
		Client:       management,
		Log:          ctrl.Log.WithName("npid-controller"),
		ClusterCache: clustercache.NewFakeClusterCache(workload, f.clusterKey()),
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.b7machine)})

	g.Expect(err).NotTo(HaveOccurred())
	stored := &infrav1.Beskar7Machine{}
	g.Expect(management.Get(context.Background(), client.ObjectKeyFromObject(f.b7machine), stored)).To(Succeed())
	g.Expect(conditions.IsTrue(stored, infrav1.NodeProviderIDReadyCondition)).To(BeTrue())
	g.Expect(conditions.IsTrue(stored, infrav1.InfrastructureReadyCondition)).To(BeTrue())
}

func TestReconcileNodeProviderID_NotReadyNeverAsksClusterCache(t *testing.T) {
	g := NewWithT(t)
	f := newNodeProviderIDFixture(nodeProviderIDTestNamespace)
	f.b7machine.Status.Ready = false

	// An empty cache would report the cluster unreachable if it were asked.
	result, err := nodeProviderIDCase{fixture: f, cache: clustercache.NewFakeEmptyClusterCache()}.run(t)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())
	g.Expect(nodeProviderIDCondition(f.b7machine)).To(BeNil())
}

func TestReconcileNodeProviderID_PausedNeverAsksClusterCache(t *testing.T) {
	cases := map[string]func(f nodeProviderIDFixture){
		"Cluster.spec.paused": func(f nodeProviderIDFixture) { f.cluster.Spec.Paused = ptr.To(true) },
		"paused annotation on the machine": func(f nodeProviderIDFixture) {
			f.b7machine.Annotations = map[string]string{clusterv1.PausedAnnotation: "true"}
		},
	}
	for name, pause := range cases {
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)
			f := newNodeProviderIDFixture(nodeProviderIDTestNamespace)
			pause(f)

			result, err := nodeProviderIDCase{fixture: f, cache: clustercache.NewFakeEmptyClusterCache()}.run(t)

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(result.IsZero()).To(BeTrue())
			g.Expect(nodeProviderIDCondition(f.b7machine)).To(BeNil())
		})
	}
}
