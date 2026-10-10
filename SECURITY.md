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

Only the latest release receives fixes; there is no backport branch. Upgrades
within the `v0.4.x` line are additive; `v0.5.0` renames the API to `v1beta2`
without conversion (see `docs/upgrading.md`).

| Version | Supported |
|---|---|
| `v0.10.1` (latest) | ✅ |
| `v0.10.0` | ✅ |
| `v0.9.0` | ✅ |
| `v0.8.0` | ✅ |
| `v0.7.0` | ✅ |
| `v0.6.2` | ✅ |
| `v0.6.1` | ✅ |
| `v0.6.0` | ⚠️ upgrade — cannot patch objects written by `v0.5.0` ([CHANGELOG](CHANGELOG.md)) |
| `v0.5.0` | ✅ |
| `v0.4.4` | ✅ |
| `v0.4.3` | ✅ |
| `v0.4.2` | ⚠️ upgrade — a released host cannot be provisioned again ([CHANGELOG](CHANGELOG.md)) |
| `v0.4.1` | ⚠️ upgrade — also makes `kubectl apply` upgrades impossible |
| `v0.4.0` | ⚠️ upgrade — also breaks `helm upgrade --reuse-values` |
| earlier `v0.4.0-alpha.*` | ❌ upgrade first |
| `v0.3.x` | ❌ end of life — not compatible with v0.4 ([CHANGELOG](CHANGELOG.md)) |

**Every release before `v0.9.0`** lets anyone who can create or patch a `PhysicalHost` forge that
host's callback credentials, and send a same-namespace Secret's `username`/`password` to an
endpoint of their choosing (fixed in `v0.9.0`, see the [CHANGELOG](CHANGELOG.md)). Until you can
upgrade, grant `create`/`patch` on `physicalhosts` only to people who may also read the Secrets in
that namespace.

**Every release before `v0.10.0`** lets anyone who can patch a `PhysicalHost` mark it provisioned
without an inspector, replace its hardware report, fail or stall its run, and delete any ConfigMap
in its namespace. It also lets anyone who can write a `PhysicalHost` choose the CA its BMC is
verified against, and lets anyone who can create a Service in the right namespace receive a BMC
connection meant for a short host name (and, over `http://`, its credentials). All are fixed in
`v0.10.0`, see the [CHANGELOG](CHANGELOG.md). Until you can upgrade, grant `patch` on
`physicalhosts` only to people trusted to provision those hosts, and write BMC addresses as IP
addresses or fully qualified names.

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
  `PhysicalHost.spec.redfishConnection.credentialsSecretRef`. The Secret, not
  the host, decides where they may be sent: it must list the BMC addresses in
  `beskar7.infrastructure.cluster.x-k8s.io/bmc-addresses`, and opt in with
  `beskar7.infrastructure.cluster.x-k8s.io/bmc-insecure-transport` before they
  travel over `http://` or unverified TLS (D-030). A host's `caBundleSecretRef`
  must be the CA Secret the credentials Secret names in
  `beskar7.infrastructure.cluster.x-k8s.io/bmc-ca-secret` (D-033). A listed
  name is resolved as an absolute DNS name, never through the pod's search path
  (D-032), and the Redfish client sends nothing outside the authorised
  address's scheme, host and port, redirects and BMC-supplied links included.
  The credentials are never logged; the controller logs a BMC address without
  them.
- **The host callback endpoint** (`:8082`) is bearer-gated per host. The per-host
  `<host>-bootstrap-token` Secret is the only credential: every route compares
  the caller's token, in constant time, against the one in that Secret, with an
  expiry the manager wrote, and only while the host's `consumerRef` names the
  machine the Secret was minted for, in the host's own namespace (D-029). The
  Secret counts only if its controller owner reference names that
  `PhysicalHost` by UID; the manager never takes over a Secret someone else
  created under that name (D-031). `status.bootstrap` shows the hashes as a
  mirror and is never used to authenticate.
- **The bearer token** is minted for 60 minutes (longer if `--inspection-timeout`
  is raised above its default, by the same amount). Once the host is `Ready`, or
  its `Beskar7Machine` has failed terminally, its expiry is brought forward to
  at most 5 minutes later, which covers the inspector's retries of its
  `/provisioned` or `/provision-failed` report and nothing else (D-031, D-036).
  The failure cut waits while the host is in an Error about its BMC, because a
  `/provisioned` report still lands there (PROV-1) and needs the token. A token
  is handed out again only while it has more life left than a boot nonce plus an
  inspection (D-031).
- **The `/boot` endpoint** is gated by a boot nonce, distinct from the bearer
  token and held in the same Secret. Its first fetch is recorded together with
  the client address that made it. After that, the same nonce renders the same
  script only for that address and only for 2 minutes; every other fetch gets
  the same `404` as a wrong nonce, even though the nonce is valid for 10 minutes
  from its mint. A new claim always gets a new one (D-031). The client address
  is the peer, or the `X-Forwarded-For` entry a configured trusted proxy added,
  so an attacker who can send from the host's own address within those 2
  minutes, or who fetches the nonce before the host does, still gets the
  script.
- **OS image integrity** is anchored by `targetImageDigest` (SHA-256), verified by
  the inspector during the write. The image may be served over plain HTTP: the
  digest, not TLS, is the trust anchor.
  `targetImageDigestURL` reads that digest from a checksum file over verified HTTPS
  instead; the trust anchor is then whoever controls that file, so use a versioned,
  never-overwritten one. The controller reads it once per machine, before it claims
  a host, and pins the result; see
  [docs/security/README.md](docs/security/README.md#10-image-digest-from-a-checksum-url-targetimagedigesturl-d-038).
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
