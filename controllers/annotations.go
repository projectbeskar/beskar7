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
	"encoding/json"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// removeAnnotationsPatch returns a merge patch that deletes exactly keys and
// no other annotation. A merge patch computed by diffing a copy of the object
// does not: when the deletion leaves the copy's map empty, JSON omits it
// (omitempty) and the patch becomes "annotations": null, which deletes every
// annotation on the server, including any another writer added after the copy
// was read.
func removeAnnotationsPatch(keys ...string) client.Patch {
	nulls := make(map[string]any, len(keys))
	for _, key := range keys {
		nulls[key] = nil
	}
	// Marshalling maps of strings to nil cannot fail.
	data, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": nulls}})
	return client.RawPatch(types.MergePatchType, data)
}

// BootstrapURLAnnotation is set by the Beskar7Machine controller on a PhysicalHost
// to signal the computed per-host bootstrap URL. The PhysicalHost controller reads
// this annotation, persists the value to Status.Bootstrap.URL, and then removes the
// annotation so it is not acted on again.
//
// Following the same "signal via annotation, status written by owner" pattern as
// InspectionRequestAnnotation (BUG-1 fix). Defined here so both controllers share
// the constant without creating an import cycle.
const BootstrapURLAnnotation = "infrastructure.cluster.x-k8s.io/bootstrap-url"

// BootstrapTokenAnnotation and BootNonceAnnotation are retired (D-029). Releases
// before D-029 signalled the hash and expiry of a freshly minted bearer token
// and boot nonce through them, and the PhysicalHost controller copied both into
// Status.Bootstrap, which the callback server trusted, so anyone allowed to
// patch a PhysicalHost could forge callback credentials (SEC-12). Nothing in
// the manager writes them: the credentials live only in the per-host
// bootstrap-token Secret. The PhysicalHost controller removes either one on
// sight without reading it (dropRetiredCredentialAnnotations); the keys stay
// defined only so it can.
const (
	BootstrapTokenAnnotation = "infrastructure.cluster.x-k8s.io/bootstrap-token"
	BootNonceAnnotation      = "infrastructure.cluster.x-k8s.io/boot-nonce"
)

// InspectionResultAnnotation is set by the inspection HTTP handler on a PhysicalHost
// to signal that a validated InspectionReport has been stored on a ConfigMap in the
// host's namespace. The PhysicalHost controller reads this annotation, fetches the
// referenced ConfigMap, persists the report to Status.InspectionReport + transitions
// Status.InspectionPhase to Complete, deletes the ConfigMap, and clears the
// annotation. Closes BUG-1 fully via D-005.
//
// Value: the metadata.name of the ConfigMap (always in the same namespace as the
// PhysicalHost).
const InspectionResultAnnotation = "infrastructure.cluster.x-k8s.io/inspection-result-ref"

// ProvisionedRequestAnnotation is set by the provisioned HTTP handler on a PhysicalHost
// to signal that the inspector has completed OS deployment and the host is ready.
// The PhysicalHost controller reads this annotation, transitions State from Deploying
// to Ready, and clears the annotation once status shows Ready, so it is not acted on
// twice (D-015). On a claimed host that is still Inspecting it leaves the annotation in
// place until the host is Deploying, and clears it without a transition if the host goes
// anywhere else (applyProvisionedRequestAnnotation).
//
// Value: "provisioned" — a fixed string, no data payload.
// The authenticated POST itself is the signal; the body is advisory only (D-015).
const ProvisionedRequestAnnotation = "infrastructure.cluster.x-k8s.io/provisioned-request"

// ProvisionFailedRequestAnnotation is set by the provision-failed HTTP handler on a
// PhysicalHost to signal that the inspector encountered a fatal error during OS
// deployment (image fetch, digest-verify, whole-disk write, or COS_OEM inject) and
// the deploy cannot proceed. The PhysicalHost controller reads this annotation,
// transitions State to Error, sets Status.ErrorMessage to the sanitized failure
// message, and clears the annotation so it is not acted on twice (v4.1). On a claimed
// host that is still Inspecting it leaves the annotation in place until the host is
// Deploying, and clears it without a transition if the host goes anywhere else
// (applyProvisionFailedRequestAnnotation).
//
// Value: a sanitized human-readable failure reason (≤256 chars, control-chars stripped,
// prefixed with "inspector reported deploy failure: "). The authenticated POST itself is
// the failure signal; the reason is advisory only and MUST be treated as untrusted input.
const ProvisionFailedRequestAnnotation = "infrastructure.cluster.x-k8s.io/provision-failed-request"
