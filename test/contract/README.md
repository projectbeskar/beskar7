# Inspector contract fixtures (contract: v4.2)

This directory holds the **canonical golden fixtures** for the controller ↔
inspector wire contract — the inbound inspection-report body and the outbound
deploy-path artifacts. They are the anti-drift guardrail between this repo and
[`beskar7-inspector`](https://github.com/projectbeskar/beskar7-inspector).

The authoritative prose spec is [`docs/inspector-contract.md`](../../docs/inspector-contract.md);
these fixtures are the machine-checked half of it.

## `golden_inspection_report.json`

The exact JSON body a `beskar7-inspector` run POSTs to
`POST {callbackBase}/api/v1/inspection/{namespace}/{hostName}`
(`namespace`/`hostName` are path parameters, not body fields, so they do **not**
appear here). It models one representative dual-socket host.

Both repos assert against the **same bytes**:

- **Beskar7** — `controllers/inspection_contract_test.go` (`go test ./controllers/...`)
  decodes it into `InspectionReportRequest`, both leniently (production parity)
  and strictly (`DisallowUnknownFields`, forward-drift catch), round-trips it to
  prove the struct is lossless, runs `buildInspectionReport`, and runs the real
  `parseMemoryCapacityGB` over the memory entries to lock the hardware-aggregate
  math.
- **beskar7-inspector** — a Rust serde round-trip test deserializes the same file
  into its `Report` type and re-serializes it, asserting byte-equality.

If the inspector adds, renames, or retypes a field, one side's test goes red.
**Keep the two copies identical.** When the contract changes, bump the version in
`docs/inspector-contract.md` **and** this directory's `VERSION` file, update this
fixture, and see "Cross-repo sync contract" below for how the inspector picks up
the change.

## Documented aggregates

The controller's hardware-requirements validation sums these from the fixture
(kept in sync with the constants in `inspection_contract_test.go`):

| Aggregate | Value | Derivation |
|---|---|---|
| Total CPU cores | 64 | 2 sockets × 32 cores |
| Memory per DIMM | 34 GB | `"32GiB"` → 32 × 2³⁰ bytes ÷ 1e9, **truncated** |
| Total memory | 136 GB | 4 × 34 |
| Total disk | 1920 GB | 2 × 960 |

> **IEC vs decimal:** `parseMemoryCapacityGB` treats `GiB`/`MiB`/`TiB` as binary
> (×1024) and `GB`/`MB`/`TB` as SI (×1000), then converts to **decimal GB**
> (÷1e9) and truncates. So a `"32GiB"` DIMM counts as **34** GB toward
> `MinMemoryGB`, not 32. Inspector authors emitting capacity strings must expect
> this. Accepted suffixes: `GB`, `GiB`, `MB`, `MiB`, `TB`, `TiB` — a bare number
> with no unit is rejected.

## `golden_boot_cmdline.txt` (contract v4.2, deploy-path)

The byte-exact iPXE `/boot` script the controller renders for a host — pinning
the kernel cmdline param order and values, including `beskar7.provider-id=b7://{ns}/{host}`
(D-014 P2, added in v4.2, immediately after `beskar7.target-digest`). Beskar7's
`controllers/deploy_contract_test.go` regenerates the render from the real
`buildBootIPXEScript`/`providerID()` and asserts byte-equality; the inspector's
cmdline parser must accept every param present here.

## `golden_provider_id_artifact.json` (contract v4.2, deploy-path)

The descriptor for the `COS_OEM` artifact the inspector writes for P2: path
`/oem/beskar7/provider-id`, `content` = the `beskar7.provider-id` value verbatim
with **no trailing newline**, mode `0600`, owner `root`. The inspector writes it
in the same mount session as `99_beskar7.yaml`; a shared bootstrap stage reads it
to set a per-host kubelet `--provider-id`. Beskar7 asserts the `content` equals
the computed `providerID(ns, host)`; the inspector asserts it writes exactly this.

## `VERSION`

A one-line plain-text marker: the contract version this checkout implements
(currently `v4.2`). It is the root of truth for the version — beskar7 pins a
Go const to it (`contract.Version`, `version.go` in this directory), and
`beskar7-inspector` pins its own Rust `CONTRACT_VERSION` to a vendored copy of
the same bytes. `TestContractVersion` (`version_test.go`) is the intra-repo
guard: it fails if the Go const and this file ever say different things.

## Cross-repo sync contract

**beskar7 is the single source of truth for everything in this directory.**
`beskar7-inspector` never edits these files directly; it **vendors
byte-copies** into its own tree and **pins an immutable `contract/<version>`
git tag** in this repo as the ref those copies were taken from. The two repos
stay in sync through one CI job that lives entirely on the inspector's side:

1. The inspector's CI fetches beskar7's canonical files (`VERSION`,
   `golden_inspection_report.json`, and any other file added to this
   directory) at its pinned `contract/<version>` tag.
2. It `diff`s the fetched bytes against its own vendored copies. Any
   difference — a single byte — fails the job.
3. It asserts its Rust `CONTRACT_VERSION` equals the vendored `VERSION`
   contents.

beskar7's own CI does **not** reach across repos. It stays hermetic: the only
new obligation on this side is `TestContractVersion`, which just proves the
`contract.Version` Go const and this directory's `VERSION` file agree with
each other — a self-consistency check, not a cross-repo one. Whether the
inspector has caught up to whatever beskar7 currently ships is visible only in
the inspector's own CI.

### Release checklist — the one manual step

**Every time a file in this directory changes (a fixture edit or a `VERSION`
bump), beskar7 must push a new immutable `contract/<version>` tag** at the
commit that lands the change, e.g. `contract/v4.1`. This is not automated by
anything in this repo — add it to the release checklist.

**Known blind spot:** if the tag push is forgotten, nothing in either repo's
CI catches it. The inspector's drift `diff` only ever compares its vendored
copies against whatever tag it has pinned — if that pin is stale, both sides
still match each other byte-for-byte, and the job stays green while the
inspector silently falls behind the contract beskar7 actually ships. The CI
diff detects *byte drift against the pinned ref*; it cannot detect *a ref that
was never advanced*. Treat the tag push as a hard requirement of any PR that
touches this directory, not an optional follow-up.

### What is and isn't covered

This mechanism keeps the **fixture bytes and the version marker** identical
across repos. It says nothing about whether the inspector's Rust code
actually parses or emits those bytes correctly — that is the inspector's own
behavioral test suite, run against its vendored copies (no network required
for those tests; only the drift `diff` step needs to reach GitHub).

Detection is intentionally one-directional: beskar7's hermetic CI cannot tell
whether the inspector has caught up, and does not try to. The party that must
react to a contract change is the inspector, so its CI is the one that gates
on drift.
