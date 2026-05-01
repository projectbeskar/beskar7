# CLAUDE.md

Project-level guidance for Claude Code working in the **Beskar7** repository.

This file is read at the start of every Claude session. Keep it tight, factual, and current. Anything that changes more than once a quarter belongs in `.claude/context/PROJECT_CONTEXT.md`, not here.

---

## Project at a glance

- **What**: A Cluster API infrastructure provider for bare-metal hosts.
- **How it provisions**: Redfish for power/boot control + iPXE network boot + an inspection image (`beskar7-inspector`) that posts hardware details back to the controller.
- **Module**: `github.com/projectbeskar/beskar7`
- **Go version**: `1.25.0` (see `go.mod`; CI uses `go-version: '1.25'`)
- **Layout**: `go.kubebuilder.io/v4` (kubebuilder v4 conventions)
- **API group / version**: `infrastructure.cluster.x-k8s.io / v1beta1`
- **Status**: `v0.4.0-alpha` — a clean break from `v0.3.x`. Not API-stable.

The four CRDs:

| Kind | Purpose |
|---|---|
| `PhysicalHost` | Inventory record for a bare-metal box; owns BMC connection, observed power state, inspection report. |
| `Beskar7Machine` | CAPI infra-machine; claims a `PhysicalHost`, drives PXE boot + kexec to target image. |
| `Beskar7MachineTemplate` | Template for `Beskar7Machine` consumed by KCP / MachineDeployments. |
| `Beskar7Cluster` | CAPI infra-cluster; tracks control-plane endpoint and failure domains. |

---

## Code layout

```
api/v1beta1/                CRD type definitions + deepcopy
api/v1beta1/webhooks/       Admission webhooks (only Beskar7Cluster has one today)
cmd/manager/main.go         Manager entrypoint; flag parsing; manager wiring
controllers/                Reconcilers + the inspection HTTP handler
internal/redfish/           gofish client wrapper + interface + mock
internal/coordination/      provisioning_queue.go (currently DEAD CODE — not wired)
internal/security/          monitor + RBAC validator + TLS validator (DEAD CODE — not wired)
internal/metrics/           Prometheus metric registrations
config/                     kustomize manifests (CRDs, RBAC, manager, webhook, security)
charts/beskar7/             Helm chart (note: charts/beskar7/crds/ is currently STALE)
docs/                       User docs (significant v0.3-vs-v0.4 drift — see PROJECT_CONTEXT.md)
examples/                   Sample CRs (simple-cluster.yaml + minimal-test.yaml are correct;
                            complete-cluster.yaml + minimal-test-cluster.yaml use REMOVED fields)
test/emulation/             Mock Redfish server (build-tagged `integration`)
test/integration/           CAPI integration tests (build-tagged `integration`)
hack/                       Boilerplate header + envtest setup
```

For deep architecture and the in-flight punch list, read `.claude/context/PROJECT_CONTEXT.md`.

---

## Build, test, lint

Use `make`. Do not invent new commands.

```bash
make build               # build manager into bin/manager
make manifests           # regenerate CRDs, RBAC, deepcopy (calls controller-gen)
make test                # go test ./... -coverprofile cover.out
make docker-build        # buildx for linux/amd64
make release-manifests   # produce single-file manifest for a release
```

CI runs (`.github/workflows/ci.yml`):

- **Lint**: `golangci-lint v1.64.8 run --verbose --timeout=5m`
- **Unit tests**: `go test -v -race -coverprofile=coverage.out -covermode=atomic $(go list ./... | grep -v /test/integration)` with `KUBEBUILDER_ASSETS` set via `setup-envtest use 1.31.x`
- **Integration**: `go test -v -tags=integration ./test/integration/... -timeout=30m`

Before declaring code "done":

```bash
make manifests           # regenerate if you touched api/ or kubebuilder markers
make test
golangci-lint run --timeout=5m
```

If you change anything under `api/v1beta1/` or any `+kubebuilder:` marker, you **must** run `make manifests` and commit the regenerated `config/crd/bases/*.yaml`, `config/rbac/role.yaml`, and `api/v1beta1/zz_generated.deepcopy.go`.

