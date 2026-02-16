# Hardware Compatibility

This document describes Beskar7's hardware compatibility and requirements.

## Overview

**Beskar7 works with ANY Redfish-compliant BMC** because it only uses universally-supported features:

- **Power Management** - On/Off/Reset operations
- **PXE Boot Flag** - Setting boot source to network
- **System Information** - Basic hardware details

**No vendor-specific code. No workarounds. No complexity.**

## Requirements

### BMC Requirements

Your server's BMC must support:

1. **Redfish API** (version 1.0 or later)
2. **Power control** (`/redfish/v1/Systems/{id}/Actions/ComputerSystem.Reset`)
3. **Boot source control** (`/redfish/v1/Systems/{id}` - `Boot.BootSourceOverrideTarget`)
4. **Network accessibility** (BMC must be reachable from Kubernetes cluster)

That's it! These are universally supported across all Redfish implementations.

### Network Requirements

1. **BMC Network** - Controller can reach BMC on port 443 (HTTPS)
2. **Provisioning Network** - Server can PXE boot and reach boot server
3. **Optional:** Separate networks for management, provisioning, and production

See [iPXE Setup Guide](ipxe-setup.md) for network architecture examples.

## Tested Vendors

While Beskar7 works with any Redfish BMC, we've specifically tested:

| Vendor | BMC Type | Redfish Version | Status | Notes |
|--------|----------|-----------------|--------|-------|
| **Dell** | iDRAC 8/9 | 1.4+ | Tested | no special handling |
| **HPE** | iLO 4/5/6 | 1.2+ | Tested | Redfish compliance |
| **Lenovo** | XCC | 1.6+ | Tested | Clean implementation |
| **Supermicro** | BMC | 1.4+ | Tested | Newer BMC versions recommended |
| **Generic** | AMI MegaRAC | 1.4+ | Tested | Used by many whitebox vendors |
| **Generic** | Aspeed OpenBMC | 1.0+ | Partial | Some implementations incomplete |

### Notes on Tested Hardware

**Dell (iDRAC):**
- Excellent Redfish implementation
- No quirks or workarounds needed
- Power management very reliable

**HPE (iLO):**
- Industry-leading Redfish compliance
- All tested features work flawlessly
- Highly recommended

**Lenovo (XCC):**
- Clean, standards-compliant implementation
- No issues encountered
- Good documentation

**Supermicro:**
- Quality varies by BMC version
- Update to latest BMC firmware for best results
- X12+ series recommended

**Whitebox/Generic:**
- AMI MegaRAC generally works well
- OpenBMC implementations vary by vendor
- Test thoroughly before production use

## What's Different from Other Bare-Metal Tools?

### No Vendor-Specific Code

**Other tools:**
- Special Dell code
- Special HPE code
- Special Lenovo code
- Special Supermicro code

**Beskar7:**
- One code path for all vendors
- Uses only standard Redfish features
- Simpler and more reliable

### Network Boot Only

**Other tools:**
- Complex ISO mounting mechanisms
- Vendor-specific implementations
- Unreliable across vendors

**Beskar7:**
- Network boot via iPXE
- Works the same everywhere
- No vendor differences

### No Boot Parameter Injection

**Other tools:**
- Inject kernel parameters via BIOS
- Different for every vendor
- Fragile and complex

**Beskar7:**
- Boot parameters in iPXE script
- Vendor agnostic
- Reliable and simple

## Compatibility Testing

### Quick Test

Test basic Redfish connectivity:

```bash
# Replace with your BMC details
BMC_IP="192.168.1.100"
USERNAME="admin"
PASSWORD="password"

# Test Redfish root
curl -k -u "${USERNAME}:${PASSWORD}" \
  "https://${BMC_IP}/redfish/v1/" | jq

# Test systems endpoint
curl -k -u "${USERNAME}:${PASSWORD}" \
  "https://${BMC_IP}/redfish/v1/Systems" | jq

# Test power state
curl -k -u "${USERNAME}:${PASSWORD}" \
  "https://${BMC_IP}/redfish/v1/Systems/1" | \
  jq '.PowerState'
```

If these work, your BMC is compatible!

### Full Test

Create a test PhysicalHost:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: test-bmc-creds
  namespace: default
stringData:
  username: "admin"
  password: "your-password"
---
apiVersion: infrastructure.cluster.x-k8s.io/v1beta1
kind: PhysicalHost
metadata:
  name: test-server
  namespace: default
spec:
  redfishConnection:
    address: "https://192.168.1.100"
    credentialsSecretRef: "test-bmc-creds"
    insecureSkipVerify: true  # Only for testing!
```

Monitor the status:

```bash
kubectl apply -f test-physicalhost.yaml

# Watch status
kubectl get physicalhost test-server -w

# Expected: Should transition to Available
# NAME          STATE       READY
# test-server   Available   true
```

If it becomes `Available`, your hardware is fully compatible!

## Known Limitations

### Redfish API Must Be Enabled

Some BMCs ship with Redfish disabled. Enable it in BMC settings:

**Dell iDRAC:**
```
Network > Redfish > Enable Redfish over LAN
```

**HPE iLO:**
```
Network > iLO RESTful API > Enable iLO RESTful API
```

**Supermicro:**
```
Configuration > Redfish API > Enable
```

### PXE Boot Must Be Enabled

Ensure PXE/network boot is enabled in BIOS:

1. Enter BIOS setup
2. Navigate to boot configuration
3. Enable "Network Boot" or "PXE Boot"
4. Set network boot in boot order
5. Save and exit

### Firewall Considerations

**BMC Firewall:**
- Port 443 (HTTPS) must be open for Redfish
- Allow traffic from Kubernetes nodes

**Server Firewall:**
- DHCP (ports 67/68) for PXE boot
- HTTP (port 80) for boot scripts and images

## Troubleshooting

### PhysicalHost Stuck in Enrolling

**Symptom:** Host never transitions to Available

**Causes:**
- BMC not reachable from controller
- Invalid credentials
- Redfish API disabled
- Firewall blocking port 443

**Debug:**
```bash
# Check controller logs
kubectl logs -n beskar7-system \
  deployment/beskar7-controller-manager -f

# Test from controller pod
kubectl run -it --rm debug \
  --image=curlimages/curl --restart=Never -- \
  curl -k -u admin:password https://BMC_IP/redfish/v1/
```

### Power Operations Fail

**Symptom:** Can't power on/off server

**Causes:**
- Insufficient BMC user permissions
- BMC licensing restrictions
- Hardware safety interlocks

**Solution:**
- Ensure BMC user has power management privileges
- Check BMC license (some vendors require licenses for remote power control)
- Verify no physical safety interlocks (e.g., open chassis)

### Can't Set PXE Boot

**Symptom:** Boot source override fails

**Causes:**
- Boot override not supported by BMC
- NIC disabled in BIOS
- Network boot not in boot order

**Solution:**
- Verify BMC supports boot source override:
  ```bash
  curl -k -u admin:password \
    https://BMC_IP/redfish/v1/Systems/1 | \
    jq '.Boot.BootSourceOverrideTarget@Redfish.AllowableValues'
  ```
- Should include `"Pxe"` in the array
- Enable network boot in BIOS if missing

## Reporting Issues

If your hardware doesn't work with Beskar7, please report it!

**Include:**

1. **Hardware Info:**
   - Vendor and model
   - BMC type and version
   - BIOS version

2. **Redfish Info:**
   ```bash
   curl -k -u admin:password https://BMC_IP/redfish/v1/ | jq
   ```

3. **Error Details:**
   - Controller logs
   - PhysicalHost status
   - Error messages

4. **What Doesn't Work:**
   - Enrollment?
   - Power management?
   - Boot source setting?

Submit to: https://github.com/projectbeskar/beskar7/issues

## Feature Support Matrix

| Feature | Requirement | All Vendors |
|---------|-------------|-------------|
| **Power On/Off** | `/redfish/v1/Systems/{id}/Actions/ComputerSystem.Reset` | Yes |
| **Power Status** | `/redfish/v1/Systems/{id}` -> `PowerState` | Yes |
| **Set PXE Boot** | `/redfish/v1/Systems/{id}` -> `Boot.BootSourceOverrideTarget = Pxe` | Yes |
| **System Info** | `/redfish/v1/Systems/{id}` -> Manufacturer, Model, Serial | Yes |
| **Network Info** | `/redfish/v1/Systems/{id}/EthernetInterfaces` | Yes |

Everything Beskar7 needs is universally supported!

## FAQ

**Q: Do I need vendor-specific configuration?**
A: No! Beskar7 works the same on all vendors.

**Q: Do I need to update BMC firmware?**
A: Recommended but not required. Latest firmware usually has best Redfish compliance.

**Q: What if my BMC doesn't support Redfish?**
A: Beskar7 won't work. Consider BMC firmware update or hardware upgrade.

**Q: Can I use IPMI instead of Redfish?**
A: No. Beskar7 requires Redfish. IPMI is obsolete.

**Q: Does Beskar7 support Legacy BIOS boot?**
A: Yes, though UEFI is recommended. Your iPXE infrastructure determines boot mode.

**Q: What about ARM servers?**
A: Should work if BMC supports Redfish and server can PXE boot. Not yet tested.

## Production Checklist

Before deploying to production:

- [ ] BMC firmware up to date
- [ ] Redfish API enabled
- [ ] Network boot enabled in BIOS
- [ ] Network boot in boot order (first position)
- [ ] BMC accessible from Kubernetes cluster
- [ ] Firewall rules configured
- [ ] BMC user accounts configured with proper permissions
- [ ] Test PhysicalHost enrollment successful
- [ ] Test power operations work
- [ ] Test PXE boot works

## Next Steps

After verifying hardware compatibility:

1. Set up iPXE infrastructure - See [iPXE Setup Guide](ipxe-setup.md)
2. Deploy inspector image - See [beskar7-inspector](https://github.com/projectbeskar/beskar7-inspector)
3. Register hosts - See [examples](../examples/)
4. Start provisioning - See [README](../README.md)

---

**The beauty of simplicity:** Any Redfish BMC works, no exceptions, no workarounds!
