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
	"strings"
	"testing"
)

func TestParseProviderID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		in        string
		wantNS    string
		wantName  string
		wantErr   bool
		errContains string
	}{
		{
			name:     "happy path",
			in:       "b7://default/worker-1",
			wantNS:   "default",
			wantName: "worker-1",
		},
		{
			name:        "missing prefix",
			in:          "provider://x/y",
			wantErr:     true,
			errContains: "b7://",
		},
		{
			name:        "empty rest after prefix",
			in:          "b7://",
			wantErr:     true,
			errContains: "b7://",
		},
		{
			name:        "no slash after prefix — single segment",
			in:          "b7://justname",
			wantErr:     true,
			errContains: "b7://",
		},
		{
			name:        "empty namespace — leading slash",
			in:          "b7:///name",
			wantErr:     true,
			errContains: "b7://",
		},
		{
			name:        "empty name — trailing slash",
			in:          "b7://ns/",
			wantErr:     true,
			errContains: "b7://",
		},
		{
			// BUG-3: the old hand-rolled loop returned ("ns", "name/extra", nil).
			name:        "multi-segment name contains slash",
			in:          "b7://ns/name/extra",
			wantErr:     true,
			errContains: "'/'",
		},
		{
			// Trailing whitespace in namespace: passes through as data. Kubernetes
			// would reject this name at admission, so we don't need a second gate here.
			name:     "namespace with trailing space — accepted as data",
			in:       "b7://ns /name",
			wantNS:   "ns ",
			wantName: "name",
		},
		{
			// Trailing newline in name: same reasoning — accept as data; the API server
			// enforces k8s name rules on write.
			name:     "name with trailing newline — accepted as data",
			in:       "b7://ns/name\n",
			wantNS:   "ns",
			wantName: "name\n",
		},
	}

	for _, tc := range tests {
		tc := tc // capture range variable
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotNS, gotName, err := parseProviderID(tc.in)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseProviderID(%q) returned nil error; want error containing %q",
						tc.in, tc.errContains)
				}
				if tc.errContains != "" && !strings.Contains(err.Error(), tc.errContains) {
					t.Fatalf("parseProviderID(%q) error = %q; want it to contain %q",
						tc.in, err.Error(), tc.errContains)
				}
				return
			}

			if err != nil {
				t.Fatalf("parseProviderID(%q) unexpected error: %v", tc.in, err)
			}
			if gotNS != tc.wantNS {
				t.Errorf("parseProviderID(%q) namespace = %q; want %q", tc.in, gotNS, tc.wantNS)
			}
			if gotName != tc.wantName {
				t.Errorf("parseProviderID(%q) name = %q; want %q", tc.in, gotName, tc.wantName)
			}
		})
	}
}
