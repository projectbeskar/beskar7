# Monitoring and Metrics

> **Audience:** Operators

Beskar7 exposes Prometheus metrics for the controllers and the host-callback endpoint. This page lists what's actually registered (source of truth: `internal/metrics/metrics.go`), how to scrape them, and which ones are useful for alerting.

## Overview

The manager serves `/metrics` on `:8443` (HTTPS, authenticated via TokenReview/SubjectAccessReview delegated to the kube-apiserver — see [Security Configuration](security/configuration.md)). Scrapers must authenticate as a SA bound to the `capb7-metrics-reader` ClusterRole. For local development, set the manager flag `--secure-metrics=false`.

The metrics provide insights into:

- Controller reconciliation performance
- PhysicalHost provisioning success/failure rates
- Infrastructure resource states and availability
- Error rates and types
- Boot configuration success rates
- Failure domain discovery
- Target image digest resolution (checksum file fetches)

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

**States** (match the `Status.State` strings — see `api/v1beta2/physicalhost_types.go:10-33`):
- `Available`
- `InUse`
- `Inspecting`
- `Deploying`
- `Ready`
- `Error`
- `Enrolling` and `Unknown` — always 0: the API defines these states, but the controller never sets them
- `""` (empty) — a host the controller has not given a state yet

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

**Phases** (match the `Status.Phase` strings — see the `Phase*` constants in `api/v1beta2/beskar7machine_types.go`):
- `Pending`
- `Inspecting`
- `Provisioning`
- `Provisioned`
- `Failed`
- `""` (empty) — a machine the controller has not given a phase yet

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

#### `beskar7_host_claim_attempts_total`
**Type:** Counter  
**Labels:** `namespace`, `outcome`, `conflict_reason`  
**Description:** Host claim attempts. `conflict_reason` distinguishes a lost optimistic-locking race from an empty pool or a failure-domain mismatch, so a cluster starved of hosts looks different from one where two machines raced for the same host.

#### `beskar7_host_claim_duration_seconds`
**Type:** Histogram (default buckets)  
**Labels:** `namespace`, `outcome`  
**Description:** How long a host claim took, from the start of `findAndClaimOrGetAssociatedHost` to the claim patch landing or failing.

### Target Image Digest Metrics

