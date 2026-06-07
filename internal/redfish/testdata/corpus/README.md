# Redfish corpus — DMTF public-rackmount1

## Provenance

Upstream repo: https://github.com/DMTF/Redfish-Mockup-Server  
Commit: `2d39eb14122337ceab0712a9610b1cd37c65f487`  
License: BSD-3-Clause (see `LICENSE`)

## What is vendored

Only the subtree that gofish walks when beskar7 calls its 9 read methods
(`GetSystemInfo`, `GetPowerState`, `GetNetworkAddresses`) against a single-system
endpoint with `BasicAuth: true` (no SessionService handshake). The minimal-complete
set was established empirically: write the test, run it, copy each 404'd resource,
repeat until the reads pass.

| File | Why included |
|---|---|
| `redfish/v1/index.json` | ServiceRoot — gofish's connect entry point |
| `redfish/v1/odata/index.json` | `/redfish/v1/odata` — fetched by gofish during service-root parse |
| `redfish/v1/Systems/index.json` | ComputerSystemCollection |
| `redfish/v1/Systems/437XR1138R2/index.json` | The one ComputerSystem: Manufacturer/Model/SerialNumber/PowerState/Boot |
| `redfish/v1/Systems/437XR1138R2/EthernetInterfaces/index.json` | EthernetInterfaceCollection (4 members) |
| `redfish/v1/Systems/437XR1138R2/EthernetInterfaces/12446A3B0411/index.json` | Physical NIC 1 — has IPv4 + IPv6 + OEM block |
| `redfish/v1/Systems/437XR1138R2/EthernetInterfaces/12446A3B8890/index.json` | Physical NIC 2 — has VLAN flag + different MAC |
| `redfish/v1/Systems/437XR1138R2/EthernetInterfaces/VLAN1/index.json` | Virtual VLAN interface |
| `redfish/v1/Systems/437XR1138R2/EthernetInterfaces/ToManager/index.json` | Management NIC |

## What is pruned

Everything else (AccountService, CertificateService, Chassis, ComponentIntegrity,
EventService, KeyService, Managers, Registries, TaskService, UpdateService,
SessionService, `$metadata`) — none of these are fetched by beskar7's read-only
interface surface. Vendoring them would add ~255 files / ~2 MB of irrelevant data
that the test never touches.

## How to refresh

If the upstream mockup changes and you want to update the corpus:

1. Re-clone or pull the upstream repo.
2. Verify the system UUID is still `437XR1138R2` (or update the test constants).
3. Copy the files listed in the table above verbatim — do not minimize fields,
   the realism of the OEM blocks and extra enum values is the point.
4. Update the commit SHA in this file.
5. Run `go test ./internal/redfish/... -race` to confirm the corpus is still valid.

## Sanitization note

This is the DMTF *reference* mockup. It uses synthetic data (Contoso manufacturer,
made-up serials/MACs). No real BMC identifiers are present. Any corpus contributed
from real hardware must be scrubbed of serial numbers, asset tags, MACs, BMC
hostnames, and private IP addresses before merging.
