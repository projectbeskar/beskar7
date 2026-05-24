# RBAC Hardening

> **Audience:** Operators

This page documents the RBAC topology Beskar7 ships with, the rationale behind each rule, and how to tighten the cluster-wide default to per-namespace scope when multi-tenant isolation matters.

The deployed roles are generated from kubebuilder markers on the controllers in `controllers/*.go` plus a few markers in `cmd/manager/main.go`. The resulting YAML is `config/rbac/role.yaml` (kustomize) and `charts/beskar7/templates/rbac.yaml` (Helm).

## Two RBAC topologies

| Topology | When applied | Resource shape |
|---|---|---|
| **Cluster-wide** (default) | `watchNamespaces` is empty | 1 `ClusterRole` + 1 `ClusterRoleBinding` covering all rules. Historical behavior, kept for compatibility. |
| **Namespace-scoped** (SEC-2) | `watchNamespaces` is a non-empty list | 1 minimal `ClusterRole` + 1 `ClusterRoleBinding` (residual cluster-scoped reads only) + 1 `Role` + 1 `RoleBinding` in the operator's namespace (leader-election) + 1 `Role` + 1 `RoleBinding` in each watched namespace (the actual reconcile permissions). |

The namespace-scoped topology eliminates the cluster-wide `Secret` and `ConfigMap` access that an attacker reaching the controller's ServiceAccount could otherwise abuse. It is the recommended posture for any deployment where the manager runs alongside workloads from tenants other than the operator team.

The default cluster-wide topology stays in place for two reasons: backward compatibility for existing installs, and ease of getting started in single-tenant clusters where the tightening adds no real security but does add operational overhead (one extra Role per watched namespace).

---

## Cluster-wide topology (default)

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: manager-role
rules:

# ConfigMaps: inspection-result handoff (D-005). list+watch are required
# so the controller-runtime cache can populate the ConfigMap informer used
# by the inspection handler's CreateOrUpdate path; the handler also
# pre-warms the informer at SetupCallbackServer time so the first POST
# does not stall waiting for an initial sync.
- apiGroups: [""]
  resources: ["configmaps"]
  verbs: ["create", "delete", "get", "list", "patch", "update", "watch"]

# Events: emitted by the controllers for major lifecycle changes.
- apiGroups: [""]
  resources: ["events"]
  verbs: ["create", "patch"]

# Secrets: get for BMC credentials, bootstrap data, and CA bundles (all by name);
# create/update/patch/delete for the per-host bootstrap-token Secret. list/watch
# are required because PhysicalHostReconciler.SetupWithManager registers a
# Watches(&Secret{}, ...) informer for credential rotation.
- apiGroups: [""]
  resources: ["secrets"]
  verbs: ["create", "delete", "get", "list", "patch", "update", "watch"]

# Cluster API: read-only on Cluster and Machine for owner-walks and endpoint
# derivation.
- apiGroups: ["cluster.x-k8s.io"]
  resources: ["clusters", "clusters/status", "machines", "machines/status"]
  verbs: ["get", "list", "watch"]

# Leases: leader election.
- apiGroups: ["coordination.k8s.io"]
  resources: ["leases"]
  verbs: ["create", "delete", "get", "list", "patch", "update", "watch"]

# Beskar7 CRDs: full management.
- apiGroups: ["infrastructure.cluster.x-k8s.io"]
  resources: ["beskar7clusters", "beskar7machines", "physicalhosts"]
  verbs: ["create", "delete", "get", "list", "patch", "update", "watch"]
- apiGroups: ["infrastructure.cluster.x-k8s.io"]
  resources: ["beskar7clusters/finalizers", "beskar7machines/finalizers", "physicalhosts/finalizers"]
  verbs: ["update"]
- apiGroups: ["infrastructure.cluster.x-k8s.io"]
  resources: ["beskar7clusters/status", "beskar7machines/status", "physicalhosts/status"]
  verbs: ["get", "patch", "update"]

# Beskar7MachineTemplate: read-only — there is no template controller, just CAPI
# walks via shared informer cache.
- apiGroups: ["infrastructure.cluster.x-k8s.io"]
  resources: ["beskar7machinetemplates"]
  verbs: ["get", "list", "watch"]

# RBAC introspection: required for the controller to read its own ClusterRole at
# startup (logged for diagnostics). Auto-generated from a kubebuilder marker in
# cmd/manager/main.go.
- apiGroups: ["rbac.authorization.k8s.io"]
  resources: ["clusterrolebindings", "clusterroles"]
  verbs: ["get", "list", "watch"]
