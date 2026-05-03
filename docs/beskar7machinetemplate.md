# Beskar7MachineTemplate

`Beskar7MachineTemplate` is a pure schema. CAPI's `KubeadmControlPlane` and `MachineDeployment` reference it to mint `Beskar7Machine` objects with the same spec.

There is **no** Beskar7MachineTemplate controller, **no** validating or defaulting webhook, and **no** immutability enforcement. The template's `template.spec` is whatever any author writes; CAPI clones it onto each `Beskar7Machine`. Validation of the inner spec happens when the cloned `Beskar7Machine` hits the API server (OpenAPI schema rules from `api/v1beta1/beskar7machine_types.go`).

## Identity

- **API:** `infrastructure.cluster.x-k8s.io/v1beta1`
- **Kind:** `Beskar7MachineTemplate`
- **Short name:** `b7mt`
- **Scope:** Namespaced
- **Categories:** `cluster-api` (so `clusterctl move` walks it during workload-cluster migration)

## Spec

The spec wraps a `Beskar7MachineSpec` exactly:

```yaml
spec:
  template:
    spec:
      # Identical to Beskar7Machine.spec
      inspectionImageURL: ...
      targetImageURL: ...
      configurationURL: ...
      hardwareRequirements:
        minCPUCores: ...
        minMemoryGB: ...
        minDiskGB: ...
```

For the field reference, see [API Reference: Beskar7Machine](api-reference.md#beskar7machine).

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
      apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
      kind: Beskar7MachineTemplate
      name: production-control-plane
  kubeadmConfigSpec:
    # ... cluster init/join config
---
apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
kind: Beskar7MachineTemplate
metadata:
  name: production-control-plane
  namespace: default
spec:
  template:
    spec:
      inspectionImageURL: "http://boot-server.local/ipxe/inspect.ipxe"
      targetImageURL:     "http://boot-server.local/images/kairos-alpine-v2.8.1.tar.gz"
      configurationURL:   "http://boot-server.local/configs/control-plane.yaml"
      hardwareRequirements:
        minCPUCores: 4
        minMemoryGB: 16
        minDiskGB:   100
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
        apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
        kind: Beskar7MachineTemplate
        name: production-workers
---
apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
kind: Beskar7MachineTemplate
metadata:
  name: production-workers
  namespace: default
spec:
  template:
    spec:
      inspectionImageURL: "http://boot-server.local/ipxe/inspect.ipxe"
      targetImageURL:     "http://boot-server.local/images/kairos-alpine-v2.8.1.tar.gz"
      configurationURL:   "http://boot-server.local/configs/worker.yaml"
      hardwareRequirements:
        minCPUCores: 4
        minMemoryGB: 8
        minDiskGB:   50
```

## Versioning

Because there is no immutability webhook, editing a template's `template.spec` is silently allowed by the API server. CAPI's behavior is to keep existing `Beskar7Machine` objects unchanged but use the new template for any future replicas. If you want strict separation, version the template's name (`worker-template-v1`, `worker-template-v2`) and update the consuming `MachineDeployment` / `KubeadmControlPlane` to reference the new name.

## Print columns

`kubectl get beskar7machinetemplate` shows the standard `kubectl` columns (no custom print columns are defined).

## See also

- [API Reference: Beskar7Machine](api-reference.md#beskar7machine)
- [Beskar7Machine](beskar7machine.md)
- [Examples](../examples/)
