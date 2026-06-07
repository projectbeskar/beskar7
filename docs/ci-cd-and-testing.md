# CI/CD Pipeline and Hardware Emulation Testing

> **Audience:** Developers

This document describes the comprehensive CI/CD pipeline and hardware emulation testing framework for Beskar7, enabling development and testing without access to real BMC hardware.

## CI/CD Pipeline Overview

Beskar7 uses GitHub Actions for automated testing, building, and releasing. The pipeline consists of several workflows designed to ensure code quality and release readiness.

### Main Workflows

#### 1. CI Workflow (`.github/workflows/ci.yml`)
Runs on every push and pull request to `main` and `develop` branches.

**Jobs:**
- **Lint and Code Quality**: Go linting, formatting checks, and code quality analysis
- **Security Scanning**: Vulnerability scanning with Gosec and Trivy
- **Unit Tests**: Multi-version Go testing with coverage reporting
- **Integration Tests**: Full integration test suite
- **Container Build**: Multi-arch Docker image building and security scanning
- **Manifest Validation**: CRD and Kubernetes manifest validation
- **E2E Setup Validation**: End-to-end deployment testing in kind cluster
- **Performance Benchmarks**: Automated benchmark testing

#### 2. Release Workflow (`.github/workflows/release.yml`)
Triggered on Git tags starting with `v*`.

**Jobs:**
- **Container Image Building**: Multi-arch builds with SBOM generation
- **Security Scanning**: Container image vulnerability assessment
- **Release Artifact Generation**: Kubernetes manifests and checksums
- **Helm Chart Creation**: Automated Helm chart packaging
- **GitHub Release**: Automated release creation with artifacts
- **Documentation Updates**: Version updates in documentation

### Pipeline Features

Yes **Multi-arch builds** (linux/amd64, linux/arm64)
Yes **Security scanning** with SARIF reports
Yes **Coverage reporting** to Codecov
Yes **Performance benchmarking**
Yes **Helm chart automation**
Yes **SBOM generation** for supply chain security
Yes **Automated releases** with proper versioning

## Hardware Emulation Testing Framework

Since real BMC hardware isn't always available, Beskar7 includes a comprehensive hardware emulation framework that simulates different vendor BMCs and failure scenarios.

### Mock Redfish Server

The `MockRedfishServer` simulates realistic BMC behavior:

```go
// Create a Dell server emulation
mockServer := NewMockRedfishServer(VendorDell)
defer mockServer.Close()

// Configure failure scenarios
mockServer.SetFailureMode(FailureConfig{
    NetworkErrors: true,
    SlowResponses: true,
})

// Use in tests
client := createRedfishClient(mockServer.GetURL(), "admin", "password123")
systemInfo, err := client.GetSystemInfo(ctx)
```

### Supported Vendor Emulations

#### Dell PowerEdge
- Manufacturer: "Dell Inc."
- Model: "PowerEdge R750"
- BIOS Attributes: `KernelArgs`, `BootMode`, `SecureBoot`
- Vendor-specific boot parameter handling

#### HPE ProLiant
- Manufacturer: "HPE"
- Model: "ProLiant DL380 Gen10"
- BIOS Attributes: `UefiOptimizedBoot`, `BootOrderPolicy`
- UEFI target boot override support

#### Lenovo ThinkSystem
- Manufacturer: "Lenovo"
- Model: "ThinkSystem SR650"
- BIOS Attributes: `SystemBootSequence`, `SecureBootEnable`
- Intelligent BIOS fallback mechanisms

#### Supermicro
- Manufacturer: "Supermicro"
- Model: "X12DPi-NT6"
- BIOS Attributes: `BootFeature`, `QuietBoot`
- Multiple fallback mechanisms

### Failure Scenario Testing

The emulation framework supports various failure modes:

```go
// Network connectivity failures
failureConfig := FailureConfig{
    NetworkErrors: true,
}

// Authentication failures
failureConfig := FailureConfig{
    AuthFailures: true,
}

// Slow response simulation
failureConfig := FailureConfig{
    SlowResponses: true, // 5-second delays
}

// Power operation failures
failureConfig := FailureConfig{
    PowerFailures: true,
}

// Virtual media failures
failureConfig := FailureConfig{
    MediaFailures: true,
}
```

### Integration Testing

Run hardware emulation tests:

