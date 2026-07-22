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

package contract

import (
	"os"
	"strings"
	"testing"
)

// TestContractVersion is the intra-repo half of the cross-repo contract-sync
// guard documented in README.md: beskar7 is the single source of truth for
// the contract, and this test proves it is self-consistent by asserting the
// Version const equals the plain-text VERSION file it mirrors. The
// beskar7-inspector repo runs the other half in its own CI — it vendors a
// byte-copy of VERSION at a pinned contract/<version> tag and asserts its
// Rust CONTRACT_VERSION equals the vendored copy. Neither side can see the
// other's check; this one only catches a desync between beskar7's own const
// and its own fixture file.
func TestContractVersion(t *testing.T) {
	b, err := os.ReadFile("VERSION")
	if err != nil {
		t.Fatalf("read test/contract/VERSION: %v", err)
	}

	got := strings.TrimSpace(string(b))
	if got != Version {
		t.Errorf("contract.Version (%q) does not match test/contract/VERSION contents (%q) — "+
			"bump both together in the same commit, then push the contract/%s tag "+
			"(see README.md \"Release checklist\")", Version, got, got)
	}
}
