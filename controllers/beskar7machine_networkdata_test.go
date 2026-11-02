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
	"testing"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
)

const networkDataWaitGeneration = 3

// networkDataWaitHost is a claimed host with a network data ref and, when
// status is set, a NetworkDataReady condition observed at observedGeneration.
func networkDataWaitHost(status metav1.ConditionStatus, observedGeneration int64) *infrav1.PhysicalHost {
	host := networkDataTestHostWithRef(networkDataTestSecret)
	host.Generation = networkDataWaitGeneration
	host.Status.State = infrav1.StateInUse
	if status == "" {
		return host
	}
	host.Status.Conditions = []metav1.Condition{{
		Type:               infrav1.NetworkDataReadyCondition,
		Status:             status,
		Reason:             infrav1.NetworkDataNotAuthorizedReason,
		ObservedGeneration: observedGeneration,
	}}
	return host
}

func TestHostWaitingForNetworkData(t *testing.T) {
	tests := []struct {
		name string
		host *infrav1.PhysicalHost
		want bool
	}{
		{name: "no ref", host: networkDataTestHostWithRef(""), want: false},
		{name: "ref, not checked yet", host: networkDataWaitHost("", 0), want: true},
		{name: "ref, refused", host: networkDataWaitHost(metav1.ConditionFalse, networkDataWaitGeneration), want: true},
		{name: "ref, ready", host: networkDataWaitHost(metav1.ConditionTrue, networkDataWaitGeneration), want: false},
		{name: "ref, ready for an older spec", host: networkDataWaitHost(metav1.ConditionTrue, networkDataWaitGeneration-1), want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hostWaitingForNetworkData(tt.host); got != tt.want {
				t.Fatalf("hostWaitingForNetworkData = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestHandlePhysicalHostStateWaitsForNetworkData proves a claimed host whose
// network data cannot be served is not booted. The reconciler has no Redfish
// client factory, so reaching triggerInspection would fail the call.
func TestHandlePhysicalHostStateWaitsForNetworkData(t *testing.T) {
	r := &Beskar7MachineReconciler{Log: logr.Discard()}
	machine := &infrav1.Beskar7Machine{ObjectMeta: metav1.ObjectMeta{Namespace: networkDataTestNamespace, Name: "b7m"}}
	host := networkDataWaitHost(metav1.ConditionFalse, networkDataWaitGeneration)

	result, err := r.handlePhysicalHostState(context.Background(), logr.Discard(), machine, host)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != (ctrl.Result{RequeueAfter: 30 * time.Second}) {
		t.Fatalf("result = %+v, want a 30s requeue", result)
	}
	cond := conditions.Get(machine, infrav1.InfrastructureReadyCondition)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != infrav1.WaitingForNetworkDataReason {
		t.Fatalf("InfrastructureReady = %+v, want False/%s", cond, infrav1.WaitingForNetworkDataReason)
	}
	if machine.Status.Phase != nil && *machine.Status.Phase == infrav1.PhaseFailed {
		t.Fatal("the machine failed; it must wait")
	}
}
