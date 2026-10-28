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
	"os"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
)

func TestInit(t *testing.T) {
	// Test that Init doesn't panic
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Init() panicked: %v", r)
		}
	}()

	// This should not panic
	Init()

	// Test some metrics exist by using them
	RecordReconciliation("test", "test-ns", ReconciliationOutcomeSuccess, 100*time.Millisecond)
	UpdatePhysicalHostStateCounts("test-ns", map[string]int{"Available": 1})
	RecordError("test", "test-ns", ErrorTypeConnection)

	// The digest resolution metrics must be in the registry the manager serves.
	RecordTargetImageDigestResolution("test-ns", DigestResolutionSuccess, time.Second)
	families, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatalf("Gather() failed: %v", err)
	}
	registered := map[string]bool{}
	for _, f := range families {
		registered[f.GetName()] = true
	}
	for _, name := range []string{
		"beskar7_controller_target_image_digest_resolutions_total",
		"beskar7_controller_target_image_digest_resolution_duration_seconds",
	} {
		if !registered[name] {
			t.Errorf("%s is not registered by Init()", name)
		}
	}
}

func TestRecordReconciliation(t *testing.T) {
	// Record a successful reconciliation
	RecordReconciliation("test-controller", "test-namespace", ReconciliationOutcomeSuccess, 100*time.Millisecond)

	// Verify the counter was incremented
	counter := ControllerReconciliationTotal.WithLabelValues("test-controller", "success", "test-namespace")
	metric := &dto.Metric{}
	if err := counter.Write(metric); err != nil {
		t.Fatalf("Failed to write metric: %v", err)
	}

	if metric.GetCounter().GetValue() == 0 {
		t.Error("Expected counter to be incremented")
	}

	// Note: Testing histograms directly is complex due to Prometheus implementation
	// We'll just verify that the histogram metric exists and can be retrieved
}

func TestUpdatePhysicalHostStateCounts(t *testing.T) {
	// Set state counts for a namespace
	UpdatePhysicalHostStateCounts("ph-ns-1", map[string]int{
		"Available":  3,
		"InUse":      2,
		"Inspecting": 1,
	})

	readGauge := func(state string) float64 {
		g := PhysicalHostStatesGauge.WithLabelValues(state, "ph-ns-1")
		m := &dto.Metric{}
		if err := g.Write(m); err != nil {
			t.Fatalf("Failed to write %s metric: %v", state, err)
		}
		return m.GetGauge().GetValue()
	}

	if got := readGauge("Available"); got != 3 {
		t.Errorf("Available: want 3, got %v", got)
	}
	if got := readGauge("InUse"); got != 2 {
		t.Errorf("InUse: want 2, got %v", got)
	}
	if got := readGauge("Inspecting"); got != 1 {
		t.Errorf("Inspecting: want 1, got %v", got)
	}
	// States absent from the map must be zeroed.
	if got := readGauge("Error"); got != 0 {
		t.Errorf("Error: want 0 (absent → zeroed), got %v", got)
	}

	// A subsequent call with only "Error" in the map must zero the previously-set states.
	UpdatePhysicalHostStateCounts("ph-ns-1", map[string]int{"Error": 5})
	if got := readGauge("Available"); got != 0 {
		t.Errorf("Available after reset: want 0, got %v", got)
	}
	if got := readGauge("Error"); got != 5 {
		t.Errorf("Error after update: want 5, got %v", got)
	}
}

func TestUpdateBeskar7MachineStateCounts(t *testing.T) {
	UpdateBeskar7MachineStateCounts("b7m-ns-1", map[string]int{
		"Provisioned": 4,
		"Failed":      1,
	})

	readGauge := func(phase string) float64 {
		g := Beskar7MachineStatesGauge.WithLabelValues(phase, "b7m-ns-1")
		m := &dto.Metric{}
		if err := g.Write(m); err != nil {
			t.Fatalf("Failed to write %s metric: %v", phase, err)
		}
		return m.GetGauge().GetValue()
	}

	if got := readGauge("Provisioned"); got != 4 {
		t.Errorf("Provisioned: want 4, got %v", got)
	}
	if got := readGauge("Failed"); got != 1 {
		t.Errorf("Failed: want 1, got %v", got)
	}
	// Absent phases are zeroed.
	if got := readGauge("Pending"); got != 0 {
		t.Errorf("Pending: want 0, got %v", got)
	}
}

