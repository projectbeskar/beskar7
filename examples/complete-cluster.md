# Complete Cluster Deployment Example

This example demonstrates a complete Kubernetes cluster deployment using Beskar7 with bare-metal machines managed via Redfish API.

## Overview

This example creates a fully functional cluster with:
- **1 Control Plane Node** - Running Kubernetes control plane components
- **2 Worker Nodes** - Running workload pods
- **Kairos OS** - Immutable Linux distribution optimized for Kubernetes
- **Cluster API Integration** - Full integration with CAPI resources

## Architecture

```

                    Beskar7 Controller                       
              (Manages PhysicalHosts & Machines)             

                            
                
                                      
       v      v
        PhysicalHost         PhysicalHost   
        control-plane          worker-01    
        (172.16.56.101)      (172.16.56.102)
             
                           
                   v
                    PhysicalHost   
                      worker-02    
                    (172.16.56.103)
                   
```

## Prerequisites

### 1. Beskar7 Installed

```bash
# Install Beskar7
kubectl apply -f https://github.com/projectbeskar/beskar7/releases/download/v0.3.0/beskar7-manifests-v0.3.0.yaml

# Verify installation
kubectl get pods -n beskar7-system
```

### 2. Cluster API Core Components

```bash
# Install Cluster API
clusterctl init

# Verify CAPI is ready
kubectl get pods -n capi-system
kubectl get pods -n capi-kubeadm-bootstrap-system
kubectl get pods -n capi-kubeadm-control-plane-system
```

### 3. cert-manager

```bash
# Install cert-manager
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.16.2/cert-manager.yaml

# Verify cert-manager is ready
kubectl wait --for=condition=Available --timeout=300s deployment/cert-manager -n cert-manager
```

### 4. Physical Infrastructure

You need:
- **3 bare-metal servers** with Redfish-compatible BMC
- **Network connectivity** from Kubernetes cluster to BMC IPs
- **BMC credentials** (username/password)
- **Web server** hosting Kairos configuration files (or use PreBakedISO mode)

## Configuration Steps

### Step 1: Update BMC Credentials

Edit the `bmc-credentials` Secret in the manifest:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: bmc-credentials
  namespace: default
type: Opaque
stringData:
  username: "admin"          # Your BMC username
  password: "your-password"  # Your BMC password
```

### Step 2: Update BMC IP Addresses

Update the `redfishConnection.address` in each `PhysicalHost`:

```yaml
spec:
  redfishConnection:
    address: "https://172.16.56.101"  # Replace with actual BMC IP
    credentialsSecretRef: "bmc-credentials"
    insecureSkipVerify: true
```

### Step 3: Update Control Plane Endpoint

Update the `controlPlaneEndpoint` in `Beskar7Cluster`:

```yaml
spec:
  controlPlaneEndpoint:
    host: "172.16.56.10"  # Your VIP or load balancer IP
    port: 6443
```

This can be:
- A static IP on the control plane node
- A virtual IP managed by keepalived/kube-vip
- A load balancer IP

### Step 4: Configure Kairos Config URLs

Update the `configURL` in `Beskar7Machine` and `Beskar7MachineTemplate` resources:

```yaml
spec:
  provisioningMode: "RemoteConfig"
  configURL: "https://your-server.com/config/control-plane-config.yaml"
```

**Alternative**: Use `PreBakedISO` mode:

```yaml
spec:
  provisioningMode: "PreBakedISO"
  imageURL: "https://your-server.com/custom-images/control-plane.iso"
```

## Kairos Configuration Files

### Control Plane Configuration

Create `control-plane-config.yaml`:

```yaml
#cloud-config

hostname: control-plane-{{ trunc 4 .MachineID }}

stages:
  rootfs:
    - name: "Setup SSH"
      authorized_keys:
        - "ssh-rsa AAAA... your-ssh-key"

  initramfs:
    - name: "Setup networking"
      commands:
        - |
          cat > /etc/systemd/network/20-wired.network <<EOF
          [Match]
          Name=eth*

          [Network]
          DHCP=yes
          EOF

  boot:
    - name: "Install Kubernetes"
      commands:
        - kubeadm init --config /etc/kubernetes/kubeadm-config.yaml
```

### Worker Configuration

Create `worker-config.yaml`:

```yaml
#cloud-config

hostname: worker-{{ trunc 4 .MachineID }}

stages:
  rootfs:
    - name: "Setup SSH"
      authorized_keys:
        - "ssh-rsa AAAA... your-ssh-key"

  initramfs:
    - name: "Setup networking"
      commands:
        - |
          cat > /etc/systemd/network/20-wired.network <<EOF
          [Match]
          Name=eth*

          [Network]
          DHCP=yes
          EOF

  boot:
    - name: "Join Kubernetes cluster"
      commands:
        - kubeadm join {{ .ControlPlaneEndpoint }} --token {{ .Token }} --discovery-token-ca-cert-hash {{ .CACertHash }}
```

## Deployment

### Deploy the Complete Cluster

```bash
# Apply the complete manifest
kubectl apply -f examples/complete-cluster.yaml

# Watch the deployment progress
watch kubectl get physicalhost,beskar7machine,beskar7cluster,cluster
```

### Monitor Progress

```bash
# Check PhysicalHost status
kubectl get physicalhost -w

