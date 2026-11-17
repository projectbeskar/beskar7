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
	"errors"
	"maps"
	"testing"

	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
)

// clusterWith copies annotations, since the mirror under test edits them in place.
func clusterWith(ref clusterv1.ContractVersionedObjectReference, annotations map[string]string) *clusterv1.Cluster {
	return &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: nodeProviderIDTestCluster, Namespace: nodeProviderIDTestNamespace, Annotations: maps.Clone(annotations)},
		Spec:       clusterv1.ClusterSpec{InfrastructureRef: ref},
	}
}

func beskar7ClusterWith(name string, setNodeProviderID bool) *infrav1.Beskar7Cluster {
	return &infrav1.Beskar7Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: nodeProviderIDTestNamespace},
		Spec:       infrav1.Beskar7ClusterSpec{SetNodeProviderID: setNodeProviderID},
	}
}

func TestNodeProviderIDClusterFilter(t *testing.T) {
	annotated := map[string]string{NodeProviderIDAnnotation: NodeProviderIDAnnotationValue}
	foreignKind := clusterv1.ContractVersionedObjectReference{
		APIGroup: infrav1.GroupVersion.Group, Kind: "Metal3Cluster", Name: "on",
	}
	foreignGroup := clusterv1.ContractVersionedObjectReference{
		APIGroup: "example.com", Kind: beskar7ClusterKind, Name: "on",
	}
	reader := fake.NewClientBuilder().WithScheme(nodeProviderIDScheme(t)).
		WithObjects(beskar7ClusterWith("on", true), beskar7ClusterWith("off", false)).Build()

	cases := map[string]struct {
		cluster *clusterv1.Cluster
		want    bool
	}{
		"nil Cluster":                      {cluster: nil},
		"no infrastructureRef":             {cluster: clusterWith(clusterv1.ContractVersionedObjectReference{}, annotated)},
		"another provider's kind":          {cluster: clusterWith(foreignKind, annotated)},
		"Beskar7Cluster kind, other group": {cluster: clusterWith(foreignGroup, annotated)},
		"Beskar7Cluster does not exist":    {cluster: clusterWith(beskar7ClusterRef("missing"), annotated)},
		"spec off, annotation set by hand": {cluster: clusterWith(beskar7ClusterRef("off"), annotated)},
		"spec on, annotation not yet set":  {cluster: clusterWith(beskar7ClusterRef("on"), nil), want: true},
		"spec on, annotation set":          {cluster: clusterWith(beskar7ClusterRef("on"), annotated), want: true},
	}
	filter := NewNodeProviderIDClusterFilter(reader)
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			NewWithT(t).Expect(filter(tc.cluster)).To(Equal(tc.want))
		})
	}
}

func TestNodeProviderIDClusterFilterFailsClosed(t *testing.T) {
	base := fake.NewClientBuilder().WithScheme(nodeProviderIDScheme(t)).
		WithObjects(beskar7ClusterWith("on", true)).Build()
	reader := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("API server unavailable")
		},
	})

	filter := NewNodeProviderIDClusterFilter(reader)

	NewWithT(t).Expect(filter(clusterWith(beskar7ClusterRef("on"), nil))).To(BeFalse())
}

// mirrorCase runs reconcileNodeProviderIDAnnotation once and returns the stored Cluster and the patch count.
func mirrorCase(t *testing.T, cluster *clusterv1.Cluster, setNodeProviderID bool) (*clusterv1.Cluster, int) {
	t.Helper()
	patches := 0
	base := fake.NewClientBuilder().WithScheme(nodeProviderIDScheme(t)).WithObjects(cluster).Build()
	c := interceptor.NewClient(base, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			patches++
			return c.Patch(ctx, obj, patch, opts...)
		},
	})
	r := &Beskar7ClusterReconciler{Client: c, Log: ctrl.Log.WithName("npid-mirror")}
	b7cluster := beskar7ClusterWith(cluster.Spec.InfrastructureRef.Name, setNodeProviderID)

	if err := r.reconcileNodeProviderIDAnnotation(context.Background(), r.Log, cluster, b7cluster); err != nil {
		t.Fatalf("mirror: %v", err)
	}
	stored := &clusterv1.Cluster{}
	if err := base.Get(context.Background(), client.ObjectKeyFromObject(cluster), stored); err != nil {
		t.Fatalf("get Cluster: %v", err)
	}
	return stored, patches
}

func TestReconcileNodeProviderIDAnnotation(t *testing.T) {
	other := map[string]string{"keep": "me"}
	cases := map[string]struct {
		annotations map[string]string
		switchOn    bool
		wantPatches int
		wantValue   string
		wantHas     bool
	}{
		"on adds the annotation":        {annotations: other, switchOn: true, wantPatches: 1, wantValue: NodeProviderIDAnnotationValue, wantHas: true},
		"on with no annotations at all": {switchOn: true, wantPatches: 1, wantValue: NodeProviderIDAnnotationValue, wantHas: true},
		"on and already mirrored":       {annotations: map[string]string{NodeProviderIDAnnotation: NodeProviderIDAnnotationValue}, switchOn: true, wantValue: NodeProviderIDAnnotationValue, wantHas: true},
		"on fixes a hand-edited value":  {annotations: map[string]string{NodeProviderIDAnnotation: "yes"}, switchOn: true, wantPatches: 1, wantValue: NodeProviderIDAnnotationValue, wantHas: true},
		"off removes the annotation":    {annotations: map[string]string{NodeProviderIDAnnotation: NodeProviderIDAnnotationValue, "keep": "me"}, wantPatches: 1},
		"off and ours is the only one": {
			annotations: map[string]string{NodeProviderIDAnnotation: NodeProviderIDAnnotationValue},
			wantPatches: 1,
		},
		"off and nothing to remove": {annotations: other},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)
			stored, patches := mirrorCase(t, clusterWith(beskar7ClusterRef("x"), tc.annotations), tc.switchOn)

			g.Expect(patches).To(Equal(tc.wantPatches))
			value, has := stored.Annotations[NodeProviderIDAnnotation]
			g.Expect(has).To(Equal(tc.wantHas))
			g.Expect(value).To(Equal(tc.wantValue))
			if tc.annotations["keep"] != "" {
				g.Expect(stored.Annotations).To(HaveKeyWithValue("keep", "me"), "other annotations survive the patch")
			}
		})
	}
}

