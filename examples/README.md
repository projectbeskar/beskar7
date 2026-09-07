# Beskar7 Examples

> **Audience:** Operators · Developers

Working YAML examples for Beskar7. Pair these with the [installation guide](../docs/installation.md) and the [first-flow walk-through](../docs/quick-start.md).

## Files

| File | Scope | Description | Apply |
|---|---|---|---|
| `minimal-test.yaml` | Minimal | One `PhysicalHost` + one `Beskar7Machine`, no hardware requirements. Quickest way to verify the operator is working. | `kubectl apply -f minimal-test.yaml` |
| `minimal-test-cluster.yaml` | Single-host | Minimal full CAPI stack (Cluster + Beskar7Cluster + Machine + Beskar7Machine) using a single host. | `kubectl apply -f minimal-test-cluster.yaml` |
| `simple-cluster.yaml` | Full cluster | Three PhysicalHosts, one control-plane node, two worker nodes, hardware requirements, full CAPI integration. Note: this file omits the KubeadmControlPlane — provisioning blocks at "Waiting for control plane" until you add one. For a fixture proven end-to-end, see `kairos-k3s-node.yaml`. | `kubectl apply -f simple-cluster.yaml` |
| `kairos-k3s-node.yaml` | Single-node | **Proven end-to-end (contract v4).** Namespace + BMC credentials Secret + Kairos `#cloud-config` bootstrap Secret + PhysicalHost + Beskar7Cluster + CAPI Cluster + standalone Machine + Beskar7Machine. Drives the full provisioning loop: claim → PXE → inspection → Deploying → whole-disk write → `/provisioned` callback → Ready k3s node with ProviderID set. Replace every `<...>` placeholder before applying. | `kubectl apply -f kairos-k3s-node.yaml` |
| `kairos-providerid-stage.yaml` | Image-side stage | **Verified end-to-end (Kairos v4.1.2 + k3s v1.34.8).** Bake into the target image's `COS_OEM` as `/oem/10_beskar7_providerid.yaml`. Reads the per-host value beskar7 injects at `/oem/beskar7/provider-id` and writes the kubelet `--provider-id`, so ONE image and ONE `Beskar7MachineTemplate` serve every replica of a pool. Must be **yip format** (a `#cloud-config` file's `stages:` block is silently ignored) and must run **before** the distro first starts (`Node.spec.providerID` is immutable after registration). | bake into the image, not `kubectl apply` |
| `complete-cluster.yaml` | (Deprecated) | Previously contained a KubeadmControlPlane stack that cannot produce a working Kairos node. Now contains a redirect comment to `kairos-k3s-node.yaml`. | — |
| `security/` | Security profile | Network policies and pod security configurations for hardened deployments. | `kubectl apply -f security/` |

## See also

- [Installation guide](../docs/installation.md) — prerequisites and install steps.
- [Quick start](../docs/quick-start.md) — first-flow walk-through using `minimal-test.yaml`.
- [Troubleshooting](../docs/troubleshooting.md) — common failures and diagnostics.
- [PhysicalHost](../docs/physicalhost.md), [Beskar7Machine](../docs/beskar7machine.md), [Beskar7Cluster](../docs/beskar7cluster.md) — per-CRD field reference.
