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

	"github.com/go-logr/logr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
)

// beskar7ClusterKind is the infrastructure kind a Cluster must reference to opt in.
const beskar7ClusterKind = "Beskar7Cluster"

// NodeProviderIDClusterFilter is the ClusterCache filter: it passes only a Cluster backed by a Beskar7Cluster that opted in.
func NodeProviderIDClusterFilter(cluster *clusterv1.Cluster) bool {
	if cluster == nil {
		return false
	}
	ref := cluster.Spec.InfrastructureRef
	if ref.Kind != beskar7ClusterKind || ref.APIGroup != infrav1.GroupVersion.Group {
		return false
	}
	return cluster.GetAnnotations()[NodeProviderIDAnnotation] == NodeProviderIDAnnotationValue
}

// reconcileNodeProviderIDAnnotation mirrors spec.setNodeProviderID onto the owner Cluster as NodeProviderIDAnnotation.
func (r *Beskar7ClusterReconciler) reconcileNodeProviderIDAnnotation(ctx context.Context, logger logr.Logger, cluster *clusterv1.Cluster, b7cluster *infrav1.Beskar7Cluster) error {
	want := b7cluster.Spec.SetNodeProviderID
	value, has := cluster.GetAnnotations()[NodeProviderIDAnnotation]
	if want && value == NodeProviderIDAnnotationValue {
		return nil
	}
	if !want && !has {
		return nil
	}

	before := cluster.DeepCopy()
	annotations := cluster.GetAnnotations()
	if want {
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[NodeProviderIDAnnotation] = NodeProviderIDAnnotationValue
	} else {
		delete(annotations, NodeProviderIDAnnotation)
	}
	cluster.SetAnnotations(annotations)
	if err := r.Patch(ctx, cluster, client.MergeFrom(before)); err != nil {
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
func (r *Beskar7MachineReconciler) ClusterToBeskar7Machines(ctx context.Context, obj client.Object) []reconcile.Request {
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
