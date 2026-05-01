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
	infrastructurev1beta1 "github.com/projectbeskar/beskar7/api/v1beta1"
	internalredfish "github.com/projectbeskar/beskar7/internal/redfish"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	conditions "sigs.k8s.io/cluster-api/util/conditions"
)

const (
	// PhysicalHostFinalizer allows PhysicalHostReconciler to clean up resources before removal
	PhysicalHostFinalizer = "physicalhost.infrastructure.cluster.x-k8s.io"
)

// PhysicalHostReconciler reconciles a PhysicalHost object.
// Simplified for power management only - provisioning happens via iPXE + inspection.
type PhysicalHostReconciler struct {
	client.Client
	Log                  logr.Logger
	Scheme               *runtime.Scheme
	Recorder             record.EventRecorder
	RedfishClientFactory internalredfish.RedfishClientFactory
}

// NewPhysicalHostReconciler creates a new PhysicalHostReconciler
func NewPhysicalHostReconciler(
	c client.Client,
	scheme *runtime.Scheme,
	redfishFactory internalredfish.RedfishClientFactory,
	logger logr.Logger,
	recorder record.EventRecorder,
) *PhysicalHostReconciler {
	return &PhysicalHostReconciler{
		Client:               c,
		Log:                  logger,
		Scheme:               scheme,
		Recorder:             recorder,
		RedfishClientFactory: redfishFactory,
	}
}

