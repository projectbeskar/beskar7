# Beskar7Machine

> **Audience:** Operators

`Beskar7Machine` is the Beskar7 CRD that implements the CAPI infrastructure-machine contract. The reconciler that owns it is `controllers/beskar7machine_controller.go`. One Beskar7Machine maps to one Kubernetes node.

For the full field reference, see [API Reference: Beskar7Machine](api-reference.md#beskar7machine). This page covers operational behavior — the reconcile flow, what conditions mean, how failures surface.

## Identity

- **API:** `infrastructure.cluster.x-k8s.io/v1beta2`
- **Kind:** `Beskar7Machine`
- **Scope:** Namespaced
- **Categories:** `cluster-api`
- **clusterctl:** the CRD carries `clusterctl.cluster.x-k8s.io`, so `clusterctl move` discovers machines; a machine is moved through its owner reference to the CAPI `Machine` (see [Installation](installation.md#clusterctl-move)).

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
  hostSelector:                                                         # optional; see "Steering the claim"
    matchLabels:
      node-role: control-plane
  # providerID is set by the controller on claim — do not set manually
```

`inspectionImageURL` and `targetImageURL` must match `^https?://[^\s]+$`; `targetImageDigest` is required and must match `^sha256:[a-f0-9]{64}$` — the inspector refuses to mount, inject user-data, or reboot on a digest mismatch (`docs/inspector-contract.md` §8.1). There is no `osFamily`, `imageURL`, `bootMode`, `provisioningMode`, `configURL` (removed v0.3 fields), or `configurationURL` (a dead, never-wired v0.4 field removed before GA).

## Steering the claim: `hostSelector` and failure domains

By default a Beskar7Machine claims the first `Available` PhysicalHost in its namespace, in list order. Two things narrow that, and they are ANDed:

- **`spec.hostSelector`** — a standard label selector over the hosts' `metadata.labels`. Label the inventory by role, rack or hardware class and give each template a selector. A control plane and a worker `MachineDeployment` provisioning at the same time then draw from disjoint pools instead of racing for the same boxes:

  ```yaml
  # on the PhysicalHosts
  metadata:
    labels:
      node-role: control-plane          # or: worker
  ---
  # in the Beskar7MachineTemplate the control plane uses
  spec:
    template:
      spec:
        hostSelector:
          matchLabels:
            node-role: control-plane
  ```

  `matchExpressions` work too (`In`, `NotIn`, `Exists`, `DoesNotExist`). An absent or empty selector means any host, so existing deployments are unaffected. See [`examples/host-pools.yaml`](../examples/host-pools.yaml).
- **`Machine.spec.failureDomain`** — CAPI's placement. `Beskar7Cluster` publishes failure domains from the `topology.kubernetes.io/zone` label on hosts; when CAPI assigns a Machine to one, the claim only considers hosts carrying that zone label.

Placement applies to a **fresh claim only**: a host the machine already holds is never re-evaluated, so labelling or relabelling hosts moves future claims, not running nodes. `hardwareRequirements` does **not** steer the claim — it is validated after inspection, and a mismatch is terminal — so on a mixed inventory use a selector to land on the right class of host in the first place.

When hosts are `Available` but none satisfies the constraint, `PhysicalHostAssociated=False` carries reason `NoMatchingPhysicalHost` (an empty inventory reports `WaitingForPhysicalHost`) and the machine requeues every minute — sooner if a `PhysicalHost` in the namespace becomes `Available` in the meantime, which re-enqueues every machine still waiting for a host. A selector that cannot be parsed (an unknown operator, for example) is terminal: `InvalidHostSelector`.

## Reconcile flow

The reconciler runs through these phases. Each phase corresponds to a state of the claimed `PhysicalHost`.

1. **Wait for owner Machine.** If the owning CAPI `Machine` does not have an `OwnerReference` to this Beskar7Machine yet, requeue.
2. **Bootstrap data check.** Read `Machine.Spec.Bootstrap.DataSecretName`. If unset, mark `BootstrapDataReady=False (WaitingForBootstrapData)` and requeue. If set but the Secret is not found, mark `BootstrapDataReady=False (BootstrapDataUnavailable)` and mark the machine terminally failed (terminal).
3. **Find or claim a host.** `findAndClaimOrGetAssociatedHost` runs three lookups in order:
    1. If `Spec.ProviderID` is set (only after inspection completes), `Get` the host directly by the encoded `<ns>/<name>` and return it.
    2. List PhysicalHosts in the namespace and return the one whose `Spec.ConsumerRef.Name` matches this Beskar7Machine — covers the window between claim and `ProviderID` assignment. Without this branch the controller would forget its own claim after the first reconcile (the host has transitioned to `InUse` so the next branch's `Available` filter skips it).
    3. List `PhysicalHost` objects in the namespace filtered by the `status.state` field index for `Available` — and by the machine's placement constraint: `spec.hostSelector` when set, ANDed with the label `topology.kubernetes.io/zone=<domain>` when CAPI has placed the owning `Machine` into a failure domain (`Machine.spec.failureDomain`; the same label `Beskar7Cluster` derives its failure domains from). The first host with no `ConsumerRef` is claimed via an optimistic-locking patch. Concurrent claims fail fast with `Conflict`; the loser requeues. If hosts are `Available` but none is in the Machine's failure domain, the condition `PhysicalHostAssociated=False` carries reason `NoMatchingPhysicalHost` (as opposed to `WaitingForPhysicalHost` for an empty inventory) and the machine requeues; it never claims a host outside its domain. Placement applies only to a fresh claim — lookups 1 and 2 return the host the machine already holds.

    See `controllers/beskar7machine_controller.go:findAndClaimOrGetAssociatedHost`.
4. **Signal the bootstrap URL.** Compute the URL deterministically as `<--bootstrap-url-base>/api/v1/bootstrap/<ns>/<host>` and patch `infrastructure.cluster.x-k8s.io/bootstrap-url` onto the host's annotations. The host reconciler persists it to `Status.Bootstrap.URL`.
5. **Trigger inspection.** When the host transitions to `InUse`:
    - Open a Redfish client with the host's credentials.
    - `SetBootSourcePXE` then `SetPowerState(On)` if not already powered on.
    - Mint a per-host bearer token unless an unexpired one is already advertised (in `Status.Bootstrap`, or in a `bootstrap-token` annotation the host reconciler has not promoted yet) **and** the per-host Secret still holds its plaintext (`auth.Verify(plaintext, tokenHash)`). Re-using avoids invalidating in-flight kernel cmdlines; the Secret cross-check makes sure a credential the host could never authenticate with is replaced rather than reused: a Secret that is missing, or does not hash to the advertised value, gets a fresh mint, logged at Info with the host name (never the token). Without that check a one-off Secret/status split — two managers minting for the same host at once, say — 401s every callback until the token expires, and survives re-claims because a re-claim inherits `Status.Bootstrap`. The boot nonce follows the same rule. The plaintext goes into a per-host Secret (`<host>-bootstrap-token`); only the SHA-256 hash and lifetime ride the `bootstrap-token` annotation. See `internal/auth/token.go` and decision D-004.
    - Patch the `inspection-request` annotation on the host with value `inspect`. The host reconciler transitions to `Inspecting`.

    If the host is in `Error` instead because it cannot reach its BMC, the machine waits rather than failing and picks up here once the host is back at `InUse` — see [A BMC outage is not a terminal failure](#a-bmc-outage-is-not-a-terminal-failure).
6. **Wait for inspection.** While the host is `Inspecting`, monitor `Status.InspectionPhase`. If `Status.InspectionTimestamp` is older than the inspection timeout (default 10 minutes; override with the `--inspection-timeout` manager flag for slow-POST hardware), call `markTerminalFailure(InspectionTimedOut, ...)` and stop. Inspection timeout is terminal — the operator must investigate (likely an iPXE misconfiguration) and delete-and-recreate.
7. **Validate hardware.** When `Status.InspectionPhase == Complete`, sum CPU cores across `report.cpus[]`, parse memory across `report.memory[]` (using the `parseMemoryCapacityGB` helper for `GB`/`GiB`/`MB`/`MiB`/`TB`/`TiB` suffixes), and sum disk size across `report.disks[]`. If any minimum is violated, call `markTerminalFailure(HardwareRequirementsNotMet, ...)`. Hardware-validation failures are terminal — the BMC's hardware does not change at runtime.
8. **Mark ready.** When validation passes, signal the host (`inspection-request: inspect-complete`) which moves the host to `Ready`. The Beskar7Machine then sets `Spec.ProviderID = b7://<ns>/<name>`, copies addresses from the host, sets `InfrastructureReady=True`, `Status.Ready=true`, and `Status.Initialization.Provisioned=true` (the CAPI v1beta2 contract field that CAPI core lifts into `Machine.status.initialization.infrastructureProvisioned` and uses to advance the parent Machine past `Pending`).

## Conditions

Every condition is a native `metav1.Condition` (`status.conditions[]`: `type`, `status`, `reason`, `message`, `lastTransitionTime`, `observedGeneration`) — there is no `severity` field, and a `True` condition carries a `reason` too.

`Ready` is the **summary** condition: the controller computes it every reconcile from the other three (worst status wins), it is not itself hand-set. It is also the condition Cluster API mirrors into the owning `Machine`'s own `InfrastructureReady` condition — a same-named-but-different condition on a different object than this resource's own `InfrastructureReady` row below. See [API Reference → Conditions and the CAPI mirror](api-reference.md#conditions-and-the-capi-mirror).

| Type | Meaning | True reason | Other reasons |
|---|---|---|---|
| `Ready` | Summary of the three rows below. | Derived from the summarized conditions. | Same, `False`/`Unknown`. |
| `InfrastructureReady` | True once the host reaches `Ready` (the inspector's provisioned callback was received) and `ProviderID` is set. | `Provisioned` | `PhysicalHostNotReady` (a host is claimed and not provisioned yet: set from the claim on, through inspection and deployment, so `Ready` never reads `True` early); `WaitingForBMC` (the host cannot reach its BMC — not terminal, see [below](#a-bmc-outage-is-not-a-terminal-failure)); terminal — see [Terminal failures](#terminal-failures). |
| `PhysicalHostAssociated` | A host has been claimed. | `PhysicalHostAssociated` | `WaitingForPhysicalHost`, `NoMatchingPhysicalHost`, `PhysicalHostAssociationFailed`, `InvalidHostSelector` (terminal). |
| `BootstrapDataReady` | `Machine.Spec.Bootstrap.DataSecretName` is set and the URL has been signalled. | `BootstrapDataReady` | `WaitingForBootstrapData`, `BootstrapDataUnavailable` (terminal). |
| `Paused` | Maintained by `sigs.k8s.io/cluster-api/util/paused`. See [Paused](#paused). | `NotPaused` | `Paused`. |

There is no `MachineProvisioned` condition — it was declared but never set by any reconciler and has been removed from the API. Use `Ready` (backed by `Status.Ready` and `Status.Initialization.Provisioned`) as the provisioned signal.

## Paused

`paused.EnsurePausedCondition` (`sigs.k8s.io/cluster-api/util/paused`) runs before every reconcile, including deletion, and pauses when either is true:

- **`Cluster.spec.paused`** — the CAPI `Cluster`'s own spec field. This is what `clusterctl move` sets on the source cluster before moving objects and clears on the target afterwards.
- **The `cluster.x-k8s.io/paused` annotation on this `Beskar7Machine`.**

It does **not** check the annotation on the owning `Cluster` object — only `Cluster.spec.paused` and the annotation on this object. If you were previously pausing by annotating the `Cluster` directly, switch to `Cluster.spec.paused` (or annotate the `Beskar7Machine` itself); the old pattern stopped working when this controller moved off its own hand-rolled `isClusterPaused` check. See [Upgrading](upgrading.md).

While paused, the reconciler returns immediately — a `Beskar7Machine` with a `DeletionTimestamp` does not finish deleting until it is unpaused.

## Terminal failures

A terminal failure is `status.phase: Failed`, `status.ready: false`, and the `InfrastructureReady` condition `False` with the reason below and a message describing the specific cause. `Ready` (the summary) goes `False` too. Once `status.phase` reads `Failed` (`isTerminallyFailed`), the controller stops reconciling that machine — it will never be healed by a later state change; only deletion is still handled. Cluster API mirrors `Ready` into the owning `Machine`'s own `InfrastructureReady` condition, which a `MachineHealthCheck`'s `unhealthyMachineConditions` can key on — see [Remediating with a `MachineHealthCheck`](#remediating-with-a-machinehealthcheck) for how, and why its timeout must outlast provisioning. There is no `status.failureReason` / `status.failureMessage` any more, and `MachineHealthCheck` on Cluster API v1.11+ does not read those fields even when present on other providers.

| Reason | Trigger |
|---|---|
| `BootstrapDataUnavailable` | The Secret named by `Machine.Spec.Bootstrap.DataSecretName` does not exist. |
| `HardwareRequirementsNotMet` | Inspection report falls below `hardwareRequirements` (CPU, memory, or disk). |
| `InvalidHostSelector` | `spec.hostSelector` cannot be parsed (for example an unknown `matchExpressions` operator). It can never match; fix the template and roll the machine. |
| `InspectionTimedOut` | No inspection report received within the inspection timeout (default 10 min; `--inspection-timeout` flag). |
| `InspectionFailed` | The claimed `PhysicalHost` reported `Status.InspectionPhase = Failed` — the inspector booted but the inspection itself errored. Check the host's `Status` and serial console. |
| `DeploymentTimedOut` | The host stayed in `Deploying` (OS image write) longer than the deployment timeout (default 20 min; `--deployment-timeout` flag). |
| `DeploymentFailed` | The inspector explicitly reported a deploy failure via `POST /api/v1/provision-failed` (contract v4.1) — image fetch, digest verify, disk write, or `COS_OEM` inject failed. |
| `PhysicalHostError` | The claimed `PhysicalHost` entered `StateError` for a Redfish/BMC-level reason that needs a change to clear — missing or refused credentials, a certificate the client rejects, a malformed address, a BMC with no `ComputerSystem`, the `insecureSkipVerify`/CA-bundle conflict — rather than a reported deploy failure. An unreachable BMC is not one of these; see below. |

To recover, delete the Beskar7Machine (and its owner `Machine`); the host returns to `Available` and a fresh attempt can be made.

### A BMC outage is not a terminal failure

A BMC that cannot be reached at the network level — a refused or reset connection, no route, a DNS failure, a timeout, a 502/503/504 from a BMC that is still starting — is a fact about the world rather than about the machine, and it usually clears by itself. The `PhysicalHost` reports it as `RedfishConnectionReady=False` with reason `BMCUnreachable` (the only reason it sets for this class) and retries every 15 seconds. What the Beskar7Machine does depends on how far provisioning got:

- **Before inspection** (the host was `InUse`): the host goes to `Error`, and the machine waits with `InfrastructureReady=False`, reason `WaitingForBMC`, a message quoting the host's error, and its phase unchanged. It is re-checked every 30 seconds and at once whenever the host changes, and once the host is back at `InUse` it triggers inspection as if nothing had happened, with the reason back at `PhysicalHostNotReady`.
- **Inspecting, Deploying or Ready**: the host keeps its state — the inspector and the installed OS do not need the BMC — and only its condition reports the outage, so the machine carries on untouched. A provisioned machine stays `Ready`. The inspection and deployment timeouts keep running meanwhile, and the host applies the inspector's callbacks only once it can reach the BMC again, so an outage that outlasts the remaining timeout still ends in `InspectionTimedOut` or `DeploymentTimedOut`.

`WaitingForBMC` is not terminal, but it is still `InfrastructureReady=False`, and so is the `Ready` summary Cluster API mirrors onto the owning `Machine`. A `MachineHealthCheck` sees only that status and how long it has held — not the reason — so the outage counts against its timeouts like any other part of provisioning. The [recommended ones](#remediating-with-a-machinehealthcheck) wait it out as long as the machine still finishes provisioning within `nodeStartupTimeoutSeconds`, and replace it after that, which costs the host nothing: the machine had not started inspection, so it has written nothing to the disk. With `timeoutSeconds: 0` on the `InfrastructureReady` check the waiting machine is replaced at once.

### Remediating with a `MachineHealthCheck`

Beskar7 never retries a failed machine itself ([inspector contract §12](inspector-contract.md#12-retry-policy)); replacing one is a `MachineHealthCheck`'s decision, and [`examples/machinehealthcheck.yaml`](../examples/machinehealthcheck.yaml) is the recommended one. The check can notice a Beskar7 machine in two ways, and neither shows it the reason:

- **An `unhealthyMachineConditions` entry on `InfrastructureReady`** reads the owning `Machine`'s condition, which Cluster API mirrors from this resource's `Ready` summary, by its status and how long it has held it. A Beskar7Machine does not report `Ready=True` until its host is provisioned. Until then it reports `False`: while it waits for a free host (`WaitingForPhysicalHost`, `NoMatchingPhysicalHost`) or for bootstrap data (`WaitingForBootstrapData`), while its host is inspected and deployed (`PhysicalHostNotReady`), and while it waits out a BMC outage (`WaitingForBMC`). A terminal failure is `False` too. Cluster API sets the `Machine`'s condition `False` as soon as the `Machine` exists, so this clock runs from the `Machine`'s creation through all of provisioning.
- **`nodeStartupTimeoutSeconds`** replaces a `Machine` that still has no Node after the timeout, counted from the latest of the `Machine`'s creation, the control plane's initialisation and the `Machine`'s `InfrastructureReady` turning `True`. Until the host is provisioned that is the `Machine`'s creation, or the control plane's initialisation for a worker created together with its cluster, so this clock runs during provisioning as well, not only after it. Cluster API defaults it to 600 seconds, which replaces machines that are still deploying.

Beskar7 bounds its own part: inspection may take `--inspection-timeout` (10 minutes by default) and deployment `--deployment-timeout` (20 minutes), after which the machine fails terminally. Nothing bounds a wait for a free host, for bootstrap data, or for a BMC to come back. The recommended values follow from that:

| Check | Recommended | Why |
|---|---|---|
| `nodeStartupTimeoutSeconds` | `2700` (45 min) | `--inspection-timeout` + `--deployment-timeout` + 15 minutes for waiting before inspection and, once the host is provisioned, for the Node to register (2–3 minutes observed on real hardware). |
| `unhealthyMachineConditions`: `InfrastructureReady` `False` | `5400` (90 min) | Twice the above. This clock starts at the `Machine`'s creation, and a worker created together with its cluster waits for its bootstrap data through the control plane's own provisioning run before its own begins. |

Raise both whenever you raise either flag. What they mean in practice:

- **A machine that fails while provisioning** never gets a Node, so `nodeStartupTimeoutSeconds` replaces it 45 minutes after it could start, however early it failed. The check cannot tell a failure from a slow run any sooner.
- **A machine that fails after its Node joined** — for instance a provisioned host whose BMC credentials, certificate or address stop working, which ends in `PhysicalHostError` — is left to the `InfrastructureReady` check, 90 minutes later. Its Node may still be serving, but a failed machine never recovers, so the check replaces it anyway. Leave the check out, or annotate the `Machine` with `cluster.x-k8s.io/skip-remediation`, if you would rather make that call by hand.
- **A wait that outlasts the timeouts** gets the machine replaced. Before its host is provisioned that costs the hardware nothing — a machine waiting for a host holds none, one waiting for its BMC has not started inspection, and a host part-way through deployment was being overwritten anyway — but the replacement starts over. A pool with more replicas than free hosts is replaced over and over; size it to the inventory. Once more than 40% of the pool is unhealthy, the example's `triggerIf` holds back remediation for the whole pool.
- **`timeoutSeconds: 0`** on the `InfrastructureReady` check replaces every machine the first time the check sees it, before it can finish provisioning.

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
[`examples/kairos-providerid-stage.yaml`](../examples/kairos-providerid-stage.yaml) for k3s,
[`examples/kairos-k0s-providerid-stage.yaml`](../examples/kairos-k0s-providerid-stage.yaml) for k0s.
[Building a target image](building-images.md) shows how to bake them into `COS_OEM`.

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

**k0s** (proven path — [`examples/kairos-k0s-providerid-stage.yaml`](../examples/kairos-k0s-providerid-stage.yaml)):
there is no working kubelet-flag route on k0s. The Kairos k0s provider drops `--kubelet-extra-args`
when it turns `k0s.args` into the systemd override, so the k0s stage patches `Node.spec.providerID`
from `/oem/beskar7/provider-id` right after the node registers — permitted, because immutability
only guards a non-empty value. A k0s image also needs
[`examples/kairos-k0s-start-gate.yaml`](../examples/kairos-k0s-start-gate.yaml): not for the
ProviderID, but because without it a k0s control plane does not form on beskar7 at all
([Building a target image → k0s: the start gate](building-images.md#k0s-the-start-gate)). Verified
on Kairos v4.1.2 + k0s v1.34.8+k0s.0 with cluster-api-provider-kairos: all four nodes registered
`b7://<namespace>/<host>` and their Machines reached `Running`.

**Plain kubelet / other distros:** set the kubelet `--provider-id` flag to the same value via your
distro's kubelet-args mechanism, before the kubelet first registers.

If you skip this, the node still comes up and is `Ready`, but the CAPI `Machine` stays at `Provisioned` and never reaches `Running` (the Node is never bound). See [Troubleshooting → CAPI Machine stuck at Provisioned](troubleshooting.md#12-capi-machine-stuck-at-provisioned-never-reaches-running-node-not-associated).

> **Scaled deployments:** hand-authoring the value works when you author the per-machine bootstrap config and therefore know which PhysicalHost the Machine will use (a single node, or hosts pinned to specific machines). A templated `MachineDeployment` or a multi-replica control plane cannot pin a per-host ProviderID in one shared template — that is what the image-side stages above are for.

## Example

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
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
