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
	"github.com/pkg/errors"
	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
	internalmetrics "github.com/projectbeskar/beskar7/internal/metrics"
	"github.com/stmcginnis/gofish/schemas"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// hostPlacementSelector returns the label selector a PhysicalHost must satisfy
// before this machine may claim it, or nil when the machine is unconstrained.
//
// Two sources, ANDed:
//   - Beskar7Machine.spec.hostSelector: the operator's intent (which boxes are
//     the control plane, which rack). An empty selector is no constraint.
//   - Machine.spec.failureDomain: CAPI's placement. Beskar7Cluster publishes
//     the domains from the topology.kubernetes.io/zone label on PhysicalHosts
//     (reconcileFailureDomains); honouring the assignment at claim time is what
//     gives them any meaning — without it a Machine placed in rack-1 claims
//     whichever host happens to list first.
func hostPlacementSelector(b7machine *infrav1.Beskar7Machine, machine *clusterv1.Machine) (labels.Selector, error) {
	var sel labels.Selector
	if hs := b7machine.Spec.HostSelector; hs != nil {
		s, err := metav1.LabelSelectorAsSelector(hs)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errInvalidHostSelector, err)
		}
		if !s.Empty() {
			sel = s
		}
	}
	if machine != nil && machine.Spec.FailureDomain != "" {
		fd := machine.Spec.FailureDomain
		req, err := labels.NewRequirement(zoneLabelKey, selection.Equals, []string{fd})
		if err != nil {
			return nil, fmt.Errorf("failure domain %q is not a valid %s label value: %w", fd, zoneLabelKey, err)
		}
		if sel == nil {
			sel = labels.NewSelector()
		}
		sel = sel.Add(*req)
	}
	return sel, nil
}

