# Beskar7Cluster

`Beskar7Cluster` is the Beskar7 CRD that implements the CAPI infrastructure-cluster contract. The reconciler that owns it is `controllers/beskar7cluster_controller.go`. It is the only Beskar7 resource with an admission webhook (`api/v1beta1/webhooks/beskar7cluster_webhook.go`).

For the full field reference, see [API Reference: Beskar7Cluster](api-reference.md#beskar7cluster). This page covers operational behavior — what the controller derives, when it sets which condition.

## Identity

- **API:** `infrastructure.cluster.x-k8s.io/v1beta1`
- **Kind:** `Beskar7Cluster`
- **Scope:** Namespaced
- **Categories:** `cluster-api`

## Spec

```yaml
spec:
  controlPlaneEndpoint:
    host: "10.0.1.10"   # optional; controller derives if unset
    port: 6443          # optional
```

## What the reconciler does

### Control-plane endpoint

The controller derives the endpoint by:

1. Listing CAPI `Machine` objects with the `cluster.x-k8s.io/cluster-name=<this-cluster>` label and the `cluster.x-k8s.io/control-plane` label.
2. Picking a `Machine` whose `InfrastructureRef` points at a Beskar7Machine and whose status reports an `InternalIP` (with `ExternalIP` as fallback).
3. Writing that address to `Status.ControlPlaneEndpoint`.

If `Spec.ControlPlaneEndpoint.Host` is non-empty, the controller honors it authoritatively and skips discovery. If only `Spec.ControlPlaneEndpoint.Port` is set, discovery still finds the host but the user's port wins. The default port when neither is supplied is `6443`.

### Failure domains

The controller lists `PhysicalHost` resources in the same namespace, extracts unique values from the `topology.kubernetes.io/zone` label, and populates `Status.FailureDomains`:

```yaml
metadata:
  labels:
    topology.kubernetes.io/zone: rack-1
```

CAPI uses these for placement.

## Conditions

| Type | Meaning | Common reasons |
|---|---|---|
| `ControlPlaneEndpointReady` | The endpoint is populated. | `ControlPlaneEndpointNotSet`. |

## Webhook

`Beskar7Cluster` has a validating webhook that checks `controlPlaneEndpoint.host` (IP or hostname) and `port` (1–65535). The webhook ships with `failurePolicy: Fail`, so a Pods/Beskar7Cluster admission attempt without a healthy webhook service is rejected.

There are no defaulting webhooks. The other CRDs (`PhysicalHost`, `Beskar7Machine`, `Beskar7MachineTemplate`) have no webhooks at all.

## Example

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
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
    apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
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
