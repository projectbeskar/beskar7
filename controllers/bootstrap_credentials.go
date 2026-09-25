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
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
	"github.com/projectbeskar/beskar7/internal/auth"
)

// The per-host bootstrap-token Secret is the only source of the credentials
// the callback server checks (D-029). The Beskar7Machine controller writes it
// when it mints; the bearer verifier and the /boot handler read it; the
// PhysicalHost controller mirrors its hashes and expiries into
// Status.Bootstrap for operators and tooling, and nothing reads that mirror to
// authenticate. Only a Secret writer can change what authenticates, and a
// Secret writer in the namespace already controls its bootstrap data.

// Data keys of the per-host bootstrap-token Secret. The plaintext keys are also
// read by the operator's boot service (docs/ipxe-setup.md).
const (
	bootstrapTokenSecretKey = "plaintext-token"
	bootNonceSecretKey      = "plaintext-boot-nonce"
	// The expiries are written by the manager when it mints. A credential
	// whose expiry is missing or unparseable never verifies.
	bootstrapTokenIssuedAtSecretKey  = "token-issued-at"
	bootstrapTokenExpiresAtSecretKey = "token-expires-at"
	bootNonceExpiresAtSecretKey      = "boot-nonce-expires-at"
	// bootstrapConsumerSecretKey names the Beskar7Machine the credentials were
	// minted for. A callback authenticates only while the host's ConsumerRef
	// names that same machine. The binding is by name, not UID, because
	// clusterctl move recreates the Beskar7Machine with a new UID.
	bootstrapConsumerSecretKey = "consumer"
	// bootstrapConsumerUIDSecretKey is the UID of that machine. Verification
	// never reads it; the mint does, so that a Beskar7Machine recreated under
	// the same name is a new claim and gets fresh credentials (contract §12)
	// instead of inheriting the earlier machine's.
	bootstrapConsumerUIDSecretKey = "consumer-uid"
)

// bootstrapTokenSecretName returns the deterministic name of the per-host
// Secret holding the callback credentials (D-006).
func bootstrapTokenSecretName(hostName string) string {
	return hostName + "-bootstrap-token"
}

// bootstrapCredentials is the content of a host's bootstrap-token Secret. A zero
// time means the Secret carries no parseable value for it.
type bootstrapCredentials struct {
	token          string
	tokenIssuedAt  time.Time
	tokenExpiresAt time.Time
	nonce          string
	nonceExpiresAt time.Time
	consumer       string
	consumerUID    string
}

// readBootstrapCredentials parses secret, which may be nil.
func readBootstrapCredentials(secret *corev1.Secret) bootstrapCredentials {
	if secret == nil {
		return bootstrapCredentials{}
	}
	return bootstrapCredentials{
		token:          string(secret.Data[bootstrapTokenSecretKey]),
		tokenIssuedAt:  parseCredentialTime(secret.Data[bootstrapTokenIssuedAtSecretKey]),
		tokenExpiresAt: parseCredentialTime(secret.Data[bootstrapTokenExpiresAtSecretKey]),
		nonce:          string(secret.Data[bootNonceSecretKey]),
		nonceExpiresAt: parseCredentialTime(secret.Data[bootNonceExpiresAtSecretKey]),
		consumer:       string(secret.Data[bootstrapConsumerSecretKey]),
		consumerUID:    string(secret.Data[bootstrapConsumerUIDSecretKey]),
	}
}

// writeTo stores c in secret's data, removing the key of every empty field and
// leaving keys it does not own alone.
func (c bootstrapCredentials) writeTo(secret *corev1.Secret) {
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	put := func(key, value string) {
		if value == "" {
			delete(secret.Data, key)
			return
		}
		secret.Data[key] = []byte(value)
	}
	put(bootstrapTokenSecretKey, c.token)
	put(bootstrapTokenIssuedAtSecretKey, formatCredentialTime(c.tokenIssuedAt))
	put(bootstrapTokenExpiresAtSecretKey, formatCredentialTime(c.tokenExpiresAt))
	put(bootNonceSecretKey, c.nonce)
	put(bootNonceExpiresAtSecretKey, formatCredentialTime(c.nonceExpiresAt))
	put(bootstrapConsumerSecretKey, c.consumer)
	put(bootstrapConsumerUIDSecretKey, c.consumerUID)
}

