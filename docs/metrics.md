# Monitoring and Metrics

> **Audience:** Operators

Beskar7 exposes Prometheus metrics for the controllers and the host-callback endpoint. This page lists what's actually registered (source of truth: `internal/metrics/metrics.go`), how to scrape them, and which ones are useful for alerting.

## Overview

The manager serves `/metrics` on `:8443` (HTTPS, authenticated via TokenReview/SubjectAccessReview delegated to the kube-apiserver — see [Security Configuration](security/configuration.md)). Scrapers must authenticate as a SA bound to the `metrics-reader` ClusterRole. For local development, set the manager flag `--secure-metrics=false`.

The metrics provide insights into:

- Controller reconciliation performance
- PhysicalHost provisioning success/failure rates
- Infrastructure resource states and availability
- Error rates and types
- Boot configuration success rates
- Failure domain discovery

> **Wiring status (v0.4.0-alpha.4 → next):** the controllers currently emit the
> reconciliation, error, and failure-domain metrics below. The rest (state
> gauges, power-operation counter, Redfish-connection counter, availability
> ratio, host-claim attempt/duration, machine provisioning histogram) are
> registered but always read zero until the BUG-15 wire-up PR lands. If you
> build dashboards today, gate them on samples present so empty series don't
> look like outages.

## Metric Categories

### Controller Performance Metrics

These metrics track the overall performance and health of the Beskar7 controllers.

#### `beskar7_controller_reconciliation_total`
**Type:** Counter  
**Labels:** `controller`, `outcome`, `namespace`  
**Description:** Total number of reconciliation attempts by controller and outcome.

**Outcomes:**
- `success` - Reconciliation completed successfully
- `error` - Reconciliation failed with an error
- `requeue` - Reconciliation was requeued for later processing
- `not_found` - Resource was not found (likely deleted)

#### `beskar7_controller_reconciliation_duration_seconds`
**Type:** Histogram  
**Labels:** `controller`, `outcome`, `namespace`  
**Description:** Time taken to complete reconciliation operations.

#### `beskar7_controller_errors_total`
**Type:** Counter  
**Labels:** `controller`, `error_type`, `namespace`  
**Description:** Total number of errors encountered by controller type.

**Error Types:**
- `transient` - Temporary errors that may resolve automatically
- `permanent` - Persistent errors requiring intervention
- `validation` - Input validation errors
- `connection` - Network/connectivity errors
- `timeout` - Operation timeout errors
- `unknown` - Unclassified errors

### PhysicalHost Metrics

These metrics track the state and health of physical hosts managed by Beskar7.

#### `beskar7_controller_physicalhost_states_total`
**Type:** Gauge  
**Labels:** `state`, `namespace`  
**Description:** Number of PhysicalHosts in each state.

**States** (match the `Status.State` strings — see `api/v1beta1/physicalhost_types.go:10-26`):
- `Available`
- `InUse`
- `Inspecting`
- `Ready`
- `Error`
- `Enrolling`
- `Unknown`

#### `beskar7_controller_physicalhost_power_operations_total`
**Type:** Counter  
**Labels:** `operation`, `outcome`, `namespace`  
**Description:** Total number of power operations performed on PhysicalHosts.

**Operations:**
- `power_on` - Power on operation
- `power_off` - Power off operation
- `power_reset` - Power reset operation

#### `beskar7_controller_physicalhost_redfish_connections_total`
**Type:** Counter  
**Labels:** `outcome`, `namespace`, `error_type`  
**Description:** Total number of Redfish connection attempts.

#### `beskar7_controller_physicalhost_availability`
**Type:** Gauge  
**Labels:** `namespace`  
**Description:** Availability ratio of PhysicalHosts (available/total).

### Beskar7Machine Metrics

These metrics track machine provisioning and lifecycle management.

#### `beskar7_controller_beskar7machine_states_total`
**Type:** Gauge  
**Labels:** `phase`, `namespace`  
**Description:** Number of Beskar7Machines in each phase.

#### `beskar7_controller_beskar7machine_provisioning_duration_seconds`
**Type:** Histogram  
**Labels:** `outcome`, `namespace`  
**Description:** Time taken to provision a Beskar7Machine from creation to ready.

**Buckets:** 30s, 60s, 2m, 5m, 10m, 20m, 30m, 1h

### Beskar7Cluster Metrics

These metrics track cluster-level operations and failure domain discovery.

#### `beskar7_controller_beskar7cluster_states_total`
**Type:** Gauge  
**Labels:** `ready`, `namespace`  
**Description:** Number of Beskar7Clusters in each readiness state.

#### `beskar7_controller_beskar7cluster_failure_domains_total`
**Type:** Gauge  
**Labels:** `cluster`, `namespace`  
**Description:** Number of failure domains discovered per cluster.

#### `beskar7_controller_beskar7cluster_failure_domain_discovery_total`
**Type:** Counter  
**Labels:** `outcome`, `namespace`  
**Description:** Total number of failure domain discovery operations.

