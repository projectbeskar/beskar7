# Upgrading Beskar7

> **Audience:** Operators

`v0.4.0` is the first GA release. `v1beta1` is stable and evolves additive-only
from here, but the **alpha series leading up to it contains breaking changes** —
read the section for your starting version before upgrading.

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

### Preserving your values across the upgrade

Do **not** use `helm upgrade --reuse-values`. It discards the incoming chart's
`values.yaml` entirely and renders against the previous release's value set, so
any value key introduced since the release you installed is simply absent. The
upgrade then fails to render, or worse, silently drops a setting. Use one of
these instead:

```bash
# Helm 3.14+ — replays your overrides on top of the NEW chart's defaults.
helm upgrade beskar7 beskar7/beskar7 -n beskar7-system --version 0.4.2 \
  --reset-then-reuse-values
```

```bash
# Any Helm version — keep your settings in a file and pass it every time.
helm upgrade beskar7 beskar7/beskar7 -n beskar7-system --version 0.4.2 \
  -f my-beskar7-values.yaml
```

Keeping a values file under version control is the recommended practice: it makes
the upgrade reproducible and reviewable. Confirm the result before and after with
`helm get values beskar7 -n beskar7-system`.

## Matching the inspector to the controller

The controller and inspector share a **versioned wire contract**. A controller
speaking contract `vN` requires an inspector implementing `vN`.

The running controller does not currently report its contract version, so map it
from the release you deployed:

| beskar7 release | contract |
|---|---|
| `v0.4.2` | `v4.2` **frozen** |
| `v0.4.1` | `v4.2` **frozen** |
| `v0.4.0` (GA) | `v4.2` **frozen** |
| `v0.4.0-alpha.9` | `v4.2` |
| `v0.4.0-alpha.8` | `v4.1` |
| `v0.4.0-alpha.7` | `v4` |

From a source checkout of the matching tag:

```bash
# alpha.9 and later carry a machine-readable marker:
git show v0.4.0:test/contract/VERSION      # -> v4.2

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

## `v0.4.1` → `v0.4.2` — one-time manual step for `kubectl apply` installs

No API, CRD or contract change. **Helm users: nothing special, upgrade normally.**

```bash
helm repo update
helm upgrade beskar7 beskar7/beskar7 -n beskar7-system --version 0.4.2 \
  --reset-then-reuse-values
```

### If you installed with `kubectl apply -f beskar7-manifests-*.yaml`, read this

Releases up to and including `v0.4.1` baked `app.kubernetes.io/version` into
`Deployment.spec.selector`. That field is **immutable**, and the label changes
every release, so upgrading by re-applying the manifest has never worked:

```
The Deployment "controller-manager" is invalid: spec.selector:
Invalid value: {...}: field is immutable
```

The same label was in the webhook Service selector, so after a partial upgrade
that Service could stop matching the controller Pod and sit with no endpoints —
and because the webhook is `failurePolicy: Fail`, every `Beskar7Cluster` create,
update and **delete** was then rejected, which shows up as a cluster that will
not finish deleting.

`v0.4.2` takes the version label out of both selectors, so from here on
`kubectl apply` upgrades work normally. Getting to `v0.4.2` needs one manual
step, because changing a selector is exactly what the old objects forbid:

```bash
# 1. Delete the two objects whose selectors changed. CRDs, CRs and your hosts
#    are untouched — this only removes the controller Pod for a moment.
kubectl -n beskar7-system delete deployment beskar7-controller-manager
kubectl -n beskar7-system delete service beskar7-webhook-service

# 2. Apply the new manifest.
kubectl apply -f https://github.com/projectbeskar/beskar7/releases/download/v0.4.2/beskar7-manifests-v0.4.2.yaml

