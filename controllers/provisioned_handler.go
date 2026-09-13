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

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
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
// pattern) — at once for a host that is Deploying, and only once it is Deploying for a
// host that is still Inspecting (see signalProvisioned). This handler does NOT write
// PhysicalHost.Status directly.
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
// for the PhysicalHostReconciler, which decides what becomes of the report
// (applyProvisionedRequestAnnotation). It does NOT write PhysicalHost.Status (D-005 /
// BUG-1 invariant).
//
// The annotation is set on a host the report can be about:
//   - Deploying: the expected case.
//   - Ready: a duplicate, which the reconciler clears.
//   - Claimed and still Inspecting. The inspector deploys as soon as /bootstrap answers
//     (contract §9.2) and does not wait for the host, which goes to Deploying only once
//     the Beskar7Machine has validated the inspection report: when the controllers were
//     away while a callback-only instance kept answering the inspector, the whole
//     deployment can finish first. The reconciler keeps the report until the host is
//     Deploying. Any inspection phase is accepted: this
//     read may come from a cache that predates the inspection report, while the
//     reconciler reads the annotation from a copy of the host at least as new as the
//     annotation and tells a report that followed this run's inspection report from one
//     that did not.
//
// Any other state is logged and ignored; the inspector still gets its 202.
func (h *ProvisionedHandler) signalProvisioned(ctx context.Context, log logr.Logger, namespace, hostName string) error {
	ph := &infrav1.PhysicalHost{}
	key := types.NamespacedName{Namespace: namespace, Name: hostName}
	if err := h.Client.Get(ctx, key, ph); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("PhysicalHost %s/%s not found", namespace, hostName)
		}
		return fmt.Errorf("failed to get PhysicalHost: %w", err)
	}

	switch state := ph.Status.State; {
	case state == infrav1.StateDeploying, state == infrav1.StateReady:
		// Expected path, or a duplicate of it.
	case state == infrav1.StateInspecting && ph.Spec.ConsumerRef != nil:
		log.Info("Provisioned callback on a host still Inspecting; the reconciler keeps it until the host is Deploying",
			"host", hostName, "inspectionPhase", ph.Status.InspectionPhase)
	default:
		log.Info("Provisioned callback received but host is not deploying; ignoring",
			"host", hostName, "state", state, "claimed", ph.Spec.ConsumerRef != nil)
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
