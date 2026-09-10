# Changelog

All notable changes to this project will be documented in this file.

The format is based on Keep a Changelog, and this project adheres to Semantic Versioning.

## [Unreleased]

### Added

- **`Paused` condition on `Beskar7Machine` and `Beskar7Cluster`.** Maintained by
  `sigs.k8s.io/cluster-api/util/paused`, reason `Paused` or `NotPaused`, alongside the existing
  `Ready` summary condition. `PhysicalHost` is unaffected — it is not a CAPI contract resource and
  has no owning `Cluster`.

### Changed

- **BREAKING: `status.conditions` on `Beskar7Machine`, `Beskar7Cluster` and `PhysicalHost` are now
  native `[]metav1.Condition`** (`type`, `status`, `reason`, `message`, `lastTransitionTime`,
  `observedGeneration`) instead of the CAPI v1beta1-shaped `clusterv1.Conditions`. There is no
  `severity` field, and every condition — including `True` ones — now carries a `reason`
  (`Provisioned`, `PhysicalHostAssociated`, `BootstrapDataReady`, `ControlPlaneEndpointSet`,
  `RedfishConnected`, `HostAvailable`, `HostInspected` are new True-state reason constants).
  `Beskar7Machine.GetV1Beta1Conditions`/`SetV1Beta1Conditions` and the matching methods on
  `Beskar7Cluster`/`PhysicalHost` (the CAPI deprecated-conditions compatibility shim) are removed.
  See `docs/upgrading.md`.
- **BREAKING: `Beskar7Machine`/`Beskar7Cluster` reconciliation now honours `Cluster.spec.paused`**
  (what `clusterctl move` sets), in addition to the `cluster.x-k8s.io/paused` annotation on the
  `Beskar7Machine`/`Beskar7Cluster` object itself — closing the CAPI conformance gap
  `docs/installation.md` previously carried as a known limitation. The replacement
  (`sigs.k8s.io/cluster-api/util/paused.EnsurePausedCondition`) no longer checks the
  `cluster.x-k8s.io/paused` annotation on the *owning* `Cluster` object the way the old
  `isClusterPaused` helper did; pause via `Cluster.spec.paused` or the annotation on the beskar7
  object itself. `PhysicalHost` is unchanged (annotation-only; it has no owning `Cluster`).
- **`Beskar7Machine.spec.providerID` is `string`, not `*string`.** No JSON/YAML change — the field
  was already `omitempty` and the `b7://<namespace>/<name>` format is unchanged. Affects Go
  importers of `api/v1beta2` only.

- **CI: vulnerability scanning moved from Trivy to OSV-Scanner.** Pull requests are diffed against
  their base branch and fail only on vulnerabilities they introduce; pushes to `main`, a weekly
  scheduled run and the release image scan feed the Security tab. The release asset is now
  `osv-scanner-report.txt` (was `trivy-report.txt`).

- **Dependency and toolchain bumps clearing the OSV-Scanner baseline.** All indirect:
  `google.golang.org/grpc` 1.80.0 → 1.83.2 (GO-2026-6061, GHSA-2v4p-qf9q-27wj,
  GHSA-qc2q-p7wx-3px3, GHSA-vp52-pcj8-j9qc), `golang.org/x/text` 0.38.0 → 0.41.0 (GO-2026-5970),
  `github.com/google/cel-go` 0.26.0 → 0.30.0 (GO-2026-6094) and `golang.org/x/mod` 0.36.0 → 0.40.0
  (GO-2026-6179, GO-2026-6180). The `golang:1.25` builder image is re-pinned to the current digest
  (go1.25.14), clearing the Go standard-library advisories the image scan reported against the
  go1.25.9 it previously carried, and `go.mod` gains `toolchain go1.25.14` so a local build cannot
  silently use an older 1.25.x than the image does.
  `x/text` is held at 0.41.0 and `x/mod` at 0.40.0 rather than latest because the next release of
  each requires `go 1.26`, which the `golang:1.25` pin and CI's `go-version: '1.25'` do not
  provide. `cel-go` is bumped for the image scan's benefit: the source scan already suppressed
  GO-2026-6094 because its call analysis finds the vulnerable `cel-go/ext` symbol unreachable from
  this codebase, but image scans match on module version and have no such analysis; that carries
  `cel-go` ahead of the 0.26.0 `k8s.io/apiserver` v0.35.4 pins.
  `sigs.k8s.io/cluster-api` v1.13.4 and `sigs.k8s.io/controller-runtime` v0.23.3 are unchanged.

### Fixed

- **A BMC that is briefly unreachable no longer strands its `PhysicalHost` in `Error` for minutes.**
  A network-level Redfish failure (connection refused or reset, no route, DNS, a dial or request
  timeout, or a 502/503/504 from a BMC that is still starting) now returns
  `RequeueAfter: 15s` with no error, so the flat interval governs the retry. Returning the error
  handed the retry to the workqueue's exponential rate-limiter, but the same failed reconcile also
  wrote the raw error text to `status.errorMessage` — and that write is a watch event on the host,
  which controller-runtime's priority queue puts ahead of the rate-limited retry. Each of those
  event-driven attempts failed again and doubled the backoff, so a BMC that refused connections for
  one second produced ~20 reconciles inside that second and then left the host in `Error` for
  minutes after it was reachable again. The message is now a stable summary of the failure class
  rather than the raw error (whose text names whichever URL failed and so differed between
  attempts), which is what makes a repeated failure a no-op write instead of its own trigger.
  Failures that need something to change before a retry can succeed — a malformed address, a
  rejected certificate, refused credentials, a Redfish tree with no `ComputerSystem` — still
  return the error and keep the exponential backoff. `status.state` is `Error` throughout, as
  before.
- **`hack/smoke/run.sh` no longer creates a `PhysicalHost` against a mock BMC that cannot yet
  answer.** Layer 3 applied the mock manifest, then patched the image with `kubectl set image`,
  which starts a second rollout; `kubectl rollout status` returns as soon as the new pod is Ready,
  which is before the `EndpointSlice` is programmed and while the `ClusterIP` can still route to
  the pod that is going away. The derived image is now substituted into the manifest before the
  first apply, and both layer 3 and layer 7 wait for a ready endpoint and a successful Redfish
  request through the Service before creating the host that points at it. Layer 3 also asserts
  `status.state == Available` alongside `status.ready == true`, reading both from one `Get`: the
  run that prompted this printed `[PASS] [layer 3] PhysicalHost Ready=true, state=Error`.
  The second mock BMC moved to `hack/smoke/manifests/60-mock-redfish-b.yaml` (from `60-pool.yaml`,
  now `61-pool.yaml`; `61-pool-inspectors.yaml` is now `62-pool-inspectors.yaml`) so it can be
  rolled out and probed before the pool fixture creates `pool-host-b`, and it has the same
  readiness probe as the layer-3 mock.
