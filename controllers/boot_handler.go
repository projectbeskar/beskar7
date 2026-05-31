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
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/go-logr/logr"
	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrastructurev1beta1 "github.com/projectbeskar/beskar7/api/v1beta1"
	"github.com/projectbeskar/beskar7/internal/auth"
)

const (
	// bootHandlerOpaqueFailureStatus is the HTTP status returned for every /boot
	// failure regardless of cause. Per contract §4.1, callers must not be able to
	// distinguish "host not found", "wrong nonce", "nonce expired", or
	// "nonce consumed" from the response.
	bootHandlerOpaqueFailureStatus = http.StatusNotFound

	// bootHandlerOpaqueFailureBody is the fixed body for all /boot failures.
	bootHandlerOpaqueFailureBody = "not found"

	// bootNonceConsumeMaxRetries is the maximum number of optimistic-lock retries
	// in the consume path. Under normal load a single retry suffices; 3 bounds
	// pathological concurrency without spinning forever.
	bootNonceConsumeMaxRetries = 3

	// bootIPRateLimitRPS is the sustained per-IP request rate allowed on /boot.
	// A single NIC retry fires once per boot; 1 r/s per IP is generous for
	// legitimate retries and blocks scanning.
	bootIPRateLimitRPS = 1.0

	// bootIPRateLimitBurst is the burst capacity per IP. iPXE may retry a
	// chainload a few times on a flaky network; 5 lets a slow NIC burst without
	// triggering the limiter under normal operation.
	bootIPRateLimitBurst = 5

	// bootIPRateLimiterTTL is how long an idle per-IP entry stays in the map
	// before eviction. Prevents unbounded map growth on networks that rotate
	// IP assignments frequently.
	bootIPRateLimiterTTL = 5 * time.Minute

	// bootScriptMaxBytes is the upper bound on the rendered iPXE boot script.
	// The Linux kernel cmdline cap is ~2–4 KiB depending on arch; 4096 bytes is
	// the safe ceiling. A script that exceeds this is operator misconfig (e.g. an
	// over-large beskar7.ca chain) and would silently truncate the cmdline on real
	// hardware. We surface it as an operator-visible Info log and an opaque failure
	// rather than delivering a broken script. (SEC-10)
	bootScriptMaxBytes = 4096
)

// BootHandlerConfig carries the operator-supplied configuration needed to
// render the iPXE cmdline. Populated once at SetupCallbackServer time and
// shared (read-only) across all handler invocations.
type BootHandlerConfig struct {
	// APIBase is the externally-reachable HTTPS base URL of the callback server,
	// e.g. "https://beskar7.example.com:8082". Rendered into beskar7.api=.
	APIBase string

	// CABytes is the PEM-encoded CA certificate the inspector uses to verify the
	// callback TLS certificate. Base64-encoded into beskar7.ca=. Sourced from
	// the callback cert dir (ca.crt if present, else tls.crt).
	CABytes []byte
}

// ipEntry pairs a rate limiter with the time it was last seen. Used by
// BootHandler to evict stale per-IP entries from the limiter map.
type ipEntry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// BootHandler serves the per-host iPXE boot script.
//
// Auth: the {nonce} path segment is verified constant-time against
// Status.Bootstrap.BootNonceHash, within TTL, and not yet consumed. NOT
// bearer-gated — the booting host has no bearer token yet; that token is
// delivered by this endpoint in the rendered cmdline.
//
// On success: marks the nonce consumed (D-010, single-use) and returns the
// rendered iPXE script. A second fetch for the same host (race loser or NIC
// retry) returns identical content (§4.1).
//
// Every failure returns the same opaque response so callers cannot distinguish
// "host not found" from "wrong nonce" from "expired" from "consumed" (§4.1).
//
// Status ownership exception: this handler writes exactly one field,
// PhysicalHost.Status.Bootstrap.BootNonceConsumedAt, via an optimistic-locked
// Status().Patch. This is the sole audited exception to the D-005 invariant.
// See D-010 in PROJECT_CONTEXT.md for the rationale.
//
// INVARIANT (D-005 amendment): grep "client.Status().Update" controllers/*_handler.go
// must remain empty. Only the single audited Status().Patch below is permitted.
type BootHandler struct {
	Client client.Client
	Log    logr.Logger
	Config BootHandlerConfig

	// ipLimiters holds per-source-IP rate limiters. Protected by ipLimitersMu.
	// Entries are evicted after bootIPRateLimiterTTL of inactivity.
	ipLimitersMu sync.Mutex
	ipLimiters   map[string]*ipEntry
}

