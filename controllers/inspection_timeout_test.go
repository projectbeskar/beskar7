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
	"testing"
	"time"
)

func TestInspectionTimeoutResolver(t *testing.T) {
	cases := []struct {
		name string
		set  time.Duration
		want time.Duration
	}{
		{
			name: "zero falls back to the default",
			set:  0,
			want: DefaultInspectionTimeout,
		},
		{
			name: "negative falls back to the default",
			set:  -5 * time.Minute,
			want: DefaultInspectionTimeout,
		},
		{
			name: "positive value is honored",
			set:  30 * time.Minute,
			want: 30 * time.Minute,
		},
		{
			name: "small positive value is honored",
			set:  1 * time.Second,
			want: 1 * time.Second,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &Beskar7MachineReconciler{InspectionTimeout: tc.set}
			if got := r.inspectionTimeout(); got != tc.want {
				t.Errorf("inspectionTimeout() with InspectionTimeout=%v = %v, want %v", tc.set, got, tc.want)
			}
		})
	}
}