- **`PhysicalHost`'s `HostAvailable` condition now reads `False` while the host is claimed.** It
  was set `True` on the transition into `Available` and never flipped back, so a host that was
  `InUse`, `Inspecting`, `Deploying` or `Ready` still advertised `HostAvailable=True`. The
  controller now asserts it on every reconcile: `False` with the new reason `HostClaimed` while
  `spec.consumerRef` is set, `True` with reason `HostAvailable` otherwise. Host selection never
  read the condition (it filters on `status.state`), so this changes what `kubectl describe`
  and `kubectl wait --for=condition=HostAvailable` report, nothing else.
- **The two published mock images no longer build on a stale Go.** `Dockerfile.mock-redfish` and
  `Dockerfile.mock-inspector` still pinned the May `golang:1.25` digest after the manager's was
  refreshed, so the release workflow would have published `mock-redfish` and `mock-inspector`
  carrying the go1.25.9 standard-library advisories while the manager image was clean. All three
  Dockerfiles now pin the same go1.25.14 builder, which is what their "same pins as the main
  Dockerfile" comment already claimed. Neither image is scanned by CI, which is why this did not
  show up in the baseline.

### Removed

- **BREAKING: `Beskar7Machine.status.failureReason` and `status.failureMessage` are gone.** A
  terminal failure is now `status.phase: Failed` (unchanged marker) plus the `InfrastructureReady`
  condition `False` with the same reason strings as before, which Cluster API mirrors into the
  owning `Machine`'s own `InfrastructureReady` condition. `MachineHealthCheck` on Cluster API
  v1.11+ never read `failureReason`/`failureMessage` — remediating a beskar7-failed machine now
  requires an explicit `spec.checks.unhealthyMachineConditions` entry keyed on `InfrastructureReady`;
  see the rewritten `examples/machinehealthcheck.yaml` and `docs/upgrading.md`.

## [v0.5.0] - 2026-09-10

A clean break from the `v0.4.x` line, with no in-place upgrade path — read
`docs/upgrading.md` before touching an existing install. The API is
`infrastructure.cluster.x-k8s.io/v1beta2`, the only served version (the
`v1beta1` schema renamed, no conversion webhook); the controller is built
against Cluster API v1.13 and needs a management cluster on Cluster API
v1.11 or newer; the install namespace is `capb7-system` and every component
is named `capb7-…`; every release now ships the clusterctl provider assets,
so `clusterctl init --infrastructure beskar7` works, and `clusterctl move`
discovers beskar7 objects. Behind the scenes: the callback-only manager mode
for management clusters off the provisioning network, a bootstrap-credential
reuse check backed by the per-host Secret, hosts turning `Available` waking
the machines waiting for one, and the credential promotion window that
re-minted tokens under an inspector's feet closed.

### Added

- **Installable with `clusterctl init`.** Every release now publishes the two assets
  the [clusterctl provider contract](https://cluster-api.sigs.k8s.io/developer/providers/contracts/clusterctl)
  asks for: `infrastructure-components.yaml` (the kustomize overlay, with the
  clusterctl variable `${BESKAR7_BOOTSTRAP_URL_BASE:=…}` for `--bootstrap-url-base`)
  and `metadata.yaml` (release series → contract, pinned to the CRD contract label by
  `test/contract`). `beskar7-manifests-<version>.yaml` stays as the plain-kubectl
  manifest with the variables resolved. Until beskar7 is in clusterctl's built-in
  list, declare it under `providers:` in the clusterctl config; `docs/installation.md`
  has the snippet. `make clusterctl-override` publishes the current tree into a
  clusterctl local repository, and a new CI job installs beskar7 that way on every
  pull request and runs the smoke suite against it.

### Fixed

- **The kustomize / release-manifest install could not provision a host.** It had
  no Service for the callback server (the manager's default `--bootstrap-url-base`
  named a `…-controller-manager` Service only the Helm chart created), the serving certificate did not cover that name, and the NetworkPolicy
  allowed neither `:8082` nor the metrics port the manager actually binds (`:8443`,
  not `:8080`). All four are fixed; the Deployment is now named
  `capb7-controller-manager` like the chart's (delete the old `controller-manager`
  Deployment after re-applying a manifest install, see `docs/upgrading.md`), and its
  image pull policy is `IfNotPresent` (the tag is pinned per release).
- **A bootstrap token or boot nonce is no longer re-minted while the host controller
  is promoting it.** `applyBootstrapTokenAnnotation` / `applyBootNonceAnnotation`
  copied the freshly minted hash into `Status.Bootstrap` and cleared the annotation
  in the same pass, but the deferred patch writes metadata before status, so for
  a moment the host advertised no credential at all. A `Beskar7Machine` reconcile
  landing in that gap (the host is still `InUse`, so `triggerInspection` runs again)
  found no unexpired hash, minted a new token, and the inspector — which had
  already read the first plaintext from the Secret — got 401 on its callbacks
  (seen in the E2E smoke on `pool-host-b`; the same mechanism as the dome-lab
  strandings). The annotation now outlives the status write by one pass and is
  cleared only once status carries the same hash, so no published version of the
  host is without its credential; a failed status patch no longer loses the mint
  either. The CI failure diagnostics keep 2000 manager log lines instead of 500 —
  the first mint had scrolled out of the dump.
- **`clusterctl move` now discovers beskar7 objects on Helm- and manifest-installed
  management clusters.** clusterctl builds its move graph only from CRDs that
  carry the `clusterctl.cluster.x-k8s.io` label; `clusterctl init` adds it and
  nothing else did, so every `Beskar7Cluster`, `Beskar7Machine`,
  `Beskar7MachineTemplate` and `PhysicalHost` was silently left behind. The four
  CRDs now carry that label from their controller-gen markers, so
  `config/crd/bases` and the chart's `crds/` stay byte-identical, plus
  `cluster.x-k8s.io/provider: infrastructure-beskar7` — the provider-contract
  component label with the value `clusterctl init` would derive. `PhysicalHost`
  also carries `clusterctl.cluster.x-k8s.io/move-hierarchy`: nothing owns a
  host, so without it a move would discover hosts and leave them all behind. The
  chart and the kustomize overlay label every other component the same way
  (`cluster.x-k8s.io/provider` was `beskar7`; metadata only, never in a
  selector) and the overlay's meaningless `cluster.x-k8s.io/contract` CRD label
  is gone. Existing installs must re-apply or re-label their CRDs — see
  `docs/upgrading.md`; `docs/installation.md` lists what a move still needs from
  the operator. A `test/contract` test pins the labels and the byte identity of
  the two CRD directories.
