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
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
)

// Resolving spec.targetImageDigestURL (D-038).
//
// The OS image is fetched over plain HTTP, so its digest is the only thing that
// vouches for it. Naming the digest by URL moves that trust to whoever controls
// the checksum file, and gives whoever can create a Beskar7Machine a way to make
// the manager open an HTTPS connection to a host of their choosing. The code here
// is written for that second fact: the fetch is small, bounded and verified, and
// nothing the server sends is ever repeated back (see digestError).

const (
	// digestFetchTimeout bounds the whole fetch: connecting, the TLS handshake,
	// any redirects, and reading the body.
	digestFetchTimeout = 10 * time.Second

	// digestFetchMaxBytes is the largest checksum file accepted. A larger one is
	// an error, and is never read past this size.
	digestFetchMaxBytes = 64 << 10

	// digestFetchMaxRedirects is the most redirects one fetch follows.
	digestFetchMaxRedirects = 3

	// digestRetryMin and digestRetryMax bound the wait between attempts to
	// resolve a digest that does not resolve: it starts at the former and doubles
	// per failed attempt up to the latter.
	digestRetryMin = 30 * time.Second
	digestRetryMax = 5 * time.Minute

	// digestPinMemory is how long a digest this process has resolved is reused for
	// the same machine and URL while status does not show it yet. The pin reaches
	// the reconciler's cache a moment after the pass that wrote it, and a pass that
	// runs in that gap would otherwise fetch the file a second time, possibly to a
	// different digest.
	digestPinMemory = 2 * time.Minute
)

// digestError is a failure to resolve the digest. Its text is published in a
// condition and a log line, so it is fixed wording plus things that come from the
// spec (the URL's host, the image's file name), and never anything the server
// sent: no body, no header, no TLS error text.
type digestError struct {
	// host is the checksum URL's host:port, empty when the URL did not parse.
	host string
	// reason is the fixed wording.
	reason string
}

func (e *digestError) Error() string {
	if e.host == "" {
		return e.reason
	}
	return e.host + ": " + e.reason
}

func digestFail(host, format string, args ...any) *digestError {
	return &digestError{host: host, reason: fmt.Sprintf(format, args...)}
}

var (
	errRedirectOffHost  = errors.New("redirect to another host refused")
	errTooManyRedirects = errors.New("too many redirects")
)

// newChecksumClient returns the HTTP client for the checksum fetch. It is its
// own client, not http.DefaultClient: HTTPS only (the redirect policy and the
// caller keep a plain-HTTP hop out), verified TLS 1.2 or later against roots
// (nil means the system roots), no connection reuse, no content decoding, and
// the redirect limits of checkChecksumRedirect.
//
// The environment proxy applies. This fetch carries no credential and the file
// it reads is public; D-035's refusal of the environment proxy is about BMC
// connections, whose credentials and names it would see.
func newChecksumClient(roots *x509.CertPool) *http.Client {
	return &http.Client{
		Timeout:       digestFetchTimeout,
		CheckRedirect: checkChecksumRedirect,
		Transport: &http.Transport{
			Proxy:                  http.ProxyFromEnvironment,
			TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
			TLSHandshakeTimeout:    digestFetchTimeout,
			ResponseHeaderTimeout:  digestFetchTimeout,
			MaxResponseHeaderBytes: 16 << 10,
			DisableKeepAlives:      true,
			DisableCompression:     true,
		},
	}
}

// checkChecksumRedirect allows at most digestFetchMaxRedirects redirects, each
// to the scheme, host and port of the request that started the fetch. A redirect
// anywhere else would let a checksum server steer the manager's connection to a
// host the Beskar7Machine's author never named.
func checkChecksumRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > digestFetchMaxRedirects {
		return errTooManyRedirects
	}
	if !sameOrigin(req.URL, via[0].URL) {
		return errRedirectOffHost
	}
	return nil
}

func sameOrigin(a, b *url.URL) bool {
	return a.Scheme == b.Scheme &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		effectivePort(a) == effectivePort(b)
}

func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}

