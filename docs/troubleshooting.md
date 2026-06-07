# Troubleshooting Beskar7

> **Audience:** Operators

This guide helps you diagnose and resolve common Beskar7 issues.

## Quick Diagnosis

```bash
# Check controller is running
kubectl get pods -n beskar7-system

# Check controller logs
kubectl logs -n beskar7-system deployment/beskar7-controller-manager -f

# Check PhysicalHost status
kubectl get physicalhost

# Check Beskar7Machine status
kubectl get beskar7machine

# Describe resources for details
kubectl describe physicalhost <name>
kubectl describe beskar7machine <name>
```

## Common Issues

### 1. Controller Crashes: "no kind is registered for the type v1beta1.Machine"

**Symptom:**
```
ERROR controller-runtime.source.EventHandler kind must be registered
no kind is registered for the type v1beta1.Machine
```

**Cause:** Cluster API is not installed

**Solution:**
```bash
# Install Cluster API
clusterctl init

# Or manually:
kubectl apply -f https://github.com/kubernetes-sigs/cluster-api/releases/download/v1.10.0/cluster-api-components.yaml
kubectl apply -f https://github.com/kubernetes-sigs/cluster-api/releases/download/v1.10.0/bootstrap-components.yaml
kubectl apply -f https://github.com/kubernetes-sigs/cluster-api/releases/download/v1.10.0/control-plane-components.yaml

# Restart Beskar7
kubectl rollout restart deployment/beskar7-controller-manager -n beskar7-system
```

### 2. Webhook Fails: "connection refused" or "certificate" errors

**Symptom:**
```
failed calling webhook "validation.beskar7cluster.infrastructure.cluster.x-k8s.io"
x509: certificate signed by unknown authority
```

There is exactly one webhook in v0.4: the Beskar7Cluster validating webhook (`api/v1beta1/webhooks/beskar7cluster_webhook.go`). If your error mentions `physicalhost`, `beskar7machine`, or `beskar7machinetemplate` webhooks, those are stale `ValidatingWebhookConfiguration`/`MutatingWebhookConfiguration` objects left over from a v0.3 install — see step 3 below.

**Cause:** cert-manager not installed or not ready, or webhook serving cert not issued.

**Solution:**
```bash
# Install cert-manager
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.16.2/cert-manager.yaml

# Wait for it to be ready
kubectl wait --for=condition=Available --timeout=300s deployment/cert-manager -n cert-manager

# Verify the chart's Certificate is Ready
kubectl get certificate -n beskar7-system

# Restart Beskar7
kubectl rollout restart deployment/beskar7-controller-manager -n beskar7-system

# Verify the only expected webhook is registered
kubectl get validatingwebhookconfigurations -l app.kubernetes.io/name=beskar7
```

### 3. Stale v0.3 webhooks blocking admission

**Symptom:** `kubectl apply` of any resource returns `failed calling webhook "mutation.physicalhost..."` or similar.

**Cause:** v0.4 removed the PhysicalHost defaulting/validating webhooks. If you upgraded from v0.3.x, the `ValidatingWebhookConfiguration` and `MutatingWebhookConfiguration` objects can survive. With `failurePolicy: Fail`, the apiserver tries to call a path that no longer exists and rejects the request.

**Diagnosis:**
```bash
kubectl get validatingwebhookconfigurations | grep -i physicalhost
kubectl get mutatingwebhookconfigurations | grep -i physicalhost
```

**Solution:**
```bash
# Delete any orphaned webhook configs that reference physicalhost / beskar7machine / beskar7machinetemplate paths
kubectl delete validatingwebhookconfigurations <name>
kubectl delete mutatingwebhookconfigurations <name>
```

### 4. PhysicalHost Stuck in "Enrolling"

**Symptom:** Host never transitions to Available

**Common Causes:**

#### A. BMC Not Reachable

```bash
# Test from your machine
curl -k -u admin:password https://BMC_IP/redfish/v1/

# Test from controller pod
kubectl run -it --rm debug --image=curlimages/curl --restart=Never -- \
  curl -k -u admin:password https://BMC_IP/redfish/v1/
```

**Solution:**
- Verify BMC IP address is correct
- Check network connectivity
- Ensure firewall allows port 443 from Kubernetes nodes

#### B. Invalid Credentials

