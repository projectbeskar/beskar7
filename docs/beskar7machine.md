# Beskar7Machine

> **Audience:** Operators

`Beskar7Machine` is the Beskar7 CRD that implements the CAPI infrastructure-machine contract. The reconciler that owns it is `controllers/beskar7machine_controller.go`. One Beskar7Machine maps to one Kubernetes node.

For the full field reference, see [API Reference: Beskar7Machine](api-reference.md#beskar7machine). This page covers operational behavior — the reconcile flow, what conditions mean, how failures surface.

## Identity

- **API:** `infrastructure.cluster.x-k8s.io/v1beta1`
- **Kind:** `Beskar7Machine`
- **Scope:** Namespaced
- **Categories:** `cluster-api`

## Spec at a glance

```yaml
spec:
  inspectionImageURL: "https://boot-server.local/inspector"              # base URL serving vmlinuz + initrd.img
  targetImageURL:     "http://boot-server.local/images/kairos-alpine-v2.8.1.raw"  # Kairos whole-disk raw image
  targetImageDigest:  "sha256:<64-hex-digest-of-the-bytes-at-targetImageURL>"
  hardwareRequirements:                                                 # optional
    minCPUCores: 4
    minMemoryGB: 8
    minDiskGB:   50
  # providerID is set by the controller on claim — do not set manually
```

`inspectionImageURL` and `targetImageURL` must match `^https?://[^\s]+$`; `targetImageDigest` is required and must match `^sha256:[a-f0-9]{64}$` — the inspector refuses to mount, inject user-data, or reboot on a digest mismatch (`docs/inspector-contract.md` §8.1). There is no `osFamily`, `imageURL`, `bootMode`, `provisioningMode`, `configURL` (removed v0.3 fields), or `configurationURL` (a dead, never-wired v0.4 field removed before GA).

## Reconcile flow

The reconciler runs through these phases. Each phase corresponds to a state of the claimed `PhysicalHost`.

1. **Wait for owner Machine.** If the owning CAPI `Machine` does not have an `OwnerReference` to this Beskar7Machine yet, requeue.
2. **Bootstrap data check.** Read `Machine.Spec.Bootstrap.DataSecretName`. If unset, mark `BootstrapDataReady=False (WaitingForBootstrapData)` and requeue. If set but the Secret is not found, mark `BootstrapDataReady=False (BootstrapDataUnavailable)` and set `FailureReason` (terminal).
3. **Find or claim a host.** `findAndClaimOrGetAssociatedHost` runs three lookups in order:
    1. If `Spec.ProviderID` is set (only after inspection completes), `Get` the host directly by the encoded `<ns>/<name>` and return it.
    2. List PhysicalHosts in the namespace and return the one whose `Spec.ConsumerRef.Name` matches this Beskar7Machine — covers the window between claim and `ProviderID` assignment. Without this branch the controller would forget its own claim after the first reconcile (the host has transitioned to `InUse` so the next branch's `Available` filter skips it).
    3. List `PhysicalHost` objects in the namespace filtered by the `status.state` field index for `Available`. The first host with no `ConsumerRef` is claimed via an optimistic-locking patch. Concurrent claims fail fast with `Conflict`; the loser requeues.

    See `controllers/beskar7machine_controller.go:findAndClaimOrGetAssociatedHost`.
4. **Signal the bootstrap URL.** Compute the URL deterministically as `<--bootstrap-url-base>/api/v1/bootstrap/<ns>/<host>` and patch `infrastructure.cluster.x-k8s.io/bootstrap-url` onto the host's annotations. The host reconciler persists it to `Status.Bootstrap.URL`.
5. **Trigger inspection.** When the host transitions to `InUse`:
    - Open a Redfish client with the host's credentials.
    - `SetBootSourcePXE` then `SetPowerState(On)` if not already powered on.
    - Mint a per-host bearer token unless an unexpired one is already in `Status.Bootstrap` (re-using avoids invalidating in-flight kernel cmdlines). The plaintext goes into a per-host Secret (`<host>-bootstrap-token`); only the SHA-256 hash and lifetime ride the `bootstrap-token` annotation. See `internal/auth/token.go` and decision D-004.
    - Patch the `inspection-request` annotation on the host with value `inspect`. The host reconciler transitions to `Inspecting`.
6. **Wait for inspection.** While the host is `Inspecting`, monitor `Status.InspectionPhase`. If `Status.InspectionTimestamp` is older than the inspection timeout (default 10 minutes; override with the `--inspection-timeout` manager flag for slow-POST hardware), call `markTerminalFailure(InspectionTimedOut, ...)` and stop. Inspection timeout is terminal — the operator must investigate (likely an iPXE misconfiguration) and delete-and-recreate.
7. **Validate hardware.** When `Status.InspectionPhase == Complete`, sum CPU cores across `report.cpus[]`, parse memory across `report.memory[]` (using the `parseMemoryCapacityGB` helper for `GB`/`GiB`/`MB`/`MiB`/`TB`/`TiB` suffixes), and sum disk size across `report.disks[]`. If any minimum is violated, call `markTerminalFailure(HardwareRequirementsNotMet, ...)`. Hardware-validation failures are terminal — the BMC's hardware does not change at runtime.
8. **Mark ready.** When validation passes, signal the host (`inspection-request: inspect-complete`) which moves the host to `Ready`. The Beskar7Machine then sets `Spec.ProviderID = b7://<ns>/<name>`, copies addresses from the host, sets `InfrastructureReady=True`, `Status.Ready=true`, and `Status.Initialization.Provisioned=true` (the CAPI v1beta2 contract field that CAPI core lifts into `Machine.status.initialization.infrastructureProvisioned` and uses to advance the parent Machine past `Pending`).

## Conditions

| Type | Meaning | Common reasons |
|---|---|---|
| `InfrastructureReady` | Standard CAPI infra-ready condition. Summary across the others; True once the host reaches `Ready` (the inspector's provisioned callback was received) and `ProviderID` is set. | – |
| `PhysicalHostAssociated` | A host has been claimed. | `WaitingForPhysicalHost`, `PhysicalHostAssociationFailed`. |
| `BootstrapDataReady` | `Machine.Spec.Bootstrap.DataSecretName` is set and the URL has been signalled. | `WaitingForBootstrapData`, `BootstrapDataUnavailable`. |

There is no `MachineProvisioned` condition — it was declared but never set by any reconciler and has been removed from the API. Use `InfrastructureReady` (backed by `Status.Ready` and `Status.Initialization.Provisioned`) as the provisioned signal.

## Terminal failures

These set `Status.FailureReason` and `Status.FailureMessage`. Once set, the controller stops requeueing — operator must intervene. CAPI surfaces both fields in `kubectl describe machine`.

| Reason | Trigger |
|---|---|
| `BootstrapDataUnavailable` | The Secret named by `Machine.Spec.Bootstrap.DataSecretName` does not exist. |
| `HardwareRequirementsNotMet` | Inspection report falls below `hardwareRequirements`. |
| `InspectionTimedOut` | No inspection report received within the inspection timeout (default 10 min; `--inspection-timeout` flag). |

To recover, delete the Beskar7Machine (and its owner `Machine`); the host returns to `Available` and a fresh attempt can be made.

## Deletion

`reconcileDelete` runs:

1. If a `ProviderID` is set and the parsed host exists with `ConsumerRef.Name == this.Name`:
    - Best-effort: open the Redfish client and call `ClearBootSourceOverride` then `SetPowerState(Off)` (graceful). All errors are logged and swallowed so a dead BMC cannot strand the finalizer.
    - Patch `ConsumerRef = nil` on the host with optimistic locking.
2. Remove the finalizer (`beskar7machine.infrastructure.cluster.x-k8s.io`).

The `infrastructure.cluster.x-k8s.io/force-release: "true"` annotation skips the Redfish steps entirely. Use only when the BMC is permanently unreachable.

## ProviderID format

The provider ID is `b7://<namespace>/<name>`, where `<name>` is the **PhysicalHost** name (the controller builds it from `providerID(physicalHost.Namespace, physicalHost.Name)`). The parser uses `strings.CutPrefix` + `strings.SplitN(rest, "/", 2)` and rejects empty segments and multi-segment names. See `controllers/beskar7machine_controller.go:parseProviderID`.

## ProviderID & Node association

Setting `Beskar7Machine.Spec.ProviderID` marks the **infrastructure** ready, which advances the CAPI `Machine` to `Provisioned`. It does **not**, on its own, advance the Machine to `Running`. CAPI reaches `Running` only after it associates the Machine with a Kubernetes Node, and it does that by matching the Machine's `spec.providerID` against the Node's `spec.providerID` — **the two must be exactly equal**.

A freshly provisioned node's kubelet, left to its defaults, registers a ProviderID that does **not** match `b7://...` (k3s, for example, uses `k3s://<hostname>`). So you must tell the node's kubelet to register with the value Beskar7 assigns:

```
b7://<namespace>/<physicalhost-name>
```

For a hand-authored Machine you set this in the **per-machine bootstrap config** (the `#cloud-config` / `KubeadmConfig` referenced by `Machine.Spec.Bootstrap.DataSecretName`), because the value is only known once you know which PhysicalHost the Machine uses. For **templated pools**, where you cannot know it in advance, see [Templated pools](#templated-pools-the-per-host-value-from-a-shared-template) below.

**k3s** (proven path — see [`examples/kairos-k3s-node.yaml`](../examples/kairos-k3s-node.yaml)):

```yaml
k3s:
  enabled: true
  args:
    - "--kubelet-arg=provider-id=b7://<namespace>/<host-name>"
```

**kubeadm** (CAPI `KubeadmConfig` / `KubeadmConfigTemplate`):

```yaml
initConfiguration:    # use joinConfiguration for worker / secondary control-plane nodes
  nodeRegistration:
    kubeletExtraArgs:
      provider-id: "b7://<namespace>/<host-name>"
```

### Templated pools: the per-host value from a shared template

Both snippets above hard-code one host, which is fine for a hand-authored Machine but impossible
from a shared `Beskar7MachineTemplate` — the template has no idea which `PhysicalHost` a replica
will claim. Since **contract v4.2** the inspector writes the correct per-host value to
**`/oem/beskar7/provider-id`** during provisioning, and a small **image-side** stage turns it into
the kubelet flag. The stage is host-independent, so one image and one template serve every replica:
[`examples/kairos-providerid-stage.yaml`](../examples/kairos-providerid-stage.yaml).

Two constraints, both of which fail silently if ignored:

- **The stage must be a yip config in the image's `/oem`, not a `#cloud-config` `stages:` block in
  the bootstrap Secret.** Kairos honors a `#cloud-config` file's top-level keys but ignores its
  `stages:`, processing it as `'<file>.0'` with `commands: 0` and logging nothing that reads as an
  error. A yip config (top-level `name:` plus `stages:`) executes.
- **It must run before the distro first starts.** `Node.spec.providerID` is immutable after the
  node registers; a node that joins without the flag keeps the distro default and must be
  re-provisioned rather than fixed in place.

Verified end to end on Kairos v4.1.2 (hadron) with k3s v1.34.8: `Node.spec.providerID` came up as
`b7://<namespace>/<host>`, and CAPI advanced the Machine past `Provisioned`.

**Other distros** (k0s, plain kubelet): set the kubelet `--provider-id` flag to the same value via your distro's kubelet-args mechanism.

If you skip this, the node still comes up and is `Ready`, but the CAPI `Machine` stays at `Provisioned` and never reaches `Running` (the Node is never bound). See [Troubleshooting → CAPI Machine stuck at Provisioned](troubleshooting.md#12-capi-machine-stuck-at-provisioned-never-reaches-running-node-not-associated).

> **Scaled deployments:** this manual wiring works when you author the per-machine bootstrap config and therefore know which PhysicalHost the Machine will use (a single node, or hosts pinned to specific machines). A templated `MachineDeployment` that claims hosts from a pool cannot pin a per-host ProviderID in one shared template; automatic provision-time delivery is planned future work.

## Example

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
kind: Beskar7Machine
metadata:
  name: control-plane-01
  namespace: default
  labels:
    cluster.x-k8s.io/cluster-name: production-cluster
    cluster.x-k8s.io/control-plane: "true"
spec:
  inspectionImageURL: "https://boot-server.local/inspector"
  targetImageURL:     "http://boot-server.local/images/kairos-alpine-v2.8.1.raw"
  targetImageDigest:  "sha256:0000000000000000000000000000000000000000000000000000000000000000"
  hardwareRequirements:
    minCPUCores: 4
    minMemoryGB: 16
    minDiskGB:   100
```

## Print columns

`kubectl get beskar7machine` shows `Cluster`, `Machine`, `Phase`, and `Age`.

## See also

- [API Reference: Beskar7Machine](api-reference.md#beskar7machine)
- [State Management](state-management.md)
- [Architecture](architecture.md)
- [Beskar7MachineTemplate](beskar7machinetemplate.md)
- [Troubleshooting](troubleshooting.md)
