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

// Package auth provides the per-host bearer token primitives used to
// authenticate inspection POSTs (PR-5.2) and bootstrap GETs (PR-5.3) against
// the manager's HTTP surface. See decision D-004 in PROJECT_CONTEXT.md.
//
// Security rules for callers:
//
//   - The plaintext token is sensitive and must NEVER be logged. MintToken
//     returns it once; the caller is expected to render it into the iPXE
//     kernel cmdline and otherwise hold it only in memory for the duration of
//     the reconcile.
//   - Only the SHA-256 hash is persisted (on PhysicalHost.Status.Bootstrap).
//     The hash leaking via reconcile logs is acceptable — it cannot be used
//     to forge a valid Authorization: Bearer header.
//   - Verify uses crypto/subtle.ConstantTimeCompare. Do not replace it with
//     ordinary string equality.
//   - Random source is crypto/rand. math/rand is forbidden for token material.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// tokenBytes is the length of the random material backing each bearer
	// token. 32 bytes = 256 bits of entropy, well above the 128-bit floor
	// for short-lived bearer tokens.
	tokenBytes = 32

	// TokenLifetime is the validity window applied at mint time per D-004.
	// The same per-host bearer token authorizes the whole inspector run:
	// inspection POST, bootstrap GET, AND (contract v4 / D-015) the
	// provisioned POST that fires only after the whole-disk write + OEM inject.
	// It must therefore outlive DefaultInspectionTimeout (10 min) + the OS
	// deploy, bounded by DefaultDeploymentTimeout (20 min) — i.e. ~30 min in the
	// worst case. 60 minutes keeps the token valid through that full span with
	// headroom for slow BIOS POST and large images, so a slow-but-healthy deploy
	// never sees the provisioned callback rejected as the token expires
	// (SEC-D015-1). Keep TokenLifetime >= DefaultInspectionTimeout +
	// DefaultDeploymentTimeout with margin if those defaults change.
	TokenLifetime = 60 * time.Minute

	// BootNonceLifetime is the validity window for per-host boot nonces (D-009).
	// Shorter than TokenLifetime because the nonce is single-use and consumed at
	// the first GET /api/v1/boot call: a long window only extends the race window
	// for a co-located provisioning-L2 attacker (see D-009 residual accepted risk).
	// 10 minutes matches DefaultInspectionTimeout — by the time inspection runs,
	// the nonce window has elapsed and a fresh nonce is minted on the next
	// triggerInspection call.
	BootNonceLifetime = 10 * time.Minute
)

// MintToken generates a fresh per-host bearer token.
//
// It returns:
//   - plaintext: the URL-safe base64 (no padding) encoding of 32 random
//     bytes drawn from crypto/rand. 43 characters. Suitable for inclusion in
//     iPXE kernel cmdlines and HTTP Authorization headers.
//   - hash: the lowercase hex encoding of sha256(plaintext). 64 characters.
//     This is the form persisted on PhysicalHost.Status.Bootstrap.TokenHash.
//
// The plaintext is returned exactly once. Callers MUST NOT log it. If the
// caller fails to deliver the plaintext to its destination (e.g. an iPXE
// render error), the only recovery is to mint a new token and rewrite the
// status — the original plaintext cannot be recovered from the hash.
func MintToken() (plaintext, hash string, err error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("read random token bytes: %w", err)
	}

	// RawURLEncoding (not URLEncoding): URL-safe alphabet with NO `=`
	// padding. Padding is undesirable because some logging and proxy layers
	// strip trailing `=` and because tokens travel on iPXE kernel cmdlines
	// where shell-special characters are best avoided.
	plaintext = base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(plaintext))
	hash = hex.EncodeToString(sum[:])
	return plaintext, hash, nil
}

// Verify reports whether plaintext hashes to storedHash.
//
// Comparison is constant-time via crypto/subtle.ConstantTimeCompare to
// foreclose timing side channels. As a defense-in-depth measure, Verify
// returns false immediately if either input is empty, so that an
// unpopulated PhysicalHost.Status.Bootstrap.TokenHash cannot be matched by
// an attacker submitting an empty Authorization header.
func Verify(plaintext, storedHash string) bool {
	if plaintext == "" || storedHash == "" {
		return false
	}
	sum := sha256.Sum256([]byte(plaintext))
	candidate := hex.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(storedHash)) == 1
}

// LifetimeFor returns the (issuedAt, expiresAt) pair to persist on
// PhysicalHost.Status.Bootstrap when a token is minted at the given instant.
// Centralized here so future PRs that wire mint-on-inspection (PR-5.2/5.3)
// share a single source of truth for the validity window.
func LifetimeFor(now time.Time) (issuedAt, expiresAt metav1.Time) {
	issuedAt = metav1.NewTime(now)
	expiresAt = metav1.NewTime(now.Add(TokenLifetime))
	return issuedAt, expiresAt
}

// NonceLifetimeFor returns the expiresAt timestamp to persist on
// PhysicalHost.Status.Bootstrap when a boot nonce is minted at the given
// instant. The nonce has no issuedAt field in Status — only expiresAt and the
// consumed marker matter for the validity check. Uses BootNonceLifetime (10 min)
// rather than TokenLifetime (30 min) because the nonce is single-use.
func NonceLifetimeFor(now time.Time) (expiresAt metav1.Time) {
	return metav1.NewTime(now.Add(BootNonceLifetime))
}
