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

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "github.com/beskar7/beskar7/api/v1beta2"
)

const (
	cleaningClaimNamespace = "cleaning-claim"
	cleaningClaimMachine   = "cleaning-claim-machine"
	cleaningClaimUsable    = "usable"
)

// cleaningClaimHost returns an unclaimed host in state with spec.cleaning.mode Metadata.
func cleaningClaimHost(name, state string) *infrav1.PhysicalHost {
	return &infrav1.PhysicalHost{
		ObjectMeta: metav1.ObjectMeta{Namespace: cleaningClaimNamespace, Name: name},
		Spec:       infrav1.PhysicalHostSpec{Cleaning: &infrav1.CleaningSpec{Mode: infrav1.CleaningModeMetadata}},
		Status:     infrav1.PhysicalHostStatus{State: state},
	}
}

// heldAfterCleaning returns a host held in Error after a failed cleaning run.
func heldAfterCleaning(name string) *infrav1.PhysicalHost {
	h := cleaningClaimHost(name, infrav1.StateError)
	setFalse(h, infrav1.HostCleanedCondition, infrav1.CleaningFailedReason, "wipe failed")
	return h
}

// TestClaimSkipsHostBeingCleaned proves a machine never claims a host that is
// being cleaned or is held after a failed cleaning run, and takes a usable host instead.
func TestClaimSkipsHostBeingCleaned(t *testing.T) {
	tests := []struct {
		name      string
		hosts     []client.Object
		wantClaim string
	}{
		{name: "only a Cleaning host", hosts: []client.Object{cleaningClaimHost("cleaning", infrav1.StateCleaning)}},
		{name: "only a host held after a failed run", hosts: []client.Object{heldAfterCleaning("held")}},
		{
			name: "a Cleaning, a held and a usable host",
			hosts: []client.Object{
				cleaningClaimHost("a-cleaning", infrav1.StateCleaning),
				heldAfterCleaning("b-held"),
				cleaningClaimHost(cleaningClaimUsable, infrav1.StateAvailable),
			},
			wantClaim: cleaningClaimUsable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := newSchemeForTest(t)
			machine := &infrav1.Beskar7Machine{
				ObjectMeta: metav1.ObjectMeta{Namespace: cleaningClaimNamespace, Name: cleaningClaimMachine},
			}
			c := fake.NewClientBuilder().WithScheme(scheme).
				WithObjects(append(tt.hosts, machine)...).
				WithStatusSubresource(&infrav1.PhysicalHost{}).
				WithIndex(&infrav1.PhysicalHost{}, PhysicalHostStateIndex, func(o client.Object) []string {
					return []string{o.(*infrav1.PhysicalHost).Status.State}
				}).Build()
			r := &Beskar7MachineReconciler{Client: c, Scheme: scheme, Log: logr.Discard()}

			host, _, err := r.findAndClaimOrGetAssociatedHost(context.Background(), logr.Discard(), machine, nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantClaim == "" {
				if host != nil {
					t.Fatalf("claimed %s, want no claim", host.Name)
				}
				return
			}
			if host == nil || host.Name != tt.wantClaim {
				t.Fatalf("claimed %v, want %s", host, tt.wantClaim)
			}
		})
	}
}

// TestHostClaimableIgnoresCleaningStates proves a cleaning or held host never
// wakes a waiting machine, and the same host does once it is Available.
func TestHostClaimableIgnoresCleaningStates(t *testing.T) {
	for _, h := range []*infrav1.PhysicalHost{
		cleaningClaimHost("cleaning", infrav1.StateCleaning),
		heldAfterCleaning("held"),
	} {
		if hostClaimable(h) {
			t.Fatalf("host in %s is claimable, want not", h.Status.State)
		}
	}
	if !hostClaimable(cleaningClaimHost("cleaned", infrav1.StateAvailable)) {
		t.Fatal("cleaned Available host is not claimable, want claimable")
	}
}
