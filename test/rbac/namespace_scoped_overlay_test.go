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

package rbac

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

// The namespace-scoped overlay (config/rbac/namespace-scoped/) is the kustomize
// twin of the chart's watchNamespaces branch: a Role and RoleBinding in each
// watched namespace, and the leader-election pair where the manager runs. The
// rule-set and binding-graph guards in rbac_driftguard_test.go read its files
// one by one, so they cannot see what kustomize does with them. That is where
// RBAC-OVERLAY lived: the kustomization set `namespace: capb7-system`, whose
// transformer rewrites the namespace of every namespaced object, so the watch
// Roles an operator added per the README all landed in capb7-system. The manager
// then held nothing in the namespaces it watched and failed closed, and with two
// watched namespaces the build stopped on a duplicate Role. This file builds the
// overlay the way the README says to and checks where everything ends up.

const (
	namespaceScopedDir        = "config/rbac/namespace-scoped"
	watchNamespacePlaceholder = "REPLACE_WITH_WATCHED_NAMESPACE"

	leaderElectionRoleName = "manager-leaderelection-role"
	watchRoleName          = "manager-watch-role"
	managerDeploymentName  = "capb7-controller-manager"
)

// metricsClusterScoped is what the manager legitimately keeps at cluster scope
// in the namespace-scoped topology: the metrics authentication role and its
// binding, plus the reader role an operator binds to their scraper. None grants
// anything on Beskar7 objects, Secrets or ConfigMaps.
var metricsClusterScoped = []string{
	"capb7-metrics-auth-role",
	"capb7-metrics-auth-rolebinding",
	"capb7-metrics-reader",
}

func requireKustomize(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("kustomize"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("kustomize not found on PATH; CI must run this check")
		}
		t.Skip("kustomize not found on PATH; skipping the namespace-scoped overlay build check (CI runs it)")
	}
}

// TestNamespaceScopedOverlayNamesItsOwnNamespaces is the part of the guard that
// needs no kustomize binary: the overlay sets no namespace transformer, and every
// object in it names the namespace it belongs in. Without the transformer a
// missing namespace would not be filled in, it would be applied to whatever
// namespace the operator's kubectl context points at.
func TestNamespaceScopedOverlayNamesItsOwnNamespaces(t *testing.T) {
	root := repoRoot(t)
	dir := filepath.Join(root, namespaceScopedDir)
	sa := findServiceAccount(t, mustReadFile(t, filepath.Join(root, "config", "rbac", "service_account.yaml")), "config/rbac/service_account.yaml")

	var k struct {
		Namespace string `json:"namespace"`
	}
	if err := yaml.Unmarshal(mustReadFile(t, filepath.Join(dir, "kustomization.yaml")), &k); err != nil {
		t.Fatalf("parse %s/kustomization.yaml: %v", namespaceScopedDir, err)
	}
	if k.Namespace != "" {
		t.Errorf("%s/kustomization.yaml sets namespace %q: the transformer rewrites every Role and RoleBinding it covers, so each watch Role lands in %q instead of the namespace it was written for (RBAC-OVERLAY)",
			namespaceScopedDir, k.Namespace, k.Namespace)
	}

	// The leader-election pair lives where the manager runs, which is where its
	// ServiceAccount is.
	leader := mustReadFile(t, filepath.Join(dir, "leader-election-role.yaml"))
	_, leaderRoles := collectRoles(t, leader)
	leaderBindings, _ := collectBindings(t, leader)
	for _, r := range leaderRoles {
		if r.Namespace != sa.Namespace {
			t.Errorf("leader-election-role.yaml: Role %s is in namespace %q, want %q (the manager ServiceAccount's)", r.Name, r.Namespace, sa.Namespace)
		}
	}
	for _, rb := range leaderBindings {
		if rb.Namespace != sa.Namespace {
			t.Errorf("leader-election-role.yaml: RoleBinding %s is in namespace %q, want %q (the manager ServiceAccount's)", rb.Name, rb.Namespace, sa.Namespace)
		}
	}

	// The template carries the placeholder in exactly the Role's and the
	// RoleBinding's namespace; instantiating it is a plain substitution.
	template := mustReadFile(t, filepath.Join(dir, "watch-role.template.yaml"))
	if n := bytes.Count(template, []byte(watchNamespacePlaceholder)); n != 2 {
		t.Errorf("watch-role.template.yaml has %d occurrences of %s, want 2 (the Role's and the RoleBinding's namespace)", n, watchNamespacePlaceholder)
	}
	_, templateRoles := collectRoles(t, template)
	templateBindings, _ := collectBindings(t, template)
	if len(templateRoles) != 1 || len(templateBindings) != 1 {
		t.Fatalf("watch-role.template.yaml: want 1 Role and 1 RoleBinding, found %d and %d", len(templateRoles), len(templateBindings))
	}
	if templateRoles[0].Namespace != watchNamespacePlaceholder || templateBindings[0].Namespace != watchNamespacePlaceholder {
		t.Errorf("watch-role.template.yaml: Role in %q and RoleBinding in %q, want both in %s", templateRoles[0].Namespace, templateBindings[0].Namespace, watchNamespacePlaceholder)
	}
}

