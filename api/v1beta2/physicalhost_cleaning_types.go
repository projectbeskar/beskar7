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

package v1beta2

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CleaningMode selects what happens to a host's disks when its claim is released.
// +kubebuilder:validation:Enum=Disabled;Metadata
type CleaningMode string

const (
	// CleaningModeDisabled leaves the disks as they are on release.
	CleaningModeDisabled CleaningMode = "Disabled"
	// CleaningModeMetadata wipes partition tables and filesystem signatures on
	// every disk on release. It is not a secure erase.
	CleaningModeMetadata CleaningMode = "Metadata"
)

// CleaningSpec configures the disk wipe of a released host.
type CleaningSpec struct {
	// Mode selects what happens to the host's disks when its claim is
	// released. Disabled (the default) leaves them as they are. Metadata takes
	// the host through Cleaning before it is Available again: the inspector
	// boots in clean mode and zeroes the first and last 8 MiB of every disk
	// and partition, removing the partition tables, their GPT backups and
	// the filesystem, RAID and LVM signatures, so nothing stale boots or
	// assembles. The data blocks, including the previous join secret, stay on
	// the disk and are recoverable: it is not a secure erase. A host with no
	// state yet, new or moved by clusterctl, is wiped too. A change takes effect
	// at the next release; setting Disabled while the host is Cleaning, or held
	// in Error after a failed run, makes the host Available as it is.
	// +kubebuilder:default=Disabled
	// +optional
	Mode CleaningMode `json:"mode,omitempty"`

	// Request is an opaque string that retries a failed cleaning run. When it
	// changes to a new non-empty value while the host is held in Error after a
	// failed or timed-out run, the controller starts one new run. A change in
	// any other state starts nothing. Every change is recorded in
	// status.cleaning.observedRequest, so a value set earlier never retries a
	// later failure on its own.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	Request string `json:"request,omitempty"`
}

// CleaningStatus is the state of a host's disk wipe runs.
type CleaningStatus struct {
	// ObservedRequest is the last spec.cleaning.request the controller has
	// seen, whether or not it started a run.
	// +optional
	ObservedRequest string `json:"observedRequest,omitempty"`

	// StartedAt is when the controller booted the host into the current
	// cleaning run. The cleaning timeout is measured from it. Cleared when
	// the run ends.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
}

// CleaningMode returns spec.cleaning.mode, or Disabled when unset.
func (h *PhysicalHost) CleaningMode() CleaningMode {
	if h.Spec.Cleaning == nil || h.Spec.Cleaning.Mode == "" {
		return CleaningModeDisabled
	}
	return h.Spec.Cleaning.Mode
}

// CleaningRequest returns spec.cleaning.request.
func (h *PhysicalHost) CleaningRequest() string {
	if h.Spec.Cleaning == nil {
		return ""
	}
	return h.Spec.Cleaning.Request
}

// ObservedCleaningRequest returns status.cleaning.observedRequest.
func (h *PhysicalHost) ObservedCleaningRequest() string {
	if h.Status.Cleaning == nil {
		return ""
	}
	return h.Status.Cleaning.ObservedRequest
}

// SetObservedCleaningRequest records request in status.cleaning.observedRequest.
func (h *PhysicalHost) SetObservedCleaningRequest(request string) {
	h.ensureCleaningStatus().ObservedRequest = request
}

// CleaningStartedAt returns status.cleaning.startedAt.
func (h *PhysicalHost) CleaningStartedAt() *metav1.Time {
	if h.Status.Cleaning == nil {
		return nil
	}
	return h.Status.Cleaning.StartedAt
}

// SetCleaningStartedAt sets status.cleaning.startedAt; nil clears it.
func (h *PhysicalHost) SetCleaningStartedAt(t *metav1.Time) {
	if t == nil && h.Status.Cleaning == nil {
		return
	}
	h.ensureCleaningStatus().StartedAt = t
}

func (h *PhysicalHost) ensureCleaningStatus() *CleaningStatus {
	if h.Status.Cleaning == nil {
		h.Status.Cleaning = &CleaningStatus{}
	}
	return h.Status.Cleaning
}