- **A `PhysicalHost` turning `Available` now wakes the `Beskar7Machine`s still
  waiting for a host.** A machine that reconciled moments before its host
  finished enrolling (or before another machine released it) parked on the
  one-minute no-host requeue with nothing to end the wait early; with
  controller-runtime 0.23's priority queue that minute was also no longer
  shortened by the finalizer-add retry. A second watch on `PhysicalHost`,
  admitted only on the transition into a claimable state, re-enqueues the
  namespace's unassociated machines. The one-minute requeue stays as a
  backstop. This is also what made the integration suite's "Delete and
  release" specs flake.
- **kustomize and single-file-manifest installs could not create a
  `Beskar7Machine` or `Beskar7MachineTemplate`.** `config/webhook/manifests.yaml`
  still declared mutating and validating webhooks for both kinds
  (`failurePolicy: Fail`) although the manager only ever served the
  `Beskar7Cluster` pair, so every create/update was rejected with a 404 from
  the webhook server (d0d418f removed the same kind of dead entry for
  `PhysicalHost` but left these four). The Helm chart was never affected — it
  only declares the `Beskar7Cluster` webhooks, which is why the Helm-based E2E
  never caught it. The file is now generated: `make manifests` runs
  controller-gen's `webhook` generator from the `+kubebuilder:webhook` markers
  (new `+kubebuilder:webhookconfiguration` markers carry the `beskar7-` names
  and the Service reference the overlay uses instead of a `namePrefix`), the
  redundant `certmanager_in_webhooks.yaml` patch is gone (it targeted the
  removed webhook names and would have re-added them as incomplete entries),
  and `test/contract` fails if either the overlay or the chart declares a path
  with no marker behind it — or leaves a marker undeclared.

### Changed

- **BREAKING: the install namespace is `capb7-system` and every component is named
  `capb7-…`**, the `cap<provider>` convention Cluster API providers follow
  (`capi-system`, `capd-controller-manager`, …). kustomize overlay, release manifest,
  clusterctl components and the Helm chart (default `fullnameOverride: capb7`) all
  produce the same names now: Deployment and callback Service
  `capb7-controller-manager`, ServiceAccount `capb7-manager`, ClusterRole
  `capb7-manager-role`, `capb7-webhook-service`, `capb7-serving-cert`,
  `capb7-selfsigned-issuer`, `capb7-{mutating,validating}-webhook-configuration`, and
  the manager's default `--bootstrap-url-base` is
  `https://capb7-controller-manager.capb7-system.svc:8082`. The chart derives that
  default from the release name and namespace instead of hardcoding it, so a
  differently named release no longer needs `bootstrap.urlBase` just to point at its
  own Service. Existing installs are removed and re-installed, not upgraded in place —
  `docs/upgrading.md`. The size overlays under `config/overlays/` patch the Deployment
  by name and had silently stopped building when it was renamed; CI builds them now.
