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
	"sort"
	"strings"
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
