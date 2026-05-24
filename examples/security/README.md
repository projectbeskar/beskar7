# Beskar7 Security Examples

Two short examples demonstrating the security knobs Beskar7 v0.4.0-alpha.5 actually enforces. The "comprehensive security framework" advertised in earlier drafts (a `beskar7-security-policy` ConfigMap, the `--enable-security-monitoring` flag, the `beskar7-security-monitor` CronJob, CIS/NIST/SOC 2 compliance scoring) was never implemented and has been removed.

## What's actually enforced

| Control | Enforced by |
|---|---|
| BMC TLS verification with optional custom CA bundle | `PhysicalHost.spec.redfishConnection.{insecureSkipVerify, caBundleSecretRef}` (`controllers/redfish_tls.go`) |
| BMC credentials in a Secret | `PhysicalHost.spec.redfishConnection.credentialsSecretRef` |
| Per-host bearer token on `:8082` (inspection POST + bootstrap GET) | `internal/auth/token.go` + `controllers/inspection_handler.go` |
| Per-namespace Secret/ConfigMap RBAC scope | `config/rbac/role.yaml` (`SEC-2 / D-007`) |
| Pod security context | `config/manager/manager.yaml` and `charts/beskar7/templates/deployment.yaml` |
| NetworkPolicy on `:8082`, `:8443`, `:9443` | `charts/beskar7/templates/networkpolicy.yaml` |
| Authenticated metrics on `:8443` | `cmd/manager/main.go` (`--secure-metrics`) |
| Distroless, digest-pinned base image | `Dockerfile` |

For details, see [`docs/security/README.md`](../../docs/security/README.md) and [`docs/security/configuration.md`](../../docs/security/configuration.md).

## Examples

### `production.yaml`

A production-style configuration:

- BMC credentials in a Secret in the host namespace.
- A cert-manager-issued CA bundle Secret (`bmc-ca-bundle`) referenced via `caBundleSecretRef`.
- `insecureSkipVerify: false`.
- A NetworkPolicy that narrows the chart's default egress to a known BMC subnet.
- Helm values for a production install (commented at the bottom of the YAML).

```bash
# Adjust the BMC address, password, CA issuer, and BMC subnet for your environment.
kubectl apply -f production.yaml
```

### `development.yaml`

A lab configuration with `insecureSkipVerify: true` for self-signed BMC certs. Includes recommended Helm values for a development install (webhook disabled, verbose logging via `--zap-devel=true`).

```bash
kubectl apply -f development.yaml
```

## Verifying

After applying:

```bash
# Are the PhysicalHosts reaching the BMC?
kubectl get physicalhost -A
kubectl describe physicalhost <name> -n <namespace> | grep -A3 RedfishConnectionReady

# Is the manager seeing the bearer-token traffic?
kubectl logs -n beskar7-system deployment/beskar7-controller-manager | grep -i bearer
```

## See also

- [Security overview](../../docs/security/README.md)
- [Security configuration](../../docs/security/configuration.md)
- [RBAC hardening](../../docs/security/rbac-hardening.md)
- [Security troubleshooting](../../docs/security/troubleshooting.md)
