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
	"bytes"
	"errors"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

const managerDeploymentName = "capb7-controller-manager"

// TestSizeOverlaysOnlyResize pins what a size overlay under config/overlays/
// may do to config/default: add objects and change replicas, resources,
// scheduling and manager flags, and nothing else. `kustomize build` succeeding
// proves none of it. Through v0.8.0 every overlay rendered cleanly and yet:
//
//   - replaced the manager's args wholesale, passing two flags the manager
//     does not define, so it exited at startup, and dropping
//     --bootstrap-url-base and --enable-webhook;
//   - renamed every object with a namePrefix, while the cert-manager
//     Certificate names the webhook and callback Services literally, so the
//     serving cert no longer matched either Service;
//   - pinned an image tag that was never published.
func TestSizeOverlaysOnlyResize(t *testing.T) {
	kustomize, err := exec.LookPath("kustomize")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("kustomize not found on PATH; CI must run this check")
		}
		t.Skip("kustomize not found on PATH; skipping the size-overlay check (CI runs it)")
	}

	root := filepath.Join("..", "..")
	overlays, err := filepath.Glob(filepath.Join(root, "config", "overlays", "*", "kustomization.yaml"))
	if err != nil || len(overlays) == 0 {
		t.Fatalf("no size overlays found under config/overlays (err=%v)", err)
	}

	base := renderKustomization(t, kustomize, filepath.Join(root, "config", "default"))
	baseManager := managerContainer(t, base)
	manager := buildManager(t, root)

	for _, kustomization := range overlays {
		dir := filepath.Dir(kustomization)
		t.Run(filepath.Base(dir), func(t *testing.T) {
			rendered := renderKustomization(t, kustomize, dir)

			for _, key := range slices.Sorted(maps.Keys(base)) {
				if _, ok := rendered[key]; !ok {
					t.Errorf("%s is missing: an overlay must not rename or drop what config/default ships", key)
				}
			}

			c := managerContainer(t, rendered)
			if c.Image != baseManager.Image {
				t.Errorf("manager image %q, config/default ships %q: the release stamps config/default, so an overlay must not pin its own", c.Image, baseManager.Image)
			}
			for _, arg := range baseManager.Args {
				if !slices.Contains(c.Args, arg) {
					t.Errorf("manager arg %q from config/default is missing: patch args by appending, never by replacing the list", arg)
				}
			}

			// flag.Parse stops at the first flag it does not know and exits 2;
			// only when every earlier argument parses does it reach --help and
			// exit 0.
			cmd := exec.Command(manager, append(slices.Clone(c.Args), "--help")...) // #nosec G204 -- args come from the repo's own manifests
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("the manager rejects the overlay's args %q: %v\n%s", c.Args, err, firstLines(out, 3))
			}
		})
	}
}

// renderKustomization builds dir and indexes the objects by kind/namespace/name.
func renderKustomization(t *testing.T, kustomize, dir string) map[string]*unstructured.Unstructured {
	t.Helper()
	cmd := exec.Command(kustomize, "build", dir) // #nosec G204 -- fixed binary, repo-relative path
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("kustomize build %s: %v\n%s", dir, err, stderr.String())
	}

	objects := map[string]*unstructured.Unstructured{}
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(out), 4096)
	for {
		obj := &unstructured.Unstructured{}
		if err := decoder.Decode(&obj.Object); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("decoding kustomize build %s: %v", dir, err)
		}
		if len(obj.Object) == 0 {
			continue
		}
		objects[obj.GetKind()+"/"+obj.GetNamespace()+"/"+obj.GetName()] = obj
	}
	return objects
}

func managerContainer(t *testing.T, objects map[string]*unstructured.Unstructured) corev1.Container {
	t.Helper()
	obj, ok := objects["Deployment/capb7-system/"+managerDeploymentName]
	if !ok {
		t.Fatalf("Deployment capb7-system/%s not rendered", managerDeploymentName)
	}
	var deployment appsv1.Deployment
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &deployment); err != nil {
		t.Fatalf("converting the manager Deployment: %v", err)
	}
	for _, c := range deployment.Spec.Template.Spec.Containers {
		if c.Name == "manager" {
			return c
		}
	}
	t.Fatalf("Deployment %s has no manager container", managerDeploymentName)
	return corev1.Container{}
}

func buildManager(t *testing.T, root string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "manager")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/manager") // #nosec G204 -- fixed arguments
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the manager: %v\n%s", err, out)
	}
	return bin
}

// firstLines keeps the flag package's error, which it prints ahead of the
// full usage text.
func firstLines(out []byte, n int) []byte {
	lines := bytes.Split(bytes.TrimRight(out, "\n"), []byte("\n"))
	if len(lines) > n {
		lines = lines[:n]
	}
	return bytes.Join(lines, []byte("\n"))
}