These track the resolution of `spec.targetImageDigestURL`: the controller fetches the checksum file over HTTPS, once, before it claims a host, and pins the digest in `status.targetImageDigest` (see [Beskar7Machine → Naming the digest by URL](beskar7machine.md#naming-the-digest-by-url-targetimagedigesturl)). A file that does not resolve holds the machine at `WaitingForTargetImageDigest` and is retried after 30 seconds, then 1, 2, 4 and every 5 minutes, so a persistent failure adds one count per retry.

Both metrics count **attempts**, one per fetch. A reconcile that finds the digest already pinned, a machine waiting out its backoff (the server is not asked), a machine that sets `spec.targetImageDigest` and a machine whose host has already been `Ready` make no attempt and count nothing. The labels never carry the URL, the host, the image's file name or the digest.

#### `beskar7_controller_target_image_digest_resolutions_total`
**Type:** Counter  
**Labels:** `outcome`, `namespace`  
**Description:** Attempts to resolve a `targetImageDigestURL`, by how they ended.

**Outcomes:**
- `success` - the file named the image and its digest is pinned
- `invalid_spec` - the spec cannot resolve whatever the server says, so nothing was fetched: the URL is not `https://` or carries credentials, `targetImageURL` has no file name, or neither digest field is set
- `fetch_error` - no response came back: the host name does not resolve (`cannot resolve the host name`), the connection failed (`cannot connect`), or the request failed some other way (`request failed`)
- `tls_error` - the server's certificate is not trusted, is for another host or is expired, or the server does not speak TLS (`TLS certificate ...`, `not an HTTPS server`)
- `timeout` - the attempt hit its 10 second cap (`timed out after 10s`), connecting, waiting for the answer or reading the body
- `redirect_refused` - the server redirected to another scheme, host or port, or more than three times
- `http_status` - the server answered with anything but `200` (`HTTP <code>`)
- `too_large` - the checksum file is over 64 KiB
- `no_entry` - the file has no entry for the image's file name (`no entry for "<name>"`)
- `invalid_entry` - the file has an entry for the image that cannot be relied on: not a SHA-256 digest, or two entries that disagree
- `canceled` - the manager's own context ended the attempt (it is shutting down); not a fault of the file or the server

Every outcome but `success` leaves the machine at `WaitingForTargetImageDigest`; the condition message says which one (see [Troubleshooting](troubleshooting.md#18-beskar7machine-reports-waitingfortargetimagedigest)).

#### `beskar7_controller_target_image_digest_resolution_duration_seconds`
**Type:** Histogram  
**Labels:** `outcome`, `namespace`  
**Description:** How long one attempt took, from the start of the fetch to its outcome, so that a histogram's `_count` equals the matching counter series.

**Buckets:** 0.05s, 0.1s, 0.25s, 0.5s, 1s, 2.5s, 5s, 7.5s, 10s, 12.5s

An attempt is capped at 10 seconds and ends a few milliseconds past it, so a `timeout` is counted in the 12.5s bucket. A healthy checksum server answers in well under a second; a `success` that creeps towards the cap is a server about to start timing out.

## Setting Up Monitoring

### Prerequisites

- Prometheus server
- Grafana (optional, for dashboards)
- ServiceMonitor CRD (if using Prometheus Operator)

### Prometheus Configuration

Metrics are HTTPS-only and require a Kubernetes-authenticated bearer token. The simplest path is via the Prometheus Operator's `ServiceMonitor` with a SA bound to `capb7-metrics-reader`:

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
  name: capb7-metrics-reader
  apiGroup: rbac.authorization.k8s.io
---
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: beskar7-controller
  namespace: capb7-system
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: beskar7
  endpoints:
    # Neither install path ships a Service for :8443 — create one targeting the
    # manager pod's 8443 and name its port `https-metrics`, or drop the selector
    # and scrape the pod directly. The RBAC that lets the manager authenticate
    # your scraper (capb7-metrics-auth-role) ships with both Helm and kustomize.
    - port: https-metrics
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

# A checksum file (spec.targetImageDigestURL) that does not resolve
- alert: Beskar7TargetImageDigestNotResolving
  expr: sum by (namespace, outcome) (increase(beskar7_controller_target_image_digest_resolutions_total{outcome!~"success|canceled"}[15m])) > 0
  annotations:
    summary: "A Beskar7Machine cannot resolve its targetImageDigestURL ({{ $labels.outcome }} in {{ $labels.namespace }})"

# A checksum server that answers slowly (the attempt is capped at 10s)
- alert: Beskar7SlowChecksumServer
  expr: histogram_quantile(0.95, sum by (le) (rate(beskar7_controller_target_image_digest_resolution_duration_seconds_bucket{outcome="success"}[1h]))) > 5
  for: 30m
  annotations:
    summary: "Checksum fetches for targetImageDigestURL are slow"
```

> A counter series starts at its first attempt, and `increase()` does not see that first sample. A machine that
> keeps failing retries every 30 seconds to 5 minutes, so `Beskar7TargetImageDigestNotResolving` fires at the
> next attempt. Attempts are rare (one per machine, plus retries), so the p95 in `Beskar7SlowChecksumServer` is
> noisy on a small cluster; alert on `outcome="timeout"` instead if that bothers you.

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

### Machines Stuck at `WaitingForTargetImageDigest`

1. Check `beskar7_controller_target_image_digest_resolutions_total` by `outcome` and `namespace`: a rising `http_status`, `tls_error` or `no_entry` points at the checksum file or its server, `timeout` and `fetch_error` at the path from the manager pod to it, `invalid_spec` at the machine's own spec
2. Read the condition message on an affected `Beskar7Machine` for the exact reason (it names the checksum server's host, which the metric deliberately does not); [Troubleshooting](troubleshooting.md#18-beskar7machine-reports-waitingfortargetimagedigest) maps each message to a fix
3. Compare `beskar7_controller_target_image_digest_resolution_duration_seconds` for `success` against the 10 second cap

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