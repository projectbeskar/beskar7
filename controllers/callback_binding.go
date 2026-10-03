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
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
	"github.com/projectbeskar/beskar7/internal/auth"
)

// The callback handlers never write PhysicalHost.Status. They leave a signal in
// an annotation (InspectionResultAnnotation, ProvisionedRequestAnnotation,
// ProvisionFailedRequestAnnotation) and the PhysicalHost reconciler turns it
// into state. Anyone allowed to patch PhysicalHosts could write the same
// annotations, and push a host to Ready, inject a hardware report or fail a run
// without any inspector (SEC-15).
//
// Each signal therefore carries a binding in a sibling annotation, written in
// the same patch: the hex HMAC-SHA256, keyed by the host's per-host bearer
// token, of the signal's key and value together with the host and the claim
// and boot cycle it is about (D-034). The handler can compute it because the
// bearer middleware has just authenticated the caller with that token; the
// reconciler recomputes it from the host's bootstrap-token Secret (D-029).
// Forging a signal then takes the Secret, the same bar as reading the host's
// bootstrap data.
//
// No wire change: the inspector sends exactly what it always did. The callback
// handlers and the reconciler have to run the same version, as they already did
// for the annotations themselves.

// callbackBindingDomain separates these MACs from any other use of the token.
// Changing the message layout means changing this string.
const callbackBindingDomain = "beskar7-callback-binding-v1"

// callbackBindingSuffix is appended to a signal annotation's key to name the
// annotation that carries its binding.
const callbackBindingSuffix = "-binding"

// callbackBindingAnnotation returns the key of the annotation carrying the
// binding of the signal annotation key.
func callbackBindingAnnotation(key string) string {
	return key + callbackBindingSuffix
}

