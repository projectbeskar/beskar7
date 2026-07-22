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

// Package contract holds the machine-checked marker for the controller <->
// beskar7-inspector wire contract version. This directory (test/contract/) is
// the single source of truth for the contract: the plain-text VERSION file,
// the golden fixtures asserted against it in controllers/*_contract_test.go,
// and this Go const. The beskar7-inspector repo (a separate Go/Rust project)
// vendors byte-copies of these files and pins an immutable contract/<version>
// git tag; it never imports this package. See README.md for the full sync
// contract.
package contract

// Version is the contract version this beskar7 checkout implements. It must
// equal the trimmed contents of the sibling VERSION file byte-for-byte;
// TestContractVersion enforces that within this repo so the const and the
// file can never silently drift from each other. Bump both together, in the
// same commit as any fixture change, then push the immutable tag
// contract/<version> (see README.md "Release checklist").
const Version = "v4.1"
