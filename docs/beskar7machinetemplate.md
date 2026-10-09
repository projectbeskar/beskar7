# Beskar7MachineTemplate

> **Audience:** Operators

`Beskar7MachineTemplate` is a pure schema. CAPI's `KubeadmControlPlane` and `MachineDeployment` reference it to mint `Beskar7Machine` objects with the same spec.

There is **no** Beskar7MachineTemplate controller, **no** validating or defaulting webhook, and **no** immutability enforcement. The template's `template.spec` is whatever any author writes; CAPI clones it onto each `Beskar7Machine`. The same OpenAPI schema rules and the CEL rule on the spec (`api/v1beta2/beskar7machine_types.go`) apply to the template itself and, again, to each cloned `Beskar7Machine` when it hits the API server.

## Identity

- **API:** `infrastructure.cluster.x-k8s.io/v1beta2`
- **Kind:** `Beskar7MachineTemplate`
- **Short name:** `b7mt`
- **Scope:** Namespaced
- **Categories:** `cluster-api` (`kubectl get cluster-api` lists templates)
- **clusterctl:** the CRD carries `clusterctl.cluster.x-k8s.io`, so `clusterctl move` discovers templates; a template is moved through the owner reference to the `Cluster` that CAPI sets when a `KubeadmControlPlane` or `MachineSet` references it (see [Installation](installation.md#clusterctl-move)).

## Spec

`spec.template.metadata` is optional and carries labels and annotations to propagate onto each generated
`Beskar7Machine`. Cluster API clones a template by lifting the whole `spec.template` map and making it the new
object, so what you put there becomes the machine's own metadata, with the labels CAPI adds (`cluster.x-k8s.io/cluster-name`,
the cloned-from annotations) merged on top. Only labels and annotations survive that clone — name, namespace, UID,
resourceVersion and finalizers are all overwritten or cleared.

`spec.template.spec` wraps a `Beskar7MachineSpec` exactly:

```yaml
spec:
  template:
    spec:
      # Identical to Beskar7Machine.spec
      inspectionImageURL: ...
      targetImageURL: ...
      targetImageDigest: ...         # exactly one of targetImageDigest and targetImageDigestURL
      # targetImageDigestURL: https://...
      hardwareRequirements:
        minCPUCores: ...
        minMemoryGB: ...
        minDiskGB: ...
      hostSelector:            # optional: only claim PhysicalHosts with these labels
        matchLabels:
          node-role: ...
```

The template's `spec` carries the same CRD rule as a `Beskar7Machine`'s: set **exactly one** of `targetImageDigest` and `targetImageDigestURL`, and an `http://` digest URL is refused. A template that breaks it is rejected when it is created, not when its first machine is. With `targetImageDigestURL` every machine cloned from the template reads the checksum file for itself, when that machine is created or replaced, and pins what it read: the template does not pin anything. Point it at a versioned file that never changes, or the machines of one `MachineDeployment` can get different images (see [Beskar7Machine → Naming the digest by URL](beskar7machine.md#naming-the-digest-by-url-targetimagedigesturl)); with `targetImageDigest` the template fixes the digest for all of them.

For the field reference, see [API Reference: Beskar7Machine](api-reference.md#beskar7machine). `hostSelector` is what keeps a control plane and a worker pool from racing for the same hosts — see [Beskar7Machine → Steering the claim](beskar7machine.md#steering-the-claim-hostselector-and-failure-domains).

## CAPI integration

### KubeadmControlPlane

```yaml
apiVersion: controlplane.cluster.x-k8s.io/v1beta2
kind: KubeadmControlPlane
metadata:
  name: production-control-plane
  namespace: default
spec:
  replicas: 3
  version: v1.31.0
  machineTemplate:
    spec:
      infrastructureRef:
        apiGroup: infrastructure.cluster.x-k8s.io
        kind: Beskar7MachineTemplate
        name: production-control-plane
  kubeadmConfigSpec:
    # ... cluster init/join config
---
apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
kind: Beskar7MachineTemplate
metadata:
  name: production-control-plane
  namespace: default
spec:
  template:
    spec:
      inspectionImageURL: "https://boot-server.local/inspector"
      targetImageURL:     "http://boot-server.local/images/kairos-alpine-v2.8.1.raw"
      targetImageDigest:  "sha256:0000000000000000000000000000000000000000000000000000000000000000"
      hardwareRequirements:
        minCPUCores: 4
        minMemoryGB: 16
        minDiskGB:   100
      hostSelector:
        matchLabels:
          node-role: control-plane     # only the hosts labelled for the control plane
```

### MachineDeployment

```yaml
apiVersion: cluster.x-k8s.io/v1beta2
kind: MachineDeployment
metadata:
  name: production-workers
  namespace: default
spec:
  clusterName: production-cluster
  replicas: 3
  selector:
    matchLabels:
      cluster.x-k8s.io/cluster-name: production-cluster
      cluster.x-k8s.io/deployment-name: production-workers
  template:
    metadata:
      labels:
        cluster.x-k8s.io/cluster-name: production-cluster
        cluster.x-k8s.io/deployment-name: production-workers
    spec:
      clusterName: production-cluster
      version: v1.31.0
      bootstrap:
        configRef:
          apiGroup: bootstrap.cluster.x-k8s.io
          kind: KubeadmConfigTemplate
          name: production-workers
      infrastructureRef:
        apiGroup: infrastructure.cluster.x-k8s.io
        kind: Beskar7MachineTemplate
        name: production-workers
---
apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
kind: Beskar7MachineTemplate
metadata:
  name: production-workers
  namespace: default
spec:
  template:
    spec:
      inspectionImageURL: "https://boot-server.local/inspector"
      targetImageURL:     "http://boot-server.local/images/kairos-alpine-v2.8.1.raw"
      targetImageDigest:  "sha256:0000000000000000000000000000000000000000000000000000000000000000"
      hardwareRequirements:
        minCPUCores: 4
        minMemoryGB: 8
        minDiskGB:   50
      hostSelector:
        matchLabels:
          node-role: worker            # disjoint from the control plane's pool
```

## Versioning

Because there is no immutability webhook, editing a template's `template.spec` is silently allowed by the API server. CAPI's behavior is to keep existing `Beskar7Machine` objects unchanged but use the new template for any future replicas. If you want strict separation, version the template's name (`worker-template-v1`, `worker-template-v2`) and update the consuming `MachineDeployment` / `KubeadmControlPlane` to reference the new name.

## Print columns

`kubectl get beskar7machinetemplate` shows the standard `kubectl` columns (no custom print columns are defined).

## See also

- [API Reference: Beskar7Machine](api-reference.md#beskar7machine)
- [Beskar7Machine](beskar7machine.md)
- [Examples](../examples/)
