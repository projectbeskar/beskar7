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
	"sort"
	"time"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/cluster-api/util/paused"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
	internalmetrics "github.com/projectbeskar/beskar7/internal/metrics"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	// Beskar7ClusterFinalizer allows Beskar7ClusterReconciler to clean up resources associated
	// with Beskar7Cluster before removing it from the apiserver.
	Beskar7ClusterFinalizer = "beskar7cluster.infrastructure.cluster.x-k8s.io"
	zoneLabelKey            = "topology.kubernetes.io/zone"
)

// Beskar7ClusterReconciler reconciles a Beskar7Cluster object
type Beskar7ClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Log    logr.Logger
	// MaxConcurrentReconciles is the worker count for this controller. Zero
	// means DefaultMaxConcurrentReconciles (1).
	MaxConcurrentReconciles int
}

//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=beskar7clusters,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=beskar7clusters/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=beskar7clusters/finalizers,verbs=update
//+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters;clusters/status,verbs=get;list;watch
// Needed to discover failure domains.
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=physicalhosts,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *Beskar7ClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, reterr error) {
	startTime := time.Now()
	logger := log.FromContext(ctx).WithValues("beskar7cluster", req.NamespacedName)
	logger.Info("Starting reconciliation")

	// Initialize outcome tracking for metrics
	outcome := internalmetrics.ReconciliationOutcomeSuccess
	var errorType internalmetrics.ErrorType

	// Record reconciliation attempt and duration at the end
	defer func() {
		duration := time.Since(startTime)
		internalmetrics.RecordReconciliation("beskar7cluster", req.Namespace, outcome, duration)

		// Record errors if any occurred
		if reterr != nil {
			internalmetrics.RecordError("beskar7cluster", req.Namespace, errorType)
		}
	}()

	// Fetch the Beskar7Cluster instance
	b7cluster := &infrav1.Beskar7Cluster{}
	if err := r.Get(ctx, req.NamespacedName, b7cluster); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("Beskar7Cluster resource not found. Ignoring since object must be deleted")
			outcome = internalmetrics.ReconciliationOutcomeNotFound
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Unable to fetch Beskar7Cluster")
		outcome = internalmetrics.ReconciliationOutcomeError
		errorType = internalmetrics.ErrorTypeUnknown
		return ctrl.Result{}, err
	}

	// Recompute readiness-state gauge on every reconcile so the gauge stays current
	// even when reconcile short-circuits. Non-fatal: metric errors don't block reconcile.
	r.recomputeBeskar7ClusterMetrics(ctx, logger, req.Namespace)

	// Set the ownerRefs on the Beskar7Cluster
	cluster, err := util.GetOwnerCluster(ctx, r.Client, b7cluster.ObjectMeta)
	if err != nil {
		logger.Error(err, "Failed to get owner Cluster")
		outcome = internalmetrics.ReconciliationOutcomeError
		errorType = internalmetrics.ErrorTypeUnknown
		return ctrl.Result{}, err
	}
	if cluster == nil {
		logger.Info("Waiting for Cluster Controller to set OwnerRef on Beskar7Cluster")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	logger = logger.WithValues("cluster", cluster.Name)

	// Cluster.spec.paused, the paused annotation on the Cluster and the one on
	// this object all pause reconciliation; the helper also keeps the Paused
	// condition current, which is why it runs before the patch helper snapshot.
	if isPaused, requeue, err := paused.EnsurePausedCondition(ctx, r.Client, cluster, b7cluster); err != nil || isPaused || requeue {
		if isPaused {
			logger.Info("Beskar7Cluster reconciliation is paused")
		}
		return ctrl.Result{}, err
	}

	// Initialize patch helper.
	patchHelper, err := patch.NewHelper(b7cluster, r.Client)
	if err != nil {
		logger.Error(err, "Failed to init patch helper")
		outcome = internalmetrics.ReconciliationOutcomeError
		errorType = internalmetrics.ErrorTypeUnknown
		return ctrl.Result{}, err
	}

	// Objects written before v0.6.0 carry conditions with no reason, which the
	// metav1.Condition schema rejects on write. Repair them here, after the
	// helper has snapshotted the object, so the fix is seen as a change.
	repairLegacyConditions(b7cluster, logger)

	// Always attempt to Patch the Beskar7Cluster object and status after reconciliation.
	defer func() {
		// Set the summary condition based on ControlPlaneEndpointReady
		setReadySummary(b7cluster, logger, infrav1.ControlPlaneEndpointReady)

		if err := patchHelper.Patch(ctx, b7cluster, patch.WithOwnedConditions{Conditions: []string{
			clusterv1.ReadyCondition,
			clusterv1.PausedCondition,
			infrav1.ControlPlaneEndpointReady,
		}}); err != nil {
			logger.Error(err, "Failed to patch Beskar7Cluster")
			if reterr == nil {
				reterr = err
			}
		}
		logger.Info("Finished reconciliation")
	}()

	// Handle deletion reconciliation
	if !b7cluster.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, logger, b7cluster)
	}

	// Handle non-deleted clusters
	return r.reconcileNormal(ctx, logger, cluster, b7cluster)
}

