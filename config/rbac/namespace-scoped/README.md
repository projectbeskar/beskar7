# Namespace-scoped RBAC overlay

This directory contains the kustomize manifests for tightening the manager's
RBAC from cluster-wide to per-namespace, equivalent to setting
`.Values.watchNamespaces` in the Helm chart (SEC-2).

It is **not included in `config/default/`**. Operators opt in by editing
their own overlay to reference this dir instead of `../rbac`.

## What's in here

| File | Purpose |
|---|---|
| `minimal-clusterrole.yaml` | Cluster-scoped reads only (`clusterroles`, `clusterrolebindings` — auto-generated from a kubebuilder marker in `cmd/manager/main.go`). |
| `leader-election-role.yaml` | `Role` + `RoleBinding` in `beskar7-system` for leader-election `Lease` access and operator-side `Event` creation. Leases live where the operator runs, not where its watched CRs live. |
| `watch-role.template.yaml` | **Template** for the per-namespace `Role` + `RoleBinding`. Not included in `kustomization.yaml`. Copy and patch the `namespace:` fields once per watched namespace. |
| `kustomization.yaml` | Resource list — references everything except `watch-role.template.yaml`. |

## Usage

For each namespace the manager should reconcile in:

1. Copy `watch-role.template.yaml` to `watch-role.<namespace>.yaml`.
2. Edit the two `namespace:` fields in the copy to that namespace name.
3. Append the new filename to `kustomization.yaml`'s `resources:` list.

Then in your own overlay (for example `config/overlays/<env>/kustomization.yaml`), stack the namespace-scoped RBAC with the shared bits from `config/rbac/`:

```yaml
namespace: beskar7-system
resources:
# Namespace-scoped manager RBAC (replaces the cluster-scoped role.yaml +
# role_binding.yaml from config/rbac/).
- ../../rbac/namespace-scoped
# Shared bits — kept in the parent dir because they apply to both topologies.
- ../../rbac/service_account.yaml
- ../../rbac/metrics_auth_role.yaml
- ../../rbac/metrics_auth_role_binding.yaml
- ../../rbac/metrics_reader_role.yaml
# Manager + CRDs + webhook + certmanager + security as before.
- ../../manager
- ../../crd
- ../../webhook
- ../../certmanager
- ../../security
```

This is the equivalent of replacing `config/default/`'s `../rbac` base with the namespace-scoped variant. **Do not** also include `../../rbac` (the parent dir) — it ships the cluster-scoped role + binding that this overlay is meant to replace, and kustomize will error on the duplicate `manager-role` name.

And patch the manager Deployment to pass `--watch-namespaces` with the same
comma-separated list:

```yaml
patches:
- target:
    kind: Deployment
    name: controller-manager
  patch: |-
    - op: add
      path: /spec/template/spec/containers/0/args/-
      value: --watch-namespaces=ns1,ns2,ns3
```

The Helm chart automates all of this via `.Values.watchNamespaces`; see
`charts/beskar7/values.yaml` and `charts/beskar7/templates/rbac.yaml` for
the templated equivalent.

## Why kustomize can't auto-generate per-namespace Roles

Helm's templating supports `{{- range $ns := .Values.watchNamespaces }}` to
emit one Role per namespace. Kustomize has no loop construct — it operates
on a fixed set of YAML files plus patches. The manual copy-template-per-
namespace step is the kustomize-idiomatic way to express this.

For very large numbers of watched namespaces, a generator script wrapping
kustomize (or migrating to Helm) is more ergonomic.
