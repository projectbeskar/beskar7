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

package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
)

const (
	// Metric name prefixes
	MetricNamespace = "beskar7"
	MetricSubsystem = "controller"
)

var (
	// PhysicalHost metrics
	PhysicalHostStatesGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: MetricNamespace,
			Subsystem: MetricSubsystem,
			Name:      "physicalhost_states_total",
			Help:      "Number of PhysicalHosts in each state",
		},
		[]string{"state", "namespace"},
	)

	PhysicalHostPowerOperationsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: MetricNamespace,
			Subsystem: MetricSubsystem,
			Name:      "physicalhost_power_operations_total",
			Help:      "Total number of power operations performed on PhysicalHosts",
		},
		[]string{"operation", "outcome", "namespace"},
	)

	PhysicalHostRedfishConnectionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: MetricNamespace,
			Subsystem: MetricSubsystem,
			Name:      "physicalhost_redfish_connections_total",
			Help:      "Total number of Redfish connection attempts",
		},
		[]string{"outcome", "namespace", "error_type"},
	)

	// Beskar7Machine metrics
	Beskar7MachineStatesGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: MetricNamespace,
			Subsystem: MetricSubsystem,
			Name:      "beskar7machine_states_total",
			Help:      "Number of Beskar7Machines in each phase",
		},
		[]string{"phase", "namespace"},
	)

	Beskar7MachineProvisioningDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: MetricNamespace,
			Subsystem: MetricSubsystem,
			Name:      "beskar7machine_provisioning_duration_seconds",
			Help:      "Time taken to provision a Beskar7Machine from creation to ready",
			Buckets:   []float64{30, 60, 120, 300, 600, 1200, 1800, 3600}, // 30s to 1h
		},
		[]string{"outcome", "namespace"},
	)

	// Beskar7Cluster metrics
	Beskar7ClusterStatesGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: MetricNamespace,
			Subsystem: MetricSubsystem,
			Name:      "beskar7cluster_states_total",
			Help:      "Number of Beskar7Clusters in each readiness state",
		},
		[]string{"ready", "namespace"},
	)

	Beskar7ClusterFailureDomainsGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: MetricNamespace,
			Subsystem: MetricSubsystem,
			Name:      "beskar7cluster_failure_domains_total",
			Help:      "Number of failure domains discovered per cluster",
		},
		[]string{"cluster", "namespace"},
	)

	Beskar7ClusterFailureDomainDiscoveryTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: MetricNamespace,
			Subsystem: MetricSubsystem,
			Name:      "beskar7cluster_failure_domain_discovery_total",
			Help:      "Total number of failure domain discovery operations",
		},
		[]string{"outcome", "namespace"},
	)

	// Controller performance metrics
	ControllerReconciliationDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: MetricNamespace,
			Subsystem: MetricSubsystem,
			Name:      "reconciliation_duration_seconds",
			Help:      "Time taken to complete reconciliation",
			Buckets:   prometheus.DefBuckets, // Default buckets: .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10
		},
		[]string{"controller", "outcome", "namespace"},
	)

	ControllerReconciliationTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: MetricNamespace,
			Subsystem: MetricSubsystem,
			Name:      "reconciliation_total",
			Help:      "Total number of reconciliation attempts",
		},
		[]string{"controller", "outcome", "namespace"},
	)

	ControllerErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: MetricNamespace,
			Subsystem: MetricSubsystem,
			Name:      "errors_total",
			Help:      "Total number of errors encountered",
		},
		[]string{"controller", "error_type", "namespace"},
	)

	// Resource availability metrics
	PhysicalHostAvailabilityGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: MetricNamespace,
			Subsystem: MetricSubsystem,
			Name:      "physicalhost_availability",
			Help:      "Availability ratio of PhysicalHosts (available/total)",
		},
		[]string{"namespace"},
	)

	// Concurrent provisioning metrics
	hostClaimAttempts = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "beskar7_host_claim_attempts_total",
			Help: "Total number of host claim attempts",
		},
		[]string{"namespace", "outcome", "conflict_reason"},
	)

	hostClaimDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "beskar7_host_claim_duration_seconds",
			Help:    "Duration of host claim operations",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"namespace", "outcome"},
	)

	// Target image digest resolution metrics (D-038)
	TargetImageDigestResolutionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: MetricNamespace,
			Subsystem: MetricSubsystem,
			Name:      "target_image_digest_resolutions_total",
			Help:      "Total number of attempts to resolve spec.targetImageDigestURL into a pinned digest, by outcome",
		},
		[]string{"outcome", "namespace"},
	)

	TargetImageDigestResolutionDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: MetricNamespace,
			Subsystem: MetricSubsystem,
			Name:      "target_image_digest_resolution_duration_seconds",
			Help:      "Time taken by one attempt to resolve spec.targetImageDigestURL into a pinned digest",
			// One attempt is capped at 10s; an attempt that hits the cap ends a few
			// milliseconds past it, so the 12.5s bucket still holds it.
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 7.5, 10, 12.5},
		},
		[]string{"outcome", "namespace"},
	)
)

