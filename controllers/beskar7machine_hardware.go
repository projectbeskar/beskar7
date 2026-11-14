/*
Copyright 2024 The Beskar7 Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/labels"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
)

// hardwareShortfall is the first requirement an inspection report misses.
type hardwareShortfall struct {
	check    string
	found    int32
	required int32
	what     string
	unit     string
}

func (s *hardwareShortfall) message() string {
	return fmt.Sprintf("insufficient %s: found %d%s, required %d%s", s.what, s.found, s.unit, s.required, s.unit)
}

// checkHardwareRequirements aggregates the report's CPU cores, memory and disk
// and compares them with reqs, checking CPU, then memory, then disk. It returns
// nil when the report meets them (or reqs is nil). Memory entries whose capacity
// cannot be parsed count as zero and are returned as errors for the caller to log.
// MinDiskGB is the sum over all inspected disks, not any single disk.
func checkHardwareRequirements(reqs *infrav1.HardwareRequirements, report *infrav1.InspectionReport) (*hardwareShortfall, []error) {
	if reqs == nil || report == nil {
		return nil, nil
	}
	var totalCores int32
	for _, cpu := range report.CPUs {
		totalCores += cpu.Cores
	}
	if reqs.MinCPUCores > 0 && totalCores < reqs.MinCPUCores {
		return &hardwareShortfall{check: "MinCPUCores", found: totalCores, required: reqs.MinCPUCores, what: "CPU cores"}, nil
	}

	var totalMemoryGB int32
	var errs []error
	for _, mem := range report.Memory {
		memGB, err := parseMemoryCapacityGB(mem.Capacity)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		totalMemoryGB += int32(memGB)
	}
	if reqs.MinMemoryGB > 0 && totalMemoryGB < reqs.MinMemoryGB {
		return &hardwareShortfall{check: "MinMemoryGB", found: totalMemoryGB, required: reqs.MinMemoryGB, what: "memory", unit: " GB"}, errs
	}

	if reqs.MinDiskGB > 0 {
		var totalDisk int32
		for _, disk := range report.Disks {
			totalDisk += disk.SizeGB
		}
		if totalDisk < reqs.MinDiskGB {
			return &hardwareShortfall{check: "MinDiskGB", found: totalDisk, required: reqs.MinDiskGB, what: "disk space", unit: " GB"}, errs
		}
	}
	return nil, errs
}

// claimTimeHardwareMatching reports whether the machine filters claim
// candidates by hardware. With no minimum set there is nothing to match.
func claimTimeHardwareMatching(b7machine *infrav1.Beskar7Machine) bool {
	reqs := b7machine.Spec.HardwareRequirements
	return reqs != nil && reqs.MatchAt == infrav1.HardwareMatchAtClaim &&
		(reqs.MinCPUCores > 0 || reqs.MinMemoryGB > 0 || reqs.MinDiskGB > 0)
}

type claimSkipReason int

const (
	claimSkipNone claimSkipReason = iota
	claimSkipNetworkData
	claimSkipNoReport
	claimSkipHardware
)

// claimSkip says whether the claim passes over an Available host, and why.
func claimSkip(b7machine *infrav1.Beskar7Machine, host *infrav1.PhysicalHost) (claimSkipReason, string) {
	if hostWaitingForNetworkData(host) {
		return claimSkipNetworkData, networkDataWaitMessage(host)
	}
	if !claimTimeHardwareMatching(b7machine) {
		return claimSkipNone, ""
	}
	report := host.Status.InspectionReport
	if report == nil {
		return claimSkipNoReport, "no inspection report"
	}
	if shortfall, _ := checkHardwareRequirements(b7machine.Spec.HardwareRequirements, report); shortfall != nil {
		return claimSkipHardware, shortfall.message()
	}
	return claimSkipNone, ""
}

// claimConstraintDescription names what a NoMatchingPhysicalHost wait is
// waiting on: the placement constraint, the claim-time hardware requirements,
// or both.
func claimConstraintDescription(b7machine *infrav1.Beskar7Machine, placement labels.Selector) string {
	var parts []string
	if placement != nil {
		parts = append(parts, "the placement constraint "+placement.String())
	}
	if claimTimeHardwareMatching(b7machine) {
		reqs := b7machine.Spec.HardwareRequirements
		var mins []string
		if reqs.MinCPUCores > 0 {
			mins = append(mins, fmt.Sprintf("minCPUCores=%d", reqs.MinCPUCores))
		}
		if reqs.MinMemoryGB > 0 {
			mins = append(mins, fmt.Sprintf("minMemoryGB=%d", reqs.MinMemoryGB))
		}
		if reqs.MinDiskGB > 0 {
			mins = append(mins, fmt.Sprintf("minDiskGB=%d", reqs.MinDiskGB))
		}
		parts = append(parts, "the hardware requirements (matchAt: Claim, "+strings.Join(mins, ", ")+")")
	}
	return strings.Join(parts, " and ")
}
