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
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
)

// poolDeletionRecheck is how often a deleting pool checks for remaining claims.
const poolDeletionRecheck = 30 * time.Second

// poolRevokedListMax caps how many revoked claims the ClaimsValid message names.
const poolRevokedListMax = 5

// PhysicalHostPoolReconciler keeps a pool's status and holds it until no host is claimed through it.
type PhysicalHostPoolReconciler struct {
	client.Client
}

// Reconcile adds the finalizer, refreshes the status, and on deletion removes
// the finalizer once no host has spec.consumerPool naming the pool.
func (r *PhysicalHostPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	pool := &infrav1.PhysicalHostPool{}
	if err := r.Get(ctx, req.NamespacedName, pool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if pool.DeletionTimestamp.IsZero() && controllerutil.AddFinalizer(pool, infrav1.PhysicalHostPoolFinalizer) {
		return ctrl.Result{}, r.Update(ctx, pool)
	}

	claimed := &infrav1.PhysicalHostList{}
	if err := r.List(ctx, claimed, client.MatchingFields{PhysicalHostConsumerPoolIndex: pool.Name}); err != nil {
		return ctrl.Result{}, err
	}

	if !pool.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(pool, infrav1.PhysicalHostPoolFinalizer) {
			return ctrl.Result{}, nil
		}
		if len(claimed.Items) > 0 {
			logger.Info("PhysicalHostPool is being deleted; waiting for its claims to be released", "claimed", len(claimed.Items))
			return ctrl.Result{RequeueAfter: poolDeletionRecheck}, r.updateStatus(ctx, pool, claimed.Items)
		}
		controllerutil.RemoveFinalizer(pool, infrav1.PhysicalHostPoolFinalizer)
		if err := r.Update(ctx, pool); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}
	return ctrl.Result{}, r.updateStatus(ctx, pool, claimed.Items)
}

// updateStatus counts members, free members and claims, and checks every claim
// made through the pool the way the callback server does (resolveConsumer).
func (r *PhysicalHostPoolReconciler) updateStatus(ctx context.Context, pool *infrav1.PhysicalHostPool, claimed []infrav1.PhysicalHost) error {
	base := pool.DeepCopy()
	var members, available int32
	for _, ns := range pool.Spec.HostNamespaces {
		hosts := &infrav1.PhysicalHostList{}
		if err := r.List(ctx, hosts, client.InNamespace(ns), client.HasLabels{poolLabelKey(pool.Name)}); err != nil {
			return err
		}
		for i := range hosts.Items {
			members++
			if (hostSearch{pool: pool.Name}).claimable(&hosts.Items[i]) && hosts.Items[i].Status.State == infrav1.StateAvailable {
				available++
			}
		}
	}

	var revoked []string
	for i := range claimed {
		host := &claimed[i]
		_, ok, err := resolveConsumer(ctx, r.Client, host)
		if err != nil {
			return err
		}
		if !ok {
			revoked = append(revoked, host.Namespace+"/"+host.Name)
		}
	}
	pool.Status.Members, pool.Status.Available, pool.Status.Claimed = members, available, int32(len(claimed))
	if len(revoked) == 0 {
		setTrue(pool, infrav1.ClaimsValidCondition, infrav1.PoolClaimValidReason)
	} else {
		shown := revoked
		if len(shown) > poolRevokedListMax {
			shown = shown[:poolRevokedListMax]
		}
		setFalse(pool, infrav1.ClaimsValidCondition, infrav1.PoolGrantRevokedReason,
			"%d claim(s) lost pool membership or grant; the hosts stay claimed until their machines are deleted: %s",
			len(revoked), strings.Join(shown, ", "))
	}
	return r.Status().Patch(ctx, pool, client.MergeFrom(base))
}

// SetupWithManager registers the consumerPool index and the controller. A host
// change wakes every pool it is labelled into or claimed through, and a
// Namespace change wakes every pool, since grants follow namespace labels.
func (r *PhysicalHostPoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&infrav1.PhysicalHost{},
		PhysicalHostConsumerPoolIndex,
		func(obj client.Object) []string {
			host, ok := obj.(*infrav1.PhysicalHost)
			if !ok || host.Spec.ConsumerPool == "" {
				return nil
			}
			return []string{host.Spec.ConsumerPool}
		},
	); err != nil {
		return fmt.Errorf("failed to index PhysicalHost.spec.consumerPool: %w", err)
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1.PhysicalHostPool{}).
		Watches(&infrav1.PhysicalHost{}, handler.EnqueueRequestsFromMapFunc(poolsOfHost)).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.allPools),
			builder.WithPredicates(predicate.LabelChangedPredicate{})).
		Complete(r)
}

// poolsOfHost maps a host to the pools it is labelled into and the pool its claim went through.
func poolsOfHost(_ context.Context, obj client.Object) []reconcile.Request {
	host, ok := obj.(*infrav1.PhysicalHost)
	if !ok {
		return nil
	}
	names := hostPools(host)
	if host.Spec.ConsumerPool != "" {
		names = append(names, host.Spec.ConsumerPool)
	}
	reqs := make([]reconcile.Request, 0, len(names))
	for _, name := range names {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKey{Name: name}})
	}
	return reqs
}

// allPools maps any event to every pool.
func (r *PhysicalHostPoolReconciler) allPools(ctx context.Context, _ client.Object) []reconcile.Request {
	pools := &infrav1.PhysicalHostPoolList{}
	if err := r.List(ctx, pools); err != nil {
		log.FromContext(ctx).Error(err, "Failed to list PhysicalHostPools for a Namespace change")
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(pools.Items))
	for i := range pools.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&pools.Items[i])})
	}
	return reqs
}
