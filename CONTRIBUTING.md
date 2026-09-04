# Contributing to Beskar7

Thanks for considering a contribution. Beskar7 is a Cluster API infrastructure
provider for bare-metal hosts, so most changes touch either a reconcile loop, the
controller↔inspector contract, or the operator-facing docs — each has slightly
different expectations, described below.

## Before you start

- **Open an issue first for anything non-trivial.** A bug fix or doc correction can
  go straight to a PR; an API change, a new CRD field, or anything touching the
  wire contract should be discussed first — some of these carry constraints that
  are not obvious from the code (see *Things that look simple but are not*).
- **Security issues do not belong in public issues.** See [SECURITY.md](SECURITY.md).

## Development setup

```bash
git clone https://github.com/projectbeskar/beskar7.git
cd beskar7
make build
make test        # unit + envtest; needs KUBEBUILDER_ASSETS
```

`docs/development-setup.md` covers envtest assets and running the manager locally.
`docs/ci-cd-and-testing.md` describes the test tiers.

## Before you open a PR

```bash
make manifests   # ONLY if you touched api/ or a +kubebuilder: marker
make test
golangci-lint run --timeout=5m
```

If you changed anything under `api/v1beta1/` or any `+kubebuilder:` marker you
**must** run `make manifests` and commit the regenerated
`config/crd/bases/*.yaml`, `config/rbac/role.yaml` and
`api/v1beta1/zz_generated.deepcopy.go`. Then run `make sync-chart-crds` so the
chart-bundled CRDs stay byte-identical — CI fails if they drift.

## PR expectations

- **One concern per PR.** Do not bundle a fix with a refactor and a doc update;
  reviewers cannot bisect that later.
- **Tests ship with the change.** New controller logic needs an envtest in
  `controllers/*_test.go`. Adding a new pending (`PIt`) spec is not accepted —
  make it a real `It` or leave it out.
- **A regression test must fail without your fix.** Verify that explicitly. A test
  that passes both with and without the change proves nothing, and it is easier to
  write one of those than it looks.
- **Explain *why* in the commit message.** What changed is visible in the diff;
  the reasoning is not, and it is what a reader needs in six months.
- **Do not document behavior that does not exist yet.** If validation is partial,
  say which part. This project has had to remove overclaims before, and they cost
  more trust than an honest gap.

## Things that look simple but are not

**The controller↔inspector contract.** `docs/inspector-contract.md` is a versioned
wire contract shared with a separate repository
([`beskar7-inspector`](https://github.com/projectbeskar/beskar7-inspector)). The
golden fixtures in `test/contract/` are the machine-checked half, and the inspector
vendors byte-identical copies pinned to an immutable `contract/<version>` tag.
Changing the cmdline, an endpoint, or the report schema means a coordinated change
in both repos plus a `contract/<version>` tag push. Read `test/contract/README.md`
first.

**RBAC.** `config/rbac/role.yaml` is generated from `+kubebuilder:rbac:` markers,
but the Helm chart and the `config/rbac/namespace-scoped/` overlay are
hand-maintained copies. `test/rbac` fails CI if they diverge — update all three.

**Failure semantics.** `FailureReason`/`FailureMessage` are lifted by CAPI onto the
owning `Machine` and treated as **unrecoverable**. Setting one means "an operator
must intervene", and CAPI may remediate the machine. Do not set them for anything
recoverable.

**Anything destructive.** Provisioning overwrites a whole disk. Retry loops,
remediation, and cleanup paths get scrutiny in review — see
`docs/inspector-contract.md` §12 for why beskar7 owns no retry loop.

## Documentation changes

Docs are verified against the code, not against other docs. If you document a
field, check `api/v1beta1/*_types.go`; if you document behavior, check the
controller. Several docs have drifted from the implementation in the past, so a
plausible-looking existing sentence is not evidence.

## Commit and PR hygiene

- Reference the issue you are addressing.
- Note anything you did **not** do and why — an honest gap is easier to review
  than a silent one.
- CI must be green. If a check is unrelated and flaky, say so in the PR rather
  than re-running until it passes.

## Code of conduct

Be decent to other contributors. Assume good faith, keep review comments about the
code, and expect the same in return.
