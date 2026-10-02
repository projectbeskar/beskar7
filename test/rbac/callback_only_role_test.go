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
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// The callback-only RBAC component (config/rbac/callback-only/) is the identity
// a --controllers=none manager runs as: the copy placed on the provisioning
// network when the management cluster has no interface there (SEC-14). It is
// not generated from markers — no reconciler runs in that mode, so there is
// nothing to put them on — which is why this file pins it by hand to what the
// callback handlers and the manager's cache actually do. The envtest in
// cmd/manager (TestCallbackOnlyRoleServesEveryRoute) proves the same rules are
// enough to serve every route; this file proves they are no more.

const (
	callbackOnlyDir          = "config/rbac/callback-only"
	callbackOnlyWatchedNSDir = "config/rbac/callback-only/watched-namespace"
)

// callbackOnlyRoleWant is every grant the callback-only Role must carry, each
// with the code that needs it. In callback-only mode the handlers read through
// the manager's cache, whose informers list and watch; get is listed with them
// because list already returns every object, so it exposes nothing more.
var callbackOnlyRoleWant = map[ruleTriple]string{
	// SetupCallbackServer pre-warms one informer per kind the handlers read.
	{"infrastructure.cluster.x-k8s.io", "physicalhosts", "get"}:     "every route reads the host (cache)",
	{"infrastructure.cluster.x-k8s.io", "physicalhosts", "list"}:    "PhysicalHost informer",
	{"infrastructure.cluster.x-k8s.io", "physicalhosts", "watch"}:   "PhysicalHost informer",
	{"infrastructure.cluster.x-k8s.io", "beskar7machines", "get"}:   "/boot renders and /bootstrap resolves the host's consumer (cache)",
	{"infrastructure.cluster.x-k8s.io", "beskar7machines", "list"}:  "Beskar7Machine informer",
	{"infrastructure.cluster.x-k8s.io", "beskar7machines", "watch"}: "Beskar7Machine informer",
	{"cluster.x-k8s.io", "machines", "get"}:                         "/bootstrap walks to the owner Machine (cache)",
	{"cluster.x-k8s.io", "machines", "list"}:                        "Machine informer",
	{"cluster.x-k8s.io", "machines", "watch"}:                       "Machine informer",
	{"", "secrets", "get"}:                                          "bearer verifier and /boot read <host>-bootstrap-token; /bootstrap reads Machine.spec.bootstrap.dataSecretName (cache)",
	{"", "secrets", "list"}:                                         "Secret informer",
	{"", "secrets", "watch"}:                                        "Secret informer",
	{"", "configmaps", "get"}:                                       "/inspection CreateOrUpdate reads <host>-inspection-result (cache)",
	{"", "configmaps", "list"}:                                      "ConfigMap informer",
	{"", "configmaps", "watch"}:                                     "ConfigMap informer",
	{"", "configmaps", "create"}:                                    "/inspection writes the first report",
	{"", "configmaps", "update"}:                                    "/inspection rewrites a report the controller has not consumed yet",
	// Writes to the host.
	{"infrastructure.cluster.x-k8s.io", "physicalhosts", "patch"}:        "/inspection, /provisioned and /provision-failed set their request annotations",
	{"infrastructure.cluster.x-k8s.io", "physicalhosts/status", "patch"}: "/boot records the nonce consume (D-010)",
	// The inspection-result ConfigMap's controller reference sets
	// blockOwnerDeletion, which the OwnerReferencesPermissionEnforcement
	// admission plugin (on by default in OpenShift, among others) allows only
	// to a caller that may update the owner's finalizers.
	{"infrastructure.cluster.x-k8s.io", "physicalhosts/finalizers", "update"}: "/inspection's owner reference on the ConfigMap (blockOwnerDeletion)",
}

// loadCallbackOnlyComponent reads every manifest the component's two
// kustomizations list.
func loadCallbackOnlyComponent(t *testing.T, root string) (identity, watched []byte) {
	t.Helper()
	return readKustomizationResources(t, filepath.Join(root, callbackOnlyDir)),
		readKustomizationResources(t, filepath.Join(root, callbackOnlyWatchedNSDir))
}

// readKustomizationResources concatenates the files a kustomization.yaml lists
// under resources, failing on anything but a plain file next to it.
func readKustomizationResources(t *testing.T, dir string) []byte {
	t.Helper()
	var k struct {
		Namespace string   `json:"namespace"`
		Resources []string `json:"resources"`
	}
	if err := yaml.Unmarshal(mustReadFile(t, filepath.Join(dir, "kustomization.yaml")), &k); err != nil {
		t.Fatalf("parse %s/kustomization.yaml: %v", dir, err)
	}
	// A namespace transformer here would rewrite the namespace of everything
	// listed: the ServiceAccount must stay where the operator runs, and the
	// Role and RoleBinding must take the namespace given at apply time.
	if k.Namespace != "" {
		t.Errorf("%s/kustomization.yaml sets namespace %q; the component must not set one", dir, k.Namespace)
	}
	var out bytes.Buffer
	for _, r := range k.Resources {
		info, err := os.Stat(filepath.Join(dir, r))
		if err != nil {
			t.Fatalf("%s/kustomization.yaml lists %q: %v", dir, r, err)
		}
		if info.IsDir() {
			t.Fatalf("%s/kustomization.yaml lists directory %q; list the watched-namespace part separately", dir, r)
		}
		out.Write(mustReadFile(t, filepath.Join(dir, r)))
		out.WriteString("\n---\n")
	}
	return out.Bytes()
}

