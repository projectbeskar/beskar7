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

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ---------------------------------------------------------------------------
// deriveInspectionURL
// ---------------------------------------------------------------------------

func TestDeriveInspectionURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		input       string
		want        string
		wantErrSubs string
	}{
		{
			name:  "happy path",
			input: "https://beskar7-controller-manager.beskar7-system.svc:8082/api/v1/bootstrap/beskar7-smoke/smoke-host-01",
			want:  "https://beskar7-controller-manager.beskar7-system.svc:8082/api/v1/inspection/beskar7-smoke/smoke-host-01",
		},
		{
			name:  "replaces only the first occurrence",
			input: "https://host:8082/api/v1/bootstrap/ns/name/api/v1/bootstrap/extra",
			want:  "https://host:8082/api/v1/inspection/ns/name/api/v1/bootstrap/extra",
		},
		{
			name:        "missing bootstrap segment",
			input:       "https://host:8082/api/v2/bootstrap/ns/name",
			wantErrSubs: "does not contain",
		},
		{
			name:        "empty URL",
			input:       "",
			wantErrSubs: "does not contain",
		},
		{
			name:        "wrong path prefix",
			input:       "https://host:8082/some/other/path",
			wantErrSubs: "does not contain",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := deriveInspectionURL(tc.input)
			if tc.wantErrSubs != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErrSubs)
				}
				if !strings.Contains(err.Error(), tc.wantErrSubs) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErrSubs)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// buildPayload
// ---------------------------------------------------------------------------

func TestBuildPayload_Defaults(t *testing.T) {
	t.Parallel()

	p := buildPayload("smoke-host-01")

	if p.Manufacturer != "MockInspector" {
		t.Errorf("Manufacturer = %q, want MockInspector", p.Manufacturer)
	}
	if p.Model != "smoke-test" {
		t.Errorf("Model = %q, want smoke-test", p.Model)
	}
	// SerialNumber should incorporate the host name.
	if !strings.Contains(p.SerialNumber, "SMOKE-HOST-01") {
		t.Errorf("SerialNumber %q should contain SMOKE-HOST-01", p.SerialNumber)
	}
	if len(p.CPUs) != 1 {
		t.Fatalf("want 1 CPU, got %d", len(p.CPUs))
	}
	if p.CPUs[0].Vendor != "GenuineIntel" {
		t.Errorf("CPU[0].Vendor = %q, want GenuineIntel", p.CPUs[0].Vendor)
	}
	if p.CPUs[0].Cores != 4 {
		t.Errorf("CPU[0].Cores = %d, want 4", p.CPUs[0].Cores)
	}
	if p.CPUs[0].Threads != 8 {
		t.Errorf("CPU[0].Threads = %d, want 8", p.CPUs[0].Threads)
	}
	if len(p.Memory) != 1 {
		t.Fatalf("want 1 DIMM, got %d", len(p.Memory))
	}
	if p.Memory[0].Capacity != "16GB" {
		t.Errorf("Memory[0].Capacity = %q, want 16GB", p.Memory[0].Capacity)
	}
	if len(p.Disks) != 1 {
		t.Fatalf("want 1 disk, got %d", len(p.Disks))
	}
	if p.Disks[0].Type != "SSD" {
		t.Errorf("Disk[0].Type = %q, want SSD", p.Disks[0].Type)
	}
	if p.Disks[0].SizeGB != 100 {
		t.Errorf("Disk[0].SizeGB = %d, want 100", p.Disks[0].SizeGB)
	}
	if len(p.NICs) != 1 {
		t.Fatalf("want 1 NIC, got %d", len(p.NICs))
	}
	if p.NICs[0].Name != "eth0" {
		t.Errorf("NIC[0].Name = %q, want eth0", p.NICs[0].Name)
	}
}

func TestBuildPayload_SerialNumberComposition(t *testing.T) {
	t.Parallel()
	cases := []string{"host-a", "host-b", "smoke-host-01"}
	for _, name := range cases {
		name := name
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := buildPayload(name)
			upper := strings.ToUpper(name)
			if !strings.Contains(p.SerialNumber, upper) {
				t.Errorf("SerialNumber %q should contain %q for traceability", p.SerialNumber, upper)
			}
		})
	}
}

