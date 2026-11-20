# PhysicalHostPool

A `PhysicalHostPool` lets a `Beskar7Machine` claim a `PhysicalHost` in another
namespace. Hosts and their BMC Secrets stay in an admin namespace; clusters in
other namespaces only name the pool. Without a pool nothing changes: a machine
claims hosts in its own namespace only.

Example: [`examples/physicalhost-pool.yaml`](../examples/physicalhost-pool.yaml).

## Turning it on

Pools are off by default. Start the manager with `--enable-host-pools`. Pools
and namespaces are cluster-scoped, so pools also need one ClusterRole next to
the manager's role. It is installed only when pools are on:

| | kustomize | Helm |
|---|---|---|
| Flag and ClusterRole (read pools and namespaces, finalize pools, write pool status) | add the component `config/host-pools` to your install overlay | `hostPools.enabled: true` (`rbac-host-pools.yaml`, with or without `watchNamespaces`) |

```yaml
# kustomize install overlay
resources:
- ../../config/default
components:
- ../../config/host-pools
```

Pools work in the cluster-wide install and in the namespace-scoped install
(`--watch-namespaces`). Without the flag the manager does not watch pools or
namespaces, a machine with `poolRef` waits with `PoolNotAvailable`, and a claim
in another namespace is never valid. A callback-only instance
(`--controllers=none`) rejects the flag: its Role cannot read pools or
namespaces, so it refuses the callbacks of a host claimed from another
namespace, and pool hosts must reach the callback server of a manager that runs
the controllers.

### Namespace-scoped install

The manager only sees `PhysicalHost`s, `Beskar7Machine`s and their Secrets in
the namespaces it watches. For a pool, it has to watch both sides:

- every namespace in the pool's `hostNamespaces`, where the hosts, their BMC
  Secrets, bootstrap-token Secrets and inspection ConfigMaps live
- every namespace whose machines claim through the pool

List all of them in `--watch-namespaces` (Helm: `watchNamespaces`), and give
each one a watch Role (the chart does this; with kustomize, copy
`watch-role.template.yaml` per namespace, see
[`config/rbac/namespace-scoped/README.md`](../config/rbac/namespace-scoped/README.md)).
If a pool names a host namespace that is not watched, the manager cannot list
it: claims through that pool fail with `PhysicalHostAssociationFailed` and an
error that names the namespace, and the pool's status is not updated. A tenant
namespace that is not watched is never reconciled. Pools and
namespaces themselves are read cluster-wide through the ClusterRole above.

## What a pool does not protect

A pool shares hardware between namespaces that trust each other. It is not an
isolation boundary between tenants:

- the wipe on release (`spec.cleaning.mode: Metadata`) removes partition tables
  and signatures, but the data blocks stay, so a later claim can read an
  earlier tenant's disks
- the claiming namespace chooses what boots (`inspectionImageURL`, the target
  image) and gets in-band access to the BMC, so changes to firmware or the BMC
  can survive a release

Write access to a namespace in `hostNamespaces` must be admin-only. Whoever can
create `PhysicalHost`s and Secrets there controls the host side of every claim.

## The pieces

| Piece | Where | Written by |
|---|---|---|
| Pool: `hostNamespaces`, `namespaceSelector` | `PhysicalHostPool` (cluster-scoped) | admin |
| Host is in pool `<name>` | label `pool.beskar7.infrastructure.cluster.x-k8s.io/<name>` on the `PhysicalHost`, any value | admin |
| Machine wants pool `<name>` | `Beskar7Machine.spec.poolRef` (cannot be added, changed or removed) | cluster owner, usually through the template |
| The pool a claim went through | `PhysicalHost.spec.consumerPool` | controller, in the same update as `consumerRef` |

A host can carry several pool labels. A machine names one pool.

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
kind: PhysicalHostPool
metadata:
  name: team-a-gpu
spec:
  hostNamespaces: [beskar7-hosts]
  namespaceSelector:
    matchLabels:
      tenant: team-a