// Every phase the controller sets must be exported: a phase missing from the
// canonical list is counted, then dropped (machines deploying were invisible).
func TestBeskar7MachineGaugeExportsEveryPhase(t *testing.T) {
	phases := []string{
		"", infrav1.PhasePending, infrav1.PhaseInspecting, infrav1.PhaseProvisioning,
		infrav1.PhaseProvisioned, infrav1.PhaseFailed,
	}
	counts := map[string]int{}
	for i, phase := range phases {
		counts[phase] = i + 1
	}
	UpdateBeskar7MachineStateCounts("b7m-ns-phases", counts)

	for _, phase := range phases {
		g := Beskar7MachineStatesGauge.WithLabelValues(phase, "b7m-ns-phases")
		m := &dto.Metric{}
		if err := g.Write(m); err != nil {
			t.Fatalf("Failed to write %q metric: %v", phase, err)
		}
		if got, want := m.GetGauge().GetValue(), float64(counts[phase]); got != want {
			t.Errorf("phase %q: want %v, got %v (missing from beskar7MachineCanonicalPhases?)", phase, want, got)
		}
	}
}

func TestUpdateBeskar7ClusterStateCounts(t *testing.T) {
	UpdateBeskar7ClusterStateCounts("b7c-ns-1", 2, 1)

	readReady := func(readyStr string) float64 {
		g := Beskar7ClusterStatesGauge.WithLabelValues(readyStr, "b7c-ns-1")
		m := &dto.Metric{}
		if err := g.Write(m); err != nil {
			t.Fatalf("Failed to write ready=%s metric: %v", readyStr, err)
		}
		return m.GetGauge().GetValue()
	}

	if got := readReady("true"); got != 2 {
		t.Errorf("ready=true: want 2, got %v", got)
	}
	if got := readReady("false"); got != 1 {
		t.Errorf("ready=false: want 1, got %v", got)
	}
}

func TestUpdatePhysicalHostAvailability(t *testing.T) {
	// Test availability calculation
	UpdatePhysicalHostAvailability("test-namespace", 3, 10)

	gauge := PhysicalHostAvailabilityGauge.WithLabelValues("test-namespace")
	metric := &dto.Metric{}

	if err := gauge.Write(metric); err != nil {
		t.Fatalf("Failed to write metric: %v", err)
	}

	expected := 0.3 // 3/10
	if metric.GetGauge().GetValue() != expected {
		t.Errorf("Expected availability ratio to be %v, got %v", expected, metric.GetGauge().GetValue())
	}

	// Test zero total case
	UpdatePhysicalHostAvailability("test-namespace-zero", 0, 0)

	zeroGauge := PhysicalHostAvailabilityGauge.WithLabelValues("test-namespace-zero")
	zeroMetric := &dto.Metric{}

	if err := zeroGauge.Write(zeroMetric); err != nil {
		t.Fatalf("Failed to write zero metric: %v", err)
	}

	if zeroMetric.GetGauge().GetValue() != 0 {
		t.Errorf("Expected zero availability ratio to be 0, got %v", zeroMetric.GetGauge().GetValue())
	}
}

func TestRecordBeskar7MachineProvisioning(t *testing.T) {
	// Record provisioning duration
	duration := 5 * time.Minute
	RecordBeskar7MachineProvisioning("test-namespace", ProvisioningOutcomeSuccess, duration)

	// Note: Testing histogram values directly is complex with Prometheus client
	// We'll just verify the function doesn't panic and can be called
	RecordBeskar7MachineProvisioning("test-namespace", ProvisioningOutcomeFailed, duration)
}

