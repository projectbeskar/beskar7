# Beskar7 — Project Context (Living Document)

> **Read this file before starting any non-trivial change.** Update it whenever you close a tracked item or discover a new one. This is the project's working memory between Claude sessions.
>
> **Last meaningful update:** 2026-05-02 — PR-2.3 closed BUG-4 (clean release on delete: ClearBootSourceOverride + graceful power-off + ForceReleaseAnnotation escape hatch), BUG-7 (Beskar7Machine now watches PhysicalHost), and the residual BUG-1 risk (latency from annotation-only signal eliminated by the new PhysicalHost watch).

---

## How to use this file

- The **Now** sections describe what's true right now. Keep them current.
- The **Punch list** is the prioritized backlog of known issues from review.
- The **Decisions** log is append-only — record architectural calls so we don't relitigate them.
- The **Glossary** disambiguates terms that show up across the codebase.

When you finish work that resolves a punch-list item:

1. Move it from the punch list to the **Recently closed** section with the PR / commit reference.
2. If the work surfaced a new issue, add it to the punch list with severity.
3. If you made an architectural call, append a Decisions entry.

When you start work, scan the **In flight** section to avoid stepping on someone else's change.

---

## State of the repo (Now)

- **Version**: `v0.4.0-alpha` — a clean break from `v0.3.x`. Not API-stable.
- **Module**: `github.com/projectbeskar/beskar7`
- **Go**: `1.25.0` (`go.mod`); CI uses `1.25`.
- **Branch state**: `main` is the trunk. The current worktree is `claude/vibrant-euclid-4405f3` for review work.
- **Test posture**: envtest set up in `controllers/suite_test.go`; many `PIt` (pending) blocks in controller tests; integration tests gated by `-tags integration` (CI does set this).
- **Lint**: `golangci-lint v1.64.8` with errcheck/gosimple/govet/ineffassign/staticcheck/typecheck/unused.
- **Critical paths exercised by no automated test today**: bootstrap data secret consumption, host claim race between two Beskar7Machines, finalizer ordering on delete (host power-off + boot-override clear), inspection timeout firing, inspection late-arrival.

## Architecture (Now)

Three reconcilers + one HTTP handler + one webhook:

- `Beskar7ClusterReconciler` (`controllers/beskar7cluster_controller.go`) — uses patch helper deferred at top of Reconcile (the reference pattern).
- `Beskar7MachineReconciler` (`controllers/beskar7machine_controller.go`) — claims a `PhysicalHost`, drives PXE → kexec.
- `PhysicalHostReconciler` (`controllers/physicalhost_controller.go`) — owns BMC connection lifecycle, observed power state, inspection report.
- `InspectionHandler` (`controllers/inspection_handler.go`) — HTTP server on `:8082` accepting POSTs from the inspection image.
- `Beskar7ClusterWebhook` (`api/v1beta1/webhooks/beskar7cluster_webhook.go`) — only webhook in the codebase.

Two unwired internal packages (decision pending, see Decisions log entry D-001):

- `internal/coordination/provisioning_queue.go` (~580 LOC) — was meant for per-BMC throttling.
- `internal/security/{monitor,rbac_validator,tls_validator}.go` (~1300 LOC) — was meant for runtime security checks.

---

## Punch list (prioritized)

### Release-blocking — must fix before next release

| ID | Area | Issue | Where |
|---|---|---|---|
| BLOCK-2 | CAPI conformance | `Beskar7MachineReconciler` never reads `Machine.Spec.Bootstrap.DataSecretName`. Provisioned hosts can't actually become Kubernetes nodes. | `controllers/beskar7machine_controller.go` (no references to bootstrap anywhere) |
| BLOCK-3 | docs / chart | `charts/beskar7/crds/` ships v0.3 schema; `config/crd/bases/` is the v0.4 source of truth. Helm install reconciles against rejecting CRDs. Stray `charts/beskar7/crds/_.yaml` with empty `kind`. | `charts/beskar7/crds/*.yaml` |
| BLOCK-4 | chart | Helm chart enables webhook plumbing but never passes `--enable-webhook=true` to the manager; also ships no `ValidatingWebhookConfiguration`. | `charts/beskar7/templates/deployment.yaml:66-72`, missing `vwc.yaml` |
| BLOCK-5 | chart / docs | Inspection endpoint port `:8082` is not exposed by the chart's Service or NetworkPolicy. Inspection workflow non-functional on Helm installs. | `charts/beskar7/templates/{service.yaml,networkpolicy.yaml}` |

