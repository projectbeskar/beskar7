# iPXE Infrastructure Setup for Beskar7

> **Audience:** Operators

This guide explains how to set up the iPXE boot infrastructure required for
Beskar7's network provisioning workflow under contract v2. Read it alongside
`docs/inspector-contract.md`, which is the authoritative specification for every
wire-protocol detail referenced here.

## Overview

Beskar7 provisions bare-metal hosts in two steps:

1. The **Beskar7 controller** mints a per-host boot nonce and bearer token when a
   `Beskar7Machine` claims a `PhysicalHost`, then instructs the BMC to PXE-boot the
   host via Redfish.
2. The **operator's boot infrastructure** intercepts the host's PXE request and
   chainloads a first-stage iPXE script that fetches the controller's rendered
   per-host boot parameters. The controller's `/boot` endpoint consumes the nonce
   and returns a complete iPXE script that boots the inspection image.

The inspection image (`beskar7-inspector`) is a single **static Rust
`x86_64-unknown-linux-musl` binary** used directly as the initramfs `/init` — there
is no shell, no Alpine userland, and no external tools. It probes hardware, POSTs
a report to the controller, fetches the CAPI bootstrap user-data, streams the
Kairos whole-disk image to the target disk (verifying its SHA-256 digest), and
reboots the host into the provisioned OS.

To provide this infrastructure you need:

1. **DHCP server** — directs hosts to boot from the network and points iPXE at your
   boot server
2. **Boot server** — serves `vmlinuz` and `initrd.img` over HTTPS; also runs the
   dynamic boot service that resolves MAC → nonce URL and emits the first-stage
   chainload script
3. **OS image server** — hosts the Kairos whole-disk raw image served at
   `Beskar7Machine.Spec.TargetImageURL`

```
  Host          DHCP          Boot server           Beskar7 controller
  BMC    ─────► DHCP/TFTP     (operator-run)        (management cluster)
                              ▲                      ▲
  Host NIC ──► iPXE boot ──► first-stage  ─HTTPS──► GET /api/v1/boot/
                              chainload              /{ns}/{host}/{nonce}
                              |
                              └── kernel: https://<boot-server>/inspector/vmlinuz
                              └── initrd: https://<boot-server>/inspector/initrd.img
                                         │
                                         ▼
                              beskar7-inspector (PID 1)
                              Phase 1: probe → POST /inspection → 202
                              Phase 2: GET /bootstrap → write image → reboot
```

## Boot flow in detail

Understanding the flow is essential before configuring any component.

### Step 1: controller mints secrets and PXE-boots the host

When `Beskar7MachineReconciler` claims a `PhysicalHost`, `triggerInspection`:

1. Mints a **bearer token** (32-byte random, 30-minute lifetime). Its SHA-256 is
   stored in `PhysicalHost.Status.Bootstrap.TokenHash`; the plaintext is stored in
   the Secret `<hostName>-bootstrap-token`, data key `plaintext-token`.
2. Mints a **boot nonce** (256-bit random, ~10-minute lifetime). Its SHA-256 is
   stored in `PhysicalHost.Status.Bootstrap.BootNonceHash`; the plaintext is stored
   in the same Secret under data key `plaintext-boot-nonce`.
3. Instructs the BMC (via Redfish) to set the boot source to PXE and power on.

The nonce is **single-use**: the controller's `/boot` handler consumes it on the
first successful fetch and never un-consumes it. A fresh nonce is minted on every
re-provision attempt. Because the TTL is ~10 minutes and a host that fails to
chainload in that window needs another provision cycle anyway, your boot service
MUST read the nonce **fresh from the Secret on every boot request** — a static
iPXE file cannot hard-code it.

### Step 2: operator boot service resolves MAC → nonce URL

Your boot infrastructure runs a **dynamic boot service** (Tinkerbell/Smee, a CGI
endpoint, an internal HTTP microservice, or equivalent) that, for each PXE-booting
host:

1. Receives the host's NIC MAC address (from DHCP or the iPXE `${net0/mac}`
   variable).
2. Maps that MAC to the corresponding `PhysicalHost` namespace and name using a
   mapping you maintain (e.g. a YAML/database keyed by MAC that you update when
   hosts are enrolled).
3. Reads the host's **current boot nonce** plaintext from the Kubernetes Secret
   `<hostName>-bootstrap-token`, data key `plaintext-boot-nonce`.
4. Emits a first-stage iPXE script that **chainloads**:

   ```
   https://<beskar7.api>/api/v1/boot/<namespace>/<hostName>/<nonce>?mac=${net0/mac}
   ```

The `?mac=${net0/mac}` query parameter lets the `/boot` endpoint render the
`BOOTIF` kernel parameter for multi-NIC hosts. It is optional; single-NIC hosts
work without it.

This chainload URL MUST be HTTPS. The nonce is a capability: possession of it
authorizes one fetch of the bearer token and boot parameters. Serving it over
plain HTTP exposes it to a network attacker before it is consumed (see §8 of
`docs/inspector-contract.md`).

### Step 3: controller renders the complete per-host iPXE script

The controller's `/boot` handler (`GET /api/v1/boot/{namespace}/{hostName}/{nonce}`)
does not require a bearer token — the nonce IS the authorization. On a valid,
unconsumed, in-TTL nonce:

1. Marks the nonce consumed (optimistic-locked, single-use).
2. Reads the bearer token plaintext from the `<hostName>-bootstrap-token` Secret.
3. Returns a complete iPXE script (`Content-Type: text/plain`) that boots the
   inspection image with all required parameters on the kernel cmdline.

The operator does NOT hand-render any `beskar7.*` kernel parameters. The controller
renders them all. The script the controller returns looks like:

```ipxe
#!ipxe
kernel {InspectionImageURL}/vmlinuz beskar7.api={api} beskar7.namespace={ns} beskar7.host={host} beskar7.token={token} beskar7.target={target} beskar7.target-digest={digest} beskar7.ca={base64CA} [beskar7.disk={disk}] [BOOTIF={01-mac}]
initrd {InspectionImageURL}/initrd.img
boot
```

The `{InspectionImageURL}` is `Beskar7Machine.Spec.InspectionImageURL` — the HTTPS
base URL of your boot server's `vmlinuz` and `initrd.img`. The kernel and initrd
URLs MUST be HTTPS: a plain-HTTP kernel or initrd fetch is an OS-image MITM
vector.

### Step 4: inspector boots as PID 1

The inspector runs in two phases (see `docs/inspector-contract.md` §9):

- **Phase 1** (always): probe hardware, POST report to `/api/v1/inspection`, wait
  for `202 Accepted`.
- **Phase 2** (when bootstrap data is ready): GET `/api/v1/bootstrap`, stream the
  Kairos whole-disk raw image to the target disk (verifying its SHA-256), inject
  the per-host cloud-config into `COS_OEM`, and reboot.

Both callback requests are over verified TLS, using the CA delivered inline via
`beskar7.ca`. The target image download (via `beskar7.target`) MAY be plain HTTP
because its integrity and authenticity come entirely from `beskar7.target-digest`
— see §8.1 of `docs/inspector-contract.md` for the exact trust model.

## Prerequisites and address requirements

### `beskar7.api` must be externally reachable

`beskar7.api` is the base URL of the controller's callback server. It MUST be
reachable from bare-metal hosts on the provisioning network — it cannot be a
cluster-internal name. The inspector runs with no DNS resolver in v2; an
**IP-literal** is strongly recommended (e.g. `https://192.0.2.10:8082`). If you
use a hostname, your DHCP server must supply a working DNS server that resolves it
at the time the inspector runs (§8.2 of `docs/inspector-contract.md`).

Set `--bootstrap-url-base` on the controller to the externally-routable address
and ensure the serving certificate has a SAN covering it. The default
(`https://beskar7-controller-manager.beskar7-system.svc:8082`) is cluster-internal
and unreachable from bare metal — the manager logs a warning at startup when it
detects a `.svc` name.

Common exposure options:

- **LoadBalancer service**: set `service.type: LoadBalancer` in the chart; the
  controller pod's Service on port 8082 gets a cloud-provider IP.
- **NodePort**: expose 8082 as a NodePort and use a worker-node IP or a keepalived
  VIP in `--bootstrap-url-base`.
- **Ingress / reverse proxy**: use a TLS-passthrough ingress on port 8082. The
  controller speaks TLS natively; SNI passthrough is simpler than termination +
  re-encryption.

### HTTPS everywhere on the boot path

| Hop | Required | Reason |
|---|---|---|
| Chainload URL (`https://<api>/api/v1/boot/.../{nonce}`) | HTTPS mandatory | The nonce is in the URL; plain HTTP leaks it to an attacker before it is consumed |
| Kernel (`vmlinuz`) and initrd (`initrd.img`) fetches | HTTPS mandatory | Plain HTTP allows OS-image MITM |
| Target OS image (`beskar7.target`) | HTTP or HTTPS | Integrity comes from `beskar7.target-digest`; TLS is not the trust anchor here |
| Callback (`/inspection`, `/bootstrap`) | HTTPS mandatory | The bootstrap endpoint returns the cluster join secret |

---

## Quick Setup (dnsmasq + nginx)

For a test environment, you can run everything on one server. Replace
`192.168.1.10` and `192.0.2.10:8082` with your actual addresses throughout.

```bash
# Install required packages
sudo apt update
sudo apt install -y dnsmasq nginx

# Configure dnsmasq for DHCP + TFTP + iPXE chainload
sudo tee /etc/dnsmasq.conf << 'EOF'
# DHCP configuration
interface=eth0
dhcp-range=192.168.1.100,192.168.1.200,12h
dhcp-option=3,192.168.1.1  # Gateway
dhcp-option=6,8.8.8.8      # DNS

# iPXE chainloading — match client architecture
dhcp-match=set:efi-x86_64,option:client-arch,7
dhcp-match=set:efi-x86,option:client-arch,6
dhcp-match=set:bios,option:client-arch,0

# First-stage chainload: points at your dynamic boot service (see "Dynamic Boot Service" below)
# The boot service resolves MAC -> nonce URL and emits the per-host first-stage script.
# This hop MUST be HTTPS: the script the boot service returns embeds the nonce-bearing
# /boot URL in its body, so a plain-HTTP response would leak the nonce to a network attacker.
dhcp-boot=tag:efi-x86_64,https://boot-server.example.com/ipxe/chain
dhcp-boot=tag:efi-x86,https://boot-server.example.com/ipxe/chain
dhcp-boot=tag:bios,https://boot-server.example.com/ipxe/chain

# Enable TFTP for fallback
enable-tftp
tftp-root=/var/lib/tftpboot
EOF

# Configure nginx (HTTPS — required for the chainload/kernel/initrd path)
# Replace the certificate paths with your actual certificate.
sudo tee /etc/nginx/sites-available/beskar7-boot << 'EOF'
server {
    listen 443 ssl http2;
    server_name boot-server.example.com;
    ssl_certificate     /etc/ssl/boot-server/tls.crt;
    ssl_certificate_key /etc/ssl/boot-server/tls.key;

    root /var/www/boot;

    # Enable directory listing for debugging
    autoindex on;

    # First-stage iPXE chainload: the dynamic boot service that emits per-host scripts
    location /ipxe/chain {
        proxy_pass http://127.0.0.1:8099;   # your dynamic boot service
        default_type text/plain;
        add_header Cache-Control "no-cache, no-store";
    }

    # Inspector boot artifacts (vmlinuz + initrd.img)
    location /inspector/ {
        default_type application/octet-stream;
        add_header Cache-Control "no-cache";
    }

    # OS images (plain HTTP is acceptable here; integrity comes from TargetImageDigest)
    # You may serve OS images on a separate plain-HTTP server if preferred.
    location /images/ {
        client_max_body_size 50G;
    }

    access_log /var/log/nginx/boot-access.log;
    error_log /var/log/nginx/boot-error.log;
}
EOF

sudo ln -s /etc/nginx/sites-available/beskar7-boot /etc/nginx/sites-enabled/
sudo rm -f /etc/nginx/sites-enabled/default

# Create directory structure
sudo mkdir -p /var/www/boot/{inspector,images}
sudo mkdir -p /var/lib/tftpboot

# Restart services
sudo systemctl restart dnsmasq
sudo systemctl restart nginx
```

---

## Detailed Setup

### 1. DHCP Server Configuration

#### Option A: dnsmasq (Recommended for Simple Setups)

```bash
# Install
sudo apt install dnsmasq

# Configure
cat > /etc/dnsmasq.conf << 'EOF'
# Basic DHCP
interface=eth0
dhcp-range=192.168.1.100,192.168.1.200,12h
dhcp-option=option:router,192.168.1.1
dhcp-option=option:dns-server,8.8.8.8

# Match client architecture
dhcp-match=set:efi-x86_64,option:client-arch,7

# Point iPXE at the dynamic boot service (HTTPS required for nonce-bearing next hop)
dhcp-boot=tag:efi-x86_64,https://boot-server.example.com/ipxe/chain

# Logging
log-dhcp
EOF

sudo systemctl enable dnsmasq
sudo systemctl restart dnsmasq
```

#### Option B: ISC DHCP Server (Enterprise)

```bash
# Install
sudo apt install isc-dhcp-server

# Configure
cat > /etc/dhcp/dhcpd.conf << 'EOF'
option space ipxe;
option ipxe-encap-opts code 175 = encapsulate ipxe;
option ipxe.priority code 1 = signed integer 8;
option ipxe.keep-san code 8 = unsigned integer 8;
option ipxe.skip-san-boot code 9 = unsigned integer 8;
option ipxe.syslogs code 85 = string;
option ipxe.cert code 91 = string;
option ipxe.privkey code 92 = string;
option ipxe.crosscert code 93 = string;
option ipxe.no-pxedhcp code 176 = unsigned integer 8;
option ipxe.bus-id code 177 = string;
option ipxe.san-filename code 188 = string;
option ipxe.bios-drive code 189 = unsigned integer 8;
option ipxe.username code 190 = string;
option ipxe.password code 191 = string;
option ipxe.reverse-username code 192 = string;
option ipxe.reverse-password code 193 = string;
option ipxe.version code 235 = string;
option iscsi-initiator-iqn code 203 = string;
option ipxe.pxeext code 16 = unsigned integer 8;
option ipxe.iscsi code 17 = unsigned integer 8;
option ipxe.aoe code 18 = unsigned integer 8;
option ipxe.http code 19 = unsigned integer 8;
option ipxe.https code 20 = unsigned integer 8;
option ipxe.tftp code 21 = unsigned integer 8;
option ipxe.ftp code 22 = unsigned integer 8;
option ipxe.dns code 23 = unsigned integer 8;
option ipxe.bzimage code 24 = unsigned integer 8;
option ipxe.multiboot code 25 = unsigned integer 8;
option ipxe.slam code 26 = unsigned integer 8;
option ipxe.srp code 27 = unsigned integer 8;
option ipxe.nbi code 32 = unsigned integer 8;
option ipxe.pxe code 33 = unsigned integer 8;
option ipxe.elf code 34 = unsigned integer 8;
option ipxe.comboot code 35 = unsigned integer 8;
option ipxe.efi code 36 = unsigned integer 8;
option ipxe.fcoe code 37 = unsigned integer 8;
option ipxe.vlan code 38 = unsigned integer 8;
option ipxe.menu code 39 = unsigned integer 8;
option ipxe.sdi code 40 = unsigned integer 8;
option ipxe.nfs code 41 = unsigned integer 8;

subnet 192.168.1.0 netmask 255.255.255.0 {
    range 192.168.1.100 192.168.1.200;
    option routers 192.168.1.1;
    option domain-name-servers 8.8.8.8;

    # UEFI (Architecture 7 = x86_64 UEFI)
    class "pxeclients" {
        match if substring (option vendor-class-identifier, 0, 9) = "PXEClient";
        if option architecture-type = 00:07 {
            filename "https://boot-server.example.com/ipxe/chain";
        }
    }
}
EOF

sudo systemctl enable isc-dhcp-server
sudo systemctl restart isc-dhcp-server
```

### 2. HTTP Server Configuration

The boot server must serve `vmlinuz` and `initrd.img` over HTTPS. The target OS
image may be on the same server or a separate one and may use plain HTTP (integrity
comes from `beskar7.target-digest`, not transport security).

#### Option A: nginx (Recommended)

```bash
# Install
sudo apt install nginx

# Create configuration
# Replace certificate paths and server_name with your actual values.
cat > /etc/nginx/sites-available/beskar7 << 'EOF'
server {
    listen 443 ssl http2;
    server_name boot-server.example.com;
    ssl_certificate     /etc/ssl/boot-server/tls.crt;
    ssl_certificate_key /etc/ssl/boot-server/tls.key;

    root /var/www/boot;
    autoindex on;

    # Inspector boot artifacts — served over HTTPS (required for kernel/initrd)
    location /inspector/ {
        default_type application/octet-stream;
        add_header Cache-Control "no-cache";
    }

    # OS images — plain HTTP is acceptable here; integrity comes from
    # Beskar7Machine.Spec.TargetImageDigest, not TLS.
    location /images/ {
        client_max_body_size 50G;
        send_timeout 600s;
    }

    # First-stage chainload: reverse-proxy to the dynamic boot service
    location /ipxe/chain {
        proxy_pass http://127.0.0.1:8099;
        default_type text/plain;
        add_header Cache-Control "no-cache, no-store";
    }

    access_log /var/log/nginx/boot-access.log combined;
    error_log /var/log/nginx/boot-error.log warn;
}
EOF

sudo ln -s /etc/nginx/sites-available/beskar7 /etc/nginx/sites-enabled/
sudo nginx -t
sudo systemctl reload nginx
```

#### Option B: Apache

```bash
# Install
sudo apt install apache2

# Create configuration
cat > /etc/apache2/sites-available/beskar7.conf << 'EOF'
<VirtualHost *:443>
    ServerName boot-server.example.com
    DocumentRoot /var/www/boot

    SSLEngine on
    SSLCertificateFile    /etc/ssl/boot-server/tls.crt
    SSLCertificateKeyFile /etc/ssl/boot-server/tls.key

    <Directory /var/www/boot>
        Options +Indexes +FollowSymLinks
        AllowOverride None
        Require all granted
    </Directory>

    # OS images
    LimitRequestBody 53687091200

    ErrorLog ${APACHE_LOG_DIR}/boot-error.log
    CustomLog ${APACHE_LOG_DIR}/boot-access.log combined
</VirtualHost>
EOF

sudo a2enmod ssl
sudo a2ensite beskar7
sudo systemctl reload apache2
```

### 3. Dynamic Boot Service

The dynamic boot service is the critical component that bridges your DHCP/iPXE
infrastructure to the Beskar7 controller's per-host `/boot` endpoint. It receives
each host's PXE request, looks up the current boot nonce from Kubernetes, and emits
a first-stage iPXE script that chainloads the controller-rendered per-host boot
parameters.

The Beskar7 controller does NOT render this first-stage script — that is the
operator's responsibility (Tinkerbell/Smee pattern: operator boot-infra owns
MAC→URL rendering; Beskar7 owns nonce mint/verify).

#### What the boot service must do for each request

1. Extract the requesting NIC's MAC address from the iPXE `${net0/mac}` variable
   (passed as a query parameter, e.g. `?mac=52:54:00:12:34:56`).
2. Look up the corresponding `PhysicalHost` (namespace and name) from your MAC →
   host mapping.
3. Read the current nonce plaintext from the Kubernetes Secret:
   ```
   kubectl -n <namespace> get secret <hostName>-bootstrap-token -o jsonpath='{.data.plaintext-boot-nonce}' | base64 -d
   ```
   The nonce changes on every provision cycle. Read it fresh for every boot
   request — never cache it.
4. Return an iPXE script that chainloads:
   ```
   https://<beskar7.api>/api/v1/boot/<namespace>/<hostName>/<nonce>?mac=${net0/mac}
   ```

#### Example: simple Python CGI boot service

The example below is a minimal reference. In production, use Tinkerbell/Smee or a
purpose-built service with proper error handling and access controls.

```python
#!/usr/bin/env python3
# /usr/lib/cgi-bin/ipxe-boot.py
#
# Called by nginx/apache as a CGI handler for GET /ipxe/chain?mac=<mac>
#
# Prerequisites:
#   - Runs with a kubeconfig (or in-cluster ServiceAccount) that has
#     read access to Secrets in the provisioning namespaces.
#   - MAC-to-host mapping in MAC_TO_HOST below (or a database/CRD lookup).
#
# The nonce is single-use with a ~10-minute TTL. Do not cache it.

import os
import subprocess
import json
import sys

# Map NIC MAC addresses to (namespace, physicalhost-name) pairs.
# Update this when enrolling new hosts.
MAC_TO_HOST = {
    "52:54:00:aa:bb:cc": ("default", "worker-01"),
    "52:54:00:dd:ee:ff": ("default", "worker-02"),
}

# External address of the Beskar7 callback server.
# Must be reachable from bare metal; use an IP-literal (the inspector has no
# DNS resolver in v2 — see docs/inspector-contract.md §8.2).
BESKAR7_API = "https://192.0.2.10:8082"

def get_nonce(namespace, host_name):
    """Read the current boot nonce from the per-host Secret."""
    result = subprocess.run(
        [
            "kubectl", "-n", namespace,
            "get", "secret", f"{host_name}-bootstrap-token",
            "-o", "jsonpath={.data.plaintext-boot-nonce}",
        ],
        capture_output=True, text=True, check=True
    )
    import base64
    return base64.b64decode(result.stdout.strip()).decode()

def main():
    qs = os.environ.get("QUERY_STRING", "")
    params = dict(p.split("=", 1) for p in qs.split("&") if "=" in p)
    mac = params.get("mac", "").lower()

    if mac not in MAC_TO_HOST:
        print("Status: 404 Not Found\r\nContent-Type: text/plain\r\n\r\n")
        print(f"unknown MAC: {mac}")
        return

    namespace, host_name = MAC_TO_HOST[mac]
    try:
        nonce = get_nonce(namespace, host_name)
    except Exception as e:
        print("Status: 500 Internal Server Error\r\nContent-Type: text/plain\r\n\r\n")
        print(f"failed to read nonce for {host_name}: {e}")
        return

    boot_url = f"{BESKAR7_API}/api/v1/boot/{namespace}/{host_name}/{nonce}?mac=${{net0/mac}}"

    print("Content-Type: text/plain\r\n\r\n", end="")
    print(f"#!ipxe")
    print(f"chain {boot_url} || goto failed")
    print(f":failed")
    print(f"echo Boot chainload failed; sleeping 30s before retry")
    print(f"sleep 30")
    print(f"reboot")

main()
```

The controller's `/boot` handler validates the nonce, consumes it, reads the bearer
token from the Secret, and returns the complete per-host iPXE script. Your boot
service only constructs the URL — it does not know or pass any `beskar7.*` kernel
parameters.

### 4. Inspector Image

The `beskar7-inspector` is a static Rust `x86_64-unknown-linux-musl` binary used
directly as the initramfs `/init`. Build it from source with `make image`; no
prebuilt release artifacts exist at a public URL.

```bash
# Clone and build
git clone https://github.com/projectbeskar/beskar7-inspector.git
cd beskar7-inspector

# Build vmlinuz + initrd.img (requires Docker with buildx)
make image
# Produces:
#   build/vmlinuz
#   build/initrd.img

# Deploy to your boot server
sudo mkdir -p /var/www/boot/inspector
sudo cp build/vmlinuz  /var/www/boot/inspector/
sudo cp build/initrd.img /var/www/boot/inspector/
```

`make image` runs the multi-stage `Dockerfile`: it compiles the static musl binary,
assembles a minimal initramfs (the binary as `/init` plus the kernel modules it
needs), and packages them as `build/vmlinuz` + `build/initrd.img`.

Set `Beskar7Machine.Spec.InspectionImageURL` to the HTTPS base URL where you serve
these files:

```yaml
spec:
  inspectionImageURL: https://boot-server.example.com/inspector
```

The controller's `/boot` endpoint appends `/vmlinuz` and `/initrd.img` to this URL
to form the `kernel` and `initrd` lines of the rendered iPXE script.

### 5. Operating System Images

The target OS is a **Kairos whole-disk raw image**. The inspector streams it
directly to the target disk and verifies its SHA-256 before booting into it.

#### Hosting the image

Serve the raw image over HTTP or HTTPS from any file server. Set
`Beskar7Machine.Spec.TargetImageURL` to its URL and
`Beskar7Machine.Spec.TargetImageDigest` to its `sha256:` digest (see "Computing
the target image digest" below). Plain HTTP is acceptable for the image server
because the inspector verifies the image's content digest, not TLS.

```bash
# Example: serve a Kairos raw image from nginx
sudo mkdir -p /var/www/boot/images

# Place your Kairos whole-disk raw image here.
# The image must be a raw whole-disk image (not a tar.gz or ISO).
sudo cp /path/to/kairos-standard-amd64-generic-v3.x.y.raw /var/www/boot/images/kairos-v3.x.y.raw
```

Plain HTTP image server example (separate from the HTTPS boot server):

```bash
# Minimal nginx serving images over plain HTTP on a dedicated IP or port
server {
    listen 192.168.1.10:8080;

    root /var/www/boot/images;
    autoindex on;

    location / {
        client_max_body_size 50G;
        send_timeout 600s;
    }
}
```

#### Computing the target image digest

`Beskar7Machine.Spec.TargetImageDigest` must be the SHA-256 of the **exact bytes**
served at `Beskar7Machine.Spec.TargetImageURL`. The inspector verifies the written
image against this digest and refuses to proceed to mount, inject user-data, or
reboot if the digest does not match. It is the sole integrity and authenticity
anchor for the OS image.

Compute it over the pinned artifact you serve:

```bash
# Compute the digest
sha256sum /var/www/boot/images/kairos-v3.x.y.raw

# Example output:
#   a3f1d2e4...abcd1234  kairos-v3.x.y.raw
#
# Format it as sha256:<hex> for the CRD field:
#   sha256:a3f1d2e4...abcd1234
```

Set the CRD fields accordingly:

```yaml
spec:
  targetImageURL: http://192.168.1.10:8080/kairos-v3.x.y.raw
  targetImageDigest: sha256:a3f1d2e4...abcd1234
```

The digest must exactly match what `sha256sum` produces for the artifact at that
URL. Pin the URL to a specific, immutable artifact — never point `targetImageURL`
at a mutable path whose bytes can change after you compute the digest. The
inspector verifies the written image byte-for-byte against this digest before
booting into it; any content change (however small) causes an abort.

The accepted format is `sha256:` followed by exactly 64 lowercase hex characters,
matching the CRD pattern `^sha256:[a-f0-9]{64}$`. The controller validates this
format at `/boot` render time and returns an opaque failure if it is malformed.

---

## Network Architecture

### Simple Single-Network Setup

```
  Network: 192.168.1.0/24

  Boot server       Beskar7 controller
  192.168.1.10      management cluster

  Provisioning hosts
  BMC/NIC: 192.168.1.100–.200
```

### Production Multi-Network Setup

```
Management Network (BMC): 10.0.1.0/24

  Beskar7 controller          BMCs
  management cluster          10.0.1.100–.200

Provisioning Network (PXE): 10.0.2.0/24

  Boot server                 Server NICs
  10.0.2.10                   10.0.2.100–.200

Production Network: 10.0.3.0/24

  Server NICs (post-provision)
  10.0.3.100–.200
```

In the multi-network setup, `--bootstrap-url-base` must be set to an address
reachable from the provisioning network — typically a LoadBalancer IP or NodePort
on that network. Both the `/boot` chainload URL (in the first-stage iPXE script)
and `beskar7.api` (rendered into the kernel cmdline) must be the same externally-
routable address.

---

## Firewall Configuration

### Boot Server

```bash
# DHCP
sudo ufw allow 67/udp
sudo ufw allow 68/udp

# TFTP (if used)
sudo ufw allow 69/udp

# HTTPS for boot artifacts and dynamic chainload service
sudo ufw allow 443/tcp

# Plain HTTP for OS image serving (optional, on a separate port if preferred)
sudo ufw allow 8080/tcp
```

### Beskar7 Controller

The manager listens on:

- `:8443` — Prometheus metrics (HTTPS, authenticated; see [Security](security/README.md)).
- `:8082` — host callback endpoint (HTTPS); three routes:
  - `GET /api/v1/boot/{ns}/{host}/{nonce}` — nonce-gated, not bearer-gated
  - `POST /api/v1/inspection/{ns}/{host}` — bearer-gated
  - `GET /api/v1/bootstrap/{ns}/{host}` — bearer-gated
- `:9443` — Beskar7Cluster webhook (when `--enable-webhook=true`).

If the boot network is segregated from the cluster network, ensure the provisioning
subnet can reach the manager on `:8082`. The chart's NetworkPolicy already allows
ingress on `:8082` cluster-wide; bare-metal IPs are not allow-listed there because
the per-host token/nonce is the access control.

```bash
# On any iptables-based firewall between the provisioning subnet and the cluster:
sudo ufw allow 8082/tcp     # Beskar7 callback endpoint
sudo ufw allow 6443/tcp     # Kubernetes API (if the boot service reads Secrets directly)
```

---

## DNS Configuration

Optional but useful for the boot server hostname. Not required for `beskar7.api`
when using an IP-literal (recommended for v2 — the inspector has no DNS resolver).

```bash
# Add to /etc/hosts or your DNS server
192.168.1.10    boot-server boot-server.example.com
192.0.2.10      beskar7-api
```

---

## Validation

### Test DHCP

```bash
# On boot server
sudo tcpdump -i eth0 port 67 or port 68

# On test client
sudo dhclient -v eth0
```

### Test HTTPS Boot Server

```bash
# Verify kernel and initrd are reachable over HTTPS
curl -fI https://boot-server.example.com/inspector/vmlinuz
curl -fI https://boot-server.example.com/inspector/initrd.img
```

### Test the Dynamic Boot Service

```bash
# Replace MAC and address with real values
curl -sf "https://boot-server.example.com/ipxe/chain?mac=52:54:00:aa:bb:cc"
# Expected: a valid #!ipxe script containing a chain https://... line
```

### Test the Controller /boot Endpoint

```bash
# Read the current nonce for a host (requires kubectl access)
NONCE=$(kubectl -n default get secret worker-01-bootstrap-token \
  -o jsonpath='{.data.plaintext-boot-nonce}' | base64 -d)

# Fetch the rendered script
curl -fk "https://192.0.2.10:8082/api/v1/boot/default/worker-01/${NONCE}"
# Expected: a valid #!ipxe script with beskar7.* params on the kernel line
# Note: after this fetch the nonce is consumed; the host needs a fresh provision
# cycle to get a new one.
```

### Test iPXE Boot

```bash
# Boot a test server and watch the serial console.
# Expected sequence:
# - DHCP request and response
# - iPXE downloads the first-stage chainload script
# - First-stage chainloads https://<api>/api/v1/boot/.../
# - Controller renders and returns the full per-host iPXE script
# - Kernel and initrd download over HTTPS from your boot server
# - Inspector boots as PID 1, probes hardware, POSTs report to /inspection (202)
```

---

## Troubleshooting

### DHCP Not Working

```bash
# Check dnsmasq is running
sudo systemctl status dnsmasq

# Check logs
sudo journalctl -u dnsmasq -f

# Test DHCP manually
sudo dhcping -s 192.168.1.10
```

### HTTPS Boot Server Not Accessible

```bash
# Check nginx
sudo systemctl status nginx
sudo nginx -t

# Check logs
sudo tail -f /var/log/nginx/boot-error.log

# Test locally
curl -v https://localhost/inspector/vmlinuz
```

### Nonce Expired or Already Consumed

If a host fails to chainload within the ~10-minute nonce TTL, or if the nonce was
already consumed by a previous boot attempt, the `/boot` endpoint returns an opaque
`404`. The host needs a fresh provision cycle:

```bash
# Delete and recreate the Beskar7Machine to trigger re-provision
kubectl delete beskar7machine <name> -n <namespace>
# Recreate or let CAPI recreate it; the controller mints a fresh nonce at triggerInspection.
```

### Server Won't PXE Boot

1. Is PXE boot enabled in BIOS/UEFI firmware settings?
2. Is network boot first in the boot order?
3. Is the server on the same network segment as the DHCP server?
4. Check the server's serial console or BMC console for errors.

### Inspector Image Won't Boot

1. Can you download `vmlinuz` and `initrd.img` manually over HTTPS?
   ```bash
   curl -fI https://boot-server.example.com/inspector/vmlinuz
   ```
2. Is `Beskar7Machine.Spec.InspectionImageURL` set to the correct HTTPS base URL?
3. Is the `/boot` endpoint reachable from the provisioning network?
4. Review the serial console for kernel panics or inspector error messages.
5. Enable `beskar7.debug=true` in the boot parameters — not available via the CRD
   in v2, but you can temporarily set it by patching `buildBootIPXEScript` in a
   local build.

### Digest Verification Failures

If the inspector aborts after writing the OS image (before mount/inject/reboot),
the digest did not match:

1. Recompute the digest: `sha256sum /path/to/image.raw`
2. Confirm `Beskar7Machine.Spec.TargetImageDigest` is `sha256:<hex>` with exactly
   64 lowercase hex characters after the colon.
3. Confirm the artifact at `TargetImageURL` has not changed since you computed the
   digest. The URL must point to a pinned, immutable artifact.
4. After the fix, delete and recreate the `Beskar7Machine` to trigger a fresh
   provision cycle.

---

## Advanced Configuration

### HTTPS/TLS for Boot Server

Use a certificate that covers the boot server hostname (the one in your DHCP boot
URL) and the `beskar7.api` external address. Let's Encrypt works if both names are
DNS-accessible:

```bash
sudo apt install certbot python3-certbot-nginx
sudo certbot --nginx -d boot-server.example.com
```

For a self-signed CA (common in closed provisioning networks), distribute the CA to
the controller's cert-manager issuer so the `/boot` endpoint's `beskar7.ca` inline
bundle is correct. The boot server's cert is separate from the callback server's
cert — the inspector verifies the callback CA from the cmdline, not from the OS
trust store.

### Multi-NIC Hosts (BOOTIF)

When a host has multiple NICs, the inspector needs to know which one to configure
for DHCP. The `/boot` endpoint renders a `BOOTIF=01-<mac-with-dashes>` parameter
if the first-stage iPXE appends `?mac=${net0/mac}` to the chainload URL. Your
dynamic boot service must pass this through:

```
https://<api>/api/v1/boot/<ns>/<host>/<nonce>?mac=${net0/mac}
```

The controller validates the MAC (must be a colon-separated 6-octet hex string);
a malformed value is silently omitted rather than rendered onto the cmdline. When
`BOOTIF` is absent the inspector configures the single physical NIC, which is
correct for single-NIC hosts.

---

## Production Checklist

- [ ] DHCP server configured, tested, and set to HTTPS chainload URL
- [ ] Boot server running HTTPS with a valid certificate
- [ ] `vmlinuz` and `initrd.img` deployed and reachable over HTTPS
- [ ] Dynamic boot service implemented and tested (MAC → nonce URL resolution)
- [ ] MAC-to-PhysicalHost mapping populated for all enrolled hosts
- [ ] `Beskar7Machine.Spec.InspectionImageURL` set to HTTPS base URL of boot server
- [ ] OS image hosted (HTTP or HTTPS)
- [ ] `Beskar7Machine.Spec.TargetImageURL` set to pinned, immutable artifact URL
- [ ] `Beskar7Machine.Spec.TargetImageDigest` computed and set (`sha256:<64-hex>`)
- [ ] `--bootstrap-url-base` set to an externally-reachable IP-literal or hostname
- [ ] Callback server TLS cert SAN covers the `--bootstrap-url-base` address
- [ ] Firewall rules allow provisioning subnet → controller `:8082`
- [ ] DNS entries created (optional; IP-literal recommended for `beskar7.api`)
- [ ] Network segregation implemented (optional)
- [ ] Monitoring configured

---

## Next Steps

After setting up iPXE infrastructure:

1. Create `PhysicalHost` resources for your bare-metal inventory.
2. Test the inspection workflow end-to-end with a single host.
3. Deploy your first `Beskar7Machine`.
4. Monitor provisioning with `kubectl get physicalhost,beskar7machine -A`.

See [README](../README.md) for usage examples and
[inspector-contract.md](inspector-contract.md) for the authoritative wire-protocol
specification.