func (r *Beskar7ClusterReconciler) reconcileNormal(ctx context.Context, logger logr.Logger, cluster *clusterv1.Cluster, b7cluster *infrav1.Beskar7Cluster) (ctrl.Result, error) {
	logger.Info("Reconciling Beskar7Cluster create/update")

	// If the Beskar7Cluster doesn't have our finalizer, add it.
	if controllerutil.AddFinalizer(b7cluster, Beskar7ClusterFinalizer) {
		logger.Info("Adding finalizer")
		return ctrl.Result{RequeueAfter: requeueShortly}, nil
	}

	// --- Reconcile Failure Domains ---
	if err := r.reconcileFailureDomains(ctx, logger, b7cluster); err != nil {
		// Treat failure to list PhysicalHosts as a transient error
		return ctrl.Result{}, err
	}

	// --- Reconcile ControlPlaneEndpoint ---
	// No requeue timer here: a missing endpoint is a condition an operator
	// resolves by editing Cluster or Beskar7Cluster spec, and the Cluster
	// watch (see SetupWithManager) wakes this reconcile the moment either
	// changes.
	reconcileControlPlaneEndpoint(logger, cluster, b7cluster)

	logger.Info("Beskar7Cluster reconciliation complete")
	return ctrl.Result{}, nil
}

func (r *Beskar7ClusterReconciler) reconcileFailureDomains(ctx context.Context, logger logr.Logger, b7cluster *infrav1.Beskar7Cluster) error {
	logger.Info("Reconciling failure domains")

	// Get current failure domains for comparison
	currentFailureDomains := b7cluster.Status.FailureDomains

	phList := &infrav1.PhysicalHostList{}
	if err := r.List(ctx, phList, client.InNamespace(b7cluster.Namespace)); err != nil {
		logger.Error(err, "Failed to list PhysicalHosts to determine failure domains")
		internalmetrics.RecordFailureDomainDiscovery(b7cluster.Namespace, internalmetrics.ProvisioningOutcomeFailed)
		return errors.Wrapf(err, "failed to list PhysicalHosts for failure domain discovery")
	}

	// Early return if no PhysicalHosts and no current domains
	if len(phList.Items) == 0 && len(currentFailureDomains) == 0 {
		logger.V(1).Info("No PhysicalHosts found and no existing failure domains, skipping update")
		internalmetrics.RecordFailureDomainDiscovery(b7cluster.Namespace, internalmetrics.ProvisioningOutcomeSuccess)
		return nil
	}

	// More efficient failure domain discovery
	zones := map[string]struct{}{}
	for _, ph := range phList.Items {
		if zone, exists := ph.Labels[zoneLabelKey]; exists && zone != "" {
			zones[zone] = struct{}{}
		}
	}
	// v1beta2 models failure domains as a list; keep it sorted so status is stable.
	newFailureDomains := make([]clusterv1.FailureDomain, 0, len(zones))
	for _, zone := range sortedKeys(zones) {
		newFailureDomains = append(newFailureDomains, clusterv1.FailureDomain{
			Name:         zone,
			ControlPlane: ptr.To(true), // Assume control plane can be placed in any discovered zone
		})
	}

	// Check if failure domains actually changed before updating
	if failureDomainsEqual(currentFailureDomains, newFailureDomains) {
		logger.V(1).Info("Failure domains unchanged, skipping status update",
			"domainCount", len(currentFailureDomains))
		internalmetrics.RecordFailureDomainDiscovery(b7cluster.Namespace, internalmetrics.ProvisioningOutcomeSuccess)
		return nil
	}

	// Update the cluster status with new failure domains
	b7cluster.Status.FailureDomains = newFailureDomains

	logger.Info("Updated failure domains",
		"previousCount", len(currentFailureDomains),
		"newCount", len(newFailureDomains),
		"domains", getFailureDomainKeys(newFailureDomains))

	// Record metrics
	internalmetrics.RecordFailureDomains(b7cluster.Name, b7cluster.Namespace, float64(len(newFailureDomains)))
	internalmetrics.RecordFailureDomainDiscovery(b7cluster.Namespace, internalmetrics.ProvisioningOutcomeSuccess)

	return nil
}

