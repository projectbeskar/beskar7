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
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	infrastructurev1beta1 "github.com/projectbeskar/beskar7/api/v1beta1"
	"github.com/projectbeskar/beskar7/internal/auth"
	internalmetrics "github.com/projectbeskar/beskar7/internal/metrics"
	internalredfish "github.com/projectbeskar/beskar7/internal/redfish"
	"github.com/stmcginnis/gofish/redfish"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	// Beskar7MachineFinalizer allows cleanup before removal
	Beskar7MachineFinalizer = "beskar7machine.infrastructure.cluster.x-k8s.io"

	// ProviderIDPrefix is the prefix used for ProviderID
	ProviderIDPrefix = "b7://"

	// InfrastructureAPIVersion for owner references
	InfrastructureAPIVersion = "infrastructure.cluster.x-k8s.io/v1beta1"

	// Inspection timeout
	DefaultInspectionTimeout = 10 * time.Minute

	// ForceReleaseAnnotation, when set to "true" on a Beskar7Machine being deleted,
	// causes the controller to skip the Redfish power-off and boot-override clear
	// during deletion. Use only when the BMC is permanently unreachable.
	ForceReleaseAnnotation = "infrastructure.cluster.x-k8s.io/force-release"

	// PhysicalHostStateIndex is the cache field index key for PhysicalHost.Status.State.
	// Registered in SetupWithManager; used in findAndClaimOrGetAssociatedHost to filter
	// Available hosts server-side instead of listing all hosts and filtering in Go.
	PhysicalHostStateIndex = "status.state"
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
	// InspectionTimeout bounds how long a host may stay in Inspecting before the
	// machine is marked terminally failed (InspectionTimedOut). Zero means use
	// DefaultInspectionTimeout. Set from the --inspection-timeout manager flag so
	// operators with slow-POST hardware can raise it.
	InspectionTimeout time.Duration
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
//   - upsertBootstrapTokenSecret: CreateOrUpdate on the per-host bootstrap
//     token Secret (deterministic name; PhysicalHost-owned).
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
	b7machine := &infrastructurev1beta1.Beskar7Machine{}
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

	// Check if paused
	if isPaused(b7machine) {
		log.Info("Beskar7Machine reconciliation is paused")
		return ctrl.Result{}, nil
	}

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

	// Check if cluster is paused
	if isClusterPaused(cluster) {
		log.Info("Reconciliation paused because owner cluster is paused")
		return ctrl.Result{}, nil
	}

	// Initialize patch helper
	patchHelper, err := patch.NewHelper(b7machine, r.Client)
	if err != nil {
		log.Error(err, "Failed to init patch helper")
		return ctrl.Result{}, err
	}

	// Always patch on exit
	defer func() {
		conditions.SetSummary(b7machine, conditions.WithConditions(infrastructurev1beta1.InfrastructureReadyCondition))
		if err := patchHelper.Patch(ctx, b7machine); err != nil {
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

	// Handle normal reconciliation
	return r.reconcileNormal(ctx, log, b7machine, machine)
}

// reconcileNormal handles normal (non-deletion) reconciliation.
func (r *Beskar7MachineReconciler) reconcileNormal(ctx context.Context, logger logr.Logger, b7machine *infrastructurev1beta1.Beskar7Machine, machine *clusterv1.Machine) (ctrl.Result, error) {
	logger.Info("Reconciling Beskar7Machine create/update")

	// Add finalizer
	if controllerutil.AddFinalizer(b7machine, Beskar7MachineFinalizer) {
		logger.Info("Adding finalizer")
		return ctrl.Result{Requeue: true}, nil
	}

	// Find or get associated host
	physicalHost, result, err := r.findAndClaimOrGetAssociatedHost(ctx, logger, b7machine)
	if err != nil {
		logger.Error(err, "Failed to find, claim, or get associated PhysicalHost")
		conditions.MarkFalse(b7machine, infrastructurev1beta1.PhysicalHostAssociatedCondition,
			infrastructurev1beta1.PhysicalHostAssociationFailedReason, clusterv1.ConditionSeverityWarning,
			"Failed to associate with PhysicalHost: %v", err)
		internalmetrics.RecordError("beskar7machine", b7machine.Namespace, internalmetrics.ErrorTypeTransient)
		return result, err
	}

	if physicalHost != nil {
		logger.Info("Successfully associated with PhysicalHost", "physicalhost", physicalHost.Name)
		conditions.MarkTrue(b7machine, infrastructurev1beta1.PhysicalHostAssociatedCondition)
	} else {
		logger.Info("No available or associated PhysicalHost found, requeuing")
		conditions.MarkFalse(b7machine, infrastructurev1beta1.PhysicalHostAssociatedCondition,
			infrastructurev1beta1.WaitingForPhysicalHostReason, clusterv1.ConditionSeverityInfo,
			"No available PhysicalHost found")
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

	// Handle based on PhysicalHost state and inspection status
	return r.handlePhysicalHostState(ctx, logger, b7machine, physicalHost)
}

// handlePhysicalHostState processes the PhysicalHost based on its current state.
func (r *Beskar7MachineReconciler) handlePhysicalHostState(ctx context.Context, logger logr.Logger, b7machine *infrastructurev1beta1.Beskar7Machine, physicalHost *infrastructurev1beta1.PhysicalHost) (ctrl.Result, error) {
	switch physicalHost.Status.State {
	case infrastructurev1beta1.StateReady:
		// Inspection complete and validated, host ready for final provisioning
		logger.Info("PhysicalHost inspection complete and ready")
		return r.handleReadyHost(ctx, logger, b7machine, physicalHost)

	case infrastructurev1beta1.StateInspecting:
		// Inspection in progress
		logger.Info("PhysicalHost inspection in progress")
		return r.handleInspectingHost(ctx, logger, b7machine, physicalHost)

	case infrastructurev1beta1.StateInUse:
		// Host claimed, need to trigger inspection
		logger.Info("PhysicalHost claimed, triggering inspection")
		return r.triggerInspection(ctx, logger, b7machine, physicalHost)

	case infrastructurev1beta1.StateError:
		logger.Error(nil, "PhysicalHost is in error state", "errorMessage", physicalHost.Status.ErrorMessage)
		conditions.MarkFalse(b7machine, infrastructurev1beta1.InfrastructureReadyCondition,
			infrastructurev1beta1.PhysicalHostErrorReason, clusterv1.ConditionSeverityError,
			"PhysicalHost %q in error state: %s", physicalHost.Name, physicalHost.Status.ErrorMessage)
		phase := "Failed"
		b7machine.Status.Phase = &phase
		b7machine.Status.Ready = false
		return ctrl.Result{}, nil

	default:
		logger.Info("PhysicalHost in intermediate state", "hostState", physicalHost.Status.State)
		conditions.MarkFalse(b7machine, infrastructurev1beta1.InfrastructureReadyCondition,
			infrastructurev1beta1.PhysicalHostNotReadyReason, clusterv1.ConditionSeverityInfo,
			"PhysicalHost %q is in state: %s", physicalHost.Name, physicalHost.Status.State)
		phase := "Pending"
		b7machine.Status.Phase = &phase
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
}

// triggerInspection initiates the inspection phase by booting the inspection image.
//
// Sequence:
//  1. Configure BMC for PXE boot and ensure power-on.
//  2. Mint a per-host bearer token (D-004); store the plaintext in a per-host
//     Secret (D-006) and signal the hash + lifetime to the PhysicalHost controller
//     via an annotation. Both writes happen before the inspection-request
//     annotation so the controller sees the token state at the same reconcile.
//  3. Signal the PhysicalHost controller to transition to Inspecting via the
//     inspection-request annotation (Pattern A; PhysicalHost owns its own status).
//
// We never write to PhysicalHost.Status here — both the token hash and the
// inspection-request travel through metadata.annotations and are persisted to
// status by the PhysicalHost reconciler on its next pass (BUG-1).
func (r *Beskar7MachineReconciler) triggerInspection(ctx context.Context, logger logr.Logger, b7machine *infrastructurev1beta1.Beskar7Machine, physicalHost *infrastructurev1beta1.PhysicalHost) (ctrl.Result, error) {
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

	if powerState != redfish.OnPowerState {
		if err := rfClient.SetPowerState(ctx, redfish.OnPowerState); err != nil {
			logger.Error(err, "Failed to power on system")
			internalmetrics.RecordPhysicalHostPowerOperation(internalmetrics.PowerOperationOn, physicalHost.Namespace, internalmetrics.ProvisioningOutcomeFailed)
			return ctrl.Result{}, err
		}
		internalmetrics.RecordPhysicalHostPowerOperation(internalmetrics.PowerOperationOn, physicalHost.Namespace, internalmetrics.ProvisioningOutcomeSuccess)
		logger.Info("Powered on system for inspection")
	}

	// Mint a fresh per-host bearer token unless the existing one is still
	// valid. Re-minting on every reconcile would invalidate any in-flight
	// bearer headers the inspector or target OS already received via the iPXE
	// kernel cmdline, and would force a re-render of the cmdline before each
	// boot. The 30-minute validity window (D-004) is comfortably above the
	// 10-minute DefaultInspectionTimeout, so reusing a still-valid token
	// across reconciles is safe.
	//
	// Re-mint conditions: no Bootstrap status block, empty TokenHash, missing
	// ExpiresAt, or ExpiresAt in the past. In any of those cases the existing
	// token is unusable and we must mint fresh. The plaintext is delivered to
	// the inspector via a Secret (so it survives manager restart during the
	// validity window — D-006); only the hash + lifetime are signalled to
	// PhysicalHost via an annotation. The plaintext is never logged.
	if bootstrapTokenStillValid(physicalHost, time.Now()) {
		logger.V(1).Info("Existing bootstrap token still valid; skipping mint")
	} else {
		if err := r.mintAndStoreBootstrapToken(ctx, logger, physicalHost); err != nil {
			logger.Error(err, "Failed to mint and store bootstrap token")
			return ctrl.Result{}, err
		}
	}

	// Mint a fresh boot nonce (D-009) unless the existing one is still valid
	// and unconsumed. The nonce has a shorter lifetime (10 min) than the bearer
	// token (30 min) and is single-use: once BootNonceConsumedAt is set by the
	// /boot handler (D-010) it is never reused — we always mint fresh on the
	// next triggerInspection. This block is independent of the token block above:
	// the two credentials have different lifecycles and neither mint affects the
	// other's state.
	if bootNonceStillValid(physicalHost, time.Now()) {
		logger.V(1).Info("Existing boot nonce still valid and unconsumed; skipping mint")
	} else {
		if err := r.mintAndStoreBootNonce(ctx, logger, physicalHost); err != nil {
			logger.Error(err, "Failed to mint and store boot nonce")
			return ctrl.Result{}, err
		}
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

// mintAndStoreBootstrapToken mints a fresh bearer token for the host, persists
// the plaintext in a per-host Secret (so it survives manager restart during the
// validity window — see D-006), and signals the hash + lifetime to the
// PhysicalHost controller via an annotation. The plaintext is never logged.
func (r *Beskar7MachineReconciler) mintAndStoreBootstrapToken(
	ctx context.Context,
	logger logr.Logger,
	physicalHost *infrastructurev1beta1.PhysicalHost,
) error {
	plaintext, hash, err := auth.MintToken()
	if err != nil {
		return fmt.Errorf("mint inspection token: %w", err)
	}
	issuedAt, expiresAt := auth.LifetimeFor(time.Now())

	// Persist plaintext in a per-host Secret BEFORE writing the hash to the
	// PhysicalHost annotation. If the Secret write fails we have not yet
	// advertised a token to the inspector, and the next reconcile will simply
	// mint a fresh one. Pass empty nonce so upsertBootstrapTokenSecret does
	// not clobber the "plaintext-boot-nonce" key written by mintAndStoreBootNonce.
	if err := r.upsertBootstrapTokenSecret(ctx, logger, physicalHost, plaintext, ""); err != nil {
		// Zero out plaintext from our local frame; the Go runtime can still hold
		// the original on the stack but this minimises the lifetime of the
		// reference held by this function.
		plaintext = "" //nolint:ineffassign,wastedassign // intentional
		return fmt.Errorf("store bootstrap token plaintext: %w", err)
	}
	plaintext = "" //nolint:ineffassign,wastedassign // intentional: drop plaintext reference ASAP

	if err := r.setBootstrapTokenAnnotation(ctx, logger, physicalHost, hash, issuedAt, expiresAt); err != nil {
		return err
	}
	logger.V(1).Info("Bootstrap token minted and stored", "host", physicalHost.Name)
	return nil
}

// bootstrapTokenSecretName returns the deterministic name of the per-host
// Secret holding the plaintext bearer token. Centralized so tests and the
// bootstrap-render code path agree on the name.
func bootstrapTokenSecretName(hostName string) string {
	return hostName + "-bootstrap-token"
}

// bootstrapTokenStillValid reports whether the PhysicalHost's existing
// Status.Bootstrap holds a usable bearer token at the given moment. A token is
// reusable when:
//   - Bootstrap status block is non-nil, AND
//   - TokenHash is non-empty (a token was minted and the hash has been
//     persisted to status — i.e. the PhysicalHost reconciler has already
//     consumed our previous BootstrapTokenAnnotation), AND
//   - ExpiresAt is set and strictly after now (still inside the validity
//     window).
//
// Returning false forces a re-mint on the next call to triggerInspection,
// which is the desired behaviour for an expired or never-issued token.
//
// Two sources are inspected, in order:
//
//  1. Status.Bootstrap.{TokenHash,ExpiresAt}. This is the steady state once
//     the PhysicalHost controller has consumed the bootstrap-token annotation.
//
//  2. A pending BootstrapTokenAnnotation that has not yet been promoted to
//     Status. Without this check, two consecutive Beskar7Machine reconciles
//     that both observe Status.Bootstrap as empty (because the PhysicalHost
//     controller has not yet processed the annotation from the first
//     reconcile) would both re-mint, race each other into the per-host
//     Secret, and end up with Secret.plaintext belonging to mint N+1 while
//     Status.TokenHash is from mint N — every inspector callback then 401s
//     because the presented token does not hash to the stored hash. Reading
//     the annotation back here gives the second reconcile a tiebreaker so
//     it skips minting when an in-flight token is already on the wire.
func bootstrapTokenStillValid(physicalHost *infrastructurev1beta1.PhysicalHost, now time.Time) bool {
	if physicalHost == nil {
		return false
	}
	// (1) Steady-state check.
	if physicalHost.Status.Bootstrap != nil &&
		physicalHost.Status.Bootstrap.TokenHash != "" &&
		physicalHost.Status.Bootstrap.ExpiresAt != nil &&
		now.Before(physicalHost.Status.Bootstrap.ExpiresAt.Time) {
		return true
	}
	// (2) Pending-annotation check: a previous reconcile of this same
	// Beskar7Machine already minted and signaled. The PhysicalHost
	// reconciler will lift this annotation into Status on its next pass;
	// until then, the Secret + annotation together are the authoritative
	// source of truth.
	if raw, ok := physicalHost.Annotations[BootstrapTokenAnnotation]; ok && raw != "" {
		var v BootstrapTokenAnnotationValue
		if err := json.Unmarshal([]byte(raw), &v); err == nil &&
			v.Hash != "" &&
			now.Before(v.ExpiresAt.Time) {
			return true
		}
	}
	return false
}

// bootNonceStillValid reports whether the PhysicalHost's existing
// Status.Bootstrap holds a usable boot nonce at the given moment. A nonce is
// reusable when ALL of the following hold:
//   - Bootstrap status block is non-nil, AND
//   - BootNonceHash is non-empty (a nonce was minted and the PhysicalHost
//     reconciler has promoted the BootNonceAnnotation into Status), AND
//   - BootNonceExpiresAt is set and strictly after now, AND
//   - BootNonceConsumedAt is nil — a consumed nonce is never valid; force a
//     fresh mint on the next triggerInspection call (re-provision path).
//
// Like bootstrapTokenStillValid, a pending BootNonceAnnotation that has not yet
// been promoted to Status also counts as valid (mint-race guard: two consecutive
// Beskar7Machine reconciles must not both mint and clobber each other's nonce in
// the per-host Secret while the PhysicalHost controller is still catching up).
func bootNonceStillValid(physicalHost *infrastructurev1beta1.PhysicalHost, now time.Time) bool {
	if physicalHost == nil {
		return false
	}
	// (1) Steady-state check from Status.Bootstrap.
	if physicalHost.Status.Bootstrap != nil &&
		physicalHost.Status.Bootstrap.BootNonceHash != "" &&
		physicalHost.Status.Bootstrap.BootNonceExpiresAt != nil &&
		now.Before(physicalHost.Status.Bootstrap.BootNonceExpiresAt.Time) &&
		physicalHost.Status.Bootstrap.BootNonceConsumedAt == nil {
		return true
	}
	// (2) Pending-annotation check: a previous reconcile already minted and
	// signalled via BootNonceAnnotation; the PhysicalHost controller has not
	// yet promoted it to Status. The annotation does not carry ConsumedAt —
	// once a nonce is consumed the handler writes Status directly (D-010), so
	// if ConsumedAt is set in Status it was caught by (1) above.
	if raw, ok := physicalHost.Annotations[BootNonceAnnotation]; ok && raw != "" {
		var v BootNonceAnnotationValue
		if err := json.Unmarshal([]byte(raw), &v); err == nil &&
			v.Hash != "" &&
			now.Before(v.ExpiresAt.Time) {
			return true
		}
	}
	return false
}

// upsertBootstrapTokenSecret writes one or both of the plaintext credentials to
// the per-host Secret in the host's namespace. The Secret is owned by the
// PhysicalHost so it is GC'd on host deletion. Idempotent via
// controllerutil.CreateOrUpdate — re-minting cleanly overwrites the previous
// value without clobbering the key that was not re-minted.
//
// token is the plaintext bearer token; nonce is the plaintext boot nonce.
// Either may be empty ("") to indicate "do not update this key this call".
// Passing both empty is a no-op (caller should not do this but it is safe).
//
// Neither plaintext is ever logged. The CreateOrUpdate operation type is
// logged at V(1) but the Secret's data field is only passed by reference into
// the mutation closure.
func (r *Beskar7MachineReconciler) upsertBootstrapTokenSecret(
	ctx context.Context,
	logger logr.Logger,
	physicalHost *infrastructurev1beta1.PhysicalHost,
	token string,
	nonce string,
) error {
	secretName := bootstrapTokenSecretName(physicalHost.Name)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: physicalHost.Namespace,
		},
	}
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		if secret.Labels == nil {
			secret.Labels = map[string]string{}
		}
		secret.Labels[inspectionResultLabelOwnedBy] = "beskar7-controller-manager"
		secret.Labels[inspectionResultLabelHost] = physicalHost.Name
		// PhysicalHost-owned: GC'd when the host is deleted.
		if err := controllerutil.SetControllerReference(physicalHost, secret, r.Scheme); err != nil {
			return fmt.Errorf("set controller reference on bootstrap-token Secret: %w", err)
		}
		secret.Type = corev1.SecretTypeOpaque
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		// Write whichever keys were provided; do not clobber keys not supplied.
		if token != "" {
			secret.Data["plaintext-token"] = []byte(token)
		}
		if nonce != "" {
			secret.Data["plaintext-boot-nonce"] = []byte(nonce)
		}
		return nil
	})
	if err != nil {
		return err
	}
	logger.V(1).Info("Bootstrap token Secret upsert", "secret", secretName, "op", op)
	return nil
}

// setBootstrapTokenAnnotation patches PhysicalHost metadata.annotations with
// the JSON-encoded BootstrapTokenAnnotationValue (hash + lifetime). The
// PhysicalHost controller reads it on its next reconcile and persists the
// values to Status.Bootstrap.{TokenHash,IssuedAt,ExpiresAt}, then clears the
// annotation. Optimistic locking ensures we don't trample a concurrent annotation
// write from another reconciler.
func (r *Beskar7MachineReconciler) setBootstrapTokenAnnotation(
	ctx context.Context,
	logger logr.Logger,
	physicalHost *infrastructurev1beta1.PhysicalHost,
	hash string,
	issuedAt, expiresAt metav1.Time,
) error {
	value := BootstrapTokenAnnotationValue{
		Hash:      hash,
		IssuedAt:  issuedAt,
		ExpiresAt: expiresAt,
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal bootstrap-token annotation value: %w", err)
	}
	base := physicalHost.DeepCopy()
	if physicalHost.Annotations == nil {
		physicalHost.Annotations = map[string]string{}
	}
	physicalHost.Annotations[BootstrapTokenAnnotation] = string(encoded)
	// Plain MergeFrom (no optimistic lock). The annotation key is unique to
	// this controller — no other writer sets BootstrapTokenAnnotation — so a
	// concurrent modification of the PhysicalHost (status updates from the
	// PhysicalHost reconciler, spec patches from finalizer logic, etc.)
	// cannot collide on this field. Optimistic locking here was failing
	// repeatedly under normal load, causing the mint to silently retry on
	// every reconcile, each retry overwriting the per-host Secret with a
	// new plaintext that nobody could ever match against Status.TokenHash
	// — every inspector callback 401'd. Switching to plain merge resolves
	// the loop because our write always succeeds on the latest server
	// version of the object.
	if err := r.Patch(ctx, physicalHost, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("set bootstrap-token annotation on PhysicalHost %s: %w", physicalHost.Name, err)
	}
	// We log the host name and operation only — never the hash plaintext, never
	// the encoded body (which is safe but contains the hash, kept off INFO out
	// of caution).
	logger.V(1).Info("Set bootstrap-token annotation", "host", physicalHost.Name)
	return nil
}

// mintAndStoreBootNonce mints a fresh boot nonce for the host (D-009), persists
// the plaintext under "plaintext-boot-nonce" in the per-host bootstrap-token
// Secret (same Secret as the bearer token, separate key — single Secret, single
// owner-ref, single GC lifecycle), and signals the hash + expiry to the
// PhysicalHost controller via BootNonceAnnotation. The plaintext is never logged.
func (r *Beskar7MachineReconciler) mintAndStoreBootNonce(
	ctx context.Context,
	logger logr.Logger,
	physicalHost *infrastructurev1beta1.PhysicalHost,
) error {
	// Reuse MintToken's primitive: base64url(32 rand bytes) + sha256-hex.
	// The resulting nonce has the same entropy properties as the bearer token.
	noncePlaintext, nonceHash, err := auth.MintToken()
	if err != nil {
		return fmt.Errorf("mint boot nonce: %w", err)
	}
	expiresAt := auth.NonceLifetimeFor(time.Now())

	// Persist the plaintext in the per-host Secret BEFORE advertising the hash
	// via the annotation. If the Secret write fails, the annotation is never set
	// and the next reconcile mints fresh. Pass empty token so we don't clobber
	// the bearer-token key that mintAndStoreBootstrapToken wrote.
	if err := r.upsertBootstrapTokenSecret(ctx, logger, physicalHost, "", noncePlaintext); err != nil {
		noncePlaintext = "" //nolint:ineffassign,wastedassign // intentional
		return fmt.Errorf("store boot nonce plaintext: %w", err)
	}
	noncePlaintext = "" //nolint:ineffassign,wastedassign // intentional: drop plaintext reference ASAP

	if err := r.setBootNonceAnnotation(ctx, logger, physicalHost, nonceHash, expiresAt); err != nil {
		return err
	}
	logger.V(1).Info("Boot nonce minted and stored", "host", physicalHost.Name)
	return nil
}

// setBootNonceAnnotation patches PhysicalHost metadata.annotations with the
// JSON-encoded BootNonceAnnotationValue (hash + expiry). The PhysicalHost
// controller reads it on its next reconcile and persists the values to
// Status.Bootstrap.{BootNonceHash,BootNonceExpiresAt}, then clears the
// annotation. Plain MergeFrom (no optimistic lock) for the same reasons as
// setBootstrapTokenAnnotation: single writer, no collision risk on this key.
func (r *Beskar7MachineReconciler) setBootNonceAnnotation(
	ctx context.Context,
	logger logr.Logger,
	physicalHost *infrastructurev1beta1.PhysicalHost,
	hash string,
	expiresAt metav1.Time,
) error {
	value := BootNonceAnnotationValue{
		Hash:      hash,
		ExpiresAt: expiresAt,
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal boot-nonce annotation value: %w", err)
	}
	base := physicalHost.DeepCopy()
	if physicalHost.Annotations == nil {
		physicalHost.Annotations = map[string]string{}
	}
	physicalHost.Annotations[BootNonceAnnotation] = string(encoded)
	if err := r.Patch(ctx, physicalHost, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("set boot-nonce annotation on PhysicalHost %s: %w", physicalHost.Name, err)
	}
	// Log host name only — never the hash or the encoded body at INFO level.
	logger.V(1).Info("Set boot-nonce annotation", "host", physicalHost.Name)
	return nil
}

// handleInspectingHost monitors the inspection phase.
// On timeout it signals the PhysicalHost controller via annotation rather than
// writing to PhysicalHost.Status directly (BUG-1 fix).
func (r *Beskar7MachineReconciler) handleInspectingHost(ctx context.Context, logger logr.Logger, b7machine *infrastructurev1beta1.Beskar7Machine, physicalHost *infrastructurev1beta1.PhysicalHost) (ctrl.Result, error) {
	logger.Info("Monitoring inspection phase", "inspectionPhase", physicalHost.Status.InspectionPhase)

	// Check for timeout. Inspection timeout is terminal: we cannot recover
	// automatically — the operator must investigate (likely an iPXE
	// misconfiguration or unreachable callback endpoint) and either
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
			r.markTerminalFailure(b7machine, infrastructurev1beta1.InspectionTimedOutReason, msg)
			return ctrl.Result{}, nil
		}
	}

	// Check if inspection is complete
	if physicalHost.Status.InspectionPhase == infrastructurev1beta1.InspectionPhaseComplete {
		logger.Info("Inspection complete, validating")
		return r.validateInspectionReport(ctx, logger, b7machine, physicalHost)
	}

	phase := "Inspecting"
	b7machine.Status.Phase = &phase
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// validateInspectionReport validates the inspection report against requirements.
func (r *Beskar7MachineReconciler) validateInspectionReport(ctx context.Context, logger logr.Logger, b7machine *infrastructurev1beta1.Beskar7Machine, physicalHost *infrastructurev1beta1.PhysicalHost) (ctrl.Result, error) {
	logger.Info("Validating inspection report")

	if physicalHost.Status.InspectionReport == nil {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	report := physicalHost.Status.InspectionReport

	// Validate hardware requirements if specified
	if b7machine.Spec.HardwareRequirements != nil {
		reqs := b7machine.Spec.HardwareRequirements

		// Calculate total cores from all CPUs
		totalCores := 0
		for _, cpu := range report.CPUs {
			totalCores += cpu.Cores
		}

		// Hardware-validation failures are terminal. The BMC's hardware does not
		// change at runtime; requeueing forever would just churn. Operator must
		// lower the requirements, allocate to a different host, or replace the
		// hardware. CAPI surfaces FailureReason/FailureMessage in
		// `kubectl describe machine`.
		if reqs.MinCPUCores > 0 && totalCores < reqs.MinCPUCores {
			msg := fmt.Sprintf("insufficient CPU cores: found %d, required %d", totalCores, reqs.MinCPUCores)
			logger.Info("Hardware validation failed (terminal)", "check", "MinCPUCores", "found", totalCores, "required", reqs.MinCPUCores)
			r.markTerminalFailure(b7machine, infrastructurev1beta1.HardwareRequirementsNotMetReason, msg)
			return ctrl.Result{}, nil
		}

		// Calculate total memory from all DIMMs
		totalMemoryGB := 0
		for _, mem := range report.Memory {
			memGB, err := parseMemoryCapacityGB(mem.Capacity)
			if err != nil {
				logger.Error(err, "Failed to parse memory capacity", "capacity", mem.Capacity)
				continue
			}
			totalMemoryGB += memGB
		}

		if reqs.MinMemoryGB > 0 && totalMemoryGB < reqs.MinMemoryGB {
			msg := fmt.Sprintf("insufficient memory: found %d GB, required %d GB", totalMemoryGB, reqs.MinMemoryGB)
			logger.Info("Hardware validation failed (terminal)", "check", "MinMemoryGB", "found", totalMemoryGB, "required", reqs.MinMemoryGB)
			r.markTerminalFailure(b7machine, infrastructurev1beta1.HardwareRequirementsNotMetReason, msg)
			return ctrl.Result{}, nil
		}

		if reqs.MinDiskGB > 0 {
			totalDisk := 0
			for _, disk := range report.Disks {
				totalDisk += disk.SizeGB
			}
			if totalDisk < reqs.MinDiskGB {
				msg := fmt.Sprintf("insufficient disk space: found %d GB, required %d GB", totalDisk, reqs.MinDiskGB)
				logger.Info("Hardware validation failed (terminal)", "check", "MinDiskGB", "found", totalDisk, "required", reqs.MinDiskGB)
				r.markTerminalFailure(b7machine, infrastructurev1beta1.HardwareRequirementsNotMetReason, msg)
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

	return ctrl.Result{Requeue: true}, nil
}

// handleReadyHost handles a host that's ready after inspection.
func (r *Beskar7MachineReconciler) handleReadyHost(ctx context.Context, logger logr.Logger, b7machine *infrastructurev1beta1.Beskar7Machine, physicalHost *infrastructurev1beta1.PhysicalHost) (ctrl.Result, error) {
	logger.Info("Host ready, marking infrastructure as ready")

	// Set ProviderID
	currentProviderID := providerID(physicalHost.Namespace, physicalHost.Name)
	if b7machine.Spec.ProviderID == nil || *b7machine.Spec.ProviderID != currentProviderID {
		logger.Info("Setting ProviderID", "ProviderID", currentProviderID)
		b7machine.Spec.ProviderID = &currentProviderID
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
	conditions.MarkTrue(b7machine, infrastructurev1beta1.InfrastructureReadyCondition)
	b7machine.Status.Ready = true
	// CAPI v1beta2 contract: surface to Machine.status.initialization.infrastructureProvisioned.
	// Without this, CAPI v1.10+ does not advance the Machine past Pending and
	// the parent Cluster never reaches Available.
	b7machine.Status.Initialization = &infrastructurev1beta1.Beskar7MachineInitializationStatus{Provisioned: true}
	phase := "Provisioned"
	b7machine.Status.Phase = &phase

	if firstProvisioning {
		internalmetrics.RecordBeskar7MachineProvisioning(
			b7machine.Namespace,
			internalmetrics.ProvisioningOutcomeSuccess,
			time.Since(b7machine.CreationTimestamp.Time),
		)
	}

	logger.Info("Beskar7Machine infrastructure is ready")
	return ctrl.Result{}, nil
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
//  3. Find any StateAvailable host with no ConsumerRef and claim it. Returned
//     with RequeueAfter so the next reconcile picks the host up via path (2).
func (r *Beskar7MachineReconciler) findAndClaimOrGetAssociatedHost(ctx context.Context, logger logr.Logger, b7machine *infrastructurev1beta1.Beskar7Machine) (*infrastructurev1beta1.PhysicalHost, ctrl.Result, error) {
	claimStart := time.Now()

	// (1) ProviderID lookup.
	if b7machine.Spec.ProviderID != nil && *b7machine.Spec.ProviderID != "" {
		ns, name, err := parseProviderID(*b7machine.Spec.ProviderID)
		if err == nil && ns == b7machine.Namespace {
			host := &infrastructurev1beta1.PhysicalHost{}
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
	allHosts := &infrastructurev1beta1.PhysicalHostList{}
	if err := r.List(ctx, allHosts, client.InNamespace(b7machine.Namespace)); err != nil {
		internalmetrics.RecordHostClaimAttempt(b7machine.Namespace, internalmetrics.ClaimOutcomeError, internalmetrics.ConflictReasonNone)
		internalmetrics.RecordHostClaimDuration(b7machine.Namespace, internalmetrics.ClaimOutcomeError, time.Since(claimStart))
		return nil, ctrl.Result{}, err
	}
	for i := range allHosts.Items {
		h := &allHosts.Items[i]
		if h.Spec.ConsumerRef != nil && h.Spec.ConsumerRef.Name == b7machine.Name {
			internalmetrics.RecordHostClaimAttempt(b7machine.Namespace, internalmetrics.ClaimOutcomeSuccess, internalmetrics.ConflictReasonNone)
			internalmetrics.RecordHostClaimDuration(b7machine.Namespace, internalmetrics.ClaimOutcomeSuccess, time.Since(claimStart))
			return h, ctrl.Result{}, nil
		}
	}

	// (3) No existing claim — list StateAvailable hosts and try to claim one.
	// The field index filters server-side to StateAvailable so the result set
	// is bounded by the count of free hosts.
	hostList := &infrastructurev1beta1.PhysicalHostList{}
	if err := r.List(ctx, hostList,
		client.InNamespace(b7machine.Namespace),
		client.MatchingFields{PhysicalHostStateIndex: string(infrastructurev1beta1.StateAvailable)},
	); err != nil {
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
					return nil, ctrl.Result{Requeue: true}, nil
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
func (r *Beskar7MachineReconciler) reconcileDelete(ctx context.Context, logger logr.Logger, b7machine *infrastructurev1beta1.Beskar7Machine) (ctrl.Result, error) {
	logger.Info("Reconciling deletion")

	host, err := r.findClaimedHostForRelease(ctx, logger, b7machine)
	if err != nil {
		logger.Error(err, "Failed to look up claimed host during deletion")
		return ctrl.Result{}, err
	}
	if host != nil {
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
				return ctrl.Result{Requeue: true}, nil
			}
			logger.Error(err, "Failed to release host")
			return ctrl.Result{}, err
		}
		logger.Info("Released PhysicalHost", "host", host.Name)
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
func (r *Beskar7MachineReconciler) findClaimedHostForRelease(ctx context.Context, logger logr.Logger, b7machine *infrastructurev1beta1.Beskar7Machine) (*infrastructurev1beta1.PhysicalHost, error) {
	// Fast path: ProviderID names the host directly.
	if b7machine.Spec.ProviderID != nil && *b7machine.Spec.ProviderID != "" {
		ns, name, parseErr := parseProviderID(*b7machine.Spec.ProviderID)
		if parseErr != nil {
			logger.V(1).Info("Cannot parse ProviderID during deletion; falling back to ConsumerRef scan", "err", parseErr)
		} else {
			host := &infrastructurev1beta1.PhysicalHost{}
			err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, host)
			switch {
			case err == nil:
				if host.Spec.ConsumerRef != nil && host.Spec.ConsumerRef.Name == b7machine.Name {
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
	allHosts := &infrastructurev1beta1.PhysicalHostList{}
	if err := r.List(ctx, allHosts, client.InNamespace(b7machine.Namespace)); err != nil {
		return nil, err
	}
	for i := range allHosts.Items {
		h := &allHosts.Items[i]
		if h.Spec.ConsumerRef != nil && h.Spec.ConsumerRef.Name == b7machine.Name {
			return h, nil
		}
	}
	return nil, nil
}

// bestEffortReleaseRedfish issues ClearBootSourceOverride and a graceful power-off
// against the host's BMC. All errors are logged at Info and swallowed so a
// dead BMC cannot strand the Beskar7Machine finalizer.
// Missing credentials are treated identically — log a warning and return.
func (r *Beskar7MachineReconciler) bestEffortReleaseRedfish(ctx context.Context, logger logr.Logger, host *infrastructurev1beta1.PhysicalHost) {
	rfClient, err := r.getRedfishClientForHost(ctx, logger, host)
	if err != nil {
		logger.Info("Could not get Redfish client during release; skipping power-off and boot clear", "err", err)
		return
	}
	defer rfClient.Close(ctx)

	if err := rfClient.ClearBootSourceOverride(ctx); err != nil {
		logger.Info("Failed to clear boot source override during release; continuing", "err", err)
	}
	if err := rfClient.SetPowerState(ctx, redfish.OffPowerState); err != nil {
		logger.Info("Failed to graceful power-off during release; continuing", "err", err)
		internalmetrics.RecordPhysicalHostPowerOperation(internalmetrics.PowerOperationOff, host.Namespace, internalmetrics.ProvisioningOutcomeFailed)
	} else {
		internalmetrics.RecordPhysicalHostPowerOperation(internalmetrics.PowerOperationOff, host.Namespace, internalmetrics.ProvisioningOutcomeSuccess)
	}
}

// markTerminalFailure sets FailureReason/FailureMessage on the Beskar7Machine,
// flips Status.Ready to false and Phase to "Failed", and marks
// InfrastructureReadyCondition=False with the same reason at Severity=Error.
// CAPI surfaces FailureReason/FailureMessage in `kubectl describe machine`.
//
// Once set, FailureReason indicates the resource needs operator intervention.
// Subsequent reconciles must NOT clear it — clearing on success would mask
// history and the operator wouldn't know the resource ever failed. Callers
// should return ctrl.Result{}, nil after invoking this helper to stop the
// requeue cycle (CAPI does not auto-recover from FailureReason).
func (r *Beskar7MachineReconciler) markTerminalFailure(b7machine *infrastructurev1beta1.Beskar7Machine, reason, message string) {
	// Only record the provisioning-failed metric on the first terminal transition
	// to avoid double-counting on re-reconciles that call markTerminalFailure again.
	if b7machine.Status.FailureReason == nil {
		internalmetrics.RecordBeskar7MachineProvisioning(
			b7machine.Namespace,
			internalmetrics.ProvisioningOutcomeFailed,
			time.Since(b7machine.CreationTimestamp.Time),
		)
	}
	b7machine.Status.FailureReason = &reason
	b7machine.Status.FailureMessage = &message
	b7machine.Status.Ready = false
	phase := "Failed"
	b7machine.Status.Phase = &phase
	conditions.MarkFalse(b7machine, infrastructurev1beta1.InfrastructureReadyCondition,
		reason, clusterv1.ConditionSeverityError, "%s", message)
}

// setInspectionRequestAnnotation patches only the annotations of a PhysicalHost to request
// a state transition. The PhysicalHost controller reads the annotation on its next reconcile
// and drives the Status transition, preserving status ownership (BUG-1 fix, Pattern A).
// Uses optimistic locking via MergeFromWithOptions so a conflict causes a fast requeue.
func (r *Beskar7MachineReconciler) setInspectionRequestAnnotation(ctx context.Context, logger logr.Logger, physicalHost *infrastructurev1beta1.PhysicalHost, value string) error {
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
	b7machine *infrastructurev1beta1.Beskar7Machine,
	machine *clusterv1.Machine,
	physicalHost *infrastructurev1beta1.PhysicalHost,
) (ctrl.Result, error) {
	if machine.Spec.Bootstrap.DataSecretName == nil {
		logger.Info("Waiting for bootstrap data secret name to be set on Machine.Spec.Bootstrap")
		conditions.MarkFalse(b7machine, infrastructurev1beta1.BootstrapDataReadyCondition,
			infrastructurev1beta1.WaitingForBootstrapDataReason, clusterv1.ConditionSeverityInfo,
			"Waiting for Machine.Spec.Bootstrap.DataSecretName to be set")
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
			conditions.MarkFalse(b7machine, infrastructurev1beta1.BootstrapDataReadyCondition,
				infrastructurev1beta1.BootstrapDataUnavailableReason, clusterv1.ConditionSeverityError,
				"%s", msg)
			reason := infrastructurev1beta1.BootstrapDataUnavailableReason
			b7machine.Status.FailureReason = &reason
			b7machine.Status.FailureMessage = &msg
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

	conditions.MarkTrue(b7machine, infrastructurev1beta1.BootstrapDataReadyCondition)
	return ctrl.Result{}, nil
}

// setBootstrapURLAnnotation patches PhysicalHost metadata annotations with the
// per-host bootstrap URL. The PhysicalHost controller consumes this annotation
// and writes the value to Status.Bootstrap.URL, then clears the annotation.
// We patch only spec/annotations here — never status — preserving status ownership.
func (r *Beskar7MachineReconciler) setBootstrapURLAnnotation(
	ctx context.Context,
	logger logr.Logger,
	physicalHost *infrastructurev1beta1.PhysicalHost,
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
func (r *Beskar7MachineReconciler) getRedfishClientForHost(ctx context.Context, logger logr.Logger, host *infrastructurev1beta1.PhysicalHost) (internalredfish.Client, error) {
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
	list := &infrastructurev1beta1.Beskar7MachineList{}
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

// isPaused and isClusterPaused functions are in utils.go

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
		&infrastructurev1beta1.PhysicalHost{},
		PhysicalHostStateIndex,
		func(obj client.Object) []string {
			host, ok := obj.(*infrastructurev1beta1.PhysicalHost)
			if !ok {
				return nil
			}
			return []string{string(host.Status.State)}
		},
	); err != nil {
		return fmt.Errorf("failed to index PhysicalHost.status.state: %w", err)
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrastructurev1beta1.Beskar7Machine{}).
		Watches(
			&infrastructurev1beta1.PhysicalHost{},
			handler.EnqueueRequestsFromMapFunc(r.PhysicalHostToBeskar7Machine),
		).
		Complete(r)
}

// PhysicalHostToBeskar7Machine maps a PhysicalHost change to a reconcile
// request for the Beskar7Machine that currently consumes it. PhysicalHosts
// without a ConsumerRef, or with a ConsumerRef pointing at a non-Beskar7Machine
// kind, produce no requests.
func (r *Beskar7MachineReconciler) PhysicalHostToBeskar7Machine(ctx context.Context, obj client.Object) []reconcile.Request {
	host, ok := obj.(*infrastructurev1beta1.PhysicalHost)
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
