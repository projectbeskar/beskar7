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

	corev1 "k8s.io/api/core/v1"
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
	if crs[0].Name != "capb7-manager-role" {
		t.Fatalf("config/rbac/role.yaml: expected ClusterRole named %q, found %q", "capb7-manager-role", crs[0].Name)
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

// ---------------------------------------------------------------------------
// Binding / subject-graph guard (SEC-2 follow-up).
//
// The triple-set guards above prove every Role/ClusterRole carries the right
// *rules*. They say nothing about whether that Role/ClusterRole is actually
// *bound* to the manager's ServiceAccount: a RoleBinding with a stale
// roleRef.name, or a subject with the wrong name/namespace, silently denies
// the manager its permissions without tripping any rule-content check. This
// section closes that gap by walking the binding graph directly.
// ---------------------------------------------------------------------------

// collectBindings walks a multi-document YAML stream and returns every
// RoleBinding and ClusterRoleBinding object found, in document order.
// Everything else is skipped by kind, mirroring collectRoles.
func collectBindings(t *testing.T, raw []byte) (roleBindings []rbacv1.RoleBinding, clusterRoleBindings []rbacv1.ClusterRoleBinding) {
	t.Helper()
	for _, doc := range splitYAMLDocs(raw) {
		var km struct {
			Kind string `json:"kind"`
		}
		if err := yaml.Unmarshal(doc, &km); err != nil {
			t.Fatalf("unmarshal document kind: %v\ndocument:\n%s", err, doc)
		}
		switch km.Kind {
		case "RoleBinding":
			var rb rbacv1.RoleBinding
			if err := yaml.Unmarshal(doc, &rb); err != nil {
				t.Fatalf("unmarshal RoleBinding: %v\ndocument:\n%s", err, doc)
			}
			roleBindings = append(roleBindings, rb)
		case "ClusterRoleBinding":
			var crb rbacv1.ClusterRoleBinding
			if err := yaml.Unmarshal(doc, &crb); err != nil {
				t.Fatalf("unmarshal ClusterRoleBinding: %v\ndocument:\n%s", err, doc)
			}
			clusterRoleBindings = append(clusterRoleBindings, crb)
		}
	}
	return roleBindings, clusterRoleBindings
}

// findServiceAccount walks a multi-document YAML stream and returns the
// single ServiceAccount object found. label identifies the source in
// failure messages.
func findServiceAccount(t *testing.T, raw []byte, label string) corev1.ServiceAccount {
	t.Helper()
	var found []corev1.ServiceAccount
	for _, doc := range splitYAMLDocs(raw) {
		var km struct {
			Kind string `json:"kind"`
		}
		if err := yaml.Unmarshal(doc, &km); err != nil {
			t.Fatalf("unmarshal document kind: %v\ndocument:\n%s", err, doc)
		}
		if km.Kind != "ServiceAccount" {
			continue
		}
		var sa corev1.ServiceAccount
		if err := yaml.Unmarshal(doc, &sa); err != nil {
			t.Fatalf("unmarshal ServiceAccount: %v\ndocument:\n%s", err, doc)
		}
		found = append(found, sa)
	}
	if len(found) != 1 {
		t.Fatalf("%s: expected exactly 1 ServiceAccount, found %d", label, len(found))
	}
	return found[0]
}

// bindingTarget identifies what a binding's roleRef resolves to: a role
// kind ("Role" or "ClusterRole") and name. Namespace is set only for a
// Role target (taken from the binding's own namespace, since a RoleBinding
// can only reference a Role that lives alongside it) — ClusterRole targets
// are cluster-scoped and carry no namespace.
type bindingTarget struct {
	Kind      string
	Name      string
	Namespace string
}

func (bt bindingTarget) String() string {
	if bt.Namespace == "" {
		return fmt.Sprintf("%s/%s", bt.Kind, bt.Name)
	}
	return fmt.Sprintf("%s/%s/%s", bt.Kind, bt.Namespace, bt.Name)
}

// roleTargetSet returns the bindingTarget for every collected Role and
// ClusterRole — the set of roles that must each be bound exactly once.
func roleTargetSet(clusterRoles []rbacv1.ClusterRole, roles []rbacv1.Role) map[bindingTarget]struct{} {
	out := make(map[bindingTarget]struct{})
	for _, cr := range clusterRoles {
		out[bindingTarget{Kind: "ClusterRole", Name: cr.Name}] = struct{}{}
	}
	for _, r := range roles {
		out[bindingTarget{Kind: "Role", Name: r.Name, Namespace: r.Namespace}] = struct{}{}
	}
	return out
}

// bindingDescriptor pairs a human-readable binding identity with the
// bindingTarget its roleRef resolves to and its raw subjects.
type bindingDescriptor struct {
	Label    string
	Target   bindingTarget
	Subjects []rbacv1.Subject
}

func describeBindings(roleBindings []rbacv1.RoleBinding, clusterRoleBindings []rbacv1.ClusterRoleBinding) []bindingDescriptor {
	out := make([]bindingDescriptor, 0, len(roleBindings)+len(clusterRoleBindings))
	for _, rb := range roleBindings {
		ns := ""
		if rb.RoleRef.Kind == "Role" {
			ns = rb.Namespace
		}
		out = append(out, bindingDescriptor{
			Label:    fmt.Sprintf("RoleBinding/%s/%s", rb.Namespace, rb.Name),
			Target:   bindingTarget{Kind: rb.RoleRef.Kind, Name: rb.RoleRef.Name, Namespace: ns},
			Subjects: rb.Subjects,
		})
	}
	for _, crb := range clusterRoleBindings {
		out = append(out, bindingDescriptor{
			Label:    fmt.Sprintf("ClusterRoleBinding/%s", crb.Name),
			Target:   bindingTarget{Kind: crb.RoleRef.Kind, Name: crb.RoleRef.Name},
			Subjects: crb.Subjects,
		})
	}
	return out
}

func hasManagerSASubject(subjects []rbacv1.Subject, sa corev1.ServiceAccount) bool {
	for _, s := range subjects {
		if s.Kind == "ServiceAccount" && s.Name == sa.Name && s.Namespace == sa.Namespace {
			return true
		}
	}
	return false
}

func subjectsString(subjects []rbacv1.Subject) string {
	if len(subjects) == 0 {
		return "(none)"
	}
	parts := make([]string, len(subjects))
	for i, s := range subjects {
		parts[i] = fmt.Sprintf("%s/%s/%s", s.Kind, s.Namespace, s.Name)
	}
	return strings.Join(parts, ", ")
}

// assertBindingsWireRolesToSA enforces the binding-graph invariant for one
// RBAC topology: every Role/ClusterRole in clusterRoles+roles must be bound
// to sa by exactly one of roleBindings+clusterRoleBindings, with no
// dangling roleRef (a binding pointing at a role that doesn't exist) and no
// wrong-identity subject.
func assertBindingsWireRolesToSA(
	t *testing.T,
	label string,
	clusterRoles []rbacv1.ClusterRole,
	roles []rbacv1.Role,
	roleBindings []rbacv1.RoleBinding,
	clusterRoleBindings []rbacv1.ClusterRoleBinding,
	sa corev1.ServiceAccount,
) {
	t.Helper()
	targets := roleTargetSet(clusterRoles, roles)
	bindings := describeBindings(roleBindings, clusterRoleBindings)

	var errs []string

	// Dangling roleRef: a binding whose target isn't a role we collected.
	boundBy := make(map[bindingTarget][]bindingDescriptor)
	for _, b := range bindings {
		if _, ok := targets[b.Target]; !ok {
			errs = append(errs, fmt.Sprintf("%s has roleRef %s, which does not match any known Role/ClusterRole (dangling roleRef)", b.Label, b.Target))
			continue
		}
		boundBy[b.Target] = append(boundBy[b.Target], b)
	}

	// Unbound roles, multiply-bound roles, and subject correctness.
	for target := range targets {
		bs := boundBy[target]
		switch len(bs) {
		case 0:
			errs = append(errs, fmt.Sprintf("%s has no binding pointing at it (unbound role)", target))
		case 1:
			b := bs[0]
			if !hasManagerSASubject(b.Subjects, sa) {
				errs = append(errs, fmt.Sprintf("%s is bound by %s, but it has no ServiceAccount subject matching the manager SA %s/%s (subjects found: %s)", target, b.Label, sa.Namespace, sa.Name, subjectsString(b.Subjects)))
			}
		default:
			names := make([]string, len(bs))
			for i, b := range bs {
				names[i] = b.Label
			}
			sort.Strings(names)
			errs = append(errs, fmt.Sprintf("%s is bound by %d bindings (expected exactly 1): %s", target, len(bs), strings.Join(names, ", ")))
		}
	}

	if len(errs) == 0 {
		return
	}
	sort.Strings(errs)
	var sb strings.Builder
	fmt.Fprintf(&sb, "RBAC binding-graph drift in %s (manager ServiceAccount %s/%s):\n", label, sa.Namespace, sa.Name)
	for _, e := range errs {
		fmt.Fprintf(&sb, "  - %s\n", e)
	}
	t.Error(sb.String())
}

// TestKustomizeRBACBindingsWireToManagerSA is the binding-graph counterpart
// to TestKustomizeRBACMatchesGeneratedRole: it proves every Role/ClusterRole
// in both kustomize topologies (cluster-wide config/rbac/, and the
// namespace-scoped overlay) is actually bound to the manager ServiceAccount,
// not just that the rule content matches. Pure Go, no external tooling —
// must always run.
func TestKustomizeRBACBindingsWireToManagerSA(t *testing.T) {
	root := repoRoot(t)
	saRaw := mustReadFile(t, filepath.Join(root, "config", "rbac", "service_account.yaml"))
	sa := findServiceAccount(t, saRaw, "config/rbac/service_account.yaml")

	t.Run("cluster-wide", func(t *testing.T) {
		roleRaw := mustReadFile(t, filepath.Join(root, "config", "rbac", "role.yaml"))
		bindingRaw := mustReadFile(t, filepath.Join(root, "config", "rbac", "role_binding.yaml"))
		crs, roles := collectRoles(t, roleRaw)
		rbs, crbs := collectBindings(t, bindingRaw)
		assertBindingsWireRolesToSA(t, "cluster-wide kustomize (config/rbac/role.yaml + role_binding.yaml)", crs, roles, rbs, crbs, sa)
	})

	t.Run("namespace-scoped", func(t *testing.T) {
		overlayDir := filepath.Join(root, "config", "rbac", "namespace-scoped")
		minimalRaw := mustReadFile(t, filepath.Join(overlayDir, "minimal-clusterrole.yaml"))
		leaderRaw := mustReadFile(t, filepath.Join(overlayDir, "leader-election-role.yaml"))
		watchRaw := mustReadFile(t, filepath.Join(overlayDir, "watch-role.template.yaml"))
		watchRaw = bytes.ReplaceAll(watchRaw, []byte("REPLACE_WITH_WATCHED_NAMESPACE"), []byte("dummy-namespace"))

		var crs []rbacv1.ClusterRole
		var roles []rbacv1.Role
		var rbs []rbacv1.RoleBinding
		var crbs []rbacv1.ClusterRoleBinding
		for _, raw := range [][]byte{minimalRaw, leaderRaw, watchRaw} {
			c, r := collectRoles(t, raw)
			crs = append(crs, c...)
			roles = append(roles, r...)
			rb, crb := collectBindings(t, raw)
			rbs = append(rbs, rb...)
			crbs = append(crbs, crb...)
		}
		assertBindingsWireRolesToSA(t, "kustomize namespace-scoped overlay (config/rbac/namespace-scoped/, watched namespace substituted for the template placeholder)", crs, roles, rbs, crbs, sa)
	})
}

// TestHelmChartRBACBindingsWireToManagerSA is the binding-graph counterpart
// to the two Helm chart triple-set tests: it proves every rendered
// Role/ClusterRole, in both the cluster-wide (watchNamespaces empty) and
// namespaced (watchNamespaces set) branches, is bound to the chart's own
// rendered ServiceAccount — read from the render itself, never hardcoded,
// since the SA name is templated on the release name.
//
// Rendered with an explicit --namespace so beskar7.namespace (which falls
// back to .Release.Namespace when .Values.namespace.create is false, the
// chart default) resolves deterministically instead of to whatever the
// local `helm template` default release namespace happens to be.
//
// Same helm-availability caveat as the triple-set chart tests: skips
// locally without helm, always runs in CI (see .github/workflows/ci.yml).
func TestHelmChartRBACBindingsWireToManagerSA(t *testing.T) {
	if !helmAvailable() {
		t.Skip("helm not found on PATH; skipping chart binding-graph check (CI installs helm for this job; see .github/workflows/ci.yml)")
	}
	root := repoRoot(t)

	t.Run("cluster-wide", func(t *testing.T) {
		rendered := renderHelmTemplate(t, root, "--namespace", "capb7-system")
		crs, roles := collectRoles(t, rendered)
		rbs, crbs := collectBindings(t, rendered)
		sa := findServiceAccount(t, rendered, "Helm chart render (watchNamespaces empty)")
		assertBindingsWireRolesToSA(t, "Helm chart cluster-wide branch (watchNamespaces empty)", crs, roles, rbs, crbs, sa)
	})

	t.Run("namespaced", func(t *testing.T) {
		rendered := renderHelmTemplate(t, root, "--namespace", "capb7-system", "--set", "watchNamespaces={rbac-driftguard-test-ns}")
		crs, roles := collectRoles(t, rendered)
		rbs, crbs := collectBindings(t, rendered)
		sa := findServiceAccount(t, rendered, "Helm chart render (watchNamespaces=[rbac-driftguard-test-ns])")
		assertBindingsWireRolesToSA(t, "Helm chart namespaced branch (watchNamespaces=[rbac-driftguard-test-ns])", crs, roles, rbs, crbs, sa)
	})
}