// getFailureDomainKeys extracts the keys from FailureDomains for logging
func getFailureDomainKeys(domains []clusterv1.FailureDomain) []string {
	keys := make([]string, 0, len(domains))
	for _, d := range domains {
		keys = append(keys, d.Name)
	}
	return keys
}

func sortedKeys(m map[string]struct{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// failureDomainsEqual compares two FailureDomains maps for equality
func failureDomainsEqual(a, b []clusterv1.FailureDomain) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || ptr.Deref(a[i].ControlPlane, false) != ptr.Deref(b[i].ControlPlane, false) {
			return false
		}
	}
	return true
}

// reconcileControlPlaneEndpoint mirrors the control-plane endpoint in effect
// to status. Beskar7 never discovers an endpoint and never writes Cluster or
// Beskar7Cluster spec (D-027): Cluster.spec.controlPlaneEndpoint wins when it
// IsValid(), since that is what a topology or an operator sets directly on
// the Cluster; otherwise Beskar7Cluster's own spec.controlPlaneEndpoint is
// used, which is what a ClusterClass variable patches in. IsValid() requires
// both host and port, so a host with no port counts as not set.
func reconcileControlPlaneEndpoint(logger logr.Logger, cluster *clusterv1.Cluster, b7cluster *infrav1.Beskar7Cluster) {
	logger.Info("Reconciling control plane endpoint")

	endpoint := cluster.Spec.ControlPlaneEndpoint
	source := "Cluster"
	if !endpoint.IsValid() {
		endpoint = b7cluster.Spec.ControlPlaneEndpoint
		source = "Beskar7Cluster"
	}

	if !endpoint.IsValid() {
		setFalse(b7cluster, infrav1.ControlPlaneEndpointReady, infrav1.ControlPlaneEndpointNotSetReason,
			"set spec.controlPlaneEndpoint (host and port) on Beskar7Cluster %s or on Cluster %s; Beskar7 does not discover one",
			b7cluster.Name, cluster.Name)
		b7cluster.Status.Ready = false
		// Initialization.Provisioned is one-shot per the v1beta2 contract; leave it
		// nil here rather than flipping back to false, so a cluster that briefly
		// loses its endpoint does not regress in CAPI's view.
		return
	}

	logger.Info("Control plane endpoint in effect", "source", source, "host", endpoint.Host, "port", endpoint.Port)
	b7cluster.Status.ControlPlaneEndpoint = endpoint
	setTrue(b7cluster, infrav1.ControlPlaneEndpointReady, infrav1.ControlPlaneEndpointSetReason)
	b7cluster.Status.Ready = true
	// CAPI v1beta2 contract: surface to Cluster.status.initialization.infrastructureProvisioned.
	b7cluster.Status.Initialization.Provisioned = ptr.To(true)
}

