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

// Package rbac is a structural drift guard between Beskar7's generated RBAC
// (config/rbac/role.yaml, produced by controller-gen from +kubebuilder:rbac
// markers — the single source of truth) and its two hand-authored copies:
//
//   - charts/beskar7/templates/rbac.yaml (Helm chart)
//   - config/rbac/namespace-scoped/ (kustomize overlay for --watch-namespaces)
//
// Unlike the CRDs (which have `make sync-chart-crds`), nothing keeps these
// copies honest today. If a +kubebuilder:rbac marker changes and role.yaml
// is regenerated but the copies are not updated by hand, the drift is
// silent — the manager just gets fewer (or more) permissions than intended
// depending on which RBAC path is deployed. This package fails the build
// when that happens.
//
// The invariant enforced is structural, not textual: every Role/ClusterRole
// is normalized into a set of (apiGroup, resource, verb) triples (the
// apiGroups x resources x verbs cross-product of every rule), and the union
// of the namespace-scoped derivative's roles must equal role.yaml's triple
// set exactly. Ordering, rule grouping, resourceNames, and nonResourceURLs
// are deliberately ignored — none are used by Beskar7's RBAC today.
package rbac

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

// ruleTriple is one (apiGroup, resource, verb) grant.
type ruleTriple struct {
	Group    string
	Resource string
	Verb     string
}

func (t ruleTriple) String() string {
	return fmt.Sprintf("{group:%q resource:%q verb:%q}", t.Group, t.Resource, t.Verb)
}

// triplesFromRules flattens the apiGroups x resources x verbs cross-product
// of every PolicyRule into a set of triples. This is the normalization the
// whole drift guard rests on: two Roles that grant the same effective
// permissions but list their rules in a different order, or split them into
// a different number of rule blocks, compare equal.
func triplesFromRules(rules []rbacv1.PolicyRule) map[ruleTriple]struct{} {
	out := make(map[ruleTriple]struct{})
	for _, r := range rules {
		for _, g := range r.APIGroups {
			for _, res := range r.Resources {
				for _, v := range r.Verbs {
					out[ruleTriple{Group: g, Resource: res, Verb: v}] = struct{}{}
				}
			}
		}
	}
	return out
}

func unionTriples(sets ...map[ruleTriple]struct{}) map[ruleTriple]struct{} {
	out := make(map[ruleTriple]struct{})
	for _, s := range sets {
		for k := range s {
			out[k] = struct{}{}
		}
	}
	return out
}

// diffTriples returns the symmetric difference between want and got:
// missing are triples in want but absent from got (under-grant in got),
// extra are triples in got but absent from want (over-grant in got).
func diffTriples(want, got map[ruleTriple]struct{}) (missing, extra []ruleTriple) {
	for tr := range want {
		if _, ok := got[tr]; !ok {
			missing = append(missing, tr)
		}
	}
	for tr := range got {
		if _, ok := want[tr]; !ok {
			extra = append(extra, tr)
		}
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i].String() < missing[j].String() })
	sort.Slice(extra, func(i, j int) bool { return extra[i].String() < extra[j].String() })
	return missing, extra
}

// assertTriplesEqual fails t with a symmetric-diff report if got != want.
// want is always the generated config/rbac/role.yaml triple set; got is the
// derivative under test (a chart render or the kustomize overlay union).
func assertTriplesEqual(t *testing.T, label string, want, got map[ruleTriple]struct{}) {
	t.Helper()
	missing, extra := diffTriples(want, got)
	if len(missing) == 0 && len(extra) == 0 {
		return
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "RBAC drift between %s and config/rbac/role.yaml:\n", label)
	if len(missing) > 0 {
		sb.WriteString("  under-granted (present in role.yaml, missing here):\n")
		for _, tr := range missing {
			fmt.Fprintf(&sb, "    - %s\n", tr)
		}
	}
	if len(extra) > 0 {
		sb.WriteString("  over-granted (present here, not in role.yaml):\n")
		for _, tr := range extra {
			fmt.Fprintf(&sb, "    - %s\n", tr)
		}
	}
	t.Error(sb.String())
}

// yamlDocSeparator matches a bare "---" document separator line, which is
// the form every manifest in this repo (controller-gen output and Helm's
// rendered "# Source:" stream) uses.
var yamlDocSeparator = regexp.MustCompile(`(?m)^---[ \t]*$`)

func splitYAMLDocs(raw []byte) [][]byte {
	parts := yamlDocSeparator.Split(string(raw), -1)
	docs := make([][]byte, 0, len(parts))
	for _, p := range parts {
		if strings.TrimSpace(p) == "" {
			continue
		}
		docs = append(docs, []byte(p))
	}
	return docs
}