// parseDigestURL accepts only an https URL with a host and no userinfo. The CRD
// pattern already requires https://; this is the check that does not depend on
// admission having run.
func parseDigestURL(raw string) (*url.URL, *digestError) {
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Host == "" {
		return nil, digestFail("", "targetImageDigestURL is not an https URL")
	}
	if u.User != nil {
		return nil, digestFail(u.Host, "targetImageDigestURL must not contain credentials")
	}
	u.Scheme = "https"
	return u, nil
}

// redactedURL is raw without userinfo, query or fragment, for logs.
func redactedURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "<unparseable URL>"
	}
	return u.Scheme + "://" + u.Host + u.EscapedPath()
}

// imageFileName is the name a checksum entry must carry to be the image's: the
// last segment of targetImageURL's path, unescaped.
func imageFileName(targetImageURL string) (string, *digestError) {
	u, err := url.Parse(targetImageURL)
	if err != nil || u.Path == "" || strings.HasSuffix(u.Path, "/") {
		return "", digestFail("", "targetImageURL has no file name to look up")
	}
	name := path.Base(u.Path)
	if name == "." || name == ".." || name == "/" || name == "" {
		return "", digestFail("", "targetImageURL has no file name to look up")
	}
	return name, nil
}

// fetchChecksumFile GETs u and returns its body, at most digestFetchMaxBytes of
// it. Every failure is a digestError with fixed wording.
func fetchChecksumFile(ctx context.Context, client *http.Client, u *url.URL) ([]byte, *digestError) {
	ctx, cancel := context.WithTimeout(ctx, digestFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, digestFail(u.Host, "cannot build the request")
	}
	req.Header.Set("Accept", "text/plain, */*;q=0.1")
	req.Header.Set("User-Agent", "beskar7-controller")

	resp, err := client.Do(req)
	if err != nil {
		return nil, digestFail(u.Host, "%s", describeFetchError(err))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, digestFail(u.Host, "HTTP %d", resp.StatusCode)
	}
	tooLarge := digestFail(u.Host, "checksum file too large (limit %d KiB)", digestFetchMaxBytes>>10)
	if resp.ContentLength > digestFetchMaxBytes {
		return nil, tooLarge
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, digestFetchMaxBytes+1))
	if err != nil {
		return nil, digestFail(u.Host, "%s", describeFetchError(err))
	}
	if len(body) > digestFetchMaxBytes {
		return nil, tooLarge
	}
	return body, nil
}

// describeFetchError names why a request failed in fixed wording. The text of
// err is never reported, only looked at for the one message the standard library
// gives a plain-HTTP server: it can carry the URL's query, and TLS and proxy
// errors can carry what the peer sent.
func describeFetchError(err error) string {
	var (
		unknownAuthority x509.UnknownAuthorityError
		wrongHost        x509.HostnameError
		invalidCert      x509.CertificateInvalidError
		notTLS           tls.RecordHeaderError
		dnsErr           *net.DNSError
		opErr            *net.OpError
		timeout          interface{ Timeout() bool }
	)
	switch {
	case errors.Is(err, errRedirectOffHost):
		return "redirect to another host refused"
	case errors.Is(err, errTooManyRedirects):
		return fmt.Sprintf("more than %d redirects", digestFetchMaxRedirects)
	case errors.As(err, &unknownAuthority):
		return "TLS certificate not trusted"
	case errors.As(err, &wrongHost):
		return "TLS certificate is for a different host"
	case errors.As(err, &invalidCert):
		return "TLS certificate is expired or not valid"
	case errors.As(err, &notTLS), strings.Contains(err.Error(), "server gave HTTP response to HTTPS client"):
		return "not an HTTPS server"
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()):
		return fmt.Sprintf("timed out after %s", digestFetchTimeout)
	case errors.Is(err, context.Canceled):
		return "request canceled"
	case errors.As(err, &dnsErr):
		return "cannot resolve the host name"
	case errors.As(err, &opErr):
		return "cannot connect"
	}
	return "request failed"
}