## Setting Up Monitoring

### Prerequisites

- Prometheus server
- Grafana (optional, for dashboards)
- ServiceMonitor CRD (if using Prometheus Operator)

### Prometheus Configuration

Metrics are HTTPS-only and require a Kubernetes-authenticated bearer token. The simplest path is via the Prometheus Operator's `ServiceMonitor` with a SA bound to `metrics-reader`:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: prometheus-metrics-reader
subjects:
- kind: ServiceAccount
  name: prometheus
  namespace: monitoring
roleRef:
  kind: ClusterRole
  name: metrics-reader
  apiGroup: rbac.authorization.k8s.io
---
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: beskar7-controller
  namespace: beskar7-system
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: beskar7
  endpoints:
    - port: https-metrics            # match the chart's Service port name
      scheme: https
      bearerTokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token
      tlsConfig:
        insecureSkipVerify: true     # cert-manager-issued cert covers the in-cluster Service DNS
      interval: 30s
```

For static scrape configs (no Operator), point Prometheus at the manager Service over HTTPS on port 8443 with bearer-token auth using the Prometheus SA's token.

### Grafana Dashboard

Create dashboards to visualize:

1. **Controller Health Dashboard**
   - Reconciliation rate and duration
   - Error rates by type
   - Requeue patterns

2. **Infrastructure Dashboard**
   - PhysicalHost state distribution
   - Host availability trends
   - Provisioning success rates

3. **Performance Dashboard**
   - Machine provisioning times
   - Boot configuration success rates
   - Failure domain discovery

## Key Metrics for Alerting

### Critical Alerts

```yaml
# High error rate
- alert: Beskar7HighErrorRate
  expr: rate(beskar7_controller_errors_total[5m]) > 0.1
  for: 2m
  annotations:
    summary: "High error rate in Beskar7 controllers"

# Controller not reconciling
- alert: Beskar7NoReconciliation
  expr: rate(beskar7_controller_reconciliation_total[10m]) == 0
  for: 5m
  annotations:
    summary: "Beskar7 controller not performing reconciliations"

# Low host availability
- alert: Beskar7LowHostAvailability
  expr: beskar7_controller_physicalhost_availability < 0.2
  for: 5m
  annotations:
    summary: "Low PhysicalHost availability"
```

### Warning Alerts

```yaml
# Slow provisioning
- alert: Beskar7SlowProvisioning
  expr: histogram_quantile(0.95, rate(beskar7_controller_beskar7machine_provisioning_duration_seconds_bucket[10m])) > 1800
  for: 10m
  annotations:
    summary: "Slow machine provisioning times"
```

> The controller-runtime workqueue already exports requeue and workqueue-depth metrics
> (`workqueue_*`, `controller_runtime_reconcile_total{result="requeue"}`); alert on those
> rather than a Beskar7-specific requeue counter.

## Troubleshooting with Metrics

### High Error Rates

1. Check `beskar7_controller_errors_total` by `error_type`
2. Look for patterns in `controller` and `namespace` labels
3. Correlate with `beskar7_controller_physicalhost_redfish_connections_total` for connectivity issues

### Slow Provisioning

1. Examine `beskar7_controller_beskar7machine_provisioning_duration_seconds` percentiles
2. Check for high error rates in boot configurations
3. Monitor power operation success rates

### Resource Exhaustion

1. Monitor `beskar7_controller_physicalhost_availability`
2. Check distribution of `beskar7_controller_physicalhost_states_total`
3. Look for stuck hosts in `provisioning` or `error` states

### Cluster Issues

1. Monitor `beskar7_controller_beskar7cluster_failure_domain_discovery_total` for discovery failures
2. Check if failure domains are being properly detected
3. Verify cluster readiness states

## Metric Retention and Storage

- **Retention Period:** Configure based on your operational needs (30-90 days typical)
- **Storage:** Size storage based on metric cardinality and retention period
- **Backup:** Include metrics in your backup strategy for historical analysis

## Integration with Other Tools

### With Cluster API

Beskar7 metrics complement Cluster API metrics to provide full cluster lifecycle visibility:

- Correlate machine provisioning times with cluster scaling events
- Monitor infrastructure readiness during cluster creation
- Track failure domain availability for cluster placement decisions

### With Hardware Monitoring

Combine with BMC/hardware metrics:

- Correlate Beskar7 host states with hardware health metrics
- Monitor power consumption during provisioning operations
- Track hardware failures that impact host availability

## Best Practices

1. **Alert Tuning:** Start with conservative thresholds and adjust based on observed patterns
2. **Dashboard Organization:** Group metrics by operational concern (health, performance, capacity)
3. **Metric Labels:** Use consistent labeling across your monitoring stack
4. **Historical Analysis:** Retain metrics long enough for trend analysis and capacity planning
5. **Documentation:** Keep runbooks that reference specific metrics for troubleshooting procedures 