```bash
# Run all emulation tests (mock BMC, no real hardware)
go test -v -tags=integration ./test/emulation/...

# Run specific vendor tests
go test -v -tags=integration ./test/emulation/... -run TestDellEmulation

# Run failure scenario tests
go test -v -tags=integration ./test/emulation/... -run TestFailureScenarios

# Run stress tests
go test -v -tags=integration ./test/emulation/... -run TestStressTesting
```

## Performance Testing & Benchmarks

### Running Benchmarks

```bash
# Run all benchmarks
go test -bench=. -benchmem ./...

# Run controller benchmarks
go test -bench=. -benchmem ./controllers/

# Run Redfish client benchmarks
go test -bench=. -benchmem ./internal/redfish/

# Generate benchmark comparison
go test -bench=. -benchmem ./... > benchmark-before.txt
# Make changes...
go test -bench=. -benchmem ./... > benchmark-after.txt
benchcmp benchmark-before.txt benchmark-after.txt
```

### Performance Targets

| Operation | Target | Current |
|-----------|--------|---------|
| Host Claim | < 100ms | Measured in CI |
| Concurrent Claims (10) | < 500ms | Measured in CI |
| Queue Operations | < 1ms | Measured in CI |
| Leader Election Check | < 10ms | Measured in CI |

## Code Quality & Security

### Linting Configuration

The project uses `golangci-lint` with comprehensive rules:

```bash
# Run linting locally
golangci-lint run

# Run with verbose output
golangci-lint run --verbose

# Auto-fix issues where possible
golangci-lint run --fix
```

### Security Scanning

Multiple security tools are integrated:

- **Gosec**: Go security checker
- **Trivy**: Vulnerability scanner for code and containers
- **CodeQL**: GitHub's semantic code analysis

```bash
# Run security scan locally
gosec ./...

# Scan container images
trivy image ghcr.io/projectbeskar/beskar7/beskar7:latest
```

## Development Workflow

### Local Development

1. **Setup Environment**
   ```bash
   # Install development dependencies
   make install-controller-gen
   go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
   ```

2. **Run Tests**
   ```bash
   # Unit tests
   make test
   
   # Integration tests with emulation
   go test -v -tags=integration ./test/integration/...
   go test -v -tags=integration ./test/emulation/...
   
   # Benchmarks
   go test -bench=. -benchmem ./...
   ```

3. **Code Quality Checks**
   ```bash
   # Linting
   golangci-lint run
   
   # Security scan
   gosec ./...
   
   # Generate manifests
   make manifests
   ```

4. **Local Container Build**
   ```bash
   # Build container
   make docker-build
   
   # Test container locally
  docker run --rm ghcr.io/projectbeskar/beskar7/beskar7:${VERSION}
   ```

### Pull Request Workflow

1. **Create Feature Branch**
   ```bash
   git checkout -b feature/my-feature
   ```

2. **Make Changes** and ensure all tests pass locally

3. **Push Branch** - CI pipeline automatically runs:
   - Code quality checks
   - Security scanning
   - Unit and integration tests
   - Container builds
   - Benchmark comparisons

4. **Review Process** - Automated checks must pass before merge

### Release Process

1. **Create Release Tag** — the current release line is `v0.4.0-alpha.N`; bump N for each release. Use a `vX.Y.Z` (or `vX.Y.Z-pre`) format the `release.yml` workflow recognises.
   ```bash
   git tag v0.4.0-alpha.7
   git push origin v0.4.0-alpha.7
   ```

2. **Automated Release** - GitHub Actions automatically:
   - Builds and pushes container images
   - Generates Kubernetes manifests
   - Creates Helm charts
   - Publishes GitHub release
   - Updates documentation

## Testing Without Real Hardware

### Complete Testing Strategy

The emulation framework enables comprehensive testing without real BMCs:

1. **Unit Tests**: Test individual components with mocks
2. **Integration Tests**: Test component interactions with emulated BMCs
3. **Vendor-Specific Tests**: Test vendor quirks and behaviors
4. **Failure Scenario Tests**: Test error handling and recovery
5. **Performance Tests**: Benchmark operations under load
6. **End-to-End Tests**: Full workflow testing in kind clusters

### Example: Testing Dell-Specific Behavior