// bsdChecksumLine recognises a BSD line; splitChecksumLine reads everything else
// as a coreutils line.
var bsdChecksumLine = regexp.MustCompile(`^SHA256\s*\((.+)\)\s*=\s*(\S+)$`)

// splitChecksumLine reads one line of a checksum file as the file name it
// names and the digest text it gives. Two layouts are understood: GNU coreutils
// ("<hash>  <name>", "<hash> *<name>") and BSD ("SHA256 (<name>) = <hash>").
// Whether the digest text is a SHA-256 digest is for the caller to decide.
func splitChecksumLine(line string) (name, hash string, ok bool) {
	if m := bsdChecksumLine.FindStringSubmatch(line); m != nil {
		return m[1], m[2], true
	}
	i := strings.IndexAny(line, " \t")
	if i < 0 {
		return "", "", false
	}
	hash = line[:i]
	name = strings.TrimPrefix(strings.TrimLeft(line[i:], " \t"), "*")
	return name, hash, name != ""
}

func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// parseChecksumFile finds the SHA-256 digest of the file called name in the
// text of a checksum file, as "sha256:<lowercase hex>".
//
// A file that is one bare 64-hex digest, whitespace aside, is that digest.
// Otherwise each line is a GNU or BSD entry (splitChecksumLine); blank lines and
// lines starting with # are skipped, a line in neither layout names nothing, and
// the entry used is the one whose name, less a leading "./", is name. It is an
// error for none to match, for a match not to give a SHA-256 digest, and for two
// matches to give different digests. Two matches that agree are fine.
//
// The errors name the file (name comes from the spec) and nothing of the text.
func parseChecksumFile(body []byte, name, host string) (string, *digestError) {
	text := strings.TrimSpace(string(body))
	if isSHA256Hex(text) {
		return "sha256:" + strings.ToLower(text), nil
	}
	var hashes []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		entry, hash, ok := splitChecksumLine(line)
		if ok && strings.TrimPrefix(entry, "./") == name {
			hashes = append(hashes, hash)
		}
	}
	if len(hashes) == 0 {
		return "", digestFail(host, "no entry for %q", name)
	}
	var digest string
	for _, h := range hashes {
		if !isSHA256Hex(h) {
			return "", digestFail(host, "the entry for %q is not a SHA-256 digest", name)
		}
		h = strings.ToLower(h)
		if digest != "" && digest != h {
			return "", digestFail(host, "conflicting entries for %q", name)
		}
		digest = h
	}
	return "sha256:" + digest, nil
}

// fetchTargetImageDigest reads the checksum file at digestURL and returns the
// digest of the image at targetImageURL.
func fetchTargetImageDigest(ctx context.Context, client *http.Client, digestURL, targetImageURL string) (string, *digestError) {
	u, derr := parseDigestURL(digestURL)
	if derr != nil {
		return "", derr
	}
	name, derr := imageFileName(targetImageURL)
	if derr != nil {
		derr.host = u.Host
		return "", derr
	}
	body, derr := fetchChecksumFile(ctx, client, u)
	if derr != nil {
		return "", derr
	}
	digest, derr := parseChecksumFile(body, name, u.Host)
	if derr != nil {
		return "", derr
	}
	if err := validateBootDigest(digest); err != nil {
		return "", digestFail(u.Host, "the entry for %q is not a SHA-256 digest", name)
	}
	return digest, nil
}

// digestRetry is what the reconciler remembers about a digest that did not
// resolve, so it asks the checksum server no more often than the backoff allows
// however often the machine is reconciled.
type digestRetry struct {
	uid      types.UID
	url      string
	failures int
	next     time.Time
	message  string
}

// digestPin is a digest this process resolved and has written to status, kept
// for digestPinMemory (see there).
type digestPin struct {
	uid    types.UID
	url    string
	digest string
	at     time.Time
}

// digestRetryDelay is the wait after the nth failed attempt in a row.
func digestRetryDelay(failures int) time.Duration {
	d := digestRetryMin
	for i := 1; i < failures && d < digestRetryMax; i++ {
		d *= 2
	}
	return min(d, digestRetryMax)
}

