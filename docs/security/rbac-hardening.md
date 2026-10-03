# RBAC Hardening

> **Audience:** Operators

This page documents the RBAC topology Beskar7 ships with, the rationale behind each rule, and how to tighten the cluster-wide default to per-namespace scope when multi-tenant isolation matters.

The deployed roles are generated from kubebuilder markers on the controllers in `controllers/*.go` plus a few markers in `cmd/manager/main.go`. The resulting YAML is `config/rbac/role.yaml` (kustomize) and `charts/beskar7/templates/rbac.yaml` (Helm).

## Two RBAC topologies

| Topology | When applied | Resource shape |
|---|---|---|
| **Cluster-wide** (default) | `watchNamespaces` is empty | 1 `ClusterRole` + 1 `ClusterRoleBinding` covering all rules. Historical behavior, kept for compatibility. |
| **Namespace-scoped** (SEC-2) | `watchNamespaces` is a non-empty list | 1 `Role` + 1 `RoleBinding` in the operator's namespace (leader-election) + 1 `Role` + 1 `RoleBinding` in each watched namespace (the actual reconcile permissions). Nothing cluster-scoped for the manager. |

The namespace-scoped topology eliminates the cluster-wide `Secret` and `ConfigMap` access that an attacker reaching the controller's ServiceAccount could otherwise abuse. It is the recommended posture for any deployment where the manager runs alongside workloads from tenants other than the operator team.

The default cluster-wide topology stays in place for two reasons: backward compatibility for existing installs, and ease of getting started in single-tenant clusters where the tightening adds no real security but does add operational overhead (one extra Role per watched namespace).

---

## Cluster-wide topology (default)

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: capb7-manager-role
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
```

The Helm chart variant is identical apart from name templating; the chart does not relax any rule.

---

## Namespace-scoped topology (SEC-2)

When `watchNamespaces` is set, the chart and kustomize variants both generate two pieces, and nothing cluster-scoped for the manager:

1. **Leader-election `Role` + `RoleBinding`** in the operator's own namespace (`capb7-system` by default) covering `coordination.k8s.io/leases` (the leader-election lease) and operator-side `events` creation. The lease lives where the operator runs, regardless of which namespaces it watches.
2. **Watch `Role` + `RoleBinding`** in each listed namespace covering everything the controller needs to reconcile a Beskar7 CR there: `Secrets`, `ConfigMaps`, `Events`, the Beskar7 CRDs, the CAPI `Machine` / `Cluster` reads.

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

1. **Generate one watch `Role` per watched namespace.** Copy `config/rbac/namespace-scoped/watch-role.template.yaml` to `watch-role.<namespace>.yaml`, patch both `namespace:` fields, and append the filename to `config/rbac/namespace-scoped/kustomization.yaml`'s `resources:` list. Then check where everything landed:

    ```bash
    kustomize build config/rbac/namespace-scoped
    ```

    Each watch `Role` and `RoleBinding` must be in the namespace you wrote it for, the leader-election pair in `capb7-system`, and every `RoleBinding` subject must be the `capb7-manager` ServiceAccount in `capb7-system`.

2. **Install through an overlay of your own.** `config/default/` ships the cluster-wide `ClusterRole` and `ClusterRoleBinding` this overlay replaces, so layer on top of it, delete those two objects, add the namespace-scoped directory, and pass `--watch-namespaces` with the same list. Create the overlay outside `config/overlays/` (which `test/contract` holds to "resize `config/default`, nothing else"), for example as `deploy/namespace-scoped/kustomization.yaml`:

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
          value: --watch-namespaces=default,tenant-a,tenant-b
    ```

    ```bash
    kustomize build deploy/namespace-scoped | kubectl apply -f -
    ```

    Everything else (ServiceAccount, the Deployment with its pinned image, the metrics RBAC, webhook, cert-manager and security objects) comes from `config/default/` unchanged. The metrics `ClusterRole`s stay cluster-scoped on purpose: they let the manager authenticate scrapes through the apiserver and grant nothing on Beskar7 objects.

Do not set `namespace:` in this overlay or in the namespace-scoped `kustomization.yaml`. A kustomize `namespace:` field rewrites every namespaced object it covers, which would move each watch `Role` and `RoleBinding` out of its watched namespace and into the one the field names: the manager would hold nothing in the namespaces it watches, and the build would stop on the duplicate `Role` as soon as two namespaces are listed. Every object in the overlay names its own namespace for that reason. See `config/rbac/namespace-scoped/README.md` for the details.

### Migration from cluster-wide → namespace-scoped

Backward-compatible in-place migration:

1. Apply the namespace-scoped RBAC alongside the existing cluster-wide RBAC (i.e. don't delete `capb7-manager-role` / `capb7-manager-rolebinding` yet), with `kustomize build config/rbac/namespace-scoped | kubectl apply -f -` once you have generated the watch `Role`s as above. The controller's ServiceAccount is now bound by both — no permission is lost.
2. Set `--watch-namespaces=<csv>` on the manager Deployment. The cache scopes to those namespaces; Beskar7 CRs elsewhere stop being reconciled.
3. Verify reconciles in the watched namespaces still work. Look for `Scoping informers to namespaces` in the manager log.
4. Delete the old cluster-wide `ClusterRole` (`capb7-manager-role`) and `ClusterRoleBinding` (`capb7-manager-rolebinding`).

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
| `Beskar7ClusterReconciler` | `beskar7clusters`, `beskar7clusters/status`, `beskar7clusters/finalizers`, `clusters` / `clusters/status` (read; also watched, so a Cluster edit wakes the reconciler), `physicalhosts` (read for failure-domain discovery) | Mirror the control-plane endpoint already set on `Cluster` or `Beskar7Cluster` spec and discover failure domains. |

## Residual cluster-wide scope

In the **cluster-wide topology** (default), two resources have cluster-wide `list, watch`:

- **Secrets**: required by the controller-runtime informer registered by `PhysicalHostReconciler` to trigger reconciles on credential rotation. The data path of every controller fetches Secrets by name only; the cluster-wide scope is for the watch only.
- **ConfigMaps**: required by the controller-runtime cache to populate the informer used by the inspection HTTP handler's `CreateOrUpdate` of the per-host inspection-result ConfigMap. Without `list, watch` the reflector loops on `configmaps is forbidden` and the first POST stalls waiting for an initial sync.

In the **namespace-scoped topology**, both scopes are tightened to per-namespace, and the manager holds no cluster-scoped permission at all. (Earlier releases also granted cluster-wide read on `clusterroles` and `clusterrolebindings` from a leftover kubebuilder marker that nothing used; it is gone.)

The SEC-2 closure plan (now landed across PRs #80, #82, #83, and this one): make tightening to per-namespace scope an opt-in via `watchNamespaces`, retain cluster-wide as the default for backward compatibility. See `.claude/context/PROJECT_CONTEXT.md`.

## Verification

After install, confirm the deployed RBAC matches your chosen topology.

**Cluster-wide topology (default):**

```bash
# Helm install:
kubectl get clusterrole -l app.kubernetes.io/name=beskar7 -o yaml

# Kustomize install:
kubectl get clusterrole capb7-manager-role -o yaml
kubectl get clusterrolebinding capb7-manager-rolebinding -o yaml
```

**Namespace-scoped topology:**

```bash
# Cluster-scoped roles: only the metrics ones (Helm: -metrics-auth-role and
# -metrics-reader), none for the manager's reconcile permissions:
kubectl get clusterrole -l app.kubernetes.io/name=beskar7

# Leader-election Role in operator namespace:
kubectl -n capb7-system get role,rolebinding

# Per-watched-namespace Roles:
for ns in default tenant-a tenant-b; do
  echo "--- $ns ---"
  kubectl -n "$ns" get role,rolebinding
done
```

Confirm that no role bound to the manager's ServiceAccount carries a wildcard or `impersonate`. (Scanning every role in the cluster instead would always report Kubernetes' own `cluster-admin` and controller roles.)

```bash
kubectl get clusterrole,role,clusterrolebinding,rolebinding -A -o json | jq -r '
  .items as $all
  | [$all[] | select(.kind | endswith("Binding"))
            | select(any(.subjects[]?; .kind == "ServiceAccount" and .name == "capb7-manager"))
            | {kind: .roleRef.kind, name: .roleRef.name, ns: (.metadata.namespace // "")}] as $bound
  | $all[] | select(.kind == "Role" or .kind == "ClusterRole")
  | . as $r | select(any($bound[]; .kind == $r.kind and .name == $r.metadata.name and .ns == ($r.metadata.namespace // "")))
  | select(any(.rules[]?; any(.apiGroups[]?, .resources[]?, .verbs[]?; . == "*" or . == "impersonate")))
  | "\(.kind) \(.metadata.namespace // "-")/\(.metadata.name)"'
```

That command should return nothing.

Test what the controller can actually do as its ServiceAccount:

```bash
kubectl auth can-i --list \
  --as=system:serviceaccount:capb7-system:capb7-manager
```

For the namespace-scoped topology, also check per-namespace permissions:

```bash
kubectl auth can-i list secrets \
  -n tenant-a \
  --as=system:serviceaccount:capb7-system:capb7-manager
# Expect: yes

kubectl auth can-i list secrets \
  -n some-other-namespace \
  --as=system:serviceaccount:capb7-system:capb7-manager
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