// tokenValid reports whether c holds a bearer token that has an expiry and
// has not reached it.
func (c bootstrapCredentials) tokenValid(now time.Time) bool {
	return c.token != "" && !c.tokenExpiresAt.IsZero() && now.Before(c.tokenExpiresAt)
}

// nonceValid reports whether c holds a boot nonce that has an expiry and has
// not reached it. Whether it has been consumed is a separate question
// (bootNonceConsumed).
func (c bootstrapCredentials) nonceValid(now time.Time) bool {
	return c.nonce != "" && !c.nonceExpiresAt.IsZero() && now.Before(c.nonceExpiresAt)
}

// parseCredentialTime parses an RFC 3339 time from the Secret, returning the
// zero time for a missing or malformed value so that the credential it
// belongs to fails closed.
func parseCredentialTime(raw []byte) time.Time {
	if len(raw) == 0 {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, string(raw))
	if err != nil {
		return time.Time{}
	}
	return t
}

// formatCredentialTime is parseCredentialTime's inverse; the zero time is "".
func formatCredentialTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// boundBootstrapCredentials returns the credentials in host's bootstrap-token
// Secret together with the Beskar7Machine they are bound to, but only while
// host is claimed and its claim names that machine: host.Spec.ConsumerRef must
// resolve to a Beskar7Machine in host's own namespace
// (resolveConsumerBeskar7Machine) whose name is the Secret's consumer.
//
// A host nobody claims, a claim naming a different machine than the one the
// credentials were minted for (a re-pointed ConsumerRef, or a re-claim the new
// machine has not minted for yet), and a Secret written before the binding
// existed all fail here, so none of them authenticates a callback. The error
// is for server-side logs only and carries no credential material.
func boundBootstrapCredentials(ctx context.Context, c client.Reader, host *infrav1.PhysicalHost) (bootstrapCredentials, types.NamespacedName, error) {
	consumer, ok := resolveConsumerBeskar7Machine(host)
	if !ok {
		return bootstrapCredentials{}, types.NamespacedName{}, fmt.Errorf("host %s/%s has no valid Beskar7Machine consumer", host.Namespace, host.Name)
	}
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: host.Namespace, Name: bootstrapTokenSecretName(host.Name)}
	if err := c.Get(ctx, key, secret); err != nil {
		return bootstrapCredentials{}, types.NamespacedName{}, fmt.Errorf("get bootstrap-token Secret %s: %w", key.Name, err)
	}
	creds := readBootstrapCredentials(secret)
	if creds.consumer == "" || creds.consumer != consumer.Name {
		return bootstrapCredentials{}, types.NamespacedName{}, fmt.Errorf("bootstrap-token Secret %s is not bound to the host's current consumer", key.Name)
	}
	return creds, consumer, nil
}

// verifyBootNonce reports whether nonce is the unexpired boot nonce creds
// holds. The comparison is sha256(nonce) against sha256(creds.nonce) in
// constant time (auth.Verify).
//
// Deliberately does NOT check the consume record — that check belongs in the
// consume path so the already-consumed branch can render identical content.
func verifyBootNonce(nonce string, creds bootstrapCredentials, now time.Time) bool {
	if !creds.nonceValid(now) {
		return false
	}
	return auth.Verify(nonce, auth.Hash(creds.nonce))
}

// bootNonceConsumed reports whether the consume record in bs names the boot
// nonce whose hash is nonceHash (D-010).
//
// The record is never cleared: Status.Bootstrap outlives a claim, and the
// nonce the next claim mints sits next to the record of the one before.
// Telling the two apart by hash is what keeps that new nonce unconsumed until
// its own first fetch.
//
// A record without a hash, written by a handler from before the record named
// one, is not attributed to the current nonce here, so the next fetch records
// its consume afresh. The Beskar7Machine reads such a record the other way
// (bootNonceConsumeUnattributed): it never reuses a nonce the record might
// describe.
func bootNonceConsumed(bs *infrav1.BootstrapStatus, nonceHash string) bool {
	return bs != nil && bs.BootNonceConsumedAt != nil &&
		nonceHash != "" && bs.BootNonceConsumedHash == nonceHash
}

// bootNonceConsumeUnattributed reports whether bs carries a consume record that
// does not say which nonce it consumed: one written by a /boot handler from
// before BootNonceConsumedHash existed, which may describe the current nonce or
// an earlier one.
func bootNonceConsumeUnattributed(bs *infrav1.BootstrapStatus) bool {
	return bs != nil && bs.BootNonceConsumedAt != nil && bs.BootNonceConsumedHash == ""
}
