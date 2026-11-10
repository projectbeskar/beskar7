# PhysicalHostPool

A `PhysicalHostPool` lets a `Beskar7Machine` claim a `PhysicalHost` in another
namespace. Hosts and their BMC Secrets stay in an admin namespace; clusters in
other namespaces only name the pool. Without a pool nothing changes: a machine
claims hosts in its own namespace only.

Example: [`examples/physicalhost-pool.yaml`](../examples/physicalhost-pool.yaml).

## When it is on

Pools need nothing to turn on. They work in the cluster-wide install, and are
off by themselves with `--watch-namespaces` or `--controllers=none`: pools and
namespaces are cluster-scoped, and neither mode may read them. When off, a
machine with `poolRef` waits with `PoolNotAvailable`. What the install adds:

| Object | kustomize | Helm (not with `watchNamespaces`) |
|---|---|---|
| ClusterRole: read pools and namespaces, finalize pools, write pool status | `config/rbac/host_pool_role.yaml` | `rbac-host-pools.yaml` |
| ValidatingAdmissionPolicy guarding pool claims | `config/admission/pool_claim_guard.yaml` | `admission-pool-claim-guard.yaml` |

## The pieces

| Piece | Where | Written by |
|---|---|---|
| Pool: `hostNamespaces`, `namespaceSelector` | `PhysicalHostPool` (cluster-scoped) | admin |
| Host is in pool `<name>` | label `pool.beskar7.infrastructure.cluster.x-k8s.io/<name>` on the `PhysicalHost`, any value | admin |
| Machine wants pool `<name>` | `Beskar7Machine.spec.poolRef` (cannot be added, changed or removed) | cluster owner, usually through the template |
| Failure domains from pools | `Beskar7Cluster.spec.poolRefs` | cluster owner, usually through the ClusterClass |
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
- only `Available`, unclaimed members with `cleaningMode: Metadata` are claimed;
  `hostSelector` and the failure domain still apply
- no fallback to the machine's own namespace or to other pools

**Claim validity (SEC-12).** A `consumerRef` in another namespace than the
host's is valid only while the host is a member of the pool in `consumerPool`
and that pool grants the consumer's namespace. Membership in any other pool does
not count. The callback server checks this on every request; an invalid claim is
treated like no claim at all, so the host gets no bootstrap data. The host's
callback credentials are also bound to the machine's namespace, not only its
name.

**Guard.** While a host is claimed through a pool, the admission policy denies
removing that pool's label from it and changing `spec.consumerPool`. Labels of
other pools stay editable. A release (clearing `consumerRef`) may drop the label
in the same write.

**Revoked grant.** A change to the pool (`hostNamespaces`, `namespaceSelector`)
or to the namespace's labels cannot be blocked. The claim becomes invalid, the
host stays claimed and the node keeps running, nothing is released
automatically. It shows as:

- `PoolClaimValid=False`, reason `PoolGrantRevoked`, on the `Beskar7Machine`
  (not part of its `Ready` summary)
- `ClaimsValid=False`, reason `PoolGrantRevoked`, on the pool, naming the hosts

The admin decides: restore the grant, or delete the machine to release the host.

**Hardware mismatch.** When the inspection report does not meet
`hardwareRequirements`, the machine is terminally failed as before. A host
claimed through a pool is also released (forced off and wiped), so a wrong guess
does not hold shared capacity.

**Release.** Deleting the machine clears `consumerRef` and `consumerPool`
together. The host goes through cleaning and is `Available` again. A machine
finds its host for release even after its pool changed, its grant was revoked,
or the manager was moved to `--watch-namespaces`.

**Deleting a pool** waits (finalizer) until no host has `consumerPool` naming it.
A deleting pool takes no new claims; existing claims stay valid.

**Failure domains.** List the pools in `Beskar7Cluster.spec.poolRefs`; the zones
of their members become failure domains. Pools that do not grant the cluster's
namespace add nothing.

## Not supported

- `clusterctl move` of a cluster that claims through a pool. Pool hosts are not
  in the cluster's namespace, so they do not move; the manager logs a warning
  when a moved machine holds a pool host.