```

```console
$ kubectl get php
NAME         MEMBERS   AVAILABLE   CLAIMED   AGE
team-a-gpu   2         1           1         3d
```

## Rules

**Membership.** A host is a member of a pool when it has the pool label AND its
namespace is in the pool's `hostNamespaces`. A pool label on a host in any other
namespace does nothing.

**Claim.** For a machine with `poolRef`:

- the pool must exist, not be deleting, and its `namespaceSelector` must match
  the machine's namespace; otherwise `PhysicalHostAssociated=False` with reason
  `PoolNotAvailable` (the same reason for all cases), retried every minute and
  on any pool or namespace label change
- only `Available`, unclaimed members whose last release ended in a wipe
  (`HostCleaned=True`) are claimed, since the claim before may have come from
  another namespace. A member with `spec.cleaning.mode: Metadata` that is
  `Available` but was never wiped (the mode was set while it was `Available`, or
  it became `Available` before the wipe existed) is wiped once first. While
  members are skipped for this, the machine's wait message says how many
- `hostSelector` and the failure domain (`Machine.spec.failureDomain`) still
  apply. Failure domains come from the hosts in the cluster's own namespace;
  pools do not add any yet
- `hardwareRequirements.matchAt: Claim` never matches a pool member. The
  inspection report comes from the claim run's inspector, which the claimant
  chooses, so a member's report is dropped when its wipe starts, and a released
  member has none. Use `hostSelector` labels to steer pool claims
- no fallback to the machine's own namespace or to other pools
- once the machine has a `providerID` it never claims a second host: if the
  host it names is gone, it waits with `WaitingForPhysicalHost`

**Claim validity (SEC-12).** A `consumerRef` in another namespace than the
host's is valid only while all of these hold:

- the host is a member of the pool in `consumerPool` (membership in any other
  pool does not count)
- that pool grants the consumer's namespace
- the machine the claim names proves it: its `poolRef` is `consumerPool`, and
  either its UID is the one on the claim, or its `providerID` names this host

The host side alone is written by whoever writes the host namespace, so the
machine side is what keeps a forged claim from reaching a machine's bootstrap
data: nobody outside the tenant namespace can read the machine's UID or write
its `providerID`. The host's callback credentials are bound to the machine's
namespace, name and UID.

This is checked on every request, not only at claim time, and an invalid claim
is treated like no claim at all, so the host gets no bootstrap data. The
callback server reads pools, namespace labels and machines through the
manager's informer cache, so a change applies once the watch has caught up,
usually within a second. A read that fails keeps a signed report for the next
try instead of dropping it.

There is no admission guard on the pool label or on `consumerPool`: whoever can
edit a host can also add pool labels to it, so the live check is what protects
the claim.

**Revoked grant.** Removing the host's pool label, or changing the pool
(`hostNamespaces`, `namespaceSelector`) or the namespace's labels so the pool no
longer grants it, makes the claim invalid. Nothing is released or powered off:
the host stays claimed and the node keeps running. Nothing new starts for that
claim either: the machine does not boot the host again and does not move it to
the next provisioning step, and the callback server refuses its bootstrap,
network data and status callbacks. It shows as:

- `PoolClaimValid=False`, reason `PoolGrantRevoked`, on the `Beskar7Machine`
  (not part of its `Ready` summary)
- `ClaimsValid=False`, reason `PoolGrantRevoked`, on the pool, naming the hosts

The admin decides: restore the grant, or delete the machine to release the host.
A claim that was not in a run carries on where it stopped. A run in progress
(the inspector booting, inspecting or writing the image) does not: its reports
are refused while the grant is revoked, the inspector retries a few times and
then stops, so the run ends with its inspection or deployment timeout once the
grant is back, and the machine fails. Delete it to get a new one.

A machine in another namespace never shows the host's `status.errorMessage`
(BMC errors can name the BMC address or its credentials Secret). It shows a
generic message, and the details stay on the `PhysicalHost`. A failure reported
by the machine's own run and an inspection timeout are shown as usual.

**Hardware mismatch.** When the inspection report does not meet
`hardwareRequirements`, the machine is terminally failed as before. A host
claimed through a pool is also released (forced off and wiped), so a wrong guess
does not hold shared capacity.

**Release.** Deleting the machine clears `consumerRef` and `consumerPool`
together. The host goes through cleaning and is `Available` again. A machine
finds its host for release even after its pool changed, its grant was revoked
or pools were turned off. Release proves the claim the same way (UID or
`providerID`), so a forged claim naming the machine cannot take the place of its
real host.

**Deleting a pool** waits (finalizer) until no host in its `hostNamespaces` is
claimed through it. A `consumerPool` left on a host without a `consumerRef` (for
example after a dangling `consumerRef` was cleared by hand) does not count. A
deleting pool takes no new claims; existing claims stay valid. The pool
reconciler honours the `cluster.x-k8s.io/paused` annotation.

## Who may claim: lock down namespace labels

`namespaceSelector` matches namespace labels, so a pool is only as strong as the
right to label namespaces. If a cluster owner can set labels on their own
namespace, they can match any pool's selector and claim its hosts. The platform
has to keep that right away from cluster owners, for example:

- do not grant `update` or `patch` on `namespaces` to cluster owners
- or use a policy engine or a multi-tenancy tool that controls namespace labels,
  and pick selector labels only it may set
- `kubernetes.io/metadata.name` is set by the API server and cannot be changed,
  so `matchExpressions` on it is a safe way to name exact namespaces

Beskar7 only knows namespaces, not tenants. Any tenancy on top is up to the
platform.

## clusterctl move

`clusterctl move` moves one namespace at a time. `PhysicalHost` carries
`clusterctl.cluster.x-k8s.io/move-hierarchy`, so a move of the hosts' namespace
takes every host with its bootstrap-token Secret and inspection ConfigMap. A move
of a tenant namespace takes its `Cluster`, `Beskar7Cluster` and
`Beskar7Machine`s, but not the pool hosts, which live in another namespace. The
`PhysicalHostPool` is cluster-scoped and is never moved.

What happens to a cross-namespace `consumerRef`:

- On the source, the moved `Beskar7Machine` is deleted with clusterctl's
  delete-for-move annotation, so its host is neither powered off nor released.
  The manager logs that the host is in another namespace and was not moved.
- The host's `consumerRef` still names the machine by namespace and name, with
  the old UID. On the target, the recreated machine has a new UID, but its
  `spec.providerID` (`b7://<host namespace>/<host>`) still names the host. A pool
  machine adopts a claim that names it when its UID matches or when its
  `providerID` names that host. The host admin writes the claim and the tenant
  writes the `providerID`, so neither side can point a host at a machine alone.
