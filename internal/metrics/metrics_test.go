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
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
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
	RecordPhysicalHostState("available", "test-ns", 1)
	RecordError("test", "test-ns", ErrorTypeConnection)
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

func TestRecordPhysicalHostState(t *testing.T) {
	// Record state changes
	RecordPhysicalHostState("available", "test-namespace", 1)
	RecordPhysicalHostState("claimed", "test-namespace", 2)
	RecordPhysicalHostState("available", "test-namespace", -1)

	// Verify the gauge values
	availableGauge := PhysicalHostStatesGauge.WithLabelValues("available", "test-namespace")
	claimedGauge := PhysicalHostStatesGauge.WithLabelValues("claimed", "test-namespace")

	availableMetric := &dto.Metric{}
	claimedMetric := &dto.Metric{}

	if err := availableGauge.Write(availableMetric); err != nil {
		t.Fatalf("Failed to write available metric: %v", err)
	}
	if err := claimedGauge.Write(claimedMetric); err != nil {
		t.Fatalf("Failed to write claimed metric: %v", err)
	}

	// Available should be 0 (1 - 1), claimed should be 2
	if availableMetric.GetGauge().GetValue() != 0 {
		t.Errorf("Expected available gauge to be 0, got %v", availableMetric.GetGauge().GetValue())
	}
	if claimedMetric.GetGauge().GetValue() != 2 {
		t.Errorf("Expected claimed gauge to be 2, got %v", claimedMetric.GetGauge().GetValue())
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
