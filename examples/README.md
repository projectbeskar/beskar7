# Beskar7 Examples

This directory contains example configurations for various Beskar7 deployment scenarios.

## Available Examples

### [minimal-test.yaml](minimal-test.yaml)
**Use Case:** Quick testing with a single server

**What's Included:**
- 1 PhysicalHost
- 1 Beskar7Machine
- Minimal configuration
- No hardware requirements

**Quick Start:**
```bash
kubectl apply -f minimal-test.yaml

# Watch progress
kubectl get physicalhost test-server -w
kubectl get beskar7machine test-machine -w
```

### [simple-cluster.yaml](simple-cluster.yaml)
**Use Case:** Small production-like cluster

**What's Included:**
- 3 PhysicalHosts
- 1 Control plane node
- 2 Worker nodes
- Hardware requirements
- Full Cluster API integration

**Deploy:**
```bash
# Prerequisites: Cluster API installed
kubectl apply -f simple-cluster.yaml

# Monitor cluster creation
kubectl get cluster test-cluster
kubectl get machines
kubectl get beskar7machines
```

## Example Workflow

### 1. Register Physical Hosts

First, register all your physical servers:

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
kind: PhysicalHost
metadata:
  name: server-01
spec:
  redfishConnection:
    address: "https://bmc-ip"
    credentialsSecretRef: "bmc-credentials"
```

Check they become `Available`:

```bash
kubectl get physicalhosts
# NAME        STATE       READY   AGE
# server-01   Available   true    1m
# server-02   Available   true    1m
# server-03   Available   true    1m
```

### 2. Create Machines

Create Beskar7Machine resources:

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
kind: Beskar7Machine
metadata:
  name: worker-01
spec:
  inspectionImageURL: "http://boot-server/ipxe/inspect.ipxe"
  targetImageURL: "http://boot-server/images/kairos.tar.gz"
  configurationURL: "http://boot-server/configs/worker.yaml"
  hardwareRequirements:
    minCPUCores: 4
    minMemoryGB: 8
```

### 3. Monitor Inspection

Watch the inspection process:

```bash
# Check machine phase
kubectl get beskar7machine worker-01 -o jsonpath='{.status.phase}'
# Output: Inspecting

# Check PhysicalHost inspection phase
kubectl get physicalhost server-01 -o jsonpath='{.status.inspectionPhase}'
# Output: InProgress -> Complete

# View inspection report
kubectl get physicalhost server-01 -o jsonpath='{.status.inspectionReport}' | jq
```

Example inspection report (the actual shape — `cpus`/`memory`/`disks`/`nics` are arrays of structs, not flat objects; full definitions in `api/v1beta1/physicalhost_types.go`):

```json
{
  "timestamp": "2026-05-23T10:30:00Z",
  "manufacturer": "Dell Inc.",
  "model": "PowerEdge R730",
  "serialNumber": "ABC1234",
  "bootModeDetected": "UEFI",
  "firmwareVersion": "2.15.0",
  "cpus": [
    {
      "id": "cpu0",
      "vendor": "GenuineIntel",
      "model": "Intel Xeon E5-2640 v4",
      "cores": 16,
      "threads": 32,
      "frequency": "2.4GHz"
    }
  ],
  "memory": [
    {
      "id": "DIMM0",
      "type": "DDR4",
      "capacity": "32GiB",
      "speed": "2400MHz"
    },
    {
      "id": "DIMM1",
      "type": "DDR4",
      "capacity": "32GiB",
      "speed": "2400MHz"
    }
  ],
  "disks": [
    {
      "name": "sda",
      "model": "Samsung 870 EVO",
      "sizeGB": 500,
      "type": "SSD",
      "serialNumber": "S5H1NS0T123456"
    }
  ],
  "nics": [
    {
      "name": "eth0",
      "macAddress": "00:25:90:f0:79:00",
      "driver": "ixgbe",
      "speed": "1Gbps",
      "ipAddresses": ["10.0.0.42"]
    }
  ]
}
```

### 4. Verify Provisioning

Check the final state:

```bash
# Machine should be ready
kubectl get beskar7machine worker-01
# NAME        PHASE        READY
# worker-01   Provisioned  true

# PhysicalHost should be ready
kubectl get physicalhost server-01
# NAME        STATE   READY
# server-01   Ready   true
```

## Hardware Requirements Example

Specify minimum requirements to ensure machines meet your needs:

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
kind: Beskar7Machine
metadata:
  name: high-performance-worker
spec:
  inspectionImageURL: "http://boot-server/ipxe/inspect.ipxe"
  targetImageURL: "http://boot-server/images/kairos.tar.gz"
  
  # Strict hardware requirements
  hardwareRequirements:
    minCPUCores: 32      # Need at least 32 cores
    minMemoryGB: 128     # Need at least 128 GB RAM
    minDiskGB: 1000      # Need at least 1 TB disk
```

If hardware doesn't meet requirements:
- Machine will not be provisioned
- Status will show validation error
- You can adjust requirements or use different hardware

## Troubleshooting

### Inspection Doesn't Start

```bash
# Check PhysicalHost state
kubectl describe physicalhost server-01

# Look for:
# - Redfish connection issues
# - PXE boot configuration errors
# - Power management failures
```

### Inspection Times Out

```bash
# Check inspection phase
kubectl get physicalhost server-01 -o jsonpath='{.status.inspectionPhase}'

# Possible causes:
# - iPXE infrastructure not configured
# - Server can't reach boot server
# - Network boot disabled in BIOS
# - Wrong boot order
```

### Hardware Validation Failed

```bash
# View inspection report
kubectl get physicalhost server-01 -o jsonpath='{.status.inspectionReport}' | jq

# Compare with requirements
kubectl get beskar7machine worker-01 -o jsonpath='{.spec.hardwareRequirements}' | jq

# Solution: Adjust requirements or use different hardware
```

## Best Practices

1. **Label Your Hosts**
   ```yaml
   metadata:
     labels:
       rack: "rack-01"
       datacenter: "dc-west"
       cpu: "high-performance"
   ```

2. **Use Hardware Requirements**
   - Prevents provisioning on inadequate hardware
   - Caught early during inspection
   - Saves time compared to runtime failures

3. **Monitor Inspection Logs**
   - Inspection image posts detailed logs
   - Check controller logs for errors
   - Use serial console for deep debugging

4. **Test With One Host First**
   - Use `minimal-test.yaml` first
   - Verify inspection works
   - Then scale to clusters

5. **Organize Configuration Files**
   ```
   boot-server/
    configs/
       control-plane-config.yaml
       worker-config.yaml
       custom-config.yaml
    images/
       kairos-v2.8.1.tar.gz
       flatcar-3602.tar.gz
    ipxe/
        boot.ipxe
        inspect.ipxe
   ```

## Next Steps

After deploying an example:

1. Check PhysicalHost becomes Available
2. Create Beskar7Machine
3. Monitor inspection phase
4. View inspection report
5. Verify hardware validation
6. Wait for provisioning complete

For more details, see:
- [iPXE Setup Guide](../docs/ipxe-setup.md)
- [Main README](../README.md)
- [Troubleshooting Guide](../docs/troubleshooting.md)
