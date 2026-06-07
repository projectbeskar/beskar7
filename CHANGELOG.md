# Changelog

All notable changes to this project will be documented in this file.

The format is based on Keep a Changelog, and this project adheres to Semantic Versioning.

## [Unreleased]

### Added
- **`POST /api/v1/provision-failed/{namespace}/{hostName}` callback endpoint** (contract v4.1) — bearer-gated HTTPS endpoint on `:8082`; the inspector calls it when Phase 2 fails (image fetch, digest verify, disk write, or `COS_OEM` inject) before exiting, reporting the failure promptly instead of waiting out `--deployment-timeout` (up to 20 min). The `PhysicalHost` transitions `StateDeploying → StateError` immediately; the `Beskar7Machine` is marked `FailureReason=DeploymentFailed` with the sanitized inspector reason in `FailureMessage`. Backward-compatible: a v4 controller without the endpoint returns 404, which the v4.1 inspector tolerates. Implemented in `controllers/provision_failed_handler.go`; route registered in `SetupCallbackServer`.
- **`DeploymentFailed` failure reason** — new `FailureReason` constant on `Beskar7Machine` (`DeploymentFailedReason = "DeploymentFailed"`), distinct from `DeploymentTimedOut` (timeout) and `PhysicalHostError` (Redfish/BMC-level error). The `StateError` handler in `Beskar7MachineReconciler` attributes the reason by inspecting the `PhysicalHost.Status.ErrorMessage` prefix set by the provision-failed handler.
- **Inspector contract v4.1** — `docs/inspector-contract.md` bumped to v4.1; §4.5 documents the new `/provision-failed` endpoint; §2 provisioning sequence updated with the fast-fail path; §11 open item on provisioning-failure recovery closed.

## [v0.4.0-alpha.7] - 2026-06-07

The "controller↔inspector contract" release: defines and ships the full provisioning contract (v1 → v4) between the Beskar7 controller and the new Rust `beskar7-inspector`, replacing the early kexec model with a digest-pinned whole-disk image handoff and a per-host iPXE boot/token flow with a provisioning-complete callback. Validated end-to-end on real bare metal: a blank host → `Ready` k3s node with `ProviderID` set.

