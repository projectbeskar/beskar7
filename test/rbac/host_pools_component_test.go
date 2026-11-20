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

package rbac

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"
)

// enableHostPoolsArg is the manager flag the config/host-pools component adds.
const enableHostPoolsArg = "--enable-host-pools=true"

// managerArgs returns the args of the manager container in built.
func managerArgs(t *testing.T, built []byte) []string {
	t.Helper()
	for _, doc := range splitYAMLDocs(built) {
		var d appsv1.Deployment
		if err := yaml.Unmarshal(doc, &d); err != nil || d.Kind != "Deployment" || d.Name != managerDeploymentName {
			continue
		}
		for _, c := range d.Spec.Template.Spec.Containers {
			if c.Name == "manager" {
				return c.Args
			}
		}
	}
	t.Fatalf("no Deployment %s with a manager container", managerDeploymentName)
	return nil
}

// TestHostPoolsComponent pins that pools are off in config/default and that the
// config/host-pools component turns them on: the pool ClusterRole with the rules
// of host_pool_role.yaml, bound to the manager, and --enable-host-pools=true.
func TestHostPoolsComponent(t *testing.T) {
	requireKustomize(t)
	root := repoRoot(t)

	plain := kustomizeBuild(t, filepath.Join(root, "config", "default"))
	crs, _ := collectRoles(t, plain)
	for _, cr := range crs {
		if cr.Name == hostPoolClusterScoped[0] {
			t.Errorf("config/default ships ClusterRole %s; pools must be opt-in", cr.Name)
		}
	}
	if slices.Contains(managerArgs(t, plain), enableHostPoolsArg) {
		t.Errorf("config/default starts the manager with %s; pools must be opt-in", enableHostPoolsArg)
	}

	dir := t.TempDir()
	rel := func(target string) string {
		r, err := filepath.Rel(dir, target)
		if err != nil {
			t.Fatalf("relative path to %s: %v", target, err)
		}
		return r
	}
	overlay := "resources:\n- " + rel(filepath.Join(root, "config", "default")) + "\n" +
		"components:\n- " + rel(filepath.Join(root, "config", "host-pools")) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(overlay), 0o600); err != nil {
		t.Fatalf("write overlay: %v", err)
	}
	built := kustomizeBuild(t, dir)

	crs, roles := collectRoles(t, built)
	cr := findClusterRoleBySuffix(t, crs, "-host-pool-role")
	assertTriplesEqual(t, "config/host-pools ClusterRole "+cr.Name, loadHostPoolRole(t, root), triplesFromRules(cr.Rules))
	rbs, crbs := collectBindings(t, built)
	sa := findServiceAccount(t, mustReadFile(t, filepath.Join(root, "config", "rbac", "service_account.yaml")),
		"config/rbac/service_account.yaml")
	assertBindingsWireRolesToSA(t, "config/default + config/host-pools", crs, roles, rbs, crbs, sa)
	if !slices.Contains(managerArgs(t, built), enableHostPoolsArg) {
		t.Errorf("config/host-pools does not start the manager with %s", enableHostPoolsArg)
	}
}