### Critical security

| ID | Area | Issue | Where |
|---|---|---|---|
| SEC-1 | inspection HTTP | Endpoint accepts unauthenticated POST that flips PhysicalHost to `InspectionComplete`/`Ready`, bypassing `HardwareRequirements` and injecting NICs/IPs into status. No `MaxBytesReader`. Plain HTTP. | `controllers/inspection_handler.go:79-204`, `cmd/manager/main.go:158` |
| SEC-2 | RBAC | Cluster-wide read of all Secrets in all namespaces. | `config/rbac/role.yaml:14-21`, `charts/beskar7/templates/rbac.yaml:16-23` |
| SEC-3 | RBAC generation | kubebuilder markers contain trailing `// comments` that emit literal verbs in generated YAML; RBAC linter skips them silently. | `controllers/beskar7cluster_controller.go:63-64` and similar |
| SEC-5 | TLS | `NewClientWithHTTPClient(...)` accepts an `*http.Client` and silently discards it. Custom CA support is implied by name but does not work. | `internal/redfish/gofish_client.go:83-90` |

### Correctness — high priority

| ID | Area | Issue | Where |
|---|---|---|---|
| BUG-2 | controller | Host claim is list-then-update with no resourceVersion precondition; two Beskar7Machines can claim the same Available host. | `controllers/beskar7machine_controller.go:425-442` |
| BUG-8 | controller | `FailureReason`/`FailureMessage` declared on the API but never set; hardware validation failures requeue forever. | `api/v1beta1/beskar7machine_types.go:96-104`, `controllers/beskar7machine_controller.go:329-363` |
| BUG-9 | controller | `findControlPlaneEndpoint` ignores user-set `Spec.ControlPlaneEndpoint` and hardcodes port 6443. | `controllers/beskar7cluster_controller.go:278-298, 350-353` |

### Documentation drift (release-relevant)

All of these need a doc rewrite when the v0.4 doc sweep happens. Tracked together because they share a root cause.