// ServeHTTP handles GET /api/v1/boot/{namespace}/{hostName}/{nonce}.
func (h *BootHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	namespace := r.PathValue("namespace")
	hostName := r.PathValue("hostName")
	// nonce is the capability token; never logged (§4.1).
	nonce := r.PathValue("nonce")

	// Log namespace + host + remote IP + outcome. Never the nonce, token,
	// CA bytes, or rendered script.
	log := h.Log.WithValues("namespace", namespace, "host", hostName, "remote", r.RemoteAddr)

	// Rate-limit per source IP before touching the API server. The /boot route is
	// ungated (no bearer token), so rate limiting is the first line of defence
	// against credential-stuffing on the nonce space.
	clientIP := remoteAddrToIP(r.RemoteAddr)
	if !h.allowIP(clientIP) {
		log.V(1).Info("boot GET: rate-limited", "ip", clientIP)
		// 429 so operators can distinguish rate-limit events from opaque 404s.
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}

	// Empty path values are an opaque failure. The route pattern requires all
	// three segments; this guards against a misconfigured mux.
	if namespace == "" || hostName == "" || nonce == "" {
		log.V(1).Info("boot GET: missing path values")
		h.opaqueFailure(w)
		return
	}

	ctx := r.Context()

	// 1. Get the PhysicalHost. NotFound and other errors are both opaque.
	ph := &infrastructurev1beta1.PhysicalHost{}
	if err := h.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: hostName}, ph); err != nil {
		log.V(1).Info("boot GET: PhysicalHost lookup failed", "err", err.Error())
		h.opaqueFailure(w)
		return
	}

	// 2. Verify the nonce. All three conditions must hold: BootNonceHash non-empty,
	// BootNonceExpiresAt in the future, and auth.Verify constant-time match.
	// Any failure is opaque — no oracle for "bad hash" vs "expired" vs "consumed".
	if !verifyBootNonce(nonce, ph) {
		log.V(1).Info("boot GET: nonce verification failed")
		h.opaqueFailure(w)
		return
	}

	// 3. Consume the nonce (D-010 — atomic single-use under optimistic lock).
	//
	// If already consumed (BootNonceConsumedAt != nil), this is a race loser or
	// a legitimate NIC retry. Skip the patch and fall through to render identical
	// content (§4.1 guarantees same response for same host regardless of order).
	//
	// D-010: this Status().Patch is the sole audited exception to D-005.
	// The field written (BootNonceConsumedAt) is owned exclusively by this
	// handler; no reconciler writes it, so the BUG-1 last-write-wins hazard
	// does not apply. A Conflict is the desired outcome (the winner consumed;
	// the loser confirms it and renders identically).
	if ph.Status.Bootstrap.BootNonceConsumedAt == nil {
		var consumeOK bool
		for attempt := 0; attempt < bootNonceConsumeMaxRetries; attempt++ {
			base := ph.DeepCopy()
			now := metav1.NewTime(time.Now())
			ph.Status.Bootstrap.BootNonceConsumedAt = &now

			// Status().Update is FORBIDDEN in handler files (D-005).
			// This single Status().Patch is the audited D-010 exception.
			patchErr := h.Client.Status().Patch(ctx, ph,
				client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
			if patchErr == nil {
				consumeOK = true
				break
			}
			if !apierrors.IsConflict(patchErr) {
				log.V(1).Info("boot GET: consume patch failed", "err", patchErr.Error())
				h.opaqueFailure(w)
				return
			}
			// Conflict: the object was mutated between our Get and our Patch.
			// Re-Get and re-evaluate.
			fresh := &infrastructurev1beta1.PhysicalHost{}
			if getErr := h.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: hostName}, fresh); getErr != nil {
				log.V(1).Info("boot GET: re-get after conflict failed", "err", getErr.Error())
				h.opaqueFailure(w)
				return
			}
			ph = fresh

			// If the fresh object is already consumed, we lost the race; render
			// identical content below.
			if ph.Status.Bootstrap != nil && ph.Status.Bootstrap.BootNonceConsumedAt != nil {
				consumeOK = true
				break
			}
			// If the nonce is no longer valid in the fresh copy (e.g. the
			// PhysicalHost reconciler cleared the hash), treat as opaque failure.
			if !verifyBootNonce(nonce, ph) {
				log.V(1).Info("boot GET: nonce no longer valid after conflict re-get")
				h.opaqueFailure(w)
				return
			}
		}
		if !consumeOK {
			// Exhausted retries without a durable consume or a confirmed
			// already-consumed state.
			log.V(1).Info("boot GET: consume retries exhausted")
			h.opaqueFailure(w)
			return
		}
	}

	// 4. Render — ONLY after consume is durable (or already-consumed confirmed).
	// renderBootScript is a pure function of (ph, machine, secret, config) so
	// the response is byte-identical on the fresh-consume and already-consumed
	// paths.
	script, err := h.renderBootScript(ctx, log, ph)
	if err != nil {
		log.V(1).Info("boot GET: render failed", "err", err.Error())
		h.opaqueFailure(w)
		return
	}

	log.Info("boot GET: served iPXE script")
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte(script)); err != nil {
		log.V(1).Info("boot GET: response write failed", "err", err.Error())
	}
}

