# Introduction to Beskar7

Welcome to Beskar7, a Cluster API (CAPI) Infrastructure Provider designed for managing bare-metal Kubernetes clusters using the Redfish standard.

## What is Beskar7?

Beskar7 acts as a bridge between the declarative Kubernetes API and the physical hardware managed via Redfish BMCs (Baseboard Management Controllers). It allows you to:

*   Define your bare-metal infrastructure (`PhysicalHost` resources) within Kubernetes.
*   Provision Kubernetes nodes onto these physical hosts using `Beskar7Machine` resources, which integrate with CAPI `Machine` objects.
*   Boot nodes via iPXE network boot to a hardware-inspection image, validate the report against operator-supplied hardware requirements, then kexec into the target OS (Kairos, Flatcar, or any image you host).
*   Orchestrate cluster-level infrastructure using `Beskar7Cluster` resources.

Beskar7 uses only universally-supported Redfish features (power state, one-time PXE boot, system info, network interface enumeration). It does not rely on Virtual Media, BIOS attribute editing, or vendor-specific Redfish extensions.

## Why Beskar7?

Managing bare-metal infrastructure traditionally involves manual steps or separate automation tools. Beskar7 brings this management into the Kubernetes ecosystem, enabling consistent, declarative lifecycle management using familiar CAPI workflows.

## Target Audience

This project is primarily aimed at platform administrators, SREs, and DevOps teams who manage Kubernetes clusters running directly on physical hardware and utilize servers with Redfish-compliant BMCs.

## Current Status

Beskar7 is currently in the **Alpha** stage. Core functionality is being developed, and APIs or implementation details may change. It is not yet recommended for production deployments.

## Next Steps

### For New Users
1. **[Quick Start Guide](./quick-start.md)** - Get Beskar7 built and deployed
2. **[Hardware Compatibility](./hardware-compatibility.md)** - Check your hardware compatibility
3. **[API Reference](./api-reference.md)** - Learn about the available resources

### For Operators
1. **[Deployment Best Practices](./deployment-best-practices.md)** - Production deployment guidance
2. **[Troubleshooting](./troubleshooting.md)** - Common issues and solutions
3. **[Metrics](./metrics.md)** - Monitoring and observability

### For Developers
1. **[Architecture](./architecture.md)** - Understand the components and how they interact
2. **[Advanced Usage](./advanced-usage.md)** - Advanced configuration scenarios
3. **[API Reference](./api-reference.md)** - Complete API documentation

### Complete Documentation
See **[Documentation Index](./README.md)** for a complete overview of all available documentation. 