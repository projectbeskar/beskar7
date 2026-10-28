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
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta/testrestmapper"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
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

// spyReader is a cache stand-in that counts the reads sent to it.
type spyReader struct{ reads int }

func (s *spyReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	s.reads++
	return nil
}

func (s *spyReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	s.reads++
	return nil
}

// TestManagerConfigClientOptions builds a client from the options each mode
// hands the manager and sees where its reads go. A callback-only instance reads
// a Secret, singly or as a list, straight from the API server and every other
// kind from the cache; the full manager's reconcilers watch Secrets, so it
// caches those too (CALLBACK-SECRET-READ). The API server here is a closed
// port: a read that is not served by the cache fails on the connection.
func TestManagerConfigClientOptions(t *testing.T) {
	cases := []struct {
		name       string
		mode       controllersMode
		wantCached map[string]bool
	}{
		{
			name:       "callback-only reads Secrets live",
			mode:       controllersNone,
			wantCached: map[string]bool{"Secret": false, "SecretList": false, "ConfigMap": true, "PhysicalHost": true},
		},
		{
			name:       "the full manager caches Secrets",
			mode:       controllersAll,
			wantCached: map[string]bool{"Secret": true, "SecretList": true, "ConfigMap": true, "PhysicalHost": true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := managerConfig{controllers: tc.mode}.clientOptions()
			opts.Scheme = scheme
			opts.Mapper = testrestmapper.TestOnlyStaticRESTMapper(scheme)
			spy := &spyReader{}
			if opts.Cache == nil {
				opts.Cache = &client.CacheOptions{}
			}
			opts.Cache.Reader = spy
			c, err := client.New(&rest.Config{Host: "http://127.0.0.1:1", Timeout: 2 * time.Second}, opts)
			if err != nil {
				t.Fatalf("create client: %v", err)
			}

			reads := map[string]func() error{
				"Secret": func() error {
					return c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "x"}, &corev1.Secret{})
				},
				"SecretList": func() error { return c.List(context.Background(), &corev1.SecretList{}, client.InNamespace("ns")) },
				"ConfigMap": func() error {
					return c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "x"}, &corev1.ConfigMap{})
				},
				"PhysicalHost": func() error {
					return c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "x"}, &infrav1.PhysicalHost{})
				},
			}
			for kind, read := range reads {
				before := spy.reads
				err := read()
				if cached := spy.reads == before+1; cached != tc.wantCached[kind] {
					t.Errorf("%s read served by the cache = %t, want %t", kind, cached, tc.wantCached[kind])
				}
				if !tc.wantCached[kind] && err == nil {
					t.Errorf("%s read did not reach the API server", kind)
				}
			}
		})
	}
}
