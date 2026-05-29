# Beskar7 Controller ↔ Inspector Contract

**Contract version: `v1`**

This document is the single source of truth for the wire contract between the
Beskar7 controller (`github.com/projectbeskar/beskar7`) and the inspection
ramdisk (`beskar7-inspector`). Both repositories pin to a contract version; a
change to the wire format, auth, endpoints, or cmdline parameters is a contract
version bump and requires updating this document and the golden fixture
(see [Versioning and anti-drift](#versioning-and-anti-drift)).

Requirement keywords (MUST, MUST NOT, SHOULD, MAY) are used per RFC 2119.

---

## 1. Scope and audience

This contract covers the network boot, hardware-inspection, and bootstrap-data
handoff between a freshly PXE-booted bare-metal host running the inspector and
the Beskar7 controller's callback server. It does **not** cover Redfish/BMC
control (the controller↔BMC channel) or the operator's DHCP/TFTP/HTTP boot
infrastructure, except where that infrastructure carries contract material
(the boot nonce — see §4).

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
        / beskar7.target / beskar7.ca(-url)
  │
inspector ramdisk (on the host)
  ├─ probe hardware (native SMBIOS/DMI + /sys + /proc)
  ├─ POST report  → {api}/api/v1/inspection/{ns}/{host}   (Bearer token, TLS-verified)  → 202
  ├─ GET bootstrap → {api}/api/v1/bootstrap/{ns}/{host}    (Bearer token, TLS-verified)  → user-data
  └─ kexec into beskar7.target with the fetched user-data
  │
controller
  └─ host → StateReady, Beskar7Machine.Spec.ProviderID set, Ready=true
```

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
| `beskar7.target` | yes | `Beskar7Machine.Spec.TargetImageURL` — the OS image to kexec into. Non-secret. |
| `beskar7.ca` **or** `beskar7.ca-url` | yes | The CA the inspector uses to verify the callback's TLS cert. Inline base64-PEM (`beskar7.ca`) only for a single self-signed cert; otherwise a fetch URL (`beskar7.ca-url`) to avoid cmdline length limits (~2–4 KiB). See §8. |
| `beskar7.timeout` | no | Inspector-side overall timeout (seconds). |
| `beskar7.debug` | no | `true` to enable verbose logging / debug shell on failure. |

The only **secret** on the cmdline is `beskar7.token`. This is acceptable for a
single-purpose, ephemeral, operator-controlled inspection ramdisk (see §8); the
inspector MUST NOT persist the cmdline to durable logs, and the bearer token
MUST NOT carry into the provisioned target OS's persistent `/proc/cmdline`.

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
  server's certificate against the CA delivered via `beskar7.ca`/`beskar7.ca-url`.
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

---

## 9. Inspector required behavior (client contract)

The inspector MUST:

1. Parse the §5 cmdline params from `/proc/cmdline`.
2. Probe hardware natively (SMBIOS/DMI via `/sys/firmware/dmi/tables`, plus `/sys`
   and `/proc`) and build the §6 report satisfying §6.1.
3. `POST` the report to `{api}/api/v1/inspection/{ns}/{host}` with the bearer
   token over verified TLS; treat **202** as success; retry transient failures
   with backoff.
4. `GET` `{api}/api/v1/bootstrap/{ns}/{host}` with the same bearer token to fetch
   CAPI user-data.
5. Download `beskar7.target`, inject the fetched user-data, and `kexec` into it.
6. Never log the bearer token, the nonce, the cmdline, or the bootstrap bytes.
7. Provide a `--dry-run` / report-only mode that performs steps 1–4 but skips
   kexec (for CI without real firmware).

---

## 10. Versioning and anti-drift

- This document carries a **contract version** (top of file). Both repos record
  the version they implement.
- A **golden report fixture** (a canonical §6 JSON document) lives in both repos.
  The controller has a test that decodes it into `InspectionReportRequest`, runs
  `buildInspectionReport` + hardware-requirements validation, and asserts success.
  The inspector has a serde round-trip test asserting it produces byte-equivalent
  JSON. A schema change fails one or both, forcing a coordinated contract bump.

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
