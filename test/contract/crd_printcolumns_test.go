/*
Copyright 2026 The Beskar7 Authors.

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

package contract

import (
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"
)

// The Beskar7Cluster Endpoint column must show the endpoint in effect. Since
// D-027 the endpoint comes from Cluster.spec when that is set, so
// spec.controlPlaneEndpoint is empty for those clusters and the column read
// blank; status.controlPlaneEndpoint is what the controller resolved.
func TestBeskar7ClusterEndpointColumnShowsTheEndpointInEffect(t *testing.T) {
	for _, dir := range crdDirs {
		path := filepath.Join(dir, "infrastructure.cluster.x-k8s.io_beskar7clusters.yaml")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var crd struct {
			Spec struct {
				Versions []struct {
					Name    string `json:"name"`
					Columns []struct {
						Name     string `json:"name"`
						JSONPath string `json:"jsonPath"`
					} `json:"additionalPrinterColumns"`
				} `json:"versions"`
			} `json:"spec"`
		}
		if err := yaml.Unmarshal(raw, &crd); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		found := false
		for _, v := range crd.Spec.Versions {
			for _, c := range v.Columns {
				if c.Name != "Endpoint" {
					continue
				}
				found = true
				if c.JSONPath != ".status.controlPlaneEndpoint.host" {
					t.Errorf("%s (%s): Endpoint column reads %q, want .status.controlPlaneEndpoint.host", path, v.Name, c.JSONPath)
				}
			}
		}
		if !found {
			t.Errorf("%s: no Endpoint column", path)
		}
	}
}
