# Beskar7 Controller ↔ Inspector Contract

**Contract version: `v2`**

This document is the single source of truth for the wire contract between the
Beskar7 controller (`github.com/projectbeskar/beskar7`) and the inspection
ramdisk (`beskar7-inspector`). Both repositories pin to a contract version; a
change to the wire format, auth, endpoints, or cmdline parameters is a contract
version bump and requires updating this document and the golden fixture
(see [Versioning and anti-drift](#versioning-and-anti-drift)).

> **v2 in one line:** the inspector no longer `kexec`s into a kernel+initrd. It
> writes a complete Kairos whole-disk image to the target disk, injects the
> per-host config (carrying CAPI bootstrap user-data) into the image's
> `COS_OEM` partition, and reboots the host into the provisioned OS. The
> inspection report schema (§6) is **unchanged** from v1. See
> [Version history](#101-version-history) for the full delta.

Requirement keywords (MUST, MUST NOT, SHOULD, MAY) are used per RFC 2119.

---

## 1. Scope and audience

This contract covers the network boot, hardware-inspection, bootstrap-data
handoff, and OS deployment between a freshly PXE-booted bare-metal host running
the inspector and the Beskar7 controller's callback server. It does **not**
cover Redfish/BMC control (the controller↔BMC channel) or the operator's
DHCP/TFTP/HTTP boot infrastructure, except where that infrastructure carries
contract material (the boot nonce — see §4). It also does **not** cover the
serving of the target OS image itself (an operator-hosted artifact reachable at
`beskar7.target`); the contract governs only how the inspector locates,
integrity-checks, and applies it (§8.1, §9).

Audience: implementers of the inspector, implementers of the controller's
callback + boot endpoints, and operators wiring the provisioning network.

---

## 2. Provisioning sequence

```
Beskar7Machine reconcile (controller)
  ├─ claim PhysicalHost (ConsumerRef) → host StateInUse
  ├─ triggerInspection:
  │    ├─ mint bearer token  → hash on Status.Bootstrap.TokenHash, plaintext in Secret
  │    ├─ mint boot nonce    → hash on Status.Bootstrap.BootNonceHash, plaintext in Secret
  │    └─ Redfish SetBootSourcePXE + power on
  │
operator DHCP/boot-infra (NOT Beskar7)
  └─ host PXE-boots → chainloads per-host iPXE script (keyed by MAC) over HTTPS,
        whose URL embeds the boot nonce: GET {api}/api/v1/boot/{ns}/{host}/{nonce}
  │
controller /boot endpoint (nonce-gated, NOT bearer-gated)
  └─ verify nonce → consume (single-use) → render kernel cmdline:
        beskar7.api / beskar7.namespace / beskar7.host / beskar7.token
        / beskar7.target / beskar7.target-digest / beskar7.ca
  │
inspector ramdisk (on the host) — Phase 1: enroll & inspect (always)
  ├─ probe hardware (native SMBIOS/DMI + /sys + /proc)
  ├─ select target disk
  └─ POST report  → {api}/api/v1/inspection/{ns}/{host}   (Bearer token, TLS-verified)  → 202
  │
inspector ramdisk — Phase 2: provision (when bootstrap data is available)
  ├─ GET bootstrap → {api}/api/v1/bootstrap/{ns}/{host}    (Bearer token, TLS-verified)  → user-data
  ├─ download beskar7.target (plain HTTP) → verify sha256 == beskar7.target-digest
  ├─ write the whole-disk image to the selected target disk (dd-equivalent)
  ├─ re-read the partition table; mount the image's COS_OEM partition
  ├─ inject a per-host cloud-config embedding the fetched user-data into COS_OEM
  └─ sync, unmount, reboot(2) → host firmware boots the provisioned OS
  │
controller
  └─ host → StateReady, Beskar7Machine.Spec.ProviderID set, Ready=true
```

**Success signal is out-of-band.** The inspector reboots and is gone before the OS
finishes booting; it does **not** report "provisioning succeeded" — no such
callback exists in this contract, by design. The final controller-side transition
(`StateReady` / `ProviderID` / `Ready=true`) is driven by inspection completing,
and the *real* proof the host provisioned correctly is the node registering with
the management cluster via the injected join secret (CAPI's normal node-join path,
matching the Metal³ posture). A host-reported provisioning-success/liveness signal
is explicitly **out of scope** for v2 (see §11).

---

## 3. Trust model (overview)

Two distinct per-host secrets, by design (decision D-009). Do not conflate them.

| Secret | Gates | Lifetime | Reuse | Delivered to host via |
|---|---|---|---|---|
| **Boot nonce** | `GET /api/v1/boot/...` | ~10 min | **single-use** | the operator's iPXE script URL (the nonce IS the capability) |
| **Bearer token** | `POST /inspection`, `GET /bootstrap` | 30 min (`auth.TokenLifetime`) | multi-use | rendered into the kernel cmdline by `/boot` |

The booting host holds no bearer token, so the endpoint that *hands out* the
bearer token (`/boot`) cannot itself be bearer-gated. The boot nonce breaks that
chicken-and-egg: possession of an unguessable 256-bit nonce — not a spoofable
MAC and not network placement — is the authorization to receive the bearer
token + boot parameters.

Both secrets are minted with `crypto/rand` and stored hashed (SHA-256 hex) on
`PhysicalHost.Status.Bootstrap`; plaintexts live only in the per-host Secret
`<hostName>-bootstrap-token` (owner-ref'd to the PhysicalHost, GC'd on delete)
and in host memory. See `internal/auth/token.go` for the primitives.

---

## 4. Endpoints

All three endpoints are served by the controller's callback server on a single
HTTPS listener (default `:8082`, `controllers/inspection_handler.go`
`SetupCallbackServer`). TLS is mandatory on all of them.

### 4.1 `GET /api/v1/boot/{namespace}/{hostName}/{nonce}` — boot-param rendering

- **Auth**: the `{nonce}` path segment, verified constant-time against
  `Status.Bootstrap.BootNonceHash`, within TTL, and **not yet consumed**
  (`Status.Bootstrap.BootNonceConsumedAt == nil`). NOT bearer-gated.
- **On success**: marks the nonce consumed (single-use, see §7) and returns the
  rendered iPXE script / kernel cmdline carrying the parameters in §5. A second
  successful fetch within the window (e.g. a NIC retry) MUST return **identical**
  content for the same host.
- **Failure**: opaque response identical for "no such host", "wrong nonce",
  "expired", and "already consumed" — no oracle. The nonce, the URL, and the
  `{nonce}` path value MUST NOT be logged (the nonce hash MAY be).
- **Rate limiting**: this route is ungated; it MUST be rate-limited per source IP
  (and SHOULD be per `{namespace}/{hostName}`).

**Rendered output** (`Content-Type: text/plain`): a complete iPXE script that
boots the inspector image with the §5 parameters on the kernel cmdline:

```ipxe
#!ipxe
kernel {InspectionImageURL}/vmlinuz beskar7.api={api} beskar7.namespace={ns} beskar7.host={host} beskar7.token={token} beskar7.target={target} beskar7.target-digest={digest} beskar7.ca={base64CA} [beskar7.timeout={t}] [beskar7.debug=true]
initrd {InspectionImageURL}/initrd.img
boot
```

- `{InspectionImageURL}` is `Beskar7Machine.Spec.InspectionImageURL` (the
  consuming machine, resolved via the host's `ConsumerRef`) — the HTTPS base URL
  of a location serving the inspector `vmlinuz` and `initrd.img`. (Contract v1
  re-purposes this field as the inspector image base; it was previously
  declared-but-unused.) If it is empty, `/boot` returns the opaque failure — the
  host cannot be booted without an inspector image.
- `{base64CA}` is the callback CA, base64-encoded (see §5 / §8).
- `{target}` is `Beskar7Machine.Spec.TargetImageURL` (the Kairos whole-disk raw
  image URL) and `{digest}` is `Beskar7Machine.Spec.TargetImageDigest`
  (`sha256:<hex>`). Both are required spec fields; if either is empty, `/boot`
  returns the opaque failure — a host MUST NOT be booted without a pinned target.
- The script boots the inspector image directly; it does NOT chainload another
  iPXE script (the per-host script IS this response).

> **Implementation status (v2 spec-first):** this document is the specification
> Phases A/B implement, not a description of shipped code. As of the v2 bump,
> `Beskar7Machine.Spec.TargetImageDigest` and the `/boot` render of
> `beskar7.target-digest` are **net-new** — the field does not yet exist on
> `Beskar7MachineSpec` and `buildBootIPXEScript` does not yet render the param.
> The present-tense MUSTs above describe the target state the controller PR adds;
> `TargetImageURL`'s godoc (still "boot via kexec") is corrected by that same PR.

### 4.2 `POST /api/v1/inspection/{namespace}/{hostName}` — hardware report

- **Auth**: `Authorization: Bearer <token>`; the token's SHA-256 MUST match
  `Status.Bootstrap.TokenHash` and `ExpiresAt` MUST be in the future
  (`auth.RequireBearer` + `newBearerTokenVerifier`).
- **Body**: JSON, the `InspectionReportRequest` schema in §6. Max 1 MiB
  (`inspectionMaxBodyBytes`); over-limit → `413`.
- **Success**: **`202 Accepted`** with body `{"status":"accepted"}`. The 202 (not
  200) is deliberate — the report is stored and the reconciler signalled, but
  `Status.InspectionReport`/`InspectionPhase` are written asynchronously by the
  PhysicalHost reconciler (D-005). The inspector MUST treat **202** as success.
- `namespace`/`hostName` come from the URL path. The JSON body MAY also carry
  `namespace`/`hostName` for legacy compatibility but they are ignored.

### 4.3 `GET /api/v1/bootstrap/{namespace}/{hostName}` — CAPI bootstrap user-data

- **Auth**: same bearer token as §4.2.
- **Success**: `200` with the raw CAPI bootstrap user-data bytes
  (`Content-Type: application/octet-stream`, `Cache-Control: no-store`). This is
  the cloud-init/Ignition payload (the CAPI Secret `data["value"]`) — see
  `controllers/bootstrap_handler.go`. **It may contain cluster join secrets.**
- **Failure**: opaque `404` for every resolution-chain failure (host → ConsumerRef
  → Beskar7Machine → owner Machine → `Spec.Bootstrap.DataSecretName` → Secret);
  `500` only for an oversize secret.

---

## 5. Kernel cmdline parameters

Rendered by `/boot` into the inspector's kernel cmdline. The inspector parses
these from `/proc/cmdline`.

| Param | Required | Meaning |
|---|---|---|
| `beskar7.api` | yes | Externally-reachable HTTPS base URL of the callback server (e.g. `https://beskar7.example.com:8082`). MUST NOT be a `*.svc` cluster-internal name (see §8). |
| `beskar7.namespace` | yes | PhysicalHost namespace (used in the endpoint paths). |
| `beskar7.host` | yes | PhysicalHost name. |
| `beskar7.token` | yes | The per-host bearer token (43-char `base64url`, no padding). Secret. |
| `beskar7.target` | yes | `Beskar7Machine.Spec.TargetImageURL` — the **Kairos whole-disk raw image** the inspector writes to the target disk. MUST be an `http://` or `https://` URL; plain HTTP is permitted (integrity comes from `beskar7.target-digest`, not TLS — see §8.1). Non-secret. |
| `beskar7.target-digest` | yes | `Beskar7Machine.Spec.TargetImageDigest` — the expected SHA-256 of the bytes at `beskar7.target`, matching `^sha256:[0-9a-f]{64}$`. The inspector MUST verify the written image against this digest and MUST refuse to **boot** (mount/inject/reboot) a non-matching image (§8.1). Non-secret; it is the sole integrity **and authenticity** anchor for the OS image. |
| `beskar7.ca` | yes | Base64-encoded PEM of the CA the inspector uses to verify the callback's TLS cert. **Inline only.** `/boot` sources it from the manager's callback cert dir (`ca.crt` if present — cert-manager and the chart's self-signed path both provide it — else the self-signed `tls.crt`). Bounded by kernel cmdline length (~2–4 KiB): a single self-signed/issuer cert fits; a full multi-cert chain may not. A `beskar7.ca-url` fetch variant for chain delivery is deferred to a later contract version. See §8. Note: this CA verifies **only** the callback endpoints (`/inspection`, `/bootstrap`); it does NOT verify `beskar7.target` (§8.1). |
| `beskar7.timeout` | no | Inspector-side overall timeout (seconds). |
| `beskar7.debug` | no | `true` to enable verbose logging / debug shell on failure. |

The only **secret** on the cmdline is `beskar7.token`. This is acceptable for a
single-purpose, ephemeral, operator-controlled inspection ramdisk (see §8); the
inspector MUST NOT persist the cmdline to durable logs. The bearer token cannot
leak into the provisioned OS's persistent `/proc/cmdline` — v2 reboots via host
firmware (§9), so the target OS boots with its own cmdline carrying no `beskar7.*`
parameters.

---

## 6. Inspection report schema (wire format)

The POST body in §4.2. This is the authoritative JSON shape; the controller
decodes it into `InspectionReportRequest` (`controllers/inspection_handler.go`).
All fields are `omitempty`. Unknown fields are ignored.

```jsonc
{
  "manufacturer": "string",
  "model": "string",
  "serialNumber": "string",
  "bootModeDetected": "UEFI" | "Legacy",
  "firmwareVersion": "string",
  "cpus": [
    { "id": "string", "vendor": "string", "model": "string",
      "cores": 0, "threads": 0, "frequency": "string" }
  ],
  "memory": [
    { "id": "string", "type": "string", "capacity": "32GiB", "speed": "string" }
  ],
  "disks": [
    { "name": "string", "model": "string", "sizeGB": 0,
      "type": "HDD" | "SSD" | "NVMe", "serialNumber": "string" }
  ],
  "nics": [
    { "name": "string", "macAddress": "string", "driver": "string",
      "speed": "string", "ipAddresses": ["string"] }
  ]
}
```

### 6.1 Validation rules the inspector MUST satisfy

The controller validates the report against `Beskar7Machine.Spec.HardwareRequirements`
(`controllers/beskar7machine_controller.go`). To produce data the controller can
evaluate correctly:

- **CPU cores**: the controller sums `cpus[].cores` across the array for
  `MinCPUCores`. The inspector MUST emit one entry **per physical CPU package**
  with that package's real core count — NOT one entry per logical processor, and
  NOT the per-socket core count repeated. (The previous bash inspector got this
  wrong.)
- **Memory capacity**: `memory[].capacity` MUST carry a unit suffix the
  controller accepts: `GB`, `GiB`, `MB`, `MiB`, `TB`, or `TiB`
  (`parseMemoryCapacityGB`). A bare integer is rejected. Emit one entry per
  populated DIMM where SMBIOS Type 17 exposes it.
- **Disk size**: `disks[].sizeGB` is summed for `MinDiskGB`; emit integer GB.
- **NIC IPs**: `nics[].ipAddresses` MUST be a real JSON array of individual
  address strings — not a single comma-joined string.

---

## 7. Single-use semantics (boot nonce)

- The nonce is consumed on first successful `/boot` fetch by setting
  `Status.Bootstrap.BootNonceConsumedAt`. The consume MUST be atomic
  (optimistic-locked) so two concurrent fetches cannot both treat the nonce as
  fresh. Note: the existing inspection/bootstrap annotation writes deliberately
  drop optimistic locking (single unique writer); the consume is the opposite
  situation — a Conflict is the desired outcome and MUST be enforced.
- A double-fetch to the **same host** (race loser, or a legitimate retry) is
  benign and MUST return identical content (§4.1). A second fetch for a
  **different** host's nonce is impossible by construction (per-host nonce).
- Re-provision (reboot, inspection-timeout retry, delete-and-recreate) MUST mint
  a **fresh** nonce (and fresh bearer token) — there is no "un-consume" path.
- If the consume is routed through the D-005 annotation→reconciler handoff rather
  than a direct status write, single-use weakens to "single-use modulo reconcile
  lag"; this is acceptable only because the same-host response is idempotent. The
  direct-vs-handoff choice is an implementation decision (see §11).

---

## 8. TLS and reachability

- **All three endpoints are HTTPS-only.** The inspector MUST verify the callback
  server's certificate against the CA delivered inline via `beskar7.ca`.
  The inspector MUST NOT offer or use an insecure-skip-verify option for the
  inspection POST or bootstrap GET — those carry/return cluster join secrets and a
  MITM there is a cluster compromise. This mirrors the `RedfishConnection` posture
  where `InsecureSkipVerify` and `caBundleSecretRef` are mutually exclusive
  (`controllers/redfish_tls.go`).
- **External reachability**: the bare-metal host reaches the callback over an
  externally-routable address (LoadBalancer/NodePort/Ingress), NOT cluster DNS.
  `beskar7.api` MUST be that external address, and the callback serving cert MUST
  have a SAN covering it. The current `--bootstrap-url-base` default
  (`https://beskar7-controller-manager.beskar7-system.svc:8082`) is unreachable
  from bare metal and a `.svc`-only cert forces operators toward insecure-skip —
  operators MUST override it (the manager SHOULD warn at startup if it is a
  `.svc` name).
- **Chainload hop**: the operator's boot infrastructure serves the per-host iPXE
  script (containing the `/boot/...{nonce}` URL) and the kernel/initrd. That hop
  MUST be HTTPS — it carries the nonce in the clear otherwise, and a plain-HTTP
  kernel/initrd hop is an OS-image MITM. (Tracked: `docs/ipxe-setup.md` currently
  ships plain `http://` — SEC-6 must convert it in lockstep.)
- **TOFU on `/boot`**: the `/boot` fetch precedes the inspector having the CA (the
  CA is delivered by `/boot`), so that single request is trust-on-first-use for
  the callback cert. This is acceptable because `/boot` carries no host secret
  (the nonce is already on the wire by assumption) and the downstream POST/GET
  re-verify against the delivered CA. The inspector MUST NOT cache the TOFU cert
  as a trust anchor across boots.

### 8.1 Target-image integrity (digest pinning, not TLS)

The target OS image (`beskar7.target`) is a **distinct trust domain** from the
callback endpoints and is integrity-checked by **content digest**, not transport
security:

- The image MAY be served over plain HTTP. Its integrity **and authenticity**
  come entirely from `beskar7.target-digest` (`sha256:<hex>`, §5), which the
  inspector receives over the same verified-TLS-derived channel as the rest of the
  cmdline (rendered by `/boot`, whose nonce/CA chain is the trust root). The digest
  is the *only* thing binding the image bytes to the operator — there is no
  signature — so the operator MUST compute `beskar7.target-digest` over the exact
  artifact served at `beskar7.target` (a pinned, immutable image; never a mutable
  `latest`-style URL whose bytes can change out from under the digest).
- **Scheme and redirects.** `beskar7.target` MUST be an `http://` or `https://`
  URL. The inspector MUST reject any other scheme (`file://`, `unix://`, etc.) and
  MUST NOT follow a redirect that changes scheme or host to a non-`http(s)` target;
  the digest gate MUST hold across any redirect that is followed. This prevents a
  plain-HTTP MITM from redirecting the fetch to a local path or an internal address
  (SSRF) — the inspector has no webpki roots and cannot rely on TLS to stop it.
- The inspector's TLS trust store carries **only** the cmdline-delivered callback
  CA (§8) — it has **no** public webpki roots. It therefore cannot, and MUST NOT,
  attempt to verify a TLS certificate for an arbitrary operator image host. The
  image fetch MUST use the digest as its only integrity check.
- **Verification model (stream-to-disk, gate the reboot — not the write).** The
  image is a multi-GB whole-disk raw; the inspector MUST NOT buffer it in RAM/tmpfs.
  It streams the download directly to the target disk while computing the SHA-256
  incrementally over the same byte stream, then compares the computed digest to
  `beskar7.target-digest` with a full-length, exact byte comparison (constant-time
  is **not** required — the digest is public, not a secret) once the write
  completes. The integrity gate is on the **boot decision**, not the disk write:
  only a verified-matching digest permits the inspector to proceed to mount
  `COS_OEM`, inject the user-data, and reboot. On any mismatch, short read, or
  size-limit breach the inspector MUST abort — it MUST NOT mount the image, MUST
  NOT inject the user-data, and MUST NOT reboot the host. A digest-failed write
  leaves the disk unbootable **by design**: the host falls back to PXE and the next
  provisioning attempt mints a fresh nonce (§7) and overwrites the disk. (Writing
  the unverified bytes to disk is safe because the image carries no secret and the
  host never boots them.)
- **Size bound (DoS).** The inspector MUST enforce a maximum image size (a build
  default; no §5 cmdline override is defined in v2) and MUST abort the download
  once it is exceeded, rather than writing or hashing unbounded bytes. The bound
  SHOULD also be capped by the selected target disk's capacity (a whole-disk image
  larger than the disk can never deploy). A plain-HTTP MITM cannot forge the digest,
  but absent a bound it can serve an unbounded body to exhaust the target disk or
  stall provisioning — a pre-verification DoS.
- **Digest format.** `beskar7.target-digest` MUST match `^sha256:[0-9a-f]{64}$`
  (lowercase-normalized before compare). The inspector MUST reject a malformed,
  truncated, or non-`sha256:` digest outright. Only `sha256:` is accepted in v2; a
  stronger or multi-digest scheme is a contract bump, not a silent client
  extension.
- This mirrors the image-handoff posture of Ironic / Metal³ / Tinkerbell, where
  the OS image is a public, non-secret artifact pinned by checksum rather than
  authenticated by TLS. It is acceptable **only** because the image carries no
  secret: the per-host secret (the CAPI join data) arrives separately via the
  verified-TLS `/bootstrap` GET and is injected locally (§9), never embedded in
  the published image.

---

## 9. Inspector required behavior (client contract)

### 9.0 Role of the inspector

The inspector is the **only in-band component** in a Beskar7 provisioning cycle —
the one piece of Beskar7 that runs *on* the bare-metal host. The BMC/Redfish
channel does power and boot-source control only (out-of-band); the controller
runs in the management cluster (out-of-band). Everything that requires being
inside the host — reading firmware inventory, choosing a disk, writing the OS,
planting the join secret — is the inspector's job. Without it, Beskar7 is a
netboot installer, not a Cluster API infrastructure provider.

The inspector has exactly three irreducible responsibilities, run in **two
phases**:

- **Phase 1 — Enroll & inspect (always).** Parse the cmdline, probe hardware from
  firmware truth, select a target disk, and POST the report. This populates
  `PhysicalHost.Status`, lets the controller gate `HardwareRequirements`, and
  records which disk will be written. Phase 1 is idempotent and side-effect-free
  on the host's disks.
- **Phase 2 — Provision (only when bootstrap data is ready).** Fetch the CAPI
  bootstrap user-data, write the digest-pinned target image to the selected disk,
  inject a per-host config embedding that user-data into the image's `COS_OEM`
  partition, and reboot the host into the provisioned OS. Phase 2 is the only
  destructive phase and the only one that handles the join secret.

`--dry-run` runs **Phase 1 only** (steps 1–3 below), making no destructive change
and never rebooting — this is the CI / report-only mode.

### 9.1 Required steps

The inspector MUST:

1. Parse the §5 cmdline params from `/proc/cmdline`.
2. Probe hardware natively (SMBIOS/DMI via `/sys/firmware/dmi/tables`, plus `/sys`
   and `/proc`) and build the §6 report satisfying §6.1. Select the target disk
   for provisioning (selection policy is an implementation decision — see §11).
3. `POST` the report to `{api}/api/v1/inspection/{ns}/{host}` with the bearer
   token over verified TLS; treat **202** as success; retry transient failures
   with backoff.
4. *(Phase 2)* `GET` `{api}/api/v1/bootstrap/{ns}/{host}` with the same bearer
   token over verified TLS to fetch the CAPI user-data.
5. *(Phase 2)* Deploy the OS to the selected disk:
   1. Stream `beskar7.target` (scheme/redirect-restricted per §8.1) directly to the
      selected target disk, computing its SHA-256 incrementally and enforcing the
      §8.1 maximum-size bound. The inspector MUST NOT buffer the whole image in
      RAM/tmpfs.
   2. After the write completes, verify the computed digest against
      `beskar7.target-digest` (full-length exact compare, §8.1). On mismatch, short
      read, or size-limit breach the inspector MUST abort: it MUST NOT mount,
      inject, or reboot — the unbootable disk is recovered on the next provisioning
      attempt (§7, §8.1).
   3. Re-read the partition table; mount the image's `COS_OEM` partition (located by
      its filesystem label) and write a numbered cloud-config file embedding the
      fetched user-data. The user-data MUST be written **only** to the target disk's
      `COS_OEM` partition — never to the ramdisk's durable storage or logs — and the
      written file MUST be root-owned with mode `0600`.
   4. `sync`, unmount, **zero the in-memory user-data buffer**, and `reboot(2)`. The
      host firmware boots the provisioned OS; Kairos applies the injected config on
      first boot. (`kexec` is an optional future speed optimization — see §11 — not
      a contract requirement.)
6. Never log the bearer token, the nonce, the cmdline, or the bootstrap/user-data
   bytes. The inspector MUST NOT let the bearer token or the user-data/join secret
   reach swap or any durable medium: the ramdisk MUST run swapless, or the inspector
   MUST `mlock` and zero those buffers.
7. Provide a `--dry-run` / report-only mode that performs steps 1–3 (Phase 1) and
   then exits **without** fetching bootstrap data, writing any disk, or rebooting
   (for CI without real firmware or a real target disk).

### 9.2 Phase 1 → Phase 2 transition (bootstrap readiness)

The bootstrap user-data is produced asynchronously by the CAPI bootstrap provider
and is **not** guaranteed to exist when the inspector finishes Phase 1. The
`/bootstrap` endpoint returns an **opaque `404`** for the not-ready case *and* for
genuine resolution failures (§4.3) — it gives the inspector no oracle to tell them
apart. The inspector therefore MUST treat the GET as a **poll**, not a one-shot:

- A `404` (or `5xx`) on `GET /bootstrap` means **not ready yet**: the inspector
  MUST retry with backoff, not abort. A `200` carrying user-data is the signal to
  enter Phase 2 (step 5).
- The inspector MUST bound the poll by `beskar7.timeout` (or a build default). On
  timeout it MUST NOT write the disk; it exits to the debug shell (if
  `beskar7.debug`) or powers the behaviour off to a non-destructive failure that
  the controller observes as an inspection/provisioning timeout (it re-mints a
  fresh nonce + token on the next attempt, §7).
- A `401`/`403` is **not** retryable the same way — it indicates an expired or
  wrong bearer token (the 30-min token lifetime, §3); the inspector MUST abort
  rather than spin (the controller must re-provision with a fresh token).
- The inspector holds no durable state between attempts: a timed-out host is
  re-driven by the controller (re-PXE → fresh nonce/token → re-inspect), not by
  the inspector retrying across reboots. There is no in-ramdisk persistence of the
  poll position.

---

## 10. Versioning and anti-drift

- This document carries a **contract version** (top of file). Both repos record
  the version they implement.
- A **golden report fixture** (a canonical §6 JSON document) lives in both repos.
  The canonical copy is [`test/contract/golden_inspection_report.json`](../test/contract/golden_inspection_report.json)
  (see [`test/contract/README.md`](../test/contract/README.md)). The controller test
  `controllers/inspection_contract_test.go` decodes it into `InspectionReportRequest`
  — both leniently (production parity) and strictly (`DisallowUnknownFields`, the
  forward-drift catch) — round-trips it to prove the struct is lossless, runs
  `buildInspectionReport`, and runs the real `parseMemoryCapacityGB` over its memory
  entries to lock the hardware-aggregate math. The inspector mirrors the same bytes
  in a serde round-trip test asserting byte-equivalent JSON. A schema change on
  either side fails one or both tests, forcing a coordinated contract bump.
- **The deploy path (Phase 2) is NOT covered by the golden fixture.** The fixture
  locks only the §6 *report* wire format. v2's destructive disk behavior — digest
  verification, `COS_OEM` injection, the cmdline render of `beskar7.target-digest`
  — has no equivalent byte-locked test and is a separate drift surface. At minimum
  it MUST be guarded by: a controller-side test that `/boot` renders
  `beskar7.target-digest` from `Beskar7MachineSpec.TargetImageDigest`; an inspector
  test asserting a digest mismatch aborts before mount/inject/reboot; and a fixture
  for the injected `COS_OEM` cloud-config shape. Until those land, "no test failed"
  does **not** imply the deploy contract held (e.g. a changed OEM filename
  convention would pass silently).

### 10.1 Version history

| Version | Delta |
|---|---|
| **v1** | Initial frozen contract: boot-nonce + bearer-token two-secret trust model, three HTTPS endpoints (`/boot`, `/inspection`, `/bootstrap`), §6 inspection report schema + golden fixture, inline `beskar7.ca`. Handoff was **kexec into `beskar7.target`** (a kernel+initrd image). |
| **v2** | **Handoff redesign — §6 report schema unchanged.** Replaces kexec with whole-disk image deployment: the inspector writes a Kairos whole-disk raw image (`beskar7.target`) to the target disk, injects the per-host config (with CAPI user-data) into the image's `COS_OEM` partition, and reboots. Adds required cmdline param `beskar7.target-digest` (`sha256:<hex>`) and the §8.1 digest-pinning trust model (target image over plain HTTP, integrity by content digest, not TLS). Reframes §9 around the two-phase enroll/provision role model (§9.0–9.2). **Report path:** the §6 schema and golden fixture are untouched, so the bump forces **no** inspector report-code or fixture change. **Controller side, however, gains a CRD delta:** a new required `Beskar7Machine.Spec.TargetImageDigest` field and a `/boot` render of `beskar7.target-digest` — net-new code in this repo, not a no-op (see implementation-status note at §4.1). The deploy path is a new, separately-tested drift surface (§10). |

---

## 11. Deferred and open items

- **Redfish virtual-media nonce delivery** (future hardening): delivering the boot
  nonce out-of-band via the BMC (BIOS attribute / virtual media) would remove the
  pre-consumption L2 exposure entirely, but requires per-vendor virtual-media
  support that does not exist in `internal/redfish/` today. Revisit only if a
  non-single-purpose provisioning OS reuses the cmdline-token channel.
- **Consume mechanism** (implementation decision for Phase A): direct
  `Status.Bootstrap.BootNonceConsumedAt` write under optimistic lock (strong
  single-use, but the handler writing status is a D-005 exception needing
  sign-off) vs. an annotation→reconciler handoff (D-005-clean, single-use modulo
  reconcile lag). The same-host idempotent response (§7) makes either acceptable.
- **MAC learning** (optional, additive): Beskar7 MAY pin the provisioning-NIC MAC
  into status on first boot / from `InspectionReport.NICs` for inventory. It MUST
  NOT become a required spec field (it duplicates the operator's DHCP mapping and
  is not a trust anchor).
- **Target-disk selection policy** (implementation decision for Phase B): step 2
  selects *a* disk; the policy (smallest disk above `MinDiskGB`? first non-removable
  NVMe? an explicit `beskar7.target-disk` hint?) is unspecified in v2 and left to
  the inspector. If it becomes operator-configurable via a spec field or cmdline
  param, that is a contract bump. Until then the inspector MUST log (non-secret)
  which disk it chose and MUST refuse to provision if no disk meets the hardware
  requirements.
- **kexec for boot-time speed** (future optimization): v2 reboots via host
  firmware (one POST cycle). A later version MAY `kexec` directly into the freshly
  written OS to skip the firmware POST, but kexec needs real firmware and a
  vendor-portable kernel/initrd extraction path; it is an optimization, never a
  correctness requirement, and is out of scope for v2.
- **Generic (non-Kairos) whole-disk images** (future): v2's OEM-injection step is
  Kairos-specific (it relies on the `COS_OEM` label and Kairos's numbered
  cloud-config convention). Supporting a generic cloud-image (cloud-init seed via
  a `cidata` ISO / config-drive partition instead of `COS_OEM`) is a future
  variant and would be a contract bump, as it changes how the per-host user-data
  is injected.
- **CAPI-bootstrap → Kairos-config mapping** (open design): how the raw CAPI
  bootstrap user-data (cloud-init/Ignition from the bootstrap provider) maps onto
  the Kairos cloud-config the inspector injects is not yet specified. The cleanest
  fit is a Kairos "standard" (k3s/k0s-baked) image paired with a matching CAPI
  bootstrap provider; this must be resolved before the end-to-end join path is
  claimed working.
- **Provisioning-failure recovery** (open design, controller-side): §8.1 covers the
  digest-mismatch abort, but the contract does not yet specify how a host that
  fails *after* a successful inspection — `dd` write error, `COS_OEM` mount/inject
  failure, or an image that writes cleanly but never boots into a joining node — is
  detected and re-driven. The inspector is stateless across reboots (§9.2), so
  recovery is necessarily controller-side: an inspection/provisioning-timeout that
  re-mints a fresh nonce + token (§7) and re-PXEs, or operator delete/recreate.
  Because the success signal is out-of-band (§2), the controller's only handle on
  "provisioned but not joining" is a timeout — its bound and the retry policy are
  unspecified in v2 and must be defined before GA.
