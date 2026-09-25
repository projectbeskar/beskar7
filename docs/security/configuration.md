# Security Configuration

> **Audience:** Operators

How to configure the security features Beskar7 actually enforces. For an inventory of those features, see [Security](README.md).

## BMC TLS

### Strict verification (default, recommended)

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
kind: PhysicalHost
metadata:
  name: server-01
  namespace: default
spec:
  redfishConnection:
    address: "https://bmc.example.com"
    credentialsSecretRef: "bmc-credentials"
    insecureSkipVerify: false   # default
```

The manager uses the system root CA pool. If the BMC presents a certificate that the system pool trusts, this works out of the box.

### Custom CA bundle

When the BMC's certificate chains to an organisation-internal CA:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: bmc-ca-bundle
  namespace: default
type: Opaque
data:
  ca.crt: <base64 PEM>          # preferred key
  # tls.crt: <base64 PEM>       # fallback key, used only if ca.crt is absent
---
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

Cert-manager-driven equivalent (auto-rotates the CA Secret):

```yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: bmc-ca-bundle
  namespace: default
spec:
  secretName: bmc-ca-bundle      # data["ca.crt"] holds the issued CA cert
  isCA: true
  commonName: "BMC Issuing CA"
  issuerRef:
    name: my-root-issuer
    kind: ClusterIssuer
```

The CA bundle Secret must live in the same namespace as the PhysicalHost. The reconciler refreshes its TLS roots on each reconcile, so rotating the Secret takes effect at the next reconcile cycle (default 5 minutes; trigger sooner with a no-op `kubectl annotate physicalhost <name> reconcile-now=...`).

If the Secret is missing or has empty `ca.crt`/`tls.crt`, the controller marks `RedfishConnectionReady=False (CABundleFetchFailed)`. A host not yet `Inspecting`, `Deploying` or `Ready` (still `InUse` or unclaimed) moves to `Error`; a host already in one of those states — for example right after the rotation above — keeps it, and only the condition reports the fault.

### Skipping verification (development only)

```yaml
spec:
  redfishConnection:
    insecureSkipVerify: true
```

`insecureSkipVerify: true` and `caBundleSecretRef` together is a hard error: the controller marks `RedfishConnectionReady=False (InsecureCABundleConflict)` and stops reconciling until you fix the spec.

## BMC credentials

Required Secret shape:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: bmc-credentials
  namespace: default
type: Opaque
stringData:
  username: "admin"
  password: "..."
```

Generate strong passwords externally (Beskar7 does not enforce any strength policy). Example:

```bash
PASS=$(openssl rand -base64 32)
kubectl create secret generic bmc-credentials \
  --from-literal=username=admin \
  --from-literal=password="$PASS" \
  -n default
```

To rotate, update the Secret. The PhysicalHost reconciler watches the Secret and re-reconciles on change; the manager rebuilds the gofish client with the new credentials on the next reconcile.

## Bearer-token authentication on the callback endpoint

This is automatic — the operator does not configure it directly. Per host:

1. The `Beskar7Machine` reconciler mints a 32-byte token (`internal/auth/token.go:MintToken`).
2. The plaintext is written to a per-host Secret named `<host-name>-bootstrap-token`, owner-ref'd to the PhysicalHost, together with its expiry (`token-expires-at`) and the name of the `Beskar7Machine` it was minted for (`consumer`), in one write.
3. The controller's nonce-gated `/boot` endpoint renders the plaintext into the kernel cmdline as `beskar7.token=<plaintext>`. See [iPXE Setup](../ipxe-setup.md).
4. The inspector presents `Authorization: Bearer <token>` on every call to `:8082`. The manager accepts it only if it hashes to the token in the Secret, the expiry in the Secret has not passed (a missing or unreadable expiry is a rejection), and the host's `ConsumerRef` names the `Beskar7Machine` in `consumer` (D-029).
5. The token stops authenticating after 60 minutes, or as soon as the host is released or claimed by another machine, whose own claim mints a fresh one. The Secret is GC'd with the PhysicalHost.

