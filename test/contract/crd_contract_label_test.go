package contract

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
	"sigs.k8s.io/yaml"
)

// The two places the CRDs are installed from. config/crd/bases is what
// controller-gen writes; charts/beskar7/crds is the copy `make sync-chart-crds`
// makes for the Helm chart. Both must carry the same labels — the chart is the
// recommended install path and never goes through clusterctl.
var (
	generatedCRDDir = filepath.Join("..", "..", "config", "crd", "bases")
	chartCRDDir     = filepath.Join("..", "..", "charts", "beskar7", "crds")
	crdDirs         = []string{generatedCRDDir, chartCRDDir}
)

// crdFiles returns the four beskar7 CRD manifests in dir.
func crdFiles(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "infrastructure.cluster.x-k8s.io_*.yaml"))
	if err != nil || len(files) != 4 {
		t.Fatalf("%s: expected the four beskar7 CRDs, got %d (%v)", dir, len(files), err)
	}
	return files
}

// crdLabels returns metadata.labels of the CRD manifest at path.
func crdLabels(t *testing.T, path string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var crd struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
	}
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return crd.Metadata.Labels
}

// TestCRDsClaimOnlyTheV1Beta1Contract pins the contract version the CRDs
// advertise. CAPI resolves the NEWEST labelled contract, so claiming
// cluster.x-k8s.io/v1beta2 makes CAPI >= v1.11 read status through the v1beta2
// accessors — which model status.failureDomains as a list — while the v1beta1
// API still publishes the map. That read fails hard and aborts the Cluster's
// infrastructure reconcile (reproduced on CAPI v1.12.2, 2026-09-09). The
// v1beta2 label may only return together with the list-shaped API (D-023).
func TestCRDsClaimOnlyTheV1Beta1Contract(t *testing.T) {
	for _, dir := range crdDirs {
		for _, f := range crdFiles(t, dir) {
			labels := crdLabels(t, f)
			if got := labels["cluster.x-k8s.io/v1beta1"]; got != "v1beta1" {
				t.Errorf("%s: cluster.x-k8s.io/v1beta1 label = %q, want v1beta1", filepath.Base(f), got)
			}
			if got, ok := labels["cluster.x-k8s.io/v1beta2"]; ok {
				t.Errorf("%s: claims the v1beta2 contract (%q) but publishes v1beta1-shaped status; CAPI >= v1.11 cannot read status.failureDomains through that contract", filepath.Base(f), got)
			}
		}
	}
}

// TestCRDsCarryTheClusterctlLabels pins the labels `clusterctl move` keys on.
//
// clusterctl builds its move graph only from CRDs that carry the
// clusterctl.cluster.x-k8s.io label (getCRDList in cluster-api's
// cmd/clusterctl/client/cluster/objectgraph.go lists CRDs with
// client.HasLabels{ClusterctlLabel}). `clusterctl init` adds that label to
// everything it installs, but a Helm or release-manifest install never goes
// through clusterctl, so the generated CRDs have to carry it themselves —
// without it `clusterctl move` silently skipped every beskar7 object.
//
// PhysicalHost also needs clusterctl.cluster.x-k8s.io/move-hierarchy: nothing
// owns a host, so a discovered host never reaches a Cluster through owner
// references and would be left behind; the label force-moves each host together
// with the objects it owns. The other three kinds reach the move set through
// their owner chain (Cluster, Machine, or the Cluster owner reference CAPI puts
// on referenced templates) and must NOT be force-moved.
//
// cluster.x-k8s.io/provider is the provider contract's component label, with
// the value clusterctl's ManifestLabel derives for an infrastructure provider.
func TestCRDsCarryTheClusterctlLabels(t *testing.T) {
	for _, dir := range crdDirs {
		for _, f := range crdFiles(t, dir) {
			name := filepath.Base(f)
			labels := crdLabels(t, f)

			if got, ok := labels[clusterctlv1.ClusterctlLabel]; !ok {
				t.Errorf("%s: missing the %s label; `clusterctl move` only discovers CRDs that carry it", name, clusterctlv1.ClusterctlLabel)
			} else if got != "" {
				t.Errorf("%s: %s label = %q, want the empty value clusterctl init sets", name, clusterctlv1.ClusterctlLabel, got)
			}

			wantProvider := clusterctlv1.ManifestLabel("beskar7", clusterctlv1.InfrastructureProviderType)
			if got := labels[clusterv1.ProviderNameLabel]; got != wantProvider {
				t.Errorf("%s: %s label = %q, want %q", name, clusterv1.ProviderNameLabel, got, wantProvider)
			}

			isHost := strings.HasSuffix(name, "_physicalhosts.yaml")
			_, hasHierarchy := labels[clusterctlv1.ClusterctlMoveHierarchyLabel]
			if isHost && !hasHierarchy {
				t.Errorf("%s: missing the %s label; nothing owns a PhysicalHost, so without it `clusterctl move` discovers hosts and leaves them all behind", name, clusterctlv1.ClusterctlMoveHierarchyLabel)
			}
			if !isHost && hasHierarchy {
				t.Errorf("%s: carries the %s label; this kind is moved through its owner chain and must not be force-moved", name, clusterctlv1.ClusterctlMoveHierarchyLabel)
			}
			if _, ok := labels[clusterctlv1.ClusterctlMoveLabel]; ok {
				t.Errorf("%s: carries the %s label; PhysicalHost uses move-hierarchy (so owned objects follow) and the other kinds follow their owner chain", name, clusterctlv1.ClusterctlMoveLabel)
			}
		}
	}
}

// TestChartCRDsMatchGeneratedCRDs pins charts/beskar7/crds to config/crd/bases
// byte for byte. `make manifests` only regenerates the bases and CI only diffs
// those, so a chart copy that misses `make sync-chart-crds` would ship
// different CRDs (labels included) on the recommended install path.
func TestChartCRDsMatchGeneratedCRDs(t *testing.T) {
	for _, f := range crdFiles(t, generatedCRDDir) {
		want, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		chartCopy := filepath.Join(chartCRDDir, filepath.Base(f))
		got, err := os.ReadFile(chartCopy)
		if err != nil {
			t.Fatalf("%s: %v (run `make sync-chart-crds`)", chartCopy, err)
		}
		if !bytes.Equal(want, got) {
			t.Errorf("%s differs between config/crd/bases and charts/beskar7/crds; run `make sync-chart-crds`", filepath.Base(f))
		}
	}
}