```

The Helm chart variant is identical apart from name templating; the chart does not relax any rule.

---

## Namespace-scoped topology (SEC-2)

When `watchNamespaces` is set, the chart and kustomize variants both generate three pieces:

1. **Minimal `ClusterRole` + `ClusterRoleBinding`** — only the `rbac.authorization.k8s.io/clusterroles, clusterrolebindings` reads, which are the residual cluster-scoped permissions the manager has via a kubebuilder marker. Everything else is removed from cluster scope.
2. **Leader-election `Role` + `RoleBinding`** in the operator's own namespace (`beskar7-system` by default) covering `coordination.k8s.io/leases` (the leader-election lease) and operator-side `events` creation. The lease lives where the operator runs, regardless of which namespaces it watches.
3. **Watch `Role` + `RoleBinding`** in each listed namespace covering everything the controller needs to reconcile a Beskar7 CR there: `Secrets`, `ConfigMaps`, `Events`, the Beskar7 CRDs, the CAPI `Machine` / `Cluster` reads.

The manager's `--watch-namespaces` flag must list the same namespaces — otherwise the controller-runtime cache will try to watch namespaces it has no RBAC for and the manager will fail at startup.

### Activate via Helm

Set `watchNamespaces` in your values:

```yaml
watchNamespaces:
  - default
  - tenant-a
  - tenant-b
```

The chart automatically:

- Emits the three RBAC pieces described above.
- Adds `--watch-namespaces=default,tenant-a,tenant-b` to the manager Deployment args.

`helm upgrade` is enough; no manual RBAC editing.

### Activate via kustomize

The kustomize equivalent is the `config/rbac/namespace-scoped/` overlay (SEC-2 PR 3). Because kustomize has no templating loop, the per-namespace `Role` + `RoleBinding` is supplied as a **template** that operators copy once per watched namespace:

1. **Swap the RBAC base.** In your top-level overlay, reference `../rbac/namespace-scoped` instead of `../rbac`. List the shared bits (`service_account.yaml`, `metrics_auth_*`, `metrics_reader_role.yaml`) individually because the namespace-scoped dir doesn't bundle them:

    ```yaml
    namespace: beskar7-system
    resources:
    - ../../rbac/namespace-scoped
    - ../../rbac/service_account.yaml
    - ../../rbac/metrics_auth_role.yaml
    - ../../rbac/metrics_auth_role_binding.yaml
    - ../../rbac/metrics_reader_role.yaml
    - ../../manager
    - ../../crd
    - ../../webhook
    - ../../certmanager
    - ../../security
    ```

2. **Generate one watch `Role` per watched namespace.** Copy `config/rbac/namespace-scoped/watch-role.template.yaml` to `watch-role.<namespace>.yaml`, patch both `namespace:` fields, and append the filename to `config/rbac/namespace-scoped/kustomization.yaml`'s `resources:` list.

3. **Add the manager flag.** Patch the manager Deployment to include `--watch-namespaces=<csv>`:

    ```yaml
    patches:
    - target:
        kind: Deployment
        name: controller-manager
      patch: |-
        - op: add
          path: /spec/template/spec/containers/0/args/-
          value: --watch-namespaces=default,tenant-a,tenant-b
    ```

See `config/rbac/namespace-scoped/README.md` for the worked example.

### Migration from cluster-wide → namespace-scoped

Backward-compatible in-place migration:

1. Apply the namespace-scoped RBAC alongside the existing cluster-wide RBAC (i.e. don't delete `manager-role` / `manager-rolebinding` yet). The controller's ServiceAccount is now bound by both — no permission is lost.
2. Set `--watch-namespaces=<csv>` on the manager Deployment. The cache scopes to those namespaces; Beskar7 CRs elsewhere stop being reconciled.
3. Verify reconciles in the watched namespaces still work. Look for `Scoping informers to namespaces` in the manager log.
4. Delete the old cluster-wide `ClusterRole` (`manager-role`) and `ClusterRoleBinding` (`manager-rolebinding`).

The manager pod does not need to be restarted between steps 2 and 4 — Kubernetes RBAC evaluations are stateless per-request.

---

## What is intentionally absent (both topologies)

- No `*` apiGroups, resources, or verbs.
- No `secrets: create/update/patch/delete` cluster-wide. The controller writes Secrets only via `controllerutil.CreateOrUpdate` on a deterministic name (`<host>-bootstrap-token`) in the host's namespace.
- No write access to `cluster.x-k8s.io` resources. Beskar7 only reads CAPI Cluster and Machine.
- No `nodes`, `pods`, `services`, `serviceaccounts`, `roles`, or `rolebindings`. The controller does not need them.
- No `impersonate` verb on any resource.

## Per-controller breakdown

| Controller | What it accesses | Why |
|---|---|---|
| `PhysicalHostReconciler` | `physicalhosts`, `physicalhosts/status`, `physicalhosts/finalizers`, `secrets` (get + list/watch via informer), `configmaps` (get + create/delete/patch/update + list/watch via informer), `events` | Manage the host's lifecycle, fetch BMC credentials, consume the inspection-result ConfigMap (handoff from the inspection HTTP handler). |
| `Beskar7MachineReconciler` | `beskar7machines`, `beskar7machines/status`, `beskar7machines/finalizers`, `physicalhosts` (get + patch), `secrets` (get + create/update/patch/delete), `machines` / `machines/status` (read), `cluster.x-k8s.io` resources (read) | Claim a host, read bootstrap data, mint per-host token Secret, walk to owner Machine. |
| `Beskar7ClusterReconciler` | `beskar7clusters`, `beskar7clusters/status`, `beskar7clusters/finalizers`, `machines` (read), `physicalhosts` (read for failure-domain discovery) | Derive the control-plane endpoint and failure domains. |

## Residual cluster-wide scope

In the **cluster-wide topology** (default), two resources have cluster-wide `list, watch`:

- **Secrets**: required by the controller-runtime informer registered by `PhysicalHostReconciler` to trigger reconciles on credential rotation. The data path of every controller fetches Secrets by name only; the cluster-wide scope is for the watch only.
- **ConfigMaps**: required by the controller-runtime cache to populate the informer used by the inspection HTTP handler's `CreateOrUpdate` of the per-host inspection-result ConfigMap. Without `list, watch` the reflector loops on `configmaps is forbidden` and the first POST stalls waiting for an initial sync.

In the **namespace-scoped topology**, both scopes are tightened to per-namespace. The only residual cluster-scoped reads are `rbac.authorization.k8s.io/clusterroles, clusterrolebindings`, kept for the kubebuilder-marker-generated rule in `cmd/manager/main.go` (the controller does not actually consult these resources at runtime; removing the marker is tracked separately).

The SEC-2 closure plan (now landed across PRs #80, #82, #83, and this one): make tightening to per-namespace scope an opt-in via `watchNamespaces`, retain cluster-wide as the default for backward compatibility. See `.claude/context/PROJECT_CONTEXT.md`.

## Verification

After install, confirm the deployed RBAC matches your chosen topology.

**Cluster-wide topology (default):**

```bash
# Helm install:
kubectl get clusterrole -l app.kubernetes.io/name=beskar7 -o yaml

