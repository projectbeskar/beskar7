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

### External reachability

The callback server (inspection POST, bootstrap GET, and the per-host `/boot` endpoint) listens on `:8082` over HTTPS. Bare-metal hosts reach it **while they are still PXE-booting** — before they have joined the cluster network — so the default cluster-internal `…svc:8082` address is not reachable from real hardware, and the manager logs a startup warning when `bootstrap.urlBase` is a `.svc` name.

For a real deployment, two things must line up:

1. **Expose the callback Service externally.** Either set `callback.service.type` to `LoadBalancer` or `NodePort`, or leave it `ClusterIP` and front `:8082` with your own Ingress controller. **Scope this to the provisioning network — do not publish it to the public internet.** The `/boot` endpoint is gated only by an unguessable single-use nonce (rate-limited), so restrict reachability with `loadBalancerSourceRanges` / cloud source-range annotations, a `NetworkPolicy`, or an upstream firewall.
2. **Cover the external address in the serving-cert SAN.** List the external DNS name(s) and/or IP(s) in `callback.externalNames` / `callback.externalIPs`. The host portion of `bootstrap.urlBase` **must** be one of these — the inspector verifies the callback certificate against the CA it is handed on the kernel cmdline and has no insecure-skip path, so a SAN mismatch is a hard failure.

```bash
helm install --devel beskar7 beskar7/beskar7 \
  --namespace beskar7-system --create-namespace \
  --set callback.service.type=LoadBalancer \
  --set bootstrap.urlBase=https://beskar7.example.com:8082 \
  --set 'callback.externalNames={beskar7.example.com}'
```

The external SAN is added to both certificate paths: the cert-manager `Certificate` (`certManager.enabled=true`) and the chart's self-signed cert (`certManager.enabled=false`). In self-signed mode the cert is generated once and reused across upgrades — changing `callback.externalNames` / `callback.externalIPs` after the first install does not rotate it; delete the Secret named in `certManager.certificate.secretName` to force regeneration with the new SANs.

## Install via release manifests

```bash
kubectl apply -f https://github.com/projectbeskar/beskar7/releases/download/v0.4.0-alpha.6/beskar7-manifests-v0.4.0-alpha.6.yaml
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
