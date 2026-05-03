# Security Troubleshooting

How to diagnose security-control failures in Beskar7.

For an inventory of what's enforced, see [Security](README.md). For configuration recipes, see [Configuration](configuration.md). For general operational issues, see [Troubleshooting](../troubleshooting.md).

## TLS to the BMC

### Symptom: `RedfishConnectionReady=False (CABundleFetchFailed)`

The controller could not fetch or parse the CA bundle Secret named by `caBundleSecretRef`.

```bash
kubectl describe physicalhost <name>
# look for the RedfishConnectionReady condition

kubectl get secret <ca-secret-name> -n <namespace> -o yaml
```

Causes and fixes:

- The Secret does not exist in the host's namespace. Create it.
- The Secret has neither `ca.crt` nor `tls.crt` data keys. Add one (PEM-encoded, base64).
- Both keys are empty. Populate `ca.crt`.

### Symptom: `RedfishConnectionReady=False (InsecureCABundleConflict)`

The PhysicalHost has both `insecureSkipVerify: true` and `caBundleSecretRef` set. These are mutually exclusive — pick one:

```bash
# Either remove caBundleSecretRef:
kubectl patch physicalhost <name> --type=json -p='[{"op":"remove","path":"/spec/redfishConnection/caBundleSecretRef"}]'

# Or flip insecureSkipVerify to false:
kubectl patch physicalhost <name> --type=merge -p='{"spec":{"redfishConnection":{"insecureSkipVerify":false}}}'
```

### Symptom: certificate validation fails for a public-CA-signed BMC

Verify the chain manually:

```bash
openssl s_client -connect bmc.example.com:443 -showcerts -verify_return_error </dev/null
```

If `openssl` is happy but Beskar7 isn't, check the manager pod's CA pool. The default `gcr.io/distroless/static:nonroot` image includes the standard Mozilla bundle; an internal CA must be provided via `caBundleSecretRef`.

## BMC credentials

### Symptom: `RedfishConnectionReady=False (MissingCredentials)` / `(SecretNotFound)` / `(MissingSecretData)`

```bash
kubectl get physicalhost <name> -o jsonpath='{.status.errorMessage}'
kubectl get secret <secret-name> -n <namespace>
kubectl get secret <secret-name> -n <namespace> -o yaml | grep -A2 data:
```

The Secret needs `username` and `password` data keys (Opaque). If both keys are present but the BMC still rejects authentication, test manually:

```bash
USER=$(kubectl get secret <secret-name> -n <ns> -o jsonpath='{.data.username}' | base64 -d)
PASS=$(kubectl get secret <secret-name> -n <ns> -o jsonpath='{.data.password}' | base64 -d)
curl -sk -u "$USER:$PASS" https://<bmc-address>/redfish/v1/Systems
```

If `curl` succeeds, the credentials are correct — the problem is elsewhere (BMC firewall, BMC user disabled, BMC role lacks required privilege).

## Bearer-token failures (`401 Unauthorized` from `:8082`)

The callback endpoint returns an opaque `401` for every authentication failure. The verifier's specific error is logged at V(1) on the manager. To see why a request failed:

```bash
# Restart the manager with --zap-devel=true (development log encoder, V(1) included)
kubectl edit deployment beskar7-controller-manager -n beskar7-system
# Add --zap-devel=true to args, save, wait for rollout

# Tail the logs while the inspector retries:
kubectl logs -n beskar7-system deployment/beskar7-controller-manager -f | grep -i bearer
```

Possible causes:

| Log message | Cause | Fix |
|---|---|---|
| `get PhysicalHost: ... not found` | URL path references a host that does not exist. | Check the iPXE-rendered cmdline matches the actual `<namespace>/<host>`. |
| `no bootstrap token issued for host ...` | `Status.Bootstrap.TokenHash` is empty. | The Beskar7Machine reconciler has not yet run `triggerInspection` for this host. Wait, or check Beskar7Machine status. |
| `bootstrap token expired for host ...` | The token's `ExpiresAt` is in the past (30-min lifetime). | Trigger a re-mint — easiest is to delete the per-host Secret (`<host>-bootstrap-token`), the Beskar7Machine reconciler will mint a fresh one. The iPXE cmdline embedding the previous plaintext will be invalid; the host must re-PXE. |
| `bootstrap token mismatch for host ...` | The plaintext on the wire does not hash to the stored hash. | Likely a stale iPXE cmdline. Rebuild the cmdline from the current Secret (`kubectl get secret <host>-bootstrap-token -o jsonpath='{.data.plaintext-token}' \| base64 -d`) and re-PXE the host. Persistent mismatch suggests cmdline truncation or shell-escape damage in your iPXE template. |
| Clock skew between manager and host > 30m | Host clock far in the future or past. | NTP. |