// collectRoles walks a multi-document YAML stream and returns every
// ClusterRole and Role object found, in document order. Everything else
// (RoleBinding, ClusterRoleBinding, ServiceAccount, Deployment, cert-manager
// CRs, webhook configurations, ...) is skipped by kind.
func collectRoles(t *testing.T, raw []byte) (clusterRoles []rbacv1.ClusterRole, roles []rbacv1.Role) {
	t.Helper()
	for _, doc := range splitYAMLDocs(raw) {
		var km struct {
			Kind string `json:"kind"`
		}
		if err := yaml.Unmarshal(doc, &km); err != nil {
			t.Fatalf("unmarshal document kind: %v\ndocument:\n%s", err, doc)
		}
		switch km.Kind {
		case "ClusterRole":
			var cr rbacv1.ClusterRole
			if err := yaml.Unmarshal(doc, &cr); err != nil {
				t.Fatalf("unmarshal ClusterRole: %v\ndocument:\n%s", err, doc)
			}
			clusterRoles = append(clusterRoles, cr)
		case "Role":
			var r rbacv1.Role
			if err := yaml.Unmarshal(doc, &r); err != nil {
				t.Fatalf("unmarshal Role: %v\ndocument:\n%s", err, doc)
			}
			roles = append(roles, r)
		}
	}
	return clusterRoles, roles
}

func findClusterRoleBySuffix(t *testing.T, crs []rbacv1.ClusterRole, suffix string) rbacv1.ClusterRole {
	t.Helper()
	var matches []rbacv1.ClusterRole
	for _, cr := range crs {
		if strings.HasSuffix(cr.Name, suffix) {
			matches = append(matches, cr)
		}
	}
	if len(matches) != 1 {
		names := make([]string, len(crs))
		for i, cr := range crs {
			names[i] = cr.Name
		}
		t.Fatalf("expected exactly 1 ClusterRole with name suffix %q, found %d (all ClusterRoles seen: %v)", suffix, len(matches), names)
	}
	return matches[0]
}

func findRoleBySuffix(t *testing.T, roles []rbacv1.Role, suffix string) rbacv1.Role {
	t.Helper()
	var matches []rbacv1.Role
	for _, r := range roles {
		if strings.HasSuffix(r.Name, suffix) {
			matches = append(matches, r)
		}
	}
	if len(matches) != 1 {
		names := make([]string, len(roles))
		for i, r := range roles {
			names[i] = r.Name
		}
		t.Fatalf("expected exactly 1 Role with name suffix %q, found %d (all Roles seen: %v)", suffix, len(matches), names)
	}
	return matches[0]
}

// repoRoot resolves the repository root by walking up from this source
// file's own path (via runtime.Caller) until it finds a directory
// containing go.mod. This is independent of the test binary's working
// directory, which `go test` sets to the package directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed to resolve this file's path")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not locate repo root: no go.mod found walking up from %s", file)
		}
		dir = parent
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

// loadGeneratedManagerRole loads config/rbac/role.yaml — the controller-gen
// source of truth — and returns its rule set as triples. Every other test
// in this package compares against this.
func loadGeneratedManagerRole(t *testing.T, root string) map[ruleTriple]struct{} {
	t.Helper()
	raw := mustReadFile(t, filepath.Join(root, "config", "rbac", "role.yaml"))
	crs, roles := collectRoles(t, raw)
	if len(roles) != 0 {
		t.Fatalf("config/rbac/role.yaml: expected no namespaced Role objects, found %d", len(roles))
	}
	if len(crs) != 1 {
		t.Fatalf("config/rbac/role.yaml: expected exactly 1 ClusterRole, found %d", len(crs))
	}
	if crs[0].Name != "manager-role" {
		t.Fatalf("config/rbac/role.yaml: expected ClusterRole named %q, found %q", "manager-role", crs[0].Name)
	}
	return triplesFromRules(crs[0].Rules)
}

// TestKustomizeRBACMatchesGeneratedRole is invariant (C): the union of the
// kustomize namespace-scoped overlay's three role files must equal
// config/rbac/role.yaml. This is pure Go (no external tooling) and must
// always run — it is the one guard in this package that CI can never skip.
func TestKustomizeRBACMatchesGeneratedRole(t *testing.T) {
	root := repoRoot(t)
	want := loadGeneratedManagerRole(t, root)

	overlayDir := filepath.Join(root, "config", "rbac", "namespace-scoped")
	minimalRaw := mustReadFile(t, filepath.Join(overlayDir, "minimal-clusterrole.yaml"))
	leaderRaw := mustReadFile(t, filepath.Join(overlayDir, "leader-election-role.yaml"))
	watchRaw := mustReadFile(t, filepath.Join(overlayDir, "watch-role.template.yaml"))
	watchRaw = bytes.ReplaceAll(watchRaw, []byte("REPLACE_WITH_WATCHED_NAMESPACE"), []byte("dummy-namespace"))

	minimalCRs, _ := collectRoles(t, minimalRaw)
	_, leaderRoles := collectRoles(t, leaderRaw)
	_, watchRoles := collectRoles(t, watchRaw)

	if len(minimalCRs) != 1 {
		t.Fatalf("minimal-clusterrole.yaml: expected exactly 1 ClusterRole, found %d", len(minimalCRs))
	}
	if len(leaderRoles) != 1 {
		t.Fatalf("leader-election-role.yaml: expected exactly 1 Role, found %d", len(leaderRoles))
	}
	if len(watchRoles) != 1 {
		t.Fatalf("watch-role.template.yaml: expected exactly 1 Role, found %d", len(watchRoles))
	}

	got := unionTriples(
		triplesFromRules(minimalCRs[0].Rules),
		triplesFromRules(leaderRoles[0].Rules),
		triplesFromRules(watchRoles[0].Rules),
	)

	assertTriplesEqual(t, "kustomize overlay config/rbac/namespace-scoped/ (minimal-clusterrole + leader-election-role + watch-role.template)", want, got)
}