# Kustomize install:
kubectl get clusterrole manager-role -o yaml
kubectl get clusterrolebinding manager-rolebinding -o yaml
```

**Namespace-scoped topology:**

```bash
# Cluster-scoped piece (residual reads only):
kubectl get clusterrole -l app.kubernetes.io/name=beskar7
# Should show one ClusterRole ending in -clusterscope-role

# Leader-election Role in operator namespace:
kubectl -n beskar7-system get role,rolebinding

# Per-watched-namespace Roles:
for ns in default tenant-a tenant-b; do
  echo "--- $ns ---"
  kubectl -n "$ns" get role,rolebinding
done
```

Confirm no wildcards or unexpected verbs anywhere:

```bash
kubectl get clusterrole,role -A -o json \
  | jq -r '.items[].rules[]? | select(any(.apiGroups[]? + " / " + .resources[]? + " / " + .verbs[]?; test("\\*|impersonate"))) | input_filename' \
  | sort -u
```

That command should return nothing.

Test what the controller can actually do as its ServiceAccount:

```bash
kubectl auth can-i --list \
  --as=system:serviceaccount:beskar7-system:beskar7-controller-manager
```

For the namespace-scoped topology, also check per-namespace permissions:

```bash
kubectl auth can-i list secrets \
  -n tenant-a \
  --as=system:serviceaccount:beskar7-system:beskar7-controller-manager
# Expect: yes

kubectl auth can-i list secrets \
  -n some-other-namespace \
  --as=system:serviceaccount:beskar7-system:beskar7-controller-manager
# Expect: no
```

## Customising

To grant additional access:

1. Edit `charts/beskar7/templates/rbac.yaml` (Helm) or `config/rbac/role.yaml` / `config/rbac/namespace-scoped/*.yaml` (kustomize) directly.
2. Re-render and apply.
3. Add a kubebuilder marker on the controller that needs the access, so `make manifests` regenerates the YAML correctly on the next round-trip.

Do **not** add `*` rules. Reviewers will reject them; production operators will not deploy them.

## See also

- [Security](README.md)
- [Configuration](configuration.md)
- [Security Troubleshooting](troubleshooting.md)
- [Helm chart README](../../charts/beskar7/README.md) — `watchNamespaces` value
- [`config/rbac/namespace-scoped/README.md`](../../config/rbac/namespace-scoped/README.md) — kustomize workflow