// findAndClaimOrGetAssociatedHost finds an available host or returns the associated one.
//
// Lookup order:
//  1. By Spec.ProviderID — only set after inspection completes (handleReadyHost).
//  2. By Spec.ConsumerRef.Name pointing back at this Beskar7Machine — covers the
//     window between claim (which sets ConsumerRef and transitions the host to
//     InUse) and ProviderID assignment. Without this branch, once a host has
//     been claimed and transitioned out of StateAvailable, the controller would
//     never re-acquire it on subsequent reconciles — it would loop "No
//     available host" forever and the inspection flow would never trigger.
//     ConsumerRef is on Spec (not indexed); we list namespace-scoped and filter
//     in-loop. Namespace scope keeps the list bounded.
//  3. Find a StateAvailable host with no ConsumerRef that satisfies placement
//     (nil = any host) and claim it. Returned with RequeueAfter so the next
//     reconcile picks the host up via path (2). Paths (1) and (2) never apply
//     placement: a host this machine already holds stays its host.
func (r *Beskar7MachineReconciler) findAndClaimOrGetAssociatedHost(ctx context.Context, logger logr.Logger, b7machine *infrav1.Beskar7Machine, placement labels.Selector) (*infrav1.PhysicalHost, ctrl.Result, error) {
	claimStart := time.Now()

	// (1) ProviderID lookup, own namespace only; a pool host is found by (2).
	if b7machine.Spec.ProviderID != "" {
		ns, name, err := parseProviderID(b7machine.Spec.ProviderID)
		if err == nil && ns == b7machine.Namespace {
			host := &infrav1.PhysicalHost{}
			if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, host); err == nil {
				internalmetrics.RecordHostClaimAttempt(b7machine.Namespace, internalmetrics.ClaimOutcomeSuccess, internalmetrics.ConflictReasonNone)
				internalmetrics.RecordHostClaimDuration(b7machine.Namespace, internalmetrics.ClaimOutcomeSuccess, time.Since(claimStart))
				return host, ctrl.Result{}, nil
			}
		}
	}

	// (2) ConsumerRef lookup: find any host already claimed by this
	// Beskar7Machine, in its namespace or, for a pool machine, through its pool.
	// A held host stays found even when its pool stops granting.
	held, err := r.findHeldHost(ctx, b7machine, false)
	if err != nil {
		internalmetrics.RecordHostClaimAttempt(b7machine.Namespace, internalmetrics.ClaimOutcomeError, internalmetrics.ConflictReasonNone)
		internalmetrics.RecordHostClaimDuration(b7machine.Namespace, internalmetrics.ClaimOutcomeError, time.Since(claimStart))
		return nil, ctrl.Result{}, err
	}
	if held != nil {
		internalmetrics.RecordHostClaimAttempt(b7machine.Namespace, internalmetrics.ClaimOutcomeSuccess, internalmetrics.ConflictReasonNone)
		internalmetrics.RecordHostClaimDuration(b7machine.Namespace, internalmetrics.ClaimOutcomeSuccess, time.Since(claimStart))
		return held, ctrl.Result{}, nil
	}

	// A new claim needs the pool to exist and grant this namespace.
	search, available, err := r.claimSearch(ctx, b7machine)
	if err != nil {
		internalmetrics.RecordHostClaimAttempt(b7machine.Namespace, internalmetrics.ClaimOutcomeError, internalmetrics.ConflictReasonNone)
		internalmetrics.RecordHostClaimDuration(b7machine.Namespace, internalmetrics.ClaimOutcomeError, time.Since(claimStart))
		return nil, ctrl.Result{}, err
	}
	if !available {
		return nil, ctrl.Result{}, errPoolNotAvailable
	}
	selector, err := search.claimSelector(placement)
	if err != nil {
		return nil, ctrl.Result{}, err
	}

	// (3) No existing claim — list StateAvailable hosts and try to claim one.
	// The field index filters server-side to StateAvailable so the result set
	// is bounded by the count of free hosts.
	hostList := &infrav1.PhysicalHostList{}
	if selector != nil && !selector.Empty() {
		logger.V(1).Info("Restricting the host claim to a placement constraint", "placement", selector.String())
	}
	for _, ns := range search.namespaces {
		listOpts := []client.ListOption{
			client.InNamespace(ns),
			client.MatchingFields{PhysicalHostStateIndex: string(infrav1.StateAvailable)},
		}
		if selector != nil && !selector.Empty() {
			// Applied together with the field index: the cache resolves the index
			// first and filters labels in memory, so this stays bounded by the
			// number of free hosts.
			listOpts = append(listOpts, client.MatchingLabelsSelector{Selector: selector})
		}
		nsHosts := &infrav1.PhysicalHostList{}
		if err := r.List(ctx, nsHosts, listOpts...); err != nil {
			internalmetrics.RecordHostClaimAttempt(b7machine.Namespace, internalmetrics.ClaimOutcomeError, internalmetrics.ConflictReasonNone)
			internalmetrics.RecordHostClaimDuration(b7machine.Namespace, internalmetrics.ClaimOutcomeError, time.Since(claimStart))
			return nil, ctrl.Result{}, err
		}
		hostList.Items = append(hostList.Items, nsHosts.Items...)
	}

	for i := range hostList.Items {
		host := &hostList.Items[i]
		// ConsumerRef is on Spec (not indexed); keep the in-loop guard defensively.
		// A host can be Available in the index but have a stale ConsumerRef that has
		// not yet been cleared, so this check prevents a double-claim.
		if search.claimable(host) {
			// Claim this host. The List above is filtered server-side via the
			// status.state field index. The Patch uses MergeFromWithOptimisticLock
			// so a concurrent claim from another Beskar7Machine fails fast with a
			// Conflict; the loser requeues and re-lists, which now sees the
			// updated state and either picks another host or returns empty.
			// BUG-2 closed.
			logger.Info("Claiming available PhysicalHost", "host", host.Name)
			base := host.DeepCopy()
			host.Spec.ConsumerRef = &corev1.ObjectReference{
				// Use hardcoded kind/apiVersion constants: b7machine.Kind and
				// b7machine.APIVersion are zero-valued on decoded objects (CLAUDE.md anti-pattern).
				Kind:       "Beskar7Machine",
				APIVersion: InfrastructureAPIVersion,
				Name:       b7machine.Name,
				Namespace:  b7machine.Namespace,
				UID:        b7machine.UID,
			}
			host.Spec.ConsumerPool = search.pool
			if err := r.Patch(ctx, host, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				if apierrors.IsConflict(err) {
					// Another reconciler won the race; requeue and try again.
					logger.V(1).Info("Conflict claiming host, will retry", "host", host.Name)
					internalmetrics.RecordHostClaimAttempt(b7machine.Namespace, internalmetrics.ClaimOutcomeConflict, internalmetrics.ConflictReasonOptimisticLock)
					internalmetrics.RecordHostClaimDuration(b7machine.Namespace, internalmetrics.ClaimOutcomeConflict, time.Since(claimStart))
					return nil, ctrl.Result{RequeueAfter: requeueShortly}, nil
				}
				logger.Error(err, "Failed to claim host")
				internalmetrics.RecordHostClaimAttempt(b7machine.Namespace, internalmetrics.ClaimOutcomeError, internalmetrics.ConflictReasonNone)
				internalmetrics.RecordHostClaimDuration(b7machine.Namespace, internalmetrics.ClaimOutcomeError, time.Since(claimStart))
				return nil, ctrl.Result{}, err
			}
			internalmetrics.RecordHostClaimAttempt(b7machine.Namespace, internalmetrics.ClaimOutcomeSuccess, internalmetrics.ConflictReasonNone)
			internalmetrics.RecordHostClaimDuration(b7machine.Namespace, internalmetrics.ClaimOutcomeSuccess, time.Since(claimStart))
			return host, ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
	}

	internalmetrics.RecordHostClaimAttempt(b7machine.Namespace, internalmetrics.ClaimOutcomeNoHosts, internalmetrics.ConflictReasonNone)
	internalmetrics.RecordHostClaimDuration(b7machine.Namespace, internalmetrics.ClaimOutcomeNoHosts, time.Since(claimStart))
	return nil, ctrl.Result{}, nil
}

