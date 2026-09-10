package contract

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
	"sigs.k8s.io/yaml"
)

// TestClusterctlMetadataMatchesTheCRDContract pins metadata.yaml, the file
// `clusterctl init` reads to learn which Cluster API contract a beskar7
// release series speaks, to the contract the CRDs actually declare.
//
// clusterctl uses the metadata contract during init/upgrade (a mismatch with
// the core provider's contract aborts the install); at runtime CAPI uses the
// CRD label. The two must agree, and metadata.yaml must be valid under
// clusterctl's strict validation (apiVersion, kind, at least one series).
// The newest series is listed first: CI's clusterctl smoke installs it.
func TestClusterctlMetadataMatchesTheCRDContract(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "metadata.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var meta clusterctlv1.Metadata
	if err := yaml.UnmarshalStrict(raw, &meta); err != nil {
		t.Fatalf("metadata.yaml: %v", err)
	}
	if meta.APIVersion != clusterctlv1.GroupVersion.String() {
		t.Errorf("metadata.yaml apiVersion = %q, want %q", meta.APIVersion, clusterctlv1.GroupVersion.String())
	}
	if meta.Kind != "Metadata" {
		t.Errorf("metadata.yaml kind = %q, want Metadata", meta.Kind)
	}
	if len(meta.ReleaseSeries) == 0 {
		t.Fatal("metadata.yaml: releaseSeries must list at least one series")
	}

	newest := meta.ReleaseSeries[0]
	for _, rs := range meta.ReleaseSeries[1:] {
		if rs.Major > newest.Major || (rs.Major == newest.Major && rs.Minor > newest.Minor) {
			t.Errorf("metadata.yaml: release series %d.%d is listed after the older %d.%d — keep the newest first (CI installs the first one)",
				rs.Major, rs.Minor, newest.Major, newest.Minor)
		}
	}

	// The contract the CRDs declare is the version suffix of the
	// cluster.x-k8s.io/<contract> label; the label's value is the served
	// apiVersion (pinned by TestCRDsClaimTheV1Beta2Contract).
	crdContract := ""
	for key := range crdLabels(t, filepath.Join(generatedCRDDir, "infrastructure.cluster.x-k8s.io_beskar7clusters.yaml")) {
		if strings.HasPrefix(key, "cluster.x-k8s.io/v1") {
			crdContract = strings.TrimPrefix(key, "cluster.x-k8s.io/")
		}
	}
	if crdContract == "" {
		t.Fatal("beskar7clusters CRD carries no cluster.x-k8s.io/<contract> label")
	}
	if newest.Contract != crdContract {
		t.Errorf("metadata.yaml newest series %d.%d declares contract %q but the CRDs declare %q",
			newest.Major, newest.Minor, newest.Contract, crdContract)
	}
}

// TestComponentsVariablesResolveForKubectl pins the two consumers of the
// kustomize overlay. `clusterctl init` gets the raw components with
// ${VAR:=default} placeholders; `make deploy` and the plain-kubectl release
// manifest run them through RESOLVE_CLUSTERCTL_DEFAULTS (a sed in the
// Makefile). Every variable must therefore carry a default in exactly the
// form that sed understands, or the kubectl manifest ships a literal `${…}`
// as a manager argument.
func TestComponentsVariablesResolveForKubectl(t *testing.T) {
	// The Makefile's sed, transcribed: `\$\{[A-Za-z_][A-Za-z0-9_]*:=([^}]*)\}` → `\1`.
	resolver := regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*:=([^}]*)\}`)
	anyVariable := regexp.MustCompile(`\$\{`)

	var files []string
	for _, dir := range []string{"manager", "rbac", "webhook", "certmanager", "security", "default"} {
		matches, err := filepath.Glob(filepath.Join("..", "..", "config", dir, "*.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, matches...)
	}
	sawVariable := false
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if !anyVariable.Match(raw) {
			continue
		}
		sawVariable = true
		resolved := resolver.ReplaceAll(raw, []byte("$1"))
		if anyVariable.Match(resolved) {
			t.Errorf("%s: a clusterctl variable has no ${VAR:=default} form; the kubectl manifest would ship it unresolved", f)
		}
		for _, m := range resolver.FindAllSubmatch(raw, -1) {
			if !strings.HasPrefix(string(m[0]), "${BESKAR7_") {
				t.Errorf("%s: clusterctl variables must be prefixed with the provider name, got %s", f, m[0])
			}
		}
	}
	if !sawVariable {
		t.Error("expected at least one clusterctl variable in the kustomize overlay (BESKAR7_BOOTSTRAP_URL_BASE); the Makefile resolver and the docs table would be dead")
	}
}
