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
//     returns it once; the caller stores it in the host's bootstrap-token
//     Secret, the only place the callback server reads it from (D-029).
//   - The SHA-256 hash is safe to publish (PhysicalHost.Status.Bootstrap
//     mirrors it for operators) and to log — it cannot be used to forge a
//     valid Authorization: Bearer header.
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
	//
	// It is the most a token lives, not what it usually does: the
	// Beskar7Machine controller cuts it to TokenReadyGrace once the host is
	// Ready (D-031) or the machine has failed terminally (D-036).
	TokenLifetime = 60 * time.Minute

	// TokenReadyGrace is how long a bearer token keeps authenticating once its
	// run is over: the host is Ready (D-031), or the Beskar7Machine has failed
	// terminally (D-036). Nothing in a run calls back after that but the
	// inspector's own retries of POST /provisioned or POST /provision-failed,
	// which it makes over about two and a half minutes when a response is lost,
	// and a 401 among them is fatal to the host. Five minutes covers those
	// retries and little else.
	TokenReadyGrace = 5 * time.Minute

	// BootNonceLifetime is the validity window for per-host boot nonces (D-009).
	// Shorter than TokenLifetime because the nonce is consumed at the first
	// GET /api/v1/boot call: a long window only extends the race window for a
	// co-located provisioning-L2 attacker (see D-009 residual accepted risk).
	// 10 minutes matches DefaultInspectionTimeout — by the time inspection runs,
	// the nonce window has elapsed and a fresh nonce is minted on the next
	// triggerInspection call.
	BootNonceLifetime = 10 * time.Minute

	// BootNonceRetryWindow is how long after its first fetch a consumed boot
	// nonce still renders the script, and only for the client that consumed it
	// (D-031): long enough for a NIC to retry a chainload, too short and too
	// narrow for a nonce read off the provisioning network to be worth much.
	BootNonceRetryWindow = 2 * time.Minute
)

// MintToken generates a fresh per-host bearer token.
//
// It returns:
//   - plaintext: the URL-safe base64 (no padding) encoding of 32 random
//     bytes drawn from crypto/rand. 43 characters. Suitable for inclusion in
//     iPXE kernel cmdlines and HTTP Authorization headers.
//   - hash: the lowercase hex encoding of sha256(plaintext). 64 characters.
//     The same value Hash returns for plaintext.
//
// The plaintext is returned exactly once. Callers MUST NOT log it. If the
// caller fails to deliver the plaintext to its destination (e.g. an iPXE
// render error), the only recovery is to mint a new token — the original
// plaintext cannot be recovered from the hash.
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
	return plaintext, Hash(plaintext), nil
}

// Hash returns the lowercase hex encoding of sha256(plaintext): the form Verify
// compares against, and the one PhysicalHost.Status.Bootstrap mirrors.
func Hash(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// Verify reports whether plaintext hashes to storedHash.
//
// Comparison is constant-time via crypto/subtle.ConstantTimeCompare to
// foreclose timing side channels. As a defense-in-depth measure, Verify
// returns false immediately if either input is empty, so that a credential
// that was never issued cannot be matched by an attacker submitting an empty
// value.
func Verify(plaintext, storedHash string) bool {
	if plaintext == "" || storedHash == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(Hash(plaintext)), []byte(storedHash)) == 1
}

// NonceLifetimeFor returns the expiresAt timestamp a boot nonce minted at the
// given instant is stored with in the host's bootstrap-token Secret (D-029).
// The nonce has no issuedAt — only expiresAt and the consume record matter for
// the validity check. Uses BootNonceLifetime (10 min) rather than
// TokenLifetime (60 min) because the nonce is consumed at the first boot.
func NonceLifetimeFor(now time.Time) (expiresAt metav1.Time) {
	return metav1.NewTime(now.Add(BootNonceLifetime))
}
