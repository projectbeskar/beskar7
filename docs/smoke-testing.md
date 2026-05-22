# Smoke testing without hardware

Beskar7 is a Cluster API infrastructure provider for bare-metal hosts. Most of what it does — power-cycling BMCs, driving iPXE boot, consuming inspection callbacks — requires hardware. That makes "did my install work?" surprisingly hard to answer in a lab or CI environment with no physical servers.

This page describes the layered smoke-test rig in `hack/smoke/` and the `make smoke` target it ships with. The goal is to take a freshly installed operator from "pod is running" through "Beskar7Machine claims a PhysicalHost and sets ProviderID" without involving real iron.

## What "smoke test" means here

For a CAPI infrastructure provider, smoke testing splits into five layers. All five can be validated against a fake BMC and a simulated inspector — no hardware required.

| Layer | What it validates | Hardware needed |
|---|---|---|
| 1. Static | Chart installs, pod runs, CRDs land, webhook + RBAC objects exist | No |
| 2. Admission | CRD validation + webhook accept valid CRs and reject invalid ones | No |
| 3. Reconcile mechanics | Controller talks to a Redfish endpoint, updates Status, surfaces errors | No — fake BMC |
| 4. CAPI claim | `Beskar7Machine` claims a `PhysicalHost` and the host state machine progresses (Available → InUse/Inspecting) | No — fake BMC |
| 5. Inspection callback | Simulated inspector POSTs a hardware report to the controller's bootstrap callback; PhysicalHost reaches `Ready` and `Beskar7Machine.Spec.ProviderID` is set | No — kubectl-run curl pod inside the cluster |

Beyond layer 5 there's a separate "real boot" tier — iPXE actually serves, the inspection image actually runs, the OS actually comes up — which requires either hardware or [sushy-tools](https://opendev.org/openstack/sushy-tools) + libvirt VMs. That's the conformance-tier validation and is out of scope for `make smoke`.

## Components

```
hack/smoke/
├── manifests/
│   ├── 00-namespace.yaml         # beskar7-smoke namespace
│   ├── 10-mock-redfish.yaml      # Deployment + Service for the fake BMC
│   ├── 20-bmc-secret.yaml        # Credentials the operator uses to talk to it
│   ├── 30-physicalhost.yaml      # PhysicalHost pointing at the fake BMC
│   └── 40-cluster-and-machine.yaml  # Beskar7Cluster + Beskar7Machine + CAPI Machine
└── run.sh                        # Layered runner
```

### The fake BMC

`cmd/mock-redfish` is a small Go binary that wraps the multi-vendor Redfish fake originally written for integration tests (`internal/redfishmock/`). It speaks enough of the Redfish API for `gofish` to drive it through the operator's full reconcile loop:

- Service root, Systems collection, single System, Managers, VirtualMedia
- Power state read + reset (`#ComputerSystem.Reset`)
- Boot source override (PXE / CD / virtual media)
- BIOS attribute read

It generates a self-signed RSA cert in memory at startup (configurable via `--tls-cert` / `--tls-key` or disable with `--tls=false`), listens on `:8443` by default, and supports Basic auth (`admin` / `password123` by default). The image is published as `ghcr.io/projectbeskar/beskar7/mock-redfish:<tag>` alongside the controller, on the same git-tag push.

### The runner

`hack/smoke/run.sh` is a Bash script (~200 lines, no external deps beyond `kubectl`) that:

1. Verifies the operator pod is running and the 4 CRDs are installed
2. Submits a CR with a malformed Redfish address via `kubectl --dry-run=server` and asserts CRD validation rejects it
3. Applies the mock + a `PhysicalHost`, waits for `Status.Ready=true`
4. Applies `Beskar7Cluster` + `Beskar7Machine` + CAPI `Machine` (with a pre-baked bootstrap data Secret — see below). Verifies (a) the controller sets `consumerRef` on the `PhysicalHost` and (b) the host state machine progresses out of `Available` (to `InUse` or `Inspecting`).
5. Waits for `PhysicalHost.Status.Bootstrap.URL` + `TokenHash` to be populated, reads the plaintext token from the per-host `<host>-bootstrap-token` Secret, derives the inspection callback URL from the bootstrap URL, and POSTs a fake hardware-report payload via a one-shot `kubectl run curl` pod inside the cluster. Then waits for `PhysicalHost.Status.State=Ready` and `Beskar7Machine.Spec.ProviderID=b7://<ns>/<host>`.

Failures print the relevant `describe` output and the tail of the controller log to make root-causing fast.

### Why the pre-baked bootstrap data Secret?

In production, CAPI's KubeadmConfig provider generates the bootstrap data Secret only after the cluster's control plane is initialised (a node has actually come up and registered with the API server). Smoke runs against a fake BMC have no real control plane — there's nothing to initialise — so KubeadmConfig would loop forever on `Waiting for Cluster status.ControlPlaneInitialized to be true`.

