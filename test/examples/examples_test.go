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

// Package examples checks that every manifest under examples/ is accepted by
// an API server serving the CRDs it is written against. Nothing else read the
// examples, so drift (CAPI v1beta1 objects after the v1beta2 move, renamed
// fields) went unnoticed until someone applied one.
package examples

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

var repoRoot = filepath.Join("..", "..")

// placeholders fills the <...> markers the examples ask readers to replace
// with values the API server accepts; anything not listed becomes "example".
var placeholders = map[string]string{
	"<bmc-port>":                          "8443",
	"<sha256-of-the-image>":               strings.Repeat("0", 64),
	"<64 hex chars>":                      strings.Repeat("0", 64),
	"<base64 of openssl rand -base64 32>": "ZXhhbXBsZQ==",
	"<bmc-address>":                       "192.0.2.10",
	"<bmc-of-cp-01>":                      "192.0.2.11",
	"<bmc-of-worker-01>":                  "192.0.2.12",
	"<node-ip>":                           "192.0.2.20",
}

var placeholderRE = regexp.MustCompile(`<[^<>\n]+>`)

func fill(doc []byte) []byte {
	return placeholderRE.ReplaceAllFunc(doc, func(m []byte) []byte {
		if v, ok := placeholders[string(m)]; ok {
			return []byte(v)
		}
		return []byte("example")
	})
}

func exampleFiles(t *testing.T) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(filepath.Join(repoRoot, "examples"), func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".yaml") {
			files = append(files, path)
		}
		return err
	})
	if err != nil || len(files) == 0 {
		t.Fatalf("no examples found: %v", err)
	}
	return files
}

func documents(t *testing.T, path string) []*unstructured.Unstructured {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(fill(raw))))
	var objs []*unstructured.Unstructured
	for {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return objs
		}
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		obj := &unstructured.Unstructured{}
		if err := utilyaml.Unmarshal(doc, &obj.Object); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if len(obj.Object) > 0 {
			objs = append(objs, obj)
		}
	}
}

func TestExamplesAreAcceptedByTheAPIServer(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set")
	}
	env := &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join(repoRoot, "config", "crd", "bases"),
			filepath.Join(repoRoot, "config", "test-external-crds"),
		},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	c, err := client.New(cfg, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	for _, path := range exampleFiles(t) {
		name, _ := filepath.Rel(repoRoot, path)
		for _, obj := range documents(t, path) {
			gvk := obj.GroupVersionKind()
			what := name + ": " + gvk.Kind + " " + obj.GetName()
			if gvk.Kind == "" {
				// Not a Kubernetes object: the Kairos stage files are cloud-config.
				continue
			}

			// The CAPI groups moved to v1beta2; v1beta1 stops being served in
			// April 2027 and is never what an example should teach.
			if strings.HasSuffix(gvk.Group, "cluster.x-k8s.io") && gvk.Version == "v1beta1" {
				t.Errorf("%s: uses %s; Cluster API objects must be v1beta2", what, gvk.GroupVersion())
				continue
			}
			if _, err := c.RESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version); err != nil {
				if apimeta.IsNoMatchError(err) {
					t.Logf("%s: %s is not served by the test API server; not validated", what, gvk.GroupVersion())
					continue
				}
				t.Fatalf("%s: %v", what, err)
			}
			if ns := obj.GetNamespace(); ns != "" {
				nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
				if err := c.Create(ctx, nsObj); client.IgnoreAlreadyExists(err) != nil {
					t.Fatalf("%s: creating namespace %q: %v", what, ns, err)
				}
			}
			err := c.Create(ctx, obj, client.DryRunAll)
			if gvk.Kind == "Namespace" {
				// Created above for an earlier document that lives in it.
				err = client.IgnoreAlreadyExists(err)
			}
			if err != nil {
				t.Errorf("%s: rejected: %v", what, err)
			}
		}
	}
}