The Secret is the only credential the manager checks. `Status.Bootstrap.{TokenHash, IssuedAt, ExpiresAt}` mirrors it for operators and is not an authentication input. The retired `infrastructure.cluster.x-k8s.io/bootstrap-token` and `boot-nonce` annotations are removed from a PhysicalHost on sight and never read, so the right to patch PhysicalHosts does not let anyone mint a credential.

The plaintext is never logged at any verbosity. The mirrored hash is safe to log — it cannot be used to forge a valid bearer header.

## Manager flags relevant to security

The manager flags that affect security posture (`cmd/manager/main.go`):

| Flag | Default | Purpose |
|---|---|---|
| `--metrics-bind-address` | `:8443` | Metrics endpoint. |
| `--secure-metrics` | `true` | When true, `/metrics` requires a TokenReview-validated SA bearer; metrics_reader role required. Set false only for local dev. |
| `--bootstrap-url-base` | `https://capb7-controller-manager.capb7-system.svc:8082` | Base URL for the per-host bootstrap URL written to `PhysicalHost.Status.Bootstrap.URL`. Override when the manager Service has a non-default DNS name (e.g. you installed with a release name other than `beskar7`). |
| `--inspection-port` | `8082` | Port the callback HTTPS endpoint listens on. |
| `--inspection-cert-dir` | `/tmp/k8s-webhook-server/serving-certs` | Directory containing `tls.crt` + `tls.key` for the callback endpoint. Defaults to the webhook cert dir; both endpoints share a cert covering the controller-manager Service DNS name when cert-manager issues the chart's Certificate. |
| `--enable-webhook` | `false` | Run the Beskar7Cluster webhook server. |
| `--webhook-port` | `9443` | Webhook server port. |
| `--webhook-cert-dir` | `/tmp/k8s-webhook-server/serving-certs` | Webhook cert dir. |

## RBAC

Beskar7 follows the principle of least privilege by default. See [RBAC Hardening](rbac-hardening.md) for the full ClusterRole.

### Verifying the deployed RBAC

```bash
kubectl get clusterrole -l app.kubernetes.io/name=beskar7 -o yaml
kubectl get clusterrolebinding -l app.kubernetes.io/name=beskar7 -o yaml
```

Or, if you installed via the kustomize overlay:

```bash
kubectl get clusterrole capb7-manager-role -o yaml
kubectl get clusterrolebinding capb7-manager-rolebinding -o yaml
```

There is no Helm value or operator flag to relax the ClusterRole at install time. To extend it (e.g. to add a custom resource the controller needs to read), edit `config/rbac/role.yaml` or the chart's `templates/rbac.yaml` directly and re-deploy.

## NetworkPolicy

The Helm chart ships a NetworkPolicy in `templates/networkpolicy.yaml` that allows ingress on `:8443` (metrics), `:9443` (webhook), and `:8082` (callback). Egress is unrestricted by default (Beskar7 needs to reach BMCs at arbitrary IPs, the kube-apiserver, and DNS).

To narrow egress to a known BMC subnet, edit the NetworkPolicy in your installation:

```yaml
egress:
  # DNS
  - ports:
      - protocol: UDP
        port: 53
  # Kubernetes API
  - to:
      - namespaceSelector: {}
    ports:
      - protocol: TCP
        port: 443
  # BMC subnet only
  - to:
      - ipBlock:
          cidr: 10.100.0.0/16
    ports:
      - protocol: TCP
        port: 443
      - protocol: TCP
        port: 5000
```

## Pod security

The `Deployment` template (kustomize and Helm) sets `runAsNonRoot: true`, `runAsUser: 65532`, `readOnlyRootFilesystem: true`, drops all capabilities, and uses the `RuntimeDefault` seccomp profile. None of this is configurable via Helm values; if you need to relax it (e.g. to debug with an ephemeral container), patch the Deployment after install.

## Auditing

There is no built-in security-scanning or compliance-reporting CronJob. Use your platform's normal tooling:

- Kubernetes audit logs from the kube-apiserver.
- A workload-CVE scanner (e.g. OSV-Scanner, Trivy, Grype) against the manager image.
- A Pod Security Admission profile (`baseline` or `restricted`) on the `capb7-system` namespace.

## See also

- [Security](README.md)
- [RBAC Hardening](rbac-hardening.md)
- [Security Troubleshooting](troubleshooting.md)
- [Installation](../installation.md)
