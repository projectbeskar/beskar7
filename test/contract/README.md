# Inspector contract fixtures (contract: v1)

This directory holds the **canonical golden fixture** for the controller ↔
inspector wire contract. It is the anti-drift guardrail between this repo and
[`beskar7-inspector`](https://github.com/projectbeskar/beskar7-inspector).

The authoritative prose spec is [`docs/inspector-contract.md`](../../docs/inspector-contract.md);
this fixture is the machine-checked half of it.

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
`docs/inspector-contract.md`, update this fixture, and update both repos in lockstep.

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
