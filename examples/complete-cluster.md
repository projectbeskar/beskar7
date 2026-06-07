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
end-to-end on real bare metal (contract v4, beskar7-inspector v4):
claim → PXE → inspection → `Deploying` → whole-disk write → `COS_OEM` inject →
`/provisioned` callback → `Ready` k3s node with `ProviderID` set. Its inline
comments cover the bootstrap-Secret-must-be-Kairos-`#cloud-config` invariant and
the `--kubelet-arg=provider-id=b7://<ns>/<host>` flag needed for CAPI
Node-association.

## Multi-node / production path

A maintained Kairos bootstrap provider (e.g. `provider-kubeadm` baked into the
Kairos image, or
[`cluster-api-provider-kairos`](https://github.com/kairos-io/cluster-api-provider-kairos))
can emit Kairos-compatible cloud-config and pair with a `KubeadmControlPlane` or
`MachineDeployment` for HA/multi-node. **That path has not been validated against
Beskar7 end-to-end** — do not treat it as a confirmed working configuration.
