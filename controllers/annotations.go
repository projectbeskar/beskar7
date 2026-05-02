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

// BootstrapURLAnnotation is set by the Beskar7Machine controller on a PhysicalHost
// to signal the computed per-host bootstrap URL. The PhysicalHost controller reads
// this annotation, persists the value to Status.Bootstrap.URL, and then removes the
// annotation so it is not acted on again.
//
// Following the same "signal via annotation, status written by owner" pattern as
// InspectionRequestAnnotation (BUG-1 fix). Defined here so both controllers share
// the constant without creating an import cycle.
const BootstrapURLAnnotation = "infrastructure.cluster.x-k8s.io/bootstrap-url"
