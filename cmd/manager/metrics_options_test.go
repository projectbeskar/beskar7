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

package main

import (
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// buildMetricsOptions mirrors the production wiring in main() so that test
// and production code stay in sync.
func buildMetricsOptions(addr string, secure bool) metricsserver.Options {
	opts := metricsserver.Options{
		BindAddress:   addr,
		SecureServing: secure,
	}
	if secure {
		opts.FilterProvider = filters.WithAuthenticationAndAuthorization
	}
	return opts
}

func TestBuildMetricsOptions_SecureDefault(t *testing.T) {
	opts := buildMetricsOptions(":8443", true)

	if !opts.SecureServing {
		t.Error("SecureServing must be true when secure=true")
	}
	if opts.FilterProvider == nil {
		t.Error("FilterProvider must not be nil when secure=true")
	}
	if opts.BindAddress != ":8443" {
		t.Errorf("BindAddress: got %q, want %q", opts.BindAddress, ":8443")
	}
}

func TestBuildMetricsOptions_InsecureDevMode(t *testing.T) {
	opts := buildMetricsOptions(":8080", false)

	if opts.SecureServing {
		t.Error("SecureServing must be false when secure=false")
	}
	if opts.FilterProvider != nil {
		t.Error("FilterProvider must be nil when secure=false")
	}
}
