# Development Setup

> **Audience:** Developers

This page covers building Beskar7 from source and deploying it into a development cluster. For installing a released version, see [Installation](installation.md).

## Prerequisites

- Go 1.25 (matches `go.mod` and the CI toolchain in `.github/workflows/ci.yml`)
- Docker with `buildx` configured for multi-arch builds:
  ```bash
  docker buildx create --use
  ```
- Access to a container registry your cluster can pull from (ghcr.io, Docker Hub, a local registry, etc.)
- A running Kubernetes cluster with `kubectl` configured (kind, minikube, or remote)
- controller-gen and kustomize (installed via `make`)

## Clone and install tools

```bash
git clone https://github.com/projectbeskar/beskar7.git
cd beskar7
make install-controller-gen
```

kustomize is pulled by the Makefile when needed.

## Build and push the manager image

```bash
make docker-build docker-push IMG=my-registry/my-repo:dev
```

The default `IMG` value in the Makefile is `ghcr.io/projectbeskar/beskar7/beskar7:v0.4.0-alpha.4`. Override it to push to your own registry.

## Regenerate CRDs and RBAC

Run this after any change to `api/v1beta1/` or any `+kubebuilder:` marker:

```bash
make manifests
```

This regenerates `config/crd/bases/*.yaml`, `config/rbac/role.yaml`, and `api/v1beta1/zz_generated.deepcopy.go`. Also run `make sync-chart-crds` to keep `charts/beskar7/crds/` in sync.

## Run tests

```bash
make test
```

Unit tests run with `-race`. Integration tests require `-tags integration` and a reachable cluster — see [CI/CD and Testing](ci-cd-and-testing.md) for the full suite.

## Install CRDs into the cluster

```bash
make install
```

This applies the CRD manifests from `config/crd/bases/` via kustomize.

## Deploy the controller

```bash
make deploy IMG=my-registry/my-repo:dev
```

This applies the full kustomize overlay including RBAC, the deployment, and the webhook configuration. Ensure cert-manager is installed first (see [Installation](installation.md#prerequisites)).

## Verify

```bash
kubectl get pods -n beskar7-system
```

## See also

- [CI/CD and Testing](ci-cd-and-testing.md) — unit/integration/lint pipeline and release workflow.
- [Architecture](architecture.md) — controller design and reconcile flow.
