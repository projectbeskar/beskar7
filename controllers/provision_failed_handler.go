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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrastructurev1beta1 "github.com/projectbeskar/beskar7/api/v1beta1"
)

const (
	// provisionFailedMaxBodyBytes caps the request body for the provision-failed handler.
	// The inspector sends {"reason":"<short reason>"} (~handful of bytes); 64 KiB is
	// generously above that and below any memory-pressure point. Body content is advisory
	// only — the authenticated POST itself is the failure signal (v4.1 contract).
	provisionFailedMaxBodyBytes = 64 << 10

	// provisionFailedReasonMaxLen is the maximum length of the sanitized failure reason
	// that will be stored in PhysicalHost.Status.ErrorMessage. Longer strings are truncated.
	// 256 chars is enough for a descriptive error message while preventing unbounded growth
	// in status (which is persisted to etcd).
	provisionFailedReasonMaxLen = 256

	// provisionFailedReasonPrefix is prepended to all failure reasons stored in
	// PhysicalHost.Status.ErrorMessage so operators know the message originated from
	// the inspector, not from a Redfish or controller-internal error.
	provisionFailedReasonPrefix = "inspector reported deploy failure: "

	// provisionFailedReasonGeneric is used when the inspector does not supply a reason or
	// the reason is empty after sanitization.
	provisionFailedReasonGeneric = "inspector reported deploy failure (no details provided)"
)

// ProvisionFailedHandler handles POST /api/v1/provision-failed/{namespace}/{hostName}
// from the inspector, signalling that OS deployment has failed and cannot proceed.
//
// Authentication: callers must present "Authorization: Bearer <token>" with the same
// per-host bearer token used for inspection POST, bootstrap GET, and the provisioned
// callback (newBearerTokenVerifier + auth.RequireBearer; D-004). ServeHTTP assumes the
// request has already passed the bearer middleware.
//
// Signal: the handler extracts and sanitizes the "reason" field from the advisory JSON
// body, then patches ProvisionFailedRequestAnnotation carrying the sanitized message
// onto the PhysicalHost metadata. The PhysicalHostReconciler reads this on its next
// pass, transitions State from Deploying to Error, sets Status.ErrorMessage, and clears
// the annotation (v4.1 / D-005 pattern). This handler does NOT write
// PhysicalHost.Status directly.
//
// The reason is attacker-influenceable (it rides the inspector→controller wire under
// the host's own token, but the inspector is on an untrusted provisioning network).
// It is sanitized: control characters and newlines stripped, capped at
// provisionFailedReasonMaxLen, and prefixed with provisionFailedReasonPrefix.
type ProvisionFailedHandler struct {
	Client client.Client
	Log    logr.Logger
}

// provisionFailedBody is the JSON payload shape for the provision-failed callback.
// The reason field is advisory only; the authenticated POST itself is the signal.
type provisionFailedBody struct {
	Reason string `json:"reason,omitempty"`
}

