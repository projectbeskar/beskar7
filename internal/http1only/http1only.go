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

// Package http1only keeps the manager's HTTPS servers (the host-callback
// endpoint, the webhook and the metrics endpoint) on HTTP/1.1.
//
// No client of those servers needs HTTP/2: the inspector's ureq client and
// iPXE speak HTTP/1.1 only, the kube-apiserver forces http/1.1 for its
// admission webhook calls, and Prometheus offers h2 as an option that ALPN
// resolves to http/1.1 when the server advertises nothing else. HTTP/2 would
// only add attack surface, reachable before any authentication: its
// server-side flow control, HPACK and trailer handling have been the source of
// repeated denial-of-service advisories (rapid reset, GO-2026-6603, -6611,
// -6612, -6617).
package http1only

import (
	"crypto/tls"
	"net/http"
)

// TLSConfig makes c advertise only http/1.1 in ALPN, so a client can never
// negotiate h2. It has the signature of a controller-runtime TLSOpts function,
// which is how the webhook and metrics servers take it.
func TLSConfig(c *tls.Config) {
	c.NextProtos = []string{"http/1.1"}
}

// Server restricts an http.Server that terminates its own TLS, through
// ServeTLS or ListenAndServeTLS, to HTTP/1.1.
//
// Advertising http/1.1 is not enough for such a server: net/http installs its
// HTTP/2 handler and puts "h2" back into the ALPN list unless Protocols (or
// TLSNextProto) rules it out, so a client that offers only h2 would still get
// it. Server sets both.
func Server(s *http.Server) {
	if s.TLSConfig == nil {
		s.TLSConfig = &tls.Config{}
	}
	TLSConfig(s.TLSConfig)

	s.Protocols = new(http.Protocols)
	s.Protocols.SetHTTP1(true)
}