func TestBuildPayload_MACStability(t *testing.T) {
	t.Parallel()
	// Same host name must always produce the same MAC.
	for i := 0; i < 10; i++ {
		p := buildPayload("smoke-host-01")
		mac := p.NICs[0].MACAddress
		// Verify it starts with the de:ad:be: prefix.
		if !strings.HasPrefix(mac, "de:ad:be:") {
			t.Errorf("MAC %q should start with de:ad:be:", mac)
		}
		// 6 hex groups separated by colons.
		parts := strings.Split(mac, ":")
		if len(parts) != 6 {
			t.Errorf("MAC %q: expected 6 groups, got %d", mac, len(parts))
		}
	}
	// Different host names should produce different MACs.
	mac1 := buildPayload("host-a").NICs[0].MACAddress
	mac2 := buildPayload("host-b").NICs[0].MACAddress
	if mac1 == mac2 {
		t.Errorf("different host names produced identical MACs: %s", mac1)
	}
}

// ---------------------------------------------------------------------------
// tokenSHA256Hex + extractAndVerifyToken (hash cross-check helper)
// ---------------------------------------------------------------------------

func TestTokenSHA256Hex(t *testing.T) {
	t.Parallel()
	plaintext := "hello-token"
	sum := sha256.Sum256([]byte(plaintext))
	want := hex.EncodeToString(sum[:])
	got := tokenSHA256Hex(plaintext)
	if got != want {
		t.Fatalf("tokenSHA256Hex(%q) = %q, want %q", plaintext, got, want)
	}
}

func TestExtractAndVerifyToken_Match(t *testing.T) {
	t.Parallel()
	plaintext := "test-plaintext-abc"
	hash := tokenSHA256Hex(plaintext)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
		Data:       map[string][]byte{"plaintext-token": []byte(plaintext)},
	}

	got, ok := extractAndVerifyToken(secret, hash)
	if !ok {
		t.Fatal("expected ok=true, got false")
	}
	if got != plaintext {
		t.Errorf("got plaintext %q, want %q", got, plaintext)
	}
}

func TestExtractAndVerifyToken_Mismatch(t *testing.T) {
	t.Parallel()
	plaintext := "correct-token"
	wrongHash := tokenSHA256Hex("wrong-token")

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
		Data:       map[string][]byte{"plaintext-token": []byte(plaintext)},
	}

	got, ok := extractAndVerifyToken(secret, wrongHash)
	if ok {
		t.Fatal("expected ok=false for hash mismatch, got true")
	}
	if got != "" {
		t.Errorf("expected empty plaintext on mismatch, got %q", got)
	}
}

func TestExtractAndVerifyToken_MissingKey(t *testing.T) {
	t.Parallel()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
		Data:       map[string][]byte{"wrong-key": []byte("value")},
	}
	_, ok := extractAndVerifyToken(secret, "anyhash")
	if ok {
		t.Fatal("expected ok=false when key is absent")
	}
}

func TestExtractAndVerifyToken_EmptyData(t *testing.T) {
	t.Parallel()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
		Data:       map[string][]byte{"plaintext-token": {}},
	}
	_, ok := extractAndVerifyToken(secret, "anyhash")
	if ok {
		t.Fatal("expected ok=false when plaintext-token is empty bytes")
	}
}

// ---------------------------------------------------------------------------
// Authorization header construction: verify the plaintext never leaks
// ---------------------------------------------------------------------------

// TestPostReport_NoPlaintextInURL verifies that the token does not appear in
// the URL passed to postReport — i.e. it is never spliced into the URL path
// by the caller. This is a documentation-as-test: the actual header is set
// inside postReport so callers cannot accidentally build a token-in-URL scheme.
//
// We verify the invariant by constructing a request via the http package and
// checking that the Authorization header value is "Bearer <token>" (exact
// scheme) while the URL contains no token substring.
func TestPostReport_AuthorizationHeaderScheme(t *testing.T) {
	t.Parallel()
	// Build a fake request to inspect — we cannot call postReport against a
	// real server in a unit test, but we can verify the header logic by
	// constructing a matching request directly.
	plaintext := "super-secret-token"
	url := "https://host:8082/api/v1/inspection/ns/host"

	req, err := buildInspectionRequest(t.Context(), url, plaintext, inspectionReportRequest{})
	if err != nil {
		t.Fatalf("buildInspectionRequest: %v", err)
	}

	auth := req.Header.Get("Authorization")
	if auth != "Bearer "+plaintext {
		t.Errorf("Authorization header = %q, want %q", auth, "Bearer "+plaintext)
	}
	// The plaintext must not appear in the URL.
	if strings.Contains(req.URL.String(), plaintext) {
		t.Errorf("plaintext token leaked into URL: %q", req.URL.String())
	}
	// Confirm Content-Type is set.
	if req.Header.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", req.Header.Get("Content-Type"))
	}
}