// TestCallbackOnlyRoleIsExactlyWhatTheCallbacksUse pins the Role's grants to
// callbackOnlyRoleWant in both directions: a missing triple breaks a route, an
// extra one hands a host-facing process more than it uses.
func TestCallbackOnlyRoleIsExactlyWhatTheCallbacksUse(t *testing.T) {
	root := repoRoot(t)
	_, watched := loadCallbackOnlyComponent(t, root)
	crs, roles := collectRoles(t, watched)
	if len(crs) != 0 || len(roles) != 1 {
		t.Fatalf("%s: want exactly 1 Role and no ClusterRole, found %d Role(s) and %d ClusterRole(s)", callbackOnlyWatchedNSDir, len(roles), len(crs))
	}

	want := make(map[ruleTriple]struct{}, len(callbackOnlyRoleWant))
	for tr := range callbackOnlyRoleWant {
		want[tr] = struct{}{}
	}
	missing, extra := diffTriples(want, triplesFromRules(roles[0].Rules))
	for _, tr := range missing {
		t.Errorf("Role %s lacks %s, needed for: %s", roles[0].Name, tr, callbackOnlyRoleWant[tr])
	}
	for _, tr := range extra {
		t.Errorf("Role %s grants %s, which no callback handler or callback-mode informer uses", roles[0].Name, tr)
	}
	for _, r := range roles[0].Rules {
		if len(r.ResourceNames) > 0 || len(r.NonResourceURLs) > 0 {
			t.Errorf("Role %s: rule %+v uses resourceNames or nonResourceURLs, which this pin does not model", roles[0].Name, r)
		}
	}
}

// TestCallbackOnlyRoleIsWithinManagerRole: the callback-only identity serves a
// subset of what the full manager does, so it can never need a grant the
// manager lacks.
func TestCallbackOnlyRoleIsWithinManagerRole(t *testing.T) {
	root := repoRoot(t)
	manager := loadGeneratedManagerRole(t, root)
	for tr := range callbackOnlyRoleWant {
		if _, ok := manager[tr]; !ok {
			t.Errorf("callback-only grant %s is not in config/rbac/role.yaml", tr)
		}
	}
}

// TestCallbackOnlyRBACIsNamespacedAndSelfContained checks the component's
// shape: one ServiceAccount of its own (never the manager's, which holds
// cluster-wide Secret CRUD), nothing cluster-scoped, and a Role and RoleBinding
// that leave their namespace to apply time and bind only that ServiceAccount.
func TestCallbackOnlyRBACIsNamespacedAndSelfContained(t *testing.T) {
	root := repoRoot(t)
	identity, watched := loadCallbackOnlyComponent(t, root)

	sa := findServiceAccount(t, identity, callbackOnlyDir)
	managerSA := findServiceAccount(t, mustReadFile(t, filepath.Join(root, "config", "rbac", "service_account.yaml")), "config/rbac/service_account.yaml")
	if sa.Name == managerSA.Name && sa.Namespace == managerSA.Namespace {
		t.Errorf("%s reuses the manager ServiceAccount %s/%s; the callback-only instance needs its own identity", callbackOnlyDir, sa.Namespace, sa.Name)
	}
	if sa.Namespace == "" {
		t.Errorf("%s: the ServiceAccount must name its namespace, since the component sets none", callbackOnlyDir)
	}
	if crs, roles := collectRoles(t, identity); len(crs) != 0 || len(roles) != 0 {
		t.Errorf("%s grants permissions itself (%d ClusterRole(s), %d Role(s)); grants belong in %s", callbackOnlyDir, len(crs), len(roles), callbackOnlyWatchedNSDir)
	}
	if rbs, crbs := collectBindings(t, identity); len(rbs) != 0 || len(crbs) != 0 {
		t.Errorf("%s binds roles itself; bindings belong in %s", callbackOnlyDir, callbackOnlyWatchedNSDir)
	}

	crs, roles := collectRoles(t, watched)
	rbs, crbs := collectBindings(t, watched)
	if len(crs) != 0 || len(crbs) != 0 {
		t.Errorf("%s ships %d ClusterRole(s) and %d ClusterRoleBinding(s); it must be namespaced", callbackOnlyWatchedNSDir, len(crs), len(crbs))
	}
	for _, r := range roles {
		if r.Namespace != "" {
			t.Errorf("Role %s pins namespace %q; it must take the watched namespace at apply time", r.Name, r.Namespace)
		}
	}
	for _, rb := range rbs {
		if rb.Namespace != "" {
			t.Errorf("RoleBinding %s pins namespace %q; it must take the watched namespace at apply time", rb.Name, rb.Namespace)
		}
		if len(rb.Subjects) != 1 {
			t.Errorf("RoleBinding %s has %d subjects (%s); want only the callback-only ServiceAccount", rb.Name, len(rb.Subjects), subjectsString(rb.Subjects))
		}
	}
	// Give the namespace-less objects the namespace they get at apply time, so
	// the binding graph check can match each RoleBinding to its Role.
	for i := range roles {
		roles[i].Namespace = "watched"
	}
	for i := range rbs {
		rbs[i].Namespace = "watched"
	}
	assertBindingsWireRolesToSA(t, callbackOnlyWatchedNSDir, crs, roles, rbs, crbs, sa)
}

