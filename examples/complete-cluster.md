# Complete Cluster Deployment Example — superseded

This walkthrough has been removed. It described a `KubeadmControlPlane` +
`KubeadmConfigTemplate` (1 control-plane + 2 worker) deployment that **cannot
produce a working Kairos node**: kubeadm bootstrap providers emit
cloud-init/Ignition, not Kairos `#cloud-config`. Beskar7 delivers the bootstrap
Secret **byte-verbatim** to the inspector, which writes it verbatim to the
image's `COS_OEM` partition (D-014); a kubeadm Secret written there is ignored
by the Kairos agent and the node never joins. The example also used an
all-zeros `targetImageDigest`, which blocks any real image download.

## Use this instead

[`kairos-k3s-node.yaml`](kairos-k3s-node.yaml) — the CR structure validated
end-to-end on the project's lab (libvirt VMs whose BMCs are emulated by
sushy-tools, not physical servers):
claim → PXE → inspection → `Deploying` → whole-disk write → `COS_OEM` inject →
`/provisioned` callback → `Ready` k3s node with `ProviderID` set. Its inline
comments cover the bootstrap-Secret-must-be-Kairos-`#cloud-config` invariant and
the `--kubelet-arg=provider-id=b7://<ns>/<host>` flag needed for CAPI
Node-association.

## Multi-node / production path

[`cluster-api-provider-kairos`](https://github.com/kairos-io/cluster-api-provider-kairos)
emits Kairos `#cloud-config`. On the same lab it has provisioned a k0s HA cluster
through Beskar7 from scratch: three control planes from a `KairosControlPlane`
behind a kube-vip VIP and two workers from a `MachineDeployment`, all on
`Beskar7MachineTemplate`s (controller↔inspector contract v4.2). That is emulated
hardware, not physical servers, and no such example ships in this repository yet.
