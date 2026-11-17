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
	"fmt"
	"time"

	"github.com/go-logr/logr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
)

const (
	// beskar7ClusterKind is the infrastructure kind a Cluster must reference to opt in.
	beskar7ClusterKind = "Beskar7Cluster"

	// nodeProviderIDFilterTimeout bounds the filter's Beskar7Cluster read, which has no caller context.
	nodeProviderIDFilterTimeout = 5 * time.Second
)

// beskar7ClusterKey is the key of the Beskar7Cluster the Cluster's infrastructureRef names; false for any other ref.
func beskar7ClusterKey(cluster *clusterv1.Cluster) (client.ObjectKey, bool) {
	if cluster == nil {
		return client.ObjectKey{}, false
	}
	ref := cluster.Spec.InfrastructureRef
	if ref.Kind != beskar7ClusterKind || ref.APIGroup != infrav1.GroupVersion.Group || ref.Name == "" {
		return client.ObjectKey{}, false
	}
	return client.ObjectKey{Namespace: cluster.Namespace, Name: ref.Name}, true
}

// nodeProviderIDOptedIn reads spec.setNodeProviderID from the Beskar7Cluster the Cluster's infrastructureRef names.
func nodeProviderIDOptedIn(ctx context.Context, reader client.Reader, cluster *clusterv1.Cluster) (bool, error) {
	key, ok := beskar7ClusterKey(cluster)
	if !ok {
		return false, nil
	}
	b7cluster := &infrav1.Beskar7Cluster{}
	if err := reader.Get(ctx, key, b7cluster); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	return b7cluster.Spec.SetNodeProviderID, nil
}

// NewNodeProviderIDClusterFilter returns the ClusterCache filter, which passes only a Cluster whose Beskar7Cluster
// opted in. NodeProviderIDAnnotation only makes ClusterCache run it again; a failed read keeps the Cluster out.
func NewNodeProviderIDClusterFilter(reader client.Reader) func(*clusterv1.Cluster) bool {
	logger := ctrl.Log.WithName("node-provider-id-filter")
	return func(cluster *clusterv1.Cluster) bool {
		ctx, cancel := context.WithTimeout(context.Background(), nodeProviderIDFilterTimeout)
		defer cancel()
		optedIn, err := nodeProviderIDOptedIn(ctx, reader, cluster)
		if err != nil {
			logger.Error(err, "Failed to read the Beskar7Cluster; not connecting",
				"cluster", client.ObjectKeyFromObject(cluster))
			return false
		}
		return optedIn
	}
}

// reconcileNodeProviderIDAnnotation mirrors spec.setNodeProviderID onto the owner Cluster as NodeProviderIDAnnotation.
func (r *Beskar7ClusterReconciler) reconcileNodeProviderIDAnnotation(ctx context.Context, logger logr.Logger, cluster *clusterv1.Cluster, b7cluster *infrav1.Beskar7Cluster) error {
	// With the operator switch off the manager writes nothing to the Cluster, so no tenant sees an opt-in take effect.
	if !r.NodeProviderIDEnabled {
		return nil
	}
	// Only the Beskar7Cluster the Cluster's infrastructureRef names may switch it.
	if key, ok := beskar7ClusterKey(cluster); !ok || key.Name != b7cluster.Name {
		logger.V(1).Info("Cluster's infrastructureRef names another object; not mirroring setNodeProviderID")
		return nil
	}

	want := b7cluster.Spec.SetNodeProviderID
	value, has := cluster.GetAnnotations()[NodeProviderIDAnnotation]
	if want && value == NodeProviderIDAnnotationValue {
		return nil
	}
	if !want && !has {
		return nil
	}

	// Deletes only our key: a MergeFrom of an emptied map sends "annotations": null and drops other writers' keys.
	patch := removeAnnotationsPatch(NodeProviderIDAnnotation)
	if want {
		before := cluster.DeepCopy()
		annotations := cluster.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[NodeProviderIDAnnotation] = NodeProviderIDAnnotationValue
		cluster.SetAnnotations(annotations)
		patch = client.MergeFrom(before)
	}
	if err := r.Patch(ctx, cluster, patch); err != nil {
		return fmt.Errorf("mirror setNodeProviderID onto Cluster %q: %w", cluster.Name, err)
	}
	logger.Info("Mirrored setNodeProviderID onto the Cluster", "setNodeProviderID", want)
	return nil
}

// nodeProviderIDAnnotationChanged passes a Cluster update only when NodeProviderIDAnnotation changed.
func nodeProviderIDAnnotationChanged() predicate.Funcs {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return false },
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			if e.ObjectOld == nil || e.ObjectNew == nil {
				return false
			}
			return e.ObjectOld.GetAnnotations()[NodeProviderIDAnnotation] != e.ObjectNew.GetAnnotations()[NodeProviderIDAnnotation]
		},
	}
}

// ClusterToBeskar7Machines maps a Cluster to the Beskar7Machines labelled with its name.
func (r *NodeProviderIDReconciler) ClusterToBeskar7Machines(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	cluster, ok := obj.(*clusterv1.Cluster)
	if !ok {
		return nil
	}
	machines := &infrav1.Beskar7MachineList{}
	if err := r.List(ctx, machines, client.InNamespace(cluster.Namespace),
		client.MatchingLabels{clusterv1.ClusterNameLabel: cluster.Name}); err != nil {
		r.Log.Error(err, "Failed to list Beskar7Machines of a Cluster", "cluster", client.ObjectKeyFromObject(cluster))
		return nil
	}
	requests := make([]reconcile.Request, 0, len(machines.Items))
	for i := range machines.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&machines.Items[i])})
	}
	return requests
}
