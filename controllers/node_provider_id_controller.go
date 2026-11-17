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

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/controllers/clustercache"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
)

// nodeProviderIDControllerName sets this controller apart from the Beskar7Machine controller, which has the same For.
const nodeProviderIDControllerName = "beskar7machine-nodeproviderid"

// NodeProviderIDReconciler sets Node.spec.providerID on workload clusters (#297) in a queue of its own,
// so a slow workload API server never holds the Beskar7Machine controller's workers.
type NodeProviderIDReconciler struct {
	client.Client
	Log logr.Logger
	// ClusterCache gives clients for workload clusters; nil means --enable-node-provider-id is off.
	ClusterCache clustercache.ClusterCache
	// MaxConcurrentReconciles is the worker count for this controller. Zero
	// means DefaultMaxConcurrentReconciles (1).
	MaxConcurrentReconciles int
}

// Reconcile writes only NodeProviderIDReady, so the Beskar7Machine controller's conditions are never overwritten.
func (r *NodeProviderIDReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, reterr error) {
	logger := r.Log.WithValues("beskar7machine", req.NamespacedName)

	b7machine := &infrav1.Beskar7Machine{}
	if err := r.Get(ctx, req.NamespacedName, b7machine); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !b7machine.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	patchHelper, err := patch.NewHelper(b7machine, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}
	defer func() {
		err := patchHelper.Patch(ctx, b7machine, patch.WithOwnedConditions{Conditions: []string{
			infrav1.NodeProviderIDReadyCondition,
		}})
		if err != nil && !apierrors.IsNotFound(err) {
			reterr = errors.Join(reterr, err)
		}
	}()

	return r.reconcileNodeProviderID(ctx, logger, b7machine)
}

// SetupWithManager registers the controller with its own name, queue and workers.
func (r *NodeProviderIDReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named(nodeProviderIDControllerName).
		For(
			&infrav1.Beskar7Machine{},
			builder.WithPredicates(nodeProviderIDCandidate())).
		// Flipping setNodeProviderID reaches the Cluster as an annotation; a linked machine has no timer to notice it.
		Watches(
			&clusterv1.Cluster{},
			handler.EnqueueRequestsFromMapFunc(r.ClusterToBeskar7Machines),
			builder.WithPredicates(nodeProviderIDAnnotationChanged())).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: maxConcurrentOrDefault(r.MaxConcurrentReconciles),
		}).
		Complete(r)
}

// nodeProviderIDCandidate passes a Beskar7Machine this controller may act on: Ready, or still carrying its condition.
// Updates pass only when Ready or the providerID changed, so this controller's own status writes do not wake it.
func nodeProviderIDCandidate() predicate.Funcs {
	candidate := func(obj client.Object) (*infrav1.Beskar7Machine, bool) {
		b7machine, ok := obj.(*infrav1.Beskar7Machine)
		if !ok {
			return nil, false
		}
		return b7machine, b7machine.Status.Ready || conditions.Has(b7machine, infrav1.NodeProviderIDReadyCondition)
	}
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			_, ok := candidate(e.Object)
			return ok
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldMachine, okOld := e.ObjectOld.(*infrav1.Beskar7Machine)
			newMachine, okNew := candidate(e.ObjectNew)
			if !okOld || !okNew {
				return false
			}
			return oldMachine.Status.Ready != newMachine.Status.Ready ||
				oldMachine.Spec.ProviderID != newMachine.Spec.ProviderID
		},
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}