// effectiveTargetImageDigest is the digest /boot renders for b7m: the spec's
// when it names one, else the one the controller pinned from the checksum URL.
// It is not validated here; the caller does (validateBootDigest).
func effectiveTargetImageDigest(b7m *infrav1.Beskar7Machine) string {
	if b7m.Spec.TargetImageDigest != "" {
		return b7m.Spec.TargetImageDigest
	}
	return b7m.Status.TargetImageDigest
}

func (r *Beskar7MachineReconciler) checksumClient() *http.Client {
	r.checksumClientOnce.Do(func() { r.checksumHTTPClient = newChecksumClient(r.ChecksumRootCAs) })
	return r.checksumHTTPClient
}

// forgetTargetImageDigest drops what the reconciler remembers, in memory, of a
// machine's digest: its backoff and its recent resolution.
func (r *Beskar7MachineReconciler) forgetTargetImageDigest(key types.NamespacedName) {
	r.digestRetries.Delete(key)
	r.digestPins.Delete(key)
}

// ensureTargetImageDigest makes the digest of a machine's image known before a
// host is claimed, when the machine names it by URL (D-038). proceed is true
// when the reconcile may go on to claim a host. When it is false the returned
// result and error are the reconcile's, and no host has been claimed.
//
//   - spec.targetImageDigest set: there is nothing to resolve and nothing is
//     fetched. A pin left over from before the spec changed is dropped, so
//     status never shows a digest /boot does not use.
//   - spec.providerID set: the machine's host reached Ready, so the digest is no
//     longer needed (a Ready host is never booted again for this machine, D-028),
//     and a status that a clusterctl move dropped need not be rebuilt. Nothing is
//     fetched.
//   - a pin made from this very URL: kept. The file is not read again, whatever
//     it holds now.
//   - a pin made from another URL: the spec changed after the pin. If a host is
//     claimed the run keeps the pin, and says so in a log line; if none is, the
//     pin is dropped and the new URL resolved, because nothing has been booted
//     with it.
//   - no pin: the URL is fetched. On success the digest is pinned and the pass
//     ends with a short requeue, so the pin is persisted before any host is
//     claimed; a pass that reads a status older than that write reuses the digest
//     it resolved (digestPin) instead of fetching again. On failure the machine reports InfrastructureReady=False with
//     reason WaitingForTargetImageDigest and retries with a backoff of
//     digestRetryMin to digestRetryMax; it is not a terminal failure, since
//     the operator can fix the file or the URL.
//
// A machine that holds a host but has lost its pin (status wiped by a restore)
// is in the last case: the pin is rebuilt from the spec, because /boot cannot
// render the machine without one.
func (r *Beskar7MachineReconciler) ensureTargetImageDigest(ctx context.Context, logger logr.Logger, b7machine *infrav1.Beskar7Machine) (ctrl.Result, bool, error) {
	spec, status := &b7machine.Spec, &b7machine.Status
	key := types.NamespacedName{Namespace: b7machine.Namespace, Name: b7machine.Name}

	if spec.TargetImageDigest != "" {
		status.TargetImageDigest, status.TargetImageDigestURL = "", ""
		r.forgetTargetImageDigest(key)
		return ctrl.Result{}, true, nil
	}
	if spec.ProviderID != "" {
		r.forgetTargetImageDigest(key)
		clearWaitingForTargetImageDigest(b7machine)
		return ctrl.Result{}, true, nil
	}

	if status.TargetImageDigest != "" {
		if status.TargetImageDigestURL == spec.TargetImageDigestURL {
			r.digestPins.Delete(key)
			clearWaitingForTargetImageDigest(b7machine)
			return ctrl.Result{}, true, nil
		}
		held, err := r.findClaimedHostForRelease(ctx, logger, b7machine)
		if err != nil {
			return ctrl.Result{}, false, fmt.Errorf("check whether the machine holds a host: %w", err)
		}
		if held != nil {
			logger.Info("spec.targetImageDigestURL changed after a host was claimed; keeping the pinned digest, which only a new machine takes up",
				"pinnedFrom", redactedURL(status.TargetImageDigestURL), "specURL", redactedURL(spec.TargetImageDigestURL), "host", held.Name)
			clearWaitingForTargetImageDigest(b7machine)
			return ctrl.Result{}, true, nil
		}
		logger.Info("spec.targetImageDigestURL changed before a host was claimed; resolving the new one",
			"pinnedFrom", redactedURL(status.TargetImageDigestURL), "specURL", redactedURL(spec.TargetImageDigestURL))
		status.TargetImageDigest, status.TargetImageDigestURL = "", ""
	}

	now := time.Now()
	if v, ok := r.digestPins.Load(key); ok {
		if p, ok := v.(digestPin); ok && p.uid == b7machine.UID && p.url == spec.TargetImageDigestURL && now.Sub(p.at) < digestPinMemory {
			status.TargetImageDigest, status.TargetImageDigestURL = p.digest, p.url
			clearWaitingForTargetImageDigest(b7machine)
			return ctrl.Result{RequeueAfter: requeueShortly}, false, nil
		}
	}
	var prev digestRetry
	if v, ok := r.digestRetries.Load(key); ok {
		if p, ok := v.(digestRetry); ok && p.uid == b7machine.UID && p.url == spec.TargetImageDigestURL {
			prev = p
		}
	}
	if prev.failures > 0 && now.Before(prev.next) {
		setFalse(b7machine, infrav1.InfrastructureReadyCondition, infrav1.WaitingForTargetImageDigestReason, "%s", prev.message)
		return ctrl.Result{RequeueAfter: prev.next.Sub(now)}, false, nil
	}

	digest, derr := r.fetchDigest(ctx, spec)
	if derr != nil {
		failures := prev.failures + 1
		delay := digestRetryDelay(failures)
		msg := fmt.Sprintf("Cannot resolve spec.targetImageDigestURL, retrying: %s", derr)
		r.digestRetries.Store(key, digestRetry{uid: b7machine.UID, url: spec.TargetImageDigestURL, failures: failures, next: now.Add(delay), message: msg})
		logger.Info("Cannot resolve the image digest from spec.targetImageDigestURL",
			"url", redactedURL(spec.TargetImageDigestURL), "reason", derr.reason, "attempt", failures, "retryIn", delay.String())
		setFalse(b7machine, infrav1.InfrastructureReadyCondition, infrav1.WaitingForTargetImageDigestReason, "%s", msg)
		return ctrl.Result{RequeueAfter: delay}, false, nil
	}

	r.digestRetries.Delete(key)
	r.digestPins.Store(key, digestPin{uid: b7machine.UID, url: spec.TargetImageDigestURL, digest: digest, at: now})
	status.TargetImageDigest, status.TargetImageDigestURL = digest, spec.TargetImageDigestURL
	clearWaitingForTargetImageDigest(b7machine)
	logger.Info("Resolved and pinned the image digest from spec.targetImageDigestURL", "url", redactedURL(spec.TargetImageDigestURL))
	return ctrl.Result{RequeueAfter: requeueShortly}, false, nil
}

// fetchDigest resolves spec's digest URL, or reports that it has none.
func (r *Beskar7MachineReconciler) fetchDigest(ctx context.Context, spec *infrav1.Beskar7MachineSpec) (string, *digestError) {
	if spec.TargetImageDigestURL == "" {
		return "", digestFail("", "neither spec.targetImageDigest nor spec.targetImageDigestURL is set")
	}
	return fetchTargetImageDigest(ctx, r.checksumClient(), spec.TargetImageDigestURL, spec.TargetImageURL)
}

// clearWaitingForTargetImageDigest removes InfrastructureReady when it reports
// that the digest is awaited, so the condition does not outlive the wait. The
// claim path sets InfrastructureReady where it is missing, and the Ready summary
// ignores a missing condition.
func clearWaitingForTargetImageDigest(b7machine *infrav1.Beskar7Machine) {
	if conditions.GetReason(b7machine, infrav1.InfrastructureReadyCondition) == infrav1.WaitingForTargetImageDigestReason {
		conditions.Delete(b7machine, infrav1.InfrastructureReadyCondition)
	}
}
