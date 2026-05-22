# Smoke testing without hardware

Beskar7 is a Cluster API infrastructure provider for bare-metal hosts. Most of what it does — power-cycling BMCs, driving iPXE boot, consuming inspection callbacks — requires hardware. That makes "did my install work?" surprisingly hard to answer in a lab or CI environment with no physical servers.

This page describes the layered smoke-test rig in `hack/smoke/` and the `make smoke` target it ships with. The goal is to take a freshly installed operator from "pod is running" through "Beskar7Machine claims a PhysicalHost and sets ProviderID" without involving real iron.

## What "smoke test" means here

For a CAPI infrastructure provider, smoke testing splits into five layers. The first four can be validated against a fake BMC. Only the last needs hardware (or a VM-based equivalent like sushy-tools + libvirt).

| Layer | What it validates | Hardware needed |
|---|---|---|
| 1. Static | Chart installs, pod runs, CRDs land, webhook + RBAC objects exist | No |
| 2. Admission | CRD validation + webhook accept valid CRs and reject invalid ones | No |
| 3. Reconcile mechanics | Controller talks to a Redfish endpoint, updates Status, surfaces errors | No — fake BMC |
| 4. CAPI claim | `Beskar7Machine` claims a `PhysicalHost` and the host state machine progresses (Available → InUse/Inspecting) | No — fake BMC |
| 5. Boot / inspector | iPXE actually boots, inspector posts callback, OS comes up | Yes (or libvirt VMs) |

The rig in `hack/smoke/` covers layers 1–4. Layer 5 belongs in a dedicated CI runner using [sushy-tools](https://opendev.org/openstack/sushy-tools) — it's not in scope here.

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
4. Applies `Beskar7Cluster` + `Beskar7Machine` + CAPI `Machine`. Verifies (a) the controller sets `consumerRef` on the `PhysicalHost` and (b) the host state machine progresses out of `Available` (typically to `InUse`/`Inspecting`). The final `ProviderID` set only happens after the inspection callback completes — that needs the inspector simulator (a separate follow-up); the smoke runner stops at the claim+progression boundary.

Failures print the relevant `describe` output and the tail of the controller log to make root-causing fast.

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
[PASS]  [layer 4b] Beskar7Machine ProviderID=b7://beskar7-smoke/smoke-host-01
[PASS]  smoke test PASSED on context my-cluster
```

End-to-end runtime is typically 60–120 seconds, dominated by the initial controller reconcile of `smoke-host-01` and the claim path on `smoke-machine-01`.

## What this catches

The chart bug that shipped in `v0.4.0-alpha.1` — pod `securityContext` missing `fsGroup` and `runAsGroup`, causing the controller to crashloop on `permission denied` when reading its TLS cert — would have been caught at layer 1 (pod never reaches Running). `make smoke` is now a release-gating signal: if it doesn't pass against a freshly installed chart on `kind`, the release isn't shippable.

Layer 3 catches RBAC misconfiguration (controller can't read the BMC `Secret`), CRD-vs-controller drift (controller doesn't know about a field the CRD requires), and TLS / connection regressions (controller can't talk to the BMC at all). Layer 4 catches finalizer leaks, owner-ref mistakes, and `ProviderID` formatting bugs.

## What this does not catch

- Real BMC quirks (boot source override that *says* it took but didn't, vendor-specific BIOS shenanigans, slow Redfish responses, transient 5xx mid-reconcile)
- iPXE chain-loading and inspector image actually running
- Bootstrap data secret consumption (the smoke fixture creates a `KubeadmConfig` but the underlying host never boots, so the `dataSecretName` never reaches a real kernel)
- TLS material rotation under load
- Multi-host claim races (only one `PhysicalHost` in the fixture)

For those, you need either real hardware or a libvirt+sushy-tools rig. The smoke runner is a 60-second pre-flight, not a full conformance suite.

## Extending the rig

If you add a feature that touches one of the existing reconcile layers, add a matching assertion in `hack/smoke/run.sh`. If you add a new CRD or a new field with non-trivial controller behavior, add a manifest under `hack/smoke/manifests/` and an assertion.

The mock server is in `internal/redfishmock/`. If the controller starts calling a Redfish endpoint the mock doesn't yet serve, you'll see the smoke run fail with a 404 in the controller log — extend `server.go` with the new endpoint rather than working around the gap in the runner.

Two follow-ups that would lift the rig:

1. A separate "fake inspector" pod that POSTs a hardware-details payload to the controller's bootstrap callback, completing the `Inspecting → Ready` transition that layer 4 can't reach today.
2. A CI job that spins up a `kind` cluster, installs the just-published chart, runs `make smoke`, and gates the GitHub Release on success.
