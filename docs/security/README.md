# Beskar7 Security

> **Audience:** Operators

This page documents the security controls Beskar7 actually enforces in v0.4.0. Each item is anchored to source code so you can verify the claim.

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
apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
kind: PhysicalHost
metadata:
  name: server-01
  namespace: default
spec:
  redfishConnection:
    address: "https://bmc.internal.example.com"
    credentialsSecretRef: "bmc-credentials"
    insecureSkipVerify: false
    caBundleSecretRef: bmc-ca-bundle
```

The Secret data must include either a `ca.crt` (preferred) or `tls.crt` key. If both are present, `ca.crt` wins. Empty values are rejected with `CABundleFetchFailed`.

### 2. BMC credentials in a referenced Secret

`PhysicalHost.spec.redfishConnection.credentialsSecretRef` names an Opaque Secret in the same namespace as the host. The Secret must contain `username` and `password` data keys. Beskar7 fetches by name only — there is no List/Watch on Secrets across the cluster outside the existing PhysicalHost-scoped informer.

The Secret, not the host, decides which BMCs its credentials may be sent to (decision D-030). It must carry `beskar7.infrastructure.cluster.x-k8s.io/bmc-addresses`, a comma- and/or whitespace-separated list of IP addresses, CIDRs (matched against IP addresses only), hostnames and `*.suffix` wildcards that names the host of `redfishConnection.address`. The host is matched as written; a listed name is trusted to resolve to the BMC through the cluster DNS search path, so prefer IP addresses or fully-qualified names (see [Where the credentials may go](configuration.md#where-the-credentials-may-go)). CIDRs wider than /8 (IPv4) or /32 (IPv6), and wildcards over a public suffix, are rejected. An `http://` address or `insecureSkipVerify: true` also needs `beskar7.infrastructure.cluster.x-k8s.io/bmc-insecure-transport: "true"` on the same Secret. Both controllers run this check before they build a Redfish client, and it fails closed: a missing or malformed annotation, an address the list does not name, or an address that is not an `http(s)://` URL with a host and no userinfo sends nothing to the BMC, and the host reports `RedfishConnectionReady=False (CredentialsNotAuthorized)`. Someone who can patch a `PhysicalHost` but cannot write Secrets therefore cannot re-point a host to collect another host's BMC password. See [Where the credentials may go](configuration.md#where-the-credentials-may-go).

Source: `controllers/bmc_access.go:resolveBMCAccess`, called from `controllers/physicalhost_controller.go:reconcileNormal` and `controllers/beskar7machine_controller.go:getRedfishClientForHost`.

Beskar7 does not log usernames or passwords at any verbosity level. The structured logger emits `passwordProvided` (a boolean) at V(1) when constructing the gofish client. See `internal/redfish/gofish_client.go`.

### 3. Per-host bearer-token authentication on the callback endpoint

The manager runs an HTTPS server on `:8082` that hosts two host-scoped endpoints:

- `POST /api/v1/inspection/{namespace}/{hostName}` — receives inspection reports.
- `GET  /api/v1/bootstrap/{namespace}/{hostName}` — serves bootstrap data Secret bytes.

Both are gated by the same `auth.RequireBearer` middleware. The verifier:

1. Resolves `{namespace,hostName}` from the URL path.
2. Loads the targeted `PhysicalHost` and its per-host Secret `<host>-bootstrap-token` (decision D-029). `PhysicalHost.Status.Bootstrap` is a read-only mirror of that Secret and is never consulted, so the right to patch PhysicalHosts does not let anyone mint a credential.
3. Rejects the request if the host is not claimed, if its `ConsumerRef` names a different `Beskar7Machine` than the one the Secret's credentials were minted for (`consumer` key), if no token has been issued, if the token's expiry in the Secret is missing, unparseable or past, or if `sha256(presented)` does not equal `sha256(Secret token)` (constant-time compare via `crypto/subtle`).

All authentication failures collapse to an opaque `401 Unauthorized` body — the verifier's specific error is logged at V(1) only, never echoed to the client.

Token shape (decision D-004 in `.claude/context/PROJECT_CONTEXT.md`):

