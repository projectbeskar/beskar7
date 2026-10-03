# Namespace-scoped RBAC overlay

This directory contains the kustomize manifests for tightening the manager's
RBAC from cluster-wide to per-namespace, equivalent to setting
`.Values.watchNamespaces` in the Helm chart (SEC-2).

It is **not included in `config/default/`**. Operators opt in by building an
install overlay of their own that references this dir (see
[Installing](#installing)).

## What's in here

| File | Purpose |
|---|---|
| `leader-election-role.yaml` | `Role` + `RoleBinding` in `capb7-system` for leader-election `Lease` access and operator-side `Event` creation. Leases live where the operator runs, not where its watched CRs live. |
| `watch-role.template.yaml` | **Template** for the per-namespace `Role` + `RoleBinding`. Not included in `kustomization.yaml`. Copy and patch the `namespace:` fields once per watched namespace. |
| `kustomization.yaml` | Resource list — references everything except `watch-role.template.yaml`. Sets no `namespace:` (see [Why there is no `namespace:`](#why-there-is-no-namespace)). |

## Usage

For each namespace the manager should reconcile in:

1. Copy `watch-role.template.yaml` to `watch-role.<namespace>.yaml`.
2. Edit the two `namespace:` fields in the copy (they read
   `REPLACE_WITH_WATCHED_NAMESPACE`) to that namespace name.
3. Append the new filename to `kustomization.yaml`'s `resources:` list.

Then build the directory and check where everything landed:

```bash
kustomize build config/rbac/namespace-scoped
```

For `tenant-a` and `tenant-b` that is, by kind/name/namespace:

```
Role         manager-leaderelection-role         capb7-system
RoleBinding  manager-leaderelection-rolebinding  capb7-system
Role         manager-watch-role                  tenant-a
RoleBinding  manager-watch-rolebinding           tenant-a
Role         manager-watch-role                  tenant-b
RoleBinding  manager-watch-rolebinding           tenant-b
```

Every `RoleBinding` names the manager's `ServiceAccount`, `capb7-manager` in
`capb7-system`, as its subject. A watch `Role` anywhere else, or a second one
in `capb7-system`, means the build went wrong: the manager would hold nothing
in the namespace it watches.

## Installing

`config/default/` ships the cluster-wide `ClusterRole` and `ClusterRoleBinding`
this overlay replaces, so the install overlay layers on top of it, removes
those two objects, and adds this directory. Create it outside
`config/overlays/`, whose contents `test/contract` holds to "resize
`config/default`, nothing else". For example `deploy/namespace-scoped/kustomization.yaml`:

```yaml
resources:
- ../../config/default
- ../../config/rbac/namespace-scoped
patches:
# The ClusterRole and ClusterRoleBinding in config/default are the cluster-wide
# topology this overlay replaces.
- target:
    group: rbac.authorization.k8s.io
    kind: ClusterRole
    name: capb7-manager-role
  patch: |-
    $patch: delete
    apiVersion: rbac.authorization.k8s.io/v1
    kind: ClusterRole
    metadata:
      name: capb7-manager-role
- target:
    group: rbac.authorization.k8s.io
    kind: ClusterRoleBinding
    name: capb7-manager-rolebinding
  patch: |-
    $patch: delete
    apiVersion: rbac.authorization.k8s.io/v1
    kind: ClusterRoleBinding
    metadata:
      name: capb7-manager-rolebinding
# The manager's cache must cover exactly the namespaces bound above.
- target:
    kind: Deployment
    name: capb7-controller-manager
  patch: |-
    - op: add
      path: /spec/template/spec/containers/0/args/-
      value: --watch-namespaces=tenant-a,tenant-b
```

then:

```bash
kustomize build deploy/namespace-scoped | kubectl apply -f -
```

Everything else (the `ServiceAccount`, the metrics `ClusterRole`s and binding,
the Deployment with its pinned image, the webhook, cert-manager and security
objects) comes from `config/default/` unchanged. The metrics `ClusterRole`s stay
cluster-scoped on purpose: they let the manager authenticate scrapes through
the apiserver and grant nothing on Beskar7 objects.

Do not list `../../config/rbac` as well: it ships the cluster-wide role and
binding again, and kustomize will error on the duplicate `capb7-manager-role`.

The Helm chart automates all of this via `.Values.watchNamespaces`; see
`charts/beskar7/values.yaml` and `charts/beskar7/templates/rbac.yaml` for
the templated equivalent.

## Why there is no `namespace:`

A kustomize `namespace:` field rewrites the namespace of every namespaced object
it covers. Set here, or in the overlay that references this directory, it would
move each watch `Role` and `RoleBinding` out of the namespace it was written
for and into the one the field names: the manager would hold no permission in
the namespaces it watches and fail closed, and with two watched namespaces the
build would stop on the duplicate `Role`. So nothing in this directory or in the
install overlay above sets one. Every object names its namespace itself:
`capb7-system` for the leader-election pair and for every `RoleBinding` subject
(the manager `ServiceAccount` lives there), the watched namespace for each watch
`Role` and `RoleBinding`. `config/rbac/callback-only/` is built the same way.

If the manager runs in a namespace other than `capb7-system`, change it in
`leader-election-role.yaml` and in the `subjects` of the template (and of every
copy you made).

## Why kustomize can't auto-generate per-namespace Roles

Helm's templating supports `{{- range $ns := .Values.watchNamespaces }}` to
emit one Role per namespace. Kustomize has no loop construct — it operates
on a fixed set of YAML files plus patches. The manual copy-template-per-
namespace step is the kustomize-idiomatic way to express this.

For very large numbers of watched namespaces, a generator script wrapping
kustomize (or migrating to Helm) is more ergonomic.
