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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
)

const (
	// nodeNotFoundRequeue is how often a Ready machine looks for its Node while the host joins.
	nodeNotFoundRequeue = 15 * time.Second

	// workloadClusterUnreachableRequeue is how often a Ready machine retries an unreachable workload cluster.
	workloadClusterUnreachableRequeue = 30 * time.Second

	// nodeProviderIDMismatchRequeue is the backstop for a mismatched Node, whose fix no watch here sees.
	nodeProviderIDMismatchRequeue = 5 * time.Minute
)

// reconcileNodeProviderID sets the machine's ProviderID on the workload Node named after the owner Machine while that Node has none (#297).
func (r *Beskar7MachineReconciler) reconcileNodeProviderID(ctx context.Context, logger logr.Logger, b7machine *infrav1.Beskar7Machine) (ctrl.Result, error) {
	if r.ClusterCache == nil || b7machine.Spec.ProviderID == "" {
		return ctrl.Result{}, nil
	}

	machine, err := util.GetOwnerMachine(ctx, r.Client, b7machine.ObjectMeta)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get owner Machine: %w", err)
	}
	if machine == nil {
		return ctrl.Result{}, nil
	}

	cluster, err := util.GetClusterFromMetadata(ctx, r.Client, machine.ObjectMeta)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get Cluster of Machine %q: %w", machine.Name, err)
	}
	// Same check as the ClusterCache filter, so an opted-out cluster is never asked for and carries no condition.
	if !NodeProviderIDClusterFilter(cluster) {
		conditions.Delete(b7machine, infrav1.NodeProviderIDReadyCondition)
		return ctrl.Result{}, nil
	}

	if machine.Status.NodeRef.IsDefined() {
		// Cluster API matched the Node by providerID, so it carries ours.
		setTrue(b7machine, infrav1.NodeProviderIDReadyCondition, infrav1.NodeProviderIDSetReason)
		return ctrl.Result{}, nil
	}

	// Uncached, so no Node informer runs on every workload cluster for a few reads.
	workloadClient, err := r.ClusterCache.GetUncachedClient(ctx, client.ObjectKeyFromObject(cluster))
	if err != nil {
		logger.V(1).Info("No client for the workload cluster yet", "cluster", cluster.Name, "err", err)
		setFalse(b7machine, infrav1.NodeProviderIDReadyCondition, infrav1.WorkloadClusterUnreachableReason,
			"Workload cluster %q is not reachable: %v", cluster.Name, err)
		return ctrl.Result{RequeueAfter: workloadClusterUnreachableRequeue}, nil
	}

	node := &corev1.Node{}
	if err := workloadClient.Get(ctx, client.ObjectKey{Name: machine.Name}, node); err != nil {
		if apierrors.IsNotFound(err) {
			setFalse(b7machine, infrav1.NodeProviderIDReadyCondition, infrav1.NodeNotFoundReason,
				"Node %q has not registered in workload cluster %q yet", machine.Name, cluster.Name)
			return ctrl.Result{RequeueAfter: nodeNotFoundRequeue}, nil
		}
		setFalse(b7machine, infrav1.NodeProviderIDReadyCondition, infrav1.WorkloadClusterUnreachableReason,
			"Reading Node %q from workload cluster %q failed: %v", machine.Name, cluster.Name, err)
		return ctrl.Result{RequeueAfter: workloadClusterUnreachableRequeue}, nil
	}

	want := b7machine.Spec.ProviderID
	switch node.Spec.ProviderID {
	case want:
		setTrue(b7machine, infrav1.NodeProviderIDReadyCondition, infrav1.NodeProviderIDSetReason)
		return ctrl.Result{}, nil
	case "":
		// Fall through to the patch below.
	default:
		logger.Info("Node has a different providerID; not changing it", "node", node.Name,
			"nodeProviderID", node.Spec.ProviderID, "providerID", want)
		setFalse(b7machine, infrav1.NodeProviderIDReadyCondition, infrav1.NodeProviderIDMismatchReason,
			"Node %q has providerID %q, expected %q", node.Name, node.Spec.ProviderID, want)
		return ctrl.Result{RequeueAfter: nodeProviderIDMismatchRequeue}, nil
	}

	// The API server rejects changing a non-empty providerID, so a racing writer wins.
	before := node.DeepCopy()
	node.Spec.ProviderID = want
	if err := workloadClient.Patch(ctx, node, client.MergeFrom(before)); err != nil {
		return ctrl.Result{}, fmt.Errorf("set providerID on Node %q: %w", node.Name, err)
	}
	logger.Info("Set providerID on the workload cluster Node", "node", node.Name, "providerID", want)
	setTrue(b7machine, infrav1.NodeProviderIDReadyCondition, infrav1.NodeProviderIDSetReason)
	return ctrl.Result{}, nil
}
