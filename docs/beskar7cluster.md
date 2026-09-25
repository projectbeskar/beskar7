# Beskar7Cluster

> **Audience:** Operators

`Beskar7Cluster` is the Beskar7 CRD that implements the CAPI infrastructure-cluster contract. The reconciler that owns it is `controllers/beskar7cluster_controller.go`. It is the only Beskar7 resource with an admission webhook (`api/v1beta2/webhooks/beskar7cluster_webhook.go`).

For the full field reference, see [API Reference: Beskar7Cluster](api-reference.md#beskar7cluster). This page covers operational behavior — what the controller derives, when it sets which condition.

## Identity

- **API:** `infrastructure.cluster.x-k8s.io/v1beta2`
- **Kind:** `Beskar7Cluster`
- **Scope:** Namespaced
- **Categories:** `cluster-api`
- **clusterctl:** the CRD carries `clusterctl.cluster.x-k8s.io`, so `clusterctl move` discovers it; it is moved through its owner reference to the CAPI `Cluster` (see [Installation](installation.md#clusterctl-move)).

## Spec

```yaml
spec:
  controlPlaneEndpoint:
    host: "10.0.1.10"   # optional; see "Control-plane endpoint" below
    port: 6443          # required if host is set
```

## What the reconciler does

### Control-plane endpoint

**Beskar7 does not discover a control-plane endpoint.** It only mirrors the endpoint already in effect elsewhere to `Status.ControlPlaneEndpoint`:

1. If `Cluster.spec.controlPlaneEndpoint` is valid (host and port both set), that value is used — this is what a `ClusterClass` topology or a direct edit to the `Cluster` object produces.
2. Otherwise, if this `Beskar7Cluster`'s own `Spec.ControlPlaneEndpoint` has both host and port set, that value is used instead.
3. Otherwise the endpoint is not set. A host with no port does not count as set — `Spec.ControlPlaneEndpoint.Port` is never defaulted by the reconciler.

The controller never writes `Cluster.spec` or `Beskar7Cluster.spec` — an operator or a `ClusterClass` patch supplies the value (see [Using it from a ClusterClass](#using-it-from-a-clusterclass) below, or [`examples/clusterclass.yaml`](../examples/clusterclass.yaml)).

Once the endpoint is populated, the controller sets `Status.Ready=true` AND `Status.Initialization.Provisioned=true` together. The second field is the CAPI v1beta2 contract: CAPI core lifts it into `Cluster.status.initialization.infrastructureProvisioned`, which the KubeadmConfig + Machine controllers gate on. Without it the bootstrap data secret is never generated and downstream Machine reconcile stalls — this was the gap fixed in v0.4.0-alpha.4.

When no endpoint is set anywhere, `ControlPlaneEndpointReady` is `False` with reason `ControlPlaneEndpointNotSet` and a message naming exactly what to set. There is no requeue timer for this case: the reconciler watches `Cluster`, so setting the endpoint on either object wakes it immediately instead of waiting on a poll.

### Using it from a ClusterClass

`Beskar7ClusterTemplate`'s `spec.template.spec` is normally `{}` (see [Beskar7ClusterTemplate](beskar7clustertemplate.md)), so the endpoint has to come from somewhere per-Cluster. `examples/clusterclass.yaml` shows the pattern: a required `controlPlaneEndpoint` `ClusterClass` variable, patched with a JSON Patch into the generated `Beskar7Cluster`'s `spec.controlPlaneEndpoint`, and set per-Cluster under `spec.topology.variables`. The alternative — bypassing Beskar7Cluster's spec entirely — is to set `Cluster.spec.controlPlaneEndpoint` directly on the topology `Cluster`; Beskar7 prefers that value over `Beskar7Cluster`'s when both are present.

### Failure domains

The controller lists `PhysicalHost` resources in the same namespace, extracts unique values from the `topology.kubernetes.io/zone` label, and populates `Status.FailureDomains`:

```yaml
metadata:
  labels:
    topology.kubernetes.io/zone: rack-1
```

CAPI uses these for placement: a `KubeadmControlPlane` or equivalent spreads its Machines across the published domains by setting `Machine.spec.failureDomain`, and the `Beskar7Machine` controller then claims only a `PhysicalHost` carrying that zone label (see [Beskar7Machine → Reconcile flow](beskar7machine.md#reconcile-flow), step 3). A Machine with no failure domain may claim any host. A host with no zone label is never a candidate for a Machine that has one, so label every host in a zoned inventory.

## Conditions

Native `metav1.Condition` (`status.conditions[]`) — no `severity` field, and a `True` condition carries a `reason` too. `Ready` is the summary the controller computes each reconcile from `ControlPlaneEndpointReady` (today its only source condition), and it is what Cluster API mirrors into the owning `Cluster`'s own `InfrastructureReady` condition. `Paused` is maintained by `sigs.k8s.io/cluster-api/util/paused`. See [API Reference → Conditions and the CAPI mirror](api-reference.md#conditions-and-the-capi-mirror).

| Type | Meaning | True reason | False reason |
|---|---|---|---|
| `Ready` | Summary of `ControlPlaneEndpointReady`. | Derived from it. | Same. |
| `ControlPlaneEndpointReady` | The endpoint is populated. | `ControlPlaneEndpointSet` | `ControlPlaneEndpointNotSet`. |
| `Paused` | See [Paused](#paused) below. | `NotPaused` | `Paused`. |

## Paused

`paused.EnsurePausedCondition` runs before every reconcile, including deletion, and pauses when either is true: **`Cluster.spec.paused`** (what `clusterctl move` sets on the source cluster before moving objects), or **the `cluster.x-k8s.io/paused` annotation on this `Beskar7Cluster`**. It does not check an annotation on the `Cluster` object itself. See [Upgrading](upgrading.md).

## Webhook

`Beskar7Cluster` has a validating webhook that checks `controlPlaneEndpoint.host` (IP or hostname) and `port` (1–65535). The webhook ships with `failurePolicy: Fail`, so a Pods/Beskar7Cluster admission attempt without a healthy webhook service is rejected.

There is also a defaulting (mutating) webhook: when `controlPlaneEndpoint.host` is set but `port` is `0`, it sets `port` to `6443`. The other CRDs (`PhysicalHost`, `Beskar7Machine`, `Beskar7MachineTemplate`) have no webhooks at all.

## Example

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
kind: Beskar7Cluster
metadata:
  name: production-cluster
  namespace: default
  labels:
    cluster.x-k8s.io/cluster-name: production-cluster
spec:
  controlPlaneEndpoint:
    host: "10.0.1.10"
    port: 6443
```

Pair with a CAPI `Cluster`:

```yaml
apiVersion: cluster.x-k8s.io/v1beta1
kind: Cluster
metadata:
  name: production-cluster
  namespace: default
spec:
  clusterNetwork:
    pods:
      cidrBlocks: ["10.244.0.0/16"]
    services:
      cidrBlocks: ["10.96.0.0/12"]
  infrastructureRef:
    apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
    kind: Beskar7Cluster
    name: production-cluster
  controlPlaneRef:
    apiVersion: controlplane.cluster.x-k8s.io/v1beta1
    kind: KubeadmControlPlane
    name: production-cluster-control-plane
```

## Print columns

`kubectl get beskar7cluster` shows `Cluster`, `Ready`, `Endpoint`, and `Age`.

## See also

- [API Reference: Beskar7Cluster](api-reference.md#beskar7cluster)
- [Beskar7Machine](beskar7machine.md)
- [Architecture](architecture.md)
- [Examples](../examples/)