If you regenerate CRDs, also regenerate the chart-bundled copies under `charts/beskar7/crds/` — they are currently stale and that drift is a release-blocker (tracked in `PROJECT_CONTEXT.md`).

---

## Coding conventions

- **Package boundaries**: controllers own their resource's status; do not write to another resource's status from a different reconciler. Use annotations or `consumerRef` for cross-controller signaling.
- **Patches over Updates**: prefer `patch.NewHelper` (controller-runtime) deferred at the top of `Reconcile`. The Beskar7Cluster controller is the reference for this pattern. Avoid `r.Update(ctx, obj)` and `r.Status().Update(ctx, obj)` in the same reconcile when both can be a single deferred patch.
- **Context**: every function that performs I/O takes `ctx context.Context` as the first arg and propagates it. Never use `context.Background()` in a handler that already has a request context.
- **Errors**: wrap with `fmt.Errorf("%w", err)` or `pkg/errors`. Don't return errors after already setting a terminal `FailureReason` — return nil and let CAPI surface the failure.
- **Logging**: use the request-scoped `logr.Logger` from controller-runtime. Never log credentials, BMC passwords, full URIs with userinfo, or Secret data. Username + address logged together is also avoided. Default verbosity is `Info`; use `V(1)` for chatty per-call debug.
- **Conditions**: follow CAPI conventions (`clusterv1.Conditions`). Set `Ready` per resource. Use existing constants in `api/v1beta1/*_types.go` rather than string literals.
- **Status idempotency**: status writes must be safe to repeat. The reconcile loop will run again; do not assume a single pass.
- **No comments on the obvious**. Comments explain *why* (a hidden constraint, a workaround). They never explain *what* well-named code already says.
- **No dead code**. If you remove a feature, remove the helpers, the constants, the metric, the doc page, the example. Half-removed features are worse than full ones.

### Lint

`.golangci.yml` enables: `errcheck`, `gosimple`, `govet`, `ineffassign`, `staticcheck`, `typecheck`, `unused`. Don't add `// nolint` without a one-line reason.

---

## Cluster API conformance — non-negotiable

Beskar7 is a CAPI infrastructure provider. The provider contract requires:

1. **Pause handling**: respect both `Cluster.Spec.Paused` and the `cluster.x-k8s.io/paused` annotation on the resource. There is a helper in `controllers/utils.go`.
2. **Bootstrap data**: `Beskar7MachineReconciler` must read `Machine.Spec.Bootstrap.DataSecretName`, fetch the Secret, and inject the user-data into the boot flow (kernel cmdline / ignition / cloud-init). **This is currently missing — see PROJECT_CONTEXT.md.**
3. **Owner refs**: `Beskar7Machine` must have an OwnerReference to its `Machine`. `Beskar7Cluster` must have one to its `Cluster`.
4. **`cluster.x-k8s.io/cluster-name` label**: set on every owned resource so `clusterctl move` works.
5. **Ready / Provisioned**: set `Status.Ready = true` and `MachineProvisionedCondition` only after the bootstrap data was applied and the host is up.
6. **`FailureReason` / `FailureMessage`**: set these on terminal failures so users see them in `kubectl describe machine`. Don't requeue forever on unrecoverable errors.
7. **`ProviderID`**: set on `Beskar7Machine.Spec.ProviderID` once the host is claimed. Format: `b7://<namespace>/<name>` (parser uses `strings.SplitN`, see `parseProviderID`).
8. **Templates**: `Beskar7MachineTemplate` must carry the `cluster.x-k8s.io/v1beta1: v1_beta1` label and survive `clusterctl move`.

