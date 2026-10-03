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
	"net/url"
	"reflect"
	"strings"
	"testing"
)

// D-035: --bmc-proxy is the only way BMC connections use a proxy.

func TestParseBMCProxy(t *testing.T) {
	t.Run("empty means direct", func(t *testing.T) {
		for _, in := range []string{"", "   ", "\t\n"} {
			got, err := parseBMCProxy(in)
			if err != nil || got != nil {
				t.Errorf("parseBMCProxy(%q) = %v, %v; want nil, nil", in, got, err)
			}
		}
	})

	t.Run("valid values", func(t *testing.T) {
		cases := []struct {
			in         string
			wantScheme string
			wantHost   string
		}{
			{"http://proxy.corp:3128", "http", "proxy.corp:3128"},
			{"https://proxy.corp", "https", "proxy.corp"},
			{"http://proxy.corp:3128/", "http", "proxy.corp:3128"},
			{"  http://proxy.corp:3128  ", "http", "proxy.corp:3128"},
			{"HTTP://Proxy.Corp:3128", "http", "Proxy.Corp:3128"},
			{"http://10.0.0.1:3128", "http", "10.0.0.1:3128"},
			{"http://[2001:db8::1]:3128", "http", "[2001:db8::1]:3128"},
			{"http://proxy.corp:65535", "http", "proxy.corp:65535"},
			// Userinfo is how a proxy that wants credentials is named.
			{"http://operator:pw-9c1d@proxy.corp:3128", "http", "proxy.corp:3128"},
			{"https://operator@proxy.corp", "https", "proxy.corp"},
		}
		for _, tc := range cases {
			got, err := parseBMCProxy(tc.in)
			if err != nil {
				t.Errorf("parseBMCProxy(%q): unexpected error: %v", tc.in, err)
				continue
			}
			if got == nil || got.Scheme != tc.wantScheme || got.Host != tc.wantHost {
				t.Errorf("parseBMCProxy(%q) = %v, want scheme %q host %q", tc.in, got, tc.wantScheme, tc.wantHost)
			}
		}
	})

	t.Run("userinfo is kept", func(t *testing.T) {
		got := mustParseBMCProxy(t, "http://operator:pw-9c1d@proxy.corp:3128")
		if pw, _ := got.User.Password(); got.User.Username() != "operator" || pw != "pw-9c1d" {
			t.Errorf("the proxy credentials were not kept")
		}
	})

	// Every rejected value carries a user name and a password, the way a real
	// misconfigured one would, and no error may repeat either, or the value, or
	// any part of it.
	rejected := []struct{ name, in, want string }{
		{"a scheme other than http or https", "socks5://u-7f3a:pw-9c1d@proxy.corp:1080", "scheme must be http or https"},
		{"ftp", "ftp://u-7f3a:pw-9c1d@proxy.corp:21", "scheme must be http or https"},
		{"no scheme: the user name parses as one", "u-7f3a:pw-9c1d@proxy.corp:3128", "scheme must be http or https"},
		{"no scheme: host and port", "proxy.corp:3128", "scheme must be http or https"},
		{"no scheme: an IP and a port", "10.0.0.1:3128", "not a URL"},
		{"no host", "http://u-7f3a:pw-9c1d@:3128", "host is required"},
		{"nothing after the userinfo", "http://u-7f3a:pw-9c1d@", "host is required"},
		{"only userinfo", "http://u-7f3a:pw-9c1d@/", "host is required"},
		{"a path", "http://u-7f3a:pw-9c1d@proxy.corp:3128/pac", "path, query or fragment"},
		{"a query", "http://u-7f3a:pw-9c1d@proxy.corp:3128/?x=1", "path, query or fragment"},
		{"a query without a path", "http://u-7f3a:pw-9c1d@proxy.corp:3128?x=1", "path, query or fragment"},
		{"an empty query", "http://u-7f3a:pw-9c1d@proxy.corp:3128/?", "path, query or fragment"},
		{"a fragment", "http://u-7f3a:pw-9c1d@proxy.corp:3128/#f", "path, query or fragment"},
		{"an empty fragment", "http://u-7f3a:pw-9c1d@proxy.corp:3128#", "path, query or fragment"},
		{"port zero", "http://u-7f3a:pw-9c1d@proxy.corp:0", "port must be between 1 and 65535"},
		{"port too large", "http://u-7f3a:pw-9c1d@proxy.corp:65536", "port must be between 1 and 65535"},
		{"a space in the host", "http://u-7f3a:pw-9c1d@pro xy.corp:3128", "not a URL"},
		{"a bad escape in the password", "http://u-7f3a:pw%zz@proxy.corp:3128", "not a URL"},
		{"an unterminated IPv6 literal", "http://u-7f3a:pw-9c1d@[::1:3128", "not a URL"},
		{"a non-numeric port", "http://u-7f3a:pw-9c1d@proxy.corp:http", "not a URL"},
	}
	for _, tc := range rejected {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			got, err := parseBMCProxy(tc.in)
			if err == nil {
				t.Fatalf("parseBMCProxy(%q) = %v, want an error", tc.in, got)
			}
			msg := err.Error()
			if !strings.Contains(msg, "--bmc-proxy") || !strings.Contains(msg, tc.want) {
				t.Errorf("error %q should name --bmc-proxy and contain %q", msg, tc.want)
			}
			for _, secret := range []string{"u-7f3a", "pw-9c1d", "pw%zz", tc.in} {
				if strings.Contains(msg, secret) {
					t.Errorf("the error repeats %q of the value: %s", secret, msg)
				}
			}
		})
	}
}

