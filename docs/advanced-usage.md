# Advanced Usage

This page covers Beskar7 features beyond the basic "register a host, claim a host" flow. The v0.3 `RemoteConfig`/`PreBakedISO`/`UefiTargetBootSourceOverride` approaches were removed in v0.4 — the only provisioning path now is iPXE network boot to an inspection image, followed by kexec into the target OS.

## Bootstrap data flow

Beskar7 honours the CAPI bootstrap-provider contract: every `Beskar7Machine` requires its owning `Machine` to expose a `Spec.Bootstrap.DataSecretName`. The Secret's `value` key holds the user-data the target OS consumes (cloud-init, ignition, or whatever your bootstrap provider produces).

End-to-end:

1. The bootstrap provider (e.g. `kubeadm` via `KubeadmConfig` or `KubeadmConfigTemplate`) writes a Secret in the workload namespace with key `value`.
2. The `Beskar7Machine` reconciler reads `Machine.Spec.Bootstrap.DataSecretName` to confirm the Secret exists. It does NOT read the bytes — that's the manager's bootstrap GET endpoint's job.
3. The reconciler computes the deterministic per-host URL `<--bootstrap-url-base>/api/v1/bootstrap/<ns>/<host>` and signals it to the PhysicalHost via the `infrastructure.cluster.x-k8s.io/bootstrap-url` annotation. The PhysicalHost reconciler persists it to `Status.Bootstrap.URL`.
4. The reconciler mints a per-host bearer token (`internal/auth/token.go`), stores the plaintext in a per-host Secret named `<host>-bootstrap-token`, and signals the SHA-256 hash + 30-minute lifetime to the PhysicalHost via `infrastructure.cluster.x-k8s.io/bootstrap-token`. The PhysicalHost reconciler persists the hash and lifetime to `Status.Bootstrap.{TokenHash,IssuedAt,ExpiresAt}`.
5. The iPXE infrastructure renders the URL and the plaintext token into the kernel cmdline (see [iPXE Setup](ipxe-setup.md)).
6. After kexec into the target OS, cloud-init / ignition fetches `https://<manager>:8082/api/v1/bootstrap/<ns>/<host>` with `Authorization: Bearer <plaintext>`. The manager validates the token (`controllers/inspection_handler.go:newBearerTokenVerifier`), walks `PhysicalHost → ConsumerRef → Beskar7Machine → owner Machine → Spec.Bootstrap.DataSecretName → Secret`, and serves `secret.data["value"]` with `Cache-Control: no-store`.

If the named Secret does not exist when the Beskar7Machine reconciles, `BootstrapDataReady=False (BootstrapDataUnavailable)` is set and the reconciler stops requeueing — operator must intervene (the bootstrap provider failed or the name is wrong).

The `--bootstrap-url-base` manager flag controls the base URL used in step 3. The default is `https://beskar7-controller-manager.beskar7-system.svc:8082`, which works when the chart release name is `beskar7`. Override it (`--bootstrap-url-base=https://<service>.<namespace>.svc:8082`) when:

- You install the chart with a non-default release name; the Service name follows `<release>-controller-manager`.
- You front the manager with an external load balancer and want hosts to reach it via that VIP rather than the cluster Service DNS.

## Hardware requirements

`Beskar7Machine.spec.hardwareRequirements` is the main "advanced knob" today. The inspection image reports CPU, memory, disk, and NIC details; the Beskar7Machine reconciler validates the report against the configured minimums:

```yaml
spec:
  hardwareRequirements:
    minCPUCores: 8       # summed across all CPUs in the report
    minMemoryGB: 64      # summed across all DIMMs
    minDiskGB:   500     # summed across all disks
```

Validation rules:

- `minCPUCores` is summed across `report.cpus[].cores`. A box with 2 CPUs × 4 cores satisfies `minCPUCores: 8`.
- `minMemoryGB` is summed across `report.memory[].capacity`, parsed by `parseMemoryCapacityGB`. The parser accepts `GB`, `GiB`, `MB`, `MiB`, `TB`, `TiB`. Bare integers are rejected. Fractional results are truncated to whole GB.
- `minDiskGB` is summed across `report.disks[].sizeGB`.

If any minimum is violated, the controller calls `markTerminalFailure(HardwareRequirementsNotMet, msg)`, sets `Status.FailureReason` and `Status.FailureMessage`, and stops requeueing. The BMC's hardware does not change at runtime — the failure is terminal. Recovery: lower the requirement, allocate to a different host, or replace the hardware (then delete-and-recreate the Beskar7Machine).

You can use `hardwareRequirements` to enforce node-class invariants before bootstrap:

```yaml
# Control-plane: needs CPU + RAM, modest disk
hardwareRequirements:
  minCPUCores: 4
  minMemoryGB: 16
  minDiskGB:   100

# Memory-heavy worker for caching workloads
hardwareRequirements:
  minCPUCores: 8
  minMemoryGB: 256
  minDiskGB:   500

# Storage worker
hardwareRequirements:
  minCPUCores: 4
  minMemoryGB: 32
  minDiskGB:   8000
```

Pair this with manual host labelling (`topology.kubernetes.io/zone`, vendor labels) and per-namespace separation to steer machines to the right hardware class.

## Failure domains

The `Beskar7Cluster` reconciler discovers failure domains by listing `PhysicalHost` resources in the same namespace and extracting unique values from the `topology.kubernetes.io/zone` label. It populates `Beskar7Cluster.status.failureDomains`; CAPI uses this for placement.

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
kind: PhysicalHost
metadata:
  name: server-01
  labels:
    topology.kubernetes.io/zone: rack-1     # consumed by Beskar7Cluster
spec:
  redfishConnection:
    address: "https://192.168.1.100"
    credentialsSecretRef: "bmc-credentials"
```

Hosts without the label are not part of any failure domain; the reconciler ignores them for placement purposes.

## Force-release a stuck host

A normal Beskar7Machine deletion runs:

1. Best-effort `ClearBootSourceOverride` on the BMC.
2. Best-effort graceful `SetPowerState(Off)`.
3. Patch `ConsumerRef = nil` on the host.
4. Remove the finalizer.

Steps 1 and 2 can take time if the BMC is unreachable (the controller logs and swallows errors so a dead BMC cannot strand the finalizer; the timeouts come from `internal/redfish/gofish_client.go:newHTTPClient`). To skip the BMC steps entirely:

```bash
kubectl annotate beskar7machine <name> \
  -n <namespace> \
  infrastructure.cluster.x-k8s.io/force-release=true
kubectl delete beskar7machine <name> -n <namespace>
```

Use only when the BMC is permanently unreachable. The host will return to `Available` immediately, but it will retain whatever boot source / power state the BMC currently has — clean up out-of-band.

## See also

- [iPXE Setup](ipxe-setup.md) — how the kernel cmdline is constructed.
- [Security Configuration](security/configuration.md) — TLS, bearer tokens, manager flags.
- [Beskar7Machine](beskar7machine.md) — full reconcile flow.
- [API Reference](api-reference.md) — every field that exists.
