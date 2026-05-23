# Resource Planning and Sizing Guide

This document provides detailed guidance for sizing Beskar7 controller manager resources based on your deployment scale and requirements.

## Overview

The Beskar7 controller manager resource requirements depend on several factors:

- **Number of physical hosts** being managed
- **Reconciliation frequency** and webhook traffic
- **Security monitoring** overhead
- **Network latency** to BMCs
- **Cluster size** and number of machines

## Default Resource Configuration

The default configuration in `config/manager/manager.yaml` is optimized for **medium-scale deployments** (50-200 hosts):

```yaml
resources:
  limits:
    cpu: 1000m           # 1 CPU core maximum
    memory: 1Gi          # 1GB memory maximum  
    ephemeral-storage: 2Gi
  requests:
    cpu: 200m            # 0.2 CPU cores reserved
    memory: 256Mi        # 256MB memory reserved
    ephemeral-storage: 100Mi
```

## Deployment Size Recommendations

### Small Deployments (< 50 hosts)

**Use Case:** Development, testing, small edge deployments

```yaml
resources:
  limits:
    cpu: 500m
    memory: 512Mi
    ephemeral-storage: 1Gi
  requests:
    cpu: 100m
    memory: 128Mi
    ephemeral-storage: 100Mi
```

**Characteristics:**
- 1 replica recommended
- Leader election optional
- Lower reconciliation overhead
- Minimal webhook traffic

### Medium Deployments (50-200 hosts)

**Use Case:** Production edge sites, department-level infrastructure

```yaml
resources:
  limits:
    cpu: 1000m
    memory: 1Gi
    ephemeral-storage: 2Gi
  requests:
    cpu: 200m
    memory: 256Mi
    ephemeral-storage: 100Mi
```

**Characteristics:**
- 2-3 replicas recommended for HA
- Leader election enabled
- Regular reconciliation cycles
- Moderate webhook validation load

### Large Deployments (200-500 hosts)

**Use Case:** Large production deployments, multi-cluster management

```yaml
resources:
  limits:
    cpu: 2000m
    memory: 2Gi
    ephemeral-storage: 4Gi
  requests:
    cpu: 500m
    memory: 512Mi
    ephemeral-storage: 200Mi
```

**Characteristics:**
- 3+ replicas for high availability
- Frequent reconciliation required
- High webhook traffic volume
- Security monitoring overhead

### Extra Large Deployments (500+ hosts)

**Use Case:** Enterprise-scale infrastructure, cloud provider workloads

```yaml
resources:
  limits:
    cpu: 4000m
    memory: 4Gi
    ephemeral-storage: 8Gi
  requests:
    cpu: 1000m
    memory: 1Gi
    ephemeral-storage: 500Mi
```

**Characteristics:**
- 3+ replicas with pod disruption budgets
- Continuous reconciliation required
- Very high webhook traffic
- Full security monitoring enabled
- Metrics and observability overhead

## Resource Optimization

### CPU Optimization

The controller manager includes several CPU optimizations:

#### GOMAXPROCS Configuration
```yaml
env:
- name: GOMAXPROCS
  valueFrom:
    resourceFieldRef:
      resource: limits.cpu
```

This automatically configures Go's runtime to use the CPU limit, preventing over-subscription.

#### CPU Usage Patterns
- **Baseline:** 50-100m for idle reconciliation
- **Spike:** During mass provisioning or error recovery
- **Sustained:** High during failure domain discovery

### Memory Optimization

#### Memory Usage Patterns
- **Controller caches:** ~50MB per 100 hosts
- **Webhook operations:** ~10MB per concurrent request
- **Security monitoring:** ~25MB baseline
- **Metrics collection:** ~15MB per 1000 metrics

#### Memory Limits Guidelines
```
Memory Limit = (Host Count × 0.5MB) + (Webhook Concurrency × 10MB) + 100MB (base)
```

### Storage Optimization

#### Ephemeral Storage Usage
- **Webhook certificates:** 50MB
- **Temporary files:** Variable during operations
- **Security scan data:** ~10MB per scan
- **Log buffers:** 50-100MB

## Performance Tuning

### Reconciliation Tuning

Configure reconciliation intervals based on your needs:

```yaml
# Real flags accepted by cmd/manager/main.go in v0.4.0-alpha.4.4.
args:
- --leader-elect=true
- --metrics-bind-address=:8443
- --secure-metrics=true
- --health-probe-bind-address=:8081
- --inspection-port=8082
- --bootstrap-url-base=https://beskar7-controller-manager.beskar7-system.svc:8082
```

There is no `--max-concurrent-reconciles*` or `--reconciliation-interval` flag; the controller-runtime defaults apply (one reconciler per controller; per-resource requeue intervals encoded in the controllers themselves). Per-controller concurrency would have to be raised in code in `controllers/<kind>_controller.go:SetupWithManager`.

### Health Check Tuning

Adjust health check timings for different deployment sizes:

#### Small/Medium Deployments (Default)
```yaml
livenessProbe:
  initialDelaySeconds: 15
  periodSeconds: 20
  timeoutSeconds: 5
  failureThreshold: 3

readinessProbe:
  initialDelaySeconds: 5
  periodSeconds: 10
  timeoutSeconds: 5
  failureThreshold: 3
```

#### Large Deployments
```yaml
livenessProbe:
  initialDelaySeconds: 30
  periodSeconds: 30
  timeoutSeconds: 10
  failureThreshold: 5

readinessProbe:
  initialDelaySeconds: 10
  periodSeconds: 15
  timeoutSeconds: 10
  failureThreshold: 5
```

## Environment-Specific Configurations

### Development Environment

Generous resources for debugging and development:

```yaml
resources:
  limits:
    cpu: 2000m        # Allow high CPU for development builds
    memory: 2Gi       # Extra memory for debugging
    ephemeral-storage: 5Gi
  requests:
    cpu: 100m         # Low requests for resource efficiency
    memory: 128Mi
    ephemeral-storage: 100Mi
```

### Production Environment

Conservative limits with proper requests for scheduling:

```yaml
resources:
  limits:
    cpu: 1000m        # Controlled limits
    memory: 1Gi
    ephemeral-storage: 2Gi
  requests:
    cpu: 200m         # Proper resource reservation
    memory: 256Mi
    ephemeral-storage: 100Mi
```

### High Availability Production

Higher requests to ensure quality of service:

```yaml
resources:
  limits:
    cpu: 2000m
    memory: 2Gi
    ephemeral-storage: 4Gi
  requests:
    cpu: 500m         # Higher requests for guaranteed QoS
    memory: 512Mi
    ephemeral-storage: 200Mi
```

## Monitoring Resource Usage

### Key Metrics to Monitor

1. **CPU Usage**
   ```promql
   rate(container_cpu_usage_seconds_total{container="manager"}[5m])
   ```

2. **Memory Usage**
   ```promql
   container_memory_working_set_bytes{container="manager"}
   ```

3. **Storage Usage**
   ```promql
   container_fs_usage_bytes{container="manager"}
   ```

### Resource Alerts

```yaml
# CPU throttling alert
- alert: Beskar7CPUThrottling
  expr: rate(container_cpu_cfs_throttled_seconds_total{container="manager"}[5m]) > 0.1
  for: 5m
  annotations:
    summary: "Beskar7 controller experiencing CPU throttling"

# Memory usage alert  
- alert: Beskar7HighMemoryUsage
  expr: container_memory_working_set_bytes{container="manager"} / container_spec_memory_limit_bytes{container="manager"} > 0.8
  for: 10m
  annotations:
    summary: "Beskar7 controller high memory usage"

# Storage usage alert
- alert: Beskar7HighStorageUsage
  expr: container_fs_usage_bytes{container="manager"} / container_fs_limit_bytes{container="manager"} > 0.8
  for: 5m
  annotations:
    summary: "Beskar7 controller high ephemeral storage usage"
```

## Scaling Strategies

### Horizontal Scaling

Enable multiple replicas with leader election:

```yaml
spec:
  replicas: 3
  template:
    spec:
      containers:
      - args:
        - --leader-elect=true
        - --leader-elect-lease-duration=15s
        - --leader-elect-renew-deadline=10s
        - --leader-elect-retry-period=2s
```

### Vertical Scaling

Automatically scale resources with VPA:

```yaml
apiVersion: autoscaling.k8s.io/v1
kind: VerticalPodAutoscaler
metadata:
  name: beskar7-controller-vpa
spec:
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: beskar7-controller-manager
  updatePolicy:
    updateMode: Auto
  resourcePolicy:
    containerPolicies:
    - containerName: manager
      minAllowed:
        cpu: 100m
        memory: 128Mi
      maxAllowed:
        cpu: 4000m
        memory: 4Gi
```

## Best Practices

### Resource Planning Checklist

- [ ] **Baseline sizing** based on host count
- [ ] **Peak load testing** with expected traffic
- [ ] **Resource monitoring** configured
- [ ] **Alerts** configured for resource exhaustion
- [ ] **Scaling strategy** documented
- [ ] **Resource limits** prevent runaway processes
- [ ] **Resource requests** ensure scheduling guarantees

### Common Pitfalls

1. **Under-provisioning requests** -> Poor scheduling
2. **Over-provisioning limits** -> Resource waste
3. **Missing GOMAXPROCS** -> CPU over-subscription
4. **Insufficient ephemeral storage** -> Pod evictions
5. **No monitoring** -> No visibility into actual usage

### Production Recommendations

1. **Start with medium configuration** and monitor
2. **Scale based on metrics** not assumptions
3. **Set appropriate requests** for scheduling
4. **Monitor resource usage trends** over time
5. **Test scaling scenarios** before production deployment
6. **Document your sizing decisions** for future reference

## Troubleshooting Resource Issues

### OOMKilled Pods

```bash
# Check memory usage patterns
kubectl top pod -l control-plane=beskar7-controller-manager -n beskar7-system

# Review resource events
kubectl get events -n beskar7-system --field-selector reason=OOMKilling

# Increase memory limits
kubectl patch deployment beskar7-controller-manager -n beskar7-system \
  --patch '{"spec":{"template":{"spec":{"containers":[{"name":"manager","resources":{"limits":{"memory":"2Gi"}}}]}}}}'
```

### CPU Throttling

```bash
# Check CPU throttling metrics
kubectl exec -it deployment/beskar7-controller-manager -n beskar7-system -- cat /sys/fs/cgroup/cpu/cpu.stat

# Increase CPU limits
kubectl patch deployment beskar7-controller-manager -n beskar7-system \
  --patch '{"spec":{"template":{"spec":{"containers":[{"name":"manager","resources":{"limits":{"cpu":"2000m"}}}]}}}}'
```

### Storage Issues

```bash
# Check ephemeral storage usage
kubectl describe pod -l control-plane=beskar7-controller-manager -n beskar7-system

# Clean up temporary files
kubectl exec -it deployment/beskar7-controller-manager -n beskar7-system -- du -sh /tmp/*

# Increase storage limits
kubectl patch deployment beskar7-controller-manager -n beskar7-system \
  --patch '{"spec":{"template":{"spec":{"containers":[{"name":"manager","resources":{"limits":{"ephemeral-storage":"4Gi"}}}]}}}}'
```

This resource planning guide should be used in conjunction with your monitoring and observability systems to ensure optimal performance and resource utilization. 