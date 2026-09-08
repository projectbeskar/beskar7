# Building a target image

Beskar7 writes a **whole-disk image** to the host and injects the per-host cloud-config into the
image's `COS_OEM` partition; it does not build images. This page is the verified way to build one
with the Kairos tooling, plus the one step that tooling does not do for you: adding beskar7's
image-side stages to `COS_OEM`.

Everything on this page ran as written on 2026-09-08 with the tool digests shown. The images it
describes are the ones behind the k3s and k0s control-plane runs cited in
[Beskar7Machine → ProviderID & Node association](beskar7machine.md#providerid--node-association).

## What the image has to be

- **A raw disk image** (`.raw` — not a qcow2, not an ISO) with the Kairos partition labels
  `COS_GRUB`, `COS_OEM` and `COS_RECOVERY`. The inspector locates `COS_OEM` by label after the
  write ([inspector contract](inspector-contract.md) §9.1).
- **Self-installing.** A raw image from the Kairos tooling carries only the recovery system. Its
  first boot runs from `COS_RECOVERY`, lays out the state partitions and installs the active system
  (`/oem/01_reset.yaml`, written by the build), then reboots into it. Beskar7 relies on this:
  after the disk write the inspector reboots the host from disk, and the image finishes installing
  itself.
- **Kairos applies the cloud-config on every boot that carries it — including that install boot.**
  The inspector's `/oem/99_beskar7.yaml` and everything the bootstrap provider put in it is
  processed by the recovery system before the active one exists. For k3s that is harmless. For
  **k0s it is not** — see [k0s: the start gate](#k0s-the-start-gate).

## Build the raw image

Two verified paths. Both produce the same layout: 64 MiB `COS_GRUB`, 64 MiB `COS_OEM`, and
`COS_RECOVERY` for the rest.

### With AuroraBoot, from a published Kairos container image

```bash
mkdir -p build
cat > build/cloud-config.yaml <<'CC'
#cloud-config
# Applied on every boot of every host built from this image, so keep it host-independent.
# The per-host cluster config arrives separately as /oem/99_beskar7.yaml.
users:
  kairos:
    groups: [admin]
    ssh_authorized_keys:
      - ssh-ed25519 AAAA... you@example
CC
docker run --rm --privileged -v "$PWD/build:/output" \
  quay.io/kairos/auroraboot@sha256:784509bb3d01c2995cf427ca2ea7ab8292860477fecf462388b86745ed02da5c \
  --set "disable_http_server=true" --set "disable_netboot=true" \
  --set "container_image=docker://quay.io/kairos/hadron:v0.4.0-standard-amd64-generic-v4.1.2-k0s-v1.34.8-k0s.0" \
  --set "state_dir=/output" --set "disk.raw=true" \
  --cloud-config /output/cloud-config.yaml
```

That digest is AuroraBoot v0.27.0. The image lands in `build/`, named after the container image
(`kairos-hadron-v0.4.0-standard-amd64-generic-v4.1.2-k0sv1.34.8+k0s.0.raw`, 2.0 GiB). Swap the
`container_image` tag for the k3s flavour to build a k3s image.

**Always pass `--cloud-config`.** It becomes `/oem/90_custom.yaml` (mode `0600`). Without it,
AuroraBoot writes a default `90_custom.yaml` that creates a `kairos` user with password `kairos`
on every host you provision.

### With osbuilder, from a rootfs directory

Use this when you start from a published Kairos ISO rather than a container image. Extract the
rootfs (`rootfs.squashfs` on the ISO, `unsquashfs -d rootfs`), then:

```bash
cat > cloud-config.yaml <<'CC'
#cloud-config
install:
  auto: true
  device: "auto"
  reboot: true
CC
sudo docker run --rm --privileged -v "$PWD:/output" \
  --entrypoint /raw-images.sh \
  quay.io/kairos/osbuilder-tools@sha256:f418d67762a82dda11b2b4382b78e5b554965ded8411eb208dab37765b34eb37 \
  /output/rootfs /output/kairos.raw /output/cloud-config.yaml
```

The third argument is not optional: without it the build writes neither `01_reset.yaml` nor
`90_custom.yaml`, and the image boots to `cos-recovery login:` and stays there. This is the path
that built the k3s image behind [`examples/kairos-k3s-node.yaml`](../examples/kairos-k3s-node.yaml).

## Add beskar7's image-side stages to `COS_OEM`

The stages are **yip** configs and have to be separate files in `/oem`: a `#cloud-config` file's
`stages:` block is silently ignored, so they cannot ride inside `--cloud-config`.

| Stage | Path in `COS_OEM` | Distro | Purpose |
|---|---|---|---|
| [`examples/kairos-providerid-stage.yaml`](../examples/kairos-providerid-stage.yaml) | `/oem/10_beskar7_providerid.yaml` | k3s | Turns `/oem/beskar7/provider-id` into the kubelet `--provider-id`. |
| [`examples/kairos-k0s-providerid-stage.yaml`](../examples/kairos-k0s-providerid-stage.yaml) | `/oem/10_beskar7_providerid.yaml` | k0s | Patches `Node.spec.providerID` from the same file after the node registers. |
| [`examples/kairos-k0s-start-gate.yaml`](../examples/kairos-k0s-start-gate.yaml) | `/oem/05_beskar7_k0s_gate.yaml` | k0s | **Required.** Keeps k0s from starting on the install boot or before its arguments exist. |

An image is one distro or the other, so it carries one `10_beskar7_providerid.yaml`. The `05_` /
`10_` prefixes keep the stages ahead of the inspector's `99_beskar7.yaml` within each yip stage.

```bash
IMG=build/kairos.raw
LOOP=$(sudo losetup -f --show -P "$IMG")
sudo blkid -o value -s LABEL "${LOOP}p2"       # must print: COS_OEM
sudo mkdir -p /mnt/oem && sudo mount "${LOOP}p2" /mnt/oem

sudo cp examples/kairos-k0s-start-gate.yaml       /mnt/oem/05_beskar7_k0s_gate.yaml    # k0s only
sudo cp examples/kairos-k0s-providerid-stage.yaml /mnt/oem/10_beskar7_providerid.yaml  # or the k3s stage
sudo chmod 0644 /mnt/oem/05_beskar7_k0s_gate.yaml /mnt/oem/10_beskar7_providerid.yaml
ls -l /mnt/oem      # 01_reset.yaml  05_beskar7_k0s_gate.yaml  10_beskar7_providerid.yaml  90_custom.yaml  grubenv

sudo umount /mnt/oem && sudo losetup -d "$LOOP"
```

`qemu-nbd --connect=/dev/nbdN --format=raw "$IMG"` with `/dev/nbdNp2` works the same way if loop
partitions are unavailable.

## Publish and pin

Serve the image over HTTP where the **inspection** network can reach it — that is the network the
inspector boots on, usually the isolated provisioning network, not the LAN — and pin its digest:

```bash
sha256sum build/kairos.raw
```

```yaml
spec:
  targetImageURL: "http://<boot-server>/kairos.raw"
  targetImageDigest: "sha256:<the sum above>"
```

Rebuilding the image changes the digest; a `Beskar7MachineTemplate` that still carries the old one
fails the write with a digest mismatch, which is the intended protection.

## k0s: the start gate

A k0s control plane does not form on beskar7 without
[`examples/kairos-k0s-start-gate.yaml`](../examples/kairos-k0s-start-gate.yaml) in the image. Two
things go wrong without it, both observed directly and both closed by the gate:

1. **The install boot joins the cluster.** The recovery system applies the CAPI cloud-config and
   starts `k0scontroller` with the join token, so a joining control-plane node registers as a
   *voting* etcd member from the installer, about 20 s before its own kernel exists. The installer
   then reboots it. A two-member etcd with one dead voter has lost quorum for good: the init node
   logs `ReadIndex response took too long`, and every later join hangs. The bootstrap provider's
   own guard keys on the `cdroot` kernel argument, which a recovery-partition boot does not carry.
   k3s survives the same boot because it joins as a learner.
2. **The first start races the arguments.** The Kairos k0s provider starts k0s at the top of the
   step that delivers its arguments and writes them a few seconds later; the arguments won three
   boots out of five. A bare `k0s controller` self-initialises a cluster of its own, and k0s never
   attempts a join once a CA exists, so a lost race means re-provisioning the host.

The gate is three `ConditionPathExists=` lines on `k0scontroller.service` and `k0sworker.service`,
written at the `initramfs` stage so they exist before systemd loads a unit: never in a recovery
or live boot, and never before `/etc/k0s/.capi-args-ready` exists. That marker is written by
**cluster-api-provider-kairos** as the last of its `write_files` — from
[kairos-io/cluster-api-provider-kairos#99](https://github.com/kairos-io/cluster-api-provider-kairos/pull/99); no released version writes it yet. The contract is
two-sided: a gated image with a provider that does not write the marker never starts k0s at all.

With the gate in place the run that validated it showed, per node: no k0s output at all in the
install boot's journal, one k0s start on the active boot with no restarts, one join attempt, and a
`KairosControlPlane` at 3/3 with `EtcdHealthy=True`.

## Checking a provisioned host

```bash
sudo cat /oem/beskar7/provider-id          # b7://<namespace>/<host>
ls /oem                                    # your stages next to 99_beskar7.yaml
ls /etc/k0s/.capi-args-ready               # k0s: written by the bootstrap provider
systemctl show k0scontroller -p ConditionResult -p NRestarts -p ExecMainStartTimestamp
                                           # k0s: ConditionResult=yes, NRestarts=0, one start time
```

Kairos does not keep systemd's own `Started …` lines for the active boot, so check the start
itself: the effective `ExecStart` must be what is running.

```bash
systemctl show -p ExecStart --value k0scontroller | grep -o 'argv\[\]=[^;]*'
sudo tr '\0' ' ' < /proc/$(systemctl show -p MainPID --value k0scontroller)/cmdline; echo
```

The install boot's journal survives under a second machine-id in `/var/log/journal/` (the one that
is not `/etc/machine-id`). With the gate in place it contains no k0s output at all:

```bash
sudo journalctl -D /var/log/journal/<other-id> -o cat | grep -c 'k0s\['   # 0
```
