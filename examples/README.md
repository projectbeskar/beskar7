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
| `kairos-k0s-providerid-stage.yaml` | Image-side stage (k0s) | **Verified end-to-end (Kairos v4.1.2 + k0s v1.34.8+k0s.0).** The k0s counterpart of the stage above, at the same path in `COS_OEM`. k0s has no working kubelet-flag route (the Kairos k0s provider drops `--kubelet-extra-args`), so this stage patches `Node.spec.providerID` from `/oem/beskar7/provider-id` right after the node registers. | bake into the image, not `kubectl apply` |
| `kairos-k0s-start-gate.yaml` | Image-side stage (k0s) | **Required for k0s; verified with the stage above.** Bake into `COS_OEM` as `/oem/05_beskar7_k0s_gate.yaml`. Keeps `k0scontroller`/`k0sworker` from starting on the recovery-partition install boot (where a joiner otherwise becomes a voting etcd member and is then rebooted, killing quorum) or before the bootstrap provider has written its arguments. Opens on `/etc/k0s/.capi-args-ready`, written by cluster-api-provider-kairos ≥ `3698d55` — with an older provider k0s never starts. | bake into the image, not `kubectl apply` |
| `complete-cluster.yaml` | (Deprecated) | Previously contained a KubeadmControlPlane stack that cannot produce a working Kairos node. Now contains a redirect comment to `kairos-k3s-node.yaml`. | — |
| `security/` | Security profile | Network policies and pod security configurations for hardened deployments. | `kubectl apply -f security/` |

## See also

- [Installation guide](../docs/installation.md) — prerequisites and install steps.
- [Quick start](../docs/quick-start.md) — first-flow walk-through using `minimal-test.yaml`.
- [Building a target image](../docs/building-images.md) — building a raw Kairos image and baking the image-side stages into `COS_OEM`.
- [Troubleshooting](../docs/troubleshooting.md) — common failures and diagnostics.
- [PhysicalHost](../docs/physicalhost.md), [Beskar7Machine](../docs/beskar7machine.md), [Beskar7Cluster](../docs/beskar7cluster.md) — per-CRD field reference.
