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

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
	"github.com/projectbeskar/beskar7/internal/auth"
	internalmetrics "github.com/projectbeskar/beskar7/internal/metrics"
	internalredfish "github.com/projectbeskar/beskar7/internal/redfish"
	"github.com/stmcginnis/gofish/schemas"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/cluster-api/util/paused"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	// Beskar7MachineFinalizer allows cleanup before removal
	Beskar7MachineFinalizer = "beskar7machine.infrastructure.cluster.x-k8s.io"

	// ProviderIDPrefix is the prefix used for ProviderID
	ProviderIDPrefix = "b7://"

	// InfrastructureAPIVersion for owner references
	InfrastructureAPIVersion = "infrastructure.cluster.x-k8s.io/v1beta2"

	// DefaultInspectionTimeout is the default bound on the Inspecting phase.
	DefaultInspectionTimeout = 10 * time.Minute

	// DefaultDeploymentTimeout is the default bound on the Deploying phase (D-015).
	// Deploying encompasses the inspector writing a multi-GB OS image to disk, so
	// the default is larger than the inspection timeout. Operators with fast storage
	// or very small images may lower it; operators with slow links should raise it.
	DefaultDeploymentTimeout = 20 * time.Minute

	// InspectionPowerRecheckDelay is how long into an inspection the controller
	// waits before it will believe a report that the host is powered off.
	// A freshly powered-on host can still be advertising a stale Off for a short
	// while, so reacting immediately would power-cycle healthy hosts; ten minutes
	// of silence is far too long to wait, so the check sits between the two.
	InspectionPowerRecheckDelay = 2 * time.Minute

	// DeploymentTimedOutReason is the terminal reason set when a host stays in
	// StateDeploying longer than the configured deployment timeout (D-015).
	DeploymentTimedOutReason = "DeploymentTimedOut"

	// ForceReleaseAnnotation, when set to "true" on a Beskar7Machine being deleted,
	// causes the controller to skip the Redfish power-off and boot-override clear
	// during deletion. Use only when the BMC is permanently unreachable.
	ForceReleaseAnnotation = "infrastructure.cluster.x-k8s.io/force-release"

	// PhysicalHostStateIndex is the cache field index key for PhysicalHost.Status.State.
	// Registered in SetupWithManager; used in findAndClaimOrGetAssociatedHost to filter
	// Available hosts server-side instead of listing all hosts and filtering in Go.
	PhysicalHostStateIndex = "status.state"

	// clusterctlDeleteForMoveAnnotation is clusterctlv1.DeleteForMoveAnnotation
	// (sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3/annotations.go).
	// Beskar7 does not depend on the clusterctl client module, so the key is
	// copied here rather than imported. The mover patches it onto the source
	// object with an empty value immediately before Delete, and may
	// force-strip finalizers itself moments later without waiting for this
	// controller to react (mover.go deleteSourceObject) — Cluster.spec.paused
	// is already true on the source by then, which normally keeps Reconcile
	// from ever reaching reconcileDelete, but a stale cache read of that
	// pause is the race this annotation guards against. Present, it marks
	// this deletion as the source side of a move: reconcileDelete (D-028)
	// must not power the host off or release its claim, because the same
	// host is being force-moved to the target (its move-hierarchy label,
	// physicalhost_types.go) with the claim still live in Spec.
	clusterctlDeleteForMoveAnnotation = "clusterctl.cluster.x-k8s.io/delete-for-move"
)

// Beskar7MachineReconciler reconciles a Beskar7Machine object.
// Simplified for iPXE + inspection workflow.
type Beskar7MachineReconciler struct {
	client.Client
	Scheme               *runtime.Scheme
	RedfishClientFactory internalredfish.RedfishClientFactory
	Log                  logr.Logger
	// BootstrapURLBase is the scheme+host+port of the manager's bootstrap/inspection
	// endpoint. Used to compute deterministic per-host bootstrap URLs of the form
	// <BootstrapURLBase>/api/v1/bootstrap/<namespace>/<name>. Must be non-empty;
	// validated in SetupWithManager.
	BootstrapURLBase string
	// MaxConcurrentReconciles is the worker count for this controller. Zero
	// means DefaultMaxConcurrentReconciles (1).
	MaxConcurrentReconciles int
	// InspectionTimeout bounds how long a host may stay in Inspecting before the
	// machine is marked terminally failed (InspectionTimedOut). Zero means use
	// DefaultInspectionTimeout. Set from the --inspection-timeout manager flag so
	// operators with slow-POST hardware can raise it.
	InspectionTimeout time.Duration
	// DeploymentTimeout bounds how long a host may stay in Deploying before the
	// machine is marked terminally failed (DeploymentTimedOut, D-015). Zero means
	// use DefaultDeploymentTimeout. Set from the --deployment-timeout manager flag.
	DeploymentTimeout time.Duration
}

// inspectionTimeout returns the configured inspection timeout, falling back to
// DefaultInspectionTimeout when unset (zero). Guards against a zero-value field
// that would otherwise time out every inspection instantly.
func (r *Beskar7MachineReconciler) inspectionTimeout() time.Duration {
	if r.InspectionTimeout > 0 {
		return r.InspectionTimeout
	}
	return DefaultInspectionTimeout
}

// deploymentTimeout returns the configured deployment timeout (D-015), falling back
// to DefaultDeploymentTimeout when unset (zero). Guards against a zero-value field
// that would otherwise time out every deployment instantly.
func (r *Beskar7MachineReconciler) deploymentTimeout() time.Duration {
	if r.DeploymentTimeout > 0 {
		return r.DeploymentTimeout
	}
	return DefaultDeploymentTimeout
}

//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=beskar7machines,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=beskar7machines/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=beskar7machines/finalizers,verbs=update
//+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines;machines/status,verbs=get;list;watch
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=physicalhosts,verbs=get;list;watch;patch
// Beskar7MachineTemplate is read-only — there is no template controller. CAPI's
// MachineDeployment / KCP walks the template via the shared informer cache, so
// list+watch are required. The chart's hand-maintained RBAC has carried this
// rule for a long time; this marker makes the generated config/rbac/role.yaml
// match the chart so kustomize-based installs are not silently more restrictive.
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=beskar7machinetemplates,verbs=get;list;watch
// Secret access in this controller is by name only:
//   - getRedfishClientForHost: r.Get on the BMC credentials Secret named
//     by host.Spec.RedfishConnection.CredentialsSecretRef.
//   - reconcileBootstrapData: r.Get on the bootstrap-data Secret named by
//     machine.Spec.Bootstrap.DataSecretName.
//   - ensureBootstrapCredentials / backfillBootstrapCredentials: Get, then
//     Create or Update at the read resourceVersion, on the per-host
//     bootstrap-token Secret (deterministic name; PhysicalHost-owned).
// No code path performs List or Watch over Secrets here, so list/watch
// are intentionally omitted (SEC-2 / D-007). The aggregate ClusterRole
// will still grant secrets:list,watch because PhysicalHostReconciler's
// SetupWithManager registers a Secret informer.
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;create;update;patch;delete

// Reconcile handles Beskar7Machine reconciliation for iPXE + inspection workflow.
func (r *Beskar7MachineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, reterr error) {
	log := r.Log.WithValues("beskar7machine", req.NamespacedName)
	log.Info("Starting reconciliation")

	// Fetch the Beskar7Machine
	b7machine := &infrav1.Beskar7Machine{}
	err := r.Get(ctx, req.NamespacedName, b7machine)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Beskar7Machine resource not found, ignoring")
			return ctrl.Result{}, nil
		}
		log.Error(err, "Unable to fetch Beskar7Machine")
		return ctrl.Result{}, err
	}

	// Recompute phase-gauge metrics on every reconcile so the gauge stays current
	// even when reconcile short-circuits. Non-fatal: metric errors don't block reconcile.
	r.recomputeBeskar7MachineMetrics(ctx, log, req.Namespace)

	// Fetch the owner Machine
	machine, err := util.GetOwnerMachine(ctx, r.Client, b7machine.ObjectMeta)
	if err != nil {
		log.Error(err, "Failed to get owner Machine")
		return ctrl.Result{}, err
	}
	if machine == nil {
		log.Info("Waiting for Machine Controller to set OwnerRef")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	log = log.WithValues("machine", machine.Name)

	// Get the owner cluster
	cluster, err := util.GetClusterFromMetadata(ctx, r.Client, machine.ObjectMeta)
	if err != nil {
		log.Error(err, "Failed to get cluster from machine metadata")
		return ctrl.Result{}, err
	}

	// Cluster.spec.paused, the paused annotation on the Cluster and the one on
	// this object all pause reconciliation; the helper also keeps the Paused
	// condition current, which is why it runs before the patch helper snapshot.
	if isPaused, requeue, err := paused.EnsurePausedCondition(ctx, r.Client, cluster, b7machine); err != nil || isPaused || requeue {
		if isPaused {
			log.Info("Beskar7Machine reconciliation is paused")
		}
		return ctrl.Result{}, err
	}

	// Initialize patch helper
	patchHelper, err := patch.NewHelper(b7machine, r.Client)
	if err != nil {
		log.Error(err, "Failed to init patch helper")
		return ctrl.Result{}, err
	}

	// Objects written before v0.6.0 carry conditions with no reason, which the
	// metav1.Condition schema rejects on write. Repair them here, after the
	// helper has snapshotted the object, so the fix is seen as a change.
	repairLegacyConditions(b7machine, log)

	// Always patch on exit
	defer func() {
		setReadySummary(b7machine, log, infrav1.InfrastructureReadyCondition, infrav1.PhysicalHostAssociatedCondition, infrav1.BootstrapDataReadyCondition)
		if err := patchHelper.Patch(ctx, b7machine, patch.WithOwnedConditions{Conditions: []string{
			clusterv1.ReadyCondition,
			clusterv1.PausedCondition,
			infrav1.InfrastructureReadyCondition,
			infrav1.PhysicalHostAssociatedCondition,
			infrav1.BootstrapDataReadyCondition,
		}}); err != nil {
			log.Error(err, "Failed to patch Beskar7Machine")
			if reterr == nil {
				reterr = err
			}
		}
		log.Info("Finished reconciliation")
	}()

	// Handle deletion
	if !b7machine.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, log, b7machine)
	}

	// A terminal failure must actually be terminal. markTerminalFailure marks
	// the machine Failed and never clears it ("needs operator intervention"),
	// but nothing stopped a later reconcile from running the normal state
	// machine anyway: if the PhysicalHost subsequently recovered,
	// handleReadyHost would set Ready/Provisioned on a machine that had already
	// failed. That was observed on real hardware — a Beskar7Machine ended up
	// Ready=true, Phase=Provisioned AND a terminal InspectionTimedOut failure.
	//
	// The contradiction is not cosmetic. CAPI mirrors the Ready summary into
	// the owning Machine's InfrastructureReady condition and a MachineHealthCheck
	// may remediate on it, so the pair "provisioned and permanently failed" both
	// misleads an operator reading status and invites remediation of a node that
	// is actually serving traffic.
	//
	// Deletion is handled above, so a failed machine can still be deleted and
	// release its PhysicalHost. Under a MachineDeployment that is precisely how
	// CAPI self-heals: the failed replica is deleted and replaced.
	if isTerminallyFailed(b7machine) {
		log.Info("Beskar7Machine is in a terminal failure state; skipping reconciliation",
			"reason", conditions.GetReason(b7machine, infrav1.InfrastructureReadyCondition))
		return ctrl.Result{}, nil
	}

	// Handle normal reconciliation
	return r.reconcileNormal(ctx, log, b7machine, machine)
}

