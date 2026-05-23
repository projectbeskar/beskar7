# Beskar7 Examples

> **Audience:** Operators · Developers

Working YAML examples for Beskar7. Pair these with the [installation guide](../docs/installation.md) and the [first-flow walk-through](../docs/quick-start.md).

## Files

| File | Scope | Description | Apply |
|---|---|---|---|
| `minimal-test.yaml` | Minimal | One `PhysicalHost` + one `Beskar7Machine`, no hardware requirements. Quickest way to verify the operator is working. | `kubectl apply -f minimal-test.yaml` |
| `minimal-test-cluster.yaml` | Single-host | Minimal full CAPI stack (Cluster + Beskar7Cluster + Machine + Beskar7Machine) using a single host. | `kubectl apply -f minimal-test-cluster.yaml` |
| `simple-cluster.yaml` | Full cluster | Three PhysicalHosts, one control-plane node, two worker nodes, hardware requirements, full CAPI integration. Note: this file omits the KubeadmControlPlane — provisioning blocks at "Waiting for control plane" until you add one. See `complete-cluster.yaml` for the full stack. | `kubectl apply -f simple-cluster.yaml` |
| `complete-cluster.yaml` | Full cluster | KubeadmControlPlane + MachineDeployment + templates + Beskar7Cluster + PhysicalHosts. The complete production-ready configuration. See `complete-cluster.md` for the walkthrough. | `kubectl apply -f complete-cluster.yaml` |
| `security/` | Security profile | Network policies and pod security configurations for hardened deployments. | `kubectl apply -f security/` |

## See also

- [Installation guide](../docs/installation.md) — prerequisites and install steps.
- [Quick start](../docs/quick-start.md) — first-flow walk-through using `minimal-test.yaml`.
- [Troubleshooting](../docs/troubleshooting.md) — common failures and diagnostics.
- [PhysicalHost](../docs/physicalhost.md), [Beskar7Machine](../docs/beskar7machine.md), [Beskar7Cluster](../docs/beskar7cluster.md) — per-CRD field reference.
