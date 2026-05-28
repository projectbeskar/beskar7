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
  inspectionImageURL: "http://boot-server.local/ipxe/inspect.ipxe"
  targetImageURL:     "http://boot-server.local/images/kairos-alpine-v2.8.1.tar.gz"
  configurationURL:   "http://boot-server.local/configs/worker.yaml"   # optional
  hardwareRequirements:                                                 # optional
    minCPUCores: 4
    minMemoryGB: 8
    minDiskGB:   50
  # providerID is set by the controller on claim — do not set manually
```

All three URL fields must match `^https?://.*`. There is no `osFamily`, `imageURL`, `bootMode`, `provisioningMode`, or `configURL` — those v0.3 fields were removed in v0.4.

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
| `InfrastructureReady` | Standard CAPI infra-ready condition. Summary across the others. | – |
| `PhysicalHostAssociated` | A host has been claimed. | `WaitingForPhysicalHost`, `PhysicalHostAssociationFailed`. |
| `MachineProvisioned` | Host is `Ready` and `ProviderID` is set. | – |
| `BootstrapDataReady` | `Machine.Spec.Bootstrap.DataSecretName` is set and the URL has been signalled. | `WaitingForBootstrapData`, `BootstrapDataUnavailable`. |

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

The provider ID is `b7://<namespace>/<name>`. The parser uses `strings.CutPrefix` + `strings.SplitN(rest, "/", 2)` and rejects empty segments and multi-segment names. See `controllers/beskar7machine_controller.go:parseProviderID`.

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
  inspectionImageURL: "http://boot-server.local/ipxe/inspect.ipxe"
  targetImageURL:     "http://boot-server.local/images/kairos-alpine-v2.8.1.tar.gz"
  configurationURL:   "http://boot-server.local/configs/control-plane.yaml"
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