// ServeHTTP handles provision-failure callbacks from the inspector.
// It is invoked only after the bearer-auth middleware has validated the caller.
func (h *ProvisionFailedHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	namespace := r.PathValue("namespace")
	hostName := r.PathValue("hostName")
	log := h.Log.WithValues("method", r.Method, "namespace", namespace, "host", hostName, "remote", r.RemoteAddr)

	if r.Method != http.MethodPost {
		log.Info("Method not allowed", "method", r.Method)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract the advisory reason from the body. Cap + discard after decode so a
	// slow-loris body cannot keep a goroutine alive unbounded.
	reason := h.extractReason(log, r, w)

	ctx := r.Context()

	if err := h.signalProvisionFailed(ctx, log, namespace, hostName, reason); err != nil {
		log.Error(err, "Failed to signal provision-failed state")
		// Opaque error response — do not leak internal resource names or k8s details.
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	log.Info("Provision-failed callback accepted; signalled reconciler via annotation")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	if _, err := w.Write([]byte(`{"status":"accepted"}`)); err != nil {
		log.V(1).Info("Failed to write response body", "err", err.Error())
	}
}

// extractReason reads and parses the advisory JSON body, sanitizes the "reason" field,
// and returns a non-empty message suitable for storage. Body is always fully consumed and
// closed before returning so the connection can be reused. Never returns the raw
// unsanitized value from the body — always sanitized.
func (h *ProvisionFailedHandler) extractReason(log logr.Logger, r *http.Request, w http.ResponseWriter) string {
	if r.Body == nil {
		return provisionFailedReasonGeneric
	}
	r.Body = http.MaxBytesReader(w, r.Body, provisionFailedMaxBodyBytes)
	defer func() { _ = r.Body.Close() }()

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		// Body was too large or unreadable. Advisory body only — still accept the signal.
		log.V(1).Info("Could not read provision-failed body (advisory only); proceeding", "err", err.Error())
		return provisionFailedReasonGeneric
	}

	var payload provisionFailedBody
	if err := json.Unmarshal(bodyBytes, &payload); err != nil {
		// Body is not valid JSON. Advisory only — still accept the signal.
		log.V(1).Info("Could not decode provision-failed body (advisory only); proceeding", "err", err.Error())
		return provisionFailedReasonGeneric
	}

	return sanitizeFailureReason(payload.Reason)
}

// sanitizeFailureReason strips control characters (including newlines) from reason,
// caps it to provisionFailedReasonMaxLen, and prepends provisionFailedReasonPrefix.
// Returns provisionFailedReasonGeneric when reason is empty after stripping.
//
// The reason comes from an inspector running on an untrusted provisioning L2 network.
// While it is bearer-authenticated (the token is per-host and short-lived), the
// inspector binary could be replaced or the host could be compromised. Stripping
// control characters prevents log-injection; capping prevents etcd bloat.
func sanitizeFailureReason(reason string) string {
	// Strip control characters (including \n, \r, \t) and non-printable Unicode.
	var b strings.Builder
	b.Grow(len(reason))
	for _, r := range reason {
		if r != unicode.ReplacementChar && unicode.IsPrint(r) {
			b.WriteRune(r)
		}
	}
	cleaned := strings.TrimSpace(b.String())
	if cleaned == "" {
		return provisionFailedReasonGeneric
	}
	// Cap to provisionFailedReasonMaxLen before prepending the prefix. The cap is a
	// byte length, but IsPrint admits multibyte runes, so a naive byte slice can
	// split a rune and emit invalid UTF-8 — which etcd/protobuf rejects on the
	// status write, defeating the fast-fail (SEC-D016-1). ToValidUTF8 drops any
	// dangling partial rune left at the cut.
	if len(cleaned) > provisionFailedReasonMaxLen {
		cleaned = strings.ToValidUTF8(cleaned[:provisionFailedReasonMaxLen], "")
	}
	return provisionFailedReasonPrefix + cleaned
}

// signalProvisionFailed fetches the PhysicalHost and patches the
// ProvisionFailedRequestAnnotation so the PhysicalHostReconciler can drive the
// Deploying→Error transition. It does NOT write PhysicalHost.Status (D-005 invariant).
//
// Guard: only act when the host is in StateDeploying. If the host is in Error already
// (idempotent delivery) or in another state, log and return nil — we do not force a
// non-Deploying host into Error. The Beskar7Machine controller will observe the
// ErrorMessage on its next reconcile regardless of which path set it.
func (h *ProvisionFailedHandler) signalProvisionFailed(ctx context.Context, log logr.Logger, namespace, hostName, sanitizedReason string) error {
	ph := &infrastructurev1beta1.PhysicalHost{}
	key := types.NamespacedName{Namespace: namespace, Name: hostName}
	if err := h.Client.Get(ctx, key, ph); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("PhysicalHost %s/%s not found", namespace, hostName)
		}
		return fmt.Errorf("failed to get PhysicalHost: %w", err)
	}

	switch ph.Status.State {
	case infrastructurev1beta1.StateDeploying:
		// Expected path: host is mid-deploy; set the failure annotation.
	case infrastructurev1beta1.StateError:
		// Already in error (idempotent delivery or a second POST after reconcile acted).
		// Clear any stale annotation so the reconciler doesn't double-process, then return.
		log.V(1).Info("Provision-failed callback on already-errored host; clearing annotation idempotently", "host", hostName)
		if _, ok := ph.Annotations[ProvisionFailedRequestAnnotation]; ok {
			base := ph.DeepCopy()
			delete(ph.Annotations, ProvisionFailedRequestAnnotation)
			if err := h.Client.Patch(ctx, ph, client.MergeFrom(base)); err != nil {
				log.V(1).Info("Failed to clear stale provision-failed annotation; continuing", "err", err.Error())
			}
		}
		return nil
	default:
		// Unexpected state — cannot safely force-error a host not in Deploying.
		log.Info("Provision-failed callback received but host is not in Deploying state; ignoring",
			"host", hostName, "state", ph.Status.State)
		return nil
	}

	base := ph.DeepCopy()
	if ph.Annotations == nil {
		ph.Annotations = map[string]string{}
	}
	ph.Annotations[ProvisionFailedRequestAnnotation] = sanitizedReason

	// Plain MergeFrom (no optimistic lock). Same reasoning as signalProvisioned:
	// this annotation key is unique to this handler; no other writer can collide on it.
	if err := h.Client.Patch(ctx, ph, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("patch PhysicalHost provision-failed annotation: %w", err)
	}
	log.V(1).Info("Provision-failed annotation set", "host", hostName)
	return nil
}
