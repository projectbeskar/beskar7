# Beskar7: Bare-Metal Provisioning for Kubernetes

A Kubernetes operator that implements the Cluster API infrastructure provider for bare-metal machines.

**Simple, reliable approach:** Redfish power management + iPXE network boot + Hardware inspection.

## Why Beskar7?

- **Simple** - No complex vendor-specific workarounds
- **Reliable** - Only uses universally-supported Redfish features
- **Vendor Agnostic** - Works with any Redfish-compliant BMC
- **Hardware Discovery** - Collects real hardware specs via inspection
- **Clean architecture, minimal dependencies** - Distroless image, no CGO, narrow RBAC

## How It Works

1. Beskar7 claims a physical host
2. Sets PXE boot flag via Redfish
3. Powers on the server
4. Server network boots inspection image (iPXE)
5. Inspection image collects hardware details
6. Reports back to Beskar7
7. Validates hardware requirements
8. Writes the digest-verified whole-disk OS image and injects the bootstrap config
9. Reboots into the provisioned OS; the machine joins the cluster

## Current Status

**Version:** v0.4.0 — **first GA release**  
**API:** `v1beta1` is **stable and frozen**. The schema evolves **additive-only**; a breaking change requires a future `v1beta2` introduced with a conversion webhook.  
**Contract:** controller↔inspector wire contract **v4.2, frozen** ([contract](docs/inspector-contract.md)). Pair with a `contract-v4.2` [inspector release](https://github.com/projectbeskar/beskar7-inspector/releases).  
**Upgrading:** v0.4.0 is **not** compatible with v0.3.x, and the alpha series contains breaking API changes — see [Upgrading](docs/upgrading.md) and the [CHANGELOG](CHANGELOG.md).

## Installation

### Prerequisites

1. Kubernetes v1.31+ with kubectl configured
2. Cluster API v1.10+ ([install with clusterctl](https://cluster-api.sigs.k8s.io/user/quick-start.html))
3. cert-manager v1.16+ ([installation guide](https://cert-manager.io/docs/installation/))
4. iPXE infrastructure - DHCP + HTTP server ([setup guide](docs/ipxe-setup.md))
5. Inspection image - `vmlinuz` + `initrd.img` from [beskar7-inspector releases](https://github.com/projectbeskar/beskar7-inspector/releases), served by your boot server. Match the inspector to the contract version your controller speaks (see [iPXE Setup](docs/ipxe-setup.md)).

### Quick Install

**Using Helm (Recommended):**

```bash
helm repo add beskar7 https://projectbeskar.github.io/beskar7
helm repo update
helm install beskar7 beskar7/beskar7 \
  --namespace beskar7-system --create-namespace
```


**Using Release Manifests:**

```bash
kubectl apply -f https://github.com/projectbeskar/beskar7/releases/download/v0.4.0/beskar7-manifests-v0.4.0.yaml
```

See [Installation](docs/installation.md) for detailed install steps, or the [Quick Start](docs/quick-start.md) for the first provisioning flow.

## Basic Usage

### 1. Register a Physical Host

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
kind: PhysicalHost
metadata:
  name: server-01
spec:
  redfishConnection:
    address: "https://192.168.1.100"
    credentialsSecretRef: "bmc-credentials"
```

### 2. Create a Machine

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
kind: Beskar7Machine
metadata:
  name: worker-01
spec:
  # Directory the boot server serves the inspector from; the controller
  # appends /vmlinuz and /initrd.img when it renders the iPXE script.
  inspectionImageURL: "http://boot-server/beskar7-inspector"
  # A whole-disk raw OS image (not a tarball or ISO) with a bootstrap agent
  # baked in — see docs/beskar7machine.md for the image requirements.
  targetImageURL: "http://boot-server/images/kairos-k3s.raw"
  # Required. The inspector verifies the downloaded bytes against this and
  # refuses to write the disk on a mismatch — it is the integrity anchor.
  #   sha256sum kairos-k3s.raw
  targetImageDigest: "sha256:<64-hex-digest-of-the-bytes-at-targetImageURL>"
  hardwareRequirements:
    minCPUCores: 4
    minMemoryGB: 16
```

> These snippets assume the boot infrastructure from the prerequisites is already
> in place (DHCP/TFTP + iPXE, a boot server serving the inspector and the OS
> image, and the controller's callback endpoint reachable from the host network).
> [Quick Start](docs/quick-start.md) walks the first provisioning flow end to end.

**Complete examples:** See [examples/](examples/) directory for full cluster configurations.

## Architecture

Beskar7 consists of three main controllers:

- **PhysicalHost Controller** - Manages BMC connections and power state
- **Beskar7Machine Controller** - Orchestrates provisioning workflow
- **Beskar7Cluster Controller** - Manages cluster-level infrastructure

**Detailed architecture:** See [docs/architecture.md](docs/architecture.md)

## Hardware Compatibility

Designed for **any Redfish-compliant BMC** — one code path, no vendor-specific
handling. Validated against emulated BMCs and the DMTF reference mockup; **no
physical vendor BMC has been validated yet**, so pilot before committing a fleet.

**Details:** See [docs/hardware-compatibility.md](docs/hardware-compatibility.md)

## Documentation

- [Installation](docs/installation.md) - Prerequisites and install steps
- [Quick Start](docs/quick-start.md) - First provisioning flow
- [iPXE Setup Guide](docs/ipxe-setup.md) - Infrastructure setup
- [Architecture](docs/architecture.md) - Technical architecture details
- [API Reference](docs/api-reference.md) - Complete API documentation
- [Examples](examples/) - Working configuration examples
- [Upgrading](docs/upgrading.md) - Version upgrade paths and breaking changes
- [Troubleshooting](docs/troubleshooting.md) - Common issues and solutions
- [Hardware Compatibility](docs/hardware-compatibility.md) - Supported BMCs

## Development

```bash
git clone https://github.com/projectbeskar/beskar7.git
cd beskar7
make build
make test
```

See [docs/ci-cd-and-testing.md](docs/ci-cd-and-testing.md) for complete development guide.

## Contributing

Contributions are welcome — see [CONTRIBUTING.md](CONTRIBUTING.md) for setup, the
checks to run before opening a PR, and the parts of the codebase that carry
non-obvious constraints (the versioned inspector contract, RBAC's three
hand-maintained copies, and CAPI failure semantics).

Found a security issue? Please report it privately — see [SECURITY.md](SECURITY.md).

## License

Apache License 2.0 - See [LICENSE](LICENSE) file for details.

## Support

- **Issues:** https://github.com/projectbeskar/beskar7/issues
- **Discussions:** https://github.com/projectbeskar/beskar7/discussions
- **Documentation:** https://github.com/projectbeskar/beskar7/tree/main/docs

## Acknowledgments

This project was inspired by and learns from:
- [Tinkerbell](https://tinkerbell.org/) - Network boot provisioning
- [Metal³](https://metal3.io/) - Kubernetes bare-metal
- [Cluster API](https://cluster-api.sigs.k8s.io/) - Kubernetes cluster lifecycle

---

**Beskar7** - Simple, reliable bare-metal provisioning for immutable Kubernetes.
