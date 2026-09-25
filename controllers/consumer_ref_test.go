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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
)

func TestResolveConsumerBeskar7Machine(t *testing.T) {
	t.Parallel()

	host := func(cr *corev1.ObjectReference) *infrav1.PhysicalHost {
		return &infrav1.PhysicalHost{
			ObjectMeta: metav1.ObjectMeta{Name: "h1", Namespace: "team-a"},
			Spec:       infrav1.PhysicalHostSpec{ConsumerRef: cr},
		}
	}

	tests := []struct {
		name    string
		cr      *corev1.ObjectReference
		wantKey types.NamespacedName
		wantOK  bool
	}{
		{
			name:   "nil ConsumerRef",
			cr:     nil,
			wantOK: false,
		},
		{
			name: "same-namespace ConsumerRef",
			cr: &corev1.ObjectReference{
				Kind: "Beskar7Machine", APIVersion: InfrastructureAPIVersion,
				Name: "m1", Namespace: "team-a",
			},
			wantKey: types.NamespacedName{Namespace: "team-a", Name: "m1"},
			wantOK:  true,
		},
		{
			// A ConsumerRef built before the machine controller always set
			// Namespace explicitly (or hand-authored) is treated as implicitly
			// same-namespace, not rejected.
			name: "empty-namespace ConsumerRef treated as same-namespace",
			cr: &corev1.ObjectReference{
				Kind: "Beskar7Machine", APIVersion: InfrastructureAPIVersion,
				Name: "m1", Namespace: "",
			},
			wantKey: types.NamespacedName{Namespace: "team-a", Name: "m1"},
			wantOK:  true,
		},
		{
			// SEC-12: the core regression case. A ConsumerRef naming a different
			// namespace must resolve to no consumer, not to a cross-namespace Get.
			name: "cross-namespace ConsumerRef rejected",
			cr: &corev1.ObjectReference{
				Kind: "Beskar7Machine", APIVersion: InfrastructureAPIVersion,
				Name: "m1", Namespace: "team-b",
			},
			wantOK: false,
		},
		{
			name: "wrong Kind rejected",
			cr: &corev1.ObjectReference{
				Kind: "Machine", APIVersion: InfrastructureAPIVersion,
				Name: "m1", Namespace: "team-a",
			},
			wantOK: false,
		},
		{
			name: "wrong APIVersion rejected",
			cr: &corev1.ObjectReference{
				Kind: "Beskar7Machine", APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1",
				Name: "m1", Namespace: "team-a",
			},
			wantOK: false,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotKey, gotOK := resolveConsumerBeskar7Machine(host(tc.cr))
			if gotOK != tc.wantOK {
				t.Fatalf("resolveConsumerBeskar7Machine() ok = %v; want %v", gotOK, tc.wantOK)
			}
			if tc.wantOK && gotKey != tc.wantKey {
				t.Errorf("resolveConsumerBeskar7Machine() key = %+v; want %+v", gotKey, tc.wantKey)
			}
		})
	}
}
