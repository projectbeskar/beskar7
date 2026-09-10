# Beskar7MachineTemplate

> **Audience:** Operators

`Beskar7MachineTemplate` is a pure schema. CAPI's `KubeadmControlPlane` and `MachineDeployment` reference it to mint `Beskar7Machine` objects with the same spec.

There is **no** Beskar7MachineTemplate controller, **no** validating or defaulting webhook, and **no** immutability enforcement. The template's `template.spec` is whatever any author writes; CAPI clones it onto each `Beskar7Machine`. Validation of the inner spec happens when the cloned `Beskar7Machine` hits the API server (OpenAPI schema rules from `api/v1beta2/beskar7machine_types.go`).

## Identity

- **API:** `infrastructure.cluster.x-k8s.io/v1beta2`
- **Kind:** `Beskar7MachineTemplate`
- **Short name:** `b7mt`
- **Scope:** Namespaced
- **Categories:** `cluster-api` (`kubectl get cluster-api` lists templates)
- **clusterctl:** the CRD carries `clusterctl.cluster.x-k8s.io`, so `clusterctl move` discovers templates; a template is moved through the owner reference to the `Cluster` that CAPI sets when a `KubeadmControlPlane` or `MachineSet` references it (see [Installation](installation.md#clusterctl-move)).

## Spec

The spec wraps a `Beskar7MachineSpec` exactly:

```yaml
spec:
  template:
    spec:
      # Identical to Beskar7Machine.spec
      inspectionImageURL: ...
      targetImageURL: ...
      targetImageDigest: ...
      hardwareRequirements:
        minCPUCores: ...
        minMemoryGB: ...
        minDiskGB: ...
      hostSelector:            # optional: only claim PhysicalHosts with these labels
        matchLabels:
          node-role: ...
```

For the field reference, see [API Reference: Beskar7Machine](api-reference.md#beskar7machine). `hostSelector` is what keeps a control plane and a worker pool from racing for the same hosts — see [Beskar7Machine → Steering the claim](beskar7machine.md#steering-the-claim-hostselector-and-failure-domains).

## CAPI integration

### KubeadmControlPlane

```yaml
apiVersion: controlplane.cluster.x-k8s.io/v1beta1
kind: KubeadmControlPlane
metadata:
  name: production-control-plane
  namespace: default
spec:
  replicas: 3
  version: v1.31.0
  machineTemplate:
    infrastructureRef:
      apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
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
apiVersion: cluster.x-k8s.io/v1beta1
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
          apiVersion: bootstrap.cluster.x-k8s.io/v1beta1
          kind: KubeadmConfigTemplate
          name: production-workers
      infrastructureRef:
        apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
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
