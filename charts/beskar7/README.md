# beskar7

> **Audience:** Operators

A Helm chart for Beskar7 — Cluster API Infrastructure Provider for Immutable Bare Metal.

Beskar7 provisions bare-metal Kubernetes nodes via Redfish power management, iPXE network boot, and a hardware inspection image. It implements the [Cluster API infrastructure provider contract](https://cluster-api.sigs.k8s.io/developer/providers/contracts/overview).

## Prerequisites

- Kubernetes v1.31+ ([docs](https://kubernetes.io/docs/setup/))
- Cluster API v1.10+ ([install with clusterctl](https://cluster-api.sigs.k8s.io/user/quick-start.html))
- cert-manager v1.16+ ([installation guide](https://cert-manager.io/docs/installation/))

Install cert-manager before installing this chart:

```bash
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.16.2/cert-manager.crds.yaml
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.16.2/cert-manager.yaml
```

## Installation

```bash
helm repo add beskar7 https://projectbeskar.github.io/beskar7
helm repo update
helm install --devel beskar7 beskar7/beskar7 \
  --namespace beskar7-system --create-namespace
```

The `--devel` flag is required while the chart version is a SemVer pre-release. Drop it once a non-prerelease version is published.

**Bootstrap URL.** The default `bootstrap.urlBase` is `https://beskar7-controller-manager.beskar7-system.svc:8082`. This matches the Service name when the release is named `beskar7`. If you use a different release name, pass the matching URL:

```bash
helm install --devel my-release beskar7/beskar7 \
  --namespace beskar7-system --create-namespace \
  --set bootstrap.urlBase=https://my-release-controller-manager.beskar7-system.svc:8082
```

Bare-metal hosts must be able to reach the `bootstrap.urlBase` during PXE boot. It is rendered into `PhysicalHost.Status.Bootstrap.URL` for each provisioned host.

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
| `certManager.enabled` | `true` | Use cert-manager to issue the TLS certificate for the webhook and callback server. |
| `certManager.issuer.name` | `beskar7-selfsigned-issuer` | cert-manager Issuer or ClusterIssuer name. |
| `certManager.issuer.kind` | `ClusterIssuer` | Kind of the cert-manager issuer (`Issuer` or `ClusterIssuer`). |
| `certManager.certificate.name` | `beskar7-serving-cert` | Name of the cert-manager Certificate resource. |
| `certManager.certificate.secretName` | `beskar7-webhook-server-cert` | Name of the Secret cert-manager writes the TLS certificate to. |
| `certManager.certificate.duration` | `8760h` | Certificate validity (1 year). |
| `certManager.certificate.renewBefore` | `720h` | Renew 30 days before expiry. |
| `namespace.create` | `false` | Render a Namespace resource. Set `true` only when not using `--create-namespace`. |
| `namespace.name` | `beskar7-system` | Namespace for all chart resources. |
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
| `bootstrap.urlBase` | `https://beskar7-controller-manager.beskar7-system.svc:8082` | Base URL for the inspection callback and bootstrap data endpoints. Must be reachable by bare-metal hosts during PXE boot. |
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
helm upgrade --devel beskar7 beskar7/beskar7
```

## Uninstall

```bash
helm uninstall beskar7 --namespace beskar7-system
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
