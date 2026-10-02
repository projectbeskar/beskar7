# Quick Start

> **Audience:** Operators · Developers

This page takes one host from enrolment to a provisioned node. It assumes Beskar7 is already installed.

- Not installed yet? See [Installation](installation.md) (operators) or [Development Setup](development-setup.md) (developers).
- Want to understand the resources before applying them? See [Concepts](introduction.md).

## 1. Enrol a host

`examples/minimal-test.yaml` holds a BMC credentials Secret, one `PhysicalHost` and one `Beskar7Machine`. Edit it first: the BMC address, the credentials, and the Secret's `beskar7.infrastructure.cluster.x-k8s.io/bmc-addresses` annotation, which must list that address (see [Physical Hosts](physicalhost.md#binding-the-credentials-to-their-bmc)).

```bash
kubectl apply -f examples/minimal-test.yaml
kubectl get physicalhost test-server -w     # → Available once the BMC answers
```

If the host does not reach `Available`, `kubectl describe physicalhost test-server` shows why in the `RedfishConnectionReady` condition.

The `Beskar7Machine` in this file stays put: a `Beskar7Machine` is driven by the Cluster API `Machine` that owns it, and without one it waits at `Waiting for Machine Controller to set OwnerRef` and never claims the host. That is expected — delete it before step 2 (`kubectl delete beskar7machine test-machine`).

## 2. Provision it

`examples/kairos-k3s-node.yaml` is the whole single-node flow: a namespace, credentials and bootstrap Secrets, a `PhysicalHost`, a `Beskar7Cluster`, a Cluster API `Cluster`, a standalone `Machine` and its `Beskar7Machine`. It needs a working iPXE setup (see [iPXE Setup](ipxe-setup.md)), the [inspector](https://github.com/projectbeskar/beskar7-inspector) served from your boot server, and a Kairos k3s image with its `sha256` digest. Replace every `<...>` placeholder, then:

```bash
kubectl apply -f examples/kairos-k3s-node.yaml

# The host: Available → InUse → Inspecting → Deploying → Ready
kubectl -n <namespace> get physicalhost -w

# The machine: Provisioning → Provisioned, with a providerID of b7://<namespace>/<host>
kubectl -n <namespace> get machine -w
```

Check for errors:

```bash
kubectl -n <namespace> describe physicalhost <host>
kubectl -n <namespace> describe beskar7machine <machine>
kubectl logs -n capb7-system -l control-plane=controller-manager
```

## Next steps

- [iPXE Setup](ipxe-setup.md) — configure DHCP and HTTP boot server so the inspection image boots correctly.
- [State Management](state-management.md) — PhysicalHost lifecycle states and recovery.
- [Troubleshooting](troubleshooting.md) — common failures and how to diagnose them.
- [Examples](../examples/) — single-host smoke test and full cluster configurations.