// ReconciliationOutcome represents the result of a reconciliation
type ReconciliationOutcome string

const (
	ReconciliationOutcomeSuccess  ReconciliationOutcome = "success"
	ReconciliationOutcomeError    ReconciliationOutcome = "error"
	ReconciliationOutcomeRequeue  ReconciliationOutcome = "requeue"
	ReconciliationOutcomeNotFound ReconciliationOutcome = "not_found"
)

// ErrorType categorizes different types of errors
type ErrorType string

const (
	ErrorTypeTransient  ErrorType = "transient"
	ErrorTypePermanent  ErrorType = "permanent"
	ErrorTypeValidation ErrorType = "validation"
	ErrorTypeConnection ErrorType = "connection"
	ErrorTypeTimeout    ErrorType = "timeout"
	ErrorTypeUnknown    ErrorType = "unknown"
)

// ProvisioningOutcome represents the result of a provisioning operation
type ProvisioningOutcome string

const (
	ProvisioningOutcomeSuccess ProvisioningOutcome = "success"
	ProvisioningOutcomeFailed  ProvisioningOutcome = "failed"
	ProvisioningOutcomeRetry   ProvisioningOutcome = "retry"
)

// PowerOperation represents different power operations
type PowerOperation string

const (
	PowerOperationOn    PowerOperation = "power_on"
	PowerOperationOff   PowerOperation = "power_off"
	PowerOperationReset PowerOperation = "power_reset"
)

// Init registers all metrics with the controller-runtime metrics registry
func Init() {
	metrics.Registry.MustRegister(
		// PhysicalHost metrics
		PhysicalHostStatesGauge,
		PhysicalHostPowerOperationsTotal,
		PhysicalHostRedfishConnectionsTotal,

		// Beskar7Machine metrics
		Beskar7MachineStatesGauge,
		Beskar7MachineProvisioningDuration,

		// Beskar7Cluster metrics
		Beskar7ClusterStatesGauge,
		Beskar7ClusterFailureDomainsGauge,
		Beskar7ClusterFailureDomainDiscoveryTotal,

		// Controller performance metrics
		ControllerReconciliationDuration,
		ControllerReconciliationTotal,
		ControllerErrorsTotal,

		// Resource availability metrics
		PhysicalHostAvailabilityGauge,

		// Concurrent provisioning metrics
		hostClaimAttempts,
		hostClaimDuration,

		// Target image digest resolution metrics
		TargetImageDigestResolutionsTotal,
		TargetImageDigestResolutionDuration,
	)
}

// RecordReconciliation records metrics for a reconciliation operation
func RecordReconciliation(controller string, namespace string, outcome ReconciliationOutcome, duration time.Duration) {
	ControllerReconciliationTotal.WithLabelValues(controller, string(outcome), namespace).Inc()
	ControllerReconciliationDuration.WithLabelValues(controller, string(outcome), namespace).Observe(duration.Seconds())
}

// RecordError records an error metric
func RecordError(controller string, namespace string, errorType ErrorType) {
	ControllerErrorsTotal.WithLabelValues(controller, string(errorType), namespace).Inc()
}

// physicalHostCanonicalStates lists every state string that can appear in
// PhysicalHost.Status.State. Enumerating them here lets UpdatePhysicalHostStateCounts
// zero-out states that have dropped to zero rather than leaving stale gauge series.
var physicalHostCanonicalStates = []string{
	infrav1.StateNone, infrav1.StateUnknown, infrav1.StateEnrolling, infrav1.StateAvailable,
	infrav1.StateInUse, infrav1.StateInspecting, infrav1.StateDeploying, infrav1.StateReady,
	infrav1.StateError,
}

