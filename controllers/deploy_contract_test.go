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
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file closes the deploy-path (Phase 2) golden-fixture gap named in
// docs/inspector-contract.md §10 and designed in
// .claude/context/GA-P2-VALIDATION.md §1: unlike §6 (the inspection report,
// guarded by inspection_contract_test.go), the rendered /boot cmdline and the
// COS_OEM provider-id artifact previously had no byte-pinned fixture a second
// repo could mirror. It mirrors inspection_contract_test.go's conventions
// (plain *testing.T, golden files read from ../test/contract/).

const (
	// goldenBootCmdlinePath is the canonical byte-exact /boot render fixture,
	// shared with beskar7-inspector per test/contract/README.md's cross-repo
	// sync contract.
	goldenBootCmdlinePath = "../test/contract/golden_boot_cmdline.txt"

	// goldenProviderIDArtifactPath is the canonical descriptor of the
	// inspector-written /oem/beskar7/provider-id COS_OEM artifact.
	goldenProviderIDArtifactPath = "../test/contract/golden_provider_id_artifact.json"
)

// Fixed inputs for the golden /boot render (GA-P2-VALIDATION.md §1.3). These
// values are deliberately arbitrary-but-documented — the point of the golden
// test is that ANY drift in buildBootIPXEScript's param order, literal text,
// or the beskar7.provider-id slot fails loudly, not that these particular
// strings are meaningful.
const (
	goldenBootNamespace          = "contract-test"
	goldenBootHost               = "host-01"
	goldenBootToken              = "test-token-0123456789abcdef"
	goldenBootInspectionImageURL = "http://images.example.invalid"
	goldenBootAPIBase            = "https://beskar7.example.invalid:8082"
	goldenBootTargetImageURL     = "http://images.example.invalid/kairos.raw"
	goldenBootTargetDigest       = "sha256:" + goldenBootDigestHex
	goldenBootDigestHex          = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" // 64 * 'a'
	goldenBootCAB64              = "dGVzdC1jYS1wZW0tcGxhY2Vob2xkZXI="                                 // base64("test-ca-pem-placeholder")
	goldenBootDisk               = "/dev/vda"
	goldenBootBootif             = "01-52-54-00-12-34-56"
)

// TestBuildBootIPXEScript_GoldenCmdline is the byte-exact deploy-path guard
// (GA-P2-VALIDATION.md §1.4 row 1). It calls buildBootIPXEScript directly —
// no HTTP, no envtest — with the fixed inputs above, deriving the
// provider-id via the REAL providerID() function (not hand-typed), and
// asserts the output is byte-identical to the golden fixture. Any drift in
// param order, a renamed beskar7.* key, or a stray space fails this test.
func TestBuildBootIPXEScript_GoldenCmdline(t *testing.T) {
	golden, err := os.ReadFile(filepath.Clean(goldenBootCmdlinePath))
	if err != nil {
		t.Fatalf("read golden boot cmdline at %s: %v", goldenBootCmdlinePath, err)
	}

	// providerID(ns, host) — same call renderBootScript makes and
	// handleReadyHost uses to stamp Spec.ProviderID. NOT hand-typed, so this
	// test cannot itself introduce the divergence it's meant to prevent.
	pid := providerID(goldenBootNamespace, goldenBootHost)

	got := buildBootIPXEScript(
		goldenBootInspectionImageURL,
		goldenBootAPIBase,
		goldenBootNamespace,
		goldenBootHost,
		goldenBootToken,
		goldenBootTargetImageURL,
		goldenBootTargetDigest,
		pid,
		goldenBootCAB64,
		goldenBootDisk,
		goldenBootBootif,
		"", // no StaticIP in the golden fixture
	)

	if got != string(golden) {
		t.Errorf("buildBootIPXEScript output does not match golden fixture %s (byte drift).\n--- got ---\n%q\n--- want ---\n%q",
			goldenBootCmdlinePath, got, string(golden))
	}
}

