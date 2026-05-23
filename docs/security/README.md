# Beskar7 Security

This page documents the security controls Beskar7 actually enforces in v0.4.0-alpha.4. Each item is anchored to source code so you can verify the claim.

If you are looking for hardening recommendations, see [Configuration](configuration.md). For RBAC details, see [RBAC Hardening](rbac-hardening.md). For incident debugging, see [Troubleshooting](troubleshooting.md).

## Threat model in scope

- A misconfigured operator: BMC with self-signed cert, weak credentials, lax NetworkPolicy, etc.
- A compromised host on the management network attempting to talk to the manager's callback endpoint.
- A multi-tenant control-plane cluster where tenants must not read each other's BMC credentials or bootstrap data.

Out of scope: kernel exploits on the inspection image, BMC firmware vulnerabilities, supply-chain attacks on the operator's iPXE image hosting.

## Controls

### 1. BMC TLS verification with optional custom CA bundle

`PhysicalHost.spec.redfishConnection.caBundleSecretRef` references a Secret in the host's namespace containing PEM CA certificates. The manager builds an `*http.Client` whose root pool includes that bundle and passes it to gofish.

`insecureSkipVerify: true` and `caBundleSecretRef` are mutually exclusive. The reconciler rejects the combination terminally with `RedfishConnectionReady=False (InsecureCABundleConflict)`.

Source: `controllers/redfish_tls.go:fetchRedfishCABundle`, `validateRedfishTLSCombination`.

Concrete cert-manager-issued example:

```yaml
# 1. Issue a CA bundle Secret with cert-manager.
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: bmc-ca-bundle
  namespace: default
spec:
  secretName: bmc-ca-bundle           # Secret data["ca.crt"] will hold the CA cert
  isCA: true
  commonName: "BMC Issuing CA"
  issuerRef:
    name: my-root-issuer
    kind: ClusterIssuer
---
# 2. Reference it from the PhysicalHost.
apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
kind: PhysicalHost
metadata:
  name: server-01
  namespace: default
spec:
  redfishConnection:
    address: "https://bmc.internal.example.com"
    credentialsSecretRef: "bmc-credentials"
    insecureSkipVerify: false
    caBundleSecretRef:
      name: bmc-ca-bundle
```

The Secret data must include either a `ca.crt` (preferred) or `tls.crt` key. If both are present, `ca.crt` wins. Empty values are rejected with `CABundleFetchFailed`.

### 2. BMC credentials in a referenced Secret

`PhysicalHost.spec.redfishConnection.credentialsSecretRef` names an Opaque Secret in the same namespace as the host. The Secret must contain `username` and `password` data keys. Beskar7 fetches by name only — there is no List/Watch on Secrets across the cluster outside the existing PhysicalHost-scoped informer.

Source: `controllers/physicalhost_controller.go:getRedfishCredentials`, `controllers/beskar7machine_controller.go:getRedfishClientForHost`.

Beskar7 does not log usernames or passwords at any verbosity level. The structured logger emits `passwordProvided` (a boolean) at V(1) when constructing the gofish client. See `internal/redfish/gofish_client.go`.

### 3. Per-host bearer-token authentication on the callback endpoint

The manager runs an HTTPS server on `:8082` that hosts two host-scoped endpoints:

- `POST /api/v1/inspection/{namespace}/{hostName}` — receives inspection reports.
- `GET  /api/v1/bootstrap/{namespace}/{hostName}` — serves bootstrap data Secret bytes.

Both are gated by the same `auth.RequireBearer` middleware. The verifier:

1. Resolves `{namespace,hostName}` from the URL path.
2. Loads the targeted `PhysicalHost` and reads `Status.Bootstrap.TokenHash` and `ExpiresAt`.
3. Rejects the request if no token has been issued, the token has expired, or `auth.Verify(presented, storedHash)` returns false (constant-time SHA-256 compare via `crypto/subtle`).

All authentication failures collapse to an opaque `401 Unauthorized` body — the verifier's specific error is logged at V(1) only, never echoed to the client.

Token shape (decision D-004 in `.claude/context/PROJECT_CONTEXT.md`):

- 32 bytes from `crypto/rand`, encoded as `base64.RawURLEncoding` (43 chars), suitable for an iPXE kernel cmdline.
- SHA-256 hash (64 hex chars) is persisted to `PhysicalHost.Status.Bootstrap.TokenHash`.
- Lifetime: 30 minutes (`auth.TokenLifetime`). Above the 10-minute `DefaultInspectionTimeout`, with headroom for slow BIOS POST + first-boot inspector.

The plaintext is stored in a per-host Secret named `<host-name>-bootstrap-token` (data key `plaintext-token`), owned by the PhysicalHost so it is GC'd on host delete. Decision D-006.

Source: `internal/auth/token.go`, `internal/auth/middleware.go`, `controllers/inspection_handler.go:newBearerTokenVerifier`, `controllers/bootstrap_handler.go`.

### 4. Body cap on inspection POST

`http.MaxBytesReader` caps the inspection POST body at 1 MiB. Over-limit reads return `413 Request Entity Too Large`. Real inspector payloads (CPUs + DIMMs + NICs + disks) are low single-digit kilobytes; the cap is a defense against runaway clients.

Source: `controllers/inspection_handler.go` (`inspectionMaxBodyBytes = 1 << 20`).