- 32 bytes from `crypto/rand`, encoded as `base64.RawURLEncoding` (43 chars), suitable for an iPXE kernel cmdline.
- The Secret holds the plaintext, its manager-written expiry and the claiming machine's name; `PhysicalHost.Status.Bootstrap.TokenHash` mirrors the SHA-256 (64 hex chars) for operators.
- Lifetime: 60 minutes (`auth.TokenLifetime`). Above the 10-minute `DefaultInspectionTimeout`, with headroom for slow BIOS POST + first-boot inspector.

The plaintext is stored in a per-host Secret named `<host-name>-bootstrap-token` (data key `plaintext-token`), owned by the PhysicalHost so it is GC'd on host delete. Decisions D-006, D-029.

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

The cluster-wide `list, watch` on Secret AND ConfigMap is the default RBAC topology. For multi-tenant deployments where the manager runs alongside workloads from other teams, set `--watch-namespaces=<csv>` on the manager (Helm: `watchNamespaces` in values; kustomize: the `config/rbac/namespace-scoped/` overlay) — the chart and overlay then generate per-namespace `Role` + `RoleBinding` pairs that scope Secret/ConfigMap access to exactly the listed namespaces, and the cluster-scoped binding shrinks to a residual reads-only ClusterRole. SEC-2 closure across PRs #80, #82, #83, and the security-docs follow-up. See [RBAC Hardening](rbac-hardening.md) for the full picture and the migration steps.

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
FROM golang:1.27@sha256:f44f6e88636cfb311f9ebace870ded69d943f227bb3cb27d32ffd84ea18c43ea as builder
...
FROM gcr.io/distroless/static:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
```

### 9. Authenticated metrics on `:8443`

The manager serves `/metrics` over HTTPS on `:8443` directly (no `kube-rbac-proxy` sidecar — removed in PR-11.1). Authentication and authorization are delegated to the kube-apiserver via TokenReview/SubjectAccessReview (`controller-runtime`'s `filters.WithAuthenticationAndAuthorization`). To scrape, your Prometheus ServiceAccount needs the `capb7-metrics-reader` ClusterRole (see `config/rbac/metrics_reader_role.yaml`). For local development you can opt out with `--secure-metrics=false`.

Source: `cmd/manager/main.go:135-145`.

## Configuration entry points

| What | How |
|---|---|
| Allow a credentials Secret to be sent to a BMC | annotate the Secret: `beskar7.infrastructure.cluster.x-k8s.io/bmc-addresses=<IPs, CIDRs, hostnames, *.suffix>` |
| Disable TLS verification on a single BMC (test only) | `PhysicalHost.spec.redfishConnection.insecureSkipVerify: true`, plus `beskar7.infrastructure.cluster.x-k8s.io/bmc-insecure-transport: "true"` on its credentials Secret |
| Use a private CA on a BMC | `PhysicalHost.spec.redfishConnection.caBundleSecretRef: <secret>` |
| Force-release a host whose BMC is dead | annotate the consuming Beskar7Machine: `infrastructure.cluster.x-k8s.io/force-release=true` |
| Open metrics for plain-HTTP development | manager flag `--secure-metrics=false` |

## What is NOT enforced

To avoid cargo-cult security claims:

- There is no built-in password-strength policy. The Secret can hold any bytes.
- There is no automatic credential rotation. Operators rotate Secret values manually; the `PhysicalHost` reconciler watches Secrets and re-reconciles on change.
- Nothing stops re-pointing a host to another BMC its Secret's `bmc-addresses` list names. The credentials still reach only a listed BMC, but the host then drives the wrong machine. Restrict who can patch `PhysicalHost` objects; there is no `PhysicalHost` admission webhook.
- There is no CIS / NIST / SOC 2 / ISO 27001 audit. Don't claim compliance you haven't measured.
- There is no security-scanning CronJob shipped with the chart. Use your platform's standard tooling.

## See also

- [Security Configuration](configuration.md)
- [RBAC Hardening](rbac-hardening.md)
- [Security Troubleshooting](troubleshooting.md)
- [Installation](../installation.md)
- [API Reference: PhysicalHost](../api-reference.md#physicalhost)
