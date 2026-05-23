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
8. Kexecs into target OS
9. Machine joins cluster

## Current Status

**Version:** v0.4.0-alpha.4  
**Status:** Alpha - Under active development. API may still change before v0.4.0 GA.  
**Breaking Changes:** v0.4.0 is NOT compatible with v0.3.x ([see CHANGELOG](CHANGELOG.md))

## Installation

### Prerequisites

1. Kubernetes v1.31+ with kubectl configured
2. Cluster API v1.10+ ([install with clusterctl](https://cluster-api.sigs.k8s.io/user/quick-start.html))
3. cert-manager v1.16+ ([installation guide](https://cert-manager.io/docs/installation/))
4. iPXE infrastructure - DHCP + HTTP server ([setup guide](docs/ipxe-setup.md))
5. Inspection image - beskar7-inspector ([repository](https://github.com/projectbeskar/beskar7-inspector))

### Quick Install

**Using Helm (Recommended):**

```bash
helm repo add beskar7 https://projectbeskar.github.io/beskar7
helm repo update
helm install --devel beskar7 beskar7/beskar7 \
  --namespace beskar7-system --create-namespace
```

The `--devel` flag is required while the chart version is a SemVer pre-release (`0.4.0-alpha.4`); drop it once a non-prerelease tag is cut.

**Using Release Manifests:**

```bash
kubectl apply -f https://github.com/projectbeskar/beskar7/releases/download/v0.4.0-alpha.4/beskar7-manifests-v0.4.0-alpha.4.yaml
```

See [Quick Start Guide](docs/quick-start.md) for detailed installation instructions.

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
  inspectionImageURL: "http://boot-server/beskar7-inspector/boot"
  targetImageURL: "http://boot-server/kairos/v2.8.1.tar.gz"
  hardwareRequirements:
    minCPUCores: 4
    minMemoryGB: 16
```

**Complete examples:** See [examples/](examples/) directory for full cluster configurations.

## Architecture

Beskar7 consists of three main controllers:

- **PhysicalHost Controller** - Manages BMC connections and power state
- **Beskar7Machine Controller** - Orchestrates provisioning workflow
- **Beskar7Cluster Controller** - Manages cluster-level infrastructure

**Detailed architecture:** See [docs/architecture.md](docs/architecture.md)

## Hardware Compatibility

Works with **any Redfish-compliant BMC**. Tested with Dell, HPE, Lenovo, Supermicro, and generic BMCs.

**Details:** See [docs/hardware-compatibility.md](docs/hardware-compatibility.md)

## Documentation

- [Quick Start Guide](docs/quick-start.md) - Step-by-step getting started
- [iPXE Setup Guide](docs/ipxe-setup.md) - Infrastructure setup
- [Architecture](docs/architecture.md) - Technical architecture details
- [API Reference](docs/api-reference.md) - Complete API documentation
- [Examples](examples/) - Working configuration examples
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

Contributions are welcome! Please:
1. Open an issue to discuss major changes
2. Follow existing code style
3. Add tests for new features
4. Update documentation

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