// beskar7MachineCanonicalPhases lists every phase string that can appear in
// Beskar7Machine.Status.Phase; "" counts machines with no phase yet. Same
// zero-reset rationale as above, and a phase missing here is never exported.
var beskar7MachineCanonicalPhases = []string{
	"", infrav1.PhasePending, infrav1.PhaseInspecting, infrav1.PhaseProvisioning,
	infrav1.PhaseProvisioned, infrav1.PhaseFailed,
}

// UpdatePhysicalHostStateCounts sets the PhysicalHost state gauge from a precomputed
// counts map (state string → count). States absent from the map are set to zero so
// series that drop to zero are reflected correctly rather than stuck at the last
// non-zero value. Callers produce the map via a namespace-scoped List; no previous
// state needs to be tracked by the caller.
func UpdatePhysicalHostStateCounts(namespace string, counts map[string]int) {
	if counts == nil {
		counts = map[string]int{}
	}
	for _, state := range physicalHostCanonicalStates {
		PhysicalHostStatesGauge.WithLabelValues(state, namespace).Set(float64(counts[state]))
	}
}

// UpdateBeskar7MachineStateCounts sets the Beskar7Machine phase gauge from a precomputed
// counts map (phase string → count). Same zero-reset semantics as UpdatePhysicalHostStateCounts.
func UpdateBeskar7MachineStateCounts(namespace string, counts map[string]int) {
	if counts == nil {
		counts = map[string]int{}
	}
	for _, phase := range beskar7MachineCanonicalPhases {
		Beskar7MachineStatesGauge.WithLabelValues(phase, namespace).Set(float64(counts[phase]))
	}
}

// UpdateBeskar7ClusterStateCounts sets the Beskar7Cluster readiness gauge from precomputed
// ready/notReady counts. Same zero-reset semantics as UpdatePhysicalHostStateCounts.
func UpdateBeskar7ClusterStateCounts(namespace string, readyCount, notReadyCount int) {
	Beskar7ClusterStatesGauge.WithLabelValues("true", namespace).Set(float64(readyCount))
	Beskar7ClusterStatesGauge.WithLabelValues("false", namespace).Set(float64(notReadyCount))
}

// RecordPhysicalHostPowerOperation records a power operation
func RecordPhysicalHostPowerOperation(operation PowerOperation, namespace string, outcome ProvisioningOutcome) {
	PhysicalHostPowerOperationsTotal.WithLabelValues(string(operation), string(outcome), namespace).Inc()
}

// RecordRedfishConnection records a Redfish connection attempt
func RecordRedfishConnection(namespace string, outcome ProvisioningOutcome, errorType ErrorType) {
	errorTypeStr := ""
	if outcome == ProvisioningOutcomeFailed {
		errorTypeStr = string(errorType)
	}
	PhysicalHostRedfishConnectionsTotal.WithLabelValues(string(outcome), namespace, errorTypeStr).Inc()
}

// RecordBeskar7MachineProvisioning records provisioning duration and outcome
func RecordBeskar7MachineProvisioning(namespace string, outcome ProvisioningOutcome, duration time.Duration) {
	Beskar7MachineProvisioningDuration.WithLabelValues(string(outcome), namespace).Observe(duration.Seconds())
}

// RecordFailureDomains records the number of failure domains for a cluster
func RecordFailureDomains(cluster string, namespace string, count float64) {
	Beskar7ClusterFailureDomainsGauge.WithLabelValues(cluster, namespace).Set(count)
}

// RecordFailureDomainDiscovery records a failure domain discovery operation
func RecordFailureDomainDiscovery(namespace string, outcome ProvisioningOutcome) {
	Beskar7ClusterFailureDomainDiscoveryTotal.WithLabelValues(string(outcome), namespace).Inc()
}

// UpdatePhysicalHostAvailability updates the availability ratio metric
func UpdatePhysicalHostAvailability(namespace string, availableCount, totalCount int) {
	ratio := 0.0
	if totalCount > 0 {
		ratio = float64(availableCount) / float64(totalCount)
	}
	PhysicalHostAvailabilityGauge.WithLabelValues(namespace).Set(ratio)
}