## RBAC

### Symptom: controller logs show `forbidden`

```bash
kubectl logs -n beskar7-system deployment/beskar7-controller-manager | grep -i forbidden
```

Identify the verb + resource being denied. Compare with the deployed ClusterRole:

```bash
kubectl auth can-i --list --as=system:serviceaccount:beskar7-system:beskar7-controller-manager
```

If the missing permission is for one of the resources the controller legitimately needs to access (e.g. `beskar7machines/finalizers`), the ClusterRole shipped with the chart is incomplete — file a bug. If it is something Beskar7 should not need, do not grant it; file a bug instead.

### Symptom: metrics scraping returns 401/403

`/metrics` on `:8443` requires a Kubernetes-authenticated request. Your Prometheus ServiceAccount needs the `metrics_reader` ClusterRole:

```bash
kubectl get clusterrole metrics-reader -o yaml
kubectl create clusterrolebinding prometheus-metrics-reader \
  --clusterrole=metrics-reader \
  --serviceaccount=monitoring:prometheus
```

For local development (`kubectl port-forward`), set `--secure-metrics=false` on the manager and use plain HTTP.

## Webhook

### Symptom: `failed calling webhook "validation.beskar7cluster..."`

The Beskar7Cluster validating webhook is the only webhook in the codebase. Check:

```bash
kubectl get validatingwebhookconfigurations -l app.kubernetes.io/name=beskar7
kubectl get certificate -n beskar7-system
kubectl get pods -n beskar7-system
```

Common causes:

- cert-manager not installed or not Ready. The chart's `Certificate` resource issues the webhook serving cert; without it the webhook server fails the TLS handshake. Install cert-manager and wait for the Certificate to reach `Ready=True`.
- `webhook.enabled=false` in Helm values, but the operator still applied a `ValidatingWebhookConfiguration`. Either delete the orphaned config or re-enable the webhook.
- The manager pod's webhook server is not running. Inspect logs for cert-load errors.

### Symptom: a PhysicalHost / Beskar7Machine / Beskar7MachineTemplate validation error from a webhook

There is **no** webhook for those three CRDs. Any "webhook denied" error mentioning them is from a stale `ValidatingWebhookConfiguration` or `MutatingWebhookConfiguration` left over from a v0.3 install. Remove the orphaned configurations:

```bash
kubectl get validatingwebhookconfigurations | grep beskar7
kubectl get mutatingwebhookconfigurations | grep beskar7
# delete any that reference physicalhost, beskar7machine, or beskar7machinetemplate paths
```

## NetworkPolicy

### Symptom: callback endpoint timeouts from booted hosts

Bare-metal nodes typically come up on a different network than the Pod CIDR. The chart's NetworkPolicy allows ingress on `:8082` from any source — the bearer token is the access control. If a Pod-network NetworkPolicy in your cluster blocks `:8082`, edit it.

To debug from inside a Pod that has cluster network access:

```bash
kubectl run -it --rm debug --image=curlimages/curl --restart=Never -- \
  curl -kv -H "Authorization: Bearer test" \
       https://<release>-controller-manager.<namespace>.svc.cluster.local:8082/healthz
```

`/healthz` does not require authentication; if even that times out, the issue is network plumbing, not auth.

## Container security

### Symptom: pod fails to start with security-context error

If a Pod Security Admission profile is enforced on the namespace, verify the pod manifest matches the chart's expectations:

```bash
kubectl get pod -n beskar7-system -l app.kubernetes.io/name=beskar7 -o yaml | grep -A20 securityContext
```

The chart's `Deployment` is compatible with `restricted`; if you have customised the manifest and dropped a required field, restore it.

## Diagnostic bundle

When opening an issue with a security symptom:

```bash
kubectl get pods -n beskar7-system -o yaml > pods.yaml
kubectl get clusterrole -l app.kubernetes.io/name=beskar7 -o yaml > rbac-clusterrole.yaml
kubectl get networkpolicy -n beskar7-system -o yaml > networkpolicy.yaml
kubectl get validatingwebhookconfigurations -l app.kubernetes.io/name=beskar7 -o yaml > webhooks.yaml
kubectl logs -n beskar7-system deployment/beskar7-controller-manager --tail=500 > controller.log

kubectl get physicalhost -o yaml > physicalhosts.yaml          # redact BMC addresses if shareable
kubectl get events -A --sort-by='.lastTimestamp' > events.txt
```

Do not include the BMC credentials Secret or any per-host bootstrap-token Secret — both contain plaintext secrets.

## See also

- [Security](README.md)
- [Configuration](configuration.md)
- [RBAC Hardening](rbac-hardening.md)
- [General Troubleshooting](../troubleshooting.md)
