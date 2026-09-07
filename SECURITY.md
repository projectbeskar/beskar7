# Security Policy

## Reporting a vulnerability

**Please do not open a public issue for a security vulnerability.**

Report it privately through
[GitHub Security Advisories](https://github.com/projectbeskar/beskar7/security/advisories/new),
which creates a private channel visible only to the maintainers.

Please include:

- what an attacker can do, and what access they need to start;
- affected version(s) — `beskar7` and, if relevant, `beskar7-inspector` and the
  contract version (`test/contract/VERSION`);
- reproduction steps or a proof of concept;
- any suggested fix or mitigation you already have.

### What to expect

Beskar7 is maintained by a small team, so please treat these as intentions rather
than a contractual SLA:

| Stage | Target |
|---|---|
| Acknowledgement | within 3 working days |
| Initial assessment (severity, affected versions) | within 10 working days |
| Fix or documented mitigation for a confirmed high/critical issue | as a priority over feature work |

We will keep you updated as the assessment progresses, credit you in the advisory
unless you ask us not to, and coordinate disclosure timing with you.

## Supported versions

Only the latest release receives fixes; there is no backport branch. `v1beta1`
is stable as of `v0.4.0`, so upgrades within the `v0.4.x` line are additive.

| Version | Supported |
|---|---|
| `v0.4.1` (latest) | ✅ |
| `v0.4.0` | ⚠️ upgrade — `helm upgrade --reuse-values` is broken against it ([CHANGELOG](CHANGELOG.md)) |
| earlier `v0.4.0-alpha.*` | ❌ upgrade first |
| `v0.3.x` | ❌ end of life — not compatible with v0.4 ([CHANGELOG](CHANGELOG.md)) |

## Verifying what you run

Container images published from `v0.4.0` onward are signed with
[cosign](https://docs.sigstore.dev/) keyless signing, and the controller image
carries a signed SPDX SBOM attestation. Verification commands are in
[docs/installation.md](docs/installation.md#verify-release-artifacts-supply-chain).
`beskar7-inspector` releases ship `sha256sums.txt` plus a cosign bundle.

A verification failure means the artifact was not produced by this project's
release workflow — do not deploy it, and please report it.

## Security-relevant design

Context that may help when assessing a finding:

- **BMC credentials** live in namespaced Secrets referenced by
  `PhysicalHost.spec.redfishConnection.credentialsSecretRef`. They are never
  logged; the controller logs a BMC address without credentials.
- **The host callback endpoint** (`:8082`) is bearer-gated per host. Every route
  matches the caller's token against the target `PhysicalHost`'s stored SHA-256.
  Plaintext tokens live only in a per-host Secret; only the hash is in status.
- **The `/boot` endpoint** is gated by a **single-use** boot nonce, distinct from
  the bearer token and consumed on first fetch.
- **OS image integrity** is anchored by `targetImageDigest` (SHA-256), verified by
  the inspector during the write. The image may be served over plain HTTP: the
  digest, not TLS, is the trust anchor.
- **RBAC** can be narrowed from cluster-wide to per-namespace with
  `--watch-namespaces`; see [docs/security/rbac-hardening.md](docs/security/rbac-hardening.md).

### Known limitations (by design, not vulnerabilities)

Please do not report these as vulnerabilities — but do tell us if you can escalate
one beyond what is described:

- **No disk sanitization on host release.** Releasing a `PhysicalHost` clears the
  boot override and powers the host off; it does not wipe the disk. The previous
  tenant's OS — including the injected bootstrap config, which may carry a cluster
  join secret at `/oem/99_beskar7.yaml` — remains until the host is re-provisioned.
  **Wipe hosts yourself before moving them between trust boundaries.**
- **`insecureSkipVerify`** on `redfishConnection` disables BMC TLS verification.
  It is opt-in, mutually exclusive with `caBundleSecretRef`, and intended for labs.