```bash
# Check secret exists
kubectl get secret <secret-name> -o yaml

# Verify username/password are correct
kubectl get secret <secret-name> -o jsonpath='{.data.username}' | base64 -d
kubectl get secret <secret-name> -o jsonpath='{.data.password}' | base64 -d
```

**Solution:**
- Update secret with correct credentials
- Verify BMC user has necessary permissions

#### C. Redfish API Disabled

**Solution:**
- Log into BMC web interface
- Enable Redfish API in settings
- Dell iDRAC: Network -> Redfish -> Enable
- HPE iLO: Network -> iLO RESTful API -> Enable
- Supermicro: Configuration -> Redfish API -> Enable

### 5. Inspection Phase Stuck in "Pending" or "Booting"

**Symptom:** InspectionPhase never progresses to "Complete"

**Check:**
```bash
# Check inspection phase
kubectl get physicalhost <name> -o jsonpath='{.status.inspectionPhase}'

# Check machine phase
kubectl get beskar7machine <name> -o jsonpath='{.status.phase}'
```

**Common Causes:**

#### A. iPXE Infrastructure Not Configured

**Solution:** Set up iPXE infrastructure
- See [iPXE Setup Guide](ipxe-setup.md)
- Verify DHCP server is running
- Verify HTTP server is accessible
- Test boot script URL manually:
  ```bash
  curl http://boot-server/ipxe/boot.ipxe
  ```

#### B. PXE Boot Not Enabled

**Solution:**
- Enter server BIOS setup
- Enable "Network Boot" or "PXE Boot"
- Set network boot first in boot order
- Save and reboot

#### C. Inspection Image Not Accessible

**Solution:**
- Verify inspection image exists:
  ```bash
  curl -I http://boot-server/inspector/vmlinuz
  curl -I http://boot-server/inspector/initrd.img
  ```
- Check HTTP server logs
- Ensure server can reach boot server from provisioning network

#### D. Network Configuration Issues

**Solution:**
- Check DHCP is working (server gets IP)
- Verify DNS resolution (if using hostnames)
- Check firewall rules
- Monitor server serial console for boot errors

### 6. Inspection Times Out

**Symptom:** InspectionPhase changes to "Timeout" after 10 minutes

**Causes:**
- Inspection image not booting
- Inspector can't reach Beskar7 API
- Inspector script failure

**Debug:**
```bash
# Check server serial console (via BMC)
# Look for:
# - Kernel boot messages
# - Network configuration
# - Script errors

# Check controller logs for inspection reports
kubectl logs -n beskar7-system deployment/beskar7-controller-manager | grep inspection

# Check HTTP server logs
sudo tail -f /var/log/nginx/boot-access.log
```

**Solution:**
- Review serial console output
- Fix network connectivity issues
- Verify inspection image is working
- Increase timeout if hardware is slow

### 7. Hardware Validation Failed

**Symptom:** Machine stuck with validation error

**Check:**
```bash
# View inspection report
kubectl get physicalhost <name> -o jsonpath='{.status.inspectionReport}' | jq

# View requirements
kubectl get beskar7machine <name> -o jsonpath='{.spec.hardwareRequirements}' | jq
```

**Solution:**

Option 1: Adjust requirements
```yaml
spec:
  hardwareRequirements:
    minCPUCores: 4    # Lower if needed
    minMemoryGB: 8    # Lower if needed
    minDiskGB: 50     # Lower if needed
```

Option 2: Use different hardware that meets requirements

### 8. Power Operations Fail

**Symptom:** Can't power on/off server

**Check:**
```bash
# Check PhysicalHost power state
kubectl get physicalhost <name> -o jsonpath='{.status.observedPowerState}'

# Check controller logs
kubectl logs -n beskar7-system deployment/beskar7-controller-manager | grep -i power
```

**Common Causes:**

#### A. Insufficient Permissions

**Solution:**
- Verify BMC user has power management privileges
- Dell iDRAC: User needs "Configure Manager" role
- HPE iLO: User needs "Virtual Power and Reset" privilege
- Lenovo XCC: User needs "Supervisor" role

#### B. BMC Licensing

**Solution:**
- Some vendors require license for remote power control
- Check BMC license status
- Upgrade license if necessary

#### C. Hardware Interlocks

**Solution:**
- Ensure chassis is closed (some servers have safety interlocks)
- Check physical power button isn't locked
- Verify power supplies are connected

### 9. Machine Never Becomes Ready

