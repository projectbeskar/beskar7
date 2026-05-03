# RBAC Hardening

This page documents the ClusterRole that ships with Beskar7, the rationale behind each rule, and what to verify before exposing the controller to a multi-tenant or untrusted environment.

The deployed ClusterRole is generated from kubebuilder markers on the controllers in `controllers/*.go` plus a few markers in `cmd/manager/main.go`. The resulting YAML is `config/rbac/role.yaml` (kustomize) and `charts/beskar7/templates/rbac.yaml` (Helm).

## Default ClusterRole

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: manager-role
rules:

# ConfigMaps: only fetched by name (inspection-result handoff) and
# created/updated/deleted by the controllers. No informer; list/watch are
# intentionally omitted.
- apiGroups: [""]
  resources: ["configmaps"]
  verbs: ["create", "delete", "get", "patch", "update"]

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
# startup (logged for diagnostics).
- apiGroups: ["rbac.authorization.k8s.io"]
  resources: ["clusterrolebindings", "clusterroles"]
  verbs: ["get", "list", "watch"]
```

(The Helm chart variant is identical apart from name templating; the chart does not relax any rule.)

## What is intentionally absent

- No `*` apiGroups, resources, or verbs.
- No `secrets: create/update/patch/delete` cluster-wide. The controller writes Secrets only via `controllerutil.CreateOrUpdate` on a deterministic name (`<host>-bootstrap-token`) in the host's namespace.
- No write access to `cluster.x-k8s.io` resources. Beskar7 only reads CAPI Cluster and Machine.
- No `nodes`, `pods`, `services`, `serviceaccounts`, `roles`, or `rolebindings`. The controller does not need them.
- No `impersonate` verb on any resource.

## Per-controller breakdown

| Controller | What it accesses | Why |
|---|---|---|
| `PhysicalHostReconciler` | `physicalhosts`, `physicalhosts/status`, `physicalhosts/finalizers`, `secrets` (get + list/watch via informer), `configmaps` (get + create/delete/patch/update), `events` | Manage the host's lifecycle, fetch BMC credentials, consume the inspection-result ConfigMap. |
| `Beskar7MachineReconciler` | `beskar7machines`, `beskar7machines/status`, `beskar7machines/finalizers`, `physicalhosts` (get + patch), `secrets` (get + create/update/patch/delete), `machines` / `machines/status` (read), `cluster.x-k8s.io` resources (read) | Claim a host, read bootstrap data, mint per-host token Secret, walk to owner Machine. |
| `Beskar7ClusterReconciler` | `beskar7clusters`, `beskar7clusters/status`, `beskar7clusters/finalizers`, `machines` (read), `physicalhosts` (read for failure-domain discovery) | Derive the control-plane endpoint and failure domains. |

## Residual cluster-wide scope

The Secret `list, watch` verbs are cluster-wide, not namespace-scoped. This is required by the controller-runtime informer registered by `PhysicalHostReconciler` to trigger reconciles on credential rotation. The data path of every controller fetches Secrets by name only; the cluster-wide scope is for the watch only.

This residual scope is tracked as `SEC-2` (the partial closure decision is `D-007`) in `.claude/context/PROJECT_CONTEXT.md`. A label-selected partial cache is the planned v0.5 follow-up: change the informer to watch only Secrets carrying a Beskar7-owned label, narrowing the cache to BMC-credentials + bootstrap-token Secrets.

## Verification

After install, confirm the deployed ClusterRole:

```bash
# Helm install:
kubectl get clusterrole -l app.kubernetes.io/name=beskar7 -o yaml

# Kustomize install:
kubectl get clusterrole manager-role -o yaml
kubectl get clusterrolebinding manager-rolebinding -o yaml
```

Look for any wildcards or unexpected verbs:

```bash
kubectl get clusterrole manager-role -o jsonpath='{range .rules[*]}{.apiGroups}{" / "}{.resources}{" / "}{.verbs}{"\n"}{end}' | grep -E "\\*|impersonate"
```

That command should return nothing.

Test what the controller can do as its ServiceAccount:

```bash
kubectl auth can-i --list --as=system:serviceaccount:beskar7-system:beskar7-controller-manager
```

## Customising

There is no Helm value to relax or extend the ClusterRole at install time. To grant additional access:

1. Edit `charts/beskar7/templates/rbac.yaml` (Helm) or `config/rbac/role.yaml` (kustomize) directly.
2. Re-render and apply.
3. Add a kubebuilder marker on the controller that needs the access, so `make manifests` regenerates the YAML correctly on the next round-trip.

Do **not** add `*` rules. Reviewers will reject them; production operators will not deploy them.

## See also

- [Security](README.md)
- [Configuration](configuration.md)
- [Security Troubleshooting](troubleshooting.md)