// contentDigest is the digest a signal that points at stored content binds in
// place of that content: the hex SHA-256 of what is stored.
func contentDigest(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// errCallbackBindingUnavailable is returned when the host's credentials cannot
// bind a signal at all: a Secret without a token or without a boot nonce.
var errCallbackBindingUnavailable = errors.New("the host's bootstrap-token Secret holds no bearer token or boot nonce to bind a callback to")

// callbackBinder computes and checks the bindings of one host's signals under
// one reading of its bootstrap-token Secret.
type callbackBinder struct {
	host        *infrav1.PhysicalHost
	token       string
	consumerUID string
	nonceHash   string
}

// newCallbackBinder returns the binder for host under creds, which must come
// from boundBootstrapCredentials for the same host. It refuses credentials with
// no token (an empty key would let anyone compute a binding) or no boot nonce
// (nothing would tell one boot cycle from the next). The consumer's UID may be
// empty, on a Secret written before the UID was recorded: the token and the
// nonce still tie the binding to one cycle of the claim.
func newCallbackBinder(host *infrav1.PhysicalHost, creds bootstrapCredentials) (*callbackBinder, error) {
	if creds.token == "" || creds.nonce == "" {
		return nil, errCallbackBindingUnavailable
	}
	return &callbackBinder{
		host:        host,
		token:       creds.token,
		consumerUID: creds.consumerUID,
		nonceHash:   auth.Hash(creds.nonce),
	}, nil
}

// newCallbackSigner returns the binder a callback handler signs with, for a
// request the bearer middleware has authenticated with presentedToken.
//
// The middleware checked the token against its own read of the Secret. A
// release and re-mint landing between that read and this one would pair the old
// token with the new claim, and sign a callback of the old cycle into the new
// one, so the token is checked again against the credentials this binder is
// built from, as the bootstrap handler does.
func newCallbackSigner(ctx context.Context, c client.Reader, host *infrav1.PhysicalHost, presentedToken string, now time.Time) (*callbackBinder, error) {
	creds, _, err := boundBootstrapCredentials(ctx, c, host)
	if err != nil {
		return nil, fmt.Errorf("read the credentials to bind the callback to: %w", err)
	}
	if !creds.tokenValid(now) || !auth.Verify(presentedToken, auth.Hash(creds.token)) {
		return nil, errors.New("the callback's bearer token does not match the host's current credentials")
	}
	return newCallbackBinder(host, creds)
}

// mac returns the binding of the signal annotation key carrying value. digest
// is contentDigest of what the signal points at, or empty for a signal that
// carries all it says in its value.
//
// The message is the domain, then the fields below, NUL-separated: the key, the
// host's namespace, name and UID, the UID of the Beskar7Machine the credentials
// were minted for, the hash of the boot nonce current in the Secret (a signal
// captured in an earlier boot cycle of the same claim binds a nonce that is no
// longer there), the digest, and the value. The value comes last because it is
// the one field that can hold anything; every field before it is a name, a
// UID or a hex digest, so the encoding is unambiguous.
func (b *callbackBinder) mac(key, value, digest string) string {
	mac := hmac.New(sha256.New, []byte(b.token))
	mac.Write([]byte(strings.Join([]string{
		callbackBindingDomain,
		key,
		b.host.Namespace,
		b.host.Name,
		string(b.host.UID),
		b.consumerUID,
		b.nonceHash,
		digest,
		value,
	}, "\x00")))
	return hex.EncodeToString(mac.Sum(nil))
}

// setAnnotation sets the signal annotation key to value on host, and its
// binding next to it, so one merge patch carries both.
func (b *callbackBinder) setAnnotation(host *infrav1.PhysicalHost, key, value, digest string) {
	if host.Annotations == nil {
		host.Annotations = map[string]string{}
	}
	host.Annotations[key] = value
	host.Annotations[callbackBindingAnnotation(key)] = b.mac(key, value, digest)
}

// holds reports whether host's annotations carry a binding for the signal
// annotation key that matches the signal's value. Constant-time (hmac.Equal).
func (b *callbackBinder) holds(host *infrav1.PhysicalHost, key, digest string) bool {
	got, ok := host.Annotations[callbackBindingAnnotation(key)]
	if !ok {
		return false
	}
	want := b.mac(key, host.Annotations[key], digest)
	return hmac.Equal([]byte(got), []byte(want))
}

// dropCallbackAnnotation removes the signal annotation key from host, and its
// binding with it. Reconcile removes what a pass consumed by key afterwards
// (restoreConsumedAnnotations), so this is the one way an apply function
// consumes a signal.
func dropCallbackAnnotation(host *infrav1.PhysicalHost, key string) {
	delete(host.Annotations, key)
	delete(host.Annotations, callbackBindingAnnotation(key))
}

// callbackVerdict is what verifyCallbackAnnotation made of a signal.
type callbackVerdict int

const (
	// callbackBound: the binding matches; the signal may be acted on.
	callbackBound callbackVerdict = iota
	// callbackRejected: it does not match, or there is nothing it could match.
	// The signal and its binding have been removed from the host.
	callbackRejected
	// callbackUnverifiable: the credentials could not be read just now. The
	// signal is left as it is for the next pass; dropping it would lose a real
	// one to an API error.
	callbackUnverifiable
)

// verifyCallbackAnnotation checks the signal annotation key on host, which must
// be present, against the host's bootstrap-token Secret, and removes the signal
// and its binding from host unless it is bound (callbackRejected). It never
// checks the token's expiry: the handler authenticated the caller when it wrote
// the signal, and the token is cut to a few minutes at Ready (D-031) while a
// /provisioned report may still be waiting.
//
// A signal is bound when the Secret is the host's (bootstrapSecretOwnedBy), is
// bound to the machine the host's claim names, and the binding is the one the
// Secret's token, consumer UID and current boot nonce give for this key, value
// and digest. A missing or foreign Secret, a missing or wrong binding, and a
// nonce or consumer that has changed since the handler wrote it all reject.
//
// The log line names the host and the key and nothing else: no token, no
// binding, no value.
func verifyCallbackAnnotation(ctx context.Context, c client.Reader, logger logr.Logger, host *infrav1.PhysicalHost, key, digest string) callbackVerdict {
	creds, _, err := boundBootstrapCredentials(ctx, c, host)
	if err != nil {
		if errors.Is(err, errBootstrapSecretUnreadable) {
			logger.Error(err, "Cannot read the host's bootstrap-token Secret to check a callback annotation; leaving it for the next reconcile",
				"host", host.Name, "annotation", key)
			return callbackUnverifiable
		}
		return rejectCallbackAnnotation(logger, host, key, err)
	}
	binder, err := newCallbackBinder(host, creds)
	if err != nil {
		return rejectCallbackAnnotation(logger, host, key, err)
	}
	if !binder.holds(host, key, digest) {
		return rejectCallbackAnnotation(logger, host, key, nil)
	}
	return callbackBound
}

// rejectCallbackAnnotation drops an unbound signal. why is for the debug log
// only; it carries no credential material by construction.
func rejectCallbackAnnotation(logger logr.Logger, host *infrav1.PhysicalHost, key string, why error) callbackVerdict {
	logger.Info("Ignoring a callback annotation that is not bound to the host's credentials; removing it",
		"host", host.Name, "annotation", key)
	if why != nil {
		logger.V(1).Info("Callback annotation not bound", "host", host.Name, "annotation", key, "reason", why.Error())
	}
	dropCallbackAnnotation(host, key)
	return callbackRejected
}
