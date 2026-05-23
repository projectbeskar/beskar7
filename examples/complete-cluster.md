# Complete Cluster Deployment Example

This walkthrough deploys a 1-control-plane + 2-worker Kubernetes cluster on bare metal using Beskar7 v0.4.0-alpha.4. The accompanying YAML is [`complete-cluster.yaml`](complete-cluster.yaml).

## Prerequisites

1. **Beskar7 controller installed and Ready.** Either Helm (`helm install beskar7 beskar7/beskar7 --namespace beskar7-system --create-namespace`) or release manifests. Verify:
   ```bash
   kubectl get pods -n beskar7-system
   ```
2. **Cluster API core + KubeadmControlPlane + KubeadmBootstrap providers.**
   ```bash
   clusterctl init
   ```
3. **cert-manager.** See [Installation](../docs/installation.md#prerequisites).
4. **iPXE infrastructure.** DHCP + HTTP boot server reachable from the bare-metal nodes' management network. See [iPXE Setup](../docs/ipxe-setup.md).
5. **The beskar7-inspector image and target OS image hosted on the boot server.**
6. **Three bare-metal servers with Redfish-compatible BMCs**, with credentials and IP addresses for each BMC.

## What the manifest deploys

| Object | Count | Purpose |
|---|---|---|
| `Secret/bmc-credentials` | 1 | BMC username + password. |
| `PhysicalHost` | 3 | One per bare-metal server. |
| `Beskar7Cluster` | 1 | CAPI infra-cluster; tracks control-plane endpoint and failure domains. |
| `Cluster` (CAPI) | 1 | The CAPI Cluster that owns Beskar7Cluster. |
| `Beskar7MachineTemplate` (control plane) | 1 | Schema for the control-plane Beskar7Machines. |
| `KubeadmControlPlane` | 1 | Drives `replicas: 1` control-plane Machine. |
| `Beskar7MachineTemplate` (workers) | 1 | Schema for the worker Beskar7Machines. |
| `MachineDeployment` | 1 | Drives `replicas: 2` worker Machines. |
| `KubeadmConfigTemplate` | 1 | Worker bootstrap config (kubeadm join). |

Each `Beskar7Machine` claims a `PhysicalHost`, runs the iPXE inspection workflow, validates hardware against `hardwareRequirements`, then kexecs into the target OS.

## Configuration steps

### 1. Update BMC credentials

Edit the Secret:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: bmc-credentials
  namespace: default
type: Opaque
stringData:
  username: "admin"
  password: "your-password"
```

### 2. Update BMC IP addresses

Update `spec.redfishConnection.address` on each `PhysicalHost`:

```yaml
spec:
  redfishConnection:
    address: "https://172.16.56.101"   # actual BMC IP
    credentialsSecretRef: "bmc-credentials"
    insecureSkipVerify: true            # set to false in production
```

For TLS-verified BMC connections in production, see [Security Configuration](../docs/security/configuration.md) — provide a `caBundleSecretRef` and leave `insecureSkipVerify: false`.

### 3. Update the control-plane endpoint

Update `Beskar7Cluster.spec.controlPlaneEndpoint.host`:

```yaml
spec:
  controlPlaneEndpoint:
    host: "172.16.56.10"  # VIP / load balancer
    port: 6443
```

This must be a stable address that survives a control-plane node move — typically a virtual IP managed by `kube-vip`/`keepalived`, or an external load balancer.

### 4. Update image URLs

Update `inspectionImageURL`, `targetImageURL`, and `configurationURL` on the two `Beskar7MachineTemplate` objects. Each must match `^https?://.*` and be reachable from the bare-metal nodes' boot network. Examples:

```yaml
inspectionImageURL: "http://boot-server.local/ipxe/inspect.ipxe"
targetImageURL:     "http://boot-server.local/images/kairos-alpine-v2.8.1.tar.gz"
configurationURL:   "http://boot-server.local/configs/control-plane.yaml"
```

For details on how the kernel cmdline is constructed from these URLs, see [iPXE Setup: kernel cmdline](../docs/ipxe-setup.md).

### 5. Adjust hardware requirements

Each template carries a `hardwareRequirements` block. The inspection workflow validates the report against these — if any minimum is violated, the Beskar7Machine fails terminally with `HardwareRequirementsNotMet`. Tune them to match your actual hardware:

```yaml
hardwareRequirements:
  minCPUCores: 4    # summed across all CPUs in the report
  minMemoryGB: 16   # summed across all DIMMs (decimal GB; "16GB" or "16GiB" both accepted)
  minDiskGB:   100  # summed across all disks
```

## Deploy

```bash
kubectl apply -f examples/complete-cluster.yaml
```

Watch progress:

```bash
kubectl get physicalhost,beskar7machine,beskar7cluster,cluster -n default -w
```

Expected sequence per host:

```
NAME             STATE         READY
control-plane-01 Available     true       # BMC reachable
control-plane-01 InUse         true       # Beskar7Machine claimed it
control-plane-01 Inspecting    true       # iPXE booted the inspection image
control-plane-01 Ready         true       # report validated, ready for kexec
```

## Verify the cluster

Once the control-plane node reports `Ready` and joined as a Kubernetes node, fetch its kubeconfig and check status:

```bash
clusterctl get kubeconfig my-cluster -n default > my-cluster.kubeconfig
kubectl --kubeconfig=my-cluster.kubeconfig get nodes
```

## Scaling

### More workers

```bash
kubectl scale machinedeployment my-cluster-workers --replicas=5 -n default
```

The `MachineDeployment` mints additional `Beskar7Machine` objects. Each claims an `Available` `PhysicalHost`. If no host is `Available`, the new machines stay in `WaitingForPhysicalHost` until one is freed or registered.

### HA control plane

Edit `KubeadmControlPlane.spec.replicas`:

```yaml
spec:
  replicas: 3
```

Make sure you have at least three `PhysicalHost` objects available; a control-plane VIP (`172.16.56.10` in this example) that fronts the API server; and `certSANs` on `kubeadmConfigSpec.clusterConfiguration.apiServer` covering all control-plane addresses.

## Cleanup

```bash
kubectl delete cluster my-cluster -n default
kubectl wait --for=delete cluster/my-cluster -n default --timeout=600s
```

CAPI walks the owner chain: deleting `Cluster` deletes `KubeadmControlPlane`, `MachineDeployment`, `Machine`, and `Beskar7Machine`. Each Beskar7Machine deletion runs the BMC release flow (best-effort `ClearBootSourceOverride` + `SetPowerState(Off)` graceful) before clearing `ConsumerRef` on the host. The `PhysicalHost` then transitions back to `Available`.

If a BMC is unreachable and a Beskar7Machine deletion is stuck, set the force-release annotation before deleting:

```bash
kubectl annotate beskar7machine <name> \
  -n default \
  infrastructure.cluster.x-k8s.io/force-release=true
```

## Troubleshooting

| Symptom | Likely cause | Where to look |
|---|---|---|
| `PhysicalHost` stuck in `Enrolling` | BMC unreachable or wrong credentials. | `kubectl describe physicalhost <name>` — check `RedfishConnectionReady` reason. |
| `Beskar7Machine` stuck in `WaitingForPhysicalHost` | No `Available` host with matching namespace. | `kubectl get physicalhost`. Register more hosts or release one. |
| `PhysicalHost` stuck in `Inspecting` | iPXE not delivering the inspection image, or the inspector cannot reach the manager's callback endpoint on `:8082`. | Server serial console; `kubectl logs -n beskar7-system deployment/beskar7-controller-manager`. |
| `FailureReason: HardwareRequirementsNotMet` | Inspection report below the configured minimums. | `kubectl get physicalhost <name> -o jsonpath='{.status.inspectionReport}' \| jq` and compare with `hardwareRequirements`. |
| `FailureReason: BootstrapDataUnavailable` | The Secret named by `Machine.Spec.Bootstrap.DataSecretName` does not exist. | Wait for the bootstrap provider to reconcile, or check the `KubeadmConfig` for the missing config. |

See [Troubleshooting](../docs/troubleshooting.md) for deeper coverage.

## See also

- [PhysicalHost](../docs/physicalhost.md)
- [Beskar7Machine](../docs/beskar7machine.md)
- [State Management](../docs/state-management.md)
- [iPXE Setup](../docs/ipxe-setup.md)
- [Security Configuration](../docs/security/configuration.md)