- **BREAKING: the API is now `infrastructure.cluster.x-k8s.io/v1beta2`, the only
  served version.** `api/v1beta1` was renamed to `api/v1beta2` in place — no
  field changed — with **no conversion webhook** (there are no `v1beta1` users to
  migrate; D-018's "v1beta2 with conversion" is superseded). `v1beta1` CRDs and
  objects must be deleted and re-created; `docs/upgrading.md` has the procedure.
  The CRD contract label becomes `cluster.x-k8s.io/v1beta2: v1beta2` on the three
  CAPI contract resources (`PhysicalHost` is not one and now carries no contract
  label) — on CAPI v1.11+ that label is what resolves the apiVersion behind a
  version-less `infrastructureRef`, and the v1beta2 contract reads the
  list-shaped `failureDomains` this API already publishes. Webhook paths move to
  `…-v1beta2-beskar7cluster`; the `PhysicalHost.spec.consumerRef` the controller
  writes carries the new apiVersion; the Go alias is `infrav1`. Requires
  Cluster API v1.11 or newer (already true since the module bump below; the
  docs now say so).
- **Built against Cluster API v1.13 and its `api/core/v1beta2` Go API** (was
  v1.10.1 / `api/v1beta1`, which no longer exists in v1.13). This is the
  mechanical half of D-023: no behaviour change and no beskar7 API change. The
  v1beta1-shaped conditions stay for now through CAPI's deprecated helpers
  (accessors `GetV1Beta1Conditions`/`SetV1Beta1Conditions` on all three types —
  required, or `patch.Helper` silently drops conditions); `Machine.spec.failureDomain`
  is a plain string; `Beskar7Cluster.status.failureDomains` is built as a sorted
  list (CAPI v1beta2 shape) but still serialised under the v1beta1 API.
  controller-runtime 0.23: the webhook builder is generic and
  `ctrl.Result{Requeue: true}` is deprecated — replaced by a one-second
  `RequeueAfter` at the six sites (finalizer add, optimistic-lock conflict,
  post-inspection re-observe). `controlPlaneEndpoint` gains `omitzero` so an
  endpoint-less `Beskar7Cluster` (legitimate until the control plane has an
  address) is not rejected by v1beta2's `APIEndpoint` schema. envtest now uses
  the real CAPI v1beta2 CRDs copied from the module (`make test-external-crds`)
  instead of hand-written v1beta1 stubs.

## [v0.4.4] - 2026-09-09

Patch release on the GA line: the CAPI contract-label fix below, plus everything merged
since `v0.4.3` — `hostSelector` (#158), the claim honouring `Machine.spec.failureDomain`
(#157), and the k0s image-side start gate and image-build guide (#156). The only schema
change is the additive `hostSelector` field; see `docs/upgrading.md` for the one manual
step (CRD label) and the CRD re-apply.

### Fixed

- **CAPI ≥ v1.11 could not read `Beskar7Cluster.status.failureDomains`, and a
  zone-labelled host stalled the Cluster.** The four CRDs carried both
  `cluster.x-k8s.io/v1beta1=v1beta1` and `cluster.x-k8s.io/v1beta2=v1beta1`
  contract labels. CAPI resolves the newest labelled contract, so on
  v1.11+ it read our status through the **v1beta2** contract — which models
  `failureDomains` as a list — while beskar7 still publishes the v1beta1 map.
  The read fails hard (`failed to retrieve status.failureDomains from
  infrastructure provider`), and `reconcileInfrastructure` aborts before
  `Cluster.status.initialization.infrastructureProvisioned` is set: any
  PhysicalHost carrying `topology.kubernetes.io/zone` stalled a new Cluster
  and errored an existing one on every reconcile (reproduced on CAPI v1.12.2).
  The `v1beta2` label is removed; beskar7 speaks the v1beta1 contract, which
  CAPI supports until v1beta1's removal (tentatively April 2027), and CAPI
  converts the map into its own list. Failure-domain placement now works on
  CAPI v1.11+. The label returns with the real v1beta2 API (list-shaped
  `failureDomains`, `metav1.Condition`) — see the migration plan in
  `PROJECT_CONTEXT.md` D-023. **Helm-installed CRDs need a manual step**, see
  `docs/upgrading.md`.

### Added

- **`hostSelector` on `Beskar7Machine` and `Beskar7MachineTemplate`.** A standard
  label selector over `PhysicalHost` labels that a fresh claim must satisfy,
  ANDed with the owning Machine's failure domain. Label hosts by role, rack or
  hardware class and give each template a selector, and a control plane and a
  worker pool stop racing for the same inventory. Absent or empty keeps the
  previous behaviour, so existing deployments are unaffected. A selector that
  cannot be parsed is terminal (`InvalidHostSelector`); no matching host is
  `PhysicalHostAssociated=False/NoMatchingPhysicalHost` and a requeue. CRD
  schema change (additive); chart CRDs regenerated. `examples/host-pools.yaml`.
- **`--controllers=none`: a callback-only manager.** Bare-metal hosts must reach
  the callback endpoints (`/boot`, `/api/v1/inspection`, `/api/v1/bootstrap`,
  `/api/v1/provisioned`) from the provisioning network during PXE boot, which is
  often not a network the management cluster is on. A second copy of the manager
  placed on that network used to run every controller as well, and two full
  managers fight: they race for host claims, and when their `--bootstrap-url-base`
  values differ they rewrite the `bootstrap-url` annotation against each other,
  hundreds of `the object has been modified` reconcile errors a minute. With
  `--controllers=none` an instance serves the callback endpoints and the health
  probes on the usual cached client and registers no reconciler or webhook.
  Leader election is off in that mode; `--leader-elect=true` and
  `--enable-webhook=true` alongside it are rejected at startup. The callback
  server now pre-warms the PhysicalHost, Beskar7Machine, Machine and Secret
  informers its handlers read, so the first callback of each kind no longer
  waits on a lazy cache sync (a no-op in the full manager, where the controllers
  create the same informers). Documented in `docs/ipxe-setup.md` and the chart
  README; troubleshooting §14 covers the two-manager symptom.

### Fixed

- **A bootstrap token or boot nonce is reused only while the per-host Secret still
  holds its plaintext.** `triggerInspection` reused a credential whenever
  `PhysicalHost.status.bootstrap` carried an unexpired hash, without looking at
  the `<host>-bootstrap-token` Secret the host actually boots with. When the two
  disagreed — seen on a lab where two active managers minted for the same host
  within a second and their writes interleaved — every inspector callback was
  rejected with `401`, the machine timed out in `Inspecting`, and because a
  re-claim inherits the host's status the mismatch survived every replacement
  Machine until the `PhysicalHost` was recreated. The reuse check now reads the
  Secret and verifies `sha256(plaintext)` against the advertised hash (a pending
  `bootstrap-token` / `boot-nonce` annotation first, then status); a missing
  Secret, a missing key or a mismatch mints a fresh credential and logs why at
  Info with the host name, never the token. A consistent pair is still reused,
  so in-flight kernel cmdlines stay valid. Rejected bearer tokens on the `:8082`
  callback routes are now logged at Info with host and remote address (never
  the token), so a `401` is visible at default verbosity instead of only at
  V(1). No CRD or contract change.
- **The host claim now honours `Machine.spec.failureDomain` (CAPI conformance).**
  `Beskar7Cluster` publishes failure domains from the `topology.kubernetes.io/zone`
  label on PhysicalHosts and CAPI places Machines into them, but the
  `Beskar7Machine` controller ignored the placement and claimed whichever
  `Available` host listed first — a Machine placed in `rack-1` could land in
  `rack-2`. A fresh claim is now filtered by that zone label (server-side,
  alongside the existing `status.state` index). When hosts are `Available` but
  none is in the Machine's domain, `PhysicalHostAssociated=False` carries the
  new reason `NoMatchingPhysicalHost` and the machine requeues. Machines with
  no failure domain, and hosts a machine already holds, are unaffected.

Docs and examples only. No controller, CRD or contract (`v4.2`) change.

### Added

- **k0s image-side stages.** `examples/kairos-k0s-start-gate.yaml` gates
  `k0scontroller`/`k0sworker` on `!/run/cos/recovery_mode`, `!/run/cos/live_mode`
  and `/etc/k0s/.capi-args-ready`. Without it a k0s control plane does not form
  on beskar7: the whole-disk image's recovery-partition install boot applies the
  CAPI cloud-config and starts k0s, so a joiner registers as a voting etcd member
  and is then rebooted by the installer, which loses quorum for good. The marker
  is written by cluster-api-provider-kairos from commit `3698d55`
  (`fix/generic-infrastructure-provider`); the gate is a two-sided contract with
  it. `examples/kairos-k0s-providerid-stage.yaml` is the k0s counterpart of the
  ProviderID glue — it patches `Node.spec.providerID` after registration, because
  the Kairos k0s provider drops `--kubelet-extra-args`. Both verified on Kairos
  v4.1.2 + k0s v1.34.8+k0s.0 (three-replica control plane, 3/3, `EtcdHealthy=True`).
- **`docs/building-images.md`.** The verified raw-image build with AuroraBoot
  (v0.27.0, `disk.raw=true`) or osbuilder (`raw-images.sh`), the `COS_OEM`
  injection step for the image-side stages, and the digest pin. Includes the
  warning that AuroraBoot's default `90_custom.yaml` creates a `kairos`/`kairos`
  user when no `--cloud-config` is passed.
- Troubleshooting §13 (k0s joins hang / joiner became its own cluster) and the
  k0s ProviderID path in `docs/beskar7machine.md`, replacing the "set the kubelet
  flag by your distro's mechanism" advice, which does not work on k0s.

## [v0.4.3] - 2026-09-07

Fixes host reuse. No API, CRD or contract (`v4.2`) change; no manual upgrade step.

### Fixed

- **A `PhysicalHost` could only ever be provisioned once.**
  `Status.InspectionTimestamp` and `Status.DeployingTimestamp` were set on the
  first run (each guarded by `== nil`) and never cleared. The `Beskar7Machine`
  controller measures its inspection and deployment timeouts as `time.Since()`
  against them, so a host released after a successful provision kept that run's
  clock and the next machine to claim it was marked terminally failed almost
  immediately with `InspectionTimedOut` — before the host had even powered on.

  This broke every path that reuses hardware: a `MachineDeployment` replacing a
  replica, rebuilding a cluster on the same hosts, and `MachineHealthCheck`
  remediation.

  The run-scoped status is now cleared when a host has no `consumerRef`, which
  also heals a host released uncleanly (a manager restart mid-release, or a
  `consumerRef` cleared by hand). `InspectionPhase` is reset and `HostInspected`
  flipped to False with a new `HostReleased` reason, since both describe the run
  that ended rather than the hardware. Verified on bare metal: a host carrying a
  stale timestamp was released, cleared, re-claimed, and provisioned again to
  `Ready` with the node coming up as `b7://<namespace>/<host>`. (#154)

## [v0.4.2] - 2026-09-07

Fixes the kustomize install path. No API, CRD or contract (`v4.2`) change.
**Helm users are unaffected and can upgrade normally**; `kubectl apply` users
need one manual step, described in [docs/upgrading.md](docs/upgrading.md).

### Fixed

- **`kubectl apply` upgrades were impossible.** `config/default/kustomization.yaml`
  set `includeSelectors: true` on a label group containing
  `app.kubernetes.io/version`, so the version landed in
  `Deployment.spec.selector` — an **immutable** field — and in the webhook
  Service selector. `make release-manifests` rewrites that label every release,
  so re-applying the published manifest, which is the first install method in the
  README and the procedure in `docs/upgrading.md`, failed outright:

  ```
  The Deployment "controller-manager" is invalid: spec.selector:
  Invalid value: {...}: field is immutable
  ```

  Reproduced v0.4.0 → v0.4.1 and verified fixed. The version label is now applied
  to metadata only; selectors carry stable identity labels. The same split was
  applied to the `large` and `extra-large` overlays. (#153)
- **Webhook Service could lose its endpoints, blocking all `Beskar7Cluster`
  admission.** Same root cause: the version label was in that Service's selector,
  so after a version skew it matched no pods. With `failurePolicy: Fail`, every
  `Beskar7Cluster` create/update/**delete** was then rejected — observed in the
  field as a cluster stuck in `Deleting`, because the controller could not patch
  the finalizer off. (#153)

## [v0.4.1] - 2026-09-07

Patch release. The CRDs and the wire contract (`v4.2`) are unchanged, so there
is nothing to re-apply and no bootstrap-template migration.

### Added

- **`--trusted-proxies`** — comma-separated CIDRs (or bare IPs) whose
  `X-Forwarded-For` the `/boot` rate limiter will believe when identifying the
  client, surfaced in the chart as `callback.trustedProxies`. Also
  `callback.service.externalTrafficPolicy`, so a LoadBalancer or NodePort
  Service can be switched to `Local`. (#152)

### Fixed

- **`helm upgrade --reuse-values` aborted against the `v0.4.0` chart.** Upgrading
  an existing release failed before rendering anything:

  ```
  ... at <.Values.callback.externalNames>: nil pointer evaluating interface {}.externalNames
  ```

  Helm's `reuseValues()` sets `chart.Values = {}` — it discards the incoming
  chart's `values.yaml` entirely and renders against the previous release's value
  set alone, so every key added since the installed release is absent. `callback`
  (added in alpha.7) and `bootstrap` were dereferenced bare in five templates.
  They now go through a defaulted local and fall back to the documented defaults.
  Found upgrading a real alpha.6 deployment to the published v0.4.0 chart. (#151)
- **`callback.service.type` with no value broke the LoadBalancer branch.**
  `eq .Values.callback.service.type "LoadBalancer"` had no default, so an unset
  `type` failed the comparison outright instead of falling back to `ClusterIP`. (#151)

- **A fleet booting through one address starved the `/boot` rate limiter.** The
  limiter keys on the peer address, and every documented exposure option can
  collapse a whole fleet onto one of them: a LoadBalancer or NodePort Service
  with the default `externalTrafficPolicy: Cluster` SNATs to a node IP, and an
  L4 proxy without PROXY protocol presents its own. All hosts then shared a
  single 1 r/s bucket, so a fleet powering on together — after a DC power event,
  say — booted a few hosts per second while the rest retried. Operators can now
  either preserve the client IP (`externalTrafficPolicy: Local`) or declare
  their hops (`--trusted-proxies`).

  `X-Forwarded-For` remains **ignored by default**. `/boot` has no bearer gate,
  so trusting a client-settable header unconditionally would let a single caller
  mint unlimited rate-limit buckets and neutralise the limiter; the header is
  read only when the peer is itself a declared proxy, and then the right-most
  entry that is not a trusted proxy wins, so values a client prepends can never
  be selected. (#152)
- **IPv6 peers produced a bracketed rate-limit key.** `remoteAddrToIP` scanned
  for the last colon, so `[2001:db8::1]:443` keyed as `[2001:db8::1]`. It now
  uses `net.SplitHostPort`. Cosmetic — the key was stable either way. (#152)

### Changed

- **`--reuse-values` is documented as unsupported.** `docs/upgrading.md` and the
  chart README now direct operators to `--reset-then-reuse-values` (Helm 3.14+)
  or an explicit `-f values.yaml`, and explain why. (#151)

## [v0.4.0] - 2026-09-07

**First GA release.** `v1beta1` is stable and frozen, the controller↔inspector
wire contract is frozen at **v4.2**, release images are signed, and the
`beskar7-inspector` is distributed as versioned, checksummed, signed artifacts.

Highlights since `v0.4.0-alpha.8`:

- **Per-host `ProviderID` for templated pools (contract v4.2)** — a shared
  `Beskar7MachineTemplate` cannot pin a per-host `ProviderID`, which blocked
  `MachineDeployment` pools and multi-replica control planes. The controller now
  renders `beskar7.provider-id` on `/boot`, the inspector writes it to
  `/oem/beskar7/provider-id`, and an image-side stage turns it into the kubelet
  flag. **Verified end-to-end on hardware**: `Node.spec.providerID` came up as
  `b7://<ns>/<host>` and CAPI advanced the Machine past `Provisioned`.
- **The distributable inspector** — `beskar7-inspector` now publishes releases
  with `vmlinuz`, `initrd.img`, checksums, a cosign bundle, and an image tagged
  by contract version. Adopters no longer clone a second repo and build it.
- **Signed releases** — images signed with cosign (keyless), controller image
  carries a signed SPDX SBOM attestation.
- **`v1beta1` frozen**; **contract v4.2 frozen** with an explicit
  backward-compatibility policy (contract §14).

**Upgrading:** not compatible with `v0.3.x`; the alpha series contains breaking
API changes. See [docs/upgrading.md](docs/upgrading.md). Pair the controller with a
`contract-v4.2` inspector release.

**Known limitation:** releasing a `PhysicalHost` does not sanitize its disk — the
previous tenant's OS and injected bootstrap config (which may carry a cluster join
secret) survive until re-provisioning. Wipe hosts before moving them between trust
boundaries. See [SECURITY.md](SECURITY.md).

### Fixed
- **The documented §2.4 ProviderID glue did not work, and now does.** `docs/troubleshooting.md` and `examples/kairos-k3s-node.yaml` presented the per-host ProviderID stage as a `stages:` block inside the `#cloud-config` bootstrap Secret. Kairos honors such a file's top-level keys (`hostname`, `users`, `k3s`) but **silently ignores its `stages:` block** — it is processed as `'<file>.0'` with `commands: 0` and nothing resembling an error is logged. Every `stages:` block shipped in beskar7's docs and examples was therefore inert, including the `enable-sshd` step (and, for the same reason, the Kairos image's own `90_custom.yaml` user setup, which is why SSH password auth failed on provisioned hosts).

### Added
- **`examples/kairos-providerid-stage.yaml`** — the working form, verified end-to-end on Kairos v4.1.2 (hadron) + k3s v1.34.8: `Node.spec.providerID` came up as `b7://<namespace>/<host>` and CAPI advanced the Machine past `Provisioned`. It is a **yip config** (top-level `name:` + `stages:`) baked into the target image's `COS_OEM` as `/oem/10_beskar7_providerid.yaml`. Because it reads whatever per-host value the inspector injected, it is host-independent: **one image and one `Beskar7MachineTemplate` serve every replica of a pool**, which is what D-014 P2 exists to enable.
- Documented the second, equally silent constraint: the stage **must run before the distro first starts**. `Node.spec.providerID` is immutable once a node registers, so a node that joins without the flag keeps the distro default (`k3s://<hostname>`) and must be re-provisioned rather than corrected in place.
- `docs/beskar7machine.md` gains a **Templated pools** section; `docs/inspector-contract.md` §9.1 gains a "who consumes this" note making clear that beskar7 only *writes* the artifact and the kubelet wiring is operator-side; `examples/README.md` indexes the new example.


### Documentation
- **Contract §12 (retry policy), §13 (node-join timeout), §14 (backward-compatibility policy)** — resolves the `docs/inspector-contract.md` open item that read *"the exact retry policy and node-join timeout are not specified in v4.1 and must be defined before GA"* (GA freeze-checklist items 1 and 3). No wire change: §12 formalises that provisioning failures are terminal and remediation belongs to CAPI (with a `--deployment-timeout` sizing rule), §13 delegates node-join detection to `MachineHealthCheck.spec.nodeStartupTimeout` (recommended `15m`) and explains why an infrastructure provider must not watch the workload cluster, and §14 states what may change inside the frozen `v4.x` line versus what requires a `v5` bump.
- **`examples/machinehealthcheck.yaml`** — recommended `MachineHealthCheck` for a Beskar7 pool, covering both "provisioned but never joined" (`nodeStartupTimeout: 15m`) and post-join unhealthiness, with `maxUnhealthy` guidance since remediation triggers a destructive whole-disk reprovision.
- **`docs/troubleshooting.md` entry 12** — the closing note still said a templated `MachineDeployment` "can't yet pin the per-host ProviderID"; that shipped in contract v4.2. Replaced with the shared-template stage that reads `/oem/beskar7/provider-id`, the v4.2-both-sides requirement, and an explicit note that the boot-time stage itself is not yet validated end to end. Added guidance for the case where the ProviderID matches and the Node still never appears.
- **`SECURITY.md`** — private vulnerability reporting via GitHub Security Advisories, response-time intentions, supported-version policy, artifact verification, and the security-relevant design points a reporter needs (bearer-gated callback, single-use boot nonce, digest-anchored image integrity). Documents two by-design limitations explicitly so they are not reported as vulnerabilities: **no disk sanitization on host release** (the previous tenant's OS and injected bootstrap config, which may carry a cluster join secret, survive until re-provisioning) and opt-in `insecureSkipVerify`.
- **`docs/upgrading.md`** — the first upgrade guide. Covers component upgrade order (CRDs before controller, since Helm does not upgrade CRDs), controller↔inspector contract matching with a verified release→contract table, the `alpha.8 → alpha.9` breaking changes with a `jq` query to find affected `PhysicalHost`s, the absence of a `v0.3.x` in-place path, and rollback limits (CRDs do not roll back automatically).
- **`CONTRIBUTING.md`** — setup, pre-PR checks, and the parts of the codebase carrying non-obvious constraints: the versioned two-repo inspector contract, RBAC's three hand-maintained copies, CAPI failure semantics, and destructive provisioning paths. States the expectation that a regression test must be verified to fail without its fix.



### Fixed
- **A terminally-failed `Beskar7Machine` could stamp success over its own failure.** `markTerminalFailure` documents that `FailureReason` is never cleared and means "needs operator intervention", but nothing stopped a later reconcile from running the normal state machine anyway: if the `PhysicalHost` subsequently recovered, `handleReadyHost` set `Ready=true`, `Phase=Provisioned` and `Initialization.Provisioned=true` on a machine still carrying `FailureReason`. Observed on real hardware — a machine finished a run reporting `Ready=true, Phase=Provisioned` **and** `FailureReason=InspectionTimedOut` simultaneously. The contradiction is not cosmetic: CAPI lifts `FailureReason`/`FailureMessage` onto the owning `Machine` and treats them as unrecoverable, so the object both misleads an operator reading status and invites `MachineHealthCheck` to remediate a node that is actually serving. `Reconcile` now returns early when `FailureReason` is set, after the deletion path — so a failed machine can still be deleted and release its `PhysicalHost`, which is exactly how a `MachineDeployment` self-heals.


### Added
- **`--max-concurrent-reconciles` manager flag** — sets the reconcile worker count for all three controllers. Defaults to `1`, matching controller-runtime, so deployments that do not set it are unaffected. The motivation is fault isolation more than throughput: with a single worker, one unreachable BMC can occupy it for a full 30s Redfish timeout and stall reconciles for healthy hosts. Raising it is safe with respect to BMC load because controller-runtime never reconciles the same object concurrently, so distinct workers always act on distinct `PhysicalHost`s. Supersedes the earlier MEDIUM-1 decision to leave concurrency code-only, which assumed operators could patch and rebuild — no longer true once external adopters run fleets. Documented in `docs/{troubleshooting,resource-planning,deployment-best-practices}.md`.
- **Smoke layer 7: templated multi-replica pool** — `hack/smoke/run.sh` gains a layer that drives a real `MachineDeployment` (`replicas: 2`) over two mock BMCs and asserts the two `Beskar7Machine`s cloned from one `Beskar7MachineTemplate` claim different `PhysicalHost`s, receive **distinct** `ProviderID`s, and that each `ProviderID` names its own claimed host. This moves the D-014 P2 guarantee from a one-off lab result into a standing CI gate; the remaining hardware-only surface is the inspector's on-disk `/oem/beskar7/provider-id` write and the kubelet glue that consumes it. On by default; `--skip-layer-7` opts out.

### Fixed
- **`Beskar7ClusterReconciler.SetupWithManager` silently discarded its `options` argument** — the `controller.Options` parameter was accepted and never applied, so any caller-supplied controller configuration was dropped. The options are now passed through to the builder (with the worker count overlaid). No behaviour change today, since the only caller passed an empty struct.


## [v0.4.0-alpha.9] - 2026-08-31

The "adoption readiness" release: the `v1beta1` API is **frozen**, the
controller↔inspector contract moves to **v4.2** with per-host `ProviderID`
delivery, release images are **signed**, and the `beskar7-inspector` is now
**distributable** — published as versioned, checksummed, signed artifacts
instead of something every adopter had to build from source.

**Upgrading from alpha.8 requires action** — see the BREAKING entry under
Changed: `configurationURL` was removed and `caBundleSecretRef` changed shape.

### Added
- **Per-host `ProviderID` delivery for templated pools/HA control planes** (contract v4.2, D-014 P2) — `/boot` now always renders a new required cmdline param `beskar7.provider-id=b7://<namespace>/<host>` (`controllers/boot_handler.go`), positioned immediately after `beskar7.target-digest` and before `beskar7.ca`. The value is `providerID(ph.Namespace, ph.Name)` — the exact call `handleReadyHost` already uses to stamp `Beskar7Machine.Spec.ProviderID` — so the rendered and stamped values cannot diverge. New `validateProviderID` injection guard (SEC-7 defence-in-depth, anchored `^b7://[a-z0-9.-]+/[a-z0-9.-]+$`). This closes the gap where a *shared* `Beskar7MachineTemplate`/bootstrap config could not pin a *per-host* `ProviderID`, blocking `MachineDeployment` worker pools and multi-replica control planes; the inspector writes the value verbatim to a new `COS_OEM` artifact `/oem/beskar7/provider-id` (no trailing newline, mode `0600`, root-owned) that a shared boot-time stage reads to set a per-host kubelet `--provider-id`. Additive and backward-compatible: a v4.1 inspector ignores the unknown param and never writes the artifact.
- **Deploy-path golden fixtures** (`test/contract/golden_boot_cmdline.txt`, `test/contract/golden_provider_id_artifact.json`) and `controllers/deploy_contract_test.go` — byte-exact `/boot` cmdline render guard, a language-neutral descriptor of the `COS_OEM` provider-id artifact cross-checked against the real `providerID()`, and an injection-guard table test for `validateProviderID`. `controllers/boot_handler_test.go` adds the regression guard proving the render-time and stamp-time `ProviderID` values are identical and that `beskar7.provider-id` renders unconditionally, even before the host reaches `Ready`.
- **Contract version bumped to v4.2** — `test/contract/VERSION`, `contract.Version`, and `docs/inspector-contract.md` all bumped; `TestContractVersion` unaffected (self-consistency check only).

### Changed
- **BREAKING (API): v1beta1 pre-freeze cleanup toward v0.4.0 GA** — removed the dead `Beskar7Machine.Spec.configurationURL` field (consumed nowhere); normalized `PhysicalHost.Spec.redfishConnection.caBundleSecretRef` from an object (`{name: <secret>}`) to a bare `string`, matching the sibling `credentialsSecretRef`; changed `Beskar7Machine.Spec.hardwareRequirements` (`minCPUCores`/`minMemoryGB`/`minDiskGB`) and the `PhysicalHost` inspection ints (`cores`/`threads`/`sizeGB`) from `int` to `int32` (Kubernetes API convention); removed the dead `RedfishConnectionInfo` Go type; and corrected the `inspectionImageURL` field description (it is the base URL under which `vmlinuz`/`initrd.img` are served, not an iPXE boot script). **Action required:** any existing CR using `configurationURL` or the object-form `caBundleSecretRef` must be updated. Part of the API-stability freeze work.

### Removed
- **`MachineProvisionedCondition` constant** — removed from `api/v1beta1/beskar7machine_types.go`. It was declared but never set by any reconciler (verified: zero non-declaration references), so a caller scripting against it would wait forever. Use `InfrastructureReady`, `Status.Ready`, and `Status.Initialization.Provisioned` instead. Not a CRD-schema change (condition types are runtime values, not schema); no `make manifests` diff.

### Security
- **Release images are signed with cosign** (keyless/OIDC, bound by digest to the release workflow identity — no long-lived key). The controller image additionally carries its SPDX SBOM as a **signed attestation** rather than a detached artifact. Verification instructions in `docs/installation.md`. Signing applies from this release onward; `v0.4.0-alpha.8` and earlier are unsigned and will not verify.
- **RBAC binding/subject-graph guard** (SEC-2, #131) — `test/rbac` now also asserts every `Role`/`ClusterRole` in each topology (cluster-wide kustomize, namespace-scoped overlay, both Helm branches) is bound to the manager ServiceAccount by exactly one binding with a matching `roleRef`, catching unbound roles, dangling `roleRef`s, and wrong-identity subjects that the rule-content guard cannot see.

### Documentation
- **v1beta1 declared frozen** (D-018, #133) — stable and **additive-only** until a future `v1beta2` introduced with a conversion webhook.
- **`docs/api-reference.md` reconciled** against the frozen types (#135) — added the missing `targetImageDigest`/`targetDisk`/`staticIP`, the `Deploying` state and `Provisioning` phase, all seven terminal `failureReason` values, corrected the bearer-token lifetime (30 → 60 min), and fixed two embedded examples that were CRD-invalid.
- **Removed-kexec sweep** (#137, #140) — `docs/` and `examples/` reconciled to the v2 whole-disk-image model, and the README's "How It Works" and `Beskar7Machine` example corrected (the example was **CRD-invalid**: it omitted the required `targetImageDigest` and used the removed `.tar.gz` format).
- **Honest hardware-compatibility claims** — five vendors were marked "Tested" with per-vendor experience claims; no physical vendor BMC has ever been validated. The doc now separates the *design* property (no vendor-specific code paths) from *validation status*, states exactly what has been exercised (a stateful fake, the DMTF `public-rackmount1` mockup, and sushy-tools emulation), and invites real-hardware reports.
- **Cross-repo contract sync documented** (D-019, #134) — `test/contract/` is the single source of truth; the inspector vendors byte-copies pinned to an immutable `contract/<version>` tag and gates on drift in its own CI.

## [v0.4.0-alpha.8] - 2026-07-21

The "contract v4.1 + hardening" release: adds the provision-failed fast-fail callback so a failed Phase-2 deploy is surfaced immediately instead of waiting out the deployment timeout, documents the ProviderID/Node-association contract that takes a provisioned node all the way to a CAPI `Machine: Running`, and adds two regression guards (a Redfish read-robustness corpus and a structural RBAC drift guard).

### Added
- **`POST /api/v1/provision-failed/{namespace}/{hostName}` callback endpoint** (contract v4.1) — bearer-gated HTTPS endpoint on `:8082`; the inspector calls it when Phase 2 fails (image fetch, digest verify, disk write, or `COS_OEM` inject) before exiting, reporting the failure promptly instead of waiting out `--deployment-timeout` (up to 20 min). The `PhysicalHost` transitions `StateDeploying → StateError` immediately; the `Beskar7Machine` is marked `FailureReason=DeploymentFailed` with the sanitized inspector reason in `FailureMessage`. Backward-compatible: a v4 controller without the endpoint returns 404, which the v4.1 inspector tolerates. Implemented in `controllers/provision_failed_handler.go`; route registered in `SetupCallbackServer`.
- **`DeploymentFailed` failure reason** — new `FailureReason` constant on `Beskar7Machine` (`DeploymentFailedReason = "DeploymentFailed"`), distinct from `DeploymentTimedOut` (timeout) and `PhysicalHostError` (Redfish/BMC-level error). The `StateError` handler in `Beskar7MachineReconciler` attributes the reason by inspecting the `PhysicalHost.Status.ErrorMessage` prefix set by the provision-failed handler.
- **Inspector contract v4.1** — `docs/inspector-contract.md` bumped to v4.1; §4.5 documents the new `/provision-failed` endpoint; §2 provisioning sequence updated with the fast-fail path; §11 open item on provisioning-failure recovery closed.

### Docs
- **ProviderID / Node-association contract (D-014 P1)** (#128) — new "ProviderID & Node association" section in `docs/beskar7machine.md` with per-distro `kubelet` `--provider-id` snippets (k3s proven, kubeadm, k0s/generic), a troubleshooting entry ("CAPI Machine stuck at `Provisioned`, never reaches `Running`"), and the `provider-id` line made default-on in `examples/kairos-k3s-node.yaml`. Covers the explicitly-authored per-machine case; the scaled `MachineDeployment` case is noted as future work (D-014 P2).

### Internal
- **Structural RBAC drift guard** (SEC-2, #129) — `test/rbac` asserts the hand-authored RBAC copies (Helm chart both `watchNamespaces` branches + the `config/rbac/namespace-scoped/` kustomize overlay) carry the same `(apiGroup, resource, verb)` rule-set as the controller-gen `config/rbac/role.yaml`, failing CI on either over- or under-grant. CI installs `helm` in the unit-test job so the chart checks run rather than skip.
- **Redfish read-robustness corpus** (TEST-1, #127) — vendored a curated subset of the DMTF `public-rackmount1` mockup under `internal/redfish/testdata/corpus/` plus a static handler and `corpus_robustness_test.go` that points the real gofish client at it, exercising the `GetSystemInfo`/`GetPowerState`/`GetNetworkAddresses` read paths. Complements (does not replace) the stateful `internal/redfishmock` fake.
- **`defaultCloseTimeout` const** (MEDIUM-2, #126) — extracted the Redfish `Close`/logout timeout literal to a named const in `internal/redfish/gofish_client.go`; no behavior change.

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

[Unreleased]: https://github.com/projectbeskar/beskar7/compare/v0.5.0...HEAD
[v0.5.0]: https://github.com/projectbeskar/beskar7/compare/v0.4.4...v0.5.0
[v0.4.4]: https://github.com/projectbeskar/beskar7/compare/v0.4.3...v0.4.4
[v0.4.3]: https://github.com/projectbeskar/beskar7/compare/v0.4.2...v0.4.3
[v0.4.2]: https://github.com/projectbeskar/beskar7/compare/v0.4.1...v0.4.2
[v0.4.1]: https://github.com/projectbeskar/beskar7/compare/v0.4.0...v0.4.1
[v0.4.0]: https://github.com/projectbeskar/beskar7/compare/v0.4.0-alpha.9...v0.4.0
[v0.4.0-alpha.9]: https://github.com/projectbeskar/beskar7/compare/v0.4.0-alpha.8...v0.4.0-alpha.9
[v0.4.0-alpha.8]: https://github.com/projectbeskar/beskar7/compare/v0.4.0-alpha.7...v0.4.0-alpha.8
[v0.4.0-alpha.7]: https://github.com/projectbeskar/beskar7/compare/v0.4.0-alpha.6...v0.4.0-alpha.7
[v0.4.0-alpha.6]: https://github.com/projectbeskar/beskar7/compare/v0.4.0-alpha.5...v0.4.0-alpha.6
[v0.4.0-alpha.5]: https://github.com/projectbeskar/beskar7/compare/v0.4.0-alpha.4...v0.4.0-alpha.5
[v0.4.0-alpha.4]: https://github.com/projectbeskar/beskar7/compare/v0.4.0-alpha...v0.4.0-alpha.4
[v0.4.0-alpha]: https://github.com/projectbeskar/beskar7/compare/v0.3.4-alpha...v0.4.0-alpha
[v0.3.4-alpha]: https://github.com/projectbeskar/beskar7/compare/v0.2.7...v0.3.4-alpha
[v0.2.7]: https://github.com/projectbeskar/beskar7/releases/tag/v0.2.7
[v0.2.6]: https://github.com/projectbeskar/beskar7/releases/tag/v0.2.6