// ClaimOutcome represents the outcome of a host claim attempt
type ClaimOutcome string

const (
	ClaimOutcomeSuccess  ClaimOutcome = "success"
	ClaimOutcomeConflict ClaimOutcome = "conflict"
	ClaimOutcomeNoHosts  ClaimOutcome = "no_hosts"
	ClaimOutcomeError    ClaimOutcome = "error"
)

// ConflictReason represents the reason for a claim conflict
type ConflictReason string

const (
	ConflictReasonOptimisticLock ConflictReason = "optimistic_lock"
	ConflictReasonAlreadyClaimed ConflictReason = "already_claimed"
	ConflictReasonInvalidState   ConflictReason = "invalid_state"
	ConflictReasonNone           ConflictReason = "none"
)

// RecordHostClaimAttempt records a host claim attempt
func RecordHostClaimAttempt(namespace string, outcome ClaimOutcome, conflictReason ConflictReason) {
	hostClaimAttempts.WithLabelValues(namespace, string(outcome), string(conflictReason)).Inc()
}

// RecordHostClaimDuration records the duration of a host claim operation
func RecordHostClaimDuration(namespace string, outcome ClaimOutcome, duration time.Duration) {
	hostClaimDuration.WithLabelValues(namespace, string(outcome)).Observe(duration.Seconds())
}

// DigestResolutionOutcome is how one attempt to resolve spec.targetImageDigestURL
// ended. The set is closed and small on purpose: it is a metric label, and it must
// never carry the URL, the host, the image's file name or the digest.
type DigestResolutionOutcome string

const (
	// DigestResolutionSuccess: the checksum file named the image and its digest is pinned.
	DigestResolutionSuccess DigestResolutionOutcome = "success"
	// DigestResolutionInvalidSpec: the spec cannot be resolved whatever the server
	// says (the URL is not https or carries credentials, the image URL has no file
	// name, or neither digest field is set). Nothing was fetched.
	DigestResolutionInvalidSpec DigestResolutionOutcome = "invalid_spec"
	// DigestResolutionFetchError: no response came back for a reason that is not
	// one of the kinds below: the host name does not resolve, the connection is
	// refused or reset, or the request failed some other way.
	DigestResolutionFetchError DigestResolutionOutcome = "fetch_error"
	// DigestResolutionTLSError: the server's certificate is not trusted, is for
	// another host or is expired, or the server does not speak TLS.
	DigestResolutionTLSError DigestResolutionOutcome = "tls_error"
	// DigestResolutionTimeout: the attempt hit its 10 second cap.
	DigestResolutionTimeout DigestResolutionOutcome = "timeout"
	// DigestResolutionRedirectRefused: the server redirected off the origin that
	// was asked for, or more than three times.
	DigestResolutionRedirectRefused DigestResolutionOutcome = "redirect_refused"
	// DigestResolutionHTTPStatus: the server answered with anything but 200.
	DigestResolutionHTTPStatus DigestResolutionOutcome = "http_status"
	// DigestResolutionTooLarge: the checksum file is over the 64 KiB cap.
	DigestResolutionTooLarge DigestResolutionOutcome = "too_large"
	// DigestResolutionNoEntry: the file has no entry for the image's file name.
	DigestResolutionNoEntry DigestResolutionOutcome = "no_entry"
	// DigestResolutionInvalidEntry: the file has an entry for the image that
	// cannot be relied on: not a SHA-256 digest, or two entries that disagree.
	DigestResolutionInvalidEntry DigestResolutionOutcome = "invalid_entry"
	// DigestResolutionCanceled: the attempt was cut short by the reconcile's own
	// context (the manager is stopping), not by the server.
	DigestResolutionCanceled DigestResolutionOutcome = "canceled"
)

// RecordTargetImageDigestResolution records one attempt to resolve a machine's
// targetImageDigestURL: the counter and the duration histogram move together, so
// a histogram's _count always equals the matching counter series.
func RecordTargetImageDigestResolution(namespace string, outcome DigestResolutionOutcome, duration time.Duration) {
	TargetImageDigestResolutionsTotal.WithLabelValues(string(outcome), namespace).Inc()
	TargetImageDigestResolutionDuration.WithLabelValues(string(outcome), namespace).Observe(duration.Seconds())
}