// Switching off must delete only our key, even when ours was the only annotation we read.
func TestReconcileNodeProviderIDAnnotationOffKeepsConcurrentAnnotations(t *testing.T) {
	g := NewWithT(t)
	read := clusterWith(
		beskar7ClusterRef("x"),
		map[string]string{NodeProviderIDAnnotation: NodeProviderIDAnnotationValue})

	// Another writer paused the Cluster after our read.
	live := read.DeepCopy()
	live.Annotations[clusterv1.PausedAnnotation] = "true"
	c := fake.NewClientBuilder().WithScheme(nodeProviderIDScheme(t)).WithObjects(live).Build()
	r := &Beskar7ClusterReconciler{Client: c, Log: ctrl.Log.WithName("npid-mirror")}

	err := r.reconcileNodeProviderIDAnnotation(context.Background(), r.Log, read, beskar7ClusterWith("x", false))
	g.Expect(err).NotTo(HaveOccurred())

	stored := &clusterv1.Cluster{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(read), stored)).To(Succeed())
	g.Expect(stored.Annotations).NotTo(HaveKey(NodeProviderIDAnnotation))
	g.Expect(stored.Annotations).To(HaveKeyWithValue(clusterv1.PausedAnnotation, "true"))
}

// A Beskar7Cluster owned by the Cluster but not named by its infrastructureRef switches nothing.
func TestReconcileNodeProviderIDAnnotationIgnoresAnotherBeskar7Cluster(t *testing.T) {
	g := NewWithT(t)
	cluster := clusterWith(beskar7ClusterRef("x"), nil)
	c := fake.NewClientBuilder().WithScheme(nodeProviderIDScheme(t)).WithObjects(cluster).Build()
	r := &Beskar7ClusterReconciler{Client: c, Log: ctrl.Log.WithName("npid-mirror")}

	err := r.reconcileNodeProviderIDAnnotation(context.Background(), r.Log, cluster, beskar7ClusterWith("x-2", true))
	g.Expect(err).NotTo(HaveOccurred())

	stored := &clusterv1.Cluster{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(cluster), stored)).To(Succeed())
	g.Expect(stored.Annotations).NotTo(HaveKey(NodeProviderIDAnnotation))
}

func TestNodeProviderIDAnnotationChanged(t *testing.T) {
	g := NewWithT(t)
	p := nodeProviderIDAnnotationChanged()
	off := clusterWith(beskar7ClusterRef("x"), nil)
	on := clusterWith(beskar7ClusterRef("x"), map[string]string{NodeProviderIDAnnotation: NodeProviderIDAnnotationValue})
	statusOnly := on.DeepCopy()
	statusOnly.Status.Phase = "Provisioned"

	g.Expect(p.Update(event.UpdateEvent{ObjectOld: off, ObjectNew: on})).To(BeTrue(), "turned on")
	g.Expect(p.Update(event.UpdateEvent{ObjectOld: on, ObjectNew: off})).To(BeTrue(), "turned off")
	g.Expect(p.Update(event.UpdateEvent{ObjectOld: on, ObjectNew: statusOnly})).To(BeFalse(), "status churn is ignored")
	g.Expect(p.Create(event.CreateEvent{Object: on})).To(BeFalse())
	g.Expect(p.Delete(event.DeleteEvent{Object: on})).To(BeFalse())
}

func TestClusterToBeskar7Machines(t *testing.T) {
	g := NewWithT(t)
	labelled := func(name, cluster string) *infrav1.Beskar7Machine {
		return &infrav1.Beskar7Machine{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: nodeProviderIDTestNamespace,
			Labels: map[string]string{clusterv1.ClusterNameLabel: cluster},
		}}
	}
	c := fake.NewClientBuilder().WithScheme(nodeProviderIDScheme(t)).WithObjects(
		labelled("mine-a", nodeProviderIDTestCluster),
		labelled("mine-b", nodeProviderIDTestCluster),
		labelled("theirs", "other-cluster"),
	).Build()
	r := &Beskar7MachineReconciler{Client: c, Log: ctrl.Log.WithName("npid-map")}

	requests := r.ClusterToBeskar7Machines(context.Background(), clusterWith(beskar7ClusterRef("x"), nil))

	names := make([]string, 0, len(requests))
	for _, req := range requests {
		names = append(names, req.Name)
	}
	g.Expect(names).To(ConsistOf("mine-a", "mine-b"))
}