# Expected output:
# NAME                STATE       POWER   BOOT
# control-plane-01    Available   On      ISO
# worker-01           Available   On      ISO
# worker-02           Available   On      ISO

# Check Beskar7Machine status
kubectl get beskar7machine -w

# Check cluster status
kubectl get cluster my-cluster -o yaml
```

### Verify Cluster Creation

```bash
# Get the kubeconfig for the new cluster
clusterctl get kubeconfig my-cluster > my-cluster.kubeconfig

# Check nodes in the new cluster
kubectl --kubeconfig=my-cluster.kubeconfig get nodes

# Expected output:
# NAME                        STATUS   ROLES           AGE   VERSION
# my-cluster-control-plane-0  Ready    control-plane   10m   v1.31.0
# my-cluster-workers-abc123   Ready    <none>          8m    v1.31.0
# my-cluster-workers-def456   Ready    <none>          8m    v1.31.0
```

## Customization Options

### Scaling Workers

To add more worker nodes:

```bash
# Edit the MachineDeployment
kubectl edit machinedeployment my-cluster-workers

# Change spec.replicas to desired count
spec:
  replicas: 5  # Scale to 5 workers
```

### High Availability Control Plane

To create an HA control plane with 3 nodes:

```yaml
apiVersion: controlplane.cluster.x-k8s.io/v1beta1
kind: KubeadmControlPlane
metadata:
  name: my-cluster-control-plane
spec:
  replicas: 3  # Change from 1 to 3
```

Make sure you have 3 PhysicalHosts labeled for control-plane role.

### Custom Kubernetes Version

```yaml
apiVersion: controlplane.cluster.x-k8s.io/v1beta1
kind: KubeadmControlPlane
spec:
  version: v1.30.0  # Change Kubernetes version
```

### Different OS Family

Replace `kairos` with other supported OS families:

```yaml
spec:
  osFamily: "flatcar"
  imageURL: "https://stable.release.flatcar-linux.net/amd64-usr/current/flatcar_production_iso_image.iso"
```

Supported OS families:
- `kairos` - Kairos (recommended)
- `flatcar` - Flatcar Container Linux
- `LeapMicro` - openSUSE Leap Micro

## Troubleshooting

### PhysicalHost Not Becoming Available

```bash
# Check PhysicalHost status
kubectl describe physicalhost control-plane-01

# Check controller logs
kubectl logs -n beskar7-system deployment/controller-manager -c manager -f
```

Common issues:
- BMC credentials incorrect
- BMC not reachable from cluster
- Redfish API not enabled on BMC

### Machine Not Provisioning

```bash
# Check Beskar7Machine status
kubectl describe beskar7machine my-cluster-control-plane-0

# Check events
kubectl get events --sort-by='.lastTimestamp'
```

Common issues:
- No available PhysicalHost with matching labels
- ISO URL not accessible from server
- Config URL not accessible (RemoteConfig mode)

### Cluster Not Becoming Ready

```bash
# Check cluster status
kubectl describe cluster my-cluster

# Check control plane status
kubectl describe kubeadmcontrolplane my-cluster-control-plane
```

Common issues:
- Control plane endpoint not reachable
- kubeadm init/join failures
- Network plugin not installed

## Cleanup

To delete the cluster and all resources:

```bash
# Delete the cluster (this will also delete machines and infrastructure)
kubectl delete cluster my-cluster

# Wait for cleanup to complete
kubectl wait --for=delete cluster/my-cluster --timeout=600s

# PhysicalHosts will be released and become Available again
kubectl get physicalhost

# Delete the secret
kubectl delete secret bmc-credentials
```

## Advanced Scenarios

### Using PreBakedISO Mode

For faster provisioning with pre-configured ISOs:

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
kind: Beskar7Machine
spec:
  osFamily: "kairos"
  provisioningMode: "PreBakedISO"
  imageURL: "https://your-server.com/images/control-plane-node.iso"
  # configURL not needed in PreBakedISO mode
```

### Using Different Boot Modes

```yaml
spec:
  bootMode: "Legacy"  # For Legacy BIOS boot
  # or
  bootMode: "UEFI"    # For UEFI boot (default)
```

### Network Segmentation

To use different networks for control plane and workers:

```yaml
# Control plane template
spec:
  template:
    spec:
      networkConfig:
        interfaces:
          - name: eth0
            addresses:
              - 172.16.56.10/24

# Worker template
spec:
  template:
    spec:
      networkConfig:
        interfaces:
          - name: eth0
            addresses:
              - 172.16.57.10/24
```

## Next Steps

After your cluster is running:

1. **Install CNI**: Deploy a network plugin (Calico, Cilium, etc.)
2. **Install CSI**: Deploy storage drivers for persistent volumes
3. **Install Ingress**: Deploy ingress controller for external access
4. **Deploy Workloads**: Start deploying your applications

## See Also

- [PhysicalHost Documentation](../docs/physicalhost.md)
- [Beskar7Machine Documentation](../docs/beskar7machine.md)
- [Beskar7Cluster Documentation](../docs/beskar7cluster.md)
- [Troubleshooting Guide](../docs/troubleshooting.md)
- [Vendor-Specific Support](../docs/vendor-specific-support.md)

