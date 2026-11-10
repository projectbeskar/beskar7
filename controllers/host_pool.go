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
	"slices"
	"strings"
	"sync/atomic"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
)

const (
	// PhysicalHostConsumerPoolIndex indexes PhysicalHosts by spec.consumerPool.
	PhysicalHostConsumerPoolIndex = "spec.consumerPool"
	// PhysicalHostConsumerIndex indexes PhysicalHosts by the namespace/name of their ConsumerRef.
	PhysicalHostConsumerIndex = "spec.consumerRef"
	// Beskar7ClusterPoolRefsIndex indexes Beskar7Clusters by each entry of spec.poolRefs.
	Beskar7ClusterPoolRefsIndex = "spec.poolRefs"
	// Beskar7MachinePoolRefIndex indexes Beskar7Machines by spec.poolRef.
	Beskar7MachinePoolRefIndex = "spec.poolRef"
)

// hostPoolsEnabled gates every cross-namespace claim; the manager turns it on in the cluster-wide install.
var hostPoolsEnabled atomic.Bool

// EnableHostPools turns cross-namespace claims through a PhysicalHostPool on or off.
func EnableHostPools(on bool) {
	hostPoolsEnabled.Store(on)
}

// HostPoolsEnabled reports whether cross-namespace claims through a PhysicalHostPool are on.
func HostPoolsEnabled() bool {
	return hostPoolsEnabled.Load()
}

// poolLabelKey is the label that puts a PhysicalHost in pool.
func poolLabelKey(pool string) string {
	return infrav1.PhysicalHostPoolLabelPrefix + pool
}

// hostPools returns the names of the pools host carries a label for.
func hostPools(host *infrav1.PhysicalHost) []string {
	var pools []string
	for key := range host.Labels {
		if pool, ok := strings.CutPrefix(key, infrav1.PhysicalHostPoolLabelPrefix); ok && pool != "" {
			pools = append(pools, pool)
		}
	}
	return pools
}

// poolMember reports whether host is a member of pool: it carries the pool label
// and lives in one of the pool's hostNamespaces.
func poolMember(host *infrav1.PhysicalHost, pool *infrav1.PhysicalHostPool) bool {
	if _, labelled := host.Labels[poolLabelKey(pool.Name)]; !labelled {
		return false
	}
	return slices.Contains(pool.Spec.HostNamespaces, host.Namespace)
}

// getPool returns the named pool, or nil when it does not exist.
func getPool(ctx context.Context, c client.Reader, name string) (*infrav1.PhysicalHostPool, error) {
	pool := &infrav1.PhysicalHostPool{}
	err := c.Get(ctx, types.NamespacedName{Name: name}, pool)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return pool, nil
}

// poolGrants reports whether pool lets Beskar7Machines in namespace claim its
// members. A selector that does not parse, or a namespace that does not exist,
// grants nothing.
func poolGrants(ctx context.Context, c client.Reader, pool *infrav1.PhysicalHostPool, namespace string) (bool, error) {
	if pool.Spec.NamespaceSelector == nil {
		return false, nil
	}
	sel, err := metav1.LabelSelectorAsSelector(pool.Spec.NamespaceSelector)
	if err != nil {
		return false, nil
	}
	ns := &corev1.Namespace{}
	if err := c.Get(ctx, types.NamespacedName{Name: namespace}, ns); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return sel.Matches(labels.Set(ns.Labels)), nil
}

// isBeskar7MachineRef reports whether cr names a Beskar7Machine of this API version.
func isBeskar7MachineRef(cr *corev1.ObjectReference) bool {
	return cr != nil && cr.Kind == "Beskar7Machine" && cr.APIVersion == InfrastructureAPIVersion
}

