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
	"testing"
)

// TestParseMemoryCapacityGB exercises parseMemoryCapacityGB with the full range of
// BMC-reported formats.  See the helper's doc comment for the GB-vs-GiB convention:
// SI suffixes (GB/MB/TB) → powers of 1000; IEC suffixes (GiB/MiB/TiB) → powers of
// 1024; result is always truncated decimal GB (÷1e9) to match MinMemoryGB semantics.
func TestParseMemoryCapacityGB(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		wantGB  int
		wantErr bool
	}{
		// --- happy-path SI decimal ---
		{
			name:   "32GB no space",
			in:     "32GB",
			wantGB: 32, // 32×10^9 / 10^9 = 32
		},
		{
			name:   "32 GB with space",
			in:     "32 GB",
			wantGB: 32,
		},
		{
			name:   "32768MB — converts to 32 decimal GB",
			in:     "32768MB",
			wantGB: 32, // 32768×10^6 / 10^9 = 32.768, truncated → 32
		},
		{
			name:   "32768 MB with space",
			in:     "32768 MB",
			wantGB: 32,
		},
		{
			name:   "1TB decimal",
			in:     "1TB",
			wantGB: 1000, // 1×10^12 / 10^9 = 1000
		},

		// --- happy-path IEC binary ---
		// 32 GiB = 32×2^30 = 34_359_738_368 bytes → ÷10^9 = 34 (truncated)
		{
			name:   "32GiB no space",
			in:     "32GiB",
			wantGB: 34,
		},
		{
			name:   "32 GiB with space",
			in:     "32 GiB",
			wantGB: 34,
		},
		// 32768 MiB = 32768×2^20 = 34_359_738_368 bytes → same as 32 GiB → 34
		{
			name:   "32768MiB — same bytes as 32GiB",
			in:     "32768MiB",
			wantGB: 34,
		},
		{
			name:   "32768 MiB with space",
			in:     "32768 MiB",
			wantGB: 34,
		},
		// 1 TiB = 2^40 = 1_099_511_627_776 bytes → ÷10^9 = 1099
		{
			name:   "1TiB binary",
			in:     "1TiB",
			wantGB: 1099,
		},

		// --- error cases ---
		{
			name:    "bare integer without unit",
			in:      "32",
			wantErr: true,
		},
		{
			name:    "empty string",
			in:      "",
			wantErr: true,
		},
		{
			name:    "non-numeric string",
			in:      "abc",
			wantErr: true,
		},
		{
			name:    "only whitespace",
			in:      "   ",
			wantErr: true,
		},
		{
			// "32PB" → strip trailing B → "32P"; 'P' is not in the allowlist (GB/GiB/MB/MiB/TB/TiB).
			name:    "unknown unit PB — not in allowlist",
			in:      "32 PB",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseMemoryCapacityGB(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseMemoryCapacityGB(%q) = %d, nil; want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseMemoryCapacityGB(%q) unexpected error: %v", tc.in, err)
			}
			if got != tc.wantGB {
				t.Errorf("parseMemoryCapacityGB(%q) = %d; want %d", tc.in, got, tc.wantGB)
			}
		})
	}
}
