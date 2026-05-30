package controllers

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// goldenInspectionReportPath is the canonical contract fixture shared with the
// beskar7-inspector repo. The inspector emits this exact wire format and asserts
// it round-trips through its Rust serde types; this test asserts the same bytes
// decode losslessly into the controller's InspectionReportRequest and convert
// faithfully into the InspectionReport API type. If the two repos drift, one of
// these tests goes red. See test/contract/README.md (contract: v1).
const goldenInspectionReportPath = "../test/contract/golden_inspection_report.json"

// Expected aggregates for the golden host. These are the numbers the controller's
// hardware-requirements validation computes from the fixture; they are part of
// the contract because the inspector must emit capacity strings the controller
// can parse. Note the IEC convention: each "32GiB" DIMM is 32*2^30 bytes, which
// divided by 1e9 (decimal GB) truncates to 34 — so four DIMMs total 136 GB, not
// 128. parseMemoryCapacityGB documents this; the inspector author must expect it.
const (
	goldenTotalCPUCores  = 64   // 2 sockets * 32 cores
	goldenMemPerDIMMGB   = 34   // "32GiB" -> 34 decimal GB (32*2^30/1e9, truncated)
	goldenTotalMemoryGB  = 136  // 4 * 34
	goldenTotalDiskGB    = 1920 // 2 * 960
	goldenNumCPUSockets  = 2
	goldenNumDIMMs       = 4
	goldenNumDisks       = 2
	goldenNumNICs        = 2
	goldenFirstNICNumIPs = 2
)

func readGoldenReport(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(goldenInspectionReportPath))
	if err != nil {
		t.Fatalf("read golden inspection report at %s: %v", goldenInspectionReportPath, err)
	}
	return b
}

