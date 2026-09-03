# Upgrading Beskar7

> **Audience:** Operators

Beskar7 is **pre-GA**: the `v1beta1` API is frozen and evolves additive-only, but
alpha releases before that freeze contain breaking changes. Read the section for
your starting version before upgrading.

## Before you start

Upgrade in this order — the CRDs must accept the new fields before the new
controller writes them:

1. **CRDs** — Helm does *not* upgrade CRDs on `helm upgrade`; apply them yourself.
2. **Controller** (`helm upgrade`, or re-apply the release manifest).
3. **`beskar7-inspector`** — if the contract version changed (see below).
4. **Your bootstrap templates** — only if a contract change requires it.

Provisioning is not interrupted by a controller restart: state lives in the CRs,
and a host mid-inspection is picked up again on the next reconcile. Avoid
upgrading while a host is in `Deploying` if you can — the inspector is writing a
disk and reporting to the callback endpoint during that window.

## Matching the inspector to the controller

The controller and inspector share a **versioned wire contract**. A controller
speaking contract `vN` requires an inspector implementing `vN`.

The running controller does not currently report its contract version, so map it
from the release you deployed:

| beskar7 release | contract |
|---|---|
| `v0.4.0-alpha.9` | `v4.2` |
| `v0.4.0-alpha.8` | `v4.1` |
| `v0.4.0-alpha.7` | `v4` |

From a source checkout of the matching tag:

```bash
# alpha.9 and later carry a machine-readable marker:
git show v0.4.0-alpha.9:test/contract/VERSION      # -> v4.2

# earlier tags predate that file — read the contract doc header instead:
git show v0.4.0-alpha.8:docs/inspector-contract.md | head -5
```

On the inspector side it is `contract-version.txt` in the release assets, or
`/contract-version.txt` equivalents documented in that repo's README.

Each inspector release states its contract version, ships it as
`contract-version.txt`, and tags its image accordingly:

```bash
docker pull ghcr.io/projectbeskar/beskar7-inspector:contract-v4.2
```

Within a frozen `v4.x` line the changes are additive, so a controller tolerates an
inspector one minor version behind — it simply does not get the newer capability
(see `docs/inspector-contract.md` §14). Do not rely on that across a major bump.

## `v0.4.0-alpha.8` → `v0.4.0-alpha.9` — **breaking**

Two API changes require editing existing CRs **before** the new CRDs are applied,
or the objects will fail validation.

### 1. `caBundleSecretRef` changed from an object to a string

```yaml
# BEFORE (alpha.8 and earlier)
spec:
  redfishConnection:
    caBundleSecretRef:
      name: my-bmc-ca

# AFTER (alpha.9+)
spec:
  redfishConnection:
    caBundleSecretRef: my-bmc-ca
```

Find affected hosts:

```bash
kubectl get physicalhosts -A -o json \
  | jq -r '.items[] | select(.spec.redfishConnection.caBundleSecretRef | type == "object")
           | "\(.metadata.namespace)/\(.metadata.name) -> \(.spec.redfishConnection.caBundleSecretRef.name)"'
```

### 2. `configurationURL` was removed

`Beskar7Machine.spec.configurationURL` was a dead field — nothing consumed it.
Remove it from your manifests and templates:

```bash
grep -rn "configurationURL" your-manifests/
```

### 3. Numeric fields are now `int32`

`hardwareRequirements` (`minCPUCores`, `minMemoryGB`, `minDiskGB`) and the
inspection fields (`cores`, `threads`, `sizeGB`) are `int32`. Existing values are
unaffected — this only matters if you generate manifests programmatically.

### Contract moves to v4.2

alpha.9 speaks **contract v4.2**, which adds per-host `ProviderID` delivery for
templated pools. Upgrade the inspector to a `contract-v4.2` build. A v4.1
inspector keeps working (it ignores the new cmdline parameter) but will not write
`/oem/beskar7/provider-id`, so `MachineDeployment` pools stay on the P1
hand-authored pattern.

### Procedure

```bash
# 1. Fix existing CRs FIRST (see above) — validation is applied on CRD upgrade.

# 2. CRDs
kubectl apply -f https://github.com/projectbeskar/beskar7/releases/download/v0.4.0-alpha.9/beskar7-manifests-v0.4.0-alpha.9.yaml

# 3. Controller
helm repo update
helm upgrade --devel beskar7 beskar7/beskar7 -n beskar7-system --version 0.4.0-alpha.9

# 4. Inspector artifacts on your boot server
REL=https://github.com/projectbeskar/beskar7-inspector/releases/latest/download
curl -fsSLO $REL/vmlinuz && curl -fsSLO $REL/initrd.img && curl -fsSLO $REL/sha256sums.txt
sha256sum -c sha256sums.txt --ignore-missing

# 5. Verify
kubectl -n beskar7-system rollout status deploy/beskar7-controller-manager
kubectl get physicalhosts -A     # existing hosts should reconcile unchanged
```

## Earlier `v0.4.0-alpha.*` releases

Upgrade to the latest alpha directly; there is no supported skip-version path
between individual alphas. Read the `CHANGELOG.md` entries between your version
and the target — several alphas contain breaking API changes, and each is listed
under **Changed → BREAKING**.

## `v0.3.x` → `v0.4.x`

**There is no in-place upgrade path.** v0.4 is a clean break: the provisioning
model changed from `RemoteConfig`/`PreBakedISO`/kexec to iPXE inspection plus a
digest-pinned whole-disk image write, and the v0.3 API fields (`osFamily`,
`imageURL`, `configURL`, `provisioningMode`, `bootMode`) no longer exist.

Migrate by standing up v0.4 alongside and re-provisioning hosts into it. Plan for
hosts to be reinstalled — the OS handoff mechanism is different.

## Rolling back

Roll the controller back with `helm rollback`. **CRDs do not roll back
automatically**, and a CRD that has already been narrowed (for example
`caBundleSecretRef` object → string) will reject an older controller's writes.
If you must roll back across a breaking CRD change, re-apply the CRDs from the
older release tag first.

## Troubleshooting an upgrade

| Symptom | Likely cause |
|---|---|
| `unknown field "spec.targetImageDigest"` | CRDs are older than the controller — apply CRDs first |
| CRs rejected on CRD apply | fix the CRs before upgrading (see the breaking changes above) |
| Hosts provision but Machines stay `Provisioned` | ProviderID mismatch — see [troubleshooting](troubleshooting.md) entry 12 |
| Inspector never posts a report | inspector/controller contract mismatch — check `contract-version.txt` |
