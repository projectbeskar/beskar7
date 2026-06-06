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

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// BootstrapURLAnnotation is set by the Beskar7Machine controller on a PhysicalHost
// to signal the computed per-host bootstrap URL. The PhysicalHost controller reads
// this annotation, persists the value to Status.Bootstrap.URL, and then removes the
// annotation so it is not acted on again.
//
// Following the same "signal via annotation, status written by owner" pattern as
// InspectionRequestAnnotation (BUG-1 fix). Defined here so both controllers share
// the constant without creating an import cycle.
const BootstrapURLAnnotation = "infrastructure.cluster.x-k8s.io/bootstrap-url"

// BootstrapTokenAnnotation is set by the Beskar7Machine controller on a PhysicalHost
// to signal the hash + lifetime of a freshly minted per-host bearer token. The
// plaintext is delivered out-of-band via a Secret (see PR-5.2 / D-006); only the
// observable hash + lifetime ride this annotation. The PhysicalHost controller reads
// it, persists the values to Status.Bootstrap.{TokenHash,IssuedAt,ExpiresAt}, then
// removes the annotation so it is not acted on twice.
//
// Value is a JSON encoding of BootstrapTokenAnnotationValue. JSON (vs. a delimited
// string) is used because RFC3339 timestamps contain ':' and '+' which would
// require escaping; encoding/json is the standard idiom in the codebase.
const BootstrapTokenAnnotation = "infrastructure.cluster.x-k8s.io/bootstrap-token"

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

// BootstrapTokenAnnotationValue is the wire format for BootstrapTokenAnnotation.
// Producer (Beskar7Machine controller) JSON-marshals one of these onto the
// PhysicalHost's annotations; consumer (PhysicalHost controller) unmarshals and
// persists to Status.Bootstrap.
//
// The plaintext token never appears here — it is delivered via a per-host Secret.
// Only the observable hash and lifetime metadata are signalled through this
// annotation, which (unlike a Secret) is visible to anyone with read access to the
// PhysicalHost. The hash by itself cannot be used to forge a valid bearer header.
type BootstrapTokenAnnotationValue struct {
	// Hash is the hex-encoded SHA-256 of the plaintext bearer token (64 chars).
	Hash string `json:"hash"`
	// IssuedAt is the time the token was minted, in RFC3339 form.
	IssuedAt metav1.Time `json:"issuedAt"`
	// ExpiresAt is the time the token stops being accepted, in RFC3339 form.
	ExpiresAt metav1.Time `json:"expiresAt"`
}

// BootNonceAnnotation is set by the Beskar7Machine controller on a PhysicalHost
// to signal the hash + expiry of a freshly minted per-host boot nonce (D-009).
// The plaintext nonce is delivered out-of-band via the per-host bootstrap-token
// Secret under key "plaintext-boot-nonce"; only the observable hash + expiry
// ride this annotation. The PhysicalHost controller reads it, persists the values
// to Status.Bootstrap.{BootNonceHash,BootNonceExpiresAt}, and clears the
// annotation so it is not acted on twice.
//
// Same "annotation in, status out, annotation cleared" pattern as
// BootstrapTokenAnnotation. Value is a JSON encoding of BootNonceAnnotationValue.
const BootNonceAnnotation = "infrastructure.cluster.x-k8s.io/boot-nonce"

// ProvisionedRequestAnnotation is set by the provisioned HTTP handler on a PhysicalHost
// to signal that the inspector has completed OS deployment and the host is ready.
// The PhysicalHost controller reads this annotation, transitions State from Deploying
// to Ready, and clears the annotation so it is not acted on twice (D-015).
//
// Value: "provisioned" — a fixed string, no data payload.
// The authenticated POST itself is the signal; the body is advisory only (D-015).
const ProvisionedRequestAnnotation = "infrastructure.cluster.x-k8s.io/provisioned-request"

// BootNonceAnnotationValue is the wire format for BootNonceAnnotation.
// Producer (Beskar7Machine controller) JSON-marshals one of these; consumer
// (PhysicalHost controller) unmarshals and persists to Status.Bootstrap.
//
// The nonce plaintext never appears here — only the hash (safe to log per the
// same reasoning as BootstrapTokenAnnotationValue.Hash) and the expiry instant.
// No IssuedAt field: the nonce lifecycle check only needs to know whether it
// is expired and whether it has been consumed (BootNonceConsumedAt, written by
// the /boot handler in D-010 — not present here).
type BootNonceAnnotationValue struct {
	// Hash is the hex-encoded SHA-256 of the plaintext boot nonce (64 chars).
	Hash string `json:"hash"`
	// ExpiresAt is the time the nonce stops being accepted, in RFC3339 form.
	ExpiresAt metav1.Time `json:"expiresAt"`
}