**Symptom:** Beskar7Machine stays in `Pending` or `Inspecting` phase. (The controller only writes one of four phase strings: `Pending`, `Inspecting`, `Provisioned`, `Failed` — no `Provisioning` phase exists; if you see that in a script, the script is filtering for a value that will never match.)

**Check Workflow:**
```bash
# 1. Check PhysicalHost was claimed
kubectl get physicalhost <name> -o jsonpath='{.spec.consumerRef}'

# 2. Check inspection completed
kubectl get physicalhost <name> -o jsonpath='{.status.inspectionPhase}'
# Should be: Complete

# 3. Check inspection report exists
kubectl get physicalhost <name> -o jsonpath='{.status.inspectionReport}'

# 4. Check machine phase
kubectl get beskar7machine <name> -o jsonpath='{.status.phase}'

# 5. Check for errors
kubectl describe beskar7machine <name>
kubectl describe physicalhost <name>
```

**Solution:** Depends on which step failed (see above sections)

### 10. Inspection or bootstrap callback returns 401 Unauthorized

**Symptom:** The inspector logs `401` from `https://<manager>:8082/api/v1/inspection/<ns>/<host>`, or the host fails to fetch bootstrap data.

The callback endpoint authenticates every request via per-host bearer tokens. Failures collapse to an opaque `401` body — the verifier's specific reason is logged on the manager at V(1).

**Diagnosis:**
```bash
# Tail manager logs for verifier output. Add --zap-devel=true to manager args
# temporarily to surface V(1) lines.
kubectl logs -n beskar7-system deployment/beskar7-controller-manager -f | grep -i bearer
```

**Common causes:**

| V(1) message | Cause | Fix |
|---|---|---|
| `no bootstrap token issued for host ...` | The Beskar7Machine reconciler has not minted a token yet. | Wait, or check `kubectl describe beskar7machine <name>` for the current phase. |
| `bootstrap token expired for host ...` | More than 30 minutes elapsed since the token was minted (`auth.TokenLifetime`). | Delete the per-host Secret `<host>-bootstrap-token`; the controller mints a fresh one and re-renders the cmdline on next reconcile. The booted host must re-PXE to pick up the new plaintext. |
| `bootstrap token mismatch for host ...` | Plaintext on the wire does not hash to `Status.Bootstrap.TokenHash`. | Stale iPXE cmdline. Compare the token in the kernel cmdline against `kubectl get secret <host>-bootstrap-token -o jsonpath='{.data.plaintext-token}' \| base64 -d`. Re-PXE if they diverge. |

Clock skew (> 30 min) between the manager pod and the BMC-managed host can also cause `expired` results — verify NTP on both sides.

### 11. PhysicalHost in Error: `InsecureCABundleConflict` or `CABundleFetchFailed`

**Symptom:**
```bash
kubectl get physicalhost <name> -o jsonpath='{.status.errorMessage}'
```
returns either `redfishConnection.insecureSkipVerify=true is mutually exclusive with caBundleSecretRef` or `CA bundle secret ... not found / has no usable ca.crt or tls.crt data key`.