//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=physicalhosts,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=physicalhosts/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=physicalhosts/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile handles PhysicalHost reconciliation.
// Simplified workflow: Connect via Redfish → Verify connection → Report ready.
func (r *PhysicalHostReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := r.Log.WithValues("physicalhost", req.NamespacedName)
	logger.Info("Starting reconciliation")

	// Fetch the PhysicalHost instance
	physicalHost := &infrastructurev1beta1.PhysicalHost{}
	if err := r.Get(ctx, req.NamespacedName, physicalHost); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("PhysicalHost resource not found, ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Unable to fetch PhysicalHost")
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !physicalHost.ObjectMeta.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, logger, physicalHost)
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(physicalHost, PhysicalHostFinalizer) {
		controllerutil.AddFinalizer(physicalHost, PhysicalHostFinalizer)
		if err := r.Update(ctx, physicalHost); err != nil {
			logger.Error(err, "Failed to add finalizer")
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Reconcile normal operation
	return r.reconcileNormal(ctx, logger, physicalHost)
}

// reconcileNormal handles normal (non-deletion) reconciliation.
func (r *PhysicalHostReconciler) reconcileNormal(ctx context.Context, logger logr.Logger, physicalHost *infrastructurev1beta1.PhysicalHost) (ctrl.Result, error) {
	logger.Info("Reconciling PhysicalHost", "currentState", physicalHost.Status.State)

	// Get Redfish credentials
	username, password, err := r.getRedfishCredentials(ctx, physicalHost)
	if err != nil {
		logger.Error(err, "Failed to get Redfish credentials")
		r.updateStatus(physicalHost, infrastructurev1beta1.StateError, false, err.Error())
		conditions.MarkFalse(physicalHost, infrastructurev1beta1.RedfishConnectionReadyCondition,
			infrastructurev1beta1.MissingCredentialsReason, clusterv1.ConditionSeverityError,
			"Failed to retrieve credentials: %v", err)
		if updateErr := r.Status().Update(ctx, physicalHost); updateErr != nil {
			logger.Error(updateErr, "Failed to update status")
			return ctrl.Result{}, updateErr
		}
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, err
	}

	// Determine insecure setting
	insecure := false
	if physicalHost.Spec.RedfishConnection.InsecureSkipVerify != nil {
		insecure = *physicalHost.Spec.RedfishConnection.InsecureSkipVerify
	}

	// Create Redfish client
	rfClient, err := r.RedfishClientFactory(ctx,
		physicalHost.Spec.RedfishConnection.Address,
		username,
		password,
		insecure,
	)
	if err != nil {
		logger.Error(err, "Failed to create Redfish client")
		r.updateStatus(physicalHost, infrastructurev1beta1.StateError, false, fmt.Sprintf("Redfish connection failed: %v", err))
		conditions.MarkFalse(physicalHost, infrastructurev1beta1.RedfishConnectionReadyCondition,
			infrastructurev1beta1.RedfishConnectionFailedReason, clusterv1.ConditionSeverityError,
			"Connection failed: %v", err)
		if updateErr := r.Status().Update(ctx, physicalHost); updateErr != nil {
			logger.Error(updateErr, "Failed to update status")
		}
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, err
	}
	defer rfClient.Close(ctx)

	// Get system information
	sysInfo, err := rfClient.GetSystemInfo(ctx)
	if err != nil {
		logger.Error(err, "Failed to get system info from Redfish")
		r.updateStatus(physicalHost, infrastructurev1beta1.StateError, false, fmt.Sprintf("Failed to query system: %v", err))
		conditions.MarkFalse(physicalHost, infrastructurev1beta1.RedfishConnectionReadyCondition,
			infrastructurev1beta1.RedfishQueryFailedReason, clusterv1.ConditionSeverityError,
			"Query failed: %v", err)
		if updateErr := r.Status().Update(ctx, physicalHost); updateErr != nil {
			logger.Error(updateErr, "Failed to update status")
		}
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, err
	}

	// Update hardware details
	physicalHost.Status.HardwareDetails = infrastructurev1beta1.HardwareDetails{
		Manufacturer: sysInfo.Manufacturer,
		Model:        sysInfo.Model,
		SerialNumber: sysInfo.SerialNumber,
		Status: infrastructurev1beta1.HardwareStatus{
			Health:       string(sysInfo.Status.Health),
			HealthRollup: string(sysInfo.Status.HealthRollup),
			State:        string(sysInfo.Status.State),
		},
	}

	// Get power state
	powerState, err := rfClient.GetPowerState(ctx)
	if err != nil {
		logger.Error(err, "Failed to get power state")
		// Non-fatal, continue
	} else {
		physicalHost.Status.ObservedPowerState = string(powerState)
		logger.Info("Observed power state", "state", powerState)
	}

	// Detect network addresses
	addresses, err := rfClient.GetNetworkAddresses(ctx)
	if err != nil {
		logger.Error(err, "Failed to get network addresses", "error", err)
		// Non-fatal, continue without addresses
	} else {
		physicalHost.Status.Addresses = internalredfish.ConvertToMachineAddresses(addresses)
		logger.Info("Retrieved network addresses", "count", len(addresses))
	}

	// Connection successful - mark as ready
	conditions.MarkTrue(physicalHost, infrastructurev1beta1.RedfishConnectionReadyCondition)

	// Determine state based on ConsumerRef
	if physicalHost.Spec.ConsumerRef != nil {
		// Host is claimed
		if physicalHost.Status.State != infrastructurev1beta1.StateInUse &&
			physicalHost.Status.State != infrastructurev1beta1.StateInspecting &&
			physicalHost.Status.State != infrastructurev1beta1.StateReady {
			logger.Info("Host claimed, transitioning to InUse", "consumer", physicalHost.Spec.ConsumerRef.Name)
			r.updateStatus(physicalHost, infrastructurev1beta1.StateInUse, true, "")
		}
	} else {
		// Host is available
		if physicalHost.Status.State != infrastructurev1beta1.StateAvailable {
			logger.Info("Host available, transitioning to Available")
			r.updateStatus(physicalHost, infrastructurev1beta1.StateAvailable, true, "")
			conditions.MarkTrue(physicalHost, infrastructurev1beta1.HostAvailableCondition)
		}
	}

	// Update status
	if err := r.Status().Update(ctx, physicalHost); err != nil {
		logger.Error(err, "Failed to update status")
		return ctrl.Result{}, err
	}

	logger.Info("Reconciliation complete", "state", physicalHost.Status.State, "ready", physicalHost.Status.Ready)
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

// reconcileDelete handles PhysicalHost deletion.
func (r *PhysicalHostReconciler) reconcileDelete(ctx context.Context, logger logr.Logger, physicalHost *infrastructurev1beta1.PhysicalHost) (ctrl.Result, error) {
	logger.Info("Reconciling PhysicalHost deletion")

	// If still claimed, log warning but allow deletion
	if physicalHost.Spec.ConsumerRef != nil {
		logger.Info("Warning: Deleting PhysicalHost that is still claimed", "consumer", physicalHost.Spec.ConsumerRef.Name)
		r.Recorder.Event(physicalHost, corev1.EventTypeWarning, "DeletingClaimedHost",
			fmt.Sprintf("Deleting host that is still claimed by %s", physicalHost.Spec.ConsumerRef.Name))
	}

	// Remove finalizer
	if controllerutil.ContainsFinalizer(physicalHost, PhysicalHostFinalizer) {
		controllerutil.RemoveFinalizer(physicalHost, PhysicalHostFinalizer)
		if err := r.Update(ctx, physicalHost); err != nil {
			logger.Error(err, "Failed to remove finalizer")
			return ctrl.Result{}, err
		}
		logger.Info("Finalizer removed")
	}

	return ctrl.Result{}, nil
}

// getRedfishCredentials retrieves Redfish credentials from the referenced secret.
func (r *PhysicalHostReconciler) getRedfishCredentials(ctx context.Context, physicalHost *infrastructurev1beta1.PhysicalHost) (string, string, error) {
	secretName := physicalHost.Spec.RedfishConnection.CredentialsSecretRef
	if secretName == "" {
		return "", "", fmt.Errorf("credentials secret reference is empty")
	}

	secret := &corev1.Secret{}
	secretKey := types.NamespacedName{
		Namespace: physicalHost.Namespace,
		Name:      secretName,
	}

	if err := r.Get(ctx, secretKey, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", "", fmt.Errorf("credentials secret %q not found", secretName)
		}
		return "", "", fmt.Errorf("failed to get credentials secret: %w", err)
	}

	username, ok := secret.Data["username"]
	if !ok {
		return "", "", fmt.Errorf("username not found in secret %q", secretName)
	}

	password, ok := secret.Data["password"]
	if !ok {
		return "", "", fmt.Errorf("password not found in secret %q", secretName)
	}

	return string(username), string(password), nil
}

// updateStatus is a helper to update PhysicalHost status fields.
func (r *PhysicalHostReconciler) updateStatus(ph *infrastructurev1beta1.PhysicalHost, state string, ready bool, errorMsg string) {
	ph.Status.State = state
	ph.Status.Ready = ready
	ph.Status.ErrorMessage = errorMsg
}

// defaultFactory sets RedfishClientFactory to the real gofish-backed constructor
// when the caller left it nil. Separated from SetupWithManager to keep the defaulting
// logic directly testable without spinning up a full controller-runtime Manager.
func (r *PhysicalHostReconciler) defaultFactory() error {
	if r.RedfishClientFactory == nil {
		r.RedfishClientFactory = internalredfish.NewClient
	}
	if r.RedfishClientFactory == nil {
		// Should be unreachable, but guard against a future programming error
		// that nilifies the field after this call.
		return fmt.Errorf("PhysicalHostReconciler: RedfishClientFactory is nil after defaulting")
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *PhysicalHostReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := r.defaultFactory(); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrastructurev1beta1.PhysicalHost{}).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.SecretToPhysicalHosts),
		).
		Complete(r)
}

// SecretToPhysicalHosts maps Secret changes to PhysicalHost reconcile requests.
func (r *PhysicalHostReconciler) SecretToPhysicalHosts(ctx context.Context, obj client.Object) []reconcile.Request {
	secret, ok := obj.(*corev1.Secret)
	if !ok {
		r.Log.Error(nil, "Expected a Secret but got something else", "object", obj)
		return nil
	}

	// Find all PhysicalHosts in the same namespace that reference this secret
	physicalHostList := &infrastructurev1beta1.PhysicalHostList{}
	if err := r.List(ctx, physicalHostList, client.InNamespace(secret.Namespace)); err != nil {
		r.Log.Error(err, "Failed to list PhysicalHosts for Secret watch", "secret", secret.Name)
		return nil
	}

	var requests []reconcile.Request
	for _, ph := range physicalHostList.Items {
		if ph.Spec.RedfishConnection.CredentialsSecretRef == secret.Name {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Namespace: ph.Namespace,
					Name:      ph.Name,
				},
			})
		}
	}

	return requests
}
