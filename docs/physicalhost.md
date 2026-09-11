# PhysicalHost

> **Audience:** Operators

`PhysicalHost` is the Beskar7 CRD that represents one bare-metal server and its BMC connection. The reconciler that owns it is `controllers/physicalhost_controller.go`.

For the full field reference, see [API Reference: PhysicalHost](api-reference.md#physicalhost). This page covers operational behavior — when fields move, what conditions mean, how the lifecycle works.

## Identity

- **API:** `infrastructure.cluster.x-k8s.io/v1beta2`
- **Kind:** `PhysicalHost`
- **Short name:** `ph`
- **Scope:** Namespaced
- **Categories:** `cluster-api` (`kubectl get cluster-api` lists hosts)
- **clusterctl:** the CRD carries `clusterctl.cluster.x-k8s.io`, so `clusterctl move` discovers hosts, and `clusterctl.cluster.x-k8s.io/move-hierarchy`, so every host in the namespace is moved together with the Secret and ConfigMap it owns — nothing owns a host, so without it a move would leave all hosts behind. The BMC credentials Secret is yours, not the host's: label it `clusterctl.cluster.x-k8s.io/move=""` or create it on the target first (see [Installation](installation.md#clusterctl-move)).

## Spec at a glance

```yaml
spec:
  redfishConnection:
    address: "https://192.168.1.100"
    credentialsSecretRef: "bmc-credentials"
    insecureSkipVerify: false        # default
    # caBundleSecretRef: bmc-ca-bundle  # optional, mutually exclusive with insecureSkipVerify=true
  # consumerRef is set by the Beskar7Machine controller; do not set manually
```

The credentials Secret must contain `username` and `password` keys (Opaque). It must live in the same namespace as the PhysicalHost.

When `caBundleSecretRef` is set, the manager builds an HTTP client whose TLS roots include the CA bundle. The Secret data must contain a `ca.crt` (preferred) or `tls.crt` key with PEM bytes. Setting `caBundleSecretRef` together with `insecureSkipVerify: true` is rejected — the controller marks `RedfishConnectionReady = False (InsecureCABundleConflict)` and stops reconciling until the operator fixes the spec.

## Lifecycle

The reconciler drives `Status.State` through these transitions:

```
created → Available                            (BMC reachable, no consumer)
Available → InUse                              (Beskar7Machine claims via spec.consumerRef)
InUse → Inspecting                             (Beskar7Machine sets the inspection-request annotation)
Inspecting → Deploying                         (inspection report consumed from ConfigMap, validated)
Deploying → Ready                              (inspector POSTs /api/v1/provisioned; see contract §4.4)
Deploying → Error                              (deployment timeout, default 20 min)
any → Error                                    (BMC unreachable, TLS conflict, inspection timeout)
Inspecting/Deploying/Ready → unchanged         (claimed, BMC unreachable: only RedfishConnectionReady reports it)
Error → Available, or InUse if claimed         (operator fixes spec, BMC recovers)
InUse/Inspecting/Deploying/Ready → Available   (Beskar7Machine deletion clears consumerRef)
```

For the diagram and the full transition table, see [State Management](state-management.md).

## Bootstrap signaling

When the Beskar7Machine controller has bootstrap data ready, it patches two annotations on the PhysicalHost. The PhysicalHost reconciler reads them on its next pass, persists the values to status, and clears the annotation. For the credential annotations (`bootstrap-token`, and the `boot-nonce` minted at inspection time) the clear happens one pass later, once status already shows the same hash: the reconciler's patch writes metadata before status, so clearing in the same pass would publish a version of the host that advertises no credential, and a reader in that gap (the Beskar7Machine controller checks the annotation, then status) would mint a fresh one over a token the inspector may already hold:

| Annotation | Persisted to | Source code |
|---|---|---|
| `infrastructure.cluster.x-k8s.io/bootstrap-url` | `Status.Bootstrap.URL` | `controllers/physicalhost_controller.go:applyBootstrapURLAnnotation` |
| `infrastructure.cluster.x-k8s.io/bootstrap-token` | `Status.Bootstrap.{TokenHash, IssuedAt, ExpiresAt}` | `controllers/physicalhost_controller.go:applyBootstrapTokenAnnotation` |

The plaintext bearer token is delivered out-of-band via a Secret named `<host-name>-bootstrap-token`, owned by the PhysicalHost (so it is GC'd when the host is deleted). The Secret has a single key: `plaintext-token`.

## Inspection result handoff

The inspection HTTP handler does not write to `PhysicalHost.Status` directly. Instead, it stores the validated `InspectionReport` on a ConfigMap named `<host>-inspection-result` (owner-ref → PhysicalHost) and patches an `infrastructure.cluster.x-k8s.io/inspection-result-ref` annotation onto the host. The reconciler consumes the ConfigMap, writes the report to `Status.InspectionReport`, marks `HostInspected=True`, deletes the ConfigMap, and clears the annotation. This keeps the controller as the sole writer of the host's status (decision D-005 in `.claude/context/PROJECT_CONTEXT.md`).

## Conditions

Native `metav1.Condition` (`status.conditions[]`) — no `severity` field, and a `True` condition carries a `reason` too. `PhysicalHost` is not a CAPI contract resource: it has no `Ready` summary condition (only these three) and no `Paused` condition — its pause check (`controllers/utils.go:isPaused`) is the `cluster.x-k8s.io/paused` annotation on the host itself; a `PhysicalHost` has no owning `Cluster` to read `spec.paused` from. See [API Reference → Conditions and the CAPI mirror](api-reference.md#conditions-and-the-capi-mirror).

| Type | Meaning | True reason | Other reasons |
|---|---|---|---|
| `RedfishConnectionReady` | BMC reachable and authenticating successfully. | `RedfishConnected` | `BMCUnreachable` (the BMC cannot be reached at the network level; retried every 15 s and clears by itself, so a `Beskar7Machine` holding the host waits for it), `MissingCredentials`, `SecretGetFailed`, `SecretNotFound`, `MissingSecretData`, `RedfishConnectionFailed`, `RedfishQueryFailed`, `InsecureCABundleConflict`, `CABundleFetchFailed`. |
| `HostAvailable` | No consumer holds the host (`spec.consumerRef` is unset). Follows the claim only — BMC health is `RedfishConnectionReady`. | `HostAvailable` | `HostClaimed` (a consumer holds the host; back to `True` once the claim is released). |
| `HostInspected` | An inspection report has been persisted. | `HostInspected` | `HostReleased` (host went back to `Available`; the prior run's inspection no longer describes it). |

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
apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
kind: PhysicalHost
metadata:
  name: server-01
  namespace: default
  labels:
    topology.kubernetes.io/zone: rack-1   # published by Beskar7Cluster as a failure domain; honoured by Beskar7Machine when claiming
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