// instantiateNamespaceScoped copies the overlay into a temp dir and performs the
// README's per-namespace steps: copy the template to watch-role.<ns>.yaml, put
// the namespace in its two namespace fields, and append the file to the
// kustomization's resources. It returns the copy.
func instantiateNamespaceScoped(t *testing.T, root string, namespaces ...string) string {
	t.Helper()
	src := filepath.Join(root, namespaceScopedDir)
	dst := filepath.Join(t.TempDir(), "namespace-scoped")
	if err := os.Mkdir(dst, 0o700); err != nil {
		t.Fatalf("create %s: %v", dst, err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			t.Fatalf("%s holds the directory %s: the build check copies the overlay flat", namespaceScopedDir, e.Name())
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), mustReadFile(t, filepath.Join(src, e.Name())), 0o600); err != nil {
			t.Fatalf("copy %s: %v", e.Name(), err)
		}
	}

	template := mustReadFile(t, filepath.Join(src, "watch-role.template.yaml"))
	var kustomization map[string]any
	if err := yaml.Unmarshal(mustReadFile(t, filepath.Join(src, "kustomization.yaml")), &kustomization); err != nil {
		t.Fatalf("parse %s/kustomization.yaml: %v", namespaceScopedDir, err)
	}
	resources, _ := kustomization["resources"].([]any)
	for _, ns := range namespaces {
		name := "watch-role." + ns + ".yaml"
		role := bytes.ReplaceAll(template, []byte(watchNamespacePlaceholder), []byte(ns))
		if err := os.WriteFile(filepath.Join(dst, name), role, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		resources = append(resources, name)
	}
	kustomization["resources"] = resources
	out, err := yaml.Marshal(kustomization)
	if err != nil {
		t.Fatalf("marshal kustomization: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dst, "kustomization.yaml"), out, 0o600); err != nil {
		t.Fatalf("write kustomization.yaml: %v", err)
	}
	return dst
}

// assertWatchRBAC checks a kustomize build of the namespace-scoped RBAC: the
// leader-election Role where the manager runs, one watch Role per watched
// namespace and nothing else namespaced, every Role bound to the manager's
// ServiceAccount (which lives in a different namespace) by a RoleBinding beside
// it, and each watched namespace holding, with the leader-election grants,
// exactly what config/rbac/role.yaml grants. clusterScoped lists the names of
// the ClusterRoles and ClusterRoleBindings the build may carry.
func assertWatchRBAC(t *testing.T, label string, built []byte, watched []string, clusterScoped []string) {
	t.Helper()
	root := repoRoot(t)
	sa := findServiceAccount(t, mustReadFile(t, filepath.Join(root, "config", "rbac", "service_account.yaml")), "config/rbac/service_account.yaml")
	crs, roles := collectRoles(t, built)
	rbs, crbs := collectBindings(t, built)

	for _, cr := range crs {
		if !slices.Contains(clusterScoped, cr.Name) {
			t.Errorf("%s: ClusterRole %s is not part of the namespace-scoped topology", label, cr.Name)
		}
	}
	for _, crb := range crbs {
		if !slices.Contains(clusterScoped, crb.Name) {
			t.Errorf("%s: ClusterRoleBinding %s is not part of the namespace-scoped topology", label, crb.Name)
		}
	}

	wantRoles := map[string]bool{sa.Namespace + "/" + leaderElectionRoleName: true}
	for _, ns := range watched {
		wantRoles[ns+"/"+watchRoleName] = true
	}
	gotRoles := map[string]rbacv1.Role{}
	for _, r := range roles {
		key := r.Namespace + "/" + r.Name
		if !wantRoles[key] {
			t.Errorf("%s: unexpected Role %s (want %s in %s and %s in each of %v)", label, key, leaderElectionRoleName, sa.Namespace, watchRoleName, watched)
		}
		gotRoles[key] = r
	}
	for key := range wantRoles {
		if _, ok := gotRoles[key]; !ok {
			t.Errorf("%s: no Role %s: the manager would hold nothing there", label, key)
		}
	}

	// Every Role needs a RoleBinding in its own namespace whose subject is the
	// manager ServiceAccount in its own namespace, not the Role's.
	assertBindingsWireRolesToSA(t, label, crs, roles, rbs, crbs, sa)

	want := loadGeneratedManagerRole(t, root)
	leader, ok := gotRoles[sa.Namespace+"/"+leaderElectionRoleName]
	if !ok {
		return
	}
	for _, ns := range watched {
		watch, ok := gotRoles[ns+"/"+watchRoleName]
		if !ok {
			continue
		}
		assertTriplesEqual(t, fmt.Sprintf("%s: %s in %s + %s in %s", label, leaderElectionRoleName, sa.Namespace, watchRoleName, ns),
			want, unionTriples(triplesFromRules(leader.Rules), triplesFromRules(watch.Rules)))
	}
}

// TestNamespaceScopedOverlayKustomizeBuild builds the overlay the way its README
// describes (copy the template per watched namespace, append the copies to the
// kustomization's resources) and checks where kustomize puts each object. On the
// RBAC-OVERLAY overlay one namespace builds with its watch Role and RoleBinding
// in capb7-system, and two fail on the duplicate Role the transformer creates.
func TestNamespaceScopedOverlayKustomizeBuild(t *testing.T) {
	requireKustomize(t)
	root := repoRoot(t)

	t.Run("as shipped", func(t *testing.T) {
		built := kustomizeBuild(t, filepath.Join(root, namespaceScopedDir))
		assertWatchRBAC(t, "kustomize build "+namespaceScopedDir, built, nil, nil)
	})
	for _, watched := range [][]string{{"tenant-a"}, {"tenant-a", "tenant-b"}} {
		t.Run(strings.Join(watched, "+"), func(t *testing.T) {
			built := kustomizeBuild(t, instantiateNamespaceScoped(t, root, watched...))
			assertWatchRBAC(t, fmt.Sprintf("kustomize build of the overlay with watch Roles for %v", watched), built, watched, nil)
		})
	}
}

// yamlBlockContaining returns the first ```yaml fenced block of a Markdown file
// that contains needle, with the indentation it has inside a list item removed.
func yamlBlockContaining(t *testing.T, path, needle string) string {
	t.Helper()
	for _, m := range regexp.MustCompile("(?s)```yaml\n(.*?)```").FindAllStringSubmatch(string(mustReadFile(t, path)), -1) {
		if !strings.Contains(m[1], needle) {
			continue
		}
		lines := strings.Split(strings.TrimRight(m[1], " \n"), "\n")
		indent := -1
		for _, l := range lines {
			if strings.TrimSpace(l) == "" {
				continue
			}
			if n := len(l) - len(strings.TrimLeft(l, " ")); indent < 0 || n < indent {
				indent = n
			}
		}
		for i, l := range lines {
			if len(l) >= indent {
				lines[i] = l[indent:]
			}
		}
		return strings.Join(lines, "\n") + "\n"
	}
	t.Fatalf("%s has no yaml block containing %q", path, needle)
	return ""
}

var watchNamespacesFlag = regexp.MustCompile(`--watch-namespaces=(\S+)`)

// readmeInstallOverlay returns the install overlay the namespace-scoped README
// shows, and the namespaces its --watch-namespaces flag lists.
func readmeInstallOverlay(t *testing.T, root string) (overlay string, watched []string) {
	t.Helper()
	overlay = yamlBlockContaining(t, filepath.Join(root, namespaceScopedDir, "README.md"), "config/default")
	m := watchNamespacesFlag.FindStringSubmatch(overlay)
	if m == nil {
		t.Fatalf("%s/README.md: the install overlay passes no --watch-namespaces", namespaceScopedDir)
	}
	return overlay, strings.Split(m[1], ",")
}

// TestNamespaceScopedInstallOverlayFromTheReadme builds the install overlay the
// README shows, byte for byte except for the two paths it points at the repo and
// at a copy of the namespace-scoped directory holding a watch Role per namespace
// the overlay's --watch-namespaces lists. It carries what the README claims:
// config/default with its cluster-wide manager role and binding removed, the
// namespace-scoped Roles in their namespaces, and the manager started with the
// flag that makes its cache match.
func TestNamespaceScopedInstallOverlayFromTheReadme(t *testing.T) {
	requireKustomize(t)
	root := repoRoot(t)
	overlay, watched := readmeInstallOverlay(t, root)

	dir := t.TempDir()
	rel := func(target string) string {
		r, err := filepath.Rel(dir, target)
		if err != nil {
			t.Fatalf("relative path to %s: %v", target, err)
		}
		return r
	}
	for old, target := range map[string]string{
		"- ../../config/default\n":               filepath.Join(root, "config", "default"),
		"- ../../config/rbac/namespace-scoped\n": instantiateNamespaceScoped(t, root, watched...),
	} {
		if strings.Count(overlay, old) != 1 {
			t.Fatalf("%s/README.md: the install overlay no longer lists %q exactly once", namespaceScopedDir, strings.TrimSpace(old))
		}
		overlay = strings.Replace(overlay, old, "- "+rel(target)+"\n", 1)
	}
	if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(overlay), 0o600); err != nil {
		t.Fatalf("write overlay: %v", err)
	}

	built := kustomizeBuild(t, dir)
	assertWatchRBAC(t, "kustomize build of the README's install overlay", built, watched, metricsClusterScoped)

	// The cluster-wide manager role and binding the overlay replaces are gone.
	crs, _ := collectRoles(t, built)
	for _, cr := range crs {
		if cr.Name == "capb7-manager-role" {
			t.Errorf("the install overlay still ships the cluster-wide ClusterRole %s", cr.Name)
		}
	}

	var args []string
	for _, doc := range splitYAMLDocs(built) {
		var d appsv1.Deployment
		if err := yaml.Unmarshal(doc, &d); err != nil || d.Kind != "Deployment" || d.Name != managerDeploymentName {
			continue
		}
		for _, c := range d.Spec.Template.Spec.Containers {
			if c.Name == "manager" {
				args = c.Args
			}
		}
	}
	if args == nil {
		t.Fatalf("the install overlay builds no Deployment %s with a manager container: its --watch-namespaces patch targets nothing", managerDeploymentName)
	}
	want := "--watch-namespaces=" + strings.Join(watched, ",")
	if !slices.Contains(args, want) {
		t.Errorf("manager args %v lack %s: the cache would watch every namespace while the RBAC covers %v", args, want, watched)
	}
}

// TestRBACHardeningDocCarriesTheReadmeInstallOverlay keeps the second copy of the
// install overlay, in docs/security/rbac-hardening.md, from drifting from the
// one the build test above runs. The namespace list may differ: the doc's other
// examples use default, tenant-a and tenant-b.
func TestRBACHardeningDocCarriesTheReadmeInstallOverlay(t *testing.T) {
	root := repoRoot(t)
	readme, watched := readmeInstallOverlay(t, root)
	doc := yamlBlockContaining(t, filepath.Join(root, "docs", "security", "rbac-hardening.md"), "config/default")
	m := watchNamespacesFlag.FindStringSubmatch(doc)
	if m == nil {
		t.Fatal("docs/security/rbac-hardening.md: the install overlay passes no --watch-namespaces")
	}
	doc = strings.Replace(doc, m[1], strings.Join(watched, ","), 1)
	if doc != readme {
		t.Errorf("the install overlay in docs/security/rbac-hardening.md differs from the one in %s/README.md; keep the two identical.\ndoc:\n%s\nREADME:\n%s", namespaceScopedDir, doc, readme)
	}
}
