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

package main

import (
	"reflect"
	"testing"
)

func TestParseWatchNamespaces(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "empty string → nil (watch all namespaces)",
			in:   "",
			want: nil,
		},
		{
			name: "whitespace-only → nil",
			in:   "   \t  ",
			want: nil,
		},
		{
			name: "single namespace",
			in:   "default",
			want: []string{"default"},
		},
		{
			name: "multiple namespaces",
			in:   "default,beskar7-system,rack-1",
			want: []string{"beskar7-system", "default", "rack-1"},
		},
		{
			name: "whitespace around entries is trimmed",
			in:   " default , beskar7-system , rack-1 ",
			want: []string{"beskar7-system", "default", "rack-1"},
		},
		{
			name: "duplicates collapsed",
			in:   "default,default,beskar7-system,default",
			want: []string{"beskar7-system", "default"},
		},
		{
			name: "trailing/leading commas ignored",
			in:   ",default,beskar7-system,",
			want: []string{"beskar7-system", "default"},
		},
		{
			name: "consecutive commas ignored",
			in:   "default,,,beskar7-system",
			want: []string{"beskar7-system", "default"},
		},
		{
			name: "single comma → nil",
			in:   ",",
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseWatchNamespaces(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseWatchNamespaces(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
