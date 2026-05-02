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
	"net/http"
	"strings"

	"github.com/go-logr/logr"
)

// Verifier validates a presented bearer token in the context of an HTTP request.
// It returns nil on success (handler is invoked) and a non-nil error on any failure
// (token unknown, expired, wrong host, etc.). The error is for server-side
// observability only — it must NOT leak to the client beyond a generic 401.
//
// Implementations typically extract host coordinates from the request (path values
// or headers), look up the matching PhysicalHost, and call Verify against the
// stored TokenHash.
type Verifier func(token string, r *http.Request) error

// unauthorizedBody is the single, opaque response body returned for every
// authentication failure. Identical bytes regardless of whether the header was
// missing, malformed, the token was unknown, the token was expired, or the host
// had no token. This denies an attacker any oracle to distinguish failure modes.
const unauthorizedBody = `{"error":"unauthorized"}`

// RequireBearer wraps next with bearer-token authentication using the supplied
// Verifier. The middleware extracts an "Authorization: Bearer <token>" header,
// passes the token to verifier, and on success invokes next. On any failure it
// responds 401 with a fixed opaque body and (at V(1)) logs the specific reason.
//
// The plaintext bearer token is NEVER logged. The Authorization header itself is
// NEVER logged. Only a boolean ("tokenPresent") and the verifier's error (which
// the verifier itself must construct without echoing the token) are logged.
//
// All 401 responses are byte-identical: an attacker probing for missing headers vs.
// wrong tokens vs. expired tokens cannot distinguish them.
func RequireBearer(log logr.Logger, verifier Verifier, next http.Handler) http.Handler {
	if verifier == nil {
		// Programming error: a verifier-less middleware would accept any caller.
		// Fail closed by rejecting every request.
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			log.Error(nil, "RequireBearer wired without a Verifier; rejecting all requests")
			writeUnauthorized(w)
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := extractBearerToken(r.Header.Get("Authorization"))
		if !ok {
			log.V(1).Info("auth: missing or malformed Authorization header", "tokenPresent", false)
			writeUnauthorized(w)
			return
		}
		if err := verifier(token, r); err != nil {
			// Verifier errors describe the failure mode (host not found, expired,
			// hash mismatch, etc.). Log at V(1) for operator debug. The error MUST
			// NOT contain the plaintext token — by contract the verifier never
			// echoes its first argument.
			log.V(1).Info("auth: verifier rejected token", "tokenPresent", true, "err", err.Error())
			writeUnauthorized(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// extractBearerToken parses an "Authorization" header value and returns the token
// portion when the scheme is "Bearer" (case-insensitive). Returns ok=false for any
// other scheme, missing header, or empty token. Returning ok=false rather than an
// error keeps the call site simple — every false branch is a 401.
//
// The "Bearer " prefix match is case-insensitive (RFC 7235 §2.1). Surrounding
// whitespace inside the token is rejected — the value must be a single word.
func extractBearerToken(headerValue string) (string, bool) {
	if headerValue == "" {
		return "", false
	}
	const prefix = "Bearer "
	if len(headerValue) < len(prefix) {
		return "", false
	}
	if !strings.EqualFold(headerValue[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(headerValue[len(prefix):])
	if token == "" {
		return "", false
	}
	// Reject internal whitespace — bearer tokens are a single opaque word.
	if strings.ContainsAny(token, " \t\r\n") {
		return "", false
	}
	return token, true
}

// writeUnauthorized writes the canonical 401 response. Always the same headers,
// always the same body. Never includes a WWW-Authenticate challenge with
// information about the failure reason.
func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	// Errors here would mean the client closed the connection early; nothing useful
	// the middleware can do, and logging the failure adds noise. The 401 status was
	// already sent on WriteHeader.
	_, _ = w.Write([]byte(unauthorizedBody))
}