// verifyBootNonce reports whether the nonce is valid for the given host.
// All three conditions must hold: BootNonceHash non-empty, BootNonceExpiresAt
// in the future, and auth.Verify constant-time match. Pulled out so it can be
// re-evaluated after an optimistic-lock conflict without duplicating logic.
//
// Deliberately does NOT check BootNonceConsumedAt — that check belongs in the
// consume path so the already-consumed branch can render identical content.
func verifyBootNonce(nonce string, ph *infrastructurev1beta1.PhysicalHost) bool {
	bs := ph.Status.Bootstrap
	if bs == nil || bs.BootNonceHash == "" {
		return false
	}
	if bs.BootNonceExpiresAt == nil || !time.Now().Before(bs.BootNonceExpiresAt.Time) {
		return false
	}
	return auth.Verify(nonce, bs.BootNonceHash)
}

// validateBootURL rejects values that could break out of a single
// space-delimited kernel cmdline parameter. Closes the cmdline-injection vector
// (SEC-7): a value with whitespace/control chars or a non-http(s) scheme could
// inject or override beskar7.* params rendered into the /boot iPXE script.
func validateBootURL(field, raw string) error {
	if strings.ContainsFunc(raw, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) {
		return fmt.Errorf("%s contains whitespace or control characters", field)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%s is not a valid http(s) URL", field)
	}
	return nil
}

// bootDigestPattern is compiled once at init time. It accepts only the
// canonical contract §5/§8.1 form: "sha256:" followed by exactly 64
// lowercase-hex characters. A value matching this pattern contains no
// whitespace or control characters, so the pattern match alone is a
// sufficient and stronger guard against cmdline injection (SEC-7 posture).
var bootDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// validateBootDigest rejects a digest that does not match the contract §5/§8.1
// canonical form. Defence-in-depth (SEC-7) on top of the CRD pattern: operates
// on the value actually rendered, not the value admitted.
func validateBootDigest(raw string) error {
	if !bootDigestPattern.MatchString(raw) {
		return fmt.Errorf("TargetImageDigest is not a valid sha256 digest (must match ^sha256:[0-9a-f]{64}$)")
	}
	return nil
}