// reconcileNormal handles normal (non-deletion) reconciliation.
func (r *Beskar7MachineReconciler) reconcileNormal(ctx context.Context, logger logr.Logger, b7machine *infrav1.Beskar7Machine, machine *clusterv1.Machine) (ctrl.Result, error) {
	logger.Info("Reconciling Beskar7Machine create/update")

	// Add finalizer
	if controllerutil.AddFinalizer(b7machine, Beskar7MachineFinalizer) {
		logger.Info("Adding finalizer")
		return ctrl.Result{RequeueAfter: requeueShortly}, nil
	}

	// Find or get associated host. The spec's hostSelector and the failure
	// domain CAPI placed the owning Machine into both constrain a fresh claim.
	placement, err := hostPlacementSelector(b7machine, machine)
	if err != nil {
		if errors.Is(err, errInvalidHostSelector) {
			// It can never match; the spec has to change. Terminal, so it shows
			// in `kubectl describe machine` instead of requeueing forever.
			logger.Error(err, "Beskar7Machine hostSelector is invalid")
			setFalse(b7machine, infrav1.PhysicalHostAssociatedCondition, infrav1.InvalidHostSelectorReason, "%v", err)
			r.markTerminalFailure(b7machine, infrav1.InvalidHostSelectorReason, err.Error())
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Invalid host placement constraint")
		setFalse(b7machine, infrav1.PhysicalHostAssociatedCondition, infrav1.PhysicalHostAssociationFailedReason, "Invalid host placement constraint: %v", err)
		return ctrl.Result{}, err
	}
	physicalHost, result, err := r.findAndClaimOrGetAssociatedHost(ctx, logger, b7machine, placement)
	if err != nil {
		logger.Error(err, "Failed to find, claim, or get associated PhysicalHost")
		setFalse(b7machine, infrav1.PhysicalHostAssociatedCondition, infrav1.PhysicalHostAssociationFailedReason, "Failed to associate with PhysicalHost: %v", err)
		internalmetrics.RecordError("beskar7machine", b7machine.Namespace, internalmetrics.ErrorTypeTransient)
		return result, err
	}

	if physicalHost != nil {
		logger.Info("Successfully associated with PhysicalHost", "physicalhost", physicalHost.Name)
		setTrue(b7machine, infrav1.PhysicalHostAssociatedCondition, infrav1.PhysicalHostAssociatedReason)
		// The Ready summary skips a condition that is missing, so with
		// PhysicalHostAssociated=True as its only input it would publish
		// Ready=True. The state machine further down sets InfrastructureReady
		// on a full pass; this covers every return before it (the pass that
		// claims the host, an error on the way), including one working from a
		// cached copy that predates the previous pass's write.
		if !conditions.Has(b7machine, infrav1.InfrastructureReadyCondition) {
			setFalse(b7machine, infrav1.InfrastructureReadyCondition, infrav1.PhysicalHostNotReadyReason,
				"PhysicalHost %q is claimed and not provisioned yet", physicalHost.Name)
		}
	} else if placement != nil {
		// Distinct from an empty inventory: hosts may well be Available, just
		// not where CAPI placed this Machine. Requeue, never terminal — a host in
		// that domain can free up, or the operator can label one. The minute is
		// a backstop: a host entering Available re-enqueues the machine at once
		// (AvailablePhysicalHostToWaitingBeskar7Machines).
		logger.Info("No available PhysicalHost satisfies the placement constraint, requeuing", "placement", placement.String())
		setFalse(b7machine, infrav1.PhysicalHostAssociatedCondition, infrav1.NoMatchingPhysicalHostReason, "No available PhysicalHost matches the placement constraint %s", placement.String())
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	} else {
		logger.Info("No available or associated PhysicalHost found, requeuing")
		setFalse(b7machine, infrav1.PhysicalHostAssociatedCondition, infrav1.WaitingForPhysicalHostReason, "No available PhysicalHost found")
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	if !result.IsZero() {
		return result, nil
	}

	logger = logger.WithValues("physicalhost", physicalHost.Name)

	// Bootstrap data must be available before we boot the inspection image, so
	// the host can fetch it during cloud-init / Ignition. We do not fetch the
	// bytes here (that's the server-side bootstrap endpoint's job) — only verify
	// the Secret exists and signal the URL to PhysicalHost.
	if result, err := r.ensureBootstrapDataReady(ctx, logger, b7machine, machine, physicalHost); err != nil || !result.IsZero() {
		if err != nil {
			internalmetrics.RecordError("beskar7machine", b7machine.Namespace, internalmetrics.ErrorTypeTransient)
		}
		return result, err
	}

	// A run that was past InUse when the manager was upgraded to D-029 keeps
	// its credentials only once they are bound to this machine; triggerInspection
	// does the same for InUse. Removed in the next minor release.
	if physicalHost.Status.State == infrav1.StateInspecting || physicalHost.Status.State == infrav1.StateDeploying {
		if err := r.backfillBootstrapCredentials(ctx, logger, b7machine, physicalHost); err != nil {
			logger.Error(err, "Failed to backfill the host's bootstrap credentials")
			return ctrl.Result{}, err
		}
	}

	// Handle based on PhysicalHost state and inspection status
	return r.handlePhysicalHostState(ctx, logger, b7machine, physicalHost)
}

// handlePhysicalHostState processes the PhysicalHost based on its current state.
//
// Every state but Ready writes InfrastructureReady=False. The Ready summary
// skips a condition that is missing, and Cluster API mirrors that summary onto
// the owning Machine, where a MachineHealthCheck times how long it has been
// False. A state that left the condition alone would report the machine Ready
// while its host was still being inspected, and restart that clock when the
// next state set it False again.
func (r *Beskar7MachineReconciler) handlePhysicalHostState(ctx context.Context, logger logr.Logger, b7machine *infrav1.Beskar7Machine, physicalHost *infrav1.PhysicalHost) (ctrl.Result, error) {
	switch physicalHost.Status.State {
	case infrav1.StateReady:
		// OS deployment complete (provisioned callback received); set ProviderID + Ready.
		logger.Info("PhysicalHost deployment complete and ready")
		return r.handleReadyHost(ctx, logger, b7machine, physicalHost)

	case infrav1.StateDeploying:
		// Inspector is writing the OS image; monitor for completion or timeout (D-015).
		logger.Info("PhysicalHost OS deployment in progress")
		return r.handleDeployingHost(ctx, logger, b7machine, physicalHost)

	case infrav1.StateInspecting:
		// Inspection in progress
		logger.Info("PhysicalHost inspection in progress")
		setFalse(b7machine, infrav1.InfrastructureReadyCondition, infrav1.PhysicalHostNotReadyReason, "PhysicalHost %q is being inspected", physicalHost.Name)
		return r.handleInspectingHost(ctx, logger, b7machine, physicalHost)

	case infrav1.StateInUse:
		// A ProviderID already set means this machine held this host at Ready
		// before — findAndClaimOrGetAssociatedHost never reassociates a
		// non-empty ProviderID with a different host, so the only host it can
		// name is the one that produced it. InUse here is not a fresh claim:
		// most commonly a clusterctl move landed the pair with status
		// stripped (D-028), and the host has not yet run adoptProvisionedClaim
		// to restore Ready. Booting the inspector would wipe and re-inspect
		// hardware that is already serving. Wait instead — the host's own
		// reconcile wakes this machine again through the existing
		// PhysicalHostToBeskar7Machine watch the moment it adopts.
		if b7machine.Spec.ProviderID != "" {
			logger.Info("PhysicalHost claimed but already held by this machine's ProviderID; waiting for host adoption instead of re-inspecting", "physicalhost", physicalHost.Name)
			setFalse(b7machine, infrav1.InfrastructureReadyCondition, infrav1.WaitingForHostAdoptionReason,
				"Waiting for PhysicalHost %q to reassert Ready (already held by this machine's ProviderID)", physicalHost.Name)
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		// Host claimed, need to trigger inspection
		logger.Info("PhysicalHost claimed, triggering inspection")
		setFalse(b7machine, infrav1.InfrastructureReadyCondition, infrav1.PhysicalHostNotReadyReason, "Starting inspection of PhysicalHost %q", physicalHost.Name)
		return r.triggerInspection(ctx, logger, b7machine, physicalHost)

	case infrav1.StateError:
		if hostWaitingForBMC(physicalHost) {
			// An unreachable BMC is a fact about the world, not about this
			// machine, and it usually clears by itself. Failing the machine for
			// it would have its replacement wipe and reprovision a host that was
			// never broken. The host's recovery wakes this machine through the
			// PhysicalHost watch; the requeue is only a backstop.
			logger.Info("PhysicalHost cannot reach its BMC; waiting for it to recover", "errorMessage", physicalHost.Status.ErrorMessage)
			setFalse(b7machine, infrav1.InfrastructureReadyCondition, infrav1.WaitingForBMCReason,
				"Waiting for PhysicalHost %q: %s", physicalHost.Name, physicalHost.Status.ErrorMessage)
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		logger.Error(nil, "PhysicalHost is in error state", "errorMessage", physicalHost.Status.ErrorMessage)
		// Distinguish a deploy-reported failure (inspector POST /provision-failed, v4.1) from
		// a Redfish/BMC-level error. The provision-failed handler prefixes ErrorMessage with
		// provisionFailedReasonPrefix; all other error paths do not use that prefix.
		// Using the prefix as the discriminator keeps the distinction simple and avoids adding
		// a new CRD field — the PhysicalHost.Status.ErrorMessage already carries the full
		// sanitized reason that the operator needs to diagnose the failure.
		//
		// An inspection timeout is named here too, although this controller fails the
		// machine itself when it writes the timeout annotation: the annotation reaches
		// the host before that reconcile's own patch, so if the patch does not land the
		// next reconcile finds the host's Error instead.
		reason := infrav1.PhysicalHostErrorReason
		switch {
		case strings.HasPrefix(physicalHost.Status.ErrorMessage, provisionFailedReasonPrefix):
			reason = infrav1.DeploymentFailedReason
		case physicalHost.Status.ErrorMessage == inspectionTimedOutMessage:
			reason = infrav1.InspectionTimedOutReason
		}
		msg := fmt.Sprintf("PhysicalHost %q in error state: %s", physicalHost.Name, physicalHost.Status.ErrorMessage)
		// markTerminalFailure sets Phase=Failed, Ready=false and
		// InfrastructureReady=False with this reason in one call.
		r.markTerminalFailure(b7machine, reason, msg)
		return ctrl.Result{}, nil

	default:
		logger.Info("PhysicalHost in intermediate state", "hostState", physicalHost.Status.State)
		setFalse(b7machine, infrav1.InfrastructureReadyCondition, infrav1.PhysicalHostNotReadyReason, "PhysicalHost %q is in state: %s", physicalHost.Name, physicalHost.Status.State)
		phase := "Pending"
		b7machine.Status.Phase = &phase
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
}

// hostWaitingForBMC reports whether a PhysicalHost in StateError is there only
// because it cannot reach its BMC: the one Error the host clears by itself,
// retrying on a flat interval until the BMC answers.
//
// The host publishes that class on its RedfishConnectionReady condition —
// False with BMCUnreachableReason, which only retryTransientRedfishFailure
// sets — so this does not have to guess from the error message.
//
// True counts as waiting as well. The patch helper writes conditions in a call
// of their own, ahead of the rest of status, so a host whose BMC has just
// answered again is published once with the condition True and State still
// Error; failing the machine on that version would bring the outage's damage
// back through a race.
//
// An Error the provisioning run reported — the inspector's /provision-failed
// report, or the inspection timeout — stays terminal whatever the condition
// says (provisioningRunFailed). The host sets it after a successful
// connection, so it normally reads True, and keeps it through a later outage,
// which turns the condition False with BMCUnreachable.
//
// Without the condition the class is unknown, and the Error stays terminal.
func hostWaitingForBMC(physicalHost *infrav1.PhysicalHost) bool {
	if provisioningRunFailed(physicalHost) {
		return false
	}
	cond := conditions.Get(physicalHost, infrav1.RedfishConnectionReadyCondition)
	if cond == nil {
		return false
	}
	switch cond.Status {
	case metav1.ConditionFalse:
		return cond.Reason == infrav1.BMCUnreachableReason
	case metav1.ConditionTrue:
		return true
	}
	return false
}

// triggerInspection initiates the inspection phase by booting the inspection image.
//
// Sequence:
//  1. Configure BMC for PXE boot and ensure power-on.
//  2. Mint the per-host bearer token (D-004) and boot nonce (D-009) into the
//     host's bootstrap-token Secret, bound to this machine (D-006, D-029). The
//     Secret is written before the inspection-request annotation, so the host
//     never starts inspecting without credentials.
//  3. Signal the PhysicalHost controller to transition to Inspecting via the
//     inspection-request annotation (Pattern A; PhysicalHost owns its own status).
//
// We never write to PhysicalHost.Status here — the inspection-request travels
// through metadata.annotations and is applied by the PhysicalHost reconciler on
// its next pass (BUG-1), which also mirrors the Secret into status.
func (r *Beskar7MachineReconciler) triggerInspection(ctx context.Context, logger logr.Logger, b7machine *infrav1.Beskar7Machine, physicalHost *infrav1.PhysicalHost) (ctrl.Result, error) {
	logger.Info("Triggering inspection boot")

	// Get Redfish client
	rfClient, err := r.getRedfishClientForHost(ctx, logger, physicalHost)
	if err != nil {
		logger.Error(err, "Failed to get Redfish client")
		return ctrl.Result{}, err
	}
	defer rfClient.Close(ctx)

	// Set boot to PXE
	if err := rfClient.SetBootSourcePXE(ctx); err != nil {
		logger.Error(err, "Failed to set boot source to PXE")
		return ctrl.Result{}, err
	}

	// Power on the system
	powerState, err := rfClient.GetPowerState(ctx)
	if err != nil {
		logger.Error(err, "Failed to get power state")
		return ctrl.Result{}, err
	}

	if powerState != schemas.OnPowerState {
		if err := rfClient.SetPowerState(ctx, schemas.OnPowerState); err != nil {
			logger.Error(err, "Failed to power on system")
			internalmetrics.RecordPhysicalHostPowerOperation(internalmetrics.PowerOperationOn, physicalHost.Namespace, internalmetrics.ProvisioningOutcomeFailed)
			return ctrl.Result{}, err
		}
		internalmetrics.RecordPhysicalHostPowerOperation(internalmetrics.PowerOperationOn, physicalHost.Namespace, internalmetrics.ProvisioningOutcomeSuccess)
		logger.Info("Powered on system for inspection")
	}

	// Mint the host's callback credentials unless the ones it has are still
	// valid and bound to this machine (ensureBootstrapCredentials). Re-minting
	// on every reconcile would invalidate a token or nonce the inspector or the
	// operator's boot service already holds; the 60-minute token lifetime
	// (D-004) is comfortably above the 10-minute DefaultInspectionTimeout.
	// The Secret is the only place they live (D-029), and the plaintext is
	// never logged.
	if err := r.ensureBootstrapCredentials(ctx, logger, b7machine, physicalHost, time.Now()); err != nil {
		logger.Error(err, "Failed to ensure the host's bootstrap credentials")
		return ctrl.Result{}, err
	}

	// Signal the PhysicalHost controller to transition to Inspecting. We patch only
	// spec/annotations — never status — so this controller does not violate the
	// "each controller owns its resource's status" rule (BUG-1).
	if err := r.setInspectionRequestAnnotation(ctx, logger, physicalHost, "inspect"); err != nil {
		return ctrl.Result{}, err
	}

	phase := "Inspecting"
	b7machine.Status.Phase = &phase
	logger.Info("Inspection boot triggered successfully")
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// ensureBootstrapCredentials makes the host's bootstrap-token Secret hold a
// bearer token and a boot nonce bound to b7machine (D-029), minting whichever
// cannot be reused, and writes the Secret at most once.
//
// A credential is reused only when the Secret is already bound to b7machine by
// name and the credential is unexpired; the nonce must also be unconsumed.
// Anything else — a Secret bound to an earlier claim, one from before the
// binding existed, an expired or missing credential — mints afresh, so a
// token captured during one claim never serves the next (SEC-13). Re-minting a
// credential that is still valid would invalidate what the inspector or the
// boot service already holds, which is why a valid one is kept.
//
// The write carries the resourceVersion of the Secret the decision was made
// from, and a create fails if the Secret appeared meanwhile. A decision made
// from a stale cache therefore fails with a Conflict (or AlreadyExists) instead
// of replacing a newer mint, and the reconcile retries from a fresh read (the
// mint race of D-024). Neither plaintext is ever logged.
func (r *Beskar7MachineReconciler) ensureBootstrapCredentials(
	ctx context.Context,
	logger logr.Logger,
	b7machine *infrav1.Beskar7Machine,
	physicalHost *infrav1.PhysicalHost,
	now time.Time,
) error {
	secret, err := r.getBootstrapTokenSecret(ctx, physicalHost)
	if err != nil {
		return err
	}
	creds := readBootstrapCredentials(secret)
	changed := backfillLegacyCredentials(&creds, physicalHost, b7machine.Name, now)
	if changed {
		creds.consumerUID = string(b7machine.UID)
	}

	switch {
	case creds.consumer != "" && creds.consumer != b7machine.Name:
		logger.Info("Bootstrap credentials belong to an earlier claim of the host; minting fresh ones",
			"host", physicalHost.Name)
	case creds.consumer == b7machine.Name && creds.consumerUID != string(b7machine.UID):
		// Verification binds by name only, so it survives clusterctl move; a
		// machine recreated under the same name is still a new claim and must
		// not inherit credentials an earlier run may have exposed.
		logger.Info("Bootstrap credentials belong to an earlier machine of the same name; minting fresh ones",
			"host", physicalHost.Name)
		creds.consumer = ""
	}
	if !bootstrapTokenReusable(creds, b7machine.Name, now) {
		token, _, err := auth.MintToken()
		if err != nil {
			return fmt.Errorf("mint bootstrap token: %w", err)
		}
		issuedAt, expiresAt := auth.LifetimeFor(now)
		creds.token, creds.tokenIssuedAt, creds.tokenExpiresAt = token, issuedAt.Time, expiresAt.Time
		changed = true
	}
	if !bootNonceReusable(creds, physicalHost.Status.Bootstrap, b7machine.Name, now) {
		nonce, _, err := auth.MintToken()
		if err != nil {
			return fmt.Errorf("mint boot nonce: %w", err)
		}
		creds.nonce, creds.nonceExpiresAt = nonce, auth.NonceLifetimeFor(now).Time
		changed = true
	}
	if !changed {
		logger.V(1).Info("Bootstrap credentials still valid and bound to this machine; skipping mint", "host", physicalHost.Name)
		return nil
	}
	creds.consumer, creds.consumerUID = b7machine.Name, string(b7machine.UID)
	if err := r.writeBootstrapCredentials(ctx, logger, physicalHost, secret, creds); err != nil {
		return fmt.Errorf("store bootstrap credentials: %w", err)
	}
	return nil
}

// bootstrapTokenReusable reports whether the bearer token in creds may be kept
// for the Beskar7Machine named consumer: the Secret is bound to that machine
// and the token has an expiry that has not passed. Nothing on the
// PhysicalHost is consulted — its status only mirrors the Secret.
func bootstrapTokenReusable(creds bootstrapCredentials, consumer string, now time.Time) bool {
	return creds.consumer == consumer && creds.tokenValid(now)
}

// bootNonceReusable reports whether the boot nonce in creds may be kept for the
// Beskar7Machine named consumer: bound to it, unexpired, and not consumed. A
// consumed nonce is never reused (D-009); the consume record in bs counts only
// when it names this nonce's hash (bootNonceConsumed), since Status.Bootstrap
// outlives a claim and an earlier cycle's record must not spend the fresh
// nonce next to it. A record that names no hash might describe this nonce, so
// it counts as well (bootNonceConsumeUnattributed).
func bootNonceReusable(creds bootstrapCredentials, bs *infrav1.BootstrapStatus, consumer string, now time.Time) bool {
	return creds.consumer == consumer && creds.nonceValid(now) &&
		!bootNonceConsumed(bs, auth.Hash(creds.nonce)) &&
		!bootNonceConsumeUnattributed(bs)
}

// backfillBootstrapCredentials is the Inspecting/Deploying half of the D-029
// upgrade backfill; triggerInspection covers InUse through
// ensureBootstrapCredentials. It never mints: a run already past InUse keeps
// the credentials its inspector booted with, or has none that work.
//
// Removed in the next minor release, together with backfillLegacyCredentials.
func (r *Beskar7MachineReconciler) backfillBootstrapCredentials(
	ctx context.Context,
	logger logr.Logger,
	b7machine *infrav1.Beskar7Machine,
	physicalHost *infrav1.PhysicalHost,
) error {
	secret, err := r.getBootstrapTokenSecret(ctx, physicalHost)
	if err != nil || secret == nil {
		return err
	}
	creds := readBootstrapCredentials(secret)
	if !backfillLegacyCredentials(&creds, physicalHost, b7machine.Name, time.Now()) {
		return nil
	}
	creds.consumerUID = string(b7machine.UID)
	if err := r.writeBootstrapCredentials(ctx, logger, physicalHost, secret, creds); err != nil {
		return fmt.Errorf("backfill bootstrap credentials: %w", err)
	}
	return nil
}

// backfillLegacyCredentials binds a run that was in flight across the upgrade
// to D-029 to its machine, so its inspector keeps authenticating. Releases
// before D-029 kept only the plaintexts in the Secret; the hash and expiry
// lived in Status.Bootstrap, promoted there from an annotation.
//
// It applies only to a Secret without a consumer, on a host in InUse,
// Inspecting or Deploying whose claim names consumer. Each credential is kept
// only if its plaintext hashes to the hash status carries — a status hash that
// came from a forged annotation matches no plaintext in the Secret — and only
// with an expiry status carries, capped at a fresh lifetime from now. A host
// already Ready is left alone: no callback follows Ready, so its pre-upgrade
// credentials simply stop working. Reports whether it changed creds.
//
// Removed in the next minor release: by then no run can still be in flight
// from before D-029.
func backfillLegacyCredentials(creds *bootstrapCredentials, physicalHost *infrav1.PhysicalHost, consumer string, now time.Time) bool {
	if creds.consumer != "" {
		return false
	}
	switch physicalHost.Status.State {
	case infrav1.StateInUse, infrav1.StateInspecting, infrav1.StateDeploying:
	default:
		return false
	}
	if key, ok := resolveConsumerBeskar7Machine(physicalHost); !ok || key.Name != consumer {
		return false
	}
	bs := physicalHost.Status.Bootstrap
	if bs == nil {
		return false
	}
	backfilled := false
	if creds.token != "" && creds.tokenExpiresAt.IsZero() && bs.ExpiresAt != nil && auth.Verify(creds.token, bs.TokenHash) {
		creds.tokenExpiresAt = earliest(bs.ExpiresAt.Time, now.Add(auth.TokenLifetime))
		if bs.IssuedAt != nil {
			creds.tokenIssuedAt = bs.IssuedAt.Time
		}
		backfilled = true
	}
	if creds.nonce != "" && creds.nonceExpiresAt.IsZero() && bs.BootNonceExpiresAt != nil && auth.Verify(creds.nonce, bs.BootNonceHash) {
		creds.nonceExpiresAt = earliest(bs.BootNonceExpiresAt.Time, now.Add(auth.BootNonceLifetime))
		backfilled = true
	}
	if backfilled {
		creds.consumer = consumer
	}
	return backfilled
}

func earliest(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// getBootstrapTokenSecret returns the host's bootstrap-token Secret, or nil
// when it does not exist. Any other read error is returned so the reconcile
// retries rather than minting over a transient API failure.
func (r *Beskar7MachineReconciler) getBootstrapTokenSecret(ctx context.Context, physicalHost *infrav1.PhysicalHost) (*corev1.Secret, error) {
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: physicalHost.Namespace, Name: bootstrapTokenSecretName(physicalHost.Name)}
	if err := r.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get bootstrap-token Secret %s: %w", key.Name, err)
	}
	return secret, nil
}

// writeBootstrapCredentials stores creds in the host's bootstrap-token Secret
// in one write. existing is the Secret creds were computed from, or nil when
// there was none: the write updates it at the resourceVersion it was read at,
// or creates the Secret, so a write computed from a stale read fails instead
// of replacing a newer one. The Secret is owned by the PhysicalHost (GC'd with
// it, D-006). Neither plaintext is ever logged.
func (r *Beskar7MachineReconciler) writeBootstrapCredentials(
	ctx context.Context,
	logger logr.Logger,
	physicalHost *infrav1.PhysicalHost,
	existing *corev1.Secret,
	creds bootstrapCredentials,
) error {
	secret := existing
	if secret == nil {
		secret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      bootstrapTokenSecretName(physicalHost.Name),
				Namespace: physicalHost.Namespace,
			},
			Type: corev1.SecretTypeOpaque,
		}
	}
	if secret.Labels == nil {
		secret.Labels = map[string]string{}
	}
	secret.Labels[inspectionResultLabelOwnedBy] = "beskar7-controller-manager"
	secret.Labels[inspectionResultLabelHost] = physicalHost.Name
	if err := controllerutil.SetControllerReference(physicalHost, secret, r.Scheme); err != nil {
		return fmt.Errorf("set controller reference on bootstrap-token Secret: %w", err)
	}
	creds.writeTo(secret)
	if existing == nil {
		if err := r.Create(ctx, secret); err != nil {
			return err
		}
	} else if err := r.Update(ctx, secret); err != nil {
		return err
	}
	logger.Info("Stored bootstrap credentials", "host", physicalHost.Name, "secret", secret.Name, "consumer", creds.consumer)
	return nil
}

// handleInspectingHost monitors the inspection phase.
// On timeout it signals the PhysicalHost controller via annotation rather than
// writing to PhysicalHost.Status directly (BUG-1 fix).
//
// Evaluation order matters: terminal/complete states are checked before the
// timeout so that an inspection that actually completed is never spuriously
// marked InspectionTimedOut just because the reconcile fires after the timeout
// window.
func (r *Beskar7MachineReconciler) handleInspectingHost(ctx context.Context, logger logr.Logger, b7machine *infrav1.Beskar7Machine, physicalHost *infrav1.PhysicalHost) (ctrl.Result, error) {
	logger.Info("Monitoring inspection phase", "inspectionPhase", physicalHost.Status.InspectionPhase)

	// 1. Inspection succeeded — validate the report. The timeout must NOT fire
	// once a report is in: validateInspectionReport is bounded and idempotent.
	if physicalHost.Status.InspectionPhase == infrav1.InspectionPhaseComplete {
		logger.Info("Inspection complete, validating")
		return r.validateInspectionReport(ctx, logger, b7machine, physicalHost)
	}

	// 2. Inspection hard-failed on the host side (inspector booted but reported
	// an error). Terminal: operator must investigate the PhysicalHost status.
	if physicalHost.Status.InspectionPhase == infrav1.InspectionPhaseFailed {
		msg := fmt.Sprintf("PhysicalHost %s/%s reported inspection failure; inspect PhysicalHost status for details",
			physicalHost.Namespace, physicalHost.Name)
		logger.Info("Inspection failed (terminal)", "host", physicalHost.Name)
		r.markTerminalFailure(b7machine, infrav1.InspectionFailedReason, msg)
		return ctrl.Result{}, nil
	}

	// 3. A host that is powered off cannot be running the inspector, so waiting
	// out the full timeout only delays a failure that is already certain — and
	// the cause is usually recoverable. It happens when the power-on decision
	// at claim time was made from a reading taken while the previous consumer's
	// release shutdown was still in flight: the read returns On, the power-on
	// is skipped, and the host powers itself off moments later (measured on
	// bare metal: the reads said On at 08:44:46-47, the console printed
	// "reboot: Power down" at 08:44:49). Confirm over Redfish rather than
	// trusting the cached reading, then power it back on and let the timeout
	// below still apply if it never comes up.
	if physicalHost.Status.InspectionTimestamp != nil &&
		time.Since(physicalHost.Status.InspectionTimestamp.Time) > InspectionPowerRecheckDelay &&
		physicalHost.Status.ObservedPowerState == string(schemas.OffPowerState) {
		if err := r.ensureHostPoweredOnForInspection(ctx, logger, physicalHost); err != nil {
			// Non-fatal: the timeout below is the backstop, and a BMC that is
			// unreachable right now is not a reason to fail the machine here.
			logger.Error(err, "Failed to power the host back on during inspection")
		}
	}

	// 4. Still in progress — check for timeout. Inspection timeout is terminal:
	// we cannot recover automatically — the operator must investigate (likely an
	// iPXE misconfiguration or unreachable callback endpoint) and either
	// delete-and-recreate the Beskar7Machine or fix the iPXE setup.
	if physicalHost.Status.InspectionTimestamp != nil {
		timeout := r.inspectionTimeout()
		elapsed := time.Since(physicalHost.Status.InspectionTimestamp.Time)
		if elapsed > timeout {
			logger.Info("Inspection timed out (terminal)", "elapsed", elapsed, "timeout", timeout)
			// Best-effort signal to the PhysicalHost controller. Don't block the
			// terminal marking on the annotation patch — the PhysicalHost catches
			// up on its next reconcile regardless.
			if err := r.setInspectionRequestAnnotation(ctx, logger, physicalHost, "timeout"); err != nil {
				logger.Error(err, "Failed to set inspection timeout annotation")
			}
			msg := fmt.Sprintf("Inspection did not complete within %s", timeout)
			r.markTerminalFailure(b7machine, infrav1.InspectionTimedOutReason, msg)
			return ctrl.Result{}, nil
		}
	}

	phase := "Inspecting"
	b7machine.Status.Phase = &phase
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// ensureHostPoweredOnForInspection powers a host back on if it really is off
// while its inspection is supposed to be running.
//
// PhysicalHost.Status.ObservedPowerState is what makes the caller suspicious,
// but it is maintained by the other reconciler on its own cadence and can be
// stale, so this confirms over Redfish before acting. When the host is already
// on there is nothing to do and no call is made.
func (r *Beskar7MachineReconciler) ensureHostPoweredOnForInspection(ctx context.Context, logger logr.Logger, physicalHost *infrav1.PhysicalHost) error {
	rfClient, err := r.getRedfishClientForHost(ctx, logger, physicalHost)
	if err != nil {
		return err
	}
	defer rfClient.Close(ctx)

	state, err := rfClient.GetPowerState(ctx)
	if err != nil {
		return err
	}
	if state == schemas.OnPowerState {
		// The cached reading was stale; nothing to correct.
		return nil
	}

	if err := rfClient.SetPowerState(ctx, schemas.OnPowerState); err != nil {
		internalmetrics.RecordPhysicalHostPowerOperation(internalmetrics.PowerOperationOn, physicalHost.Namespace, internalmetrics.ProvisioningOutcomeFailed)
		return err
	}
	internalmetrics.RecordPhysicalHostPowerOperation(internalmetrics.PowerOperationOn, physicalHost.Namespace, internalmetrics.ProvisioningOutcomeSuccess)
	logger.Info("Host was powered off while its inspection was pending; powered it back on", "host", physicalHost.Name)
	return nil
}

// handleDeployingHost monitors the Deploying phase (D-015).
//
// The host is in StateDeploying when the inspector is writing the OS image to disk.
// We requeue every 30 s and enforce a deployment timeout (--deployment-timeout, default
// 20 m) measured from PhysicalHost.Status.DeployingTimestamp. A timeout is terminal:
// the operator must investigate (e.g. the image URL is unreachable, the disk failed).
//
// The transition to StateReady is driven by the provisioned callback handler setting
// ProvisionedRequestAnnotation, which the PhysicalHostReconciler consumes and applies.
// This function does NOT set StateReady — it only waits for it.
func (r *Beskar7MachineReconciler) handleDeployingHost(ctx context.Context, logger logr.Logger, b7machine *infrav1.Beskar7Machine, physicalHost *infrav1.PhysicalHost) (ctrl.Result, error) {
	logger.Info("Monitoring deployment phase", "deployingTimestamp", physicalHost.Status.DeployingTimestamp)

	// Enforce the deployment timeout measured from the DeployingTimestamp recorded by
	// the PhysicalHost controller when it processed the inspect-complete annotation.
	if physicalHost.Status.DeployingTimestamp != nil {
		timeout := r.deploymentTimeout()
		elapsed := time.Since(physicalHost.Status.DeployingTimestamp.Time)
		if elapsed > timeout {
			logger.Info("Deployment timed out (terminal)", "elapsed", elapsed, "timeout", timeout)
			msg := fmt.Sprintf("OS deployment did not complete within %s", timeout)
			r.markTerminalFailure(b7machine, DeploymentTimedOutReason, msg)
			return ctrl.Result{}, nil
		}
	}

	phase := "Provisioning"
	b7machine.Status.Phase = &phase
	setFalse(b7machine, infrav1.InfrastructureReadyCondition, infrav1.PhysicalHostNotReadyReason, "PhysicalHost %q is deploying OS image", physicalHost.Name)
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// validateInspectionReport validates the inspection report against requirements.
func (r *Beskar7MachineReconciler) validateInspectionReport(ctx context.Context, logger logr.Logger, b7machine *infrav1.Beskar7Machine, physicalHost *infrav1.PhysicalHost) (ctrl.Result, error) {
	logger.Info("Validating inspection report")

	if physicalHost.Status.InspectionReport == nil {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	report := physicalHost.Status.InspectionReport

	// Validate hardware requirements if specified
	if b7machine.Spec.HardwareRequirements != nil {
		reqs := b7machine.Spec.HardwareRequirements

		// Calculate total cores from all CPUs
		var totalCores int32
		for _, cpu := range report.CPUs {
			totalCores += cpu.Cores
		}

		// Hardware-validation failures are terminal. The BMC's hardware does not
		// change at runtime; requeueing forever would just churn. Operator must
		// lower the requirements, allocate to a different host, or replace the
		// hardware. The reason and message surface on the Machine's
		// InfrastructureReady condition in `kubectl describe machine`.
		if reqs.MinCPUCores > 0 && totalCores < reqs.MinCPUCores {
			msg := fmt.Sprintf("insufficient CPU cores: found %d, required %d", totalCores, reqs.MinCPUCores)
			logger.Info("Hardware validation failed (terminal)", "check", "MinCPUCores", "found", totalCores, "required", reqs.MinCPUCores)
			r.markTerminalFailure(b7machine, infrav1.HardwareRequirementsNotMetReason, msg)
			return ctrl.Result{}, nil
		}

		// Calculate total memory from all DIMMs
		var totalMemoryGB int32
		for _, mem := range report.Memory {
			memGB, err := parseMemoryCapacityGB(mem.Capacity)
			if err != nil {
				logger.Error(err, "Failed to parse memory capacity", "capacity", mem.Capacity)
				continue
			}
			totalMemoryGB += int32(memGB)
		}

		if reqs.MinMemoryGB > 0 && totalMemoryGB < reqs.MinMemoryGB {
			msg := fmt.Sprintf("insufficient memory: found %d GB, required %d GB", totalMemoryGB, reqs.MinMemoryGB)
			logger.Info("Hardware validation failed (terminal)", "check", "MinMemoryGB", "found", totalMemoryGB, "required", reqs.MinMemoryGB)
			r.markTerminalFailure(b7machine, infrav1.HardwareRequirementsNotMetReason, msg)
			return ctrl.Result{}, nil
		}

		if reqs.MinDiskGB > 0 {
			var totalDisk int32
			for _, disk := range report.Disks {
				totalDisk += disk.SizeGB
			}
			if totalDisk < reqs.MinDiskGB {
				msg := fmt.Sprintf("insufficient disk space: found %d GB, required %d GB", totalDisk, reqs.MinDiskGB)
				logger.Info("Hardware validation failed (terminal)", "check", "MinDiskGB", "found", totalDisk, "required", reqs.MinDiskGB)
				r.markTerminalFailure(b7machine, infrav1.HardwareRequirementsNotMetReason, msg)
				return ctrl.Result{}, nil
			}
		}
	}

	logger.Info("Hardware validation passed")

	// Mark HostInspectedCondition on PhysicalHost via a spec annotation signal.
	// PhysicalHost owns its own status; we must not call r.Status().Update on it here (BUG-1 fix).
	// The "inspect-complete" value tells the PhysicalHost controller to set StateReady and
	// MarkTrue(HostInspectedCondition) on its next reconcile.
	if err := r.setInspectionRequestAnnotation(ctx, logger, physicalHost, "inspect-complete"); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: requeueShortly}, nil
}

// handleReadyHost handles a host that's ready after inspection.
func (r *Beskar7MachineReconciler) handleReadyHost(ctx context.Context, logger logr.Logger, b7machine *infrav1.Beskar7Machine, physicalHost *infrav1.PhysicalHost) (ctrl.Result, error) {
	logger.Info("Host ready, marking infrastructure as ready")

	// Set ProviderID
	currentProviderID := providerID(physicalHost.Namespace, physicalHost.Name)
	if b7machine.Spec.ProviderID != currentProviderID {
		logger.Info("Setting ProviderID", "ProviderID", currentProviderID)
		b7machine.Spec.ProviderID = currentProviderID
	}

	// Copy addresses from PhysicalHost
	if len(physicalHost.Status.Addresses) > 0 {
		b7machine.Status.Addresses = physicalHost.Status.Addresses
		logger.Info("Copied network addresses", "count", len(physicalHost.Status.Addresses))
	}

	// Record provisioning success only on the first transition to ready. We check
	// Status.Ready before updating it so we don't double-record on re-reconciles
	// that reach handleReadyHost after the machine is already provisioned.
	firstProvisioning := !b7machine.Status.Ready

	// Mark as ready
	setTrue(b7machine, infrav1.InfrastructureReadyCondition, infrav1.ProvisionedReason)
	b7machine.Status.Ready = true
	// CAPI v1beta2 contract: surface to Machine.status.initialization.infrastructureProvisioned.
	// Without this, CAPI v1.10+ does not advance the Machine past Pending and
	// the parent Cluster never reaches Available.
	b7machine.Status.Initialization.Provisioned = ptr.To(true)
	phase := "Provisioned"
	b7machine.Status.Phase = &phase

	if firstProvisioning {
		// Clear the PXE boot-source override so the host boots from disk on the
		// next power-on (D-015 / issue #2). The inspector already rebooted into
		// the provisioned OS; this is belt-and-suspenders for firmwares that don't
		// consume Once correctly. Best-effort: log on error, don't fail the reconcile.
		rfClient, rfErr := r.getRedfishClientForHost(ctx, logger, physicalHost)
		if rfErr != nil {
			logger.Info("Could not get Redfish client to clear boot override; skipping", "err", rfErr)
		} else {
			if err := rfClient.ClearBootSourceOverride(ctx); err != nil {
				logger.Info("Failed to clear boot source override on first provisioning; continuing", "err", err)
			} else {
				logger.V(1).Info("Cleared boot source override after first provisioning")
			}
			rfClient.Close(ctx)
		}

		internalmetrics.RecordBeskar7MachineProvisioning(
			b7machine.Namespace,
			internalmetrics.ProvisioningOutcomeSuccess,
			time.Since(b7machine.CreationTimestamp.Time),
		)
	}

	logger.Info("Beskar7Machine infrastructure is ready")
	return ctrl.Result{}, nil
}

// errInvalidHostSelector marks a hostSelector that cannot be parsed. That is a
// property of the spec, not of the cluster, so the caller treats it as terminal.
var errInvalidHostSelector = errors.New("invalid hostSelector")

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

	// (1) ProviderID lookup.
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

	// (2) ConsumerRef lookup: find any host in our namespace already claimed by
	// this Beskar7Machine. List all hosts in the namespace (no field index for
	// Spec.ConsumerRef — it's a nested pointer; an index would not save much
	// because the host count per namespace is bounded by physical inventory).
	allHosts := &infrav1.PhysicalHostList{}
	if err := r.List(ctx, allHosts, client.InNamespace(b7machine.Namespace)); err != nil {
		internalmetrics.RecordHostClaimAttempt(b7machine.Namespace, internalmetrics.ClaimOutcomeError, internalmetrics.ConflictReasonNone)
		internalmetrics.RecordHostClaimDuration(b7machine.Namespace, internalmetrics.ClaimOutcomeError, time.Since(claimStart))
		return nil, ctrl.Result{}, err
	}
	for i := range allHosts.Items {
		h := &allHosts.Items[i]
		// resolveConsumerBeskar7Machine also rejects a ConsumerRef naming a
		// different namespace than h's own (SEC-12): a host claimed "by" a
		// same-named machine in another namespace must not re-find as ours.
		if key, ok := resolveConsumerBeskar7Machine(h); ok && key == client.ObjectKeyFromObject(b7machine) {
			internalmetrics.RecordHostClaimAttempt(b7machine.Namespace, internalmetrics.ClaimOutcomeSuccess, internalmetrics.ConflictReasonNone)
			internalmetrics.RecordHostClaimDuration(b7machine.Namespace, internalmetrics.ClaimOutcomeSuccess, time.Since(claimStart))
			return h, ctrl.Result{}, nil
		}
	}

	// (3) No existing claim — list StateAvailable hosts and try to claim one.
	// The field index filters server-side to StateAvailable so the result set
	// is bounded by the count of free hosts.
	hostList := &infrav1.PhysicalHostList{}
	listOpts := []client.ListOption{
		client.InNamespace(b7machine.Namespace),
		client.MatchingFields{PhysicalHostStateIndex: string(infrav1.StateAvailable)},
	}
	if placement != nil && !placement.Empty() {
		// Applied together with the field index: the cache resolves the index
		// first and filters labels in memory, so this stays bounded by the
		// number of free hosts.
		listOpts = append(listOpts, client.MatchingLabelsSelector{Selector: placement})
		logger.V(1).Info("Restricting the host claim to a placement constraint", "placement", placement.String())
	}
	if err := r.List(ctx, hostList, listOpts...); err != nil {
		internalmetrics.RecordHostClaimAttempt(b7machine.Namespace, internalmetrics.ClaimOutcomeError, internalmetrics.ConflictReasonNone)
		internalmetrics.RecordHostClaimDuration(b7machine.Namespace, internalmetrics.ClaimOutcomeError, time.Since(claimStart))
		return nil, ctrl.Result{}, err
	}

	for i := range hostList.Items {
		host := &hostList.Items[i]
		// ConsumerRef is on Spec (not indexed); keep the in-loop guard defensively.
		// A host can be Available in the index but have a stale ConsumerRef that has
		// not yet been cleared, so this check prevents a double-claim.
		if host.Spec.ConsumerRef == nil {
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

// reconcileDelete handles deletion. The sequence is:
//  1. Locate the PhysicalHost this machine claimed (by ConsumerRef ownership).
//  2. Best-effort Redfish cleanup (ClearBootSourceOverride + graceful power-off).
//  3. Patch ConsumerRef = nil on the PhysicalHost spec.
//  4. Remove the Beskar7Machine finalizer.
//
// Release keys off ConsumerRef ownership, NOT ProviderID. ProviderID is only set
// once inspection completes (handleReadyHost), but ConsumerRef is set much earlier
// at claim time, so a machine deleted mid-inspection has a claimed host with no
// ProviderID. Keying release off ProviderID would skip the release entirely in
// that window and strand the host in InUse with a dangling ConsumerRef (#107).
//
// Redfish cleanup is skipped when the ForceReleaseAnnotation is "true" (BMC
// permanently unreachable) or when the credentials Secret no longer exists.
// Neither case strands the finalizer: errors from Redfish are logged and
// swallowed so that a dead BMC cannot block object deletion.
func (r *Beskar7MachineReconciler) reconcileDelete(ctx context.Context, logger logr.Logger, b7machine *infrav1.Beskar7Machine) (ctrl.Result, error) {
	logger.Info("Reconciling deletion")

	host, err := r.findClaimedHostForRelease(ctx, logger, b7machine)
	if err != nil {
		logger.Error(err, "Failed to look up claimed host during deletion")
		return ctrl.Result{}, err
	}
	if host != nil {
		if _, movingAway := b7machine.Annotations[clusterctlDeleteForMoveAnnotation]; movingAway {
			// The source side of a clusterctl move: this Beskar7Machine's
			// deletion is only clearing the way for the pair now live on the
			// target, which claims the same host with the same ProviderID
			// (D-028). Powering it off or releasing the claim here would
			// stop a node that must keep serving, for no benefit — the
			// source object is going away either way.
			logger.Info("Beskar7Machine carries the clusterctl delete-for-move annotation; leaving its PhysicalHost claimed and powered as-is", "host", host.Name)
		} else {
			forceRelease := b7machine.Annotations[ForceReleaseAnnotation] == "true"
			if forceRelease {
				logger.Info("ForceReleaseAnnotation set; skipping Redfish power-off and boot-override clear")
			} else {
				// Best-effort Redfish cleanup. Errors are logged but do not block release.
				r.bestEffortReleaseRedfish(ctx, logger, host)
			}
			// Always clear ConsumerRef on the host spec.
			base := host.DeepCopy()
			host.Spec.ConsumerRef = nil
			if err := r.Patch(ctx, host, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				if apierrors.IsConflict(err) {
					return ctrl.Result{RequeueAfter: requeueShortly}, nil
				}
				logger.Error(err, "Failed to release host")
				return ctrl.Result{}, err
			}
			logger.Info("Released PhysicalHost", "host", host.Name)
		}
	}

	if controllerutil.RemoveFinalizer(b7machine, Beskar7MachineFinalizer) {
		logger.Info("Removing finalizer")
	}
	return ctrl.Result{}, nil
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
				// ProviderID can name a host in any namespace; only a host whose
				// consumer resolves to this very machine, namespace included, is
				// ours to release (SEC-12).
				if key, ok := resolveConsumerBeskar7Machine(host); ok && key == client.ObjectKeyFromObject(b7machine) {
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

	// Fallback: scan the namespace for any host whose ConsumerRef names us. This
	// covers the claimed-but-not-yet-provisioned window where ProviderID is unset.
	allHosts := &infrav1.PhysicalHostList{}
	if err := r.List(ctx, allHosts, client.InNamespace(b7machine.Namespace)); err != nil {
		return nil, err
	}
	for i := range allHosts.Items {
		h := &allHosts.Items[i]
		// resolveConsumerBeskar7Machine also rejects a ConsumerRef naming a
		// different namespace than h's own (SEC-12): a host claimed "by" a
		// same-named machine in another namespace must not be released by us.
		if key, ok := resolveConsumerBeskar7Machine(h); ok && key == client.ObjectKeyFromObject(b7machine) {
			return h, nil
		}
	}
	return nil, nil
}

// bestEffortReleaseRedfish issues ClearBootSourceOverride and a graceful power-off
// against the host's BMC. All errors are logged at Info and swallowed so a
// dead BMC cannot strand the Beskar7Machine finalizer.
// Missing credentials are treated identically — log a warning and return.
func (r *Beskar7MachineReconciler) bestEffortReleaseRedfish(ctx context.Context, logger logr.Logger, host *infrav1.PhysicalHost) {
	rfClient, err := r.getRedfishClientForHost(ctx, logger, host)
	if err != nil {
		logger.Info("Could not get Redfish client during release; skipping power-off and boot clear", "err", err)
		return
	}
	defer rfClient.Close(ctx)

	if err := rfClient.ClearBootSourceOverride(ctx); err != nil {
		logger.Info("Failed to clear boot source override during release; continuing", "err", err)
	}
	if err := rfClient.SetPowerState(ctx, schemas.OffPowerState); err != nil {
		logger.Info("Failed to graceful power-off during release; continuing", "err", err)
		internalmetrics.RecordPhysicalHostPowerOperation(internalmetrics.PowerOperationOff, host.Namespace, internalmetrics.ProvisioningOutcomeFailed)
	} else {
		internalmetrics.RecordPhysicalHostPowerOperation(internalmetrics.PowerOperationOff, host.Namespace, internalmetrics.ProvisioningOutcomeSuccess)
	}
}

// markTerminalFailure flips Status.Ready to false and Phase to Failed and
// marks InfrastructureReady=False with the terminal reason and message; the
// Ready summary follows, and Cluster API mirrors it into the owning Machine's
// InfrastructureReady condition, which is what a MachineHealthCheck's
// unhealthyMachineConditions can key on (the v1beta2 contract has no
// failureReason/failureMessage: terminal failures are conditions).
//
// Phase=Failed is the terminal marker: Reconcile returns early on it, so a
// failed machine is never healed by a later state change and the history
// stays visible. Callers return ctrl.Result{}, nil after this helper.
func (r *Beskar7MachineReconciler) markTerminalFailure(b7machine *infrav1.Beskar7Machine, reason, message string) {
	// Only record the provisioning-failed metric on the first terminal transition
	// to avoid double-counting on re-reconciles that call markTerminalFailure again.
	if !isTerminallyFailed(b7machine) {
		internalmetrics.RecordBeskar7MachineProvisioning(
			b7machine.Namespace,
			internalmetrics.ProvisioningOutcomeFailed,
			time.Since(b7machine.CreationTimestamp.Time),
		)
	}
	b7machine.Status.Ready = false
	b7machine.Status.Phase = ptr.To(infrav1.PhaseFailed)
	setFalse(b7machine, infrav1.InfrastructureReadyCondition, reason, "%s", message)
}

// setInspectionRequestAnnotation patches only the annotations of a PhysicalHost to request
// a state transition. The PhysicalHost controller reads the annotation on its next reconcile
// and drives the Status transition, preserving status ownership (BUG-1 fix, Pattern A).
// Uses optimistic locking via MergeFromWithOptions so a conflict causes a fast requeue.
func (r *Beskar7MachineReconciler) setInspectionRequestAnnotation(ctx context.Context, logger logr.Logger, physicalHost *infrav1.PhysicalHost, value string) error {
	base := physicalHost.DeepCopy()
	if physicalHost.Annotations == nil {
		physicalHost.Annotations = make(map[string]string)
	}
	physicalHost.Annotations[InspectionRequestAnnotation] = value
	if err := r.Patch(ctx, physicalHost, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("failed to set inspection-request annotation %q on PhysicalHost %s: %w", value, physicalHost.Name, err)
	}
	logger.V(1).Info("Set inspection-request annotation", "host", physicalHost.Name, "value", value)
	return nil
}

// ensureBootstrapDataReady verifies that the bootstrap Secret named by
// Machine.Spec.Bootstrap.DataSecretName exists, then signals the computed per-host
// bootstrap URL to PhysicalHost via an annotation. Returns a non-zero ctrl.Result to
// short-circuit the reconcile when bootstrap data is not yet available.
//
// We verify the Secret exists but do not read its bytes — the server-side bootstrap
// endpoint (PR-5.3) fetches the bytes at request time, which correctly handles
// secret rotation between claim and boot.
func (r *Beskar7MachineReconciler) ensureBootstrapDataReady(
	ctx context.Context,
	logger logr.Logger,
	b7machine *infrav1.Beskar7Machine,
	machine *clusterv1.Machine,
	physicalHost *infrav1.PhysicalHost,
) (ctrl.Result, error) {
	if machine.Spec.Bootstrap.DataSecretName == nil {
		logger.Info("Waiting for bootstrap data secret name to be set on Machine.Spec.Bootstrap")
		setFalse(b7machine, infrav1.BootstrapDataReadyCondition, infrav1.WaitingForBootstrapDataReason, "Waiting for Machine.Spec.Bootstrap.DataSecretName to be set")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	secretName := *machine.Spec.Bootstrap.DataSecretName
	secret := &corev1.Secret{}
	secretKey := types.NamespacedName{Namespace: b7machine.Namespace, Name: secretName}
	if err := r.Get(ctx, secretKey, secret); err != nil {
		if apierrors.IsNotFound(err) {
			// Terminal: the bootstrap provider set a secret name that doesn't exist.
			msg := fmt.Sprintf("bootstrap data secret %q not found in namespace %q", secretName, b7machine.Namespace)
			logger.Error(err, msg)
			setFalse(b7machine, infrav1.BootstrapDataReadyCondition, infrav1.BootstrapDataUnavailableReason, "%s", msg)
			r.markTerminalFailure(b7machine, infrav1.BootstrapDataUnavailableReason, msg)
			// Don't requeue — this is terminal until the operator resolves it.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get bootstrap data secret %q: %w", secretName, err)
	}

	// Compute the deterministic per-host bootstrap URL and signal it to the
	// PhysicalHost controller via an annotation if it hasn't been set yet.
	bootstrapURL := fmt.Sprintf("%s/api/v1/bootstrap/%s/%s",
		strings.TrimRight(r.BootstrapURLBase, "/"),
		physicalHost.Namespace, physicalHost.Name)

	if physicalHost.Status.Bootstrap == nil || physicalHost.Status.Bootstrap.URL != bootstrapURL {
		if err := r.setBootstrapURLAnnotation(ctx, logger, physicalHost, bootstrapURL); err != nil {
			return ctrl.Result{}, err
		}
	}

	setTrue(b7machine, infrav1.BootstrapDataReadyCondition, infrav1.BootstrapDataReadyReason)
	return ctrl.Result{}, nil
}

// setBootstrapURLAnnotation patches PhysicalHost metadata annotations with the
// per-host bootstrap URL. The PhysicalHost controller consumes this annotation
// and writes the value to Status.Bootstrap.URL, then clears the annotation.
// We patch only spec/annotations here — never status — preserving status ownership.
func (r *Beskar7MachineReconciler) setBootstrapURLAnnotation(
	ctx context.Context,
	logger logr.Logger,
	physicalHost *infrav1.PhysicalHost,
	url string,
) error {
	base := physicalHost.DeepCopy()
	if physicalHost.Annotations == nil {
		physicalHost.Annotations = make(map[string]string)
	}
	physicalHost.Annotations[BootstrapURLAnnotation] = url
	if err := r.Patch(ctx, physicalHost, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("failed to set bootstrap-url annotation on PhysicalHost %s: %w", physicalHost.Name, err)
	}
	logger.V(1).Info("Set bootstrap-url annotation", "host", physicalHost.Name)
	return nil
}

// getRedfishClientForHost creates a Redfish client for the given PhysicalHost.
func (r *Beskar7MachineReconciler) getRedfishClientForHost(ctx context.Context, logger logr.Logger, host *infrav1.PhysicalHost) (internalredfish.Client, error) {
	// Get credentials
	secret := &corev1.Secret{}
	secretKey := types.NamespacedName{
		Namespace: host.Namespace,
		Name:      host.Spec.RedfishConnection.CredentialsSecretRef,
	}
	if err := r.Get(ctx, secretKey, secret); err != nil {
		return nil, errors.Wrap(err, "failed to get credentials secret")
	}

	username := string(secret.Data["username"])
	password := string(secret.Data["password"])

	insecure := false
	if host.Spec.RedfishConnection.InsecureSkipVerify != nil {
		insecure = *host.Spec.RedfishConnection.InsecureSkipVerify
	}

	// Reject the conflicting combination at this layer too. The PhysicalHost
	// reconciler is the canonical gate (it sets the InsecureCABundleConflict
	// condition), but Beskar7Machine consumes the same Spec, so we must not
	// silently produce a working client here while PhysicalHost is in error.
	if err := validateRedfishTLSCombination(insecure, host.Spec.RedfishConnection.CABundleSecretRef); err != nil {
		return nil, err
	}

	caBundle, err := fetchRedfishCABundle(ctx, r.Client, host)
	if err != nil {
		return nil, errors.Wrap(err, "failed to fetch CA bundle")
	}

	return r.RedfishClientFactory(ctx, host.Spec.RedfishConnection.Address, username, password, insecure, caBundle)
}

// Helper functions
func providerID(namespace, name string) string {
	return fmt.Sprintf("%s%s/%s", ProviderIDPrefix, namespace, name)
}

func parseProviderID(id string) (string, string, error) {
	rest, ok := strings.CutPrefix(id, ProviderIDPrefix)
	if !ok {
		return "", "", fmt.Errorf("invalid provider ID %q: missing %q prefix", id, ProviderIDPrefix)
	}
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid provider ID %q: expected %s<namespace>/<name>", id, ProviderIDPrefix)
	}
	if strings.Contains(parts[1], "/") {
		return "", "", fmt.Errorf("invalid provider ID %q: name segment contains '/'", id)
	}
	return parts[0], parts[1], nil
}

// recomputeBeskar7MachineMetrics lists all Beskar7Machines in the given namespace
// and emits the phase-gauge metric. Called at the top of each Reconcile so the
// gauge stays current even when reconcile short-circuits. Errors are logged and
// swallowed — a metric failure must not affect reconcile correctness.
func (r *Beskar7MachineReconciler) recomputeBeskar7MachineMetrics(ctx context.Context, logger logr.Logger, namespace string) {
	list := &infrav1.Beskar7MachineList{}
	if err := r.List(ctx, list, client.InNamespace(namespace)); err != nil {
		logger.V(1).Info("Failed to list Beskar7Machines for metric recompute; skipping", "err", err.Error())
		return
	}
	counts := make(map[string]int, len(list.Items))
	for _, m := range list.Items {
		phase := ""
		if m.Status.Phase != nil {
			phase = *m.Status.Phase
		}
		counts[phase]++
	}
	internalmetrics.UpdateBeskar7MachineStateCounts(namespace, counts)
}

// defaultFactory sets RedfishClientFactory to the real gofish-backed constructor
// when the caller left it nil. Separated from SetupWithManager to keep the defaulting
// logic directly testable without spinning up a full controller-runtime Manager.
func (r *Beskar7MachineReconciler) defaultFactory() error {
	if r.RedfishClientFactory == nil {
		r.RedfishClientFactory = internalredfish.NewClient
	}
	if r.RedfishClientFactory == nil {
		// Should be unreachable, but guards against a future programming error
		// that nilifies the field after this call.
		return fmt.Errorf("Beskar7MachineReconciler: RedfishClientFactory is nil after defaulting")
	}
	return nil
}

// validateAndDefault defaults the RedfishClientFactory and validates that
// BootstrapURLBase is non-empty. Both are required for normal operation;
// failing fast at setup time prevents the first reconcile from panicking or
// producing a confusing error.
func (r *Beskar7MachineReconciler) validateAndDefault() error {
	if err := r.defaultFactory(); err != nil {
		return err
	}
	if r.BootstrapURLBase == "" {
		return fmt.Errorf("Beskar7MachineReconciler: BootstrapURLBase is empty; set --bootstrap-url-base")
	}
	return nil
}

// SetupWithManager sets up the controller.
func (r *Beskar7MachineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := r.validateAndDefault(); err != nil {
		return err
	}
	// Register a cache field index on PhysicalHost.Status.State so that
	// findAndClaimOrGetAssociatedHost can filter Available hosts server-side
	// instead of listing all hosts in the namespace and filtering in Go.
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&infrav1.PhysicalHost{},
		PhysicalHostStateIndex,
		func(obj client.Object) []string {
			host, ok := obj.(*infrav1.PhysicalHost)
			if !ok {
				return nil
			}
			return []string{string(host.Status.State)}
		},
	); err != nil {
		return fmt.Errorf("failed to index PhysicalHost.status.state: %w", err)
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1.Beskar7Machine{}).
		Watches(
			&infrav1.PhysicalHost{},
			handler.EnqueueRequestsFromMapFunc(r.PhysicalHostToBeskar7Machine),
		).
		Watches(
			&infrav1.PhysicalHost{},
			handler.EnqueueRequestsFromMapFunc(r.AvailablePhysicalHostToWaitingBeskar7Machines),
			builder.WithPredicates(hostBecameAvailable()),
		).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: maxConcurrentOrDefault(r.MaxConcurrentReconciles),
		}).
		Complete(r)
}

// PhysicalHostToBeskar7Machine maps a PhysicalHost change to a reconcile
// request for the Beskar7Machine that currently consumes it. PhysicalHosts
// without a ConsumerRef, or with a ConsumerRef pointing at a non-Beskar7Machine
// kind, produce no requests.
func (r *Beskar7MachineReconciler) PhysicalHostToBeskar7Machine(ctx context.Context, obj client.Object) []reconcile.Request {
	host, ok := obj.(*infrav1.PhysicalHost)
	if !ok {
		r.Log.Error(nil, "Expected a PhysicalHost in PhysicalHostToBeskar7Machine map", "object", obj)
		return nil
	}
	cr := host.Spec.ConsumerRef
	if cr == nil {
		return nil
	}
	if cr.Kind != "Beskar7Machine" || cr.APIVersion != InfrastructureAPIVersion {
		return nil
	}
	return []reconcile.Request{
		{NamespacedName: types.NamespacedName{Namespace: cr.Namespace, Name: cr.Name}},
	}
}

// hostBecameAvailable admits exactly the PhysicalHost events on which an
// unclaimed host enters Available: creation already in that state, and the
// transition into it (enrolment, release by a deleted machine). Status churn
// on a host that is already free does not pass, so waiting machines are not
// re-enqueued on every host resync.
func hostBecameAvailable() predicate.Funcs {
	free := func(o client.Object) bool {
		h, ok := o.(*infrav1.PhysicalHost)
		return ok && h.Status.State == infrav1.StateAvailable && h.Spec.ConsumerRef == nil
	}
	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return free(e.Object) },
		UpdateFunc:  func(e event.UpdateEvent) bool { return free(e.ObjectNew) && !free(e.ObjectOld) },
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

// AvailablePhysicalHostToWaitingBeskar7Machines maps a PhysicalHost that has
// just become claimable to every Beskar7Machine in its namespace that is
// still waiting for one. The no-host path requeues after a minute as a
// backstop, and a machine that reconciled moments before its host finished
// enrolling would otherwise sit out that whole minute: nothing else
// re-enqueues it, and controller-runtime's priority queue collapses a
// pending delayed requeue into any earlier event-driven reconcile.
func (r *Beskar7MachineReconciler) AvailablePhysicalHostToWaitingBeskar7Machines(ctx context.Context, obj client.Object) []reconcile.Request {
	host, ok := obj.(*infrav1.PhysicalHost)
	if !ok {
		r.Log.Error(nil, "Expected a PhysicalHost in AvailablePhysicalHostToWaitingBeskar7Machines map", "object", obj)
		return nil
	}
	if host.Status.State != infrav1.StateAvailable || host.Spec.ConsumerRef != nil {
		return nil
	}
	machines := &infrav1.Beskar7MachineList{}
	if err := r.List(ctx, machines, client.InNamespace(host.Namespace)); err != nil {
		r.Log.Error(err, "Failed to list Beskar7Machines waiting for a PhysicalHost", "namespace", host.Namespace)
		return nil
	}
	var requests []reconcile.Request
	for i := range machines.Items {
		m := &machines.Items[i]
		if !m.DeletionTimestamp.IsZero() || isTerminallyFailed(m) ||
			conditions.IsTrue(m, infrav1.PhysicalHostAssociatedCondition) {
			continue
		}
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(m)})
	}
	return requests
}

// parseMemoryCapacityGB converts a BMC-reported capacity string to whole decimal gigabytes.
//
// Convention: IEC binary suffixes (GiB, MiB, TiB) are treated as powers of 1024; SI suffixes
// (GB, MB, TB) are treated as powers of 1000. Both are converted to decimal GB (÷1e9) so that
// the resulting integer aligns with the operator-supplied MinMemoryGB threshold (which users
// express in round decimal numbers, e.g. "32 GB" on a datasheet). Fractional GB is truncated.
//
// Accepts: "32GB", "32 GB", "32GiB", "32 GiB", "32768MB", "32768 MiB", "1TB", "1TiB".
// Rejects: bare numbers without unit ("32"), unknown/unsupported units, empty string.
//
// Implementation: strip a trailing 'B' so that "32GB"→"32G" and "32GiB"→"32Gi", validate
// the resulting suffix against an explicit allowlist, then hand the normalised string to
// resource.ParseQuantity, which handles both SI (G=1e9) and IEC (Gi=2^30) correctly.
// The trailing-B strip and allowlist are both necessary: Kubernetes Quantity syntax uses G/Gi
// (not GB/GiB), and resource.ParseQuantity accepts any prefix letter (e.g. 'P' for peta)
// which we do not want to silently accept.
func parseMemoryCapacityGB(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty capacity string")
	}

	// Strip internal spaces (e.g. "32 GB" → "32GB"), then normalise the unit suffix.
	noSpace := strings.ReplaceAll(s, " ", "")

	// Strip trailing 'B' so "32GB"→"32G", "32GiB"→"32Gi", "32MB"→"32M", "32MiB"→"32Mi".
	// Only strip when the character before 'B' is also a letter — this avoids turning a
	// hypothetical bare "32B" (32 bytes) into "32", which resource.ParseQuantity would
	// accept as 32 bytes instead of returning an error.
	quantityStr := noSpace
	if len(quantityStr) >= 2 && quantityStr[len(quantityStr)-1] == 'B' {
		prev := quantityStr[len(quantityStr)-2]
		if (prev >= 'A' && prev <= 'Z') || (prev >= 'a' && prev <= 'z') {
			quantityStr = quantityStr[:len(quantityStr)-1]
		}
	}

	// Validate the suffix against an explicit allowlist. This prevents:
	//  - bare integers ("32" → resource.ParseQuantity returns 32 bytes silently)
	//  - unsupported SI prefixes ("32P" = peta, accepted by resource.ParseQuantity)
	// Allowed suffixes after trailing-B strip: G, Gi, M, Mi, T, Ti.
	allowedSuffixes := []string{"Gi", "Mi", "Ti", "G", "M", "T"}
	suffixOK := false
	for _, sfx := range allowedSuffixes {
		if strings.HasSuffix(quantityStr, sfx) {
			suffixOK = true
			break
		}
	}
	if !suffixOK {
		return 0, fmt.Errorf("capacity %q has unsupported unit suffix; expected GB, GiB, MB, MiB, TB, or TiB", s)
	}

	q, err := resource.ParseQuantity(quantityStr)
	if err != nil {
		return 0, fmt.Errorf("cannot parse memory capacity %q: %w", s, err)
	}

	bytes := q.Value() // exact int64 bytes (SI G = 1e9, IEC Gi = 2^30)
	if bytes <= 0 {
		return 0, fmt.Errorf("memory capacity %q parsed to non-positive value %d", s, bytes)
	}

	return int(bytes / 1_000_000_000), nil
}

// isTerminallyFailed reports whether markTerminalFailure has run: Phase=Failed
// is the marker (InfrastructureReady=False alone also covers a machine that is
// merely not provisioned yet).
func isTerminallyFailed(b7machine *infrav1.Beskar7Machine) bool {
	return ptr.Deref(b7machine.Status.Phase, "") == infrav1.PhaseFailed
}
