/*
Copyright 2026 The Beskar7 Authors.

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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infrastructurev1beta1 "github.com/projectbeskar/beskar7/api/v1beta1"
)

// TestReconcileDelete_ClaimedHost_NilRecorder is a regression test for #103:
// PhysicalHostReconciler was constructed in cmd/manager/main.go without a
// Recorder, so deleting a still-claimed host (ConsumerRef != nil) dereferenced
// a nil Recorder and panicked in reconcileDelete — before RemoveFinalizer ran,
// leaving the host stranded in Terminating.
//
// reconcileDelete touches no API server (it only mutates the in-memory object
// and emits an Event), so it can be exercised with a zero-value reconciler and
// no envtest. The reconciler here deliberately leaves Recorder nil to assert
// the guard added in the fix.
func TestReconcileDelete_ClaimedHost_NilRecorder(t *testing.T) {
	r := &PhysicalHostReconciler{} // Recorder intentionally nil

	ph := &infrastructurev1beta1.PhysicalHost{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "claimed-host",
			Namespace:  "default",
			Finalizers: []string{PhysicalHostFinalizer},
		},
		Spec: infrastructurev1beta1.PhysicalHostSpec{
			// A non-nil ConsumerRef is the condition that drives the
			// Recorder.Event call that used to panic.
			ConsumerRef: &corev1.ObjectReference{
				Kind:       "Beskar7Machine",
				Name:       "some-machine",
				Namespace:  "default",
				APIVersion: InfrastructureAPIVersion,
			},
		},
	}

	// Must not panic even though Recorder is nil.
	if _, err := r.reconcileDelete(context.Background(), log.Log, ph); err != nil {
		t.Fatalf("reconcileDelete returned error: %v", err)
	}

	// The finalizer must be removed so the deferred patch can let the object
	// finalize (the whole point — a panic here would strand it).
	for _, f := range ph.Finalizers {
		if f == PhysicalHostFinalizer {
			t.Fatalf("finalizer %q was not removed; host would be stranded in Terminating", PhysicalHostFinalizer)
		}
	}
}