// TestCallbackOnlyRBACIsNotInTheDefaultInstall walks the kustomization graph
// from config/default and fails if it reaches the callback-only component: the
// component is opt-in, and its identity is for a process outside the cluster.
func TestCallbackOnlyRBACIsNotInTheDefaultInstall(t *testing.T) {
	root := repoRoot(t)
	component := filepath.Join(root, callbackOnlyDir)
	seen := map[string]bool{}
	var walk func(dir string)
	walk = func(dir string) {
		if seen[dir] {
			return
		}
		seen[dir] = true
		var k struct {
			Resources  []string `json:"resources"`
			Components []string `json:"components"`
		}
		if err := yaml.Unmarshal(mustReadFile(t, filepath.Join(dir, "kustomization.yaml")), &k); err != nil {
			t.Fatalf("parse %s/kustomization.yaml: %v", dir, err)
		}
		for _, r := range append(k.Resources, k.Components...) {
			p := filepath.Clean(filepath.Join(dir, r))
			if p == component || strings.HasPrefix(p, component+string(filepath.Separator)) {
				t.Errorf("%s/kustomization.yaml pulls in %s; the callback-only RBAC must stay out of the default install", dir, r)
				continue
			}
			if info, err := os.Stat(p); err == nil && info.IsDir() {
				walk(p)
			}
		}
	}
	walk(filepath.Join(root, "config", "default"))
}

// TestCallbackOnlyRBACKustomizeBuild builds the component the way the docs
// apply it: the identity as-is, and the watched-namespace part from an overlay
// that sets the namespace. It checks that the overlay moves the Role and
// RoleBinding into the watched namespace while the binding's subject keeps
// pointing at the ServiceAccount where it lives. Requires kustomize on PATH.
func TestCallbackOnlyRBACKustomizeBuild(t *testing.T) {
	if _, err := exec.LookPath("kustomize"); err != nil {
		t.Skip("kustomize not found on PATH; skipping the callback-only component build check")
	}
	root := repoRoot(t)
	sa := findServiceAccount(t, kustomizeBuild(t, filepath.Join(root, callbackOnlyDir)), callbackOnlyDir+" (kustomize build)")

	overlay := t.TempDir()
	rel, err := filepath.Rel(overlay, filepath.Join(root, callbackOnlyWatchedNSDir))
	if err != nil {
		t.Fatalf("relative path to the component: %v", err)
	}
	kustomization := fmt.Sprintf("namespace: tenant-a\nresources:\n- %s\n", rel)
	if err := os.WriteFile(filepath.Join(overlay, "kustomization.yaml"), []byte(kustomization), 0o600); err != nil {
		t.Fatalf("write overlay: %v", err)
	}
	built := kustomizeBuild(t, overlay)
	crs, roles := collectRoles(t, built)
	rbs, crbs := collectBindings(t, built)
	if len(roles) != 1 || len(rbs) != 1 || len(crs) != 0 || len(crbs) != 0 {
		t.Fatalf("overlay build: want 1 Role and 1 RoleBinding, got %d Role(s), %d RoleBinding(s), %d ClusterRole(s), %d ClusterRoleBinding(s)",
			len(roles), len(rbs), len(crs), len(crbs))
	}
	if roles[0].Namespace != "tenant-a" || rbs[0].Namespace != "tenant-a" {
		t.Errorf("overlay build: Role in %q and RoleBinding in %q, want both in tenant-a", roles[0].Namespace, rbs[0].Namespace)
	}
	assertBindingsWireRolesToSA(t, "callback-only overlay build", crs, roles, rbs, crbs, sa)
}

func kustomizeBuild(t *testing.T, dir string) []byte {
	t.Helper()
	cmd := exec.Command("kustomize", "build", dir) // #nosec G204 -- fixed binary, repo or temp-dir path
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("kustomize build %s: %v\n%s", dir, err, stderr.String())
	}
	return stdout.Bytes()
}
