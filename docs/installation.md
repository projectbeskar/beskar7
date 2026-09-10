# Installation

> **Audience:** Operators

This page covers installing Beskar7 into an existing Kubernetes cluster. For building and deploying from source, see [Development setup](development-setup.md).

## Prerequisites

- Kubernetes v1.31+
- Cluster API v1.11+ — the controller reads the `cluster.x-k8s.io/v1beta2` API ([install with clusterctl](https://cluster-api.sigs.k8s.io/user/quick-start.html))
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
helm install beskar7 beskar7/beskar7 \
  --namespace capb7-system --create-namespace
```


**Bootstrap URL.** The chart's default `bootstrap.urlBase` follows the release name and namespace: `https://<release>-controller-manager.<namespace>.svc:8082`, the callback Service the chart creates. Override it when hosts must reach the callback server through another address (see below).

The `bootstrap.urlBase` value is rendered into `PhysicalHost.Status.Bootstrap.URL`. Bare-metal hosts must be able to reach it during PXE boot.

### External reachability

The callback server (inspection POST, bootstrap GET, and the per-host `/boot` endpoint) listens on `:8082` over HTTPS. Bare-metal hosts reach it **while they are still PXE-booting** — before they have joined the cluster network — so the default cluster-internal `…svc:8082` address is not reachable from real hardware, and the manager logs a startup warning when `bootstrap.urlBase` is a `.svc` name.

For a real deployment, two things must line up:

1. **Expose the callback Service externally.** Either set `callback.service.type` to `LoadBalancer` or `NodePort`, or leave it `ClusterIP` and front `:8082` with your own Ingress controller. **Scope this to the provisioning network — do not publish it to the public internet.** The `/boot` endpoint is gated only by an unguessable single-use nonce (rate-limited), so restrict reachability with `loadBalancerSourceRanges` / cloud source-range annotations, a `NetworkPolicy`, or an upstream firewall.
2. **Cover the external address in the serving-cert SAN.** List the external DNS name(s) and/or IP(s) in `callback.externalNames` / `callback.externalIPs`. The host portion of `bootstrap.urlBase` **must** be one of these — the inspector verifies the callback certificate against the CA it is handed on the kernel cmdline and has no insecure-skip path, so a SAN mismatch is a hard failure.

```bash
helm install beskar7 beskar7/beskar7 \
  --namespace capb7-system --create-namespace \
  --set callback.service.type=LoadBalancer \
  --set bootstrap.urlBase=https://beskar7.example.com:8082 \
  --set 'callback.externalNames={beskar7.example.com}'
```

The external SAN is added to both certificate paths: the cert-manager `Certificate` (`certManager.enabled=true`) and the chart's self-signed cert (`certManager.enabled=false`). In self-signed mode the cert is generated once and reused across upgrades — changing `callback.externalNames` / `callback.externalIPs` after the first install does not rotate it; delete the Secret named in `certManager.certificate.secretName` to force regeneration with the new SANs.

## Install via clusterctl

beskar7 implements the [clusterctl provider contract](https://cluster-api.sigs.k8s.io/developer/providers/contracts/clusterctl): every release publishes `infrastructure-components.yaml` and `metadata.yaml`, so `clusterctl init` can install it like any other infrastructure provider, and `clusterctl move` / `clusterctl upgrade` know about it through the provider inventory.

Until beskar7 is in clusterctl's built-in provider list, declare it in `~/.cluster-api/clusterctl.yaml`:

```yaml
providers:
  - name: beskar7
    type: InfrastructureProvider
    url: https://github.com/projectbeskar/beskar7/releases/latest/infrastructure-components.yaml
```

Then initialise the management cluster (clusterctl installs cert-manager and the core providers itself):

```bash
export BESKAR7_BOOTSTRAP_URL_BASE=https://beskar7.example.com:8082   # optional, see below
clusterctl init --infrastructure beskar7
```

The assets exist from `v0.5.0` on; `clusterctl init --infrastructure beskar7:<version>` pins a release.

Variables the components accept (set them in the environment or in the clusterctl config file):

| Variable | Default | Meaning |
|---|---|---|
| `BESKAR7_BOOTSTRAP_URL_BASE` | `https://capb7-controller-manager.capb7-system.svc:8082` | Scheme, host and port bare-metal hosts use to reach the callback server (`--bootstrap-url-base`). Rendered into `PhysicalHost.Status.Bootstrap.URL`. |

A clusterctl install is the kustomize-based install: the `capb7-system` namespace, a `ClusterIP` Service `capb7-controller-manager` for the callback server on `:8082`, and a cert-manager `Certificate` whose SANs cover the webhook and callback Service names. Bare-metal hosts reach the callback server while PXE-booting, so for real hardware either expose that Service (patch it to `LoadBalancer` / `NodePort`, or front it with an Ingress) and set `BESKAR7_BOOTSTRAP_URL_BASE` to the external address before `clusterctl init`, or run a [callback-only manager](ipxe-setup.md#management-cluster-off-the-provisioning-network-a-callback-only-instance) on the provisioning network. The external name must also appear in the serving certificate: edit the `Certificate` `capb7-serving-cert` in `capb7-system` (`spec.dnsNames` / `spec.ipAddresses`) after the install, the way the chart's `callback.externalNames` does it.

To install a build of the current tree instead of a release, publish it into a clusterctl local repository first: `make clusterctl-override VERSION=v0.5.0` writes `~/.cluster-api/overrides/infrastructure-beskar7/v0.5.0/`, and `clusterctl init --infrastructure beskar7:v0.5.0` uses it without touching the network. CI does exactly that on every pull request and runs the smoke suite against the result.

## Install via release manifests

```bash
kubectl apply -f https://github.com/projectbeskar/beskar7/releases/download/v0.4.4/beskar7-manifests-v0.4.4.yaml
```

This applies CRDs, RBAC, and the controller deployment in a single manifest. The release manifest always uses the `capb7-system` namespace and the default `bootstrap.urlBase`. It is the clusterctl components file with the variables above resolved to their defaults; do not `kubectl apply` `infrastructure-components.yaml` itself — it keeps the `${…}` placeholders for `clusterctl init` to fill in.

## `clusterctl move`

With a [clusterctl install](#install-via-clusterctl) the provider inventory and the labels are in place and `clusterctl move` works as for any other provider; the notes below are for Helm and release-manifest installs.

`clusterctl move` builds its object graph only from CRDs that carry the `clusterctl.cluster.x-k8s.io` label. `clusterctl init` adds that label to everything it installs, but neither the Helm chart nor the release manifest goes through `clusterctl init`, so the four beskar7 CRDs carry it themselves, together with `cluster.x-k8s.io/provider: infrastructure-beskar7` (the component label the CAPI provider contract asks for; every other beskar7 component carries the same two). `Beskar7Cluster`, `Beskar7Machine` and `Beskar7MachineTemplate` objects are then moved through their owner references to the `Cluster`, as with any provider. `PhysicalHost` also carries `clusterctl.cluster.x-k8s.io/move-hierarchy`: nothing owns a host, so without it a move would discover the hosts and leave every one of them behind. With it, every `PhysicalHost` in the namespace being moved goes along, together with the bootstrap-token Secret and inspection-result ConfigMap it owns, and is deleted from the source afterwards like any other namespaced object.

Before moving a namespace:

1. **Install beskar7 on the target management cluster first.** A Helm or manifest install does not register beskar7 in clusterctl's provider inventory, so `clusterctl move` cannot check that the target has beskar7 the way it does for providers installed with `clusterctl init`; it simply creates the objects there, which fails if the CRDs are missing.
2. **Bring the BMC credentials along.** The Secret a `PhysicalHost` names in `credentialsSecretRef` (and any `caBundleSecretRef` Secret) is yours, not owned by the host, so the owner-reference walk does not reach it. Either label it so clusterctl moves it, or create it on the target by hand:

   ```bash
   kubectl label secret bmc-credentials -n <namespace> clusterctl.cluster.x-k8s.io/move=""
   ```

3. **On an existing install, re-apply the CRDs first.** `helm upgrade` never touches CRDs, so CRDs installed before the labels were added still lack them; see [Upgrading](upgrading.md).

**Known limitation.** `clusterctl move` pauses each `Cluster` through `spec.paused`, but the beskar7 controllers currently honour only the `cluster.x-k8s.io/paused` annotation, so they keep reconciling on the source during the move — including the deletion path, which powers a host off over Redfish when its `Beskar7Machine` goes away. Until that is fixed, pause the beskar7 objects yourself before the move and unpause them on the target afterwards:

```bash
kubectl annotate -n <namespace> beskar7clusters,beskar7machines --all cluster.x-k8s.io/paused=""
clusterctl move -n <namespace> --to-kubeconfig target.kubeconfig
kubectl --kubeconfig target.kubeconfig annotate -n <namespace> beskar7clusters,beskar7machines --all cluster.x-k8s.io/paused-
```

Moving a live workload cluster has not been exercised end to end yet; rehearse on a lab cluster before relying on it.

## Verify release artifacts (supply chain)

Container images are signed with [cosign](https://docs.sigstore.dev/) using keyless
(OIDC) signing bound to the release workflow's identity — there is no long-lived
signing key. The controller image also carries a signed SPDX SBOM attestation.

> **Applies from `v0.4.0-alpha.9` onward** (including `v0.4.0`). Signing was
> enabled after `v0.4.0-alpha.8` was published, so that and earlier alpha images
> carry no signature and will not verify — expected, not a tampering signal.

Verify an image before deploying it:

```bash
IMAGE=ghcr.io/projectbeskar/beskar7/beskar7:<tag>   # a release from alpha.9 onward

cosign verify "$IMAGE" \
  --certificate-identity-regexp '^https://github.com/projectbeskar/beskar7/\.github/workflows/release\.yml@refs/tags/.*$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

Inspect the SBOM attestation:

```bash
cosign verify-attestation --type spdxjson "$IMAGE" \
  --certificate-identity-regexp '^https://github.com/projectbeskar/beskar7/\.github/workflows/release\.yml@refs/tags/.*$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

A failed verification means the image was not produced by this project's release
workflow — do not deploy it.

> The `beskar7-inspector` boot artifacts (`vmlinuz`, `initrd.img`) ship with
> `sha256sums.txt` on each inspector release; verify them with
> `sha256sum -c sha256sums.txt --ignore-missing`.

## Verify the installation

```bash
kubectl get pods -n capb7-system
```

The controller manager pod should reach `Running` status within a minute. The webhook service and a self-signed certificate are also created:

```bash
kubectl get certificate -n capb7-system
kubectl get svc -n capb7-system
```

## Next steps

- [Quick Start](quick-start.md) — create your first `PhysicalHost` and `Beskar7Machine`.
- [iPXE Setup](ipxe-setup.md) — configure your DHCP and HTTP boot server.
- [Concepts](introduction.md) — understand the four CRD kinds.
