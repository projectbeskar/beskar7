# iPXE Infrastructure Setup for Beskar7

This guide explains how to set up the iPXE infrastructure required for Beskar7's network boot provisioning workflow.

## Overview

Beskar7 uses iPXE for network boot provisioning. This requires you to provide:

1. **DHCP Server** - Directs servers to boot from network
2. **HTTP Server** - Serves iPXE boot scripts and images
3. **Boot Scripts** - Dynamic iPXE scripts per host
4. **Inspection Image** - Alpine-based hardware inspection image
5. **OS Images** - Target operating systems for final deployment

```
                  
  Server   DHCP       DHCP     HTTP      HTTP   
   BMC    >  Server  >  Server  
                  
                                               
      PXE Boot             iPXE Chain           Boot Script
                                               
     v                     v                     v
                  
  iPXE              iPXE              Inspector 
  Boot    > Script   >  Image   
                  
```

## Quick Setup (dnsmasq + nginx)

For a test environment, you can run everything on one server:

```bash
# Install required packages
sudo apt update
sudo apt install -y dnsmasq nginx

# Configure dnsmasq for DHCP + TFTP + iPXE
sudo tee /etc/dnsmasq.conf << 'EOF'
# DHCP configuration
interface=eth0
dhcp-range=192.168.1.100,192.168.1.200,12h
dhcp-option=3,192.168.1.1  # Gateway
dhcp-option=6,8.8.8.8      # DNS

# iPXE chainloading
dhcp-match=set:efi-x86_64,option:client-arch,7
dhcp-match=set:efi-x86,option:client-arch,6
dhcp-match=set:bios,option:client-arch,0

# Boot different firmware types
dhcp-boot=tag:efi-x86_64,http://192.168.1.10/ipxe/boot.ipxe
dhcp-boot=tag:efi-x86,http://192.168.1.10/ipxe/boot.ipxe
dhcp-boot=tag:bios,http://192.168.1.10/ipxe/boot.ipxe

# Enable TFTP for fallback
enable-tftp
tftp-root=/var/lib/tftpboot
EOF

# Configure nginx
sudo tee /etc/nginx/sites-available/beskar7-boot << 'EOF'
server {
    listen 80;
    server_name boot-server;
    root /var/www/boot;
    
    # Enable directory listing
    autoindex on;
    
    # iPXE boot scripts
    location /ipxe/ {
        default_type text/plain;
    }
    
    # Inspection images
    location /inspector/ {
        # Large file support
        client_max_body_size 500M;
    }
    
    # OS images
    location /images/ {
        client_max_body_size 5G;
    }
    
    # Logs for debugging
    access_log /var/log/nginx/boot-access.log;
    error_log /var/log/nginx/boot-error.log;
}
EOF

sudo ln -s /etc/nginx/sites-available/beskar7-boot /etc/nginx/sites-enabled/
sudo rm /etc/nginx/sites-enabled/default

# Create directory structure
sudo mkdir -p /var/www/boot/{ipxe,inspector,images}
sudo mkdir -p /var/lib/tftpboot

# Restart services
sudo systemctl restart dnsmasq
sudo systemctl restart nginx
```

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

# iPXE chainload via HTTP
dhcp-boot=tag:efi-x86_64,http://boot-server.local/ipxe/boot.ipxe

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
            filename "http://boot-server.local/ipxe/boot.ipxe";
        }
    }
}
EOF

sudo systemctl enable isc-dhcp-server
sudo systemctl restart isc-dhcp-server
```

### 2. HTTP Server Configuration

#### Option A: nginx (Recommended)

```bash
# Install
sudo apt install nginx

# Create configuration
cat > /etc/nginx/sites-available/beskar7 << 'EOF'
server {
    listen 80;
    server_name boot-server boot-server.local;
    
    root /var/www/boot;
    autoindex on;
    
    # Increase timeouts for large files
    client_max_body_size 10G;
    client_body_timeout 300s;
    send_timeout 300s;
    
    # iPXE scripts
    location /ipxe/ {
        default_type text/plain;
        add_header Cache-Control "no-cache";
    }
    
    # Inspector image
    location /inspector/ {
        # Served as-is
    }
    
    # OS images
    location /images/ {
        # Large file support
    }
    
    # Logging
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
<VirtualHost *:80>
    ServerName boot-server
    DocumentRoot /var/www/boot
    
    <Directory /var/www/boot>
        Options +Indexes +FollowSymLinks
        AllowOverride None
        Require all granted
    </Directory>
    
    # Large file uploads
    LimitRequestBody 10737418240
    
    ErrorLog ${APACHE_LOG_DIR}/boot-error.log
    CustomLog ${APACHE_LOG_DIR}/boot-access.log combined
</VirtualHost>
EOF

sudo a2ensite beskar7
sudo systemctl reload apache2
```

### 3. iPXE Boot Scripts

Create dynamic boot scripts that pass host information to the inspection image:

```bash
# Create boot script directory
sudo mkdir -p /var/www/boot/ipxe

# Create main boot script
cat > /var/www/boot/ipxe/boot.ipxe << 'EOF'
#!ipxe

# Beskar7 Network Boot

echo Beskar7 PXE Boot
echo MAC: ${net0/mac}
echo IP: ${net0/ip}

# Set variables
# The Beskar7 callback endpoint is HTTPS on :8082. It hosts both the inspection
# POST and the bootstrap GET. Both require an Authorization: Bearer header — the
# token is per-host and is rendered into the kernel cmdline (see below).
set beskar7-api https://beskar7-controller-manager.beskar7-system.svc:8082
set boot-server http://boot-server.local

# Boot inspection image
chain ${boot-server}/ipxe/inspect.ipxe || goto failed

:failed
echo Boot failed, retrying in 30 seconds...
sleep 30
reboot
EOF

# Create inspection boot script.
#
# The inspection kernel cmdline must include four Beskar7-specific variables:
#
#   beskar7.api              Base URL of the manager's callback endpoint (https://...:8082).
#                             Used by the inspector to POST the report and by the target OS
#                             to GET bootstrap data.
#   beskar7.namespace        Namespace of the PhysicalHost (path component for both endpoints).
#   beskar7.host             Name of the PhysicalHost (path component for both endpoints).
#   beskar7.token            Plaintext per-host bearer token. Sent in the Authorization
#                             header of every request to :8082. Lifetime: 30 minutes.
#                             Source: Secret <host>-bootstrap-token, key plaintext-token.
#
# The bootstrap endpoint URL is computed deterministically from --bootstrap-url-base
# and exposed as beskar7.bootstrap-url for consumers (cloud-init, ignition) that
# prefer reading a single URL over assembling one from parts.
#
# In a real deployment the iPXE script is rendered per host by your boot infrastructure
# (Tinkerbell, dnsmasq + a CGI script, an internal HTTP service, etc.) — the snippets
# below show the variable shape that infrastructure must produce.
cat > /var/www/boot/ipxe/inspect.ipxe << 'EOF'
#!ipxe

echo Booting Beskar7 Inspector...

# Operator-controlled variables. In production these come from your DHCP/iPXE
# infrastructure rendering a per-host boot script.
set api-url       https://beskar7-controller-manager.beskar7-system.svc:8082
set namespace     default
set host-name     ${net0/mac:hexhyp}      # adapt to your naming scheme
set bootstrap-url ${api-url}/api/v1/bootstrap/${namespace}/${host-name}
# Plaintext bearer token for THIS host — render from the per-host Secret
# <host>-bootstrap-token, data key plaintext-token. NEVER bake into a static script.
set bearer-token  REPLACE_WITH_PER_HOST_TOKEN

# Boot Alpine inspection kernel
kernel http://boot-server.local/inspector/vmlinuz \
    beskar7.api=${api-url} \
    beskar7.namespace=${namespace} \
    beskar7.host=${host-name} \
    beskar7.bootstrap-url=${bootstrap-url} \
    beskar7.token=${bearer-token} \
    console=tty0 \
    console=ttyS0,115200

initrd http://boot-server.local/inspector/initrd.img

boot || goto failed

:failed
echo Inspection boot failed
sleep 30
reboot
EOF
```

### 4. Host Inspector Image

Deploy the beskar7-inspector Alpine image:

```bash
# Download inspector image (from beskar7-inspector repository)
cd /var/www/boot/inspector

# Option 1: Build from source
git clone https://github.com/projectbeskar/beskar7-inspector.git
cd beskar7-inspector
make build
cp dist/vmlinuz /var/www/boot/inspector/
cp dist/initrd.img /var/www/boot/inspector/

# Option 2: Download pre-built (if available)
wget https://github.com/projectbeskar/beskar7-inspector/releases/download/v1.0.0/vmlinuz
wget https://github.com/projectbeskar/beskar7-inspector/releases/download/v1.0.0/initrd.img
```

### 5. Operating System Images

Host your target OS images:

```bash
# Create images directory
sudo mkdir -p /var/www/boot/images

# Download Kairos (example)
cd /var/www/boot/images
wget https://github.com/kairos-io/kairos/releases/download/v2.8.1/kairos-alpine-v2.8.1.tar.gz

# Or build custom images
# ... (your image build process)
```

## Network Architecture

### Simple Single-Network Setup

```

  Network: 192.168.1.0/24            
                                     
            
    Boot          Beskar7       
    Server        Controller     
  .1.10         .1.20           
            
                                   
        
        Servers                    
             
    BMC   BMC   BMC         
    .1.100 .1.101 .1.102     
             
        

```

### Production Multi-Network Setup

```
Management Network (BMC): 10.0.1.0/24

                           
   Beskar7                           
  .1.10                              
                           
                                      
                     
    BMCs                             
   .1.100-.1.200                     
                     


Provisioning Network (PXE): 10.0.2.0/24

                           
    Boot                             
    Server                           
  .2.10                              
                           
                                      
                     
    Server NICs                      
   .2.100-.2.200                     
                     


Production Network: 10.0.3.0/24

                     
    Server NICs                      
   .3.100-.3.200                     
                     

```

## Firewall Configuration

### Boot Server

```bash
# Allow DHCP
sudo ufw allow 67/udp
sudo ufw allow 68/udp

# Allow TFTP (if used)
sudo ufw allow 69/udp

# Allow HTTP
sudo ufw allow 80/tcp

# Allow HTTPS (optional)
sudo ufw allow 443/tcp
```

### Beskar7 Controller

The manager listens on:

- `:8443` — Prometheus metrics (HTTPS, authenticated; see [Security](security/README.md)).
- `:8082` — host callback endpoint (HTTPS, bearer-token-gated; receives inspection POST and serves bootstrap GET).
- `:9443` — Beskar7Cluster webhook (when `--enable-webhook=true`).

If the boot network is segregated from the cluster network, ensure the boot subnet can reach the manager on `:8082`. The chart's NetworkPolicy already allows ingress on `:8082` cluster-wide; bare-metal IPs are not allow-listed there because the bearer token is the access control. On host firewalls (controller pods aside):

```bash
# On any iptables-based firewall between the boot subnet and the cluster:
# allow TCP/8082 to the manager Service IP / NodePort.
sudo ufw allow 6443/tcp     # Kubernetes API
sudo ufw allow 8082/tcp     # Beskar7 callback endpoint
```

## DNS Configuration

Optional but recommended:

```bash
# Add to /etc/hosts or DNS server
192.168.1.10    boot-server boot-server.local
192.168.1.20    beskar7-controller beskar7-controller.local
```

## Validation

### Test DHCP

```bash
# On boot server
sudo tcpdump -i eth0 port 67 or port 68

# On test client
sudo dhclient -v eth0
```

### Test HTTP Server

```bash
# Test from another machine
curl http://boot-server.local/ipxe/boot.ipxe
curl -I http://boot-server.local/inspector/vmlinuz
```

### Test iPXE Boot

```bash
# Boot a test server and watch serial console
# Should see:
# - DHCP request
# - iPXE chain loading
# - Boot script download
# - Kernel/initrd download
# - Boot into inspection image
```

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

### HTTP Not Accessible

```bash
# Check nginx
sudo systemctl status nginx
sudo nginx -t

# Check logs
sudo tail -f /var/log/nginx/boot-error.log

# Test locally
curl -v http://localhost/ipxe/boot.ipxe
```

### Server Won't PXE Boot

**Checks:**
1. Is PXE boot enabled in BIOS?
2. Is network boot first in boot order?
3. Is server on same network as DHCP server?
4. Check server serial console for errors

### Inspection Image Won't Boot

**Checks:**
1. Can download kernel/initrd manually?
2. Check kernel parameters in boot script
3. Review serial console for kernel panic
4. Verify image integrity (checksums)

## Advanced Configuration

### HTTPS/TLS for HTTP Server

```bash
# Get Let's Encrypt certificate
sudo apt install certbot python3-certbot-nginx
sudo certbot --nginx -d boot-server.example.com

# Update nginx config
server {
    listen 443 ssl http2;
    ssl_certificate /etc/letsencrypt/live/boot-server.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/boot-server.example.com/privkey.pem;
    # ... rest of config
}
```

### Dynamic Boot Scripts

Generate per-host boot scripts using a simple web service:

```python
# /var/www/cgi-bin/boot.py
#!/usr/bin/env python3
import os

mac = os.environ.get('HTTP_X_REAL_MAC', 'unknown')
ip = os.environ.get('REMOTE_ADDR', 'unknown')

print("Content-Type: text/plain\n")
print(f"#!ipxe")
print(f"echo Booting {mac} from {ip}")
print(f"kernel http://boot-server/inspector/vmlinuz beskar7.mac={mac}")
print(f"initrd http://boot-server/inspector/initrd.img")
print(f"boot")
```

## Production Checklist

- [ ] DHCP server configured and tested
- [ ] HTTP server configured and tested
- [ ] iPXE boot scripts created
- [ ] Inspection image deployed
- [ ] OS images uploaded
- [ ] Firewall rules configured
- [ ] DNS entries created (optional)
- [ ] Network segregation implemented (optional)
- [ ] Monitoring configured
- [ ] Backup/HA for boot server (optional)

## Next Steps

After setting up iPXE infrastructure:

1. Deploy beskar7-inspector image
2. Create PhysicalHost resources
3. Test inspection workflow
4. Deploy first Beskar7Machine
5. Monitor provisioning

See main [README](../README.md) for usage examples.

