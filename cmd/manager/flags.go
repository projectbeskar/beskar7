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
	"sort"
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
