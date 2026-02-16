# Beskar7 Documentation

Welcome to the Beskar7 documentation! This directory contains comprehensive documentation for the Beskar7 Cluster API infrastructure provider.

## Getting Started

- [**Introduction**](introduction.md) - Overview of Beskar7 and its purpose
- [**Quick Start Guide**](quick-start.md) - Get Beskar7 up and running quickly
- [**🚀 NEW: Quick Start - Vendor Support**](quick-start-vendor-support.md) - Get started with automatic vendor detection
- [**Architecture**](architecture.md) - Understand how Beskar7 components work together

## API Documentation

- [**API Reference**](api-reference.md) - Complete reference for all Beskar7 CRDs
- [**PhysicalHost**](physicalhost.md) - Detailed documentation for PhysicalHost resources
- [**Beskar7Machine**](beskar7machine.md) - Detailed documentation for Beskar7Machine resources
- [**Beskar7Cluster**](beskar7cluster.md) - Detailed documentation for Beskar7Cluster resources
- [**Beskar7MachineTemplate**](beskar7machinetemplate.md) - Detailed documentation for template resources

## Deployment and Operations

- [**Deployment Best Practices**](deployment-best-practices.md) - Production deployment guidelines
- [**Advanced Usage**](advanced-usage.md) - Advanced configuration and usage scenarios
- [**🚀 NEW: State Management**](state-management.md) - State machine operations and recovery
- [**Troubleshooting**](troubleshooting.md) - Common issues and solutions

## Hardware and Compatibility

- [**Hardware Compatibility Matrix**](hardware-compatibility.md) - Vendor support and compatibility information
- [**🚀 NEW: Vendor-Specific Support**](vendor-specific-support.md) - Automatic vendor detection and configuration

## Monitoring and Observability

- [**Metrics**](metrics.md) - Available metrics and monitoring setup

## Documentation Organization

### For New Users
1. Start with [Introduction](introduction.md) to understand Beskar7's purpose
2. **NEW:** Check out [Quick Start - Vendor Support](quick-start-vendor-support.md) for automatic hardware support
3. Follow the [Quick Start Guide](quick-start.md) to deploy your first setup
4. Review [Hardware Compatibility](hardware-compatibility.md) for your specific hardware

### For Operators
1. Review [Deployment Best Practices](deployment-best-practices.md) for production deployments
2. **NEW:** Understand [State Management](state-management.md) for operational procedures
3. Set up monitoring using [Metrics](metrics.md) documentation
4. Familiarize yourself with [Troubleshooting](troubleshooting.md) procedures

### For Developers
1. Understand the [Architecture](architecture.md) and component interactions
2. Use the [API Reference](api-reference.md) for comprehensive field documentation
3. Explore [Advanced Usage](advanced-usage.md) for complex scenarios

## Key Concepts

### Resources
- **PhysicalHost**: Represents a physical server manageable via Redfish
- **Beskar7Machine**: Infrastructure provider for CAPI Machine resources
- **Beskar7Cluster**: Infrastructure provider for CAPI Cluster resources
- **Beskar7MachineTemplate**: Template for creating machine configurations

### Provisioning Modes
- **RemoteConfig**: Boot generic ISO with remote configuration URL (requires configURL)
- **PreBakedISO**: Boot pre-configured ISO with embedded settings
- **PXE**: Network boot using PXE (requires external PXE infrastructure)
- **iPXE**: Network boot using iPXE (requires external iPXE infrastructure)

### Supported Operating Systems
The following immutable OS families are currently supported with full RemoteConfig capabilities:
- **Kairos** (recommended) - Cloud-native, immutable OS with built-in cluster provisioning
- **Flatcar Container Linux** - Minimal, secure container-optimized OS
- **openSUSE Leap Micro** - Lightweight immutable OS for containers and edge computing

## Hardware Support

Beskar7 works with any Redfish-compliant BMC. See the [Hardware Compatibility Matrix](hardware-compatibility.md) for vendor-specific information and known limitations.

**Tested Vendors:**
- Dell Technologies (iDRAC)
- HPE (iLO)
- Lenovo (XCC)
- Supermicro (BMC)

## Contributing

For information about contributing to Beskar7, see the main repository documentation.

## Support

- **GitHub Issues**: https://github.com/projectbeskar/beskar7/issues
- **Documentation Issues**: Report problems with this documentation as GitHub issues

## Document Status

| Document | Status | Last Updated |
|----------|--------|--------------|
| Introduction | Yes Complete | Current |
| Quick Start Guide | Yes Complete | Current |
| **🚀 Quick Start - Vendor Support** | **Yes NEW** | **Current** |
| **🚀 Vendor-Specific Support** | **Yes NEW** | **Current** |
| Architecture | Yes Complete | Current |
| API Reference | Yes Complete | Current |
| PhysicalHost | Yes Complete | Current |
| Beskar7Machine | Yes Complete | Current |
| Beskar7Cluster | Yes Complete | Current |
| Beskar7MachineTemplate | Yes Complete | Current |
| Deployment Best Practices | Yes Complete | Current |
| Advanced Usage | Yes Complete | Current |
| Hardware Compatibility | Yes Complete | Current |
| Troubleshooting | Yes Complete | Current |
| **🚀 State Management** | **Yes NEW** | **Current** |
| Metrics | Yes Complete | Current |

All documentation is current and comprehensive as of the latest release. 