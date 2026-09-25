# Troubleshooting Beskar7

> **Audience:** Operators

This guide helps you diagnose and resolve common Beskar7 issues.

## A Machine fails with `InspectionTimedOut` and the host is powered off

Look at the host's power state and its boot override:

```bash
kubectl get physicalhost <host> -n <ns> -o jsonpath='{.status.state}/{.status.inspectionPhase}/{.status.observedPowerState}{"\n"}'
```

`InUse/Timeout/Off` with the BMC showing `boot: Pxe` means the host was told to PXE boot but never
powered on, so the inspector never ran. This used to happen when a host was re-claimed immediately
after being released: the power-on decision was made from a reading taken while the previous
consumer's shutdown was still in flight, so it returned `On` and the power-on was skipped.

The controller now notices this: once an inspection is more than two minutes old and the host reads
`Off`, it confirms over Redfish and powers the host back on. If you are on an older version, or the
BMC is genuinely unreachable, delete the `Machine` and let its `MachineSet` or control-plane
provider create a replacement — by then the host has settled and the fresh claim powers it on
correctly.

## Quick Diagnosis

```bash
# Check controller is running
kubectl get pods -n capb7-system

# Check controller logs
kubectl logs -n capb7-system deployment/capb7-controller-manager -f

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
kubectl apply -f https://github.com/kubernetes-sigs/cluster-api/releases/download/v1.11.0/cluster-api-components.yaml
kubectl apply -f https://github.com/kubernetes-sigs/cluster-api/releases/download/v1.11.0/bootstrap-components.yaml
kubectl apply -f https://github.com/kubernetes-sigs/cluster-api/releases/download/v1.11.0/control-plane-components.yaml

# Restart Beskar7
kubectl rollout restart deployment/capb7-controller-manager -n capb7-system
```

### 2. Webhook Fails: "connection refused" or "certificate" errors

**Symptom:**
```
failed calling webhook "validation.beskar7cluster.infrastructure.cluster.x-k8s.io"
x509: certificate signed by unknown authority
```

There is exactly one webhook in v0.4: the Beskar7Cluster validating webhook (`api/v1beta2/webhooks/beskar7cluster_webhook.go`). If your error mentions `physicalhost`, `beskar7machine`, or `beskar7machinetemplate` webhooks, those are stale `ValidatingWebhookConfiguration`/`MutatingWebhookConfiguration` objects left over from a v0.3 install — see step 3 below.

**Cause:** cert-manager not installed or not ready, or webhook serving cert not issued.

**Solution:**
```bash
# Install cert-manager
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.16.2/cert-manager.yaml

# Wait for it to be ready
kubectl wait --for=condition=Available --timeout=300s deployment/cert-manager -n cert-manager

# Verify the chart's Certificate is Ready
kubectl get certificate -n capb7-system

# Restart Beskar7
kubectl rollout restart deployment/capb7-controller-manager -n capb7-system

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
kubectl logs -n capb7-system deployment/capb7-controller-manager | grep inspection

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
kubectl logs -n capb7-system deployment/capb7-controller-manager | grep -i power
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

The callback endpoint authenticates every request via per-host bearer tokens. Failures collapse to an opaque `401` body — the verifier logs every rejection on the manager at default verbosity as `auth: rejected bearer token`, with `host`, `remote` and a `reason` field (never the token itself).

**Diagnosis:**
```bash
# Tail manager logs for rejected bearers (Info level; no --zap-devel needed).
kubectl logs -n capb7-system deployment/capb7-controller-manager -f | grep "rejected bearer token"
```

**Common causes:**

| `reason` | Cause | Fix |
|---|---|---|
| `no bootstrap token issued for host ...` | The Beskar7Machine reconciler has not minted a token yet. | Wait, or check `kubectl describe beskar7machine <name>` for the current phase. |
| `bootstrap token expired for host ...` | More than 60 minutes elapsed since the token was minted (`auth.TokenLifetime`). | Delete the per-host Secret `<host>-bootstrap-token`; the controller mints a fresh one and re-renders the cmdline on next reconcile. The booted host must re-PXE to pick up the new plaintext. |
| `bootstrap token mismatch for host ...` | Plaintext on the wire does not hash to the token in the `<host>-bootstrap-token` Secret. | Stale iPXE cmdline. Compare the token in the kernel cmdline against `kubectl get secret <host>-bootstrap-token -o jsonpath='{.data.plaintext-token}' \| base64 -d`; re-PXE if they diverge. The Secret is the only credential checked (D-029); `status.bootstrap.tokenHash` only mirrors it. |
| `bootstrap-token Secret ... is not bound to the host's current consumer` | The token was minted for a different `Beskar7Machine` than the one the host is claimed by now, or by a release before D-029 on a host that was already `Ready` at the upgrade. | A new claim mints fresh credentials; re-PXE the host to pick them up. See [Security troubleshooting](security/troubleshooting.md). |

Clock skew (> 60 min) between the manager pod and the BMC-managed host can also cause `expired` results — verify NTP on both sides.

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
kubectl edit deployment capb7-controller-manager -n capb7-system

# Add to container args:
spec:
  containers:
  - name: manager
    args:
    - --leader-elect
    - --zap-log-level=debug  # Add this line (debug|info|error, or an integer for finer levels)
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
kubectl get deployment -n capb7-system capb7-controller-manager
# Should show: READY 1/1

# Check controller logs for errors
kubectl logs -n capb7-system deployment/capb7-controller-manager --tail=100 | grep -i error

# Check webhook is healthy
kubectl get endpoints -n capb7-system capb7-webhook-service
```

### PhysicalHost Health

```bash
# List all hosts
kubectl get physicalhost -o wide

# Check for hosts in error state
kubectl get physicalhost -o json | jq '.items[] | select(.status.state=="Error")'

# Check Redfish connectivity
kubectl get physicalhost -o json | jq '.items[] | select(.status.conditions[]? | select(.type=="RedfishConnectionReady" and .status=="False"))'
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

Each controller runs **one reconcile worker by default** (`--max-concurrent-reconciles=1`,
matching controller-runtime). With a single worker, reconciles are serialised — and
because every Redfish call carries a 30s timeout, **one unreachable BMC can occupy the
only worker and stall reconciles for healthy hosts**. That head-of-line blocking, not
raw throughput, is usually what "slow reconciliation" turns out to be.

Check whether a few unreachable BMCs are consuming the worker:

```bash
kubectl get physicalhosts -A -o custom-columns=\
NAME:.metadata.name,STATE:.status.state,ERROR:.status.errorMessage | grep -iv '<none>'
```

If so, raise concurrency so healthy hosts are not queued behind failing ones:

```yaml
- --max-concurrent-reconciles=4
```

This is safe with respect to BMC load: controller-runtime never reconciles the same
object concurrently, so distinct workers always act on distinct `PhysicalHost`s — and
therefore distinct BMCs. Size it to your fleet; there is no benefit to setting it far
above the number of hosts you expect to reconcile in parallel.

### High CPU/Memory Usage

**Symptom:** Controller pod consuming too many resources

**Solution:**
```bash
# Check resource usage
kubectl top pod -n capb7-system

# Set resource limits
kubectl edit deployment -n capb7-system capb7-controller-manager

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

For k0s there is no working kubelet-flag route — the Kairos k0s provider drops `--kubelet-extra-args` — so use the image-side stage [`examples/kairos-k0s-providerid-stage.yaml`](../examples/kairos-k0s-providerid-stage.yaml), which patches the Node right after it registers. For a plain kubelet, set `--provider-id` to the same value by your distro's mechanism. See [docs/beskar7machine.md → ProviderID & Node association](beskar7machine.md#providerid--node-association) for the full contract.

> **Scaled deployments (`MachineDeployment` pools, multi-replica control planes).** A shared
> template cannot hard-code a per-host ProviderID, so since **contract v4.2** the inspector writes
> the correct value for each host to **`/oem/beskar7/provider-id`** (mode `0600`, root-owned, no
> trailing newline) during provisioning. A small stage in the **target image** reads that file, so
> one image and one template serve every replica — see
> [`examples/kairos-providerid-stage.yaml`](../examples/kairos-providerid-stage.yaml), verified
> end to end on Kairos v4.1.2 + k3s v1.34.8 (`Node.spec.providerID = b7://<ns>/<host>`).
>
> Two requirements are easy to get wrong, and both fail **silently**:
>
> 1. **The stage belongs in the image, in yip format — not in the bootstrap Secret as
>    `#cloud-config`.** A file beginning with `#cloud-config` has its top-level keys honored
>    (`hostname`, `users`, `k3s`) but its `stages:` block **ignored**; Kairos processes it as
>    `'<file>.0'` with `commands: 0` and logs nothing that looks like an error. A yip config
>    (top-level `name:` plus `stages:`) executes normally.
> 2. **It must run before k3s first starts.** `Node.spec.providerID` is immutable once a node
>    registers. A node that joins without the flag keeps the distro default (`k3s://<hostname>`)
>    and **cannot be corrected in place** — it has to be re-provisioned. Baking the stage into the
>    image guarantees the ordering; adding it after the fact does not.
>
> Requires a controller **and** inspector both at v4.2 or later — a v4.1 inspector ignores the new
> cmdline parameter and never writes the file, so the stage is a no-op and the Machine stays at
> `Provisioned`. Check with `cat /oem/beskar7/provider-id` on the host; when the stage has run it
> also leaves `/oem/.beskar7-providerid-applied`.

### If no Node appears because the host never left the inspector

**Symptom:** the `Beskar7Machine` is `Provisioned` and its `PhysicalHost` is `Ready`, but no Node ever
registers, and the host's console (serial or BMC KVM) still shows the inspector. After
`beskar7-inspector: bootstrap data received, provisioning` it ends with
`beskar7-inspector: run failed: exhausted retries; last error: …` (or `callback returned HTTP …`),
and there is no `provisioned-complete callback accepted` line.

**Cause:** the inspector reboots into the deployed OS only once the controller has acknowledged
`POST /api/v1/provisioned` ([contract §9.1 step 6](inspector-contract.md#91-required-steps)), and it
retries that call for about 2.5 minutes. Here the call reached the controller — that is why the host is
`Ready` — but no response got back within those retries, because of a network partition or a callback
server that went away right after it took the report. The inspector stopped without rebooting. The disk
is complete, join config included, and nothing boots it.

**Solution:** a `MachineHealthCheck` replaces the machine by itself: `nodeStartupTimeoutSeconds` counts
from when the machine went `Provisioned` (45 minutes with the recommended value), and the replacement
re-provisions the host. To keep the deployment instead, power-cycle the host through its BMC. It boots
the deployed OS from the disk and the node joins as it would have; if the host network-boots instead,
clear its one-time boot override on the BMC and power-cycle it again. Do this only while the
`PhysicalHost` is `Ready`: on a host still `Deploying` the controller never recorded the call, and the
node that boots would have no provisioned `Machine` behind it.

### If the ProviderID matches and the Node still never appears

A mismatch is the common cause, but if `Node.spec.providerID` is correct and the Machine still
never reaches `Running`, the Node is not registering at all — the OS booted but the distro never
started, or it cannot reach the control plane.

Beskar7 cannot detect this: observing the workload Node requires the workload kubeconfig, which an
infrastructure provider does not hold (see `docs/inspector-contract.md` §13). CAPI models it with
`MachineHealthCheck.spec.checks.nodeStartupTimeoutSeconds` — **recommended `2700` (45m)** — which
remediates a machine that never produced a Node. That clock covers provisioning as well as the
join: until the Machine's `InfrastructureReady` turns `True`, CAPI counts from the Machine's creation
(or the control plane's initialisation), so a value shorter than a whole provisioning run replaces
machines that are still being inspected or deployed. See
[`examples/machinehealthcheck.yaml`](../examples/machinehealthcheck.yaml),
[Beskar7Machine → Remediating with a `MachineHealthCheck`](beskar7machine.md#remediating-with-a-machinehealthcheck),
and [Upgrading](upgrading.md) if you have an older `cluster.x-k8s.io/v1beta1`-shaped
`MachineHealthCheck` to convert.

Note that remediation is **destructive**: CAPI deletes the Machine and beskar7 re-provisions the
host with a whole-disk overwrite. Keep `spec.remediation.triggerIf` (`unhealthyLessThanOrEqualTo` /
`unhealthyInRange`) set so a fleet-wide fault (a bad image digest, an unreachable boot server)
cannot put the whole pool into a reprovision loop.

A `Beskar7Machine` that beskar7 itself marks terminally failed (see
[Beskar7Machine → Terminal failures](beskar7machine.md#terminal-failures)) surfaces as
`InfrastructureReady=False` on the owning `Machine`, and one that failed while provisioning never
produces a Node either, so `nodeStartupTimeoutSeconds` remediates it. A `MachineHealthCheck` never sees
the reason, though: a machine that is still being inspected or deployed, or waiting for a host or for
its BMC ([`WaitingForBMC`](#15-beskar7machine-reports-waitingforbmc)), reads exactly the same. Only
the timeouts separate a failure from a slow run, which is why both — `nodeStartupTimeoutSeconds` and
the `timeoutSeconds` of an `unhealthyMachineConditions` entry on `InfrastructureReady` — have to
outlast a whole provisioning run.

### 13. k0s control plane never forms: joins hang, or a joiner became its own cluster

**Symptoms:** a `KairosControlPlane` with `distribution: k0s` stays at one ready replica. On the
init node `k0s etcd member-list` shows a second member, and etcd logs `ReadIndex response took too
long`; every later joiner's `k0scontroller` retries its join forever. In the other form, a joiner
is `Ready` but `k0s kubectl get nodes` run on it lists only itself, and its CA differs from the init
node's.

**Cause:** the image has no start gate. Beskar7's whole-disk image installs itself from the
recovery partition on its first boot, and Kairos applies the CAPI cloud-config on that boot too, so
a joiner registers as a voting etcd member from the installer and is then rebooted by it — a
two-member etcd with one dead voter never regains quorum. Separately, the Kairos k0s provider
starts k0s a few seconds before it writes k0s's arguments; a bare `k0s controller` initialises a
cluster of its own and k0s never attempts a join once a CA exists. Neither is reachable on k3s.

**Solution:** bake [`examples/kairos-k0s-start-gate.yaml`](../examples/kairos-k0s-start-gate.yaml)
into the image as `/oem/05_beskar7_k0s_gate.yaml` ([Building a target image → k0s: the start
gate](building-images.md#k0s-the-start-gate)) and run a cluster-api-provider-kairos that writes
`/etc/k0s/.capi-args-ready` (commit `3698d55` on `fix/generic-infrastructure-provider`). Then
re-provision: delete the affected Machines (or let a `MachineHealthCheck` remediate). There is no
in-place fix — a node that has initialised its own CA will not join, and a dead etcd voter has to
be removed from the init node with `k0s etcd leave --peer-address <addr>` before it will accept
new members.

**Verify on a provisioned host:** `/etc/k0s/.capi-args-ready` exists; `systemctl show k0scontroller
-p ConditionResult -p NRestarts` prints `yes` and `0`; and the install boot's journal — the machine-id
directory under `/var/log/journal/` that is not the current one — has no k0s output:
`sudo journalctl -D /var/log/journal/<other-id> -o cat | grep -c 'k0s\['` is `0`. Before the gate,
that journal is where the premature join showed up.

### 14. Reconcile errors storm with `the object has been modified`: two full managers share a namespace

**Symptom:** the manager logs hundreds of lines a minute like
`Operation cannot be fulfilled on physicalhosts.infrastructure.cluster.x-k8s.io "<host>": the object has been modified; please apply your changes to the latest version and try again`,
the `infrastructure.cluster.x-k8s.io/bootstrap-url` annotation on a PhysicalHost appears, disappears
and reappears with a different value, or a host is claimed by a Beskar7Machine, released and claimed
again by another.

**Cause:** two manager instances are running every controller against the same namespaces. The usual
way to get there is a second copy started on the boot server so that PXE-booting hosts can reach the
callback endpoints ([iPXE setup → callback-only instance](ipxe-setup.md#management-cluster-off-the-provisioning-network-a-callback-only-instance)),
but started as a full manager. Two Beskar7Machine controllers then race for hosts, and if their
`--bootstrap-url-base` values differ each rewrites `PhysicalHost.Status.Bootstrap.URL` to its own
value through the annotation, forever. Leader election does not protect against this: the
out-of-cluster copy runs with `--leader-elect=false` because it has no in-cluster namespace to hold
the lease in.

**Solution:** restart the second copy with `--controllers=none`. It keeps serving `/boot`,
`/api/v1/inspection`, `/api/v1/bootstrap` and `/api/v1/provisioned` and registers no reconciler, so
the conflicts stop as soon as it comes back. Give both instances the same `--bootstrap-url-base`: the
in-cluster controller writes that value into `PhysicalHost.Status.Bootstrap.URL` and the callback-only
instance renders it into the iPXE cmdline.

### 15. Beskar7Machine reports `WaitingForBMC`

**Symptom:** the `Beskar7Machine` has `InfrastructureReady=False` with reason `WaitingForBMC`, and its
`PhysicalHost` is in `Error` with `RedfishConnectionReady=False`, reason `BMCUnreachable`, message
`BMC unreachable (connection refused); retrying every 15s` or similar.

**Cause:** the controller cannot reach the host's BMC at the network level — a refused or reset
connection, no route, a DNS failure, a timeout, or a 502/503/504 from a BMC that is still starting.
This is not a terminal failure: `status.phase` is not `Failed`, the host retries every 15 seconds, and
the machine carries on by itself on the first attempt that connects. A host that was already
`Inspecting`, `Deploying` or `Ready` keeps that state through the outage (only its condition changes)
and goes on with its provisioning, so a machine whose host got that far never shows this reason.

**Solution:** nothing to delete. If the outage does not clear, check the path from the controller pod to
the BMC (the checks under [PhysicalHost Stuck in "Enrolling"](#4-physicalhost-stuck-in-enrolling) → BMC
Not Reachable apply). A `MachineHealthCheck` cannot tell this reason from a terminal one, so its
timeouts decide whether it waits. The recommended ones
([`examples/machinehealthcheck.yaml`](../examples/machinehealthcheck.yaml)) wait as long as the machine
can still finish provisioning within `nodeStartupTimeoutSeconds` (45 minutes, counted from the Machine's
creation or the control plane's initialisation); a longer outage gets the machine replaced, which costs
the host nothing, because this reason only appears before inspection starts. See
[Beskar7Machine → A BMC outage is not a terminal failure](beskar7machine.md#a-bmc-outage-is-not-a-terminal-failure).
If the BMC is gone for good, delete the machine with the `force-release` annotation
([State Management → Force release](state-management.md#force-release)).

### 16. k0s worker goes `NotReady` after a reboot and never rejoins: `another k0s process is still running`

**Symptom:** a k0s worker that joined normally at provisioning is `NotReady` after its first reboot
and stays that way. On the host, `k0sworker` restarts every few seconds and its journal repeats
`failed to initialize runtime config: another k0s process is still running`, while
`systemctl is-active k0scontroller` prints `active` on what is a worker, and `/run/k0s/` holds a
controller's `etcd.pid` and `kube-apiserver.pid`. Control-plane nodes come back normally.

**Cause:** the image was built with [`examples/kairos-k0s-providerid-stage.yaml`](../examples/kairos-k0s-providerid-stage.yaml)
from beskar7 v0.4.4 through v0.7.0 — every release that shipped it — whose
`beskar7-k0s-providerid.service` carried `Wants=k0scontroller.service`. The Kairos k0s image ships
both k0s unit files on every node and only *disables* the one a node does not run, and `Wants=`
starts a disabled unit. On a worker that brings up a stand-alone `k0s controller`, which takes k0s's
runtime lock before `k0sworker` can. On the first boot the [start gate](#13-k0s-control-plane-never-forms-joins-hang-or-a-joiner-became-its-own-cluster)
skips it, because `/etc/k0s/.capi-args-ready` is not written yet — by milliseconds. Kairos keeps
`/etc/k0s` across reboots, so from the second boot on the marker is already there and the controller
starts.

**Solution:** rebuild the image with the current stage, which orders after the k0s units with
`After=` and never pulls them in, and re-provision. To bring a running worker back without
re-imaging, stop the stray controller — `sudo systemctl stop k0scontroller` — and `k0sworker`'s own
restart takes the lock and rejoins within seconds. That lasts until the next reboot. To fix a node in
place, replace `/oem/10_beskar7_providerid.yaml` **and** delete the `Wants=` line from
`/etc/systemd/system/beskar7-k0s-providerid.service`: `/etc/systemd` persists too, and the next boot
starts the unit it finds there before the stage rewrites it, so fixing `/oem` alone costs one more
failed boot.

**Verify on the host after a reboot:** `systemctl show k0scontroller -p WantedBy` is empty,
`journalctl -b -u k0scontroller` has no lines at all, `k0sworker` is `active`, and
`/run/k0s/etcd.pid` does not exist.

## Getting Help

If you can't resolve your issue:

### 1. Gather Information

```bash
# Controller logs
kubectl logs -n capb7-system deployment/capb7-controller-manager > controller-logs.txt

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
kubectl logs -n capb7-system deployment/capb7-controller-manager -f
```

## FAQ

**Q: Why is my PhysicalHost stuck in Enrolling for 5 minutes?**
A: A Redfish failure that needs something to change — a wrong address, wrong credentials, a rejected certificate — backs off exponentially, up to 30 minutes between attempts, so the host keeps the error for a while after you fix it. Edit the `PhysicalHost` or its credentials Secret to wake the controller at once. A BMC that is merely unreachable is different: it is retried every 15 seconds and enrols on the first attempt that connects.

**Q: Inspection keeps timing out, can I increase the timeout?**
A: Yes — the manager's `--inspection-timeout` flag (default `10m`). Raise it for hardware with a slow POST. `--deployment-timeout` covers the image-write phase separately.

**Q: Can I manually trigger inspection again?**
A: Delete and recreate the Beskar7Machine to trigger new inspection.

**Q: How do I reset a PhysicalHost?**
A: Delete the Beskar7Machine that claimed it, and it will return to Available state.

**Q: Controller logs are too verbose, how do I reduce them?**
A: Remove the `--zap-log-level` override, or set it back to `info`.

---

**Still stuck?** Open an issue: https://github.com/projectbeskar/beskar7/issues