func TestRecordFailureDomains(t *testing.T) {
	// Record failure domains count
	RecordFailureDomains("test-cluster", "test-namespace", 3)

	gauge := Beskar7ClusterFailureDomainsGauge.WithLabelValues("test-cluster", "test-namespace")
	metric := &dto.Metric{}

	if err := gauge.Write(metric); err != nil {
		t.Fatalf("Failed to write metric: %v", err)
	}

	if metric.GetGauge().GetValue() != 3 {
		t.Errorf("Expected failure domains count to be 3, got %v", metric.GetGauge().GetValue())
	}
}

func TestRecordError(t *testing.T) {
	// Record different types of errors
	RecordError("test-controller", "test-namespace", ErrorTypeConnection)
	RecordError("test-controller", "test-namespace", ErrorTypeTimeout)
	RecordError("test-controller", "test-namespace", ErrorTypeValidation)

	// Verify counters
	connectionCounter := ControllerErrorsTotal.WithLabelValues("test-controller", "connection", "test-namespace")
	timeoutCounter := ControllerErrorsTotal.WithLabelValues("test-controller", "timeout", "test-namespace")
	validationCounter := ControllerErrorsTotal.WithLabelValues("test-controller", "validation", "test-namespace")

	connectionMetric := &dto.Metric{}
	timeoutMetric := &dto.Metric{}
	validationMetric := &dto.Metric{}

	if err := connectionCounter.Write(connectionMetric); err != nil {
		t.Fatalf("Failed to write connection metric: %v", err)
	}
	if err := timeoutCounter.Write(timeoutMetric); err != nil {
		t.Fatalf("Failed to write timeout metric: %v", err)
	}
	if err := validationCounter.Write(validationMetric); err != nil {
		t.Fatalf("Failed to write validation metric: %v", err)
	}

	if connectionMetric.GetCounter().GetValue() != 1 {
		t.Errorf("Expected connection error counter to be 1, got %v", connectionMetric.GetCounter().GetValue())
	}
	if timeoutMetric.GetCounter().GetValue() != 1 {
		t.Errorf("Expected timeout error counter to be 1, got %v", timeoutMetric.GetCounter().GetValue())
	}
	if validationMetric.GetCounter().GetValue() != 1 {
		t.Errorf("Expected validation error counter to be 1, got %v", validationMetric.GetCounter().GetValue())
	}
}

// TestPhysicalHostCanonicalStatesCoversEveryAPIState pins the gauge's state list
// against the API's own constants. The list claims to enumerate "every state
// string that can appear in PhysicalHost.Status.State", and zero-resets only the
// states it names — so a state missing from it is never emitted at all. Deploying
// was missing, which made every host writing its OS image invisible in
// beskar7_controller_physicalhost_states_total.
func TestPhysicalHostCanonicalStatesCoversEveryAPIState(t *testing.T) {
	apiStates := []string{
		infrav1.StateNone,
		infrav1.StateUnknown,
		infrav1.StateEnrolling,
		infrav1.StateAvailable,
		infrav1.StateInUse,
		infrav1.StateInspecting,
		infrav1.StateDeploying,
		infrav1.StateReady,
		infrav1.StateError,
	}
	listed := make(map[string]bool, len(physicalHostCanonicalStates))
	for _, s := range physicalHostCanonicalStates {
		listed[s] = true
	}
	for _, s := range apiStates {
		if !listed[s] {
			t.Errorf("state %q is set by the controllers but missing from physicalHostCanonicalStates, so the gauge never emits it", s)
		}
	}
}

