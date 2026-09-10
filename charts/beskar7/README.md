# beskar7

> **Audience:** Operators

A Helm chart for Beskar7 — Cluster API Infrastructure Provider for Immutable Bare Metal.

Beskar7 provisions bare-metal Kubernetes nodes via Redfish power management, iPXE network boot, and a hardware inspection image. It implements the [Cluster API infrastructure provider contract](https://cluster-api.sigs.k8s.io/developer/providers/contracts/overview).

## Prerequisites

- Kubernetes v1.31+ ([docs](https://kubernetes.io/docs/setup/))
- Cluster API v1.10+ ([install with clusterctl](https://cluster-api.sigs.k8s.io/user/quick-start.html))
- cert-manager v1.16+ ([installation guide](https://cert-manager.io/docs/installation/)) — **recommended but optional** (see TLS section below)

### TLS certificates

The webhook server and the inspection/bootstrap callback server both need a
serving certificate. Two modes:

- **`certManager.enabled=true` (default)** — cert-manager issues the cert and
  injects the CA into the webhook configs. Install cert-manager first:

  ```bash
  kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.16.2/cert-manager.crds.yaml
  kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.16.2/cert-manager.yaml
  ```

- **`certManager.enabled=false`** — the chart self-generates a self-signed
  serving cert and writes the matching CA into the webhook `caBundle`. No
  cert-manager required. The cert is reused across upgrades and valid 10 years.
  Use this on clusters where you don't run cert-manager:

  ```bash
  helm install beskar7 beskar7/beskar7 \
    --namespace capb7-system --create-namespace \
    --set certManager.enabled=false
  ```

## Installation

```bash
helm repo add beskar7 https://projectbeskar.github.io/beskar7
helm repo update
helm install beskar7 beskar7/beskar7 \
  --namespace capb7-system --create-namespace
```


**Bootstrap URL.** The default `bootstrap.urlBase` follows the release name and namespace: `https://<release>-controller-manager.<namespace>.svc:8082`, the callback Service this chart creates. Set it explicitly when hosts reach the callback server through another address.

Bare-metal hosts must be able to reach the `bootstrap.urlBase` during PXE boot. It is rendered into `PhysicalHost.Status.Bootstrap.URL` for each provisioned host.

**Callback-only instance.** When the management cluster has no interface on the provisioning network, nothing the chart can expose is reachable from a PXE-booting host. The supported answer is a second copy of the manager binary on a machine that is on that network, started with `--controllers=none`: it serves the callback endpoints and registers no reconciler or webhook, so it does not compete with this release's controllers. The chart does not deploy that instance (it needs a kubeconfig for this cluster, which the in-cluster Deployment does not use), but three values of this release must line up with it:

- `bootstrap.urlBase` — the callback-only instance's external address, e.g. `https://192.0.2.10:8082`. The in-cluster controller writes it into `PhysicalHost.Status.Bootstrap.URL`; start the callback-only instance with the same `--bootstrap-url-base`.
- `callback.externalIPs` / `callback.externalNames` — include that address so the serving cert in `certManager.certificate.secretName` covers it; the callback-only instance can then use a copy of that Secret as its `--inspection-cert-dir`.
- `watchNamespaces` — give the callback-only instance the same list (or, if empty here, cluster-wide read access).

Give it a kubeconfig whose identity holds the manager's RBAC; a token for the chart's ServiceAccount is the simplest. `callback.service.type` can stay `ClusterIP`, since hosts never talk to the in-cluster instance in this topology. Walk-through: [docs/ipxe-setup.md](../../docs/ipxe-setup.md#management-cluster-off-the-provisioning-network-a-callback-only-instance).

## Configuration

All configurable values with their defaults:

| Key | Default | Description |
|---|---|---|
| `controllerManager.replicas` | `1` | Number of controller-manager replicas. |
| `controllerManager.image.repository` | `ghcr.io/projectbeskar/beskar7/beskar7` | Manager image repository. |
| `controllerManager.image.tag` | `""` (uses `Chart.appVersion`) | Image tag. Empty string defers to `Chart.appVersion`. |
| `controllerManager.image.pullPolicy` | `IfNotPresent` | Image pull policy. |
| `controllerManager.resources.limits.cpu` | `500m` | CPU limit for the manager container. |
| `controllerManager.resources.limits.memory` | `128Mi` | Memory limit for the manager container. |
| `controllerManager.resources.requests.cpu` | `10m` | CPU request for the manager container. |
| `controllerManager.resources.requests.memory` | `64Mi` | Memory request for the manager container. |
| `controllerManager.env` | `[]` | Additional environment variables for the manager container. |
| `imagePullSecrets` | `[]` | Image pull secrets for the manager pod. |
| `nameOverride` | `""` | Override the chart name component of generated resource names. |
| `fullnameOverride` | `""` | Override the full generated resource name prefix. |
| `serviceAccount.create` | `true` | Create a ServiceAccount for the manager. |
| `serviceAccount.annotations` | `{}` | Annotations to add to the ServiceAccount. |
| `serviceAccount.name` | `""` | ServiceAccount name; generated from fullname template if empty. |
| `podAnnotations` | `{}` | Annotations to add to the manager pod. |
| `podSecurityContext.runAsNonRoot` | `true` | Run the pod as a non-root user. |
| `podSecurityContext.runAsUser` | `65532` | UID for the manager process (matches distroless nonroot). |
| `podSecurityContext.runAsGroup` | `65532` | GID for the manager process. |
| `podSecurityContext.fsGroup` | `65532` | fsGroup so the container can read the TLS cert Secret (defaultMode 0400). |
| `podSecurityContext.seccompProfile.type` | `RuntimeDefault` | Seccomp profile; required by Restricted PSA. |
| `securityContext.allowPrivilegeEscalation` | `false` | Prevent privilege escalation in the container. |
| `securityContext.capabilities.drop` | `["ALL"]` | Drop all Linux capabilities. |
| `securityContext.readOnlyRootFilesystem` | `true` | Mount the root filesystem read-only. |
| `securityContext.runAsNonRoot` | `true` | Enforce non-root in the container security context. |
| `securityContext.runAsUser` | `65532` | UID for the container. |
| `securityContext.runAsGroup` | `65532` | GID for the container. |
| `webhook.enabled` | `true` | Deploy the MutatingWebhookConfiguration and ValidatingWebhookConfiguration for `Beskar7Cluster`. |
| `webhook.failurePolicy` | `Fail` | Webhook failure policy. |
| `webhook.matchPolicy` | `Equivalent` | Webhook match policy. |
| `webhook.service.port` | `443` | Port the webhook Service exposes. |
| `webhook.service.targetPort` | `9443` | Container port the webhook handler listens on. |
| `certManager.enabled` | `true` | `true`: cert-manager issues the TLS cert + injects the webhook caBundle. `false`: the chart self-generates a self-signed cert and writes the caBundle directly (no cert-manager dependency). |
| `certManager.issuer.name` | `beskar7-selfsigned-issuer` | cert-manager Issuer or ClusterIssuer name. |
| `certManager.issuer.kind` | `ClusterIssuer` | Kind of the cert-manager issuer (`Issuer` or `ClusterIssuer`). |
| `certManager.certificate.name` | `beskar7-serving-cert` | Name of the cert-manager Certificate resource. |
| `certManager.certificate.secretName` | `beskar7-webhook-server-cert` | Name of the Secret cert-manager writes the TLS certificate to. |
| `certManager.certificate.duration` | `8760h` | Certificate validity (1 year). |
| `certManager.certificate.renewBefore` | `720h` | Renew 30 days before expiry. |
| `namespace.create` | `false` | Render a Namespace resource. Set `true` only when not using `--create-namespace`. |
| `namespace.name` | `capb7-system` | Namespace for all chart resources. |
| `rbac.create` | `true` | Create RBAC resources for the manager. With `watchNamespaces` empty, renders a cluster-scoped ClusterRole + ClusterRoleBinding. With `watchNamespaces` set, renders a minimal ClusterRole + per-namespace Role/RoleBinding pairs (see `watchNamespaces`). |
| `watchNamespaces` | `[]` | Namespaces the controller watches. Empty (default) = all namespaces, cluster-scoped RBAC. Non-empty list scopes both the cache (`--watch-namespaces` flag on the manager) and the RBAC (per-namespace Role/RoleBinding in each listed namespace + leader-election Role in the operator's own namespace). Beskar7 CRs outside the listed namespaces are ignored. |
| `networkPolicy.enabled` | `false` | Deploy NetworkPolicy rules for the manager pod. |
| `monitoring.enabled` | `true` | Enable the metrics server on `:8443` (HTTPS, TokenReview/SAR auth). |
| `monitoring.serviceMonitor.enabled` | `false` | Create a Prometheus Operator `ServiceMonitor`. |
| `monitoring.serviceMonitor.namespace` | `""` | Namespace for the ServiceMonitor (defaults to release namespace). |
| `monitoring.serviceMonitor.labels` | `{}` | Additional labels for the ServiceMonitor. |
| `nodeSelector` | `{}` | Node selector for the manager pod. |
| `tolerations` | `[]` | Tolerations for the manager pod. |
| `affinity` | `{}` | Affinity rules for the manager pod. |
| `livenessProbe.httpGet.path` | `/healthz` | Liveness probe HTTP path. |
| `livenessProbe.httpGet.port` | `8081` | Liveness probe port. |
| `readinessProbe.httpGet.path` | `/readyz` | Readiness probe HTTP path. |
| `readinessProbe.httpGet.port` | `8081` | Readiness probe port. |
| `bootstrap.urlBase` | `https://<release>-controller-manager.<namespace>.svc:8082` | Base URL for the inspection callback and bootstrap data endpoints. Must be reachable by bare-metal hosts during PXE boot. |
| `labels` | `{}` | Additional labels applied to all chart resources. |
| `annotations` | `{}` | Additional annotations applied to all chart resources. |

## Upgrade

Before running `helm upgrade`, check [CHANGELOG.md](../../CHANGELOG.md) for version-to-version notes.

**CRDs are not upgraded automatically by Helm.** The CRDs bundled in `charts/beskar7/crds/` are installed on first `helm install` but Helm does not update them on subsequent `helm upgrade` runs (this is a Helm convention for CRDs). If a Beskar7 upgrade changes CRD schemas, apply the updated CRDs manually before upgrading the chart:

```bash
kubectl apply -f https://github.com/projectbeskar/beskar7/releases/download/<version>/beskar7-manifests-<version>.yaml
```

Or from a local clone:

```bash
kubectl apply -f charts/beskar7/crds/
```

Then upgrade the chart:

```bash
helm upgrade beskar7 beskar7/beskar7 --reset-then-reuse-values
```

**Do not use `--reuse-values`.** It reconstructs values from the *old* chart's
defaults, so value keys added since the release you installed are missing and the
upgrade fails to render. `--reset-then-reuse-values` (Helm 3.14+) replays your
overrides on top of the new chart's defaults; on older Helm, pass your own
`-f values.yaml` every time. See [docs/upgrading.md](../../docs/upgrading.md).

## `clusterctl move`

The bundled CRDs carry the `clusterctl.cluster.x-k8s.io` label that `clusterctl move` discovers CRDs by (a Helm install never goes through `clusterctl init`, which is what would otherwise add it), and `PhysicalHost` carries `clusterctl.cluster.x-k8s.io/move-hierarchy` so hosts move along with the cluster objects. Helm does not register beskar7 in clusterctl's provider inventory, so install the chart on the target management cluster before moving, and re-apply the CRDs on installs that predate the labels (`helm upgrade` never touches CRDs). Prerequisites and current limitations: [docs/installation.md](../../docs/installation.md#clusterctl-move).

## Uninstall

```bash
helm uninstall beskar7 --namespace capb7-system
```

`helm uninstall` does not remove CRDs. Delete them explicitly:

```bash
kubectl delete crd physicalhosts.infrastructure.cluster.x-k8s.io
kubectl delete crd beskar7machines.infrastructure.cluster.x-k8s.io
kubectl delete crd beskar7machinetemplates.infrastructure.cluster.x-k8s.io
kubectl delete crd beskar7clusters.infrastructure.cluster.x-k8s.io
```

Deleting CRDs removes all existing custom resources of those types. Do this only after the associated `Cluster` and `Machine` objects have been deleted.

## Further reading

- [Repository README](../../README.md)
- [Installation guide](../../docs/installation.md)
- [Troubleshooting](../../docs/troubleshooting.md)