// resolveConsumer returns the Beskar7Machine that legitimately consumes host.
// A same-namespace ConsumerRef resolves as in resolveConsumerBeskar7Machine,
// with no API call. One in another namespace resolves only while host pools are
// enabled and the pool in host.Spec.ConsumerPool still has host as a member and
// still grants that namespace; membership in any other pool does not count.
// Anything else is "no consumer", exactly like an absent ConsumerRef.
func resolveConsumer(ctx context.Context, c client.Reader, host *infrav1.PhysicalHost) (types.NamespacedName, bool, error) {
	if key, ok := resolveConsumerBeskar7Machine(host); ok {
		return key, true, nil
	}
	cr := host.Spec.ConsumerRef
	if !HostPoolsEnabled() || !isBeskar7MachineRef(cr) || cr.Namespace == "" || host.Spec.ConsumerPool == "" {
		return types.NamespacedName{}, false, nil
	}
	pool, err := getPool(ctx, c, host.Spec.ConsumerPool)
	if err != nil || pool == nil || !poolMember(host, pool) {
		return types.NamespacedName{}, false, err
	}
	granted, err := poolGrants(ctx, c, pool, cr.Namespace)
	if err != nil || !granted {
		return types.NamespacedName{}, false, err
	}
	return types.NamespacedName{Namespace: cr.Namespace, Name: cr.Name}, true, nil
}

// claimedBy reports whether host is claimed by b7machine, as the machine side
// sees it: a same-namespace claim, or a claim made through the machine's own
// poolRef. It reads nothing; the grant is checked live where host data is
// served (resolveConsumer), and the machine only looks at hosts in the
// namespaces its search allows (hostSearch).
func claimedBy(host *infrav1.PhysicalHost, b7machine *infrav1.Beskar7Machine) bool {
	if key, ok := resolveConsumerBeskar7Machine(host); ok {
		return key == client.ObjectKeyFromObject(b7machine)
	}
	return HostPoolsEnabled() && claimedThroughPool(host, b7machine)
}

// claimedThroughPool reports whether host's claim names b7machine and was made through its poolRef.
func claimedThroughPool(host *infrav1.PhysicalHost, b7machine *infrav1.Beskar7Machine) bool {
	cr := host.Spec.ConsumerRef
	return isBeskar7MachineRef(cr) &&
		cr.Namespace == b7machine.Namespace && cr.Name == b7machine.Name &&
		b7machine.Spec.PoolRef != "" && host.Spec.ConsumerPool == b7machine.Spec.PoolRef
}

// consumerIndexValue is the PhysicalHostConsumerIndex value of a claim by namespace/name.
func consumerIndexValue(namespace, name string) string {
	return namespace + "/" + name
}

// physicalHostConsumer indexes a host by the namespace/name its ConsumerRef names.
func physicalHostConsumer(obj client.Object) []string {
	host, ok := obj.(*infrav1.PhysicalHost)
	if !ok || !isBeskar7MachineRef(host.Spec.ConsumerRef) {
		return nil
	}
	ns := host.Spec.ConsumerRef.Namespace
	if ns == "" {
		ns = host.Namespace
	}
	return []string{consumerIndexValue(ns, host.Spec.ConsumerRef.Name)}
}

// hostSearch is where a Beskar7Machine looks for hosts.
type hostSearch struct {
	// namespaces to list hosts in.
	namespaces []string
	// pool is the pool the machine claims through, "" for its own namespace.
	pool string
}

// claimSelector adds the pool label requirement to placement for a pool search.
func (s hostSearch) claimSelector(placement labels.Selector) (labels.Selector, error) {
	if s.pool == "" {
		return placement, nil
	}
	req, err := labels.NewRequirement(poolLabelKey(s.pool), selection.Exists, nil)
	if err != nil {
		return nil, err
	}
	if placement == nil {
		placement = labels.NewSelector()
	}
	return placement.Add(*req), nil
}

// claimable reports whether a free host in the search may be claimed. A pool
// claim takes only hosts that are wiped on release, since the next claim may
// come from another namespace.
func (s hostSearch) claimable(host *infrav1.PhysicalHost) bool {
	if host.Spec.ConsumerRef != nil {
		return false
	}
	return s.pool == "" || cleaningEnabled(host)
}

// claimSearch returns where b7machine may claim a host, or ok=false when its
// poolRef names a pool that is disabled, missing, or does not grant its namespace.
func (r *Beskar7MachineReconciler) claimSearch(ctx context.Context, b7machine *infrav1.Beskar7Machine) (hostSearch, bool, error) {
	if b7machine.Spec.PoolRef == "" {
		return hostSearch{namespaces: []string{b7machine.Namespace}}, true, nil
	}
	if !HostPoolsEnabled() {
		return hostSearch{}, false, nil
	}
	pool, err := getPool(ctx, r.Client, b7machine.Spec.PoolRef)
	// A pool being deleted takes no new claims; claims already made stay valid.
	if err != nil || pool == nil || !pool.DeletionTimestamp.IsZero() {
		return hostSearch{}, false, err
	}
	granted, err := poolGrants(ctx, r.Client, pool, b7machine.Namespace)
	if err != nil || !granted {
		return hostSearch{}, false, err
	}
	return hostSearch{namespaces: pool.Spec.HostNamespaces, pool: pool.Name}, true, nil
}