// findClaimedHostForRelease locates the PhysicalHost currently claimed by this
// Beskar7Machine so reconcileDelete can release it. ConsumerRef ownership is the
// source of truth: a machine deleted before inspection completes has a claimed
// host (ConsumerRef set) but no ProviderID yet (#107), so a ProviderID-only
// lookup would miss it and strand the host.
//
// ProviderID, when present and parseable, is used as a fast-path Get to avoid a
// list in the common provisioned case. If it is unset, malformed, or points at a
// host that is not (or no longer) claimed by us, we fall back to a namespace list
// scan keyed on ConsumerRef.Name — the same lookup findAndClaimOrGetAssociatedHost
// uses in the claim direction. Returns (nil, nil) when no host is claimed by us.
func (r *Beskar7MachineReconciler) findClaimedHostForRelease(ctx context.Context, logger logr.Logger, b7machine *infrav1.Beskar7Machine) (*infrav1.PhysicalHost, error) {
	// Fast path: ProviderID names the host directly.
	if b7machine.Spec.ProviderID != "" {
		ns, name, parseErr := parseProviderID(b7machine.Spec.ProviderID)
		if parseErr != nil {
			logger.V(1).Info("Cannot parse ProviderID during deletion; falling back to ConsumerRef scan", "err", parseErr)
		} else {
			host := &infrav1.PhysicalHost{}
			err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, host)
			switch {
			case err == nil:
				// ProviderID can name a host in any namespace; only a host claimed
				// by this very machine, namespace included, is ours to release (SEC-12).
				if claimedBy(host, b7machine) {
					return host, nil
				}
				// ProviderID points at a host not claimed by us; fall through to scan.
			case apierrors.IsNotFound(err):
				// Named host already gone; fall through in case a different host is ours.
			default:
				return nil, err
			}
		}
	}

	// Fallback: scan for any host whose ConsumerRef names us. This covers the
	// claimed-but-not-yet-provisioned window where ProviderID is unset.
	return r.findHeldHost(ctx, b7machine, true)
}