- Until the host is on the target too, the machine waits with
  `PhysicalHostAssociated=False`, `WaitingForPhysicalHost`, and never claims a
  second host, since it already has a `providerID`. The node keeps running.
- The claim is valid on the target only if the pool exists there and grants the
  tenant namespace there. Otherwise it is a revoked grant (see above): the node
  keeps running and nothing new starts.

What the operator does, in this order:

1. On the target, start the manager with `--enable-host-pools`, and create the
   tenant namespaces with the labels the pools' `namespaceSelector`s match.
   `clusterctl move` creates a missing namespace without labels, and then no
   pool grants it.
2. On the target, create the `PhysicalHostPool`s (`kubectl get php <name> -o
   yaml` on the source, without `status`, `uid` and `resourceVersion`).
3. Label the BMC credentials Secrets in the host namespace with
   `clusterctl.cluster.x-k8s.io/move=""`, or create them on the target first
   (see [PhysicalHost](physicalhost.md)).
4. On the source, pause every `Cluster` that has a claim on any host in the host
   namespace (`spec.paused: true`). Otherwise the tenant clusters keep running
   there while their hosts disappear: a machine still provisioning goes back to
   claiming, and health checks may act on the rest.
5. `clusterctl move -n <host namespace>`. It takes every host in the namespace,
   not only the pools' members.
6. `clusterctl move -n <tenant namespace>` for every namespace with a claim on
   any host in the moved namespace, through a pool or not. A host can only be on
   one management cluster, so every tenant that claims from it moves with it.
7. Check that every `Cluster` paused in step 4 runs again on the target, and
   unpause any that does not.

After step 7, every moved machine finds its host by `providerID`, and
`PoolClaimValid` and the pools' `ClaimsValid` are `True` again. A machine that
was moved before it had a `providerID` cannot prove its claim (its UID changed),
which is one more reason to pause first. The source pools then have no claims
left and can be deleted.

If you move only a tenant namespace and leave the hosts on the source, the
machines on the target wait with `WaitingForPhysicalHost`. On the source, the
hosts stay claimed by machines that no longer exist there, so they are not
`Available` and the pool cannot be deleted. Move the host namespace too, or move
the tenant back.
