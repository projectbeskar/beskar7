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
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrastructurev1beta1 "github.com/projectbeskar/beskar7/api/v1beta1"
)

const (
	// provisionedMaxBodyBytes caps the request body for the provisioned handler.
	// The inspector sends {"status":"provisioned"} (~22 bytes); 64 KiB is far above
	// that and far below any memory-pressure point. Body content is advisory only —
	// the authenticated POST itself is the signal (D-015).
	provisionedMaxBodyBytes = 64 << 10
)

// ProvisionedHandler handles POST /api/v1/provisioned/{namespace}/{hostName} from the
// inspector, signalling that OS deployment is complete.
//
// Authentication: callers must present "Authorization: Bearer <token>" with the same
// per-host bearer token used for inspection POST and bootstrap GET
// (newBearerTokenVerifier + auth.RequireBearer; D-004). ServeHTTP assumes the request
// has already passed the bearer middleware.
//
// Signal: the handler patches ProvisionedRequestAnnotation="provisioned" onto the
// PhysicalHost metadata. The PhysicalHostReconciler reads this on its next pass,
// transitions State from Deploying to Ready, and clears the annotation (D-015 / D-005
// pattern). This handler does NOT write PhysicalHost.Status directly.
type ProvisionedHandler struct {
	Client client.Client
	Log    logr.Logger
}

// ServeHTTP handles provisioning-complete callbacks from the inspector.
// It is invoked only after the bearer-auth middleware has validated the caller.
func (h *ProvisionedHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	namespace := r.PathValue("namespace")
	hostName := r.PathValue("hostName")
	log := h.Log.WithValues("method", r.Method, "namespace", namespace, "host", hostName, "remote", r.RemoteAddr)

	if r.Method != http.MethodPost {
		log.Info("Method not allowed", "method", r.Method)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Drain and discard the body. The body is advisory only (D-015 contract:
	// the POST itself is the signal). We cap the read to prevent a slow-loris
	// body keeping a goroutine alive for an unbounded time.
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, provisionedMaxBodyBytes)
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
	}

	ctx := r.Context()

	if err := h.signalProvisioned(ctx, log, namespace, hostName); err != nil {
		log.Error(err, "Failed to signal provisioned state")
		// Opaque error response — do not leak internal resource names or k8s details.
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	log.Info("Provisioned callback accepted; signalled reconciler via annotation")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	if _, err := w.Write([]byte(`{"status":"accepted"}`)); err != nil {
		log.V(1).Info("Failed to write response body", "err", err.Error())
	}
}

// signalProvisioned fetches the PhysicalHost and patches the ProvisionedRequestAnnotation
// so the PhysicalHostReconciler can drive the Deploying→Ready transition. It does NOT
// write PhysicalHost.Status (D-005 / BUG-1 invariant).
func (h *ProvisionedHandler) signalProvisioned(ctx context.Context, log logr.Logger, namespace, hostName string) error {
	ph := &infrastructurev1beta1.PhysicalHost{}
	key := types.NamespacedName{Namespace: namespace, Name: hostName}
	if err := h.Client.Get(ctx, key, ph); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("PhysicalHost %s/%s not found", namespace, hostName)
		}
		return fmt.Errorf("failed to get PhysicalHost: %w", err)
	}

	// Guard: only signal if the host is in Deploying. If it is already Ready
	// (idempotent duplicate POST) or in an unexpected state, log and return 202
	// without patching — the reconciler will handle it.
	if ph.Status.State != infrastructurev1beta1.StateDeploying && ph.Status.State != infrastructurev1beta1.StateReady {
		log.Info("Provisioned callback received but host is not in Deploying state; ignoring",
			"host", hostName, "state", ph.Status.State)
		return nil
	}

	base := ph.DeepCopy()
	if ph.Annotations == nil {
		ph.Annotations = map[string]string{}
	}
	ph.Annotations[ProvisionedRequestAnnotation] = "provisioned"

	// Plain MergeFrom (no optimistic lock). Same reasoning as
	// setBootstrapTokenAnnotation: this annotation key is unique to this handler,
	// no other writer collides on it. Optimistic lock caused repeated Conflict
	// failures under normal load in the inspection path; we apply the same
	// lesson here.
	if err := h.Client.Patch(ctx, ph, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("patch PhysicalHost provisioned annotation: %w", err)
	}
	log.V(1).Info("Provisioned annotation set", "host", hostName)
	return nil
}
