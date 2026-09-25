# Beskar7ClusterTemplate

> **Audience:** Operators

`Beskar7ClusterTemplate` is a pure schema. It exists so Beskar7 can be used from a
[`ClusterClass`](https://cluster-api.sigs.k8s.io/tasks/experimental-features/cluster-class/): a `ClusterClass` points
`spec.infrastructure.templateRef` at one of these, and Cluster API's topology controller creates one `Beskar7Cluster`
per `Cluster` from it.

Without this CRD a `ClusterClass` naming Beskar7 cannot be created at all — which is the whole reason it exists
([#209](https://github.com/projectbeskar/beskar7/issues/209)).

There is **no** Beskar7ClusterTemplate controller, **no** webhook, and **no** immutability enforcement. Templates are
inert: Cluster API reads them and nothing in Beskar7 reconciles them.

## Identity

- **API:** `infrastructure.cluster.x-k8s.io/v1beta2`
- **Kind:** `Beskar7ClusterTemplate`
- **Short name:** `b7ct`
- **Scope:** Namespaced
- **Categories:** `cluster-api`
- **clusterctl:** the CRD carries `clusterctl.cluster.x-k8s.io`, so `clusterctl move` discovers it.

## Spec

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
kind: Beskar7ClusterTemplate
metadata:
  name: beskar7-cluster-template
  namespace: default
spec:
  template:
    metadata:          # optional; labels/annotations propagated to each Beskar7Cluster
      labels:
        example.com/tier: production
    spec: {}           # see below — empty is correct
```

### Why `spec` is empty

`Beskar7ClusterSpec` has exactly one field, `controlPlaneEndpoint`, and that is precisely the field you should **not**
template. Every cluster needs its own endpoint, and Beskar7 does not discover one (see
[Beskar7Cluster → control-plane endpoint](beskar7cluster.md)) — setting it in the template would hand every cluster
in the `ClusterClass` the same endpoint. Instead, each `Cluster` supplies its own value another way: a
`controlPlaneEndpoint` `ClusterClass` variable, patched per-Cluster into the generated `Beskar7Cluster`'s
`spec.controlPlaneEndpoint` (see [`examples/clusterclass.yaml`](../examples/clusterclass.yaml)), or
`Cluster.spec.controlPlaneEndpoint` set directly on the topology `Cluster`.

So `spec: {}` is the normal, correct content. The template earns its keep by existing, not by carrying configuration.
If `Beskar7ClusterSpec` grows genuinely per-class fields later, they belong here.

### `spec.template.metadata`

Cluster API reads labels and annotations from `spec.template.metadata` (the `InfrastructureClusterTemplate` contract)
and propagates them onto each generated `Beskar7Cluster`. Other `ObjectMeta` fields are ignored.

## Using it from a ClusterClass

See [`examples/clusterclass.yaml`](../examples/clusterclass.yaml) for a complete, applyable `ClusterClass` plus the
topology `Cluster` that consumes it.

The shape is:

```yaml
apiVersion: cluster.x-k8s.io/v1beta2
kind: ClusterClass
spec:
  infrastructure:
    templateRef:
      apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
      kind: Beskar7ClusterTemplate
      name: beskar7-cluster-template
```

and each `Cluster` then names the class instead of an infrastructure ref of its own:

```yaml
apiVersion: cluster.x-k8s.io/v1beta2
kind: Cluster
spec:
  topology:
    classRef:
      name: beskar7-cluster-class
    version: v1.34.8+k0s.0
```

## Versioning

There is no immutability webhook, so editing a template in place is allowed by the API server. Cluster API's topology
controller reconciles existing clusters toward the current template, which for an empty `spec` means nothing changes.
If the spec ever carries real fields, version the template by name (`…-v1`, `…-v2`) and point the `ClusterClass` at the
new one, the same way [Beskar7MachineTemplate](beskar7machinetemplate.md#versioning) recommends.

## Print columns

`kubectl get beskar7clustertemplate` shows the standard `kubectl` columns (no custom print columns are defined).

## See also

- [Beskar7Cluster](beskar7cluster.md)
- [Beskar7MachineTemplate](beskar7machinetemplate.md) — the machine-side equivalent, which a `ClusterClass` also needs
  for its `MachineDeployment` classes
- [Examples](../examples/)