// allDigestResolutionOutcomes is every outcome the resolution metrics can carry,
// written out as the literal label values operators write alerts against.
var allDigestResolutionOutcomes = map[DigestResolutionOutcome]string{
	DigestResolutionSuccess:         "success",
	DigestResolutionInvalidSpec:     "invalid_spec",
	DigestResolutionFetchError:      "fetch_error",
	DigestResolutionTLSError:        "tls_error",
	DigestResolutionTimeout:         "timeout",
	DigestResolutionRedirectRefused: "redirect_refused",
	DigestResolutionHTTPStatus:      "http_status",
	DigestResolutionTooLarge:        "too_large",
	DigestResolutionNoEntry:         "no_entry",
	DigestResolutionInvalidEntry:    "invalid_entry",
	DigestResolutionCanceled:        "canceled",
}

func readDigestResolutions(t *testing.T, outcome DigestResolutionOutcome, namespace string) (count float64, observed uint64, sum float64) {
	t.Helper()
	c := &dto.Metric{}
	if err := TargetImageDigestResolutionsTotal.WithLabelValues(string(outcome), namespace).Write(c); err != nil {
		t.Fatalf("Failed to write counter: %v", err)
	}
	h := &dto.Metric{}
	obs, ok := TargetImageDigestResolutionDuration.WithLabelValues(string(outcome), namespace).(prometheus.Metric)
	if !ok {
		t.Fatal("the duration histogram's observer is not a prometheus.Metric")
	}
	if err := obs.Write(h); err != nil {
		t.Fatalf("Failed to write histogram: %v", err)
	}
	return c.GetCounter().GetValue(), h.GetHistogram().GetSampleCount(), h.GetHistogram().GetSampleSum()
}

func TestRecordTargetImageDigestResolution(t *testing.T) {
	for outcome, want := range allDigestResolutionOutcomes {
		if string(outcome) != want {
			t.Errorf("outcome %q: the label value changed from %q, which breaks alerts written against it", outcome, want)
		}
		ns := "digest-" + want
		RecordTargetImageDigestResolution(ns, outcome, 250*time.Millisecond)
		RecordTargetImageDigestResolution(ns, outcome, 750*time.Millisecond)

		count, observed, sum := readDigestResolutions(t, outcome, ns)
		if count != 2 {
			t.Errorf("%s: counter = %v, want 2", want, count)
		}
		if observed != 2 {
			t.Errorf("%s: histogram observed %d attempts, want 2 (it must move with the counter)", want, observed)
		}
		if sum != 1 {
			t.Errorf("%s: histogram sum = %v seconds, want 1", want, sum)
		}

		// No other outcome of the same namespace moves.
		for other := range allDigestResolutionOutcomes {
			if other == outcome {
				continue
			}
			if c, o, _ := readDigestResolutions(t, other, ns); c != 0 || o != 0 {
				t.Errorf("recording %s moved %s in the same namespace (counter %v, histogram %d)", want, other, c, o)
			}
		}
	}
}

func TestRecordTargetImageDigestResolutionIsPerNamespace(t *testing.T) {
	RecordTargetImageDigestResolution("digest-ns-a", DigestResolutionHTTPStatus, time.Second)

	if c, o, _ := readDigestResolutions(t, DigestResolutionHTTPStatus, "digest-ns-a"); c != 1 || o != 1 {
		t.Errorf("digest-ns-a: counter %v, histogram %d, want 1 and 1", c, o)
	}
	if c, o, _ := readDigestResolutions(t, DigestResolutionHTTPStatus, "digest-ns-b"); c != 0 || o != 0 {
		t.Errorf("digest-ns-b moved with digest-ns-a: counter %v, histogram %d", c, o)
	}
}

