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

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
	"github.com/projectbeskar/beskar7/internal/auth"
)

// cleaningReportMaxBodyBytes caps the advisory body of a cleaning report.
const cleaningReportMaxBodyBytes = 64 << 10

// CleaningReportHandler serves POST /api/v1/cleaned and, with Failed, /api/v1/clean-failed by leaving a bound annotation (D-034).
type CleaningReportHandler struct {
	Client client.Client
	Log    logr.Logger
	// Failed selects the /clean-failed report; false is /cleaned.
	Failed bool
}

// cleaningReport is the signal one report leaves on its host.
type cleaningReport struct {
	Namespace string
	HostName  string
	Token     string
	Key       string
	Value     string
}

// ServeHTTP handles a cleaning report already authenticated by the middleware.
func (h *CleaningReportHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	report := cleaningReport{
		Namespace: r.PathValue("namespace"),
		HostName:  r.PathValue("hostName"),
		Key:       CleanedRequestAnnotation,
		Value:     cleanedRequestValue,
	}
	log := h.Log.WithValues("namespace", report.Namespace, "host", report.HostName, "remote", r.RemoteAddr)

	reason := readCleaningReason(log, w, r)
	if h.Failed {
		report.Key = CleanFailedRequestAnnotation
		report.Value = sanitizeReportedReason(reason, cleanFailedReasonPrefix)
	}

	token, ok := auth.BearerToken(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	report.Token = token

	if err := h.signal(r.Context(), log, report); err != nil {
		log.Error(err, "Failed to signal the cleaning report")
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	log.Info("Cleaning report accepted; signalled reconciler via annotation", "annotation", report.Key)
	w.Header().Set(headerContentType, "application/json")
	w.WriteHeader(http.StatusAccepted)
	if _, err := w.Write([]byte(`{"status":"accepted"}`)); err != nil {
		log.V(1).Info("Failed to write response body", "err", err.Error())
	}
}

// signal sets the report's annotation, bound to its token, on a Cleaning host.
func (h *CleaningReportHandler) signal(ctx context.Context, log logr.Logger, report cleaningReport) error {
	ph := &infrav1.PhysicalHost{}
	if err := h.Client.Get(ctx, types.NamespacedName{Namespace: report.Namespace, Name: report.HostName}, ph); err != nil {
		return fmt.Errorf("get PhysicalHost: %w", err)
	}
	if ph.Status.State != infrav1.StateCleaning {
		log.Info("Cleaning report on a host that is not Cleaning; ignoring", "state", ph.Status.State)
		return nil
	}
	signer, err := newCleaningCallbackSigner(ctx, h.Client, ph, report.Token)
	if err != nil {
		return fmt.Errorf("bind the cleaning report to the host's cleaning credentials: %w", err)
	}
	base := ph.DeepCopy()
	signer.setAnnotation(ph, report.Key, report.Value, "")
	// The key is this handler's alone, so no optimistic lock (as signalProvisioned).
	if err := h.Client.Patch(ctx, ph, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("patch PhysicalHost cleaning annotation: %w", err)
	}
	return nil
}

// readCleaningReason returns the body's unsanitized "reason", or "" when there is none.
func readCleaningReason(log logr.Logger, w http.ResponseWriter, r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	r.Body = http.MaxBytesReader(w, r.Body, cleaningReportMaxBodyBytes)
	defer func() { _ = r.Body.Close() }()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.V(1).Info("Could not read the cleaning report body (advisory only); proceeding", "err", err.Error())
		return ""
	}
	var payload provisionFailedBody
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	return payload.Reason
}
