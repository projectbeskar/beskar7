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
	infrastructurev1beta1 "github.com/projectbeskar/beskar7/api/v1beta1"
	internalredfish "github.com/projectbeskar/beskar7/internal/redfish"
	"github.com/stmcginnis/gofish/redfish"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
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
)

// Beskar7MachineReconciler reconciles a Beskar7Machine object.
// Simplified for iPXE + inspection workflow.
type Beskar7MachineReconciler struct {
	client.Client
	Scheme               *runtime.Scheme
	RedfishClientFactory internalredfish.RedfishClientFactory
	Log                  logr.Logger
}

//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=beskar7machines,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=beskar7machines/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=beskar7machines/finalizers,verbs=update
//+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines;machines/status,verbs=get;list;watch
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=physicalhosts,verbs=get;list;watch;patch

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
	if !b7machine.ObjectMeta.DeletionTimestamp.IsZero() {
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
// It signals the PhysicalHost controller via an annotation (Pattern A) rather than
// writing to PhysicalHost.Status directly; PhysicalHost owns its own status.
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
			return ctrl.Result{}, err
		}
		logger.Info("Powered on system for inspection")
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

// handleInspectingHost monitors the inspection phase.
// On timeout it signals the PhysicalHost controller via annotation rather than
// writing to PhysicalHost.Status directly (BUG-1 fix).
func (r *Beskar7MachineReconciler) handleInspectingHost(ctx context.Context, logger logr.Logger, b7machine *infrastructurev1beta1.Beskar7Machine, physicalHost *infrastructurev1beta1.PhysicalHost) (ctrl.Result, error) {
	logger.Info("Monitoring inspection phase", "inspectionPhase", physicalHost.Status.InspectionPhase)

	// Check for timeout
	if physicalHost.Status.InspectionTimestamp != nil {
		elapsed := time.Since(physicalHost.Status.InspectionTimestamp.Time)
		if elapsed > DefaultInspectionTimeout {
			logger.Error(nil, "Inspection timeout", "elapsed", elapsed)
			// Signal the PhysicalHost controller to record the timeout in Status.
			// We patch only spec/annotations here; the PhysicalHost controller owns status.
			if err := r.setInspectionRequestAnnotation(ctx, logger, physicalHost, "timeout"); err != nil {
				logger.Error(err, "Failed to set inspection timeout annotation")
			}
			return ctrl.Result{}, fmt.Errorf("inspection timeout")
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

		if reqs.MinCPUCores > 0 && totalCores < reqs.MinCPUCores {
			err := fmt.Errorf("insufficient CPU cores: found %d, required %d", totalCores, reqs.MinCPUCores)
			logger.Error(err, "Hardware validation failed")
			return ctrl.Result{}, err
		}

		// Calculate total memory from all DIMMs
		totalMemoryGB := 0
		for _, mem := range report.Memory {
			// Parse capacity string (e.g., "32GB" -> 32)
			var memGB int
			if _, err := fmt.Sscanf(mem.Capacity, "%d", &memGB); err != nil {
				logger.Error(err, "Failed to parse memory capacity", "capacity", mem.Capacity)
				continue
			}
			totalMemoryGB += memGB
		}

		if reqs.MinMemoryGB > 0 && totalMemoryGB < reqs.MinMemoryGB {
			err := fmt.Errorf("insufficient memory: found %d GB, required %d GB", totalMemoryGB, reqs.MinMemoryGB)
			logger.Error(err, "Hardware validation failed")
			return ctrl.Result{}, err
		}

		if reqs.MinDiskGB > 0 {
			totalDisk := 0
			for _, disk := range report.Disks {
				totalDisk += disk.SizeGB
			}
			if totalDisk < reqs.MinDiskGB {
				err := fmt.Errorf("insufficient disk space: found %d GB, required %d GB", totalDisk, reqs.MinDiskGB)
				logger.Error(err, "Hardware validation failed")
				return ctrl.Result{}, err
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

	// Mark as ready
	conditions.MarkTrue(b7machine, infrastructurev1beta1.InfrastructureReadyCondition)
	b7machine.Status.Ready = true
	phase := "Provisioned"
	b7machine.Status.Phase = &phase

	logger.Info("Beskar7Machine infrastructure is ready")
	return ctrl.Result{}, nil
}

// findAndClaimOrGetAssociatedHost finds an available host or returns the associated one.
func (r *Beskar7MachineReconciler) findAndClaimOrGetAssociatedHost(ctx context.Context, logger logr.Logger, b7machine *infrastructurev1beta1.Beskar7Machine) (*infrastructurev1beta1.PhysicalHost, ctrl.Result, error) {
	// Check if we already have an associated host via ProviderID
	if b7machine.Spec.ProviderID != nil && *b7machine.Spec.ProviderID != "" {
		ns, name, err := parseProviderID(*b7machine.Spec.ProviderID)
		if err == nil && ns == b7machine.Namespace {
			host := &infrastructurev1beta1.PhysicalHost{}
			if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, host); err == nil {
				return host, ctrl.Result{}, nil
			}
		}
	}

	// Find available host
	hostList := &infrastructurev1beta1.PhysicalHostList{}
	if err := r.List(ctx, hostList, client.InNamespace(b7machine.Namespace)); err != nil {
		return nil, ctrl.Result{}, err
	}

	for i := range hostList.Items {
		host := &hostList.Items[i]
		if host.Status.State == infrastructurev1beta1.StateAvailable && host.Spec.ConsumerRef == nil {
			// Claim this host. Use a spec-only patch with optimistic locking so that a
			// concurrent claim by another Beskar7Machine fails fast with a conflict error
			// instead of silently winning (BUG-2 partial mitigation; full fix is PR-2.2).
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
					return nil, ctrl.Result{Requeue: true}, nil
				}
				logger.Error(err, "Failed to claim host")
				return nil, ctrl.Result{}, err
			}
			return host, ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
	}

	return nil, ctrl.Result{}, nil
}

// reconcileDelete handles deletion.
func (r *Beskar7MachineReconciler) reconcileDelete(ctx context.Context, logger logr.Logger, b7machine *infrastructurev1beta1.Beskar7Machine) (ctrl.Result, error) {
	logger.Info("Reconciling deletion")

	// Release the host via a spec-only patch (optimistic locking).
	if b7machine.Spec.ProviderID != nil && *b7machine.Spec.ProviderID != "" {
		ns, name, err := parseProviderID(*b7machine.Spec.ProviderID)
		if err == nil {
			host := &infrastructurev1beta1.PhysicalHost{}
			if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, host); err == nil {
				if host.Spec.ConsumerRef != nil && host.Spec.ConsumerRef.Name == b7machine.Name {
					base := host.DeepCopy()
					host.Spec.ConsumerRef = nil
					if err := r.Patch(ctx, host, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
						logger.Error(err, "Failed to release host")
						return ctrl.Result{}, err
					}
					logger.Info("Released PhysicalHost", "host", name)
				}
			}
		}
	}

	// Remove finalizer
	if controllerutil.RemoveFinalizer(b7machine, Beskar7MachineFinalizer) {
		logger.Info("Removing finalizer")
	}

	return ctrl.Result{}, nil
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

	return r.RedfishClientFactory(ctx, host.Spec.RedfishConnection.Address, username, password, insecure)
}

// Helper functions
func providerID(namespace, name string) string {
	return fmt.Sprintf("%s%s/%s", ProviderIDPrefix, namespace, name)
}

func parseProviderID(id string) (string, string, error) {
	if len(id) <= len(ProviderIDPrefix) {
		return "", "", fmt.Errorf("invalid provider ID format")
	}
	parts := id[len(ProviderIDPrefix):]
	idx := 0
	for i, c := range parts {
		if c == '/' {
			idx = i
			break
		}
	}
	if idx == 0 {
		return "", "", fmt.Errorf("invalid provider ID format")
	}
	return parts[:idx], parts[idx+1:], nil
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
		// Should be unreachable, but guard against a future programming error
		// that nilifies the field after this call.
		return fmt.Errorf("Beskar7MachineReconciler: RedfishClientFactory is nil after defaulting")
	}
	return nil
}

// SetupWithManager sets up the controller.
func (r *Beskar7MachineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := r.defaultFactory(); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrastructurev1beta1.Beskar7Machine{}).
		Complete(r)
}
