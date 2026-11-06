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
	"net/http"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// NetworkDataHandler serves the network data Secret bytes of a claimed
// PhysicalHost (contract v4.3 §4.6).
//
// Authentication is the same as /bootstrap: the bearer middleware, then
// boundCallbackHost, so only a token bound to the machine that claims the host
// is served.
//
// Responses:
//   - 200: the "networkData" bytes, verbatim, never parsed.
//   - 401: boundCallbackHost refused the request: the host is gone, its claim
//     moved or its token changed since the bearer check. Unlike /bootstrap,
//     this is not a 404, because a 404 here installs the host without its
//     network.
//   - 404: only when the host has no spec.networkDataSecretRef. A controller
//     before v4.3 answers 404 too, so the inspector reads both as "no network
//     data".
//   - 409: the ref is set but the Secret is missing, not opted in or invalid.
//     The host must not be installed without the network it was given, so the
//     inspector fails the deploy. NetworkDataReady on the host says why.
//   - 500: the host, its credentials or the Secret could not be read. The
//     inspector retries it.
type NetworkDataHandler struct {
	Client client.Client
	// APIReader reads the network data Secret straight from the API server, so
	// a Secret that just lost its opt-in is never served from a stale cache
	// (§4.6). Unset, the handler reads through Client.
	APIReader client.Reader
	Log       logr.Logger
}

// secretReader returns the reader the network data Secret is checked with.
func (h *NetworkDataHandler) secretReader() client.Reader {
	if h.APIReader != nil {
		return h.APIReader
	}
	return h.Client
}

// ServeHTTP serves the network data for the host identified by the URL path.
func (h *NetworkDataHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log := h.Log.WithValues("method", r.Method, "namespace", r.PathValue("namespace"), "host", r.PathValue("hostName"), "remote", r.RemoteAddr)

	ph, _, err := boundCallbackHost(h.Client, r)
	if err != nil && callbackReadFailed(err) {
		log.Error(err, "network-data GET: host could not be resolved")
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if err != nil {
		log.V(1).Info("network-data GET: refused", "err", err.Error())
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if ph.Spec.NetworkDataSecretRef == "" {
		log.V(1).Info("network-data GET: host has no network data")
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	data, err := loadNetworkData(r.Context(), h.secretReader(), ph)
	if err != nil && networkDataReason(err) != "" {
		log.Info("network-data GET: network data Secret refused", "err", err.Error())
		http.Error(w, "conflict", http.StatusConflict)
		return
	}
	if err != nil {
		log.Error(err, "network-data GET: network data Secret could not be read")
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// The bytes and any digest of them are never logged.
	if err := writeNoStore(w, data); err != nil {
		log.V(1).Info("network-data GET: response write failed", "err", err.Error())
		return
	}
	log.V(1).Info("network-data GET: served network data")
}