// releaseHostPower issues ClearBootSourceOverride and a power-off against the
// BMC of a host this Beskar7Machine is releasing, and reports how long to wait
// before looking again, or zero once the host may be released.
//
// A host released mid-run is forced off and released at once: it is running the
// inspector, which has nothing that acts on the ACPI power button, and nothing
// on it needs a clean shutdown. Any other host is shut down gracefully, and a
// graceful shutdown is only a request. A host released soon after its OS booted
// can lose it, and the host then stays on indefinitely, where its kubelet can
// register a Node into the cluster the Machine was just removed from. So a
// request the BMC accepted is followed until the host reads Off
// (confirmReleasedHostOff), and forced after releaseShutdownGrace if it does not.
//
// Every failure is logged at Info and swallowed, never returned, so a dead BMC
// cannot strand the Beskar7Machine finalizer: one before the request was
// accepted releases the host at once, as it always has, and one while the
// request is being followed is retried until releaseForceOffWindow past the
// grace. Missing credentials, or a credentials Secret that does not authorise
// the host's address (D-030), are treated like the former either way.
func (r *Beskar7MachineReconciler) releaseHostPower(ctx context.Context, logger logr.Logger, b7machine *infrav1.Beskar7Machine, host *infrav1.PhysicalHost) time.Duration {
	if requestedAt, ok := releasePowerOffRequestedAt(b7machine); ok {
		return r.confirmReleasedHostOff(ctx, logger, b7machine, host, requestedAt)
	}

	rfClient, err := r.getRedfishClientForHost(ctx, logger, host)
	if err != nil {
		logger.Info("Could not get Redfish client during release; skipping power-off and boot clear", "err", err)
		return 0
	}
	defer rfClient.Close(ctx)

	if err := rfClient.ClearBootSourceOverride(ctx); err != nil {
		logger.Info("Failed to clear boot source override during release; continuing", "err", err)
	}
	midRun := releasedMidRun(host)
	how := "graceful"
	if midRun {
		how = "forced"
		err = rfClient.ForcePowerOff(ctx)
	} else {
		err = rfClient.SetPowerState(ctx, schemas.OffPowerState)
	}
	if err != nil {
		logger.Info("Failed to power off during release; continuing", "powerOff", how, "err", err)
		internalmetrics.RecordPhysicalHostPowerOperation(internalmetrics.PowerOperationOff, host.Namespace, internalmetrics.ProvisioningOutcomeFailed)
		return 0
	}
	internalmetrics.RecordPhysicalHostPowerOperation(internalmetrics.PowerOperationOff, host.Namespace, internalmetrics.ProvisioningOutcomeSuccess)
	if midRun {
		return 0
	}

	if state, err := rfClient.GetPowerState(ctx); err == nil && state == schemas.OffPowerState {
		return 0
	}
	setReleasePowerOffRequestedAt(b7machine, time.Now())
	logger.Info("Graceful shutdown requested; keeping the host claimed until it is off", "host", host.Name, "grace", releaseShutdownGrace)
	return releasePowerPollInterval
}

