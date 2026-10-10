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
	"crypto/tls"

	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	"github.com/projectbeskar/beskar7/internal/http1only"
)

// buildWebhookOptions returns the webhook server's options. The server speaks
// HTTP/1.1 only: controller-runtime starts its TLS config at
// NextProtos=["h2"], and the TLSOpts run after that default.
func buildWebhookOptions(port int, certDir string) webhook.Options {
	return webhook.Options{
		Port:    port,
		CertDir: certDir,
		TLSOpts: []func(*tls.Config){http1only.TLSConfig},
	}
}

// buildMetricsOptions returns the metrics server's options. When secure, it
// serves HTTPS and authenticates and authorizes /metrics through TokenReview
// and SubjectAccessReview delegated to the kube-apiserver. That needs the
// manager ServiceAccount to hold the authentication.k8s.io:tokenreviews and
// authorization.k8s.io:subjectaccessreviews create verbs (see
// config/rbac/metrics_auth_role.yaml). Like the webhook it speaks HTTP/1.1
// only; TLSOpts are ignored by the plain-HTTP listener of the insecure mode.
func buildMetricsOptions(addr string, secure bool) metricsserver.Options {
	opts := metricsserver.Options{
		BindAddress:   addr,
		SecureServing: secure,
		TLSOpts:       []func(*tls.Config){http1only.TLSConfig},
	}
	if secure {
		opts.FilterProvider = filters.WithAuthenticationAndAuthorization
	}
	return opts
}