### 5. Per-namespace Secret/ConfigMap RBAC scope

The controllers' RBAC markers grant Secret read access only as needed:

- `PhysicalHost` reconciler: `secrets: get, list, watch` (the watch is required by the controller-runtime informer that triggers reconciles on credential rotation).
- `Beskar7Machine` reconciler: `secrets: get, create, update, patch, delete` (no list/watch — Secret access is by name only).
- ConfigMaps: `get, list, watch, create, update, patch, delete`. The inspection handler upserts a per-host inspection-result ConfigMap via the cached client, which requires the controller-runtime informer to be backed by `list, watch`. The handler also pre-warms the informer at `SetupCallbackServer` time so the first POST does not stall waiting for an initial sync.

Source: `controllers/physicalhost_controller.go:97`, `controllers/beskar7machine_controller.go:87-103`, `controllers/inspection_handler.go` (`SetupCallbackServer` pre-warm), `config/rbac/role.yaml`.

The cluster-wide list/watch on Secret AND ConfigMap remain as residual scopes (tracked as `SEC-2` / D-007 in `.claude/context/PROJECT_CONTEXT.md`); a label-selected partial cache for both is an open v0.5 follow-up. See [RBAC Hardening](rbac-hardening.md) for detail.

### 6. Pod security context

The kustomize manifests under `config/manager/` and the Helm chart in `charts/beskar7/templates/deployment.yaml` set both pod-level and container-level security context:

```yaml
# Pod-level (chart: .Values.podSecurityContext)
podSecurityContext:
  runAsNonRoot: true
  runAsUser: 65532
  runAsGroup: 65532
  fsGroup: 65532
  seccompProfile:
    type: RuntimeDefault

# Container-level (chart: .Values.securityContext)
securityContext:
  runAsNonRoot: true
  runAsUser: 65532
  runAsGroup: 65532
  readOnlyRootFilesystem: true
  allowPrivilegeEscalation: false
  capabilities:
    drop: ["ALL"]
```

`fsGroup` and the matching `runAsGroup` on the pod-level context are required so the non-root container (UID 65532) can read the cert-manager-issued TLS Secret mounted at `/tmp/k8s-webhook-server/serving-certs` (Secret volumes use `defaultMode: 0400` to match the kustomize variant; without `fsGroup` the files are owned by `root:root` and the manager crashloops on the eager TLS load in the inspection callback server — this was the bug fixed in v0.4.0-alpha.2).

### 7. NetworkPolicy

The chart ships a NetworkPolicy that allows ingress on:

- `:8443` — metrics (HTTPS, authenticated; see #9).
- `:9443` — webhook server (when `webhook.enabled=true`).
- `:8082` — callback endpoint for the inspection POST and bootstrap GET. Bare-metal IPs cannot be allow-listed at the policy level — the bearer token is the access control.

Source: `charts/beskar7/templates/networkpolicy.yaml`.

### 8. Container image

The Dockerfile uses a multi-stage build with `CGO_ENABLED=0` and a distroless `nonroot` runtime base, both pinned by digest:

```Dockerfile
FROM golang:1.25@sha256:8a7adc288b77e9b787cd2695029eb54d10ae80571b21d44fed68d067ad0a9c96 as builder
...
FROM gcr.io/distroless/static:nonroot@sha256:e3f945647ffb95b5839c07038d64f9811adf17308b9121d8a2b87b6a22a80a39
```

### 9. Authenticated metrics on `:8443`

The manager serves `/metrics` over HTTPS on `:8443` directly (no `kube-rbac-proxy` sidecar — removed in PR-11.1). Authentication and authorization are delegated to the kube-apiserver via TokenReview/SubjectAccessReview (`controller-runtime`'s `filters.WithAuthenticationAndAuthorization`). To scrape, your Prometheus ServiceAccount needs the `metrics_reader` ClusterRole (see `config/rbac/metrics_reader_role.yaml`). For local development you can opt out with `--secure-metrics=false`.

Source: `cmd/manager/main.go:135-145`.

## Configuration entry points

| What | How |
|---|---|
| Disable TLS verification on a single BMC (test only) | `PhysicalHost.spec.redfishConnection.insecureSkipVerify: true` |
| Use a private CA on a BMC | `PhysicalHost.spec.redfishConnection.caBundleSecretRef.name: <secret>` |
| Force-release a host whose BMC is dead | annotate the consuming Beskar7Machine: `infrastructure.cluster.x-k8s.io/force-release=true` |
| Open metrics for plain-HTTP development | manager flag `--secure-metrics=false` |

## What is NOT enforced

To avoid cargo-cult security claims:

- There is no built-in password-strength policy. The Secret can hold any bytes.
- There is no automatic credential rotation. Operators rotate Secret values manually; the `PhysicalHost` reconciler watches Secrets and re-reconciles on change.
- There is no CIS / NIST / SOC 2 / ISO 27001 audit. Don't claim compliance you haven't measured.
- There is no security-scanning CronJob shipped with the chart. Use your platform's standard tooling.

## See also

- [Security Configuration](configuration.md)
- [RBAC Hardening](rbac-hardening.md)
- [Security Troubleshooting](troubleshooting.md)
- [Quick Start](../quick-start.md)
- [API Reference: PhysicalHost](../api-reference.md#physicalhost)
