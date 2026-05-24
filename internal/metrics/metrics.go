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
	"", "Unknown", "Enrolling", "Available", "InUse", "Inspecting", "Ready", "Error",
}

// beskar7MachineCanonicalPhases lists every phase string that can appear in
// Beskar7Machine.Status.Phase. Same zero-reset rationale as above.
var beskar7MachineCanonicalPhases = []string{
	"Pending", "Inspecting", "Provisioned", "Failed",
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
