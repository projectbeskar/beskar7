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

import "testing"

// TestMaxConcurrentOrDefault pins the normalisation rule the three
// SetupWithManager implementations rely on: an unset (zero) or nonsensical
// worker count must collapse to the historical single-worker behaviour rather
// than to controller-runtime's "0 means default" implicit handling, so the
// value we pass is always explicit and auditable.
func TestMaxConcurrentOrDefault(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   int
		want int
	}{
		{"unset zero value falls back to the default", 0, DefaultMaxConcurrentReconciles},
		{"negative is treated as unset", -1, DefaultMaxConcurrentReconciles},
		{"large negative is treated as unset", -9999, DefaultMaxConcurrentReconciles},
		{"one is preserved", 1, 1},
		{"explicit higher value is preserved", 8, 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := maxConcurrentOrDefault(tc.in); got != tc.want {
				t.Errorf("maxConcurrentOrDefault(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestDefaultMaxConcurrentReconcilesMatchesControllerRuntime documents why the
// default is 1: it is the value controller-runtime itself uses, so adding this
// flag is a no-op for every existing deployment. A change here is a behaviour
// change for operators who never set the flag, and should be deliberate.
func TestDefaultMaxConcurrentReconcilesMatchesControllerRuntime(t *testing.T) {
	if DefaultMaxConcurrentReconciles != 1 {
		t.Fatalf("DefaultMaxConcurrentReconciles = %d, want 1 — raising the default "+
			"silently changes concurrency for deployments that never set the flag",
			DefaultMaxConcurrentReconciles)
	}
}
