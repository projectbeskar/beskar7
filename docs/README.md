# Beskar7 Documentation

This directory contains the user-facing documentation for the Beskar7 Cluster API infrastructure provider. The source of truth for everything described here is the code in `api/v1beta1/`, `controllers/`, `cmd/manager/`, and `internal/`. If a doc and the code disagree, the code wins.

## Getting started

- [Concepts](introduction.md) — what each CRD represents and how a node gets provisioned.
- [Installation](installation.md) — prerequisites, Helm install, manifest install, verification.
- [Quick Start](quick-start.md) — first `PhysicalHost` + `Beskar7Machine` flow.
- [Architecture](architecture.md) — controllers, callback endpoint, inspection workflow.

## API reference

- [API Reference](api-reference.md) — every CRD field in `v1beta1`.
- [PhysicalHost](physicalhost.md) — operational notes for the host CRD.
- [Beskar7Machine](beskar7machine.md) — operational notes for the machine CRD.
- [Beskar7Cluster](beskar7cluster.md) — operational notes for the cluster CRD.
- [Beskar7MachineTemplate](beskar7machinetemplate.md) — template schema; consumed by KubeadmControlPlane / MachineDeployment.

## Operations

- [State Management](state-management.md) — PhysicalHost lifecycle, transition rules, recovery.
- [Deployment Best Practices](deployment-best-practices.md) — production deployment guidance.
- [iPXE Setup](ipxe-setup.md) — DHCP, HTTP boot server, kernel cmdline variables consumed by inspection and bootstrap.
- [Advanced Usage](advanced-usage.md) — bootstrap-data flow, hardware requirements.
- [Troubleshooting](troubleshooting.md) — diagnostic procedures for common failures.
- [Resource Planning](resource-planning.md) — sizing the controller and the inspection footprint.

## Hardware and compatibility

- [Hardware Compatibility Matrix](hardware-compatibility.md) — Redfish-compliant BMCs that have been exercised.

## Observability

- [Metrics](metrics.md) — Prometheus surface and how to scrape it.

## Security

- [Security overview](security/README.md) — what the operator actually enforces.
- [Security configuration](security/configuration.md) — TLS, credentials, manager flags.
- [RBAC hardening](security/rbac-hardening.md) — the deployed ClusterRole and rationale.
- [Security troubleshooting](security/troubleshooting.md) — diagnosing security-control failures.

## Development

- [Development Setup](development-setup.md) — clone, build, test, deploy from source.

## Testing

- [Smoke testing](smoke-testing.md) — five-layer end-to-end harness (`make smoke`) for verifying a build against a live kind cluster.
- [CI/CD and Testing](ci-cd-and-testing.md) — unit/integration/lint expectations and the release workflow.

## Examples

- [`examples/`](../examples/) — working YAML for a minimal test, a single-host smoke test, and a complete cluster.

## Reading order

If you are new to Beskar7:

1. [Concepts](introduction.md)
2. [Installation](installation.md)
3. [Quick Start](quick-start.md)
4. [Hardware Compatibility](hardware-compatibility.md)
5. [iPXE Setup](ipxe-setup.md)
6. The CRD docs you will be editing: [PhysicalHost](physicalhost.md), [Beskar7Machine](beskar7machine.md), [Beskar7Cluster](beskar7cluster.md).

If you are operating an existing install:

1. [Deployment Best Practices](deployment-best-practices.md)
2. [State Management](state-management.md)
3. [Metrics](metrics.md)
4. [Troubleshooting](troubleshooting.md)

## Vendor notes

Beskar7 uses only the universally-supported portions of Redfish (power state, one-time PXE boot source, system info, network interface enumeration). It does not require vendor-specific extensions and does not ship vendor-specific code paths.

| Vendor | BMC product | Tested | Notes |
|---|---|---|---|
| Dell EMC | iDRAC 8/9 | Yes | Redfish must be enabled in iDRAC settings. |
| HPE | iLO 5 | Yes | Requires an iLO Advanced license for some power operations. |
| Lenovo | XCC | Yes | – |
| Supermicro | BMC | Yes | Redfish API enable in Configuration → Redfish API. |
| Other Redfish-compliant BMCs | – | – | Should work; please report results. |

For BMC-specific configuration tips, see [Hardware Compatibility](hardware-compatibility.md).

## Contributing

For information about contributing, see the main repository documentation.