// confirmReleasedHostOff is the pass of a release that follows a graceful
// shutdown the BMC accepted at requestedAt. It reports how long to wait before
// the next pass, or zero once the host may be released: it reads Off, the
// ForcePowerOff that followed a missed grace period was accepted, the BMC could
// not be made to answer for releaseShutdownGrace + releaseForceOffWindow, or its
// credentials no longer authorise a connection.
//
// The deadlines are measured from requestedAt, which the Beskar7Machine carries
// (ReleasePowerOffRequestedAnnotation), so a manager restart does not restart
// them. An annotation dated in the future (a clock stepped back, a hand edit) is
// taken as now, so it cannot hold the claim longer than the bound.
func (r *Beskar7MachineReconciler) confirmReleasedHostOff(ctx context.Context, logger logr.Logger, b7machine *infrav1.Beskar7Machine, host *infrav1.PhysicalHost, requestedAt time.Time) time.Duration {
	now := time.Now()
	if requestedAt.After(now) {
		requestedAt = now
		setReleasePowerOffRequestedAt(b7machine, now)
	}
	waited := now.Sub(requestedAt)
	if waited >= releaseShutdownGrace+releaseForceOffWindow {
		logger.Info("Could not confirm the released host is off in time; releasing it anyway", "host", host.Name, "waited", waited.Round(time.Second))
		return 0
	}

	rfClient, err := r.getRedfishClientForHost(ctx, logger, host)
	if err != nil {
		var refusal *bmcAccessError
		if errors.As(err, &refusal) {
			logger.Info("Cannot reach the released host's BMC, and nothing will change that by waiting; releasing it without confirming it is off", "host", host.Name, "err", err)
			return 0
		}
		logger.Info("Could not get Redfish client while waiting for the released host to power off; will retry", "host", host.Name, "err", err)
		return releasePowerPollInterval
	}
	defer rfClient.Close(ctx)

	state, err := rfClient.GetPowerState(ctx)
	switch {
	case err != nil:
		logger.Info("Could not read the released host's power state; will retry", "host", host.Name, "err", err)
		return releasePowerPollInterval
	case state == schemas.OffPowerState:
		logger.Info("Released host is off", "host", host.Name, "waited", waited.Round(time.Second))
		return 0
	case waited < releaseShutdownGrace:
		logger.V(1).Info("Released host is still shutting down", "host", host.Name, "state", string(state), "waited", waited.Round(time.Second))
		return releasePowerPollInterval
	}

	logger.Info("Released host did not act on the graceful shutdown; forcing it off", "host", host.Name, "state", string(state), "waited", waited.Round(time.Second))
	if err := rfClient.ForcePowerOff(ctx); err != nil {
		logger.Info("Failed to force the released host off; will retry", "host", host.Name, "err", err)
		internalmetrics.RecordPhysicalHostPowerOperation(internalmetrics.PowerOperationOff, host.Namespace, internalmetrics.ProvisioningOutcomeFailed)
		return releasePowerPollInterval
	}
	internalmetrics.RecordPhysicalHostPowerOperation(internalmetrics.PowerOperationOff, host.Namespace, internalmetrics.ProvisioningOutcomeSuccess)
	return 0
}

// releasePowerOffRequestedAt returns the time ReleasePowerOffRequestedAnnotation
// records on b7machine. An annotation that is absent or not a time reads as
// unset, so a release that finds a bad one sends the request again and writes a
// good one.
func releasePowerOffRequestedAt(b7machine *infrav1.Beskar7Machine) (time.Time, bool) {
	raw, ok := b7machine.Annotations[ReleasePowerOffRequestedAnnotation]
	if !ok {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false
	}
	return at, true
}

func setReleasePowerOffRequestedAt(b7machine *infrav1.Beskar7Machine, at time.Time) {
	if b7machine.Annotations == nil {
		b7machine.Annotations = map[string]string{}
	}
	b7machine.Annotations[ReleasePowerOffRequestedAnnotation] = at.UTC().Format(time.RFC3339)
}

// releasedMidRun reports whether a host being released was in a provisioning
// run that never finished, and so may be running the inspector.
func releasedMidRun(host *infrav1.PhysicalHost) bool {
	switch host.Status.State {
	case infrav1.StateInspecting, infrav1.StateDeploying, infrav1.StateError:
		return true
	}
	return false
}