// findHeldHost returns the host b7machine holds. Own namespace first, by scan
// (claimedBy rejects a ConsumerRef naming another namespace, SEC-12); then, for
// a pool machine, any namespace through the consumerRef index. Outside its own
// namespace a machine adopts only a claim carrying its UID, which nobody outside
// its namespace can read, and keeps it after the host leaves the pool
// (PoolClaimValid reports that). Release takes any claim naming the machine, so
// a host is given back even after its pool or the switch changed.
func (r *Beskar7MachineReconciler) findHeldHost(ctx context.Context, b7machine *infrav1.Beskar7Machine, forRelease bool) (*infrav1.PhysicalHost, error) {
	own := &infrav1.PhysicalHostList{}
	if err := r.List(ctx, own, client.InNamespace(b7machine.Namespace)); err != nil {
		return nil, err
	}
	for i := range own.Items {
		if claimedBy(&own.Items[i], b7machine) {
			return &own.Items[i], nil
		}
	}
	if b7machine.Spec.PoolRef == "" {
		return nil, nil
	}
	held := &infrav1.PhysicalHostList{}
	if err := r.List(ctx, held, client.MatchingFields{PhysicalHostConsumerIndex: consumerIndexValue(b7machine.Namespace, b7machine.Name)}); err != nil {
		return nil, err
	}
	for i := range held.Items {
		host := &held.Items[i]
		if host.Namespace == b7machine.Namespace || !claimedThroughPool(host, b7machine) {
			continue
		}
		if forRelease {
			return host, nil
		}
		if HostPoolsEnabled() && host.Spec.ConsumerRef.UID != "" && host.Spec.ConsumerRef.UID == b7machine.UID {
			return host, nil
		}
	}
	return nil, nil
}

// waitingPoolMachines lists the Beskar7Machines in any namespace whose poolRef
// names one of the pools host is labelled into.
func (r *Beskar7MachineReconciler) waitingPoolMachines(ctx context.Context, host *infrav1.PhysicalHost) ([]infrav1.Beskar7Machine, error) {
	if !HostPoolsEnabled() {
		return nil, nil
	}
	var machines []infrav1.Beskar7Machine
	for _, pool := range hostPools(host) {
		list := &infrav1.Beskar7MachineList{}
		if err := r.List(ctx, list, client.MatchingFields{Beskar7MachinePoolRefIndex: pool}); err != nil {
			return nil, err
		}
		machines = append(machines, list.Items...)
	}
	return machines, nil
}

// failHardwareRequirements marks b7machine terminally failed on a hardware
// mismatch. A host claimed through a pool is also released, forced off and
// wiped, so a wrong guess does not hold shared capacity until the machine is
// deleted. A host in the machine's own namespace stays held, as before.
func (r *Beskar7MachineReconciler) failHardwareRequirements(ctx context.Context, logger logr.Logger, b7machine *infrav1.Beskar7Machine, host *infrav1.PhysicalHost, msg string) (ctrl.Result, error) {
	r.markTerminalFailure(b7machine, infrav1.HardwareRequirementsNotMetReason, msg)
	if host.Namespace == b7machine.Namespace || !claimedThroughPool(host, b7machine) {
		return ctrl.Result{}, nil
	}
	logger.Info("Releasing the pool host after the hardware mismatch", "host", host.Name, "pool", host.Spec.ConsumerPool)
	r.releaseHostPower(ctx, logger, b7machine, host)
	base := host.DeepCopy()
	host.Spec.ConsumerRef = nil
	host.Spec.ConsumerPool = ""
	if err := r.Patch(ctx, host, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		// Not retried: the machine is terminal now, and deleting it releases the host.
		logger.Error(err, "Failed to release the pool host after the hardware mismatch; deleting the machine releases it", "host", host.Name)
		return ctrl.Result{}, nil
	}
	r.booted.Delete(host.UID)
	return ctrl.Result{}, nil
}

// markPoolClaim sets PoolClaimValid on a machine holding a host in another
// namespace through its pool, checked like the callback server checks it.
func (r *Beskar7MachineReconciler) markPoolClaim(ctx context.Context, b7machine *infrav1.Beskar7Machine, host *infrav1.PhysicalHost) error {
	if host.Namespace == b7machine.Namespace || !claimedThroughPool(host, b7machine) {
		return nil
	}
	_, ok, err := resolveConsumer(ctx, r.Client, host)
	if err != nil {
		return err
	}
	if ok {
		setTrue(b7machine, infrav1.PoolClaimValidCondition, infrav1.PoolClaimValidReason)
		return nil
	}
	setFalse(b7machine, infrav1.PoolClaimValidCondition, infrav1.PoolGrantRevokedReason,
		"PhysicalHost %s/%s left pool %q or the pool no longer grants this namespace; it stays claimed, but the host can no longer reach its callbacks",
		host.Namespace, host.Name, host.Spec.ConsumerPool)
	return nil
}

// poolToBeskar7Machines maps a pool to every machine whose poolRef names it.
func (r *Beskar7MachineReconciler) poolToBeskar7Machines(ctx context.Context, obj client.Object) []reconcile.Request {
	list := &infrav1.Beskar7MachineList{}
	if err := r.List(ctx, list, client.MatchingFields{Beskar7MachinePoolRefIndex: obj.GetName()}); err != nil {
		r.Log.Error(err, "Failed to list Beskar7Machines of a PhysicalHostPool", "pool", obj.GetName())
		return nil
	}
	return machineRequests(list.Items)
}

// namespaceToPoolBeskar7Machines maps a namespace to its machines that claim through a pool.
func (r *Beskar7MachineReconciler) namespaceToPoolBeskar7Machines(ctx context.Context, obj client.Object) []reconcile.Request {
	list := &infrav1.Beskar7MachineList{}
	if err := r.List(ctx, list, client.InNamespace(obj.GetName())); err != nil {
		r.Log.Error(err, "Failed to list Beskar7Machines of a Namespace", "namespace", obj.GetName())
		return nil
	}
	var pooled []infrav1.Beskar7Machine
	for i := range list.Items {
		if list.Items[i].Spec.PoolRef != "" {
			pooled = append(pooled, list.Items[i])
		}
	}
	return machineRequests(pooled)
}

func machineRequests(machines []infrav1.Beskar7Machine) []reconcile.Request {
	reqs := make([]reconcile.Request, 0, len(machines))
	for i := range machines {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&machines[i])})
	}
	return reqs
}

// poolMembersForCluster lists the members of the pools in b7cluster.spec.poolRefs
// that grant its namespace.
func (r *Beskar7ClusterReconciler) poolMembersForCluster(ctx context.Context, b7cluster *infrav1.Beskar7Cluster) ([]infrav1.PhysicalHost, error) {
	if !HostPoolsEnabled() {
		return nil, nil
	}
	var members []infrav1.PhysicalHost
	for _, name := range b7cluster.Spec.PoolRefs {
		pool, err := getPool(ctx, r.Client, name)
		if err != nil {
			return nil, err
		}
		if pool == nil {
			continue
		}
		granted, err := poolGrants(ctx, r.Client, pool, b7cluster.Namespace)
		if err != nil {
			return nil, err
		}
		if !granted {
			continue
		}
		for _, ns := range pool.Spec.HostNamespaces {
			hosts := &infrav1.PhysicalHostList{}
			if err := r.List(ctx, hosts, client.InNamespace(ns), client.HasLabels{poolLabelKey(pool.Name)}); err != nil {
				return nil, err
			}
			members = append(members, hosts.Items...)
		}
	}
	return members, nil
}

// poolToBeskar7Clusters maps a pool to the clusters whose poolRefs name it.
func (r *Beskar7ClusterReconciler) poolToBeskar7Clusters(ctx context.Context, obj client.Object) []reconcile.Request {
	if !HostPoolsEnabled() {
		return nil
	}
	list := &infrav1.Beskar7ClusterList{}
	if err := r.List(ctx, list, client.MatchingFields{Beskar7ClusterPoolRefsIndex: obj.GetName()}); err != nil {
		r.Log.Error(err, "Failed to list Beskar7Clusters of a PhysicalHostPool", "pool", obj.GetName())
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
	}
	return reqs
}