// TestDigestResolutionLabelsAreOutcomeAndNamespaceOnly pins the label set. The
// checksum URL, its host, the image's file name and the digest come from the spec
// or from a server; none of them may become a label, for cardinality and so a
// metrics reader learns nothing about what a machine fetches.
func TestDigestResolutionLabelsAreOutcomeAndNamespaceOnly(t *testing.T) {
	RecordTargetImageDigestResolution("digest-labels", DigestResolutionNoEntry, time.Second)

	c := &dto.Metric{}
	if err := TargetImageDigestResolutionsTotal.WithLabelValues(string(DigestResolutionNoEntry), "digest-labels").Write(c); err != nil {
		t.Fatalf("Failed to write counter: %v", err)
	}
	got := map[string]string{}
	for _, l := range c.GetLabel() {
		got[l.GetName()] = l.GetValue()
	}
	want := map[string]string{"outcome": "no_entry", "namespace": "digest-labels"}
	if len(got) != len(want) || got["outcome"] != want["outcome"] || got["namespace"] != want["namespace"] {
		t.Errorf("counter labels = %v, want %v", got, want)
	}

	obs, ok := TargetImageDigestResolutionDuration.WithLabelValues(string(DigestResolutionNoEntry), "digest-labels").(prometheus.Metric)
	if !ok {
		t.Fatal("the duration histogram's observer is not a prometheus.Metric")
	}
	h := &dto.Metric{}
	if err := obs.Write(h); err != nil {
		t.Fatalf("Failed to write histogram: %v", err)
	}
	if len(h.GetLabel()) != 2 {
		t.Errorf("histogram has %d labels, want outcome and namespace only", len(h.GetLabel()))
	}
}

// TestDigestResolutionDurationBucketsSuitTheTenSecondCap: an attempt is cut off
// at 10 seconds and ends a few milliseconds after, so the bucket above 10s must
// exist, or every timeout would land only in +Inf.
func TestDigestResolutionDurationBucketsSuitTheTenSecondCap(t *testing.T) {
	RecordTargetImageDigestResolution("digest-buckets", DigestResolutionTimeout, 10*time.Second+5*time.Millisecond)
	RecordTargetImageDigestResolution("digest-buckets", DigestResolutionTimeout, 80*time.Millisecond)

	obs, ok := TargetImageDigestResolutionDuration.WithLabelValues(string(DigestResolutionTimeout), "digest-buckets").(prometheus.Metric)
	if !ok {
		t.Fatal("the duration histogram's observer is not a prometheus.Metric")
	}
	m := &dto.Metric{}
	if err := obs.Write(m); err != nil {
		t.Fatalf("Failed to write histogram: %v", err)
	}
	cumulative := map[float64]uint64{}
	for _, b := range m.GetHistogram().GetBucket() {
		cumulative[b.GetUpperBound()] = b.GetCumulativeCount()
	}
	for _, bound := range []float64{0.1, 10, 12.5} {
		if _, ok := cumulative[bound]; !ok {
			t.Fatalf("no bucket with upper bound %v; buckets: %v", bound, cumulative)
		}
	}
	if cumulative[0.1] != 1 {
		t.Errorf("le=0.1 holds %d, want the 80ms attempt only", cumulative[0.1])
	}
	if cumulative[10] != 1 {
		t.Errorf("le=10 holds %d, want 1: the attempt that ran past the cap is not in it", cumulative[10])
	}
	if cumulative[12.5] != 2 {
		t.Errorf("le=12.5 holds %d, want both attempts", cumulative[12.5])
	}
}

// TestDocsDescribeTheDigestResolutionMetrics keeps docs/metrics.md in step with
// the code: a section for each metric, and every outcome an operator can see in a
// label listed in the counter's section.
func TestDocsDescribeTheDigestResolutionMetrics(t *testing.T) {
	raw, err := os.ReadFile("../../docs/metrics.md")
	if err != nil {
		t.Fatalf("read docs/metrics.md: %v", err)
	}
	section := func(name string) string {
		heading := "#### `" + name + "`"
		_, rest, found := strings.Cut(string(raw), heading)
		if !found {
			t.Fatalf("docs/metrics.md has no section %q", heading)
		}
		body, _, _ := strings.Cut(rest, "\n#")
		return body
	}
	counter := section("beskar7_controller_target_image_digest_resolutions_total")
	section("beskar7_controller_target_image_digest_resolution_duration_seconds")
	for _, want := range allDigestResolutionOutcomes {
		if !strings.Contains(counter, "\n- `"+want+"` ") {
			t.Errorf("the counter's section does not list the outcome %q as a bullet", want)
		}
	}
}
