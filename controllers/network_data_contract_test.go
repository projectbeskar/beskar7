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

package controllers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
)

// goldenNetworkDataArtifactPath is the canonical descriptor of the
// /networkdata endpoint and the /oem/beskar7/network-data COS_OEM artifact
// (contract v4.3), shared with beskar7-inspector.
const goldenNetworkDataArtifactPath = "../test/contract/golden_network_data_artifact.json"

const (
	goldenNetworkDataMachine = "b7m-01"
	goldenNetworkDataSecret  = "host-01-network"
	goldenNetworkDataToken   = "test-token-0123456789abcdef"
)

// goldenNetworkDataArtifact mirrors golden_network_data_artifact.json.
type goldenNetworkDataArtifact struct {
	Namespace      string `json:"namespace"`
	Host           string `json:"host"`
	Endpoint       string `json:"endpoint"`
	SecretKey      string `json:"secretKey"`
	HostAnnotation string `json:"hostAnnotation"`
	MaxSizeBytes   int    `json:"maxSizeBytes"`
	Content        string `json:"content"`
	ArtifactPath   string `json:"artifactPath"`
	Mode           string `json:"mode"`
	Owner          string `json:"owner"`
	StatusServed   int    `json:"statusServed"`
	StatusNoData   int    `json:"statusNoData"`
	StatusRefused  int    `json:"statusRefused"`
}

func readGoldenNetworkDataArtifact(t *testing.T) goldenNetworkDataArtifact {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(goldenNetworkDataArtifactPath))
	if err != nil {
		t.Fatalf("read %s: %v", goldenNetworkDataArtifactPath, err)
	}
	var fixture goldenNetworkDataArtifact
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("decode %s: %v", goldenNetworkDataArtifactPath, err)
	}
	return fixture
}

// goldenNetworkDataCase is one host and network data Secret state the fixture
// gives a status for.
type goldenNetworkDataCase struct {
	setRef         bool
	annotationHost string
}

// serveGoldenNetworkData drives the real NetworkDataHandler for the fixture's
// claimed host, as boundCallbackHost resolves it, and returns the response.
func serveGoldenNetworkData(t *testing.T, fixture goldenNetworkDataArtifact, tc goldenNetworkDataCase) *httptest.ResponseRecorder {
	t.Helper()
	scheme := newSchemeForTest(t)
	host := &infrav1.PhysicalHost{
		ObjectMeta: metav1.ObjectMeta{Namespace: fixture.Namespace, Name: fixture.Host, UID: types.UID("golden-host-uid")},
		Spec: infrav1.PhysicalHostSpec{
			ConsumerRef: &corev1.ObjectReference{
				Kind: "Beskar7Machine", APIVersion: InfrastructureAPIVersion,
				Name: goldenNetworkDataMachine, Namespace: fixture.Namespace,
			},
		},
	}
	if tc.setRef {
		host.Spec.NetworkDataSecretRef = goldenNetworkDataSecret
	}
	credentials := unownedCredentialSecret(fixture.Namespace, fixture.Host,
		boundCredentialData(goldenNetworkDataMachine, goldenNetworkDataToken, time.Hour, "", 0))
	if err := controllerutil.SetControllerReference(host, credentials, scheme); err != nil {
		t.Fatalf("own credentials Secret: %v", err)
	}
	networkData := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   fixture.Namespace,
			Name:        goldenNetworkDataSecret,
			Annotations: map[string]string{fixture.HostAnnotation: tc.annotationHost},
		},
		Data: map[string][]byte{fixture.SecretKey: []byte(fixture.Content)},
	}
	objects := []client.Object{host, credentials, networkData}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()

	req := httptest.NewRequest(http.MethodGet, fixture.Endpoint, nil)
	req.Header.Set("Authorization", "Bearer "+goldenNetworkDataToken)
	req.SetPathValue("namespace", fixture.Namespace)
	req.SetPathValue("hostName", fixture.Host)
	w := httptest.NewRecorder()
	(&NetworkDataHandler{Client: c, Log: logr.Discard()}).ServeHTTP(w, req)
	return w
}

// TestGoldenNetworkDataArtifact_MatchesHandler proves the fixture the inspector
// vendors is what the controller really does: the same Secret key, annotation
// and cap, the content served byte for byte, and the status for each case. It
// cannot prove the inspector writes the file; that is the inspector's own test.
func TestGoldenNetworkDataArtifact_MatchesHandler(t *testing.T) {
	fixture := readGoldenNetworkDataArtifact(t)

	if fixture.SecretKey != networkDataSecretKey {
		t.Errorf("secretKey = %q, want %q", fixture.SecretKey, networkDataSecretKey)
	}
	if fixture.HostAnnotation != NetworkDataHostAnnotation {
		t.Errorf("hostAnnotation = %q, want %q", fixture.HostAnnotation, NetworkDataHostAnnotation)
	}
	if fixture.MaxSizeBytes != maxNetworkDataSize {
		t.Errorf("maxSizeBytes = %d, want %d", fixture.MaxSizeBytes, maxNetworkDataSize)
	}
	if fixture.ArtifactPath != "/oem/beskar7/network-data" || fixture.Mode != "0600" || fixture.Owner != "root" {
		t.Errorf("artifact = %s %s %s, want /oem/beskar7/network-data 0600 root (contract v4.3 §9.1)", fixture.ArtifactPath, fixture.Mode, fixture.Owner)
	}

	served := serveGoldenNetworkData(t, fixture, goldenNetworkDataCase{setRef: true, annotationHost: fixture.Host})
	if served.Code != fixture.StatusServed {
		t.Fatalf("served: status %d, want %d", served.Code, fixture.StatusServed)
	}
	if got := served.Body.String(); got != fixture.Content {
		t.Errorf("served body differs from the fixture content:\n got %q\nwant %q", got, fixture.Content)
	}

	if got := serveGoldenNetworkData(t, fixture, goldenNetworkDataCase{}).Code; got != fixture.StatusNoData {
		t.Errorf("no ref: status %d, want %d", got, fixture.StatusNoData)
	}
	if got := serveGoldenNetworkData(t, fixture, goldenNetworkDataCase{setRef: true, annotationHost: "another-host"}).Code; got != fixture.StatusRefused {
		t.Errorf("refused Secret: status %d, want %d", got, fixture.StatusRefused)
	}
}