### Added
- **Controller↔inspector contract spec + golden-fixture test** (#110, #114) — `docs/inspector-contract.md` (now v4) plus a Go golden-fixture test guarding the `InspectionReportRequest` schema against drift with the inspector.
- **Per-host iPXE `/boot` endpoint + single-use boot nonce** (D-009/D-010, #111, #112) — nonce-gated `GET /api/v1/boot/{ns}/{host}/{nonce}` renders the per-host kernel cmdline (`beskar7.api/token/ca/target/digest`); the boot nonce is minted alongside, and is distinct from, the bearer token, and is single-use (consumed on first fetch).
- **External callback Service + serving-cert SAN sizing** (H-1, #113) — the chart exposes the callback Service externally and sizes the serving-cert SAN via `callback.externalNames`/`callback.externalIPs`, so bare-metal hosts can reach `:8082` with a verifiable cert.
- **Whole-disk image handoff with digest pinning** (contract v2, D-011, #115, #116, #117) — repurposed `Beskar7Machine.Spec.TargetImageURL` + new **required** `TargetImageDigest`; the inspector streams the image to the target disk verifying sha256, discovers/mounts the `COS_OEM` partition, injects the per-host cloud-config, and reboots. `beskar7.target-digest` rendered on the cmdline. Supersedes the kexec model.
- **Target-disk selection** (#118) — optional `Beskar7Machine.Spec.TargetDisk` rendered as `beskar7.disk`; absent → the inspector auto-selects the smallest eligible whole disk.
- **D-013 provisioning networking** (#119, #121, #122) — `BOOTIF` rendered from the `?mac=` query param; native DHCP with multi-NIC race resolution (gatewayed-winner) + DNS `resolv.conf`; static-network override `Beskar7Machine.Spec.StaticIP` → `beskar7.ip` for DHCP-less / VLAN-pinned provisioning networks.
- **`StateDeploying` phase** — `PhysicalHost` now transitions `Inspecting → Deploying` when the inspection report is accepted, and `Deploying → Ready` only when the inspector POSTs the provisioned-complete callback. `ProviderID`, `Status.Ready`, and `Status.Initialization.Provisioned` on `Beskar7Machine` are set at `Ready` entry, not at inspection completion (D-015).
- **`POST /api/v1/provisioned/{namespace}/{hostName}` callback endpoint** — bearer-gated HTTPS endpoint on `:8082`; the inspector calls it after the verified whole-disk write and `COS_OEM` inject, before `reboot(2)`. Returns `202 Accepted`. Implemented in `controllers/provisioned_handler.go`; route registered in `SetupCallbackServer` (D-015).
- **`--deployment-timeout` manager flag** — bounds how long a host may stay in `Deploying` before the `Beskar7Machine` is marked terminally failed with `FailureReason=DeploymentTimedOut`. Default 20 min. Measured from `PhysicalHost.Status.DeployingTimestamp` (D-015).
- **`PhysicalHost.Status.DeployingTimestamp`** — set by the `PhysicalHostReconciler` on the `Inspecting → Deploying` transition; used by the `Beskar7Machine` controller to enforce the deployment timeout (D-015).
- **Inspector contract v4** — `docs/inspector-contract.md` bumped to v4; §4.4 documents the new `/provisioned` endpoint; §2 provisioning sequence updated; §9.1 step ordering updated; §11 open item on CAPI-bootstrap → Kairos mapping closed with the D-014 ruling (byte-verbatim, bootstrap provider owns the format).

### Fixed
- **`ClearBootSourceOverride` now sends `Target=NoneBootSourceOverrideTarget`** — previously sent `Enabled=Disabled` with no `BootSourceOverrideTarget`, which caused a `400` on real BMCs (Redfish requires the field). Corrected in `internal/redfish/gofish_client.go` (D-015 bonus fix).
- **Inspection timeout no longer fires on a completed inspection** — `handleInspectingHost` now checks `InspectionPhase==Complete` before the timeout, so a completed-but-slow inspection is not spuriously failed `InspectionTimedOut` (D-015 finding #1).
- **Bearer token lifetime extended to 60 min** — `InspectionTimeout(10m) + DeploymentTimeout(20m) = 30m` could expire the token before the provisioned callback fires on a slow deploy. `TokenLifetime` raised from 30 min to 60 min in `internal/auth/token.go` (SEC-D015-1).

### Documentation
- **Inspector contract → v4** — `docs/inspector-contract.md` documents the `/provisioned` endpoint (§4.4), the `StateDeploying` flow (§2), and the deploy timeout; §11 closes the CAPI-bootstrap→Kairos mapping open item with the D-014 ruling (byte-verbatim; the bootstrap provider owns the format).
- **Callback serving-cert constraint (TEST-2)** — §8 now states the callback serving cert must be a non-CA leaf with its issuing CA in `ca.crt`; the Rust inspector verifies with rustls/webpki (stricter than OpenSSL) and rejects a self-signed `CA:TRUE` cert used as both leaf and CA. cert-manager and the chart's self-signed path (`genCA`+`genSignedCert`) already comply.
- **Examples** — replaced the broken kubeadm `examples/complete-cluster.yaml` (cannot produce a Kairos node) with the end-to-end-proven `examples/kairos-k3s-node.yaml`; `state-management.md` and `ipxe-setup.md` updated for the `StateDeploying` phase and `/provisioned` step.

## [v0.4.0-alpha.6] - 2026-05-29

The "post-alpha.5 hardening arc": closed the last correctness bug surfaced after alpha.5 (a host that leaked permanently when a machine was deleted mid-provision), stood up a real manager-level integration test tier to catch that class of bug at PR speed, brought the tooling/config onto current schemas (kustomize v2, golangci-lint v2), and landed the CAPI pause-conformance + webhook + flag work that had been queued. Also fixes the release workflow itself, which broke on the alpha.5 tag run.

### Added
- **Manager-level integration test suite** (#94, PR #106) — replaces the hollow `test/integration` placeholder (a single `t.Skip` that asserted nothing) with a real suite that boots envtest, starts one shared controller-runtime manager wiring all three reconcilers exactly as `cmd/manager/main.go` does, and drives cross-controller scenarios under real watch/informer timing. Three specs: full provision flow (claim → inspect → `Ready`/ProviderID, with the inspector callback simulated via the inspection-result ConfigMap + annotation handoff), credentials-Secret rotation re-trigger via `SecretToPhysicalHosts`, and delete-and-release. Build-tagged `integration`; the existing `integration-test` CI job already runs it, so no workflow change. Catches the watch-wiring / concurrent-reconcile bug class that the manual-`Reconcile` unit tier cannot and that was previously only covered by the slow kind smoke.
- **`--inspection-timeout` manager flag** (#91, PR #99) — exposes the previously-hardcoded inspection timeout (`DefaultInspectionTimeout`, 10m) as a `Beskar7MachineReconciler` field resolved via `inspectionTimeout()`, wired through `cmd/manager/main.go`. Lets operators tune how long a host may sit in `Inspecting` before the machine is marked terminally failed.
- **Self-signed webhook certificate path** (GAP-2, #88, PR #98) — the Helm chart can now serve the `Beskar7Cluster` admission webhook without cert-manager. When `certManager.enabled=false`, a memoized `beskar7.webhookCerts` template helper generates a CA + serving cert (Sprig `genCA`/`genSignedCert`), renders a `kubernetes.io/tls` Secret, and injects the CA bundle into the webhook configuration. `certManager.enabled=true` (default) keeps the cert-manager-issued path unchanged.
- **Smoke watch-namespaces isolation layer + CI gate** (#95, PR #102) — adds layer 6 to `hack/smoke/run.sh` (env/flag-gated `SMOKE_NS_ISOLATION` / `--with-isolation`) that proves a watch-namespaces-scoped operator ignores resources outside its watched set, plus a `make smoke-watch-namespaces` target and a CI phase. Self-skips when the operator was installed cluster-wide.

### Fixed
- **#107**: `Beskar7MachineReconciler.reconcileDelete` only released the claimed `PhysicalHost` when `Spec.ProviderID` was set, but `ProviderID` is assigned late (after inspection, in `handleReadyHost`) while `ConsumerRef` is set at claim time. A machine deleted mid-inspection thus had its finalizer removed without clearing the host's `ConsumerRef`, stranding the host in `InUse` with a dangling reference — a permanent, unclaimable leak. Release now keys off `ConsumerRef` ownership (new `findClaimedHostForRelease` helper; `ProviderID` kept as a fast-path `Get`, with a namespace list-scan fallback) (PR #108).
- **#103**: `PhysicalHostReconciler.reconcileDelete` panicked on a nil `Recorder` (`r.Recorder.Event(...)`) because the manager wiring never set `Recorder`. Wired `mgr.GetEventRecorderFor(...)` in `cmd/manager/main.go` and added a nil-guard, with a regression test that panics without the guard (PR #104).
- **GAP-1 / #87**: `PhysicalHostReconciler` now honours the CAPI pause signal (`cluster.x-k8s.io/paused` annotation), matching the other reconcilers and the provider contract. Reconcile returns early without mutating the host while paused (PR #97).
- **Release workflow kustomize install** (#86) — the `Generate Release Artifacts` step used the upstream `install_kustomize.sh` script, whose asset glob changed at kustomize-master and started failing with `tar: ... No such file or directory` (this broke the v0.4.0-alpha.5 release artifacts). Replaced with a pinned `go install sigs.k8s.io/kustomize/kustomize/v5@v5.4.3`, matching the controller-gen / golangci-lint pattern.

### Changed
- **golangci-lint migrated to v2** (#93, PR #105) — `.golangci.yml` rewritten to the v2 schema (`version: "2"`, `linters.default: standard`, `formatters` section); `make lint` and CI pinned to the matching v2 binary (`v2.12.2`, module path `.../v2/cmd/golangci-lint`). Seven surfaced staticcheck quick-fix findings resolved behavior-preservingly (tagged `switch`, De Morgan simplification, `time.Time` method shortcuts).
- **kustomize configs migrated off deprecated v1 fields** (#92, PR #101) — `bases:` → `resources:`, `commonLabels:` → `labels: [{pairs, includeSelectors: true}]` (the `includeSelectors` is load-bearing for the Deployment selector), `patchesStrategicMerge:` → `patches: [{path}]` across seven `kustomization.yaml` files. All eight `kustomize build` entry points verified byte-identical before/after.

### Docs
- **Scrubbed fictional performance-tuning flags and env vars** (#89, PR #100) from the docs — removed references to manager flags and environment variables that `cmd/manager/main.go` never accepted, so the documented surface matches the real one.

### Internal
- **`test/emulation/hardware_emulation_test.go` deleted** (PR #106) — orphaned dead test code (`//go:build integration`, package `emulation`) that no CI job or `make` target ever ran; its mock-Redfish assertions are already covered by `internal/redfishmock/server_test.go`.
- **`charts/beskar7/Chart.yaml`** `version` and `appVersion` bumped to v0.4.0-alpha.6 (this release).
- **`Makefile` `VERSION`** bumped to v0.4.0-alpha.6 — image-tag default for `make docker-build`, `make release-manifests`, etc.

## [v0.4.0-alpha.5] - 2026-05-24

The "backlog cleanup arc": tightened RBAC to per-namespace scope as an opt-in, brought the Prometheus metric surface in line with reality (delete what was dead, wire what should emit), and closed every correctness bug + hygiene item that had been carried since the v0.4-alpha review.

### Added
- **`--watch-namespaces` manager flag** (PR #80) — comma-separated list of namespaces. Empty (default) = watch all namespaces (historical behavior). When set, `ctrl.Options.Cache.DefaultNamespaces` scopes informers to the listed namespaces. Parser is stateless (trim, dedupe, sort) with table-driven coverage in `cmd/manager/flags_test.go`.
- **Helm chart `watchNamespaces` value** (PR #82) — switches the chart's RBAC topology from cluster-wide ClusterRole to per-namespace Role/RoleBinding when set. Default (empty list) is byte-for-byte equivalent to the previous chart output; opting in renders a minimal residual ClusterRole + a leader-election Role in the operator's namespace + a watch Role in each listed namespace. Manager Deployment also picks up the `--watch-namespaces` arg automatically.
- **`config/rbac/namespace-scoped/` overlay** (PR #83) — kustomize parity for the chart change. Reference manifests + per-namespace template + worked-example README. Operators swap their overlay's RBAC base from `../rbac` to `../rbac/namespace-scoped` and duplicate the template per watched namespace.
- **Wire-up of 9 previously-registered-but-unemitted metric helpers** (PR #76): `RecordPhysicalHostState`, `RecordPhysicalHostPowerOperation`, `RecordRedfishConnection`, `RecordBeskar7MachineState`, `RecordBeskar7MachineProvisioning`, `RecordBeskar7ClusterState`, `UpdatePhysicalHostAvailability`, `RecordHostClaimAttempt`, `RecordHostClaimDuration`. Plus extended `RecordError` to fire from `Beskar7MachineReconciler` and `PhysicalHostReconciler` (was only `Beskar7ClusterReconciler`). The advertised metric surface in `docs/metrics.md` now matches what the controllers actually emit.
- **`make lint` Makefile target** (PR #77) pinning golangci-lint to v1.64.8, the version CI uses. Local devs no longer hit "unsupported version of the configuration" errors when a system-installed golangci-lint v2 reads the v1-syntax `.golangci.yml`.

### Fixed
- **BUG-12**: `RecordPowerOperation` accepted an `errorType` parameter that it silently discarded. Parameter removed; error-type cardinality lives on the Redfish-connection / Redfish-query counters already (PR #73).
- **BUG-13**: removed the deprecated `PhysicalHost` state alias constants (`StateClaimed = "InUse"`, `StateProvisioning = "Inspecting"`, `StateProvisioned = "Ready"`, `StateDeprovisioning = "Error"`). They collided with the canonical constants on string value, so switch statements casing on the deprecated names compiled but never matched at runtime. Zero in-tree callers (PR #72).
- **BUG-14**: `PhysicalHostReconciler` error paths no longer return `Result{RequeueAfter: 1*time.Minute}, err`. The workqueue is configured with `workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](5*time.Second, 30*time.Minute)`; error returns now use `Result{}, err` and the rate-limiter governs the retry cadence (5s → 10s → 20s → ... capped at 30m). A persistently-misconfigured BMC settles into a low-frequency check instead of a 60s hot loop (PR #74).
- **BUG-15 dead-metric surface**: deleted 10 broken/redundant `Record*` helpers (`RecordRequeue`, `RecordPhysicalHostProvisioning`, `RecordBootConfiguration`, `RecordPhysicalHostConsumerMapping`, `RecordRedfishQuery`, `RecordNetworkAddress`, `RecordPowerOperation`, `RecordVirtualMediaOperation`, `RecordBootOperation`, `RecordDeprovisioningOperation`), 4 metric variables only referenced by them, and 5 unused `ErrorType` enum values (`ErrorTypeQuery`, `ErrorTypeAddress`, `ErrorTypePower`, `ErrorTypeBoot`, `ErrorTypeVirtualMedia`). Companion `docs/metrics.md` sections removed (PR #75).
- **REFACTOR-1**: `controllers/beskar7cluster_controller.go` no longer uses a string literal for the `"cluster.x-k8s.io/control-plane"` label. Uses `clusterv1.MachineControlPlaneLabel` from the CAPI v1beta1 package, which was already imported (PR #79).
- **`.gitignore`** `manager` rule was unanchored, accidentally hiding any untracked file under `cmd/manager/`. Anchored to `/manager` (repo-root binary only) (PR #80).
- **Dockerfile + Makefile** builds now use `go build ./cmd/...` (package) instead of `go build cmd/.../main.go` (single file). The latter silently dropped sibling files in the same package — broke the Container Build job when `cmd/manager/flags.go` was added (PR #80).
- **`.golangci.yml`** `run.skip-dirs` deprecation warning eliminated by moving `vendor` to `issues.exclude-dirs` (PR #77).

### Changed
- **State-gauge helper API refactored to per-reconcile Set** (PR #76): `RecordPhysicalHostState`/`RecordBeskar7MachineState`/`RecordBeskar7ClusterState` (which took `(label, namespace, delta float64)` and made the caller track previous state to subtract correctly) replaced with `UpdatePhysicalHostStateCounts(ns, map[string]int)`, `UpdateBeskar7MachineStateCounts(...)`, `UpdateBeskar7ClusterStateCounts(...)`. Each reconciler computes counts via a namespace-scoped List at the top of `Reconcile` and Sets each gauge series including zero for absent states. Stateless, restart-safe, no drift.
- **`Beskar7ClusterFailureDomainsGauge`** call patterns unchanged; only the unwired helpers were touched.
- **`RecordReconciliation`, `RecordError`, `RecordFailureDomains`, `RecordFailureDomainDiscovery`** retain their existing behavior; this release wires them more broadly across the three reconcilers (previously only `Beskar7ClusterReconciler` fired `RecordError`).
- **Chart RBAC topology is now selectable** via `.Values.watchNamespaces`. Default (empty) keeps the existing ClusterRole+ClusterRoleBinding. Non-empty list switches to per-namespace Role+RoleBinding pairs plus a minimal ClusterRole for the residual cluster-scoped reads (`clusterroles`, `clusterrolebindings` — kubebuilder-marker autogen).
- **`charts/beskar7/Chart.yaml`** `version` and `appVersion` bumped to v0.4.0-alpha.5 (this release).
- **`Makefile` `VERSION`** bumped to v0.4.0-alpha.5 — image-tag default for `make docker-build`, `make release-manifests`, etc.
- **`Beskar7MachineReconciler.findAndClaimOrGetAssociatedHost`** now records `RecordHostClaimAttempt` + `RecordHostClaimDuration` at all 6 exit branches (success / conflict-via-optimistic-lock / no-hosts / error), giving observability for the PR-2.2 race fix.

### Docs
- **`docs/security/rbac-hardening.md`** rewritten with a two-topology framing (cluster-wide default vs. namespace-scoped opt-in), Helm and kustomize activation paths, and a 4-step backward-compatible migration recipe (PR #84).
- **`docs/security/README.md`** section 5 ("Per-namespace Secret/ConfigMap RBAC scope") updated: no longer punts to a hypothetical v0.5 label-selected partial cache. The actual closure path (watchNamespaces) is documented with links.
- **`docs/ci-cd-and-testing.md`** style-normalized: 43 heading lines stripped of leading emoji and `**...**` bold-wrap to match the rest of `docs/` (PR #81).
- **`docs/metrics.md`** lost the "Wiring status (v0.4.0-alpha.4 → next)" disclaimer that PR #75 added: every advertised metric now emits data.

### Internal
- **9 previously-orphan metric helpers** are now wired at the right controller call sites. Per-reconcile `recompute*Metrics` helpers (`recomputePhysicalHostMetrics`, `recomputeBeskar7MachineMetrics`, `recomputeBeskar7ClusterMetrics`) run at the top of each `Reconcile` so the state gauges and availability ratio stay current even when downstream reconcile errors. Cost is one namespace-scoped List per reconcile, dominated by the reconcile itself.

## [v0.4.0-alpha.4] - 2026-05-23

The "smoke-test arc": a hardware-free CI smoke gate, the four production bugs the gate uncovered, and the CAPI v1.10+ conformance work needed to make any of it run.

### Added
- **CAPI v1beta1 + v1beta2 contract labels** on all four CRDs (PR #65). Without these, CAPI v1.10+ cannot discover Beskar7 infrastructure objects at all.
- **CAPI v1beta2 status contract**: `status.initialization.provisioned` on `Beskar7Cluster` and `Beskar7Machine` (PR #68). CAPI core lifts this into the parent `Cluster`/`Machine.status.initialization.infrastructureProvisioned`; without it, KubeadmConfig never generates bootstrap data and Beskar7Machine never mints its bearer token.
- **`cmd/mock-inspector` binary** (PR #69) — standalone Go binary that simulates an iPXE-booted inspector POST to the controller's bootstrap callback. Published as `ghcr.io/projectbeskar/beskar7/mock-inspector:<tag>` alongside the controller. Powers smoke layer 5; also doubles as a manual-demo entry point via `hack/smoke/manifests/50-mock-inspector-job.yaml`.
- **kind-based CI smoke gate** (PR #66): every PR's `E2E Setup Validation` job spins up kind, installs cert-manager + CAPI core + the in-tree Helm chart, then runs `make smoke` end-to-end through layer 5 (ProviderID assertion). Replaces the previous ad-hoc "PhysicalHost against test.example.com" check.
- **Unit-test coverage for `internal/redfishmock`** (PR #67): 88.9% statement coverage. Documents the spec-conformant unauthenticated service root as a regression test.
- **`+kubebuilder:rbac` marker for `beskar7machinetemplates`** so generated `config/rbac/role.yaml` matches the chart's hand-maintained RBAC (PR A — this CHANGELOG entry's PR).

### Fixed
- **`findAndClaimOrGetAssociatedHost` re-find by `ConsumerRef.Name`** (PR #68). Previously the controller could only re-find a claimed host by `Spec.ProviderID` (set only after inspection) or by `Status.State=Available` (filters out claimed hosts). After a claim, the host transitioned to `InUse` and became invisible to the controller; provisioning looped "No available host" forever. New third lookup branch returns the host whose `ConsumerRef.Name` matches this Beskar7Machine.
- **Bootstrap-token mint race** (PR #68). `bootstrapTokenStillValid` now honours a pending `BootstrapTokenAnnotation`; the `setBootstrapTokenAnnotation` patch dropped its `OptimisticLock` (the annotation key is unique to this controller, so the lock was over-defensive and was actually causing the failure mode it was meant to prevent — repeated Conflicts, repeated re-mints, every Secret write overwriting the previous plaintext while Status held a different hash). Inspector callbacks 401'd as a result.
- **`setInspectionResultAnnotation` `OptimisticLock` drop** (PR #68). Same shape as the bootstrap-token annotation fix.
- **ConfigMap RBAC + informer pre-warm** (PR #68). Added `list, watch` to the ConfigMap RBAC so the controller-runtime cache can populate the informer the inspection handler uses; the handler also pre-warms the informer at `SetupCallbackServer` time so the first POST does not stall on initial sync.

### Changed
- `bootstrapTokenStillValid` now inspects both `Status.Bootstrap` and the pending `BootstrapTokenAnnotation` (PR #68 — companion to the mint-race fix).
- Helm chart `namespace.create` default flipped from `true` to `false` and chart pod-level `securityContext` now includes `fsGroup: 65532`, `runAsGroup: 65532`, `seccompProfile.type: RuntimeDefault` to match `config/manager/manager.yaml` (PR #62, shipped in alpha.2 but logged here for completeness).

### Smoke-rig changes
- Smoke runner gains layer 5 (inspector POST → `state=Ready` → `Beskar7Machine.Spec.ProviderID`) using the new `cmd/mock-inspector` Job (PR #69).
- Smoke fixture restructured: `40-cluster-and-machine.yaml` pre-bakes a bootstrap data Secret and points `Machine.Spec.Bootstrap.DataSecretName` at it directly, bypassing KubeadmConfig generation (which would otherwise wait forever for a non-existent control plane).
- Runner auto-derives both `mock-redfish` and `mock-inspector` image tags from the installed controller. `MOCK_IMAGE` and `MOCK_INSPECTOR_IMAGE` env vars override.
- Smoke runner teardown force-removes finalizers after a 5s grace period so the namespace can finalize cleanly.

## [v0.4.0-alpha] - 2025-11-27

### Added
- TLS CA bundle support on `RedfishConnection` via `caBundleSecretRef` (PR-3.2 / SEC-5). Mutually exclusive with `insecureSkipVerify=true`; conflict reported as `RedfishConnectionReady=False (InsecureCABundleConflict)`.
- Per-host bearer-token authentication on the host callback endpoint `:8082` (PR-5.1, PR-5.2 / D-004). 32-byte random token, SHA-256 hash on `PhysicalHost.Status.Bootstrap.TokenHash`, plaintext in a per-host Secret named `<host>-bootstrap-token` (PR-5.2 / D-006).
- Bootstrap GET endpoint `GET /api/v1/bootstrap/{ns}/{name}` on `:8082`, gated by the same bearer-token middleware as the inspection POST (PR-5.3 / D-003). The reconciler reads `Machine.Spec.Bootstrap.DataSecretName` and signals the per-host URL to the host via the `infrastructure.cluster.x-k8s.io/bootstrap-url` annotation (PR-1.1).
- `BootstrapStatus` sub-object on `PhysicalHostStatus` with `URL`, `TokenHash`, `IssuedAt`, `ExpiresAt` (PR-5.1).
- `--bootstrap-url-base`, `--inspection-port`, `--inspection-cert-dir`, `--secure-metrics` manager flags (PR-5.x, PR-11.1).
- `ForceReleaseAnnotation` (`infrastructure.cluster.x-k8s.io/force-release=true`) on Beskar7Machine to skip Redfish power-off / boot-clear during deletion (PR-2.3 / BUG-4).
- Watch on `PhysicalHost` from the `Beskar7Machine` reconciler so host state changes trigger immediate machine reconciles (PR-2.3 / BUG-7).
- Cache field index on `PhysicalHost.Status.State` so the `Beskar7Machine` reconciler filters `Available` hosts server-side (PR-2.2 / BUG-2).
- Helm chart parity: `--enable-webhook` wired in Deployment args; webhook configuration template ships MWC + VWC for Beskar7Cluster only; always-on `<release>-controller-manager` Service on port 8082; NetworkPolicy ingress for 8082; CRDs synced from `config/crd/bases/`; `sync-chart-crds` and `manifests-and-sync` Makefile targets (PR-4 / BLOCK-3,4,5).

### Changed
- Metrics now served directly on `:8443` (HTTPS) with TokenReview/SubjectAccessReview authentication via controller-runtime's `WithAuthenticationAndAuthorization` (PR-11.1). The `kube-rbac-proxy` sidecar has been removed; metrics auth runs in-process. Scrapers need the `metrics-reader` ClusterRole.
- `PhysicalHost` state machine simplified to `Available` / `InUse` / `Inspecting` / `Ready` / `Error` (the v0.3 `Claimed`/`Provisioning`/`Provisioned`/`Deprovisioning` strings are gone; deprecated Go aliases map them all to the new strings).
- `PhysicalHost.Status.InspectionReport` is now an array-of-structs shape: `cpus []CPUInfo`, `memory []MemoryInfo`, `disks []DiskInfo`, `nics []NICInfo`. The flat-object shape from earlier drafts is gone.
- `Beskar7MachineSpec` simplified to `inspectionImageURL`, `targetImageURL`, `configurationURL`, `hardwareRequirements`. The v0.3 fields (`imageURL`, `osFamily`, `provisioningMode`, `bootMode`, `configURL`) are removed.
- Controllers use `patch.NewHelper` deferred at the top of `Reconcile`; no `r.Update`/`r.Status().Update` in the same reconcile cycle (PR-2.1 / BUG-5).
- The inspection HTTP handler no longer writes to `PhysicalHost.Status` directly. It writes the validated `InspectionReport` to a ConfigMap and patches an annotation; the `PhysicalHost` reconciler is the sole writer of `Status.InspectionReport`/`Status.InspectionPhase` (PR-5.2 / D-005).
- `SetPowerState(OffPowerState)` issues `GracefulShutdown` (was `ForceOff`); a separate `ForcePowerOff` method is available for callers that need an immediate cut (PR-3.1 / BUG-6).
- All gofish I/O calls now race against `ctx` cancellation with a 30-second per-call HTTP timeout (PR-3.1 / BUG-10).
- Memory-capacity parser accepts `GB`/`GiB`/`MB`/`MiB`/`TB`/`TiB` and rejects bare integers and exotic SI prefixes (PR-3.3 / BUG-11).
- BMC username and password no longer logged at any verbosity (PR-3.3 / SEC-4). At V(1), the structured logger emits `passwordProvided` (boolean) only.
- RBAC: cluster-wide ClusterRole, but Secret reads in the controllers are by name only. Cluster-wide `secrets: list,watch` retained for the credentials-rotation informer (residual scope tracked as SEC-2 / D-007).

### Removed
- Dead-code packages `internal/coordination/` and `internal/security/{monitor,rbac_validator,tls_validator}.go` (PR-6.1 / D-001). Companion cleanup removed unused metric definitions, the `config/security/security-policy.yaml` ConfigMap, and references in `config/security/kustomization.yaml`.
- `kube-rbac-proxy` sidecar (PR-11.1). Metrics auth is now in-process.
- Documentation overclaims: the `beskar7-security-policy` ConfigMap, the `--enable-security-monitoring` flag, the `beskar7-security-monitor` CronJob, and CIS/NIST/SOC2/ISO27001 compliance claims have been removed from the security docs (none of these were ever shipped).

### Fixed
- `parseProviderID` rejects malformed and multi-segment provider IDs (PR-1.3 / BUG-3).
- Atomic host claim — concurrent claims are resolved server-side via `MergeFromWithOptimisticLock`; exactly one Beskar7Machine wins (PR-2.2 / BUG-2).
- Clean release on Beskar7Machine deletion: `ClearBootSourceOverride` + graceful power-off before clearing `ConsumerRef`; errors are logged and swallowed so a dead BMC cannot strand the finalizer (PR-2.3 / BUG-4).

### Security
- Inspection POST endpoint now requires TLS and `Authorization: Bearer <token>`. Body capped at 1 MiB; over-limit returns 413; auth failures return opaque 401 (PR-5.2 / SEC-1).
- Bootstrap GET endpoint requires the same per-host bearer token; chain-walk failures collapse to opaque 404 to avoid leaking host topology (PR-5.3).

### BREAKING CHANGES

This release represents a complete architectural redesign of Beskar7, moving from a complex VirtualMedia-based provisioning system to a simplified iPXE + inspection workflow. This is a major version bump with significant breaking changes.

#### Removed Features (Breaking)
- **VirtualMedia Provisioning**: Complete removal of ISO mounting capabilities
  - Removed `SetBootSourceISO()` method from Redfish client
  - Removed `EjectVirtualMedia()` method
  - Removed `findFirstVirtualMedia()` helper
  - Removed all boot parameter injection logic
- **Vendor-Specific Workarounds**: Deleted all vendor-specific code
  - Deleted `internal/redfish/bios_manager.go` (150+ lines)
  - Deleted `internal/redfish/vendor.go` (200+ lines)
  - Removed BIOS configuration manipulation
  - Removed vendor detection and quirk handling
- **Provisioning Modes**: Removed all legacy provisioning modes
  - Removed `PreBakedISO` mode
  - Removed `RemoteConfig` mode
  - Removed traditional `PXE` mode (TFTP-based)
  - Only `iPXE` mode remains (HTTP-based)
- **API Fields**: Removed deprecated fields from Beskar7MachineSpec
  - Removed `ImageURL` field
  - Removed `ConfigURL` field
  - Removed `OSFamily` field
  - Removed `ProvisioningMode` field
  - Removed `BootMode` field (UEFI only now)
- **Complex Coordination**: Removed host claim coordination package
  - Deleted `internal/coordination/` package (500+ lines)
  - Deleted `HostClaimCoordinator`
  - Simplified to direct PhysicalHost.Spec.ConsumerRef assignment
- **State Machine**: Removed complex state machine implementation
  - Deleted `internal/statemachine/` package
  - Replaced with simple phase-based status tracking
- **Webhooks**: Removed PhysicalHost webhook implementations
  - No defaulting webhook for PhysicalHost
  - No validation webhook for PhysicalHost
  - PhysicalHost relies on controller-based validation only

### Major Features

#### iPXE + Inspection Workflow
- **New Provisioning Architecture**: Completely redesigned provisioning flow
  - Boot target machine via iPXE to inspection image
  - Inspection image collects hardware details
  - Hardware report sent to Beskar7 controller
  - Validation of hardware against requirements
  - Kexec into final operating system
- **Inspector Image**: Created separate beskar7-inspector repository
  - Alpine Linux-based inspection environment
  - Hardware detection scripts (CPU, memory, disks, NICs)
  - Automatic reporting to Beskar7 API
  - Kexec-based boot into target OS
  - Repository: https://github.com/projectbeskar/beskar7-inspector
- **Inspection HTTP API**: New endpoint for receiving inspection reports
  - Endpoint: `POST /api/v1/inspection/{namespace}/{physicalhost-name}`
  - Listens on port 8082
  - Token-based authentication
  - Automatic PhysicalHost status updates

#### API Enhancements

##### PhysicalHost API
- **InspectionReport Type**: New structured hardware information (array of structs per category)
  - `cpus []CPUInfo`: per-CPU `id`, `vendor`, `model`, `cores`, `threads`, `frequency`
  - `memory []MemoryInfo`: per-DIMM `id`, `type`, `capacity` (string with `GB`/`GiB`/`MB`/`MiB`/`TB`/`TiB`), `speed`
  - `disks []DiskInfo`: per-disk `name`, `model`, `sizeGB`, `type` (SSD/HDD/NVMe), `serialNumber`
  - `nics []NICInfo`: per-NIC `name`, `macAddress`, `driver`, `speed`, `ipAddresses []string`
  - System metadata: `manufacturer`, `model`, `serialNumber`, `bootModeDetected`, `firmwareVersion`
- **InspectionPhase Enum**: New phase tracking
  - `Pending`: Inspection not yet started
  - `Booting`: iPXE boot in progress
  - `InProgress`: Inspection scripts running
  - `Complete`: Hardware report received
  - `Failed`: Inspection encountered errors
  - `Timeout`: Inspection took too long
- **State Simplification**: Cleaner state model
  - Added `StateNone`, `StateUnknown`, `StateEnrolling`
  - Removed complex transition logic
  - Controller-driven state management

##### Beskar7Machine API
- **New Fields**: Inspection workflow configuration
  - `inspectionImageURL`: iPXE script URL that boots the inspection image
  - `targetImageURL`: URL for the final OS image (kexec target after inspection)
  - `configurationURL`: optional OS configuration URL passed through to the target
  - `hardwareRequirements`: minimum CPU / memory / disk validated against the inspection report
  - `BootMode`: removed (UEFI only)
- **Condition Constants**: Added condition types
  - `MachineProvisionedCondition`
  - `WaitingForHostReason`
  - `InspectionFailedReason`
  - `InspectionTimedOutReason`

### Enhancements

#### Redfish Client Simplification
- **Minimal Interface**: Reduced to essential operations only
  - `GetSystemInfo()`: Basic system information
  - `GetPowerState()`: Current power status
  - `SetPowerState()`: Power control (On, Off, ForceOff, GracefulShutdown)
  - `SetBootSourcePXE()`: Configure one-time PXE boot
  - `Reset()`: System reset for troubleshooting
  - `GetNetworkAddresses()`: Network interface discovery
- **Removed Complexity**: No more vendor-specific code paths
- **Better Error Handling**: Simplified error propagation
- **Reduced Dependencies**: Smaller gofish client footprint

#### Controller Simplification

##### PhysicalHost Controller
- **Power Management Only**: Removed all provisioning logic
  - Redfish connection validation
  - Power state monitoring
  - Basic system info gathering
  - State transitions: Available -> InUse (when claimed)
- **No Webhooks**: Validation happens in controller, not webhooks
- **Cleaner Reconciliation**: Single responsibility principle

##### Beskar7Machine Controller
- **Inspection Workflow**: New reconciliation phases
  1. **Claim Phase**: Find and claim available PhysicalHost
  2. **Boot Phase**: Configure PXE boot and power on
  3. **Inspection Wait**: Monitor for hardware report
  4. **Validation Phase**: Verify hardware meets requirements
  5. **Provisioning Phase**: Wait for final OS kexec and readiness
- **Hardware Validation**: Implemented requirement checking
  - Minimum CPU cores
  - Minimum memory GB
  - Disk requirements
  - Network interface requirements
- **Simplified Logic**: Removed mode-specific branching
- **Better Logging**: Clear phase transitions and status updates

##### Beskar7Cluster Controller
- **No Changes**: Control plane endpoint logic unchanged
- **Compatible**: Works with new simplified machine controller

### Bug Fixes

#### Critical Fixes
- **Linter Errors**: Fixed all 330+ linter errors across 21 files
  - Removed unused imports
  - Fixed variable shadowing
  - Corrected type mismatches
  - Added missing error checks
- **Test Suite**: Fixed failing unit tests
  - Added proper CAPI Machine owner references
  - Fixed type assertions for new API fields
  - Updated mock clients for new interfaces
  - 26 tests passing, 11 deferred to hardware testing
- **CI/CD Pipeline**: Fixed all 7 GitHub Actions workflows
  - Lint and Code Quality: Passing
  - Security Scanning: Passing
  - Unit Tests: 26/26 passing
  - Integration Tests: Passing
  - Container Build and Test: Passing
  - Generate and Validate Manifests: Passing
  - E2E Setup Validation: Passing
- **Webhook Configurations**: Removed orphaned webhook references
  - Deleted PhysicalHost mutating webhook config
  - Deleted PhysicalHost validating webhook config
  - Fixed E2E test to check existing webhooks only

#### Code Quality
- **gofmt Compliance**: Applied `gofmt -s -w .` to entire codebase
- **Struct Alignment**: Fixed field alignment in all structs
- **DeepCopy Methods**: Regenerated for new InspectionReport types
- **Manifest Generation**: Fixed kustomize regex errors
  - Changed `kind: "*"` to `kind: ".*"` for proper regex matching
  - Fixed sed backup file handling in Makefile

### Documentation

#### New Documentation
- **iPXE Setup Guide**: Comprehensive iPXE infrastructure documentation
  - `docs/ipxe-setup.md`: iPXE server setup, DHCP configuration, boot scripts
  - Network boot infrastructure requirements
  - Example iPXE boot script with kernel parameters
  - Dynamic boot parameter injection guide
- **Inspector README**: Complete documentation for beskar7-inspector
  - Hardware detection capabilities
  - Inspection workflow
  - API communication
  - Kexec boot process

#### Updated Documentation
- **README.md**: Major rewrite for new architecture
  - Updated feature list (iPXE-only)
  - Removed VirtualMedia references
  - Added inspection workflow diagram
  - Updated quick start guide
- **Architecture Documentation**: Reflects simplified design
  - Single provisioning path (iPXE)
  - Inspection-based hardware discovery
  - No vendor-specific code
- **API Reference**: Updated for new fields
  - InspectionReport structure
  - InspectionPhase enum
  - Removed deprecated fields
- **Troubleshooting**: Updated for new workflow
  - Removed VirtualMedia troubleshooting
  - Added inspection debugging steps
  - Added iPXE boot troubleshooting

#### Removed Documentation
- **VirtualMedia Guides**: Deleted obsolete provisioning docs
- **Vendor Workarounds**: Removed vendor-specific documentation
- **Multi-Mode Examples**: Deleted PreBakedISO and RemoteConfig examples
- **PXE Mode**: Removed TFTP-based PXE documentation

### Examples

#### New Examples
- **simple-cluster.yaml**: Updated for iPXE + inspection workflow
  - Shows `inspectionImageURL` and `targetImageURL` fields
  - Hardware requirements specification
  - Simplified configuration

#### Removed Examples
- **pxe-provisioning-example.yaml**: Traditional PXE mode removed
- **pxe-simple-test.yaml**: TFTP-based testing removed
- **PXE_QUICK_START.md**: Obsolete quick start guide
- **PXE_TESTING_GUIDE.md**: Obsolete testing procedures
- **pxe-ipxe-prerequisites.md**: Replaced with docs/ipxe-setup.md

### Testing

#### Test Updates
- **Unit Tests**: Comprehensive updates for new architecture
  - Fixed Beskar7Machine controller tests (7 tests)
  - Fixed PhysicalHost controller tests (3 tests)
  - Fixed Beskar7Cluster controller tests (1 test)
  - 11 complex integration tests deferred to hardware testing
- **Integration Tests**: Simplified test suite
  - Removed concurrent provisioning tests (obsolete)
  - Created placeholder for future integration tests
- **E2E Tests**: Updated for webhook changes
  - Removed PhysicalHost webhook validation
  - Tests CRD creation and controller startup
  - Validates webhook connectivity for implemented webhooks

### Internal Improvements

#### Code Deletion
- **Removed Files**: Cleaned up obsolete implementation
  - `internal/redfish/bios_manager.go` (deleted)
  - `internal/redfish/vendor.go` (deleted)
  - `internal/coordination/` package (deleted)
  - `internal/statemachine/` package (deleted)
  - `controllers/template_controller.go` (deleted)
  - `api/v1beta1/validation.go` (deleted)
  - Integration tests for old architecture (deleted)
- **Lines Removed**: Over 2000 lines of code deleted
- **Complexity Reduction**: Significantly simplified codebase

#### Build System
- **Dockerfile**: Updated Go version to 1.25
- **Makefile**: Fixed manifest generation with proper sed handling
- **CI Configuration**: Updated all workflow steps for new architecture

### Migration Guide

#### For Existing Users

**This release is NOT backward compatible. A complete redeployment is required.**

##### What to Do Before Upgrading
1. **Backup existing resources**: Export all Beskar7Machine and PhysicalHost resources
2. **Document configurations**: Note any custom configurations or workarounds
3. **Plan downtime**: This is a clean-break upgrade requiring full redeployment

##### Migration Steps
1. **Set up iPXE infrastructure**
   - Configure iPXE boot server (HTTP-based)
   - Deploy DHCP with iPXE chainloading
   - Host inspection image and target OS images
   - See `docs/ipxe-setup.md` for complete guide
2. **Deploy beskar7-inspector image**
   - Build or pull beskar7-inspector:1.0
   - Host inspection image on HTTP server
   - Configure inspection endpoint URL
3. **Update CRDs**
   - Delete old CRDs (they are incompatible)
   - Apply new CRDs from v0.4.0-alpha manifests
4. **Recreate resources**
   - Convert Beskar7Machine specs to new format
   - Remove: `imageURL`, `configURL`, `osFamily`, `provisioningMode`, `bootMode`
   - Add: `inspectionImage`, `targetOSImage`
   - Adjust hardware requirements if needed
5. **Redeploy Beskar7 controller**
   - Use new v0.4.0-alpha manifests
   - Ensure inspection endpoint is accessible from hosts
   - Monitor logs for inspection workflow

##### What Will NOT Work
- Any ISO-based provisioning configurations
- VirtualMedia references in PhysicalHost specs
- RemoteConfig or PreBakedISO provisioning modes
- Legacy PXE (TFTP) configurations
- Vendor-specific workarounds or BIOS settings
- BootMode selection (UEFI only)

##### What You Gain
- **Simpler architecture**: Easier to understand and troubleshoot
- **No vendor lock-in**: Generic iPXE + kexec workflow works everywhere
- **Better observability**: Hardware inspection provides rich details
- **Faster provisioning**: Direct network boot, no ISO mounting delays
- **Reduced complexity**: No more vendor quirks or BIOS manipulation
- **Cleaner code**: 2000+ lines removed, easier to contribute to

### Statistics

- **Code Changes**: 50+ files modified
- **Lines Removed**: 2000+ lines of complex code deleted
- **Lines Added**: 1500+ lines of new inspection workflow
- **Documentation**: 10+ files updated, 5 obsolete docs removed
- **Tests**: 26 unit tests passing, 11 deferred to hardware phase
- **CI Workflows**: All 7 workflows passing
- **Linter Errors Fixed**: 330+ errors resolved
- **Breaking Changes**: Major version bump warranted

### Known Limitations

#### Hardware Testing Pending
- **Real Hardware Validation**: Inspection workflow not yet tested on physical servers
- **Deferred Tests**: 11 integration tests marked as pending, require hardware
- **Kexec Validation**: Kexec boot into final OS not validated end-to-end
- **Network Stack**: Network persistence from inspection to final OS not tested

#### Future Work
- Hardware testing on real servers (Dell, HP, Supermicro, etc.)
- Performance benchmarking of inspection workflow
- Additional hardware detection (GPU, RAID controllers, etc.)
- Inspection timeout tuning based on real-world data
- Documentation improvements based on field testing feedback

### Acknowledgments

This release represents a complete rethinking of Beskar7's architecture, prioritizing simplicity and reliability over feature breadth. The decision to remove VirtualMedia support was made after extensive experience showing it to be unreliable and vendor-specific.

Special thanks to the Cluster API community for the excellent foundation, and to the iPXE and Alpine Linux projects for enabling this simplified workflow.

### Notes

**Why This Major Refactoring?**

The previous architecture (v0.3.4-alpha) relied heavily on Redfish VirtualMedia, which proved to be:
- Unreliable across vendors (Dell, HP, Supermicro all behave differently)
- Complex to implement (300+ lines of vendor-specific workarounds)
- Slow to provision (ISO mounting and BMC limitations)
- Hard to debug (black-box BMC behavior)

The new iPXE + inspection workflow is:
- Vendor-agnostic (standard PXE boot + HTTP)
- Simple to implement (no vendor quirks)
- Fast (direct network boot, no ISO overhead)
- Observable (rich inspection data, clear phases)

This is a **clean break** from the past, setting Beskar7 on a path toward production readiness.

## [v0.3.4-alpha] - 2025-10-23

### Major Features

#### Network Boot Support
- **PXE/iPXE Provisioning**: Full implementation of PXE and iPXE network boot modes
  - Added `SetBootSourcePXE` method to Redfish client interface
  - Implemented BMC configuration for network boot (PXE/UEFI)
  - Added comprehensive examples and documentation
  - Infrastructure prerequisites guide with full setup instructions
- **Provisioning Modes**: All four modes now fully documented and working
  - `PreBakedISO` - Pre-configured ISO boot
  - `RemoteConfig` - Generic ISO with remote configuration
  - `PXE` - Traditional network boot via TFTP
  - `iPXE` - Modern network boot via HTTP

#### Boot Mode Control
- **Boot Mode Field**: Added `bootMode` field to `Beskar7MachineSpec` API
  - Supports `UEFI` (recommended) and `Legacy` boot modes
  - Webhook validation for boot mode values
  - Updated all examples to include boot mode configuration
  - CRD manifests regenerated with new field

### Enhancements

#### Hardware Management
- **Hardware Requirements Matching**: Implemented label-based host selection
  - Added `RequiredLabels` and `PreferredLabels` to `HostRequirements`
  - Implemented label matching logic in `HostClaimCoordinator`
  - CPU/Memory requirements documented (pending HardwareDetails enhancement)
  - Comprehensive logging for host selection decisions

#### Network Discovery
- **Network Interface Traversal**: Enhanced network address detection
  - Implemented NetworkPorts traversal in Redfish client
  - Implemented NetworkDeviceFunctions traversal
  - Added comprehensive logging for network discovery
  - Documented standard Redfish schema limitations

### Bug Fixes

#### API & Validation
- **OS Family Cleanup**: Removed unsupported operating systems from API
  - Removed: `talos`, `ubuntu`, `rhel`, `centos`, `fedora`, `debian`, `opensuse`
  - Retained: `kairos` (recommended), `flatcar`, `LeapMicro`
  - Updated all tests to use supported OS families
  - Regenerated CRD manifests with correct enum values
  - Updated Helm chart CRDs

#### Test Suite
- **Test Coverage**: Fixed and unskipped all previously skipped tests
  - Fixed control plane endpoint detection tests (2 tests)
  - Fixed RemoteConfig validation test
  - Updated test to use LeapMicro instead of Talos
  - Added comprehensive condition checks in PhysicalHost tests
  - All controller tests now passing

### Documentation

#### Complete Documentation Overhaul
- **Comprehensive PXE/iPXE Guide**: 67-page infrastructure prerequisites document
  - Network infrastructure setup (VLANs, routing, topology)
  - DHCP server configuration (ISC DHCP, dnsmasq)
  - TFTP server setup for PXE
  - HTTP server setup for iPXE with nginx configuration
  - OS image hosting and management
  - Firewall rules and port requirements
  - Validation checklist and automated validation script
  - Complete troubleshooting guide

- **Quick Start Guides**:
  - `PXE_QUICK_START.md` - 5-minute testing guide
  - `PXE_TESTING_GUIDE.md` - Comprehensive testing procedures
  - Complete example YAML files for all provisioning modes

- **Examples**:
  - `pxe-simple-test.yaml` - Quick PXE/iPXE testing
  - `pxe-provisioning-example.yaml` - Full PXE cluster deployment
  - `ipxe-provisioning-example.yaml` - Full iPXE cluster deployment

#### Documentation Alignment
- **API Reference**: Updated to reflect only supported features
  - Accurate OS family documentation
  - Boot mode field documented
  - UserDataSecretRef status clarified (pending full integration)
  - Provisioning mode requirements clearly stated

- **README Updates**: Complete rewrite of key sections
  - Added "Supported Features" section
  - All four provisioning modes documented with examples
  - Hardware and OS compatibility tables
  - Quick reference section for common tasks
  - Recent updates section

- **Hardware Compatibility**: Updated matrix
  - Only supported OS families listed
  - Clear note about unsupported traditional distributions
  - OS-specific configuration requirements documented

### Internal Improvements

#### Code Quality
- **Removed TODO Comments**: Cleaned up implementation TODOs
  - Removed generic kubebuilder scaffold comments
  - Removed completed TODO markers
  - Converted remaining TODOs to tracked issues

#### API Cleanup
- **Type Definitions**: Streamlined and validated
  - Removed references to unsupported OS families
  - Added proper validation annotations
  - Updated webhook validation logic

#### Testing Infrastructure
- **Mock Clients**: Enhanced test mocks
  - Added `SetBootSourcePXE` to mock client
  - Updated test assertions for new functionality
  - Improved test coverage across all controllers

### Breaking Changes

 **OS Family Support**: The following OS families have been removed from the API enum:
- `talos` - Removed (open for community contribution)
- `ubuntu`, `rhel`, `centos`, `fedora`, `debian`, `opensuse` - Removed (never fully implemented)

**Migration Path**:
- Update any `Beskar7Machine` resources using removed OS families
- Use `kairos` (recommended), `flatcar`, or `LeapMicro` instead
- See documentation for OS-specific configuration requirements

 **API Changes**: 
- Added optional `bootMode` field to `Beskar7MachineSpec`
  - Defaults to `UEFI` if not specified
  - No action required for existing resources (backward compatible)

### New Files

#### Examples
- `examples/pxe-simple-test.yaml`
- `examples/pxe-provisioning-example.yaml`
- `examples/ipxe-provisioning-example.yaml`
- `examples/pxe-ipxe-prerequisites.md` (1165 lines)
- `examples/PXE_QUICK_START.md`
- `examples/PXE_TESTING_GUIDE.md`

#### Documentation
- Updated all documentation files for accuracy
- Added PXE/iPXE infrastructure guides
- Enhanced troubleshooting documentation

### Technical Details

#### API Changes
```go
// Added to Beskar7MachineSpec
BootMode string `json:"bootMode,omitempty"` // UEFI or Legacy
```

#### New Redfish Client Methods
```go
SetBootSourcePXE(ctx context.Context) error
```

#### Enhanced Coordination
```go
// HostRequirements - Added fields
RequiredLabels  map[string]string
PreferredLabels map[string]string
```

### Statistics

- **Code Changes**: 25+ files modified
- **Documentation**: 11 files updated, 3 comprehensive guides added
- **Examples**: 4 new complete examples
- **Tests**: 3 previously skipped tests fixed and unskipped
- **TODO Items**: 13 resolved
- **API Enum Cleanup**: 7 unsupported OS families removed
- **Lines of Documentation**: 1500+ new lines

### Project Status

**Alpha Release**: This release significantly improves the project's maturity:
- All provisioning modes implemented and documented
- Complete alignment between API, code, and documentation
- Comprehensive testing and examples
- Clear feature support documentation

### Notes

This release represents a comprehensive audit and cleanup of the entire codebase:
- Resolved all TODO comments from code audit
- Fixed all API/documentation misalignments
- Implemented missing critical features (PXE/iPXE, boot mode)
- Enhanced test coverage and quality
- Created comprehensive infrastructure guides

For detailed implementation information, see the examples directory and documentation.

## [v0.2.7] - 2025-08-11
### ✨ Features
- **Security Scanning**: Re-enabled Trivy security scanning for public repository with SARIF upload to GitHub Security tab
- **Enhanced CI/CD**: Improved E2E validation with local image building and comprehensive debugging
- **Webhook Integration**: Complete PhysicalHost webhook validation and mutation support

### 🐛 Bug Fixes
- **Critical**: Fixed PhysicalHost finalizer removal bug that caused indefinite deletion hanging
- **CI**: Fixed container image availability in E2E tests by building locally and setting `imagePullPolicy: Never`
- **CI**: Corrected deployment name mismatch in E2E validation (`beskar7-controller-manager` → `controller-manager`)
- **CI**: Fixed kind cluster name mismatch in image loading (`kind` → `beskar7-test`)
- **Linting**: Resolved variable shadowing in `internal/redfish/gofish_client.go`
- **Linting**: Fixed `gosimple` S1021 error in controller tests
- **Manifests**: Fixed `${VERSION}` placeholder substitution in Kubernetes manifests

### 🔧 Improvements
- **Code Quality**: Introduced constants for hardcoded strings (security levels, API versions, URL schemes)
- **Error Handling**: Enhanced webhook error diagnostics and CI failure debugging
- **Testing**: Added comprehensive E2E test timeout protection and cleanup validation
- **Dependencies**: Updated CI job dependencies to ensure proper build order
- **Documentation**: Improved inline code documentation and error messages

### 🛠️ Infrastructure
- **CI Pipeline**: Complete overhaul of GitHub Actions workflow with proper job dependencies
- **Container Build**: Optimized Docker build process with proper caching and multi-platform support
- **Manifest Generation**: Automated version substitution in release manifests
- **Quality Gates**: Comprehensive linting, testing, and security scanning integration

### 📦 Dependencies
- **golangci-lint**: Improved compatibility with v1.64.8 in CI environment
- **cert-manager**: Enhanced certificate management in E2E validation
- **kind**: Better integration with local Kubernetes testing

## [v0.2.6] - 2025-08-10
- Initial public manifests bundle.
- CI: lint, tests, container build, CRD generation, Kind sanity checks.
- Core controllers and CRDs for `PhysicalHost`, `Beskar7Machine`, `Beskar7Cluster`.

[Unreleased]: https://github.com/projectbeskar/beskar7/compare/v0.4.0-alpha.6...HEAD
[v0.4.0-alpha.6]: https://github.com/projectbeskar/beskar7/compare/v0.4.0-alpha.5...v0.4.0-alpha.6
[v0.4.0-alpha.5]: https://github.com/projectbeskar/beskar7/compare/v0.4.0-alpha.4...v0.4.0-alpha.5
[v0.4.0-alpha.4]: https://github.com/projectbeskar/beskar7/compare/v0.4.0-alpha...v0.4.0-alpha.4
[v0.4.0-alpha]: https://github.com/projectbeskar/beskar7/compare/v0.3.4-alpha...v0.4.0-alpha
[v0.3.4-alpha]: https://github.com/projectbeskar/beskar7/compare/v0.2.7...v0.3.4-alpha
[v0.2.7]: https://github.com/projectbeskar/beskar7/releases/tag/v0.2.7
[v0.2.6]: https://github.com/projectbeskar/beskar7/releases/tag/v0.2.6