The smoke fixture short-circuits this by creating a placeholder Secret named `smoke-machine-01-bootstrap-data` and pointing `Machine.Spec.Bootstrap.DataSecretName` at it directly. CAPI's Machine controller skips KubeadmConfig generation entirely when `DataSecretName` is preset and flips `bootstrapReady=true` immediately, unblocking the Beskar7Machine controller's `ensureBootstrapDataReady` check. The Secret's `value` contents are never actually consumed (the inspector simulator POSTs hardware data; it never fetches the bootstrap payload).

By default the runner tears down everything in the `beskar7-smoke` namespace on exit. Pass `--keep` to leave fixtures in place for inspection, or `--teardown` to clean up a previous run without re-running.

## Running it

Prerequisites on the target cluster:

- Beskar7 controller installed (`helm install --devel beskar7 beskar7/beskar7 -n beskar7-system --create-namespace`)
- cert-manager installed (the chart depends on it for the webhook serving cert)
- Cluster API core installed (so the CAPI `Machine` controller is present and won't block reconciliation on a missing webhook)

Then:

```bash
make smoke
```

Or directly:

```bash
bash hack/smoke/run.sh
```

Useful flags:

```bash
# Skip teardown so you can inspect state
bash hack/smoke/run.sh --keep

# Tear down a previous --keep run
make smoke-teardown

# Use a custom mock image (e.g. a locally-built one)
MOCK_IMAGE=localhost:5000/mock-redfish:dev bash hack/smoke/run.sh

# Skip a specific layer (useful when iterating on the rig itself)
bash hack/smoke/run.sh --skip-layer-4
```

A successful run looks like:

```
[INFO]  kubectl context: my-cluster
[INFO]  [layer 1] verifying operator install
[PASS]  [layer 1] operator running, 4 CRDs present
[INFO]  [layer 2] verifying webhook admission
[PASS]  [layer 2] CRD validation rejects malformed addresses
[INFO]  [layer 3] applying mock BMC + PhysicalHost
[PASS]  [layer 3] PhysicalHost Ready=true, state=Available
[INFO]  [layer 4] applying Beskar7Cluster + Beskar7Machine + CAPI Machine
[PASS]  [layer 4a] PhysicalHost claimed by Beskar7Machine=smoke-machine-01
[PASS]  [layer 4b] PhysicalHost progressed to state=InUse (claim drove state machine)
[INFO]  [layer 5] simulating inspector POST to bootstrap callback
[PASS]  [layer 5a] inspector POST accepted (2xx)
[PASS]  [layer 5b] PhysicalHost reached state=Ready
[PASS]  [layer 5c] Beskar7Machine ProviderID=b7://beskar7-smoke/smoke-host-01
[PASS]  smoke test PASSED on context my-cluster
```

End-to-end runtime is typically 90–180 seconds, dominated by the initial controller reconcile of `smoke-host-01`, the claim path on `smoke-machine-01`, and the inspection-callback round-trip plus `handleReadyHost` reconcile.

## What this catches

The chart bug that shipped in `v0.4.0-alpha.1` — pod `securityContext` missing `fsGroup` and `runAsGroup`, causing the controller to crashloop on `permission denied` when reading its TLS cert — would have been caught at layer 1 (pod never reaches Running). `make smoke` is now a release-gating signal: if it doesn't pass against a freshly installed chart on `kind`, the release isn't shippable.

Layer 3 catches RBAC misconfiguration (controller can't read the BMC `Secret`), CRD-vs-controller drift (controller doesn't know about a field the CRD requires), and TLS / connection regressions (controller can't talk to the BMC at all). Layer 4 catches finalizer leaks, owner-ref mistakes, and claim-path regressions. Layer 5 catches CAPI conformance regressions (the v1beta2 status contract on `Beskar7Cluster`/`Beskar7Machine`), bootstrap token mint/store path regressions, and the controller's re-acquire-after-claim path that the v0.4.0-alpha.3 → alpha.4 work uncovered.

## What this does not catch

- Real BMC quirks (boot source override that *says* it took but didn't, vendor-specific BIOS shenanigans, slow Redfish responses, transient 5xx mid-reconcile)
- iPXE chain-loading and inspector image actually running
- Bootstrap data secret consumption by a real kernel (the smoke fixture pre-bakes a placeholder; the contents are never fetched)
- TLS material rotation under load
- Multi-host claim races (only one `PhysicalHost` in the fixture)

For those, you need either real hardware or a libvirt+sushy-tools rig. The smoke runner is a ~2-minute pre-flight, not a full conformance suite.

## Extending the rig

If you add a feature that touches one of the existing reconcile layers, add a matching assertion in `hack/smoke/run.sh`. If you add a new CRD or a new field with non-trivial controller behavior, add a manifest under `hack/smoke/manifests/` and an assertion.

The mock server is in `internal/redfishmock/`. If the controller starts calling a Redfish endpoint the mock doesn't yet serve, you'll see the smoke run fail with a 404 in the controller log — extend `server.go` with the new endpoint rather than working around the gap in the runner.

Open follow-up: factor the bash inspector logic in layer 5 into a proper `cmd/mock-inspector` binary (matching the `cmd/mock-redfish` pattern), so the simulator can also be used for docs and manual demos outside the smoke runner.
