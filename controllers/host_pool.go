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
	"fmt"
	"slices"
	"strings"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1 "github.com/beskar7/beskar7/api/v1beta2"
)

const (
	// PhysicalHostConsumerPoolIndex indexes PhysicalHosts by spec.consumerPool.
	PhysicalHostConsumerPoolIndex = "spec.consumerPool"
	// PhysicalHostConsumerIndex indexes PhysicalHosts by the namespace/name of their ConsumerRef.
	PhysicalHostConsumerIndex = "spec.consumerRef"
	// Beskar7MachinePoolRefIndex indexes Beskar7Machines by spec.poolRef.
	Beskar7MachinePoolRefIndex = "spec.poolRef"
)

// claimReader reads what a claim is checked against. hostPools is
// --enable-host-pools: without it no claim in another namespace is valid.
type claimReader struct {
	client.Reader
	hostPools bool
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

// errConsumerUnreadable is returned (wrapped with the cause) when the pool, the
// Namespace or the Beskar7Machine a cross-namespace claim is checked against
// could not be read. Like errBootstrapSecretUnreadable it says nothing about the
// claim, so a caller deciding something permanent waits instead.
var errConsumerUnreadable = errors.New("the consumer of a pool claim could not be read")

// hostConsumer is the Beskar7Machine that legitimately consumes a host.
type hostConsumer struct {
	key types.NamespacedName
	// uid is the machine's UID for a pool claim, which the host's credentials
	// must be bound to; empty for a claim in the host's own namespace.
	uid types.UID
}

// resolveConsumer returns the Beskar7Machine that legitimately consumes host.
// A same-namespace ConsumerRef resolves as in resolveConsumerBeskar7Machine,
// with no API call. One in another namespace resolves only while host pools are
// enabled, the pool in host.Spec.ConsumerPool still has host as a member and
// still grants that namespace, and the machine proves the claim
// (provesPoolClaim): the host side alone is writable by whoever writes the
// host namespace. Anything else is "no consumer", exactly like an absent
// ConsumerRef.
func resolveConsumer(ctx context.Context, c claimReader, host *infrav1.PhysicalHost) (hostConsumer, bool, error) {
	if key, ok := resolveConsumerBeskar7Machine(host); ok {
		return hostConsumer{key: key}, true, nil
	}
	cr := host.Spec.ConsumerRef
	if !c.hostPools || !isBeskar7MachineRef(cr) || cr.Namespace == "" || host.Spec.ConsumerPool == "" {
		return hostConsumer{}, false, nil
	}
	pool, err := getPool(ctx, c, host.Spec.ConsumerPool)
	if err != nil {
		return hostConsumer{}, false, fmt.Errorf("get pool %q: %w: %w", host.Spec.ConsumerPool, errConsumerUnreadable, err)
	}
	if pool == nil || !poolMember(host, pool) {
		return hostConsumer{}, false, nil
	}
	granted, err := poolGrants(ctx, c, pool, cr.Namespace)
	if err != nil {
		return hostConsumer{}, false, fmt.Errorf("get namespace %q: %w: %w", cr.Namespace, errConsumerUnreadable, err)
	}
	if !granted {
		return hostConsumer{}, false, nil
	}
	key := types.NamespacedName{Namespace: cr.Namespace, Name: cr.Name}
	b7machine := &infrav1.Beskar7Machine{}
	if err := c.Get(ctx, key, b7machine); err != nil {
		if apierrors.IsNotFound(err) {
			return hostConsumer{}, false, nil
		}
		return hostConsumer{}, false, fmt.Errorf("get Beskar7Machine %s: %w: %w", key, errConsumerUnreadable, err)
	}
	if !provesPoolClaim(host, b7machine) {
		return hostConsumer{}, false, nil
	}
	return hostConsumer{key: key, uid: b7machine.UID}, true, nil
}

// consumerUnreadable reports whether err comes from a failed read of what a
// claim is checked against, rather than from a refusal.
func consumerUnreadable(err error) bool {
	return errors.Is(err, errBootstrapSecretUnreadable) || errors.Is(err, errConsumerUnreadable)
}

// claimedBy reports whether host is claimed by b7machine, as the machine side
// sees it: a same-namespace claim, or a claim made through the machine's own
// poolRef. It reads nothing; the grant is checked live where host data is
// served (resolveConsumer), and the machine only looks at hosts in the
// namespaces its search allows (hostSearch).
func (r *Beskar7MachineReconciler) claimedBy(host *infrav1.PhysicalHost, b7machine *infrav1.Beskar7Machine) bool {
	if key, ok := resolveConsumerBeskar7Machine(host); ok {
		return key == client.ObjectKeyFromObject(b7machine)
	}
	return r.HostPools && claimedThroughPool(host, b7machine)
}

// claims is what this reconciler checks a pool claim with.
func (r *Beskar7MachineReconciler) claims() claimReader {
	return claimReader{Reader: r.Client, hostPools: r.HostPools}
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
// claim takes only a host whose last release ended in a wipe (HostCleaned=True),
// since the claim before it may have come from another namespace; the mode alone
// says nothing about a release that already happened.
func (s hostSearch) claimable(host *infrav1.PhysicalHost) bool {
	if host.Spec.ConsumerRef != nil {
		return false
	}
	return s.pool == "" || conditions.IsTrue(host, infrav1.HostCleanedCondition)
}

// unwiped reports whether a free host is skipped by a pool search only for want of a wipe.
func (s hostSearch) unwiped(host *infrav1.PhysicalHost) bool {
	return s.pool != "" && host.Spec.ConsumerRef == nil && !s.claimable(host)
}

// claimSearch returns where b7machine may claim a host, or ok=false when its
// poolRef names a pool that is disabled, missing, or does not grant its namespace.
func (r *Beskar7MachineReconciler) claimSearch(ctx context.Context, b7machine *infrav1.Beskar7Machine) (hostSearch, bool, error) {
	if b7machine.Spec.PoolRef == "" {
		return hostSearch{namespaces: []string{b7machine.Namespace}}, true, nil
	}
	if !r.HostPools {
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
// namespace a machine takes only a claim it can prove is its own
// (provesPoolClaim), for a release too: a forged claim naming the machine must
// not end the release of the real one. It keeps the claim after the host leaves
// the pool (PoolClaimValid reports that). Release takes the claim even after the
// switch turned pools off, so a host is given back.
func (r *Beskar7MachineReconciler) findHeldHost(ctx context.Context, b7machine *infrav1.Beskar7Machine, forRelease bool) (*infrav1.PhysicalHost, error) {
	own := &infrav1.PhysicalHostList{}
	if err := r.List(ctx, own, client.InNamespace(b7machine.Namespace)); err != nil {
		return nil, err
	}
	for i := range own.Items {
		if r.claimedBy(&own.Items[i], b7machine) {
			return &own.Items[i], nil
		}
	}
	if b7machine.Spec.PoolRef == "" || (!forRelease && !r.HostPools) {
		return nil, nil
	}
	held := &infrav1.PhysicalHostList{}
	if err := r.List(ctx, held, client.MatchingFields{PhysicalHostConsumerIndex: consumerIndexValue(b7machine.Namespace, b7machine.Name)}); err != nil {
		return nil, err
	}
	for i := range held.Items {
		host := &held.Items[i]
		if host.Namespace != b7machine.Namespace && provesPoolClaim(host, b7machine) {
			return host, nil
		}
	}
	return nil, nil
}

// provesPoolClaim reports whether a pool claim on host is b7machine's own: it
// names the machine and the machine's poolRef, and adoptsPoolClaim holds.
func provesPoolClaim(host *infrav1.PhysicalHost, b7machine *infrav1.Beskar7Machine) bool {
	return claimedThroughPool(host, b7machine) && adoptsPoolClaim(host, b7machine)
}

// adoptsPoolClaim reports whether a pool claim naming b7machine is its own. The
// claim carries the machine's UID, which nobody outside its namespace can read,
// or, after clusterctl move recreated the machine with a new UID, the machine's
// providerID names the host: the host admin writes the claim and the tenant the
// providerID, so neither can point a host at a machine alone.
func adoptsPoolClaim(host *infrav1.PhysicalHost, b7machine *infrav1.Beskar7Machine) bool {
	if uid := host.Spec.ConsumerRef.UID; uid != "" && uid == b7machine.UID {
		return true
	}
	return b7machine.Spec.ProviderID != "" && b7machine.Spec.ProviderID == providerID(host.Namespace, host.Name)
}

// waitingPoolMachines lists the Beskar7Machines in any namespace whose poolRef
// names one of the pools host is labelled into.
func (r *Beskar7MachineReconciler) waitingPoolMachines(ctx context.Context, host *infrav1.PhysicalHost) ([]infrav1.Beskar7Machine, error) {
	if !r.HostPools {
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
// namespace through its pool, checked like the callback server checks it, and
// reports whether the claim is still valid. A claim in the machine's own
// namespace is always valid.
func (r *Beskar7MachineReconciler) markPoolClaim(
	ctx context.Context,
	b7machine *infrav1.Beskar7Machine,
	host *infrav1.PhysicalHost,
) (bool, error) {
	if host.Namespace == b7machine.Namespace || !claimedThroughPool(host, b7machine) {
		return true, nil
	}
	_, ok, err := resolveConsumer(ctx, r.claims(), host)
	if err != nil {
		return false, err
	}
	if ok {
		setTrue(b7machine, infrav1.PoolClaimValidCondition, infrav1.PoolClaimValidReason)
		return true, nil
	}
	setFalse(b7machine, infrav1.PoolClaimValidCondition, infrav1.PoolGrantRevokedReason,
		"PhysicalHost %s/%s left pool %q or the pool no longer grants this namespace; it stays claimed and "+
			"powered as it is, but it is not booted again and cannot reach its callbacks",
		host.Namespace, host.Name, host.Spec.ConsumerPool)
	return false, nil
}

// hostErrorWithheld stands in for a host's error on a machine in another namespace.
const hostErrorWithheld = "the host reported an error; its details are on the PhysicalHost, ask the host admin"

// hostErrorForMachine is the host error b7machine may show. A host in another
// namespace keeps its BMC errors to itself, since they can name the BMC address
// or its credentials Secret; a failure reported by the machine's own run and an
// inspection timeout carry nothing of the host namespace and are shown.
func hostErrorForMachine(b7machine *infrav1.Beskar7Machine, host *infrav1.PhysicalHost) string {
	msg := host.Status.ErrorMessage
	if host.Namespace == b7machine.Namespace ||
		strings.HasPrefix(msg, provisionFailedReasonPrefix) || msg == inspectionTimedOutMessage {
		return msg
	}
	return hostErrorWithheld
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