// renderBootScript resolves the consuming Beskar7Machine, reads the bearer-token
// plaintext from the per-host Secret, and renders the §4.1 iPXE script.
//
// Pure function of (host, machine, secret, config): the rendered output is
// byte-identical on fresh-consume and already-consumed paths for the same host.
// No secret material is logged here; the caller logs only namespace + host +
// outcome.
func (h *BootHandler) renderBootScript(
	ctx context.Context,
	log logr.Logger,
	ph *infrastructurev1beta1.PhysicalHost,
) (string, error) {
	// Walk to the consuming Beskar7Machine via Spec.ConsumerRef.
	cr := ph.Spec.ConsumerRef
	if cr == nil || cr.Kind != "Beskar7Machine" || cr.APIVersion != InfrastructureAPIVersion {
		return "", fmt.Errorf("PhysicalHost %s/%s has no Beskar7Machine consumer", ph.Namespace, ph.Name)
	}
	b7m := &infrastructurev1beta1.Beskar7Machine{}
	if err := h.Client.Get(ctx, types.NamespacedName{Namespace: cr.Namespace, Name: cr.Name}, b7m); err != nil {
		return "", fmt.Errorf("get Beskar7Machine %s/%s: %w", cr.Namespace, cr.Name, err)
	}

	// InspectionImageURL is the base URL for vmlinuz + initrd.img (contract v2:
	// this field is the HTTPS base URL of a location serving the inspector
	// vmlinuz and initrd.img).
	if b7m.Spec.InspectionImageURL == "" {
		return "", fmt.Errorf("Beskar7Machine %s/%s has empty InspectionImageURL", b7m.Namespace, b7m.Name)
	}

	// Validate URL fields and the digest before rendering (SEC-7 / C-1a). A space
	// or control character in any value would break out of its cmdline parameter
	// and allow injection or override of subsequent beskar7.* parameters. This
	// check is defence-in-depth on top of the CRD pattern (C-1b); it is the
	// airtight fix because it operates on the value actually rendered, not the
	// value admitted.
	if err := validateBootURL("InspectionImageURL", b7m.Spec.InspectionImageURL); err != nil {
		return "", err
	}
	if err := validateBootURL("TargetImageURL", b7m.Spec.TargetImageURL); err != nil {
		return "", err
	}
	if err := validateBootDigest(b7m.Spec.TargetImageDigest); err != nil {
		return "", err
	}

	// Read the bearer-token plaintext from the per-host bootstrap-token Secret.
	// The handler received the {nonce} in the URL; it hands back the bearer token
	// in the rendered cmdline — these are two distinct secrets (§3, D-009).
	secret := &corev1.Secret{}
	secretKey := types.NamespacedName{Namespace: ph.Namespace, Name: bootstrapTokenSecretName(ph.Name)}
	if err := h.Client.Get(ctx, secretKey, secret); err != nil {
		return "", fmt.Errorf("get bootstrap-token Secret %s: %w", secretKey.Name, err)
	}
	tokenBytes, ok := secret.Data["plaintext-token"]
	if !ok || len(tokenBytes) == 0 {
		return "", fmt.Errorf("bootstrap-token Secret %s has no plaintext-token key", secretKey.Name)
	}

	// base64-encode the CA PEM for inline delivery via beskar7.ca=.
	caB64 := base64.StdEncoding.EncodeToString(h.Config.CABytes)

	script := buildBootIPXEScript(
		b7m.Spec.InspectionImageURL,
		h.Config.APIBase,
		ph.Namespace,
		ph.Name,
		string(tokenBytes),
		b7m.Spec.TargetImageURL,
		b7m.Spec.TargetImageDigest,
		caB64,
	)

	// Cap the rendered script length (SEC-10). The Linux kernel cmdline cap is
	// ~2–4 KiB; a script that exceeds bootScriptMaxBytes would silently truncate
	// on real hardware. This is operator misconfig (e.g. an over-large CA chain);
	// log at Info so it is operator-visible, then return an opaque failure.
	if len(script) > bootScriptMaxBytes {
		log.Info("boot GET: rendered script exceeds maximum allowed size; operator must shorten CA or URL fields",
			"scriptBytes", len(script), "maxBytes", bootScriptMaxBytes)
		return "", fmt.Errorf("rendered boot script (%d bytes) exceeds bootScriptMaxBytes (%d)", len(script), bootScriptMaxBytes)
	}

	_ = log // outcome is logged by the caller; no script or secret content logged here.

	return script, nil
}

