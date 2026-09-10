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
	"strings"
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
			in:   "default,capb7-system,rack-1",
			want: []string{"capb7-system", "default", "rack-1"},
		},
		{
			name: "whitespace around entries is trimmed",
			in:   " default , capb7-system , rack-1 ",
			want: []string{"capb7-system", "default", "rack-1"},
		},
		{
			name: "duplicates collapsed",
			in:   "default,default,capb7-system,default",
			want: []string{"capb7-system", "default"},
		},
		{
			name: "trailing/leading commas ignored",
			in:   ",default,capb7-system,",
			want: []string{"capb7-system", "default"},
		},
		{
			name: "consecutive commas ignored",
			in:   "default,,,capb7-system",
			want: []string{"capb7-system", "default"},
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

func TestParseControllersMode(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    controllersMode
		wantErr bool
	}{
		{name: "all", in: "all", want: controllersAll},
		{name: "none", in: "none", want: controllersNone},
		{name: "case and whitespace are ignored", in: "  NONE ", want: controllersNone},
		{name: "empty is rejected", in: "", wantErr: true},
		{name: "typo is rejected", in: "nonee", wantErr: true},
		{name: "a controller list is not supported", in: "physicalhost,beskar7machine", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseControllersMode(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseControllersMode(%q) = %q, want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseControllersMode(%q): unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("parseControllersMode(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestManagerConfigValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     managerConfig
		wantErr string // substring of the expected error; empty means valid
	}{
		{
			name: "all mode accepts the defaults",
			cfg:  managerConfig{controllers: controllersAll, enableLeaderElection: true},
		},
		{
			name: "all mode accepts the webhook",
			cfg:  managerConfig{controllers: controllersAll, enableLeaderElection: true, enableWebhook: true},
		},
		{
			name: "callback-only with the leader-elect default is fine",
			cfg:  managerConfig{controllers: controllersNone, enableLeaderElection: true},
		},
		{
			name: "callback-only with an explicit --leader-elect=false is fine",
			cfg:  managerConfig{controllers: controllersNone, enableLeaderElection: false, leaderElectSet: true},
		},
		{
			name:    "callback-only rejects an explicit --leader-elect=true",
			cfg:     managerConfig{controllers: controllersNone, enableLeaderElection: true, leaderElectSet: true},
			wantErr: "--leader-elect=true contradicts --controllers=none",
		},
		{
			name:    "callback-only rejects the webhook",
			cfg:     managerConfig{controllers: controllersNone, enableWebhook: true},
			wantErr: "--enable-webhook=true contradicts --controllers=none",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validate() = nil, want an error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("validate() = %q, want it to contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestManagerConfigLeaderElection(t *testing.T) {
	cases := []struct {
		name string
		cfg  managerConfig
		want bool
	}{
		{name: "all mode keeps the flag on", cfg: managerConfig{controllers: controllersAll, enableLeaderElection: true}, want: true},
		{name: "all mode keeps the flag off", cfg: managerConfig{controllers: controllersAll, enableLeaderElection: false}, want: false},
		{name: "callback-only forces it off even at the default", cfg: managerConfig{controllers: controllersNone, enableLeaderElection: true}, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.leaderElection(); got != tc.want {
				t.Errorf("leaderElection() = %v, want %v", got, tc.want)
			}
		})
	}
}