// reconcileDelete handles the cleanup when a Beskar7Cluster is marked for deletion.
func (r *Beskar7ClusterReconciler) reconcileDelete(ctx context.Context, logger logr.Logger, b7cluster *infrav1.Beskar7Cluster) (ctrl.Result, error) {
	logger.Info("Reconciling Beskar7Cluster deletion")

	// Mark conditions False
	setFalse(b7cluster, infrav1.ControlPlaneEndpointReady, clusterv1.DeletingReason, "Beskar7Cluster is being deleted")

	// Beskar7Cluster typically does not own external resources that require cleanup.
	// All infrastructure resources (PhysicalHosts, Beskar7Machines) are cleaned up
	// via their own controllers and finalizers.
	logger.Info("No cluster-specific cleanup required.")

	// Beskar7Cluster is deleted, remove the finalizer.
	if controllerutil.RemoveFinalizer(b7cluster, Beskar7ClusterFinalizer) {
		logger.Info("Removing finalizer")
		// Patching is handled by the deferred patch function in Reconcile.
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *Beskar7ClusterReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager, options controller.Options) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1.Beskar7Cluster{}).
		Watches(
			&infrav1.PhysicalHost{},
			handler.EnqueueRequestsFromMapFunc(r.PhysicalHostToBeskar7Clusters),
		).
		// Cluster.spec.controlPlaneEndpoint is an endpoint source (D-027) and
		// pause is read from the Cluster, so an edit to either must wake this
		// reconcile; nothing else would, as a missing endpoint sets no timer.
		Watches(
			&clusterv1.Cluster{},
			handler.EnqueueRequestsFromMapFunc(util.ClusterToInfrastructureMapFunc(ctx, infrav1.GroupVersion.WithKind("Beskar7Cluster"), r.Client, &infrav1.Beskar7Cluster{})),
		).
		// options was previously accepted and silently discarded; apply it, with
		// the worker count overlaid from the reconciler's own configuration.
		WithOptions(func() controller.Options {
			options.MaxConcurrentReconciles = maxConcurrentOrDefault(r.MaxConcurrentReconciles)
			return options
		}()).
		Complete(r)
}

// recomputeBeskar7ClusterMetrics lists all Beskar7Clusters in the given namespace and
// emits the readiness-state gauge metric. Called at the top of each Reconcile so the
// gauge stays current even when reconcile short-circuits. Errors are logged and
// swallowed — a metric failure must not affect reconcile correctness.
func (r *Beskar7ClusterReconciler) recomputeBeskar7ClusterMetrics(ctx context.Context, logger logr.Logger, namespace string) {
	list := &infrav1.Beskar7ClusterList{}
	if err := r.List(ctx, list, client.InNamespace(namespace)); err != nil {
		logger.V(1).Info("Failed to list Beskar7Clusters for metric recompute; skipping", "err", err.Error())
		return
	}
	readyCount, notReadyCount := 0, 0
	for _, c := range list.Items {
		if c.Status.Ready {
			readyCount++
		} else {
			notReadyCount++
		}
	}
	internalmetrics.UpdateBeskar7ClusterStateCounts(namespace, readyCount, notReadyCount)
}

// PhysicalHostToBeskar7Clusters maps a PhysicalHost event to reconcile requests for all Beskar7Clusters in the same namespace.
func (r *Beskar7ClusterReconciler) PhysicalHostToBeskar7Clusters(ctx context.Context, obj client.Object) []reconcile.Request {
	log := r.Log.WithValues("mapping", "PhysicalHostToBeskar7Clusters")
	physicalHost, ok := obj.(*infrav1.PhysicalHost)
	if !ok {
		log.Error(errors.New("unexpected type"), "Expected a PhysicalHost but got a %T", obj)
		return nil
	}

	clusterList := &infrav1.Beskar7ClusterList{}
	if err := r.List(ctx, clusterList, client.InNamespace(physicalHost.Namespace)); err != nil {
		log.Error(err, "failed to list Beskar7Clusters in namespace", "namespace", physicalHost.Namespace)
		return nil
	}

	requests := make([]reconcile.Request, len(clusterList.Items))
	for i, cluster := range clusterList.Items {
		requests[i] = reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      cluster.Name,
				Namespace: cluster.Namespace,
			},
		}
	}
	log.V(1).Info("Triggering reconciliation for Beskar7Clusters in namespace", "namespace", physicalHost.Namespace, "count", len(requests))
	return requests
}
