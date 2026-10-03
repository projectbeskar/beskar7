# PhysicalHost

> **Audience:** Operators

`PhysicalHost` is the Beskar7 CRD that represents one bare-metal server and its BMC connection. The reconciler that owns it is `controllers/physicalhost_controller.go`.

For the full field reference, see [API Reference: PhysicalHost](api-reference.md#physicalhost). This page covers operational behavior — when fields move, what conditions mean, how the lifecycle works.

## Identity

- **API:** `infrastructure.cluster.x-k8s.io/v1beta2`
- **Kind:** `PhysicalHost`
- **Short name:** `ph`
- **Scope:** Namespaced
- **Categories:** `cluster-api` (`kubectl get cluster-api` lists hosts)
- **clusterctl:** the CRD carries `clusterctl.cluster.x-k8s.io`, so `clusterctl move` discovers hosts, and `clusterctl.cluster.x-k8s.io/move-hierarchy`, so every host in the namespace is moved together with the Secret and ConfigMap it owns — nothing owns a host, so without it a move would leave all hosts behind. The BMC credentials Secret is yours, not the host's: label it `clusterctl.cluster.x-k8s.io/move=""` or create it on the target first, with its [address annotations](#binding-the-credentials-to-their-bmc) (see [Installation](installation.md#clusterctl-move)).

## Spec at a glance

```yaml
spec:
  redfishConnection:
    address: "https://192.168.1.100"
    credentialsSecretRef: "bmc-credentials"
    insecureSkipVerify: false        # default
    # caBundleSecretRef: bmc-ca-bundle  # optional, mutually exclusive with insecureSkipVerify=true
  # consumerRef is set by the Beskar7Machine controller; do not set manually
```

The credentials Secret must contain `username` and `password` keys (Opaque). It must live in the same namespace as the PhysicalHost, and it must say which BMCs its credentials may be sent to — see the next section.

### Binding the credentials to their BMC

Anyone allowed to edit a `PhysicalHost` can change its `address`. Without a binding, pointing a host at an endpoint you control, with `insecureSkipVerify: true`, and naming any Secret in the namespace as its `credentialsSecretRef` made the controller send that Secret's username and password to you on its first Redfish request (SEC-16). So the Secret decides where its credentials go (decisions D-030, D-033), through annotations on the **Secret**, never on the host:

| Annotation (on the credentials Secret) | Required | Meaning |
|---|---|---|
| `beskar7.infrastructure.cluster.x-k8s.io/bmc-addresses` | always | The BMC addresses these credentials may be sent to, separated by commas and/or whitespace. |
| `beskar7.infrastructure.cluster.x-k8s.io/bmc-insecure-transport` | for an `http://` address or `insecureSkipVerify: true` | Must be exactly `"true"`. Allows sending the credentials over a connection that does not verify the BMC. |
| `beskar7.infrastructure.cluster.x-k8s.io/bmc-ca-secret` | for a host with `caBundleSecretRef` | The name of the Secret, in the same namespace, holding the CA these BMCs present. A host's `caBundleSecretRef` must be exactly this name. |

The host of `redfishConnection.address` (the part between `://` and the port) is matched against `bmc-addresses` entries:

| Entry | Matches | Does not match |
|---|---|---|
| `10.0.0.5`, `2001:db8::1` | that IP address | anything else |
| `10.0.0.0/24`, `2001:db8::/32` | IP addresses in the range | hostnames, even `10.0.0.5.nip.io` |
| `bmc01.example.com` | that hostname, case-insensitively | subdomains |
| `*.bmc.example.com` | one or more labels under the suffix: `r1.bmc.example.com`, `a.r1.bmc.example.com` | `bmc.example.com` itself, `xbmc.example.com`, IP addresses |

Matching is literal: a hostname is matched as written, and a CIDR never matches a hostname. A listed name is resolved only when the manager connects, and then as an absolute DNS name, without the pod's search path (decision D-032): `bmc1.lab` is looked up as `bmc1.lab.`, never first as `bmc1.lab.<namespace>.svc.cluster.local`, where a Service named `bmc1` in a namespace called `lab` would otherwise answer. An in-cluster BMC is therefore written fully qualified — `mock-redfish.my-namespace.svc.cluster.local`, with your cluster's domain — in both the host's `address` and `bmc-addresses`; a short in-cluster name fails its DNS lookup (`BMCUnreachable`). List IP addresses where you can, and otherwise fully-qualified names under a domain you control. CIDRs wider than /8 (IPv4) or /32 (IPv6), and wildcards over a public suffix (`*.com`, `*.co.uk`, `*.lab`, `*.svc`), are rejected. The address must be an `http://` or `https://` URL with a host, no `user:password@` part, and no query (`?`) or fragment (`#`). **Any malformed entry authorises no address at all**, so a typo shows up as a refusal instead of silently widening or narrowing the list.

The credentials go only to the address's origin — its scheme, host and port. A redirect from the BMC to anywhere else (another port, another host or a subdomain, or `http://` on the same host), or a link in a BMC response that names another host, is refused before anything is sent, and the call fails with `RedfishConnectionFailed` or `RedfishQueryFailed`.

When the Secret does not authorise the host's address, the controller makes no Redfish request: the host reports `RedfishConnectionReady = False (CredentialsNotAuthorized)` with a message naming the annotation to add and the address's host, never the credentials. An unclaimed or `InUse` host goes to `Error` (and is not claimed); an `Inspecting`, `Deploying` or `Ready` host keeps its state. A `Beskar7Machine` holding the host waits (`InfrastructureReady = False (WaitingForBMC)`) instead of failing, and nothing is reprovisioned. Annotating the Secret recovers the host at once: the controller watches the Secret.

List the addresses your BMCs really have. Do not widen the list to whatever the hosts currently say — a host someone already re-pointed would put their endpoint on it. Re-pointing a host to another address that *is* on the list still works; that is an integrity concern the list does not address, not a credential leak.

When `caBundleSecretRef` is set, the manager builds an HTTP client whose TLS roots include the CA bundle. The Secret data must contain a `ca.crt` (preferred) or `tls.crt` key with PEM bytes, and the credentials Secret must name it in `bmc-ca-secret` (above): the CA decides who can answer as the BMC, so whoever can edit a host must not be the one choosing it (decision D-033). A `caBundleSecretRef` the credentials Secret does not name is refused like an unlisted address, with `CredentialsNotAuthorized`, and the message does not repeat the name the annotation holds. Setting `caBundleSecretRef` together with `insecureSkipVerify: true` is rejected — the controller marks `RedfishConnectionReady = False (InsecureCABundleConflict)` and stops reconciling until the operator fixes the spec.

## Lifecycle

The reconciler drives `Status.State` through these transitions:

```
created → Available                            (BMC reachable, no consumer)
Available → InUse                              (Beskar7Machine claims via spec.consumerRef)
InUse → Inspecting                             (Beskar7Machine sets the inspection-request annotation)
Inspecting → Deploying                         (inspection report consumed from ConfigMap, validated)
Deploying → Ready                              (inspector POSTs /api/v1/provisioned; see contract §4.4)
Deploying → Error                              (inspector POSTs /api/v1/provision-failed; see contract §4.5)
Inspecting → unchanged, then Ready             (claimed; /provisioned before Deploying: kept until Deploying)
Inspecting → unchanged, then Error             (claimed; /provision-failed before Deploying: kept until Deploying)
Error about the BMC → Error of a failed run    (claimed; /provision-failed on a host v0.8.0 or earlier left in a BMC Error over Deploying)
Error about the BMC → Ready                    (claimed; /provisioned on a host v0.8.0 or earlier left in a BMC Error over Deploying)
Inspecting → Error                             (inspection timeout, default 10 min)
InUse or unclaimed → Error                     (BMC unreachable, TLS conflict, missing credentials, credentials not authorised for the address)
Inspecting/Deploying/Ready → unchanged         (claimed; BMC unreachable, missing/refused credentials, credentials not authorised for the address, a rejected certificate, a malformed address, no ComputerSystem, or the insecureSkipVerify/CA-bundle conflict: only RedfishConnectionReady reports it, and the run goes on)
Error → Available, or InUse if claimed         (operator fixes spec, BMC recovers)
Error of a failed run (claimed) → unchanged    (the two run failures above: kept until release, whatever the BMC does)
any claimed state → Available                  (Beskar7Machine deletion clears consumerRef)
```

For the diagram and the full transition table, see [State Management](state-management.md).

## Bootstrap signaling

When the Beskar7Machine controller has bootstrap data ready, it patches the bootstrap URL onto the PhysicalHost as an annotation. The PhysicalHost reconciler reads it on its next pass, persists the value to status, and clears the annotation:

| Annotation | Persisted to | Source code |
|---|---|---|
| `infrastructure.cluster.x-k8s.io/bootstrap-url` | `Status.Bootstrap.URL` | `controllers/physicalhost_controller.go:applyBootstrapURLAnnotation` |

The callback credentials — the bearer token and the boot nonce — never travel through the PhysicalHost. The Beskar7Machine controller mints them into a Secret named `<host-name>-bootstrap-token`, owned by the PhysicalHost (so it is GC'd when the host is deleted), with keys `plaintext-token`, `token-issued-at`, `token-expires-at`, `plaintext-boot-nonce`, `boot-nonce-expires-at` and `consumer` (the name of the `Beskar7Machine` they were minted for). That Secret is the only thing the callback server checks, only while the host's `ConsumerRef` names that machine (decision D-029), and only if its controller owner reference names this PhysicalHost by UID: the controller never uses or takes over a Secret of that name that it did not create for the host (D-031). The token's expiry is cut to 5 minutes once the host is `Ready`. The PhysicalHost reconciler watches it and mirrors the hashes and expiries into `Status.Bootstrap.{TokenHash, IssuedAt, ExpiresAt, BootNonceHash, BootNonceExpiresAt}` for operators; nothing authenticates against the mirror.

The `infrastructure.cluster.x-k8s.io/bootstrap-token` and `infrastructure.cluster.x-k8s.io/boot-nonce` annotations are retired: releases before D-029 promoted them into `Status.Bootstrap`, which let anyone allowed to patch a PhysicalHost forge callback credentials (SEC-12). The reconciler removes either one on sight without reading it.

## Inspection result handoff

The inspection HTTP handler does not write to `PhysicalHost.Status` directly. Instead, it stores the validated `InspectionReport` on a ConfigMap named `<host>-inspection-result` (owner-ref → PhysicalHost) and patches an `infrastructure.cluster.x-k8s.io/inspection-result-ref` annotation onto the host. The reconciler consumes the ConfigMap, writes the report to `Status.InspectionReport`, marks `HostInspected=True`, deletes the ConfigMap, and clears the annotation. This keeps the controller as the sole writer of the host's status (decision D-005 in `.claude/context/PROJECT_CONTEXT.md`).

The annotation carries a binding in `infrastructure.cluster.x-k8s.io/inspection-result-ref-binding`, written in the same patch: an HMAC-SHA256, keyed by the host's bearer token, that covers the ConfigMap's name and the SHA-256 of the `report.json` stored in it (decision D-034, SEC-15). The reconciler hashes the `report.json` it reads and checks the binding against the host's `<host>-bootstrap-token` Secret before it uses any of it, so an annotation written by hand, or a ConfigMap rewritten after the handler bound it, is removed and ignored, and the ConfigMap is left alone. The `provisioned-request` and `provision-failed-request` annotations the other two callbacks leave are bound the same way; see [State Management → The inspector's annotations carry a binding](state-management.md#the-inspectors-annotations-carry-a-binding).

## Conditions

Native `metav1.Condition` (`status.conditions[]`) — no `severity` field, and a `True` condition carries a `reason` too. `PhysicalHost` is not a CAPI contract resource: it has no `Ready` summary condition (only these three) and no `Paused` condition — its pause check (`controllers/utils.go:isPaused`) is the `cluster.x-k8s.io/paused` annotation on the host itself; a `PhysicalHost` has no owning `Cluster` to read `spec.paused` from. See [API Reference → Conditions and the CAPI mirror](api-reference.md#conditions-and-the-capi-mirror).

| Type | Meaning | True reason | Other reasons |
|---|---|---|---|
| `RedfishConnectionReady` | BMC reachable and authenticating successfully. | `RedfishConnected` | `BMCUnreachable` (the BMC cannot be reached at the network level; retried every 15 s and clears by itself, so a `Beskar7Machine` holding the host waits for it), `CredentialsNotAuthorized` (the credentials Secret does not [authorise the address, or the CA Secret](#binding-the-credentials-to-their-bmc); no request is made, and a `Beskar7Machine` holding the host waits for the Secret to be annotated), `MissingCredentials` (the credentials Secret is missing, unreadable, or lacks `username`/`password`), `RedfishConnectionFailed`, `RedfishQueryFailed`, `InsecureCABundleConflict`, `CABundleFetchFailed`. |
| `HostAvailable` | No consumer holds the host (`spec.consumerRef` is unset). Follows the claim only — BMC health is `RedfishConnectionReady`. | `HostAvailable` | `HostClaimed` (a consumer holds the host; back to `True` once the claim is released). |
| `HostInspected` | An inspection report has been persisted. | `HostInspected` | `HostReleased` (host went back to `Available`; the prior run's inspection no longer describes it). |

## Deletion

`reconcileDelete` removes the finalizer and lets Kubernetes garbage-collect the host. The PhysicalHost reconciler does NOT call Redfish during deletion; that is the job of the consuming `Beskar7Machine` (best-effort `ClearBootSourceOverride` and a power-off before clearing `ConsumerRef`: graceful, or forced for a host released mid-run — see [Beskar7Machine → Deletion](beskar7machine.md#deletion)). If a host is deleted while still claimed, the controller emits a warning event but does not block.

## Operator escape hatches

- **Force release:** Set `infrastructure.cluster.x-k8s.io/force-release: "true"` on the consuming `Beskar7Machine` before deletion. The Beskar7Machine controller will skip the BMC power-off / boot-clear during release. Use only when the BMC is permanently unreachable.

## Example

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: bmc-credentials
  namespace: default
  annotations:
    # The only BMCs these credentials may be sent to (IPs, CIDRs, hostnames, *.suffix).
    beskar7.infrastructure.cluster.x-k8s.io/bmc-addresses: "192.168.1.100"
type: Opaque
stringData:
  username: "admin"
  password: "changeme"
---
apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
kind: PhysicalHost
metadata:
  name: server-01
  namespace: default
  labels:
    topology.kubernetes.io/zone: rack-1   # published by Beskar7Cluster as a failure domain; honoured by Beskar7Machine when claiming
spec:
  redfishConnection:
    address: "https://192.168.1.100"
    credentialsSecretRef: "bmc-credentials"
```

## Print columns

`kubectl get physicalhost` shows `State`, `Ready`, and `Age`.

## See also

- [API Reference: PhysicalHost](api-reference.md#physicalhost)
- [State Management](state-management.md)
- [Architecture](architecture.md)
- [Troubleshooting](troubleshooting.md)
