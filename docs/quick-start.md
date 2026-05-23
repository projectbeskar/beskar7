# Quick Start

> **Audience:** Operators · Developers

This page shows the smallest working flow: one `PhysicalHost` plus one `Beskar7Machine`. It assumes Beskar7 is already installed.

- Not installed yet? See [Installation](installation.md) (operators) or [Development Setup](development-setup.md) (developers).
- Want to understand the resources before applying them? See [Concepts](introduction.md).

## Apply the minimal example

The `examples/minimal-test.yaml` file in the repository contains a single `PhysicalHost` and a single `Beskar7Machine` with no hardware requirements.

```bash
kubectl apply -f https://github.com/projectbeskar/beskar7/raw/main/examples/minimal-test.yaml
```

Or, from a local clone:

```bash
kubectl apply -f examples/minimal-test.yaml
```

Edit the file first to set your BMC address, credentials Secret name, inspection image URL, and target image URL.

## Watch the provisioning flow

```bash
# Host should transition Available → Inspecting → InUse
kubectl get physicalhost test-server -w

# Machine should reach Ready=true
kubectl get beskar7machine test-machine -w
```

Check for errors:

```bash
kubectl describe physicalhost test-server
kubectl describe beskar7machine test-machine
kubectl logs -n beskar7-system -l control-plane=beskar7-controller-manager
```

## Next steps

- [iPXE Setup](ipxe-setup.md) — configure DHCP and HTTP boot server so the inspection image boots correctly.
- [State Management](state-management.md) — PhysicalHost lifecycle states and recovery.
- [Troubleshooting](troubleshooting.md) — common failures and how to diagnose them.
- [Examples](../examples/) — single-host smoke test and full cluster configurations.