func TestManagerConfigValidateBMCProxy(t *testing.T) {
	proxy := mustParseBMCProxy(t, "http://operator:pw-9c1d@proxy.corp:3128")

	for _, tc := range []struct {
		name    string
		cfg     managerConfig
		wantErr string
	}{
		{name: "all mode accepts a BMC proxy", cfg: managerConfig{controllers: controllersAll, enableLeaderElection: true, bmcProxy: proxy}},
		{name: "all mode accepts none", cfg: managerConfig{controllers: controllersAll, enableLeaderElection: true}},
		{name: "callback-only is fine without one", cfg: managerConfig{controllers: controllersNone}},
		{
			name:    "callback-only rejects a BMC proxy",
			cfg:     managerConfig{controllers: controllersNone, bmcProxy: proxy},
			wantErr: "--bmc-proxy contradicts --controllers=none",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validate() = %v, want an error containing %q", err, tc.wantErr)
			}
			// The URL may hold the proxy's password; the error names the flag only.
			for _, leaked := range []string{"operator", "pw-9c1d", "proxy.corp", "3128"} {
				if strings.Contains(err.Error(), leaked) {
					t.Errorf("the error repeats %q: %s", leaked, err)
				}
			}
		})
	}
}

func mustParseBMCProxy(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := parseBMCProxy(raw)
	if err != nil || u == nil {
		t.Fatalf("parseBMCProxy(%q) = %v, %v", raw, u, err)
	}
	return u
}

func TestProxyEnvironmentVariables(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}

	if got := proxyEnvironmentVariables(env(nil)); len(got) != 0 {
		t.Errorf("nothing set: got %v", got)
	}
	got := proxyEnvironmentVariables(env(map[string]string{
		"HTTPS_PROXY": "http://operator:pw-9c1d@proxy.corp:3128",
		"http_proxy":  "http://proxy.corp:3128",
		"HTTP_PROXY":  "", // set but empty: net/http ignores it too
		"NO_PROXY":    "10.0.0.0/8",
		"ALL_PROXY":   "http://proxy.corp:3128", // net/http does not read it
	}))
	want := []string{"http_proxy", "HTTPS_PROXY"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
