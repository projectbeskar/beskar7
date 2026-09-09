package contract

import (
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"
)

// TestCRDsClaimOnlyTheV1Beta1Contract pins the contract version the CRDs
// advertise. CAPI resolves the NEWEST labelled contract, so claiming
// cluster.x-k8s.io/v1beta2 makes CAPI >= v1.11 read status through the v1beta2
// accessors — which model status.failureDomains as a list — while the v1beta1
// API still publishes the map. That read fails hard and aborts the Cluster's
// infrastructure reconcile (reproduced on CAPI v1.12.2, 2026-09-09). The
// v1beta2 label may only return together with the list-shaped API (D-023).
func TestCRDsClaimOnlyTheV1Beta1Contract(t *testing.T) {
	for _, dir := range []string{
		filepath.Join("..", "..", "config", "crd", "bases"),
		filepath.Join("..", "..", "charts", "beskar7", "crds"),
	} {
		files, err := filepath.Glob(filepath.Join(dir, "infrastructure.cluster.x-k8s.io_*.yaml"))
		if err != nil || len(files) != 4 {
			t.Fatalf("%s: expected the four beskar7 CRDs, got %d (%v)", dir, len(files), err)
		}
		for _, f := range files {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			var crd struct {
				Metadata struct {
					Labels map[string]string `json:"labels"`
				} `json:"metadata"`
			}
			if err := yaml.Unmarshal(raw, &crd); err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			if got := crd.Metadata.Labels["cluster.x-k8s.io/v1beta1"]; got != "v1beta1" {
				t.Errorf("%s: cluster.x-k8s.io/v1beta1 label = %q, want v1beta1", filepath.Base(f), got)
			}
			if got, ok := crd.Metadata.Labels["cluster.x-k8s.io/v1beta2"]; ok {
				t.Errorf("%s: claims the v1beta2 contract (%q) but publishes v1beta1-shaped status; CAPI >= v1.11 cannot read status.failureDomains through that contract", filepath.Base(f), got)
			}
		}
	}
}