**`InsecureCABundleConflict`:** the spec has both `insecureSkipVerify: true` and `caBundleSecretRef` set. Pick one. See [Security Configuration](security/configuration.md#bmc-tls).

**`CABundleFetchFailed`:** the named Secret does not exist in the host's namespace, or the data is missing. The expected data keys are `ca.crt` (preferred) or `tls.crt`. Check the Secret:
```bash
kubectl get secret <ca-bundle-secret> -n <namespace> -o yaml
```
Populate `data.ca.crt` (base64 PEM) and re-apply.

## Debugging Tools

### Enable Verbose Logging

```bash
# Edit controller deployment
kubectl edit deployment beskar7-controller-manager -n beskar7-system

# Add to container args:
spec:
  containers:
  - name: manager
    args:
    - --leader-elect
    - -v=5  # Add this line (1-10, higher = more verbose)
```

### Watch Events

```bash
# Watch all events
kubectl get events -A -w

# Watch specific resource events
kubectl get events --field-selector involvedObject.name=<resource-name> -w
```

### Serial Console

Access server serial console through BMC:
- Dell iDRAC: Launch Virtual Console
- HPE iLO: Launch Remote Console
- Lenovo XCC: Launch Remote Console
- Supermicro: Launch SOL

Watch boot process to debug:
- PXE boot failures
- Kernel panics
- Inspection script errors

### Network Capture

Capture network traffic to debug DHCP/PXE:
```bash
# On boot server
sudo tcpdump -i eth0 port 67 or port 68 or port 69 -w boot-debug.pcap

# Analyze with Wireshark
wireshark boot-debug.pcap
```

## Controller Logs Reference

### Normal Startup

```
Starting Beskar7Controller Manager
Starting EventSource controller=physicalhost
Starting Controller controller=physicalhost
Starting workers worker count=1
```

### Successful PhysicalHost Enrollment

```
Enrolling PhysicalHost host=server-01
Connected to Redfish endpoint host=server-01
PhysicalHost transitioned to Available host=server-01
```

### Successful Inspection

```
Starting inspection host=server-01 machine=worker-01
Setting PXE boot source host=server-01
Powering on host host=server-01
Inspection report received host=server-01
Hardware validation passed host=server-01
PhysicalHost ready host=server-01
```

### Error Examples

```
# Redfish connection failed
Failed to connect to Redfish endpoint: dial tcp: i/o timeout

# Invalid credentials
Failed to authenticate: 401 Unauthorized

# Power operation failed
Failed to set power state: operation not permitted

# Inspection timeout
Inspection timed out after 10m0s
```

## Health Checks

### Controller Health

```bash
# Check controller is running
kubectl get deployment -n beskar7-system beskar7-controller-manager
# Should show: READY 1/1

# Check controller logs for errors
kubectl logs -n beskar7-system deployment/beskar7-controller-manager --tail=100 | grep -i error

# Check webhook is healthy
kubectl get endpoints -n beskar7-system beskar7-webhook-service
```

### PhysicalHost Health

```bash
# List all hosts
kubectl get physicalhost -o wide

# Check for hosts in error state
kubectl get physicalhost -o json | jq '.items[] | select(.status.state=="Error")'

# Check Redfish connectivity
kubectl get physicalhost -o json | jq '.items[] | select(.status.conditions[]? | select(.type=="RedfishConnected" and .status=="False"))'
```

### Beskar7Machine Health

```bash
# List all machines
kubectl get beskar7machine -o wide

# Check for machines not ready
kubectl get beskar7machine -o json | jq '.items[] | select(.status.ready==false)'

# Check phases
kubectl get beskar7machine -o custom-columns=NAME:.metadata.name,PHASE:.status.phase
```

## Performance Issues

### Slow Reconciliation

**Symptom:** Resources take long time to update.

There is no per-controller `--max-concurrent-reconciles-*` flag in v0.4. The reconcilers use controller-runtime's default (`MaxConcurrentReconciles = 1`). If you genuinely need to raise concurrency, the change is in code — `controllers/<kind>_controller.go:SetupWithManager` — not configuration. Open an issue with your scaling profile if the default is a real constraint.

### High CPU/Memory Usage

**Symptom:** Controller pod consuming too many resources

**Solution:**
```bash
# Check resource usage
kubectl top pod -n beskar7-system

# Set resource limits
kubectl edit deployment -n beskar7-system beskar7-controller-manager

# Add resources:
resources:
  limits:
    cpu: 500m
    memory: 512Mi
  requests:
    cpu: 100m
    memory: 128Mi
```

### 12. CAPI Machine stuck at `Provisioned`, never reaches `Running` (Node not associated)

**Symptom:** Beskar7 finishes provisioning — `Beskar7Machine.status.phase` is `Provisioned`, `ProviderID` is set, and the workload node even shows up in the workload cluster's `kubectl get nodes` — but the **CAPI `Machine`** (not the `Beskar7Machine`) never advances from `Provisioned` to `Running`.

```bash
# The CAPI Machine, not the Beskar7Machine:
kubectl get machine <machine-name> -o jsonpath='{.status.phase}'   # shows: Provisioned (not Running)
kubectl get machine <machine-name> -o jsonpath='{.spec.providerID}' # b7://<namespace>/<host-name>

# On the WORKLOAD cluster — what ProviderID did the node self-register?
kubectl --kubeconfig <workload.kubeconfig> get node <node> -o jsonpath='{.spec.providerID}'
# If this is e.g. "k3s://<hostname>" instead of "b7://<namespace>/<host-name>", that is the bug.
```

**Cause:** CAPI marks a `Machine` `Running` only after it matches the Machine's `spec.providerID` to a Node's `spec.providerID` (they must be **equal**). Beskar7 stamps `b7://<namespace>/<host-name>` on the Machine, but the node's kubelet, left to its defaults, self-registers a different value (k3s uses `k3s://<hostname>`). The mismatch means CAPI never associates the Node, so the Machine never reaches `Running` — even though the node itself is healthy and `Ready`.

**Solution:** Tell the node's kubelet to register with the exact ProviderID Beskar7 assigns — `b7://<namespace>/<host-name>` (the `<host-name>` is the **PhysicalHost** name) — via the per-machine bootstrap config. For k3s, add to the `k3s.args` in the bootstrap `#cloud-config`:

```yaml
k3s:
  enabled: true
  args:
    - "--kubelet-arg=provider-id=b7://<namespace>/<host-name>"
```

For kubeadm (CAPI `KubeadmConfig`/`KubeadmConfigTemplate`), set it under `nodeRegistration`:

```yaml
initConfiguration:    # (use joinConfiguration for worker/secondary nodes)
  nodeRegistration:
    kubeletExtraArgs:
      provider-id: "b7://<namespace>/<host-name>"
```

For other distros (k0s, plain kubelet), set the kubelet's `--provider-id` flag to the same value by your distro's mechanism. See [docs/beskar7machine.md → ProviderID & Node association](beskar7machine.md#providerid--node-association) for the full contract.

> **Scaled deployments:** this works when you author the per-machine bootstrap config and know which PhysicalHost the Machine will use (e.g. a single node, or hosts pinned to specific machines). A templated `MachineDeployment` that claims hosts from a pool can't yet pin the per-host ProviderID in a shared template — automatic provision-time delivery is planned (see the D-014 design).

## Getting Help

If you can't resolve your issue:

### 1. Gather Information

```bash
# Controller logs
kubectl logs -n beskar7-system deployment/beskar7-controller-manager > controller-logs.txt

# Resource dumps
kubectl get physicalhost -o yaml > physicalhosts.yaml
kubectl get beskar7machine -o yaml > beskar7machines.yaml

# Events
kubectl get events -A > events.txt

# Redfish test
curl -k -u admin:password https://BMC_IP/redfish/v1/ > redfish-test.json
```

### 2. Open an Issue

https://github.com/projectbeskar/beskar7/issues

Include:
- Beskar7 version
- Kubernetes version
- Hardware details (vendor, BMC version)
- What you were trying to do
- What happened instead
- Logs and resource dumps
- Steps to reproduce

### 3. Community Support

- GitHub Discussions: https://github.com/projectbeskar/beskar7/discussions
- Check existing issues for similar problems
- Join community chat (if available)

## Best Practices

### Avoid Common Mistakes

1. **Don't skip Cluster API installation** - Required prerequisite
2. **Don't skip cert-manager installation** - Required for webhooks
3. **Don't use production hardware for testing** - Test with dedicated hardware first
4. **Don't ignore inspection reports** - They show real hardware capabilities
5. **Don't set unrealistic hardware requirements** - Match to your actual hardware

### Test Incrementally

1. Deploy controller
2. Register ONE PhysicalHost
3. Verify it becomes Available
4. Create ONE Beskar7Machine
5. Monitor inspection process
6. Verify provisioning completes
7. THEN scale up

### Monitor Actively

```bash
# Watch everything
watch kubectl get physicalhost,beskar7machine -o wide

# Follow logs continuously
kubectl logs -n beskar7-system deployment/beskar7-controller-manager -f
```

## FAQ

**Q: Why is my PhysicalHost stuck in Enrolling for 5 minutes?**
A: Controller has exponential backoff for Redfish connection failures. Check connectivity and credentials.

**Q: Inspection keeps timing out, can I increase the timeout?**
A: Currently hardcoded to 10 minutes. If hardware is slow, consider filing an issue for configurable timeout.

**Q: Can I manually trigger inspection again?**
A: Delete and recreate the Beskar7Machine to trigger new inspection.

**Q: How do I reset a PhysicalHost?**
A: Delete the Beskar7Machine that claimed it, and it will return to Available state.

**Q: Controller logs are too verbose, how do I reduce them?**
A: Remove the `-v=X` flag or set to `-v=1` for minimal logging.

---

**Still stuck?** Open an issue: https://github.com/projectbeskar/beskar7/issues
