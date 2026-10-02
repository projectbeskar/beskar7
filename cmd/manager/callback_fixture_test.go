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

package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
	"github.com/projectbeskar/beskar7/controllers"
	"github.com/projectbeskar/beskar7/internal/auth"
)

// callbackFixture is one host claimed by a Beskar7Machine whose owner Machine
// names a bootstrap data Secret, with live callback credentials: everything
// every callback route reads on the inspector's happy path.
type callbackFixture struct {
	namespace     string
	host          string
	token         string
	nonce         string
	bootstrapData string
}

// createCallbackFixture creates the fixture in namespace with c. The host is
// left Inspecting, a state in which /provisioned and /provision-failed both
// record their report.
func createCallbackFixture(t *testing.T, c client.Client, namespace string) callbackFixture {
	t.Helper()
	ctx := context.Background()
	fx := callbackFixture{namespace: namespace, host: "host-1", bootstrapData: "#cloud-config\n"}
	const machineName, b7mName = "machine-1", "b7m-1"

	bootstrapSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: machineName + "-bootstrap", Namespace: namespace},
		Data:       map[string][]byte{"value": []byte(fx.bootstrapData)},
	}
	mustCreate(t, c, bootstrapSecret)

	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      machineName,
			Namespace: namespace,
			Labels:    map[string]string{clusterv1.ClusterNameLabel: "cluster-1"},
		},
		Spec: clusterv1.MachineSpec{
			ClusterName:       "cluster-1",
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{APIGroup: infrav1.GroupVersion.Group, Kind: "Beskar7Machine", Name: b7mName},
			Bootstrap:         clusterv1.Bootstrap{DataSecretName: &bootstrapSecret.Name},
		},
	}
	mustCreate(t, c, machine)

	b7m := &infrav1.Beskar7Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      b7mName,
			Namespace: namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: clusterv1.GroupVersion.String(),
				Kind:       "Machine",
				Name:       machine.Name,
				UID:        machine.UID,
			}},
		},
		Spec: infrav1.Beskar7MachineSpec{
			InspectionImageURL: "https://boot.example/inspector",
			TargetImageURL:     "https://boot.example/images/os.raw",
			TargetImageDigest:  "sha256:" + strings.Repeat("ab", 32),
		},
	}
	mustCreate(t, c, b7m)

	host := &infrav1.PhysicalHost{
		ObjectMeta: metav1.ObjectMeta{Name: fx.host, Namespace: namespace},
		Spec: infrav1.PhysicalHostSpec{
			RedfishConnection: infrav1.RedfishConnection{
				Address:              "https://192.0.2.10",
				CredentialsSecretRef: "bmc-creds",
			},
			ConsumerRef: &corev1.ObjectReference{
				Kind: "Beskar7Machine", APIVersion: infrav1.GroupVersion.String(),
				Name: b7mName, Namespace: namespace,
			},
		},
	}
	mustCreate(t, c, host)
	host.Status.State = infrav1.StateInspecting
	if err := c.Status().Update(ctx, host); err != nil {
		t.Fatalf("set PhysicalHost state: %v", err)
	}

	var err error
	if fx.token, _, err = auth.MintToken(); err != nil {
		t.Fatalf("mint token: %v", err)
	}
	if fx.nonce, _, err = auth.MintToken(); err != nil {
		t.Fatalf("mint nonce: %v", err)
	}
	expires := []byte(time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	// The data keys are the manager's (controllers package, D-029).
	mustCreate(t, c, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: fx.host + "-bootstrap-token", Namespace: namespace},
		Data: map[string][]byte{
			"plaintext-token":       []byte(fx.token),
			"token-expires-at":      expires,
			"plaintext-boot-nonce":  []byte(fx.nonce),
			"boot-nonce-expires-at": expires,
			"consumer":              []byte(b7mName),
		},
	})
	return fx
}

func (fx callbackFixture) route(base, kind string) string {
	if kind == "boot" {
		return fmt.Sprintf("%s/api/v1/boot/%s/%s/%s", base, fx.namespace, fx.host, fx.nonce)
	}
	return fmt.Sprintf("%s/api/v1/%s/%s/%s", base, kind, fx.namespace, fx.host)
}

func mustCreate(t *testing.T, c client.Client, obj client.Object) {
	t.Helper()
	if err := c.Create(context.Background(), obj); err != nil {
		t.Fatalf("create %T %s: %v", obj, obj.GetName(), err)
	}
}

func createNamespace(t *testing.T, c client.Client, prefix string) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: prefix}}
	mustCreate(t, c, ns)
	return ns.Name
}

// runCallbackOnlyManager starts a --controllers=none manager on restCfg
// serving the callback routes on a free port from certDir, its cache scoped to
// watchNamespaces the way main does for --watch-namespaces (all namespaces
// when none are given). It returns the callback server's base URL once the
// cache has synced, and stops the manager when the test ends.
func runCallbackOnlyManager(t *testing.T, restCfg *rest.Config, certDir string, watchNamespaces ...string) string {
	t.Helper()
	opts := ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	}
	if len(watchNamespaces) > 0 {
		namespaces := make(map[string]cache.Config, len(watchNamespaces))
		for _, ns := range watchNamespaces {
			namespaces[ns] = cache.Config{}
		}
		opts.Cache = cache.Options{DefaultNamespaces: namespaces}
	}
	mgr, err := ctrl.NewManager(restCfg, opts)
	if err != nil {
		t.Fatalf("create manager: %v", err)
	}
	port := freePort(t)
	base := fmt.Sprintf("https://127.0.0.1:%d", port)
	cfg := managerConfig{
		controllers:             controllersNone,
		bootstrapURLBase:        base,
		inspectionPort:          port,
		inspectionCertDir:       certDir,
		inspectionTimeout:       controllers.DefaultInspectionTimeout,
		deploymentTimeout:       controllers.DefaultDeploymentTimeout,
		maxConcurrentReconciles: controllers.DefaultMaxConcurrentReconciles,
	}
	if err := setupManager(mgr, cfg); err != nil {
		t.Fatalf("setupManager: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		// controller-runtime's Start never returns when it is cancelled before
		// its cache has synced (runnableGroup.Start loops on ctx.Done), which is
		// how a forbidden informer fails; don't let that hang the test binary.
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("manager exited with error: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Error("manager did not stop within 30s of cancel")
		}
	})
	// An informer whose list is forbidden never syncs; bound the wait so a
	// missing grant fails the test instead of hanging it.
	syncCtx, syncCancel := context.WithTimeout(ctx, 30*time.Second)
	defer syncCancel()
	if !mgr.GetCache().WaitForCacheSync(syncCtx) {
		t.Fatal("manager cache did not sync within 30s (is an informer's list or watch forbidden?)")
	}
	return base
}

// callbackRequest sends one request, with a bearer token and a body when given,
// and returns the status and the response body.
func callbackRequest(t *testing.T, c *http.Client, method, url, bearer, body string) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: read body: %v", method, url, err)
	}
	return resp.StatusCode, got
}