// TestInspectorReportContract is the anti-drift guardrail between Beskar7 and the
// beskar7-inspector. It exercises the full server-side path the inspector's POST
// body takes: decode -> buildInspectionReport -> hardware-sum validation.
func TestInspectorReportContract(t *testing.T) {
	golden := readGoldenReport(t)

	// 1. Lenient decode — exactly what controllers/inspection_handler.go does in
	//    production (json.Decoder without DisallowUnknownFields). Proves the
	//    fixture is accepted by the live handler.
	t.Run("decodes leniently (production parity)", func(t *testing.T) {
		var req InspectionReportRequest
		if err := json.Unmarshal(golden, &req); err != nil {
			t.Fatalf("lenient decode failed: %v", err)
		}
		if req.Manufacturer == "" || req.Model == "" || req.SerialNumber == "" {
			t.Errorf("system identity fields not populated: %+v", req)
		}
		if len(req.CPUs) != goldenNumCPUSockets {
			t.Errorf("CPUs: got %d, want %d", len(req.CPUs), goldenNumCPUSockets)
		}
		if len(req.Memory) != goldenNumDIMMs {
			t.Errorf("Memory: got %d, want %d", len(req.Memory), goldenNumDIMMs)
		}
		if len(req.Disks) != goldenNumDisks {
			t.Errorf("Disks: got %d, want %d", len(req.Disks), goldenNumDisks)
		}
		if len(req.NICs) != goldenNumNICs {
			t.Errorf("NICs: got %d, want %d", len(req.NICs), goldenNumNICs)
		}
		if len(req.NICs) > 0 && len(req.NICs[0].IPAddresses) != goldenFirstNICNumIPs {
			t.Errorf("first NIC IP count: got %d, want %d (ipAddresses must be a JSON array)",
				len(req.NICs[0].IPAddresses), goldenFirstNICNumIPs)
		}
		if req.BootModeDetected == "" || req.FirmwareVersion == "" {
			t.Errorf("bootModeDetected/firmwareVersion not populated: %+v", req)
		}
	})

	// 2. Strict decode — DisallowUnknownFields fails if the inspector emits a
	//    field the controller does not model. This is the forward-drift catch:
	//    a new key in the inspector's output that Beskar7 would silently drop in
	//    production turns this test red instead.
	t.Run("decodes strictly (no unknown fields)", func(t *testing.T) {
		dec := json.NewDecoder(bytes.NewReader(golden))
		dec.DisallowUnknownFields()
		var req InspectionReportRequest
		if err := dec.Decode(&req); err != nil {
			t.Fatalf("strict decode failed — the golden fixture carries a field "+
				"InspectionReportRequest does not model (schema drift): %v", err)
		}
	})

	// 3. Round-trip — decode then re-encode, and compare the JSON object graphs.
	//    Catches the reverse drift: a struct field renamed or dropped so that a
	//    key present in the golden is no longer emitted. (omitempty means we rely
	//    on the fixture populating every modelled field with a non-zero value.)
	t.Run("round-trips losslessly", func(t *testing.T) {
		var req InspectionReportRequest
		if err := json.Unmarshal(golden, &req); err != nil {
			t.Fatalf("decode for round-trip failed: %v", err)
		}
		reEncoded, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("re-encode failed: %v", err)
		}

		var goldenObj, rtObj map[string]any
		if err := json.Unmarshal(golden, &goldenObj); err != nil {
			t.Fatalf("decode golden to map: %v", err)
		}
		if err := json.Unmarshal(reEncoded, &rtObj); err != nil {
			t.Fatalf("decode round-trip to map: %v", err)
		}
		if !reflect.DeepEqual(goldenObj, rtObj) {
			t.Errorf("round-trip object graph differs from golden — a modelled field "+
				"was renamed, dropped, or retyped.\n golden: %s\n re-enc: %s", golden, reEncoded)
		}
	})

	// 4. Conversion fidelity — buildInspectionReport is the DTO -> API-type hop
	//    consumed by every downstream reconcile. Assert each collection and a few
	//    representative scalars survive the conversion.
	t.Run("buildInspectionReport conversion is faithful", func(t *testing.T) {
		var req InspectionReportRequest
		if err := json.Unmarshal(golden, &req); err != nil {
			t.Fatalf("decode failed: %v", err)
		}
		report := buildInspectionReport(req)

		if report.Manufacturer != req.Manufacturer ||
			report.Model != req.Model ||
			report.SerialNumber != req.SerialNumber ||
			report.BootModeDetected != req.BootModeDetected ||
			report.FirmwareVersion != req.FirmwareVersion {
			t.Errorf("scalar identity fields not carried through conversion: %+v", report)
		}
		if len(report.CPUs) != len(req.CPUs) ||
			len(report.Memory) != len(req.Memory) ||
			len(report.Disks) != len(req.Disks) ||
			len(report.NICs) != len(req.NICs) {
			t.Fatalf("collection lengths changed in conversion: cpus %d/%d mem %d/%d disk %d/%d nic %d/%d",
				len(report.CPUs), len(req.CPUs), len(report.Memory), len(req.Memory),
				len(report.Disks), len(req.Disks), len(report.NICs), len(req.NICs))
		}
		if report.CPUs[0].Cores != req.CPUs[0].Cores || report.CPUs[0].Threads != req.CPUs[0].Threads {
			t.Errorf("CPU core/thread counts not carried through: %+v", report.CPUs[0])
		}
		if report.Memory[0].Capacity != req.Memory[0].Capacity {
			t.Errorf("memory capacity string not carried through: %q != %q",
				report.Memory[0].Capacity, req.Memory[0].Capacity)
		}
		if report.Disks[0].SizeGB != req.Disks[0].SizeGB {
			t.Errorf("disk sizeGB not carried through: %d != %d", report.Disks[0].SizeGB, req.Disks[0].SizeGB)
		}
		if !reflect.DeepEqual(report.NICs[0].IPAddresses, req.NICs[0].IPAddresses) {
			t.Errorf("NIC IP list not carried through: %v != %v",
				report.NICs[0].IPAddresses, req.NICs[0].IPAddresses)
		}
	})

	// 5. Hardware-requirement aggregates — the controller validates by summing
	//    cpu.Cores, parseMemoryCapacityGB(capacity), and disk.SizeGB. Running the
	//    REAL parseMemoryCapacityGB over the fixture locks the memory-suffix
	//    contract (the field the inspector most easily gets wrong) and the IEC
	//    truncation semantics documented above.
	t.Run("hardware aggregates match documented totals", func(t *testing.T) {
		var req InspectionReportRequest
		if err := json.Unmarshal(golden, &req); err != nil {
			t.Fatalf("decode failed: %v", err)
		}
		report := buildInspectionReport(req)

		totalCores := 0
		for _, c := range report.CPUs {
			totalCores += c.Cores
		}
		if totalCores != goldenTotalCPUCores {
			t.Errorf("total CPU cores: got %d, want %d", totalCores, goldenTotalCPUCores)
		}

		totalMem := 0
		for _, m := range report.Memory {
			gb, err := parseMemoryCapacityGB(m.Capacity)
			if err != nil {
				t.Fatalf("parseMemoryCapacityGB(%q) failed — the inspector emitted a "+
					"capacity the controller cannot parse (contract violation): %v", m.Capacity, err)
			}
			if gb != goldenMemPerDIMMGB {
				t.Errorf("per-DIMM parse of %q: got %d GB, want %d GB", m.Capacity, gb, goldenMemPerDIMMGB)
			}
			totalMem += gb
		}
		if totalMem != goldenTotalMemoryGB {
			t.Errorf("total memory: got %d GB, want %d GB", totalMem, goldenTotalMemoryGB)
		}

		totalDisk := 0
		for _, d := range report.Disks {
			totalDisk += d.SizeGB
		}
		if totalDisk != goldenTotalDiskGB {
			t.Errorf("total disk: got %d GB, want %d GB", totalDisk, goldenTotalDiskGB)
		}
	})
}
