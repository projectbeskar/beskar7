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

The credentials Secret must also carry `beskar7.infrastructure.cluster.x-k8s.io/bmc-insecure-transport: "true"`, or the credentials are not sent (see [BMC credentials](#bmc-credentials)). The same applies to an `http://` address.

`insecureSkipVerify: true` and `caBundleSecretRef` together is a hard error: the controller marks `RedfishConnectionReady=False (InsecureCABundleConflict)` and stops reconciling until you fix the spec.

## BMC credentials

Required Secret shape:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: bmc-credentials
  namespace: default
  annotations:
    # Required: the BMC addresses these credentials may be sent to.
    beskar7.infrastructure.cluster.x-k8s.io/bmc-addresses: "10.20.0.0/24, *.bmc.example.com"
    # Only for http:// addresses or insecureSkipVerify: true.
    # beskar7.infrastructure.cluster.x-k8s.io/bmc-insecure-transport: "true"
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
kubectl annotate secret bmc-credentials -n default \
  beskar7.infrastructure.cluster.x-k8s.io/bmc-addresses="10.20.0.0/24, *.bmc.example.com"
```

To rotate, update the Secret. The PhysicalHost reconciler watches the Secret and re-reconciles on change; the manager rebuilds the gofish client with the new credentials on the next reconcile.

### Where the credentials may go

Anyone who can create or patch a `PhysicalHost` chooses its `redfishConnection.address`, and can name any Secret in the namespace as its `credentialsSecretRef`. Without a binding, that was enough to have the controller send another host's BMC password to an endpoint of the attacker's choosing, as HTTP Basic auth on the first Redfish request (SEC-16). The binding lives on the Secret, because only someone who may write the Secret should decide where its credentials go (decision D-030):

- **`beskar7.infrastructure.cluster.x-k8s.io/bmc-addresses`** (required) lists the BMC addresses the credentials may be sent to, separated by commas and/or whitespace: IP addresses, CIDRs (matched against IP addresses only), hostnames (exact, case-insensitive) and `*.suffix` wildcards (one or more labels under the suffix, never the suffix itself). The address's host is matched as written; a listed name is resolved only when the manager connects (see below). CIDRs wider than /8 (IPv4) or /32 (IPv6), and wildcards over a public suffix such as `*.com`, `*.lab` or `*.svc`, are rejected. A single malformed entry makes the whole annotation authorise nothing.
- **`beskar7.infrastructure.cluster.x-k8s.io/bmc-insecure-transport: "true"`** is also required before the credentials travel over `http://` or to a host with `insecureSkipVerify: true`, since either lets whoever is on the path read them.

Both readers of BMC credentials — the `PhysicalHost` controller and the `Beskar7Machine` controller's power and boot calls — check the Secret before they build a Redfish client, and nothing is sent when it does not authorise the address. The host reports `RedfishConnectionReady = False (CredentialsNotAuthorized)`, and its message names the annotation and the address's host to add; it never contains the credentials. See [PhysicalHost → Binding the credentials to their BMC](../physicalhost.md#binding-the-credentials-to-their-bmc) for the matching rules and how the host and its machine behave meanwhile.

Keep the list to the addresses your BMCs really have. Copying every host's current address into it would also authorise a host someone has already re-pointed. What the list does not stop is re-pointing a host to *another* listed BMC: the credentials still go only to an address you listed, but the host then drives the wrong machine. Restrict who can patch `PhysicalHost` objects for that.

A listed name is trusted to resolve to your BMC. The manager resolves it through its pod's resolver and the cluster DNS search path (`ndots:5`), so a short name such as `bmc1.lab` is tried as `bmc1.lab.<namespace>.svc.cluster.local` before the name itself, and a Service named `bmc1` in a namespace called `lab` would receive the connection. Prefer IP addresses and CIDRs; otherwise use fully-qualified names under a domain you control, and never wildcard a service that maps names to arbitrary IP addresses, such as nip.io or sslip.io.

`caBundleSecretRef` needs no such annotation: only its `ca.crt`/`tls.crt` keys are read, and they are not sent anywhere.

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
