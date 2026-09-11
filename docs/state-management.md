# State Management

> **Audience:** Operators

This page describes the lifecycle of a `PhysicalHost` — the states it moves through, what triggers each transition, and how to recover when it gets stuck.

The state constants are defined in `api/v1beta2/physicalhost_types.go`. The transitions are driven by `controllers/physicalhost_controller.go` and the `Beskar7Machine` reconciler in `controllers/beskar7machine_controller.go`.

## States

| Constant | Status string | Meaning |
|---|---|---|
| `StateNone` | `""` | Initial state before the first reconcile. |
| `StateUnknown` | `"Unknown"` | The reconciler could not determine state (rare; transient). |
| `StateEnrolling` | `"Enrolling"` | The reconciler is establishing the BMC connection for the first time. |
| `StateAvailable` | `"Available"` | BMC reachable; no consumer claim. Eligible for a Beskar7Machine to claim. |
| `StateInUse` | `"InUse"` | A Beskar7Machine has claimed the host (`Spec.ConsumerRef` is set). |
| `StateInspecting` | `"Inspecting"` | The inspection image is booting / running on the host. |
| `StateDeploying` | `"Deploying"` | Hardware inspection passed and the inspector is writing the OS image to disk. Entered when the inspection report is accepted; left through the inspector's provisioned or provision-failed callback. The deployment timeout fails only the Beskar7Machine and does not signal the host. |
| `StateReady` | `"Ready"` | OS deployment complete (the inspector's provisioned callback was received). `Beskar7Machine.Spec.ProviderID`, `Status.Ready`, and `Status.Initialization.Provisioned` are set at this point. |
| `StateError` | `"Error"` | A terminal-or-recoverable error condition. See `Status.ErrorMessage`. An unreachable BMC (`RedfishConnectionReady` reason `BMCUnreachable`) is the kind that clears by itself. A failed provisioning run — the inspector's `/provision-failed` report or an inspection timeout — is the kind that does not: a claimed host keeps that `Error` until it is released. |

The legacy `Claimed`, `Provisioning`, `Provisioned`, `Deprovisioning` strings from v0.3 are gone. The Go code previously kept deprecated alias constants (`StateClaimed = "InUse"`, etc.); those aliases have been removed. Code that imported them must switch to the canonical state constants above.

## State diagram

```
            (object created)
                  │
                  ▼
              Available  ◀───────────────────────────────────────┐
                  │                                              │
   Beskar7Machine │ sets spec.consumerRef                        │ Beskar7Machine
   (atomic claim) │                                              │ deletion releases
                  ▼                                              │ ConsumerRef
                InUse                                            │
                  │                                              │
   Beskar7Machine │ patches inspection-request="inspect"         │
                  ▼                                              │
              Inspecting ── inspection timeout ──► Error ¹       │
                  │                                              │
   PhysicalHost   │ inspection-result ConfigMap consumed,        │
   controller     │ report validated (inspection-complete signal)│
                  ▼                                              │
              Deploying  ── POST /provision-failed ² ─► Error ¹  │
                  │                                              │
   inspector      │ POSTs /api/v1/provisioned (202)              │
                  ▼                                              │
                Ready  ───────────────────────────────────────► (deletion)

              ┌───────────────────────────┐
              │   Error  (any state)      │  recovers when the underlying
              └───────────────────────────┘  problem clears (e.g. BMC reachable)

   ¹ The run failed. A claimed host keeps this Error, through any later BMC
     failure, until Beskar7Machine deletion releases it.
   ² Applied before the host tries its BMC, so a BMC failure cannot get in
     first. A report that arrives while the host is still Inspecting is kept
     until the host is Deploying.
```

## What drives each transition

| From | To | Trigger | Where |
|---|---|---|---|
| (no state) | `Available` | First successful Redfish reconcile, no `ConsumerRef`. | `physicalhost_controller.go:reconcileNormal` |
| `Available` | `InUse` | A Beskar7Machine writes `Spec.ConsumerRef`. | `beskar7machine_controller.go:findAndClaimOrGetAssociatedHost` |
| `InUse` | `Inspecting` | Beskar7Machine writes `inspection-request: inspect` annotation. | `beskar7machine_controller.go:triggerInspection` |
| `Inspecting` | `Deploying` | PhysicalHost controller consumes the inspection-result ConfigMap, validates the report, and advances the phase. Beskar7Machine then drives the state forward via `inspection-request: inspect-complete`. | `physicalhost_controller.go:applyInspectionResultAnnotation`, `beskar7machine_controller.go:validateInspectionReport` |
| `Inspecting` | `Error` | Inspection timeout (`--inspection-timeout`, default 10 min) — Beskar7Machine writes `inspection-request: timeout`, and the host records `Inspection timed out`. The run has failed: the host keeps this `Error` until it is released. | `beskar7machine_controller.go:handleInspectingHost`, `physicalhost_controller.go:applyInspectionRequest` |
| `Deploying` | `Ready` | Inspector POSTs `POST /api/v1/provisioned/{ns}/{host}` (§4.4 of `docs/inspector-contract.md`). The provisioned handler patches `ProvisionedRequestAnnotation`; the PhysicalHost reconciler transitions to `Ready` and clears the annotation. | `controllers/provisioned_handler.go`, `physicalhost_controller.go` |
| `Deploying` | `Error` | Inspector POSTs `POST /api/v1/provision-failed/{ns}/{host}` (§4.5 of `docs/inspector-contract.md`). The provision-failed handler patches `ProvisionFailedRequestAnnotation`; the PhysicalHost reconciler transitions to `Error` with the inspector's sanitized reason in `Status.ErrorMessage` and clears the annotation. It does so before it tries the BMC, so the report lands during an outage or a BMC failure that needs a fix, and the machine fails with `DeploymentFailed` rather than a BMC reason. The run has failed: the host keeps this `Error` until it is released. | `controllers/provision_failed_handler.go`, `physicalhost_controller.go:applyProvisionFailedRequestAnnotation` |
| `Inspecting` (claimed) | unchanged, then `Error` | `/provision-failed` arrives before the host is `Deploying`: the inspector starts Phase 2 as soon as it has posted its inspection report (§9.2), and a fast failure — a target image URL that answers 404 — reports before the `Beskar7Machine` has validated the inspection report. The host keeps the annotation and applies it once `inspect-complete` has moved it to `Deploying`. It clears the annotation without a transition if the host goes anywhere else — the machine rejects the hardware (`HardwareRequirementsNotMet`), the host is released — or if the report came before the run's inspection report. | `physicalhost_controller.go:applyProvisionFailedRequestAnnotation` |
| `Error` about the BMC, during deployment (claimed) | `Error` of the failed run | `/provision-failed` arrives after a BMC failure that needs a fix (credentials, certificate, TLS config, no `ComputerSystem`) has written its `Error` over `Deploying`. The inspector does not need the BMC, so the report still applies; the host takes the inspector's reason and keeps it until it is released. | `physicalhost_controller.go:deployInterruptedByBMCError` |
| `Deploying` | unchanged | Deployment timeout (`--deployment-timeout`, default 20 min, measured from `Status.DeployingTimestamp`). Only the Beskar7Machine fails (`DeploymentTimedOut`); the timeout does not signal the host. | `beskar7machine_controller.go:handleDeployingHost` |
| any | `Error` | Credentials missing, TLS-config conflict, Redfish connection or query failed — and an unreachable BMC, except on the claimed states in the next two rows. | `physicalhost_controller.go:reconcileNormal` |
| `Inspecting` / `Deploying` / `Ready` (claimed) | unchanged | BMC unreachable. The host keeps its state — the inspector and the installed OS do not need the BMC — and `RedfishConnectionReady=False (BMCUnreachable)` reports the outage until the BMC answers. | `physicalhost_controller.go:retryTransientRedfishFailure` |
| `Error` from a failed run (claimed) | unchanged | Any BMC failure, and the BMC's recovery. `RedfishConnectionReady` reports the BMC; `Status.ErrorMessage` keeps the run's reason. An `inspection-request` annotation is consumed without effect — the `Beskar7Machine` can send `inspect-complete` a second time after reading a copy of the host from before its first request was applied. | `physicalhost_controller.go:provisioningRunFailed`, `physicalhost_controller.go:applyInspectionRequest` |
| `Error` | `Available`, or `InUse` if claimed | The underlying error clears (BMC reachable again, secret fixed, TLS config fixed). Not the `Error` of a failed run, which a claimed host keeps until it is released. | `physicalhost_controller.go:reconcileNormal` |
| `InUse` / `Inspecting` / `Deploying` / `Ready` / `Error` | `Available` | Beskar7Machine deletion clears `Spec.ConsumerRef`. | `beskar7machine_controller.go:reconcileDelete` |

## Atomic claim

Two Beskar7Machines can race to claim the same `Available` host. The race is resolved server-side:

1. The reconciler lists `PhysicalHost` filtered by the `status.state` field index for `Available`.
2. It selects the first host with `ConsumerRef == nil`.
3. It patches the host with `MergeFromWithOptions(base, MergeFromWithOptimisticLock{})`. The optimistic-locking option means the patch carries the host's current `resourceVersion`, so a concurrent claim from another reconciler fails fast with `409 Conflict`.
4. The losing reconciler gets the conflict, requeues, re-lists, and either picks a different host or returns empty.

This means exactly one Beskar7Machine succeeds per host. There is no separate state for "claim in flight" — the claim is the patch.

## Bootstrap and inspection signaling

The `Beskar7Machine` reconciler signals work to the host through annotations on `Spec` (never via `Status`); the `PhysicalHost` reconciler is the sole writer of the host's status. This pattern (decision D-005 in `.claude/context/PROJECT_CONTEXT.md`) ensures every controller owns its own resource's status.

Annotations consumed by the `PhysicalHost` reconciler:

| Annotation | Producer | Consumer action |
|---|---|---|
| `infrastructure.cluster.x-k8s.io/inspection-request` | `Beskar7Machine` controller | Drive `Status.State` and `Status.InspectionPhase`. Values: `inspect`, `inspect-complete`, `timeout`. |
| `infrastructure.cluster.x-k8s.io/bootstrap-url` | `Beskar7Machine` controller | Persist URL to `Status.Bootstrap.URL`. |
| `infrastructure.cluster.x-k8s.io/bootstrap-token` | `Beskar7Machine` controller | Persist hash + lifetime to `Status.Bootstrap.{TokenHash,IssuedAt,ExpiresAt}`. |
| `infrastructure.cluster.x-k8s.io/inspection-result-ref` | Inspection HTTP handler | Read referenced ConfigMap, persist the `InspectionReport`, mark `HostInspected=True`, delete the ConfigMap. |
| `infrastructure.cluster.x-k8s.io/provisioned-request` | `ProvisionedHandler` (HTTP) | Transition `Status.State` from `Deploying` to `Ready` (D-015). Value: `"provisioned"`. Clear after action. |
| `infrastructure.cluster.x-k8s.io/provision-failed-request` | `ProvisionFailedHandler` (HTTP) | Transition `Status.State` to `Error` and persist the value — the sanitized reason, prefixed `inspector reported deploy failure: ` — to `Status.ErrorMessage` (contract v4.1), from `Deploying` or from an `Error` a BMC failure wrote over it. Applied before the BMC is tried. On a claimed host still `Inspecting`, left in place until the host is `Deploying`. Clear after action, or without one when the host cannot be failed by it. |

## Recovery

### Stuck in `Enrolling`

The reconciler is unable to complete the first BMC handshake.

```bash
kubectl describe physicalhost <name>
```

Look at the `RedfishConnectionReady` condition reason — it is one of `BMCUnreachable`, `MissingCredentials`, `SecretNotFound`, `MissingSecretData`, `RedfishConnectionFailed`, or `RedfishQueryFailed`. Fix the credentials Secret or the BMC address; the next reconcile transitions to `Available`.

Two retry cadences sit behind that. A host whose BMC is simply unreachable — reason `BMCUnreachable`, message `BMC unreachable (connection refused)` or similar, covering a refused or reset connection, no route, a DNS failure, a timeout, or a 502/503/504 from a BMC that is still booting — is retried every 15 seconds, flat, and enrols on the first attempt after the BMC answers. Nothing has to be fixed for that to clear, and a `Beskar7Machine` holding the host waits for it instead of failing (see [Beskar7Machine → A BMC outage is not a terminal failure](beskar7machine.md#a-bmc-outage-is-not-a-terminal-failure)). Every other Redfish failure needs a change to the spec, the Secret or the BMC itself, so it backs off exponentially (5s, 10s, 20s, … capped at 30 minutes) and a host that has been failing for a while can take a few minutes to notice the fix. Editing the `PhysicalHost` or its credentials Secret wakes the controller immediately.

### Stuck in `Inspecting`

The host booted but the inspection image never POSTed a report. Check:

```bash
kubectl get physicalhost <name> -o jsonpath='{.status.inspectionPhase}'
kubectl get physicalhost <name> -o jsonpath='{.status.inspectionTimestamp}'
```

If `inspectionTimestamp` is more than 10 minutes ago, the Beskar7Machine controller will mark `InspectionTimedOut` (terminal) and write `inspection-request: timeout`, which moves the host to `Error` for as long as it stays claimed. To recover, fix the underlying iPXE / inspection-image / network issue, then delete-and-recreate the `Beskar7Machine` (which deletes the BMC token Secret, clears `ConsumerRef`, and lets the host return to `Available`).

### Stuck in `Deploying`

The host entered `Deploying` (inspection passed and the inspector is writing the OS image) but the provisioned callback never arrived. Check `Status.DeployingTimestamp`:

```bash
kubectl get physicalhost <name> -o jsonpath='{.status.deployingTimestamp}'
```

If the timestamp is older than the `--deployment-timeout` (default 20 min), the Beskar7Machine controller marks the `Beskar7Machine` terminally failed: `status.phase=Failed` and `InfrastructureReady=False` with reason `DeploymentTimedOut`. The timeout does not signal the host. An inspector that can still report a failure calls `/provision-failed` instead, which moves the host to `Error` at once, whether or not its BMC is reachable (see [Stuck in `Error`](#stuck-in-error)). Common causes of a timeout:

- The OS image download is slow or stalled — check network reachability from the host to `Beskar7Machine.Spec.TargetImageURL`.
- The inspector's TLS verification failed for the provisioned-callback endpoint — check that `beskar7.api` is externally reachable and that the certificate uses a two-tier PKI (CA cert distinct from the server cert; see the TLS note in `docs/inspector-contract.md` §8).
- The inspector aborted after the disk write (e.g. digest mismatch, `COS_OEM` mount failure) — check the host's serial console or BMC event log.

To recover, delete and recreate the `Beskar7Machine`. This clears `ConsumerRef`, releases the host to `Available`, and starts a fresh provision cycle with new nonce and token.

### Stuck in `Error`

`Status.ErrorMessage` is the source of truth. Common cases:

- `redfishConnection.insecureSkipVerify=true is mutually exclusive with caBundleSecretRef`: edit the spec — pick one. The reconciler resumes once the spec is valid.
- `failed to get credentials secret`: the named Secret does not exist or is missing `username`/`password`. Create or fix it.
- `BMC unreachable (…); retrying every 15s`: nothing to fix on the object. The host retries and leaves `Error` on the first attempt that connects; check the network path from the controller to the BMC if it does not. A `Beskar7Machine` holding the host reports `InfrastructureReady=False (WaitingForBMC)` meanwhile and carries on afterwards — it is not failed and does not need replacing.
- `inspector reported deploy failure: …`: the inspector's `/provision-failed` report, with its reason after the colon (`no details provided` when it sent none). The run failed, and the `Beskar7Machine` is failed with `DeploymentFailed`, quoting the message. The host keeps this `Error` while it is claimed — a later BMC failure or recovery changes only `RedfishConnectionReady` — and returns to `Available` once the `Beskar7Machine` is deleted and releases it.
- `Inspection timed out`: see above. Kept the same way until the host is released.

### Force release

If the BMC is permanently unreachable and you need to delete the consuming Beskar7Machine without waiting for the Redfish power-off / boot-clear, set the force-release annotation on the Beskar7Machine before deleting it:

```bash
kubectl annotate beskar7machine <name> \
  infrastructure.cluster.x-k8s.io/force-release=true
kubectl delete beskar7machine <name>
```

The Beskar7Machine controller skips the Redfish cleanup, clears `ConsumerRef`, and removes the finalizer. The host returns to `Available`.

### Last-resort finalizer removal

If a finalizer is genuinely stuck (and only after exhausting the recovery paths above), you can remove it manually. This is destructive — it leaves Redfish state untouched.

```bash
kubectl patch physicalhost <name> --type=merge -p '{"metadata":{"finalizers":[]}}'
```

## Observability

### Conditions

`kubectl describe physicalhost <name>` shows the conditions list — native `metav1.Condition`, no `severity` field, every condition (including `True`) carries a `reason`. Key types:

- `RedfishConnectionReady` — BMC connectivity. True reason `RedfishConnected`; `False (BMCUnreachable)` while the BMC cannot be reached, the one `False` reason that clears by itself.
- `HostAvailable` — no consumer holds the host. True reason `HostAvailable`; `False (HostClaimed)` while `spec.consumerRef` is set.
- `HostInspected` — inspection report has been persisted. True reason `HostInspected`; `False (HostReleased)` when a host returns to `Available` after a run.

Full reason lists: [PhysicalHost → Conditions](physicalhost.md#conditions).

### Events

```bash
kubectl get events --field-selector involvedObject.kind=PhysicalHost
```

The controller emits events for major transitions and for warnings like deleting a still-claimed host.

### Metrics

See [Metrics](metrics.md) for the Prometheus surface. The relevant metric for state observation is `beskar7_controller_physicalhost_states_total{state=...}`.

## See also

- [PhysicalHost](physicalhost.md)
- [Beskar7Machine](beskar7machine.md)
- [Architecture](architecture.md)
- [Troubleshooting](troubleshooting.md)