// buildBootIPXEScript is a pure function that renders the §4.1 iPXE boot
// script. All inputs are caller-resolved; this function performs no I/O.
// Package-level so tests can call it directly for golden-string assertions.
//
// The parameter order on the kernel cmdline follows contract v2 §4.1 exactly:
// beskar7.api, beskar7.namespace, beskar7.host, beskar7.token, beskar7.target,
// beskar7.target-digest, beskar7.ca.
//
// Optional parameters (beskar7.timeout, beskar7.debug) are omitted in
// contract v2 — no operator UI to supply them yet.
func buildBootIPXEScript(
	inspectionImageURL string,
	apiBase string,
	namespace string,
	hostName string,
	token string,
	targetImageURL string,
	targetDigest string,
	caB64 string,
) string {
	return fmt.Sprintf(
		"#!ipxe\nkernel %s/vmlinuz beskar7.api=%s beskar7.namespace=%s beskar7.host=%s beskar7.token=%s beskar7.target=%s beskar7.target-digest=%s beskar7.ca=%s\ninitrd %s/initrd.img\nboot\n",
		inspectionImageURL,
		apiBase,
		namespace,
		hostName,
		token,
		targetImageURL,
		targetDigest,
		caB64,
		inspectionImageURL,
	)
}

// opaqueFailure writes the canonical §4.1 failure response. Every failure
// path calls this so the response is byte-identical regardless of cause.
// Mirrors the bootstrap_handler.go opaque-404 discipline.
func (h *BootHandler) opaqueFailure(w http.ResponseWriter) {
	http.Error(w, bootHandlerOpaqueFailureBody, bootHandlerOpaqueFailureStatus)
}

// allowIP returns true if the given client IP is within its rate-limit budget.
// Uses golang.org/x/time/rate (already an indirect dep via client-go), keyed
// by IP in a bounded sync.Map-style map. Stale entries are opportunistically
// evicted after bootIPRateLimiterTTL to bound memory growth.
func (h *BootHandler) allowIP(ip string) bool {
	h.ipLimitersMu.Lock()
	defer h.ipLimitersMu.Unlock()

	if h.ipLimiters == nil {
		h.ipLimiters = make(map[string]*ipEntry)
	}

	entry, ok := h.ipLimiters[ip]
	if !ok {
		entry = &ipEntry{
			limiter: rate.NewLimiter(bootIPRateLimitRPS, bootIPRateLimitBurst),
		}
		h.ipLimiters[ip] = entry
	}
	entry.lastSeen = time.Now()

	// Opportunistic eviction: scan for entries idle beyond TTL. Amortised O(n)
	// where n is the number of active IPs on the provisioning network; typically
	// small (one entry per host being booted concurrently).
	for k, e := range h.ipLimiters {
		if k != ip && time.Since(e.lastSeen) > bootIPRateLimiterTTL {
			delete(h.ipLimiters, k)
		}
	}

	return entry.limiter.Allow()
}

// remoteAddrToIP extracts the IP portion from an "ip:port" RemoteAddr.
// Falls back to the full string on parse failure so rate limiting still
// operates (the key is stable per connection either way).
func remoteAddrToIP(remoteAddr string) string {
	for i := len(remoteAddr) - 1; i >= 0; i-- {
		if remoteAddr[i] == ':' {
			return remoteAddr[:i]
		}
	}
	return remoteAddr
}
