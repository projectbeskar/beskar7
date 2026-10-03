/*
Copyright 2026 The Beskar7 Authors.

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

package main

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// parseWatchNamespaces parses a --watch-namespaces flag value into a
// deduplicated, sorted list of namespace names. Empty entries and pure
// whitespace are dropped. Input is comma-separated; whitespace around each
// entry is trimmed.
//
// Returns an empty slice for an empty / whitespace-only input — the caller
// interprets that as "watch all namespaces" (current default).
//
// Sorting is for determinism so two equivalent flag values produce identical
// cache.Options.DefaultNamespaces maps when iterated.
func parseWatchNamespaces(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}

	seen := make(map[string]struct{})
	for _, part := range strings.Split(raw, ",") {
		ns := strings.TrimSpace(part)
		if ns == "" {
			continue
		}
		seen[ns] = struct{}{}
	}

	if len(seen) == 0 {
		return nil
	}

	out := make([]string, 0, len(seen))
	for ns := range seen {
		out = append(out, ns)
	}
	sort.Strings(out)
	return out
}

// parseBMCProxy parses a --bmc-proxy flag value: the one proxy every BMC
// connection goes through (D-035). Empty means none, and returns nil.
//
// The value is an http:// or https:// URL with a host and an optional port, and
// optional user:password@ for a proxy that wants credentials. A path other than
// "/", a query and a fragment are rejected.
//
// Every error is a fixed message that repeats nothing of the value. The value
// may carry the proxy's password, url.Parse's own errors quote the whole URL,
// and even a single piece of a mistyped value can be a secret: user:pw@host:3128
// parses with the user name as its scheme.
func parseBMCProxy(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, invalidBMCProxy("it is not a URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, invalidBMCProxy("the scheme must be http or https")
	}
	if u.Hostname() == "" {
		return nil, invalidBMCProxy("a host is required")
	}
	if strings.ContainsAny(raw, "?#") || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, invalidBMCProxy("a path, query or fragment is not allowed")
	}
	if port := u.Port(); port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return nil, invalidBMCProxy("the port must be between 1 and 65535")
		}
	}
	return u, nil
}

func invalidBMCProxy(reason string) error {
	return fmt.Errorf("invalid --bmc-proxy: %s; expected http://[user:password@]host[:port] or https://[user:password@]host[:port]", reason)
}

// proxyEnvironmentVariables returns the names, never the values, of the
// proxy variables net/http reads that getenv reports set. BMC connections
// ignore them (D-035); the manager says so at startup, so an operator who
// relied on one finds out from the log and not from unreachable BMCs.
func proxyEnvironmentVariables(getenv func(string) string) []string {
	var set []string
	for _, name := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy"} {
		if getenv(name) != "" {
			set = append(set, name)
		}
	}
	return set
}

// controllersMode is the value of the --controllers flag: which reconcilers
// a manager instance registers.
type controllersMode string

const (
	// controllersAll runs every reconciler plus the callback server — the
	// normal in-cluster manager.
	controllersAll controllersMode = "all"

	// controllersNone is callback-only. The instance registers no reconciler
	// and no webhook; it serves the host-callback HTTPS endpoints and the
	// health probes on top of the same cached client the handlers always
	// use. That lets a copy of the manager sit on the provisioning network
	// (where PXE-booting hosts can reach it) without competing with the
	// in-cluster controllers for host claims or for the bootstrap-url
	// annotation handshake — two full managers race on both.
	controllersNone controllersMode = "none"
)

// parseControllersMode parses a --controllers flag value. Matching ignores
// case and surrounding whitespace; anything other than "all" or "none" is an
// error so a typo is a startup failure rather than a silently-full manager.
func parseControllersMode(raw string) (controllersMode, error) {
	switch mode := controllersMode(strings.ToLower(strings.TrimSpace(raw))); mode {
	case controllersAll, controllersNone:
		return mode, nil
	default:
		return "", fmt.Errorf("invalid --controllers value %q: must be %q or %q", raw, controllersAll, controllersNone)
	}
}

// managerConfig is the parsed command line as far as the wiring in
// setupManager and the flag-compatibility checks in validate care about it.
// main() fills it from the flags; tests build it directly.
type managerConfig struct {
	controllers controllersMode

	// enableLeaderElection is --leader-elect as parsed. leaderElectSet records
	// whether the flag was given on the command line at all, which validate
	// needs to tell the default apart from an operator asking for it.
	enableLeaderElection bool
	leaderElectSet       bool

	enableWebhook bool

	// bmcProxy is the parsed --bmc-proxy; nil means BMC connections are direct.
	bmcProxy *url.URL

	bootstrapURLBase  string
	inspectionPort    int
	inspectionCertDir string
	trustedProxies    []*net.IPNet

	inspectionTimeout       time.Duration
	deploymentTimeout       time.Duration
	maxConcurrentReconciles int
}

// validate rejects flag combinations that contradict each other. It runs
// after flag.Parse and before the manager is built, so a bad command line
// fails at startup with a message naming both flags instead of producing a
// process that looks healthy and misbehaves.
func (c managerConfig) validate() error {
	if c.controllers != controllersNone {
		return nil
	}
	if c.enableWebhook {
		return errors.New("--enable-webhook=true contradicts --controllers=none: a callback-only " +
			"instance registers no webhook, so admission requests routed to it would fail closed; " +
			"drop --enable-webhook")
	}
	if c.bmcProxy != nil {
		return errors.New("--bmc-proxy contradicts --controllers=none: a callback-only " +
			"instance never connects to a BMC, so the proxy would go unused; drop --bmc-proxy")
	}
	if c.leaderElectSet && c.enableLeaderElection {
		return errors.New("--leader-elect=true contradicts --controllers=none: a callback-only " +
			"instance runs no leader-elected work, and holding the lease would keep the real " +
			"controller from ever becoming leader; drop --leader-elect or pass --leader-elect=false")
	}
	return nil
}

// leaderElection is the effective leader-election setting. A callback-only
// instance never takes part: it has nothing to lead, and if it won the lease
// the full manager would sit idle as a non-leader with nothing reconciling.
func (c managerConfig) leaderElection() bool {
	if c.controllers == controllersNone {
		return false
	}
	return c.enableLeaderElection
}