When in doubt, read the [CAPI provider contract](https://cluster-api.sigs.k8s.io/developer/providers/contracts/overview) and the Metal³ / OpenStack providers for reference.

---

## Security guardrails

- **Never log secrets**. BMC username + password + URI are PII-adjacent; log address + a boolean `passwordProvided` only at debug level if at all.
- **TLS to BMCs**: never silently default `InsecureSkipVerify=true`. Add a `caBundleSecretRef` if you need self-signed support.
- **Inspection endpoint** (`controllers/inspection_handler.go`): currently unauthenticated — do not extend it without first adding a per-host token. Treat as a known issue, not a pattern to copy.
- **RBAC**: prefer per-namespace `Role`/`RoleBinding` over cluster-wide `ClusterRoleBinding`. The Secret-read scope must be namespaced.
- **Webhook `failurePolicy`**: `Fail` is correct, but only ship a webhook configuration when the manager actually serves the path. Listing a webhook path in `config/webhook/manifests.yaml` that has no Go handler will block all admission for that resource.
- **Container image**: distroless-nonroot, CGO off, multi-stage. Don't regress this. Pin base images by digest where possible.
- **Pod security**: keep `runAsNonRoot`, `readOnlyRootFilesystem`, drop `ALL` caps, seccomp `RuntimeDefault`. Match the kustomize `config/manager/manager.yaml` settings in the Helm chart.

---

## Testing expectations

- **Every controller change** needs an envtest-level test in `controllers/*_test.go`. The current suite has many `PIt` (pending) — adding new `PIt` is not acceptable; convert to real `It` or don't add the case.
- **Race detector**: `-race` runs in CI; tests must be race-clean.
- **Mock Redfish**: use `internal/redfish/mock_client.go` for unit tests. For integration, `test/emulation/mock_redfish_server.go` exists but is gated behind `-tags integration`.
- **Coverage**: not enforced today, but new code should land with tests for the happy path, the claim/release path, and at least one error path.
- **Webhook tests**: live in `api/v1beta1/webhooks/*_test.go`. New webhooks need both validation and defaulting tests.

---

## Working agreements

1. **Read `.claude/context/PROJECT_CONTEXT.md` before starting any non-trivial change.** It tracks known issues, in-flight work, and the v0.3→v0.4 drift surface.
2. **Update `PROJECT_CONTEXT.md`** when you finish work that closes one of its tracked items, or when you discover a new issue worth recording.
3. **One concern per PR.** Don't bundle a security fix with a refactor with a doc update. Reviewers will not be able to bisect later.
4. **Don't commit unless asked.** Show the diff, run the checks, then wait for explicit "commit" / "push" / "open PR" from the user.
5. **Use the right agent.** This repo has specialized agents under `.claude/agents/`. For a Go controller change, dispatch `golang-engineer`. For a security review, `security-engineer`. For docs, `tech-writer`. For multi-area design or trade-off decisions, `staff-architect`. For test design or coverage, `qa-engineer`. The agent definitions list when each is appropriate.
6. **Don't fabricate field names or behaviors.** When uncertain, read `api/v1beta1/*_types.go` and the relevant controller — do not infer from the docs (which are partially stale).

---

## Quick file map for common tasks

| Task | Start here |
|---|---|
| Add a CRD field | `api/v1beta1/<kind>_types.go` → `make manifests` → wire in controller → update webhook validation → update example + doc |
| Change reconcile logic | `controllers/<kind>_controller.go` + matching `*_test.go` |
| Add Redfish capability | `internal/redfish/client.go` (interface) + `gofish_client.go` (impl) + `mock_client.go` (test double) |
| Add a metric | `internal/metrics/metrics.go` (register) + call site + `docs/metrics.md` (document) |
| Tighten RBAC | edit `+kubebuilder:rbac:` markers on the controller → `make manifests` → diff `config/rbac/role.yaml` → update Helm chart `templates/rbac.yaml` |
| Touch the chart | edit under `charts/beskar7/templates/` and `values.yaml` → run `helm template` to verify → keep CRDs in `charts/beskar7/crds/` in sync with `config/crd/bases/` |

---

## Don't

- Don't add features to `internal/coordination/provisioning_queue.go` or `internal/security/*.go` until they are wired into the manager. They are currently dead code; extending dead code makes it harder to remove.
- Don't ship documentation for unimplemented behavior. The current security docs already overclaim — don't add to that.
- Don't introduce a second source of truth for CRD schemas. `config/crd/bases/` is generated; the chart copy must be derived from it.
- Don't paper over a CAPI conformance gap with a workaround. Fix the gap.
