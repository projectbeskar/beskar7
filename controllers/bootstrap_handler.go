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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrastructurev1beta1 "github.com/projectbeskar/beskar7/api/v1beta1"
)

// bootstrapDataSecretKey is the canonical key under which CAPI bootstrap
// providers store user-data in the bootstrap Secret. Defined by the CAPI
// bootstrap-provider contract.
const bootstrapDataSecretKey = "value"

// maxBootstrapDataSize is a defensive cap on bootstrap user-data size. Real
// kubeadm / cloud-init payloads are well under 64 KiB; anything above 1 MiB is
// a sign of operator misconfiguration (e.g. an entire image baked into the
// secret) and we refuse to serve it. The 1 MiB cap also matches
// inspectionMaxBodyBytes — same envelope on both sides of the callback server.
const maxBootstrapDataSize = 1 << 20

// BootstrapHandler serves the bootstrap data Secret bytes for the
// Beskar7Machine that consumes a given PhysicalHost.
//
// Authentication: callers must present "Authorization: Bearer <token>" with a
// token whose SHA-256 matches the targeted PhysicalHost's
// Status.Bootstrap.TokenHash and whose ExpiresAt is in the future. The bearer
// middleware (auth.RequireBearer + newBearerTokenVerifier) enforces this
// before ServeHTTP is invoked. The same bearer token authorises the inspection
// POST and the bootstrap GET on the same host — by design (D-004).
//
// Resolution chain (handler internals):
//
//	PhysicalHost(ns,host)
//	  └─ Spec.ConsumerRef → Beskar7Machine
//	       └─ OwnerReferences → cluster.x-k8s.io/Machine
//	            └─ Spec.Bootstrap.DataSecretName → Secret
//	                 └─ data["value"] → response body
//
// Every failure path returns the same opaque "404 not found" body so that
// callers cannot use the response to distinguish "host doesn't exist" from
// "host has no consumer" from "secret missing" from "secret has no value
// key". Specific reasons are logged at V(1) only. The single 500 case
// (oversize secret) is operator-fault, not host-fault.
type BootstrapHandler struct {
	Client client.Client
	Log    logr.Logger
}

// ServeHTTP serves the bootstrap data Secret bytes for the host identified by
// the URL path. Authentication has already been validated by the bearer
// middleware before this method runs.
func (h *BootstrapHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	namespace := r.PathValue("namespace")
	hostName := r.PathValue("hostName")
	log := h.Log.WithValues("method", r.Method, "namespace", namespace, "host", hostName, "remote", r.RemoteAddr)

	// Defensive: the bearer middleware's verifier already validated non-empty
	// path values before this handler runs. Re-checking here keeps this method
	// safe to call directly in tests against an httptest.Server bound to the
	// same path pattern.
	if namespace == "" || hostName == "" {
		log.V(1).Info("bootstrap GET: missing namespace or hostName in request path")
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	ctx := r.Context()

	// 1. Get PhysicalHost. The bearer verifier already did the same Get for
	// auth purposes, but we re-Get here to read Spec.ConsumerRef from the same
	// resourceVersion as the rest of the chain walk.
	ph := &infrastructurev1beta1.PhysicalHost{}
	if err := h.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: hostName}, ph); err != nil {
		log.V(1).Info("bootstrap GET: PhysicalHost lookup failed", "err", err.Error())
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// 2. Walk to the Beskar7Machine via Spec.ConsumerRef.
	cr := ph.Spec.ConsumerRef
	if cr == nil || cr.Kind != "Beskar7Machine" || cr.APIVersion != InfrastructureAPIVersion {
		log.V(1).Info("bootstrap GET: PhysicalHost has no Beskar7Machine consumer")
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	b7m := &infrastructurev1beta1.Beskar7Machine{}
	if err := h.Client.Get(ctx, types.NamespacedName{Namespace: cr.Namespace, Name: cr.Name}, b7m); err != nil {
		log.V(1).Info("bootstrap GET: Beskar7Machine lookup failed", "err", err.Error())
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// 3. Walk to the owner CAPI Machine via OwnerReferences.
	machine, err := util.GetOwnerMachine(ctx, h.Client, b7m.ObjectMeta)
	if err != nil {
		log.V(1).Info("bootstrap GET: owner Machine lookup failed", "err", err.Error())
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if machine == nil {
		log.V(1).Info("bootstrap GET: Beskar7Machine has no owner Machine yet")
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if machine.Spec.Bootstrap.DataSecretName == nil {
		log.V(1).Info("bootstrap GET: owner Machine has no Spec.Bootstrap.DataSecretName")
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// 4. Fetch the bootstrap data Secret. CAPI convention places it in the
	// owner Machine's namespace, which equals the Beskar7Machine's namespace
	// (CAPI controllers reject cross-namespace bootstrap-data secrets).
	secret := &corev1.Secret{}
	secretKey := types.NamespacedName{Namespace: b7m.Namespace, Name: *machine.Spec.Bootstrap.DataSecretName}
	if err := h.Client.Get(ctx, secretKey, secret); err != nil {
		log.V(1).Info("bootstrap GET: bootstrap data Secret lookup failed", "err", err.Error())
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// 5. Extract the bytes.
	data, ok := secret.Data[bootstrapDataSecretKey]
	if !ok || len(data) == 0 {
		log.V(1).Info("bootstrap GET: bootstrap data Secret has no usable 'value' key")
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// 6. Sanity cap. >1 MiB of bootstrap user-data is operator-fault, not
	// host-fault: differentiate this case with 500 so an oncall sees it
	// distinctly from the 404 chain-walk failures. We never echo the size to
	// the client.
	if len(data) > maxBootstrapDataSize {
		log.Error(nil, "bootstrap GET: bootstrap data Secret exceeds size cap",
			"limit", maxBootstrapDataSize, "size", len(data))
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// 7. Serve the bytes. Cache-Control: no-store so reverse proxies and
	// HTTP-cache-aware clients do not retain bootstrap data on disk. We do
	// not log the bytes nor any digest of them — the manager logs say only
	// that a bootstrap fetch happened, not what it returned.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(data); err != nil {
		// Client disconnect mid-write is the typical case. V(1) — not actionable
		// at INFO. Do not include any bytes-related detail in the log.
		log.V(1).Info("bootstrap GET: response write failed", "err", err.Error())
		return
	}
	log.V(1).Info("bootstrap GET: served bootstrap data")
}