```go
func TestDellKernelArgsHandling(t *testing.T) {
    // Create Dell server emulation
    mockServer := NewMockRedfishServer(VendorDell)
    defer mockServer.Close()
    
    // Test Dell-specific BIOS attribute handling
    client := createRedfishClient(mockServer.GetURL(), "admin", "password")
    
    // This would test the actual vendor-specific logic
    err := client.SetBootParameters(ctx, []string{"config_url=http://example.com/config.yaml"})
    assert.NoError(t, err)
    
    // Verify Dell uses BIOS attributes instead of UEFI boot override
    logs := mockServer.GetRequestLog()
    assert.Contains(t, logs, "BIOS")
    assert.Contains(t, logs, "KernelArgs")
}
```

### Testing Failure Scenarios

```go
func TestNetworkFailureRecovery(t *testing.T) {
    mockServer := NewMockRedfishServer(VendorGeneric)
    defer mockServer.Close()
    
    // Simulate network failures
    mockServer.SetFailureMode(FailureConfig{
        NetworkErrors: true,
    })
    
    // Test that controller handles failures gracefully
    reconciler := &PhysicalHostReconciler{
        RedfishClientFactory: createMockClientFactory(mockServer),
    }
    
    result, err := reconciler.Reconcile(ctx, reconcileRequest)
    assert.Error(t, err)
    assert.True(t, result.Requeue) // Should requeue for retry
}
```

## Monitoring & Observability

### CI/CD Metrics

The pipeline tracks:
- **Build Success Rate**: Percentage of successful builds
- **Test Coverage**: Code coverage trends over time  
- **Performance Regression**: Benchmark comparison across releases
- **Security Vulnerabilities**: New vulnerabilities introduced
- **Deployment Success**: Release deployment success rate

### Integration with External Services

- **Codecov**: Code coverage reporting and trends
- **GitHub Security**: SARIF report integration
- **Container Registry**: Automated image scanning
- **Dependabot**: Automated dependency updates

## Configuration & Customization

### CI/CD Configuration

Key configuration files:
- `.github/workflows/ci.yml`: Main CI pipeline
- `.github/workflows/release.yml`: Release automation
- `.golangci.yml`: Code quality rules
- `Dockerfile`: Container build configuration

### Emulation Configuration

Customize emulation behavior:
- Vendor-specific configurations
- Failure scenario parameters
- Performance characteristics
- Authentication requirements

### Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `REGISTRY` | Container registry | `ghcr.io` |
| `IMAGE_NAME` | Image repository | `${{ github.repository }}` |
| `GO_VERSION` | Go version for builds | `1.24` |
| `RECONCILE_TIMEOUT` | Max reconciliation duration | `2m` |
| `STUCK_STATE_TIMEOUT` | Stuck state detection timeout | `15m` |
| `MAX_RETRIES` | Guarded state transition retries | `3` |
| `KUBEBUILDER_VERSION` | Kubebuilder version | `latest` |

## Best Practices

### Development
- Yes Write tests first (TDD approach)
- Yes Use emulation for development without real hardware
- Yes Run full test suite before committing
- Yes Keep benchmarks for performance-critical code

### Testing
- Yes Test vendor-specific behaviors separately
- Yes Include failure scenario testing
- Yes Use realistic data in tests
- Yes Test concurrent operations

### CI/CD
- Yes Keep builds fast (< 10 minutes)
- Yes Fail fast on critical errors
- Yes Generate comprehensive reports
- Yes Automate security scanning

### Releases
- Yes Use semantic versioning
- Yes Generate detailed changelogs
- Yes Include security assessments
- Yes Test upgrade paths

## Troubleshooting

### Common CI Issues

1. **Test Timeouts**
   ```bash
   # Increase timeout in test
   go test -timeout=30m ./test/integration/...
   ```

2. **Coverage Failures**
   ```bash
   # Check coverage locally
   go test -coverprofile=coverage.out ./...
   go tool cover -html=coverage.out
   ```

3. **Linting Errors**
   ```bash
   # Fix auto-fixable issues
   golangci-lint run --fix
   
   # Disable specific rules if needed
   //nolint:gosec
   ```

### Emulation Issues

1. **Mock Server Connection**
   ```go
   // Ensure TLS verification is disabled for test servers
   client := createRedfishClient(mockServer.GetURL(), "admin", "password")
   ```

2. **Vendor Behavior**
   ```go
   // Verify vendor-specific initialization
   assert.Equal(t, VendorDell, mockServer.vendor)
   assert.Contains(t, mockServer.biosAttributes, "KernelArgs")
   ```

This comprehensive CI/CD and testing framework enables confident development and deployment of Beskar7 without requiring access to real BMC hardware, while maintaining production-ready quality standards. 