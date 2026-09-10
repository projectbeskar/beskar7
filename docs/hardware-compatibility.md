# Hardware Compatibility

> **Audience:** Operators

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

## Vendor compatibility

Beskar7 has **no vendor-specific code paths** — one code path drives every BMC
through the small Redfish subset listed above. Any BMC that implements those
operations to spec is expected to work.

That expectation is a design property, not a test result. Be precise about the
difference when planning a deployment:

| Vendor | BMC | Redfish | Expected | Validated on real hardware |
|---|---|---|---|---|
| Dell | iDRAC 8/9 | 1.4+ | Yes | Not yet |
| HPE | iLO 4/5/6 | 1.2+ | Yes | Not yet |
| Lenovo | XCC | 1.6+ | Yes | Not yet |
| Supermicro | BMC (X12+ recommended) | 1.4+ | Yes | Not yet |
| Generic | AMI MegaRAC | 1.4+ | Yes | Not yet |
| Generic | Aspeed OpenBMC | 1.0+ | Varies — see below | Not yet |

### What has actually been validated

Beskar7's Redfish surface is exercised against three things today, none of which
is a physical vendor BMC:

- **A stateful fake** (`internal/redfishmock`) that emulates Dell/HPE/Lenovo/
  Supermicro/Generic service roots, used for claim → power → boot-override →
  release state-machine tests.
- **The DMTF reference mockup** (`public-rackmount1`, vendored under
  `internal/redfish/testdata/corpus/`), walked by the *real* gofish client to
  catch parsing and link-navigation regressions.
- **sushy-tools** (libvirt-backed Redfish emulator), used for the full
  end-to-end provisioning loop: claim → PXE → inspect → whole-disk write →
  callback → `Ready` node.

**No physical vendor BMC has been validated.** Emulators are faithful to the
spec, which is exactly why they cannot surface the firmware quirks real BMCs
have. Treat the table above as "should work, unverified" and pilot on a small
number of hosts before committing a fleet.

### Known spec-conformance caveats

General Redfish guidance, not Beskar7 findings:

- **Supermicro** — Redfish completeness varies by firmware revision; update to
  current BMC firmware before testing.
- **OpenBMC/Aspeed** — implementation coverage differs significantly between
  vendors shipping it; verify boot-source override in particular.
- **One-time boot override** — Beskar7 sets `BootSourceOverrideEnabled=Once` and
  relies on firmware to consume it after the provisioning boot. Real BMCs honor
  this; note that the sushy-tools emulator does **not** (it persists the boot
  device), which is a harness limitation rather than a product behavior.

### Reporting real-hardware results

Validation reports are welcome and are the fastest way to move a row from
"Not yet" to a real result — please
[open an issue](https://github.com/projectbeskar/beskar7/issues) with the vendor,
BMC model, firmware revision, and what did or did not work.

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
apiVersion: infrastructure.cluster.x-k8s.io/v1beta2
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
kubectl logs -n capb7-system \
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

| Feature | Requirement | Mandated by the Redfish spec |
|---------|-------------|------------------------------|
| **Power On/Off** | `/redfish/v1/Systems/{id}/Actions/ComputerSystem.Reset` | Yes |
| **Power Status** | `/redfish/v1/Systems/{id}` -> `PowerState` | Yes |
| **Set PXE Boot** | `/redfish/v1/Systems/{id}` -> `Boot.BootSourceOverrideTarget = Pxe` | Yes |
| **System Info** | `/redfish/v1/Systems/{id}` -> Manufacturer, Model, Serial | Yes |
| **Network Info** | `/redfish/v1/Systems/{id}/EthernetInterfaces` | Yes |

Everything Beskar7 needs is part of the base Redfish specification, which is
why no vendor-specific handling is required. Whether a given BMC *implements*
the spec correctly is a firmware question — see "What has actually been
validated" above.

## FAQ

**Q: Do I need vendor-specific configuration?**
A: No — Beskar7 uses the same code path on every vendor, with no vendor-specific branches.

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