# 3. Confirm the webhook Service actually has an endpoint — an empty list here
#    is what blocks Beskar7Cluster admission.
kubectl -n beskar7-system get endpoints beskar7-webhook-service
```

Provisioning is not interrupted by the controller being down briefly: state
lives in the CRs and a host mid-inspection is picked up on the next reconcile.

**If a `Beskar7Cluster` is already stuck deleting** because the webhook has no
endpoints, completing the steps above unblocks it — the finalizer clears as soon
as admission works again. Do not remove the finalizer by hand.

## `v0.4.0` → `v0.4.1` — chart fix only, no API or contract change

`v0.4.1` leaves the CRDs and the wire contract untouched, so there is nothing to
re-apply and no bootstrap-template migration. It carries one chart fix and one
controller change, both opt-in or invisible by default.

**Why upgrade:** the `v0.4.0` chart cannot be upgraded with `--reuse-values` —
it aborts before rendering with `nil pointer evaluating interface {}.service`
(or `.externalNames`). That flag discards the incoming chart's defaults, so
`callback` and `bootstrap`, both added after alpha.6, are missing. `v0.4.1`
tolerates the missing maps and falls back to the documented defaults.

```bash
helm repo update
helm upgrade beskar7 beskar7/beskar7 -n beskar7-system --version 0.4.1 \
  --reset-then-reuse-values
```

CRDs are unchanged, so there is nothing to re-apply. If you are already on
`v0.4.0` and previously worked around the bug by re-passing your values with
`-f`, that keeps working — no action needed beyond the version bump.

**Worth checking while you are here:** `/boot` is rate-limited per source
address, and a LoadBalancer or NodePort Service with the default
`externalTrafficPolicy: Cluster` SNATs every host to a node IP — so your whole
fleet shares one 1 r/s bucket and a mass power-on boots slowly. `v0.4.1` adds
two ways out: `callback.service.externalTrafficPolicy: Local` to preserve the
client IP, or `callback.trustedProxies` to declare the hops allowed to set
`X-Forwarded-For`. Both default to off, so nothing changes until you set them.
See [ipxe-setup.md](ipxe-setup.md) for which to pick.

## `v0.4.0-alpha.9` → `v0.4.0` — no API changes

`v0.4.0` is `alpha.9` plus documentation, examples and the contract freeze. There
are **no API or wire-contract changes**, so this is a straight image + chart
upgrade:

```bash
kubectl apply -f https://github.com/projectbeskar/beskar7/releases/download/v0.4.0/beskar7-manifests-v0.4.0.yaml
helm repo update && helm upgrade beskar7 beskar7/beskar7 -n beskar7-system --version 0.4.0
```

The `--devel` flag is no longer needed: `0.4.0` is not a SemVer pre-release.

**One thing worth acting on even though nothing forces you to.** If you run
templated `MachineDeployment` pools, the per-host ProviderID stage documented
before `v0.4.0` **never executed** — Kairos silently ignores a `stages:` block in
a `#cloud-config` file. The working form is a yip config baked into the target
image: [`examples/kairos-providerid-stage.yaml`](../examples/kairos-providerid-stage.yaml).
Existing nodes that joined without it kept their distro-default ProviderID
(`k3s://<hostname>`), and because `Node.spec.providerID` is immutable they must be
**re-provisioned** to pick up `b7://<ns>/<host>` — it cannot be fixed in place.

## `v0.4.0-alpha.8` → `v0.4.0` — **breaking**

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

`v0.4.0` speaks **contract v4.2** (frozen), which adds per-host `ProviderID`
delivery for templated pools. Upgrade the inspector to a `contract-v4.2` build. A
v4.1 inspector keeps working — it ignores the new cmdline parameter — but will not
write `/oem/beskar7/provider-id`, so `MachineDeployment` pools stay on the
hand-authored per-host pattern.

### Procedure

```bash
# 1. Fix existing CRs FIRST (see above) — validation is applied on CRD upgrade.

# 2. CRDs
kubectl apply -f https://github.com/projectbeskar/beskar7/releases/download/v0.4.0/beskar7-manifests-v0.4.0.yaml

# 3. Controller
helm repo update
helm upgrade beskar7 beskar7/beskar7 -n beskar7-system --version 0.4.0

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