func helmAvailable() bool {
	_, err := exec.LookPath("helm")
	return err == nil
}

// renderHelmTemplate shells out to `helm template` against the in-tree
// chart. The release name is fixed and arbitrary — every assertion in this
// package selects rendered objects by name *suffix*, not by full name,
// specifically so it doesn't care what release name (or prefix) produced
// them.
func renderHelmTemplate(t *testing.T, root string, extraArgs ...string) []byte {
	t.Helper()
	args := append([]string{"template", "rbac-driftguard", filepath.Join(root, "charts", "beskar7")}, extraArgs...)
	cmd := exec.Command("helm", args...) // #nosec G204 -- args are a fixed literal + repo-relative path, no user input
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("helm template failed: %v\nstderr:\n%s", err, stderr.String())
	}
	return stdout.Bytes()
}

// TestHelmChartClusterWideRBACMatchesGeneratedRole is invariant (A): with
// watchNamespaces empty (the chart default), the single rendered
// "*-manager-role" ClusterRole must equal config/rbac/role.yaml.
//
// Requires `helm` on PATH. CI installs it for this job specifically so this
// check runs (not skips) in CI — see the "Install Helm" step in the unit
// test job of .github/workflows/ci.yml. Locally, without helm, this
// sub-test skips with an explanatory message; it does not fail the whole
// package.
func TestHelmChartClusterWideRBACMatchesGeneratedRole(t *testing.T) {
	if !helmAvailable() {
		t.Skip("helm not found on PATH; skipping chart-vs-role.yaml drift check (CI installs helm for this job; see .github/workflows/ci.yml)")
	}
	root := repoRoot(t)
	want := loadGeneratedManagerRole(t, root)

	rendered := renderHelmTemplate(t, root)
	crs, roles := collectRoles(t, rendered)
	if len(roles) != 0 {
		t.Fatalf("chart render (watchNamespaces empty): expected no namespaced Role objects, found %d", len(roles))
	}
	cr := findClusterRoleBySuffix(t, crs, "-manager-role")

	assertTriplesEqual(t, "Helm chart cluster-wide branch (watchNamespaces empty, ClusterRole *-manager-role)", want, triplesFromRules(cr.Rules))
}

// TestHelmChartNamespacedRBACMatchesGeneratedRole is invariant (B): with
// watchNamespaces set to one namespace, the union of the rendered
// "*-manager-clusterscope" ClusterRole, "*-manager-leaderelection" Role, and
// "*-manager-watch" Role must equal config/rbac/role.yaml.
//
// Same helm-availability caveat as TestHelmChartClusterWideRBACMatchesGeneratedRole.
func TestHelmChartNamespacedRBACMatchesGeneratedRole(t *testing.T) {
	if !helmAvailable() {
		t.Skip("helm not found on PATH; skipping chart-vs-role.yaml drift check (CI installs helm for this job; see .github/workflows/ci.yml)")
	}
	root := repoRoot(t)
	want := loadGeneratedManagerRole(t, root)

	rendered := renderHelmTemplate(t, root, "--set", "watchNamespaces={rbac-driftguard-test-ns}")
	crs, roles := collectRoles(t, rendered)

	clusterscope := findClusterRoleBySuffix(t, crs, "-manager-clusterscope")
	leaderelection := findRoleBySuffix(t, roles, "-manager-leaderelection")
	watch := findRoleBySuffix(t, roles, "-manager-watch")

	got := unionTriples(
		triplesFromRules(clusterscope.Rules),
		triplesFromRules(leaderelection.Rules),
		triplesFromRules(watch.Rules),
	)

	assertTriplesEqual(t, "Helm chart namespaced branch (watchNamespaces=[rbac-driftguard-test-ns]: *-manager-clusterscope + *-manager-leaderelection + *-manager-watch)", want, got)
}