- `docs/api-reference.md`, `docs/beskar7machine.md`, `docs/beskar7machinetemplate.md`, `docs/physicalhost.md` describe v0.3 API (`imageURL`, `osFamily`, `bootMode`, `provisioningMode`, `configURL`); real fields are `inspectionImageURL`, `targetImageURL`, `configurationURL`, `hardwareRequirements`.
- `docs/state-management.md` uses `Claimed`/`Provisioning`/`Provisioned` states; real states are `Available`/`InUse`/`Inspecting`/`Ready`/`Error` (`api/v1beta1/physicalhost_types.go:18-26`).
- `docs/advanced-usage.md` describes the removed `RemoteConfig`/`PreBakedISO`/`UefiTargetBootSourceOverride` flow.
- `docs/security/*.md` advertise: `caCertificateSecretRef` field (doesn't exist), `beskar7-security-policy` ConfigMap (not shipped/read), `--enable-security-monitoring` flag (not declared), security metrics like `beskar7_tls_certificate_expiry_days` (not registered), CIS/NIST/SOC2 compliance (not audited), compliance-scan CronJob (not shipped).
- `docs/troubleshooting.md:51-75` debugs a removed PhysicalHost mutating webhook; `:443-447` references undeclared `--max-concurrent-reconciles-physicalhost` flag.
- `docs/ipxe-setup.md:305,324,471` says inspection endpoint is on port 8080 — it's 8082; ASCII art is also mangled.
- `docs/quick-start.md:7` requires Go 1.24 (real: 1.25); lines 55, 80 reference image `v0.2.7` (real: `v0.4.0-alpha`).
- `examples/complete-cluster.yaml` and `examples/minimal-test-cluster.yaml` use removed v0.3 fields and will be rejected by the live CRDs. `simple-cluster.yaml` and `minimal-test.yaml` are correct.
- `docs/README.md` links to `quick-start-vendor-support.md` and `vendor-specific-support.md` — neither exists.
- `README.md:144` links to `docs/migration-to-projectbeskar.md` — does not exist.
- `CHANGELOG.md` lists `InspectionInProgressReason` and `InspectionCompleteReason` constants — they don't exist in code. Documents wrong `InspectionReport` shape (flat object vs. real `[]CPUInfo` array).
- `Chart.yaml:21` has trailing garbage (`...Infrastructure**ucture**`).
- `charts/beskar7/values.yaml` has a top-level `image:`/`resources:`/probe block that is unused (chart consumes `controllerManager.*`); `webhook.service.namespace` is hardcoded; `monitoring.enabled` does nothing.

### Medium / hardening

- Pin Dockerfile base images by digest (currently `golang:1.25` and `gcr.io/distroless/static:nonroot` unpinned).
- `cmd/manager/main.go:96` `Development: true` zap default — produces verbose logs in prod. Make it a flag.
- Webhook private key `defaultMode` mismatch: kustomize `0400`, Helm chart `420` (decimal `0644`).
- Webhook hostname validation in `beskar7cluster_webhook.go:192-197` is unreadable; use `net.ParseIP` + standard regex.
- Beskar7MachineTemplate missing kubebuilder markers (path, storageversion, categories) — will not survive `clusterctl move`.
- `metrics.RecordPowerOperation` always records `PowerOperationOn` regardless of input (`internal/metrics/metrics.go:496-498`).
- `physicalhost_types.go:30-33` deprecated state aliases all collide on the same string value.
- `inspection_handler.go:108` discards request context (`ctx := context.Background()`).
- No exponential backoff on PhysicalHost reconcile failures — misconfigured address pings BMC every 60s indefinitely.

### Dead code (decision pending)

- `internal/coordination/provisioning_queue.go` — never imported. Worker stubs `time.Sleep` and return success. See D-001.
- `internal/security/{monitor,rbac_validator,tls_validator}.go` — never instantiated by main. Advertised in `docs/security/` as enforcing controls. See D-001.

---

## In flight

> Add an entry when you start non-trivial work; remove it when the PR merges or the work is abandoned.

| Owner | Branch | Scope | Started | Status |
|---|---|---|---|---|
| _none_ | | | | |

---

## Recently closed

> Move items here from the punch list when fixed. Include PR / commit ref. Trim entries older than ~3 months.

| Item | Resolution | Date |
|---|---|---|
| BUG-1 | PR-2.1 + PR-2.3: direct `r.Status().Update(physicalHost)` calls removed in PR-2.1; replaced with `InspectionRequestAnnotation` signal. Residual latency concern (one-cycle lag + race on annotation-clear) eliminated in PR-2.3 by adding `Watches(&PhysicalHost{}, ...)` to `Beskar7MachineReconciler.SetupWithManager` — PhysicalHost state changes now trigger an immediate Beskar7Machine reconcile. | 2026-05-02 |
| BUG-4 | PR-2.3: `reconcileDelete` now issues `ClearBootSourceOverride` then graceful `SetPowerState(Off)` before clearing `ConsumerRef`. Both Redfish calls are best-effort — errors are logged and swallowed so a dead BMC cannot strand the finalizer. `ForceReleaseAnnotation = "infrastructure.cluster.x-k8s.io/force-release"` added as operator escape hatch (skips Redfish ops entirely). Missing-credentials path treated identically. `ClearBootSourceOverride` added to the `Client` interface, `gofishClient`, and `MockClient`. 4 new `It` blocks in `controllers/beskar7machine_controller_test.go`. | 2026-05-02 |
| BUG-7 | PR-2.3: `Beskar7MachineReconciler.SetupWithManager` now calls `Watches(&PhysicalHost{}, handler.EnqueueRequestsFromMapFunc(r.PhysicalHostToBeskar7Machine))`. The mapping function only enqueues requests where `ConsumerRef.Kind == "Beskar7Machine"` and `ConsumerRef.APIVersion == InfrastructureAPIVersion`, so unrelated consumers produce no spurious reconciles. 4 mapping unit tests added. | 2026-05-02 |
| BLOCK-1 | PR-0.1: defaulted `RedfishClientFactory` to `internalredfish.NewClient` in each reconciler's `SetupWithManager` with a fail-fast non-nil guard; removed misleading `// Use default` from `cmd/manager/main.go`; added unit specs covering nil → default and explicit-factory-preserved for both reconcilers. | 2026-05-01 |
| kube-rbac-proxy removal (Phase 11, PR-11.1) | Deleted `config/default/manager_auth_proxy_patch.yaml` and the kube-rbac-proxy sidecar from the Helm chart; wired `filters.WithAuthenticationAndAuthorization` via `metricsserver.Options.FilterProvider`; metrics now served directly on `:8443` (HTTPS) with TokenReview/SAR-based auth; added `--secure-metrics` flag (default true) for local dev; added `config/rbac/metrics_auth_role.yaml`, `metrics_auth_role_binding.yaml`, `metrics_reader_role.yaml`; updated all overlay patches and networkpolicies to `:8443`. | 2026-05-01 |
| BUG-3 | PR-1.3: Replaced hand-rolled loop in `parseProviderID` with `strings.CutPrefix` + `strings.SplitN`. Now rejects missing prefix with informative message, conflates no-slash / empty-namespace / empty-name with a single clear error, and explicitly rejects multi-segment names (BUG-3 root cause: `b7://ns/name/extra` previously returned `("ns", "name/extra", nil)`). Table-driven `TestParseProviderID` added in `controllers/parse_provider_id_test.go` (9 subtests, race-clean). | 2026-05-02 |
| BUG-5 | PR-2.1: `PhysicalHostReconciler.Reconcile` now uses `patch.NewHelper` deferred at top; `r.Update` (finalizer add/remove) and all `r.Status().Update` calls removed; a single `patchHelper.Patch(ctx, ph, patch.WithStatusObservedGeneration{})` handles both spec and status in one round-trip. `InspectionRequestAnnotation` constant exported for cross-controller use. 2 PIt blocks converted: "Should add finalizer via patch …" and "Should apply inspection-request annotation …". | 2026-05-02 |
| SEC-4 | PR-3.3: Dropped `"username", username` from both INFO log calls in `NewClient` (`gofish_client.go:25, 57-62`); both calls also moved to `V(1)` debug since they fire on every reconcile. `PasswordProvided` bool retained at V(1) for diagnostics. No username appears in any log path. | 2026-05-02 |
| BUG-11 | PR-3.3: Replaced `fmt.Sscanf(mem.Capacity, "%d", &memGB)` with `parseMemoryCapacityGB` helper in `controllers/beskar7machine_controller.go`. Helper uses `resource.ParseQuantity` after stripping trailing `B` from BMC unit strings, plus an explicit suffix allowlist (G/Gi/M/Mi/T/Ti) to reject bare integers and exotic SI prefixes. 15-case table test in `controllers/parse_memory_test.go`. | 2026-05-02 |
| BUG-6 | PR-3.1: `SetPowerState(OffPowerState)` now maps to `GracefulShutdownResetType` instead of `ForceOffResetType`. New `ForcePowerOff` method on the `Client` interface (+ `gofishClient` impl + `MockClient` with `ForcePowerOffCalled` counter and `ShouldFail` support) for callers that need an immediate power-cut. | 2026-05-02 |
| BUG-10 | PR-3.1: Added `defaultHTTPTimeout = 30s` const and `newHTTPClient(insecure bool)` helper; `NewClient` now passes a custom `*http.Client` with that timeout and TLS config into `gofish.ClientConfig.HTTPClient`. Added `doWithCtx` helper that races a synchronous gofish call against ctx cancellation. Applied to all gofish I/O call sites: `getSystemService`, `SetPowerState`, `SetBootSourcePXE`, `Reset`, `GetNetworkAddresses`, `extractAddressesFromNetworkInterface`. `Close` uses a derived 5-second context. 9 new unit tests in `internal/redfish/gofish_client_test.go`. | 2026-05-02 |
| _initial population_ | review baseline established | 2026-05-01 |

---

## Decisions log

> Append-only. One entry per architectural call. Don't rewrite past entries — supersede with a new one.

### D-001 — Dead-code packages: delete

- **Date**: 2026-05-01.
- **Decision**: Delete `internal/coordination/` and `internal/security/` in their entirety. Replace any future per-BMC throttling need with `MaxConcurrentReconciles` tuning + a small per-BMC `sync.Mutex` map at the call site, when measured to be necessary. Replace the security-monitor "controls" with real enforcement at the points they purport to cover (RBAC: a tightened `role.yaml`; TLS: a real CA bundle plumbed through `gofish_client.go`; runtime audit: existing controller-runtime structured logs + Prometheus metrics).
- **Rationale**: Both packages are unreferenced from `cmd/manager/main.go`; the queue's worker bodies are stubs that `time.Sleep` and return success; the security packages are observation, not enforcement. Wiring them correctly requires design we have not done. Keeping them blocks the security-docs rewrite and rewards continued overclaiming. Cost of deletion is low (no callers); cost of keeping is ongoing drift.
- **Implementation**: PR-6.1 in the v0.4 stabilization plan (D-002).
- **Status**: closed.

### D-002 — v0.4 stabilization plan

- **Date**: 2026-05-01.
- **Decision**: Adopt the 12-phase, 22-PR plan produced by `staff-architect` to resolve all findings from the v0.4.0-alpha review. Phases gate as follows: Phase 0 (BLOCK-1) gates everything; Phase 1 (CAPI conformance) and Phase 2 (host lifecycle) run in parallel after Phase 0; Phase 4 (chart parity) gates on Phase 1; Phase 5 (inspection endpoint security) gates on Phases 0+2; Phase 6 (dead-code delete) gates on D-001 (now closed); Phase 7 (RBAC) gates on Phase 6; Phase 9 (docs sweep) gates on Phases 1–7 so docs describe what ships; Phases 3, 8, 11 are independent.
- **Phase summary**:
  | Phase | Theme | Owner | Gating |
  |---|---|---|---|
  | 0 | Stop the panic (BLOCK-1) | golang-engineer | — |
  | 1 | CAPI conformance: bootstrap data, ProviderID, FailureReason | golang-engineer | Phase 0 |
  | 2 | Host lifecycle: patch helper, claim race, release path, watches | golang-engineer | Phase 0 (parallel with 1) |
  | 3 | Redfish hardening: ctx, graceful power, TLS CA, no creds in logs | golang-engineer + security-engineer | independent |
  | 4 | Chart parity: CRD regen, webhook wiring, expose port 8082 | golang-engineer + tech-writer | Phase 1 |
  | 5 | Inspection endpoint security: per-host token, TLS, body cap | security-engineer | Phases 0+2 |
  | 6 | Delete dead code (D-001) | golang-engineer | D-001 ratified |
  | 7 | RBAC tightening: namespace-scoped Secret reads | security-engineer | Phase 6 |
  | 8 | Cluster controller correctness | golang-engineer | independent |
  | 9 | Docs + examples sweep (v0.4 rewrite) | tech-writer | Phases 1–7 |
  | 10 | Test coverage backfill (envtest, drop PIt) | qa-engineer | Phases 1–2 |
  | 11 | Hardening tail: digest pin, zap flag (kube-rbac-proxy done in PR-11.1) | golang-engineer | independent |
- **PR breakdown amendment (2026-05-02, after D-003/D-004/D-005)**:
  | PR | Phase | Title | Owner | Depends on |
  |---|---|---|---|---|
  | PR-1.1 | 1 | Beskar7Machine reads bootstrap secret + populates `PhysicalHost.Status.Bootstrap.URL` on inspection trigger | golang-engineer | PR-0.1 (closed) |
  | PR-1.2 | 1 | Wire `FailureReason` / `FailureMessage` on terminal failures (closes BUG-8) | golang-engineer | PR-1.1 |
  | PR-1.3 | 1 | Fix `parseProviderID` (closes BUG-3) — small, independent | golang-engineer | none |
  | PR-2.1 | 2 | Patch helper + remove cross-controller status writes (BUG-5, BUG-1 partial) | golang-engineer | PR-0.1 → **closed in PR #37** |
  | PR-2.2 | 2 | Atomic claim (BUG-2 full fix) | golang-engineer | PR-2.1 |
  | PR-2.3 | 2 | Clean release on delete + watch PhysicalHost from Beskar7Machine (BUG-4, BUG-7) | golang-engineer | PR-2.1 |
  | PR-3.1 | 3 | Graceful power-off + ctx + per-call HTTP timeout (BUG-6, BUG-10) | golang-engineer | independent |
  | PR-3.2 | 3 | TLS CA bundle support on `RedfishConnection` (SEC-5) | security-engineer | independent |
  | PR-3.3 | 3 | Stop logging BMC creds; fix memory parse (SEC-4, BUG-11) | golang-engineer | independent |
  | PR-5.1 | 5 | CRD: add `PhysicalHostStatus.Bootstrap`; mint+verify token in `internal/auth` | security-engineer | PR-1.1, PR-2.x |
  | PR-5.2 | 5 | Inspection handler: bearer auth, body cap, TLS, annotation handoff (closes SEC-1, BUG-1 fully via D-005) | security-engineer | PR-5.1 |
  | PR-5.3 | 5 | Bootstrap GET endpoint with same auth (closes BLOCK-2 server side) | security-engineer | PR-5.1 |
- **Status**: closed (active execution).

### D-003 — Bootstrap delivery via authenticated manager HTTP endpoint

- **Date**: 2026-05-02.
- **Decision**: `Beskar7MachineReconciler` reads `Machine.Spec.Bootstrap.DataSecretName`, and the manager serves the secret bytes at `GET /api/v1/bootstrap/{ns}/{name}` on the existing inspection port `:8082` over HTTPS, gated by a per-host bearer token in the `Authorization` header. The plaintext token is rendered into the iPXE kernel cmdline; only its SHA-256 is persisted in `PhysicalHost.Status.Bootstrap`. Kernel-cmdline-inline (Option A) and virtual-media / configdrive (Option C) are rejected: A is size-bounded (~2-4 KB) and leaks user-data including kubeadm join tokens into BMC and Redfish audit logs; C requires infrastructure the project explicitly avoids.
- **Rationale**: closes BLOCK-2 without a CRD schema break beyond an additive `PhysicalHostStatus.Bootstrap` sub-object. Reuses the existing `:8082` surface so chart Service / NetworkPolicy parity (BLOCK-5) is one fix instead of two. Token-on-header keeps user-data and join-tokens out of access logs. URL is deterministic from `(namespace, name)` so the reconciler is idempotent across manager restarts. The manager becoming a soft dependency of host bring-up matches the existing inspection-callback dependency — operational topology unchanged.
- **Implementation**: PR-1.1 (controller-side: read secret, populate URL on `PhysicalHost.Status.Bootstrap`), PR-5.3 (server-side: serve the GET endpoint with bearer auth).
- **Status**: closed.

### D-004 — Inspection / bootstrap auth: per-host bearer token, hashed in status

- **Date**: 2026-05-02.
- **Decision**: At the start of `triggerInspection`, the manager mints a 32-byte random token per `PhysicalHost`. It stores `sha256(token)` plus `IssuedAt` and `ExpiresAt = IssuedAt + 30m` in `PhysicalHost.Status.Bootstrap.{TokenHash, IssuedAt, ExpiresAt}`. The plaintext token is rendered into the iPXE kernel cmdline (`beskar7.token=<plaintext>`). Both the inspection POST (`POST /api/v1/inspection`) and the bootstrap GET (`GET /api/v1/bootstrap/{ns}/{name}`) authenticate via `Authorization: Bearer <token>`. Constant-time compare via `crypto/subtle`. Multi-use within window; revoked when `MachineProvisionedCondition=True` or on Beskar7Machine deletion.
- **Alternatives rejected**: URL-path token (leaks into HTTP access logs and reverse-proxy logs); annotation storage (the hash belongs in observed-state on the Status subresource so only the controller's status writer can mutate it); custom `X-Beskar7-Inspection-Token` header (standard `Bearer` works with existing log scrubbers and proxies); one-shot tokens (fights the multi-fetch reality of inspector POST + bootstrap GET + cloud-init re-fetch).
- **Rationale**: closes SEC-1. Hash-in-Status preserves the controller-only status-write boundary. `Bearer` over `Authorization` is the path of least surprise for proxies and log-scrubbers. 30-minute window is comfortably above `DefaultInspectionTimeout` (10 min) and gives operators headroom for slow BIOS POST + first-boot inspector + bootstrap fetch.
- **Implementation**: PR-5.1 (CRD addition + `internal/auth` mint/verify), PR-5.2 (handler bearer-auth wiring + body cap + TLS), PR-5.3 (bootstrap GET endpoint reuses the same auth).
- **Status**: closed.

### D-005 — Inspection handler must not write `PhysicalHost.Status` directly

- **Date**: 2026-05-02. Surfaced during D-004 design.
- **Decision**: The inspection HTTP handler stores the validated `InspectionReport` on a ConfigMap referenced by an annotation `infrastructure.cluster.x-k8s.io/inspection-result-ref` on `PhysicalHost`. The `PhysicalHostReconciler` watches the annotation, reads the referenced ConfigMap, and is the sole writer of `PhysicalHost.Status.InspectionReport` and `Status.InspectionPhase`.
- **Rationale**: today `controllers/inspection_handler.go:199` calls `h.Client.Status().Update(ctx, physicalHost)` — same boundary violation as the cross-controller writes that PR-2.1 just removed (BUG-1). Closes the remaining BUG-1 surface and is required by the boundary rule in `CLAUDE.md`. ConfigMap is preferred over a spec field because the inspection report is large (`[]CPUInfo`, `[]MemoryInfo`, `[]NIC`, `[]Disk`) and shouldn't bloat the PhysicalHost object.
- **Implementation**: PR-5.2.
- **Status**: closed.

---

## Glossary

| Term | Meaning |
|---|---|
| **PhysicalHost** | CRD representing a real bare-metal box and its BMC connection. Lifecycle: `Available` → `Inspecting` → `InUse` → (back to `Available` on release) or `Error`. |
| **Beskar7Machine** | CAPI infra-machine; one per Kubernetes node. Owns the claim on a `PhysicalHost`. |
| **Beskar7MachineTemplate** | Template referenced by KubeadmControlPlane / MachineDeployment to mint Beskar7Machines. |
| **Beskar7Cluster** | CAPI infra-cluster; tracks `ControlPlaneEndpoint` + failure domains. |
| **Inspection image** | `beskar7-inspector` (separate repo). PXE-booted; collects hardware details and POSTs them back to the manager's `:8082`. |
| **Target image** | The actual OS image (e.g., Kairos) that the host kexecs into after inspection passes hardware validation. |
| **ConsumerRef** | `PhysicalHost.Spec.ConsumerRef` — the Beskar7Machine currently claiming the host. Acts as the lock. |
| **InspectionPhase** | `physicalhost_types.go` typed string: `Pending`/`InProgress`/`Complete`/`Failed`. Note duplicate constant sets — see BUG list. |
| **iPXE chainload** | The DHCP+HTTP infrastructure (operator-provided) that boots the inspection image and the target image. Setup is in `docs/ipxe-setup.md`. |

## External references

- CAPI provider contract: https://cluster-api.sigs.k8s.io/developer/providers/contracts/overview
- gofish (Redfish client): https://github.com/stmcginnis/gofish (`v0.20.0`)
- controller-runtime: `v0.20.4`
- cluster-api: `v1.10.1`
- Inspection image repo: `https://github.com/projectbeskar/beskar7-inspector`

---

## How to update this file

1. **When you fix something**: move from punch list → Recently closed; include PR/commit ref and date.
2. **When you discover something**: add to punch list under the right severity; cite file:line.
3. **When you make an arch call**: append a `D-NNN` entry to Decisions; never edit past entries.
4. **When the in-flight list goes stale**: prune any branch that's been idle > 2 weeks.
5. **Keep entries concise.** One row per item. If it needs paragraphs, link out to an issue or design doc.
6. **Update the "Last meaningful update" date at the top** when you make non-trivial edits.
