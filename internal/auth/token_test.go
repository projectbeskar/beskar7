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

package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"
)

func TestMintToken(t *testing.T) {
	plaintext, hash, err := MintToken()
	if err != nil {
		t.Fatalf("MintToken returned error: %v", err)
	}
	if len(plaintext) != 43 {
		t.Errorf("plaintext = %d chars, want 43", len(plaintext))
	}
	if len(hash) != 64 {
		t.Errorf("hash = %d chars, want 64", len(hash))
	}
	sum := sha256.Sum256([]byte(plaintext))
	if hex.EncodeToString(sum[:]) != hash {
		t.Error("hash does not match sha256(plaintext)")
	}
}

func TestMintToken_Distinct(t *testing.T) {
	p1, h1, err := MintToken()
	if err != nil {
		t.Fatalf("MintToken #1 returned error: %v", err)
	}
	p2, h2, err := MintToken()
	if err != nil {
		t.Fatalf("MintToken #2 returned error: %v", err)
	}
	if p1 == p2 || h1 == h2 {
		t.Error("MintToken returned the same value twice — PRNG is broken")
	}
}

func TestVerify(t *testing.T) {
	p, h, err := MintToken()
	if err != nil {
		t.Fatalf("MintToken returned error: %v", err)
	}
	if !Verify(p, h) {
		t.Error("Verify returned false for matching pair")
	}
	if Verify(p, "") {
		t.Error("Verify returned true for empty hash")
	}
	if Verify("", h) {
		t.Error("Verify returned true for empty plaintext")
	}
	if Verify("", "") {
		t.Error("Verify returned true for both empty")
	}
	if Verify(p+"x", h) {
		t.Error("Verify returned true for tampered plaintext")
	}
	if Verify(p, h[:len(h)-1]+"f") {
		// h[:len-1]+"f" only differs from h when the last hex digit is not
		// already 'f'; if it happens to be, fall through to the next check.
		if h[len(h)-1] != 'f' {
			t.Error("Verify returned true for tampered hash")
		}
	}
}

// TestVerify_BothBranches deliberately exercises the matching and the
// non-matching code paths so static analysis can see both. We do not assert
// anything about timing — we trust crypto/subtle.ConstantTimeCompare to be
// constant-time. This is here only to ensure the constant-time path is not
// accidentally short-circuited in a future refactor.
func TestVerify_BothBranches(t *testing.T) {
	p, h, err := MintToken()
	if err != nil {
		t.Fatalf("MintToken returned error: %v", err)
	}
	// Matching path.
	if !Verify(p, h) {
		t.Fatal("matching path returned false")
	}
	// Non-matching but same-length path (the constant-time path that
	// actually reaches the subtle.ConstantTimeCompare call).
	other, _, err := MintToken()
	if err != nil {
		t.Fatalf("MintToken returned error: %v", err)
	}
	if Verify(other, h) {
		t.Fatal("non-matching path returned true")
	}
}

func TestLifetimeFor(t *testing.T) {
	now := time.Now()
	issued, expires := LifetimeFor(now)
	if !issued.Time.Equal(now) {
		t.Errorf("IssuedAt = %v, want %v", issued.Time, now)
	}
	if expires.Sub(now) != 30*time.Minute {
		t.Errorf("ExpiresAt - IssuedAt = %v, want 30m", expires.Sub(now))
	}
}

func TestNonceLifetimeFor(t *testing.T) {
	now := time.Now()
	expires := NonceLifetimeFor(now)
	if expires.Sub(now) != 10*time.Minute {
		t.Errorf("NonceLifetimeFor: expiresAt - now = %v, want %v", expires.Sub(now), BootNonceLifetime)
	}
}

func TestBootNonceLifetime_ShorterThanTokenLifetime(t *testing.T) {
	if BootNonceLifetime >= TokenLifetime {
		t.Errorf("BootNonceLifetime (%v) must be shorter than TokenLifetime (%v)", BootNonceLifetime, TokenLifetime)
	}
}