// TestValidateProviderID_InjectionGuard table-tests validateProviderID
// (GA-P2-VALIDATION.md §1.4 row 2, GA-CONTRACT-FREEZE.md §3.4). Mirrors the
// validateBootDigest / validateStaticIP table-test style in
// controllers/boot_handler_test.go.
func TestValidateProviderID_InjectionGuard(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr bool
	}{
		// ── accept ──
		{name: "canonical form", input: "b7://default/worker-1", wantErr: false},
		{name: "hyphens in both segments", input: "b7://my-namespace/my-host-01", wantErr: false},
		{name: "dots in the name segment", input: "b7://default/host.example-01", wantErr: false},
		{name: "dots in the namespace segment", input: "b7://ns.sub/host", wantErr: false},
		{name: "digits in both segments", input: "b7://ns1/host2", wantErr: false},
		// ── reject ──
		{name: "embedded space", input: "b7://default/worker 1", wantErr: true},
		{name: "embedded tab", input: "b7://default/worker\t1", wantErr: true},
		{name: "embedded newline", input: "b7://default/worker\n1", wantErr: true},
		{name: "embedded NUL", input: "b7://default/worker\x001", wantErr: true},
		{name: "missing b7:// prefix", input: "default/worker-1", wantErr: true},
		{name: "wrong scheme prefix", input: "http://default/worker-1", wantErr: true},
		{name: "no slash separator", input: "b7://defaultworker1", wantErr: true},
		{name: "empty namespace", input: "b7:///worker-1", wantErr: true},
		{name: "empty host", input: "b7://default/", wantErr: true},
		{name: "empty string", input: "", wantErr: true},
		{name: "path-traversal shape (embedded ../ extra segment)", input: "b7://default/../etc/passwd", wantErr: true},
		{name: "path-traversal shape (namespace side)", input: "b7://../etc/worker-1", wantErr: true},
		{name: "uppercase namespace (RFC1123 is lowercase)", input: "b7://Default/worker-1", wantErr: true},
		{name: "uppercase host (RFC1123 is lowercase)", input: "b7://default/Worker-1", wantErr: true},
		{name: "extra path segment (name contains slash)", input: "b7://default/worker-1/extra", wantErr: true},
		{name: "injection: trailing beskar7.x= via space", input: "b7://default/worker-1 beskar7.x=ATTACKER", wantErr: true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := validateProviderID(tc.input)
			if tc.wantErr && err == nil {
				t.Errorf("validateProviderID(%q): expected an error, got nil", tc.input)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("validateProviderID(%q): expected no error, got %v", tc.input, err)
			}
		})
	}
}

// goldenProviderIDArtifact mirrors the JSON shape of
// test/contract/golden_provider_id_artifact.json (GA-P2-VALIDATION.md §1.3).
// Field order/names must stay in lockstep with the fixture and with the
// beskar7-inspector repo's Rust deserialization of the same bytes.
type goldenProviderIDArtifact struct {
	Namespace                 string `json:"namespace"`
	Host                      string `json:"host"`
	ArtifactPath              string `json:"artifactPath"`
	Content                   string `json:"content"`
	ContentHasTrailingNewline bool   `json:"contentHasTrailingNewline"`
	Mode                      string `json:"mode"`
	Owner                     string `json:"owner"`
}

// TestGoldenProviderIDArtifact_MatchesComputedValue is the beskar7-executable
// half of the artifact-side dual-repo guard (GA-P2-VALIDATION.md §1.4 row 5,
// §1.7). It cannot prove the inspector writes the artifact correctly (that
// requires a real COS_OEM-mounted block device — out of reach for Go CI, see
// §1.7) but it proves the fixture's expected content is not hand-typed drift
// from what the controller's real providerID() actually computes.
func TestGoldenProviderIDArtifact_MatchesComputedValue(t *testing.T) {
	raw, err := os.ReadFile(filepath.Clean(goldenProviderIDArtifactPath))
	if err != nil {
		t.Fatalf("read golden provider-id artifact at %s: %v", goldenProviderIDArtifactPath, err)
	}

	var fixture goldenProviderIDArtifact
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("decode golden provider-id artifact: %v", err)
	}

	want := providerID(fixture.Namespace, fixture.Host)
	if fixture.Content != want {
		t.Errorf("golden_provider_id_artifact.json content %q does not equal providerID(%q, %q) = %q",
			fixture.Content, fixture.Namespace, fixture.Host, want)
	}

	if fixture.ArtifactPath != "/oem/beskar7/provider-id" {
		t.Errorf("artifactPath = %q, want %q (contract v4.2 §3.4)", fixture.ArtifactPath, "/oem/beskar7/provider-id")
	}
	if fixture.ContentHasTrailingNewline {
		t.Errorf("contentHasTrailingNewline = true, want false — the artifact MUST NOT carry a trailing newline (contract v4.2 §3.4)")
	}
	if fixture.Mode != "0600" {
		t.Errorf("mode = %q, want %q", fixture.Mode, "0600")
	}
	if fixture.Owner != "root" {
		t.Errorf("owner = %q, want %q", fixture.Owner, "root")
	}
	if strings.Contains(fixture.Content, "\n") {
		t.Errorf("content %q contains an embedded newline — the artifact bytes MUST be exactly the provider-id string", fixture.Content)
	}
}
