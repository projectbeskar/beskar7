# Concepts

> **Audience:** Operators · Developers

Beskar7 is a Cluster API (CAPI) infrastructure provider for bare-metal machines. It manages the lifecycle of physical servers through four CRD kinds. This page explains what each kind represents and how they work together.

## PhysicalHost

`PhysicalHost` is the inventory record for a single bare-metal server. It carries the Redfish BMC connection details (`redfishConnection.address` + `redfishConnection.credentialsSecretRef`), the observed power state, any hardware requirements the server must satisfy, and the inspection report collected by the `beskar7-inspector` image after a PXE boot. A host is either unclaimed (`Available`), being inspected (`Inspecting`), or in use by a `Beskar7Machine` (`InUse`). On release it cycles back to `Available`. The `ConsumerRef` field is the claim lock — only the `Beskar7MachineReconciler` writes it.

## Beskar7Machine

`Beskar7Machine` is the CAPI infrastructure-machine resource. One `Beskar7Machine` maps to one Kubernetes node. It finds a compatible `PhysicalHost`, claims it, triggers the inspection boot via Redfish + iPXE, validates the returned hardware report against `spec.hardwareRequirements`, and — once validation passes — kexecs into the target OS image. The provisioning workflow is driven by `spec.inspectionImageURL`, `spec.targetImageURL`, and `spec.configurationURL`. `Beskar7Machine` gets its bootstrap data (kubeadm join token, cloud-init, etc.) from the CAPI `Machine` object's `spec.bootstrap.dataSecretName`; the manager serves that data over HTTPS at `GET /api/v1/bootstrap/{namespace}/{name}`.

## Beskar7MachineTemplate

`Beskar7MachineTemplate` is consumed by `KubeadmControlPlane` and `MachineDeployment` objects to mint `Beskar7Machine` resources. It carries the same spec fields as `Beskar7Machine`. Templates must survive `clusterctl move` — they carry the `cluster.x-k8s.io/v1beta1: v1_beta1` label.

## Beskar7Cluster

`Beskar7Cluster` is the CAPI infrastructure-cluster resource. It tracks the control-plane endpoint (`spec.controlPlaneEndpoint`) and reports failure domains derived from `PhysicalHost` availability. The `Beskar7ClusterReconciler` does not provision machines; it provides the cluster-level infrastructure binding that CAPI's core controller uses to set `Cluster.Status.InfrastructureReady`.

## How a node gets provisioned

1. A `Beskar7Machine` is created (by a `MachineDeployment` or directly).
2. The reconciler finds an `Available` `PhysicalHost` that satisfies `hardwareRequirements` and claims it (sets `ConsumerRef`).
3. It mints a per-host bearer token and sets a one-time PXE boot flag via Redfish, then powers on the host.
4. The host PXE-boots the inspection image (`beskar7-inspector`), which posts hardware details to `POST /api/v1/inspection/{namespace}/{name}` on the manager.
5. The controller validates the report against requirements. On failure it releases the host and tries another.
6. On success the host kexecs into the target OS image. The machine fetches its bootstrap data from `GET /api/v1/bootstrap/{namespace}/{name}`, joins the cluster, and the `Beskar7Machine` sets `Status.Ready = true`.

## Where to go next

**Operators**

- [Installation guide](installation.md) — prerequisites, Helm install, manifest install, verification.
- [iPXE Setup](ipxe-setup.md) — DHCP and HTTP boot server configuration.
- [State Management](state-management.md) — PhysicalHost lifecycle and recovery.

**Developers**

- [Development setup](development-setup.md) — clone, build, test.
- [Architecture](architecture.md) — controller flow, callback endpoint, Redfish integration.
- [API Reference](api-reference.md) — every CRD field.
