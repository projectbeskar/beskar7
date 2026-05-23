# Installation

> **Audience:** Operators

This page covers installing Beskar7 into an existing Kubernetes cluster. For building and deploying from source, see [Development setup](development-setup.md).

## Prerequisites

- Kubernetes v1.31+
- Cluster API v1.10+ ([install with clusterctl](https://cluster-api.sigs.k8s.io/user/quick-start.html))
- cert-manager v1.16+ ([installation guide](https://cert-manager.io/docs/installation/))
- iPXE infrastructure — DHCP server + HTTP boot server accessible by bare-metal hosts ([setup guide](ipxe-setup.md))
- `beskar7-inspector` image hosted on your boot server ([inspector repository](https://github.com/projectbeskar/beskar7-inspector))

### Install cert-manager

Beskar7 uses cert-manager to provision TLS certificates for the webhook admission endpoint and the inspection/bootstrap callback server. Install it before Beskar7:

```bash
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.16.2/cert-manager.crds.yaml
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.16.2/cert-manager.yaml
```

Wait for cert-manager pods to be running before proceeding:

```bash
kubectl get pods -n cert-manager
```

## Install via Helm (recommended)

```bash
helm repo add beskar7 https://projectbeskar.github.io/beskar7
helm repo update
helm install --devel beskar7 beskar7/beskar7 \
  --namespace beskar7-system --create-namespace
```

The `--devel` flag is required while the chart version is a SemVer pre-release (`0.4.0-alpha.*`). Drop it once a non-prerelease tag is published.

**Release name and bootstrap URL.** The chart's default `bootstrap.urlBase` is `https://beskar7-controller-manager.beskar7-system.svc:8082`, which matches the Service name when the Helm release is named `beskar7`. If you install with a different release name, pass the matching URL:

```bash
helm install --devel my-release beskar7/beskar7 \
  --namespace beskar7-system --create-namespace \
  --set bootstrap.urlBase=https://my-release-controller-manager.beskar7-system.svc:8082
```

The `bootstrap.urlBase` value is rendered into `PhysicalHost.Status.Bootstrap.URL`. Bare-metal hosts must be able to reach it during PXE boot.

## Install via release manifests

```bash
kubectl apply -f https://github.com/projectbeskar/beskar7/releases/download/v0.4.0-alpha.4/beskar7-manifests-v0.4.0-alpha.4.yaml
```

This applies CRDs, RBAC, and the controller deployment in a single manifest. The release manifest always uses the `beskar7-system` namespace and the default `bootstrap.urlBase`.

## Verify the installation

```bash
kubectl get pods -n beskar7-system
```

The controller manager pod should reach `Running` status within a minute. The webhook service and a self-signed certificate are also created:

```bash
kubectl get certificate -n beskar7-system
kubectl get svc -n beskar7-system
```

## Next steps

- [Quick Start](quick-start.md) — create your first `PhysicalHost` and `Beskar7Machine`.
- [iPXE Setup](ipxe-setup.md) — configure your DHCP and HTTP boot server.
- [Concepts](introduction.md) — understand the four CRD kinds.
