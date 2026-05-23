# PhysicalHost

> **Audience:** Operators

`PhysicalHost` is the Beskar7 CRD that represents one bare-metal server and its BMC connection. The reconciler that owns it is `controllers/physicalhost_controller.go`.

For the full field reference, see [API Reference: PhysicalHost](api-reference.md#physicalhost). This page covers operational behavior — when fields move, what conditions mean, how the lifecycle works.

## Identity

- **API:** `infrastructure.cluster.x-k8s.io/v1beta1`
- **Kind:** `PhysicalHost`
- **Short name:** `ph`
- **Scope:** Namespaced
- **Categories:** `cluster-api` (so `clusterctl move` walks it)

## Spec at a glance

```yaml
spec:
  redfishConnection:
    address: "https://192.168.1.100"
    credentialsSecretRef: "bmc-credentials"
    insecureSkipVerify: false        # default
    # caBundleSecretRef:             # optional, mutually exclusive with insecureSkipVerify=true
    #   name: bmc-ca-bundle
  # consumerRef is set by the Beskar7Machine controller; do not set manually
```

The credentials Secret must contain `username` and `password` keys (Opaque). It must live in the same namespace as the PhysicalHost.

When `caBundleSecretRef` is set, the manager builds an HTTP client whose TLS roots include the CA bundle. The Secret data must contain a `ca.crt` (preferred) or `tls.crt` key with PEM bytes. Setting `caBundleSecretRef` together with `insecureSkipVerify: true` is rejected — the controller marks `RedfishConnectionReady = False (InsecureCABundleConflict)` and stops reconciling until the operator fixes the spec.

## Lifecycle

The reconciler drives `Status.State` through these transitions:

```
created → Available           (BMC reachable, no consumer)
Available → InUse             (Beskar7Machine claims via spec.consumerRef)
InUse → Inspecting            (Beskar7Machine sets the inspection-request annotation)
Inspecting → Ready            (inspection report consumed from ConfigMap)
Ready → InUse                 (back to InUse after the host stays claimed)
any → Error                   (BMC unreachable, TLS conflict, inspection timeout)
Error → Available             (operator fixes spec, BMC recovers)
Ready/InUse → Available       (Beskar7Machine deletion clears consumerRef)
```

For the diagram and the full transition table, see [State Management](state-management.md).

## Bootstrap signaling

When the Beskar7Machine controller has bootstrap data ready, it patches two annotations on the PhysicalHost. The PhysicalHost reconciler reads them on its next pass, persists the values to status, and clears the annotation:

| Annotation | Persisted to | Source code |
|---|---|---|
| `infrastructure.cluster.x-k8s.io/bootstrap-url` | `Status.Bootstrap.URL` | `controllers/physicalhost_controller.go:applyBootstrapURLAnnotation` |
| `infrastructure.cluster.x-k8s.io/bootstrap-token` | `Status.Bootstrap.{TokenHash, IssuedAt, ExpiresAt}` | `controllers/physicalhost_controller.go:applyBootstrapTokenAnnotation` |

The plaintext bearer token is delivered out-of-band via a Secret named `<host-name>-bootstrap-token`, owned by the PhysicalHost (so it is GC'd when the host is deleted). The Secret has a single key: `plaintext-token`.

## Inspection result handoff

The inspection HTTP handler does not write to `PhysicalHost.Status` directly. Instead, it stores the validated `InspectionReport` on a ConfigMap named `<host>-inspection-result` (owner-ref → PhysicalHost) and patches an `infrastructure.cluster.x-k8s.io/inspection-result-ref` annotation onto the host. The reconciler consumes the ConfigMap, writes the report to `Status.InspectionReport`, marks `HostInspected=True`, deletes the ConfigMap, and clears the annotation. This keeps the controller as the sole writer of the host's status (decision D-005 in `.claude/context/PROJECT_CONTEXT.md`).

## Conditions

| Type | Meaning | Common reasons |
|---|---|---|
| `RedfishConnectionReady` | BMC reachable and authenticating successfully. | `MissingCredentials`, `SecretGetFailed`, `SecretNotFound`, `MissingSecretData`, `RedfishConnectionFailed`, `RedfishQueryFailed`, `InsecureCABundleConflict`, `CABundleFetchFailed`. |
| `HostAvailable` | Host has no consumer claim. | – |
| `HostInspected` | An inspection report has been persisted. | – |

## Deletion

`reconcileDelete` removes the finalizer and lets Kubernetes garbage-collect the host. The PhysicalHost reconciler does NOT call Redfish during deletion; that is the job of the consuming `Beskar7Machine` (best-effort `ClearBootSourceOverride` + graceful `SetPowerState(Off)` before clearing `ConsumerRef`). If a host is deleted while still claimed, the controller emits a warning event but does not block.

## Operator escape hatches

- **Force release:** Set `infrastructure.cluster.x-k8s.io/force-release: "true"` on the consuming `Beskar7Machine` before deletion. The Beskar7Machine controller will skip the BMC power-off / boot-clear during release. Use only when the BMC is permanently unreachable.

## Example

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: bmc-credentials
  namespace: default
type: Opaque
stringData:
  username: "admin"
  password: "changeme"
---
apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
kind: PhysicalHost
metadata:
  name: server-01
  namespace: default
  labels:
    topology.kubernetes.io/zone: rack-1   # consumed by Beskar7Cluster failure domains
spec:
  redfishConnection:
    address: "https://192.168.1.100"
    credentialsSecretRef: "bmc-credentials"
```

## Print columns

`kubectl get physicalhost` shows `State`, `Ready`, and `Age`.

## See also

- [API Reference: PhysicalHost](api-reference.md#physicalhost)
- [State Management](state-management.md)
- [Architecture](architecture.md)
- [Troubleshooting](troubleshooting.md)
