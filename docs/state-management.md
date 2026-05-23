# State Management

> **Audience:** Operators

This page describes the lifecycle of a `PhysicalHost` — the states it moves through, what triggers each transition, and how to recover when it gets stuck.

The state constants are defined in `api/v1beta1/physicalhost_types.go:10-26`. The transitions are driven by `controllers/physicalhost_controller.go` and the `Beskar7Machine` reconciler in `controllers/beskar7machine_controller.go`.

## States

| Constant | Status string | Meaning |
|---|---|---|
| `StateNone` | `""` | Initial state before the first reconcile. |
| `StateUnknown` | `"Unknown"` | The reconciler could not determine state (rare; transient). |
| `StateEnrolling` | `"Enrolling"` | The reconciler is establishing the BMC connection for the first time. |
| `StateAvailable` | `"Available"` | BMC reachable; no consumer claim. Eligible for a Beskar7Machine to claim. |
| `StateInUse` | `"InUse"` | A Beskar7Machine has claimed the host (`Spec.ConsumerRef` is set). |
| `StateInspecting` | `"Inspecting"` | The inspection image is booting / running on the host. |
| `StateReady` | `"Ready"` | The inspection report has been validated and persisted; the host is ready for the target OS. |
| `StateError` | `"Error"` | A terminal-or-recoverable error condition. See `Status.ErrorMessage`. |

The legacy `Claimed`, `Provisioning`, `Provisioned`, `Deprovisioning` strings from v0.3 are gone. The Go code retains deprecated aliases (`StateClaimed = "InUse"`, etc.) for backward compatibility, but they all map to the new state strings — do not rely on the old strings appearing in `kubectl get`.

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
              Inspecting                                         │
                  │                                              │
   Beskar7Machine │ patches inspection-request="inspect-complete"│
                  │ after validating the report                  │
                  ▼                                              │
                Ready  ───────────────────────────────────────► (deletion)
                  
              ┌───────────────────────────┐
              │   Error  (any state)      │  recovers when the underlying
              └───────────────────────────┘  problem clears (e.g. BMC reachable)
```

## What drives each transition

| From | To | Trigger | Where |
|---|---|---|---|
| (no state) | `Available` | First successful Redfish reconcile, no `ConsumerRef`. | `physicalhost_controller.go:reconcileNormal` |
| `Available` | `InUse` | A Beskar7Machine writes `Spec.ConsumerRef`. | `beskar7machine_controller.go:findAndClaimOrGetAssociatedHost` |
| `InUse` | `Inspecting` | Beskar7Machine writes `inspection-request: inspect` annotation. | `beskar7machine_controller.go:triggerInspection` |
| `Inspecting` | `Ready` | Beskar7Machine writes `inspection-request: inspect-complete` after hardware validation, OR the inspection-result ConfigMap is consumed and the report is good. | `beskar7machine_controller.go:validateInspectionReport`, `physicalhost_controller.go:applyInspectionResultAnnotation` |
| `Inspecting` | `Error` | Inspection timeout (10 min default) — Beskar7Machine writes `inspection-request: timeout`. | `beskar7machine_controller.go:handleInspectingHost` |
| any | `Error` | BMC unreachable, credentials missing, TLS-config conflict, Redfish query failed. | `physicalhost_controller.go:reconcileNormal` |
| `Error` | `Available` | The underlying error clears (BMC reachable again, secret fixed, TLS config fixed). | `physicalhost_controller.go:reconcileNormal` |
| `InUse` / `Inspecting` / `Ready` | `Available` | Beskar7Machine deletion clears `Spec.ConsumerRef`. | `beskar7machine_controller.go:reconcileDelete` |

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

## Recovery

### Stuck in `Enrolling`

The reconciler is unable to complete the first BMC handshake.

```bash
kubectl describe physicalhost <name>
```

Look at the `RedfishConnectionReady` condition reason — it is one of `MissingCredentials`, `SecretNotFound`, `MissingSecretData`, `RedfishConnectionFailed`, or `RedfishQueryFailed`. Fix the credentials Secret or the BMC address; the next reconcile transitions to `Available`.

### Stuck in `Inspecting`

The host booted but the inspection image never POSTed a report. Check:

```bash
kubectl get physicalhost <name> -o jsonpath='{.status.inspectionPhase}'
kubectl get physicalhost <name> -o jsonpath='{.status.inspectionTimestamp}'
```

If `inspectionTimestamp` is more than 10 minutes ago, the Beskar7Machine controller will mark `InspectionTimedOut` (terminal) and write `inspection-request: timeout`, which moves the host to `Error`. To recover, fix the underlying iPXE / inspection-image / network issue, then delete-and-recreate the `Beskar7Machine` (which deletes the BMC token Secret, clears `ConsumerRef`, and lets the host return to `Available`).

### Stuck in `Error`

`Status.ErrorMessage` is the source of truth. Common cases:

- `redfishConnection.insecureSkipVerify=true is mutually exclusive with caBundleSecretRef`: edit the spec — pick one. The reconciler resumes once the spec is valid.
- `failed to get credentials secret`: the named Secret does not exist or is missing `username`/`password`. Create or fix it.
- `Inspection timed out`: see above.

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

`kubectl describe physicalhost <name>` shows the conditions list. Key types:

- `RedfishConnectionReady` — BMC connectivity.
- `HostAvailable` — host has no consumer.
- `HostInspected` — inspection report has been persisted.

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
