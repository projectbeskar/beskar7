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
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
	"github.com/projectbeskar/beskar7/controllers"
	"github.com/projectbeskar/beskar7/internal/auth"
)

// callbackOnlyRBACDir is the opt-in RBAC component for a --controllers=none
// instance, relative to this package.
var callbackOnlyRBACDir = filepath.Join("..", "..", "config", "rbac", "callback-only")

// TestCallbackOnlyRoleServesEveryRoute runs a callback-only manager as the
// identity config/rbac/callback-only creates, holding nothing but the Role that
// component binds in the one watched namespace, and drives every callback
// route the way the inspector does on a successful run (SEC-14). Every route
// must succeed: a grant missing from the Role shows up here as a cache that
// never syncs or a route that fails. test/rbac pins the same Role from the
// other side, so it can grant nothing these routes do not use.
//
// The Role lets the identity get a Secret and nothing more (CALLBACK-SECRET-READ):
// the instance reads Secrets live, so a Secret read that went through the cache
// would start an informer whose list is forbidden, and the cache would never
// sync.
//
// The apiserver runs with OwnerReferencesPermissionEnforcement, which hardened
// clusters (OpenShift among them) enable: the inspection-result ConfigMap's
// owner reference sets blockOwnerDeletion, which that plugin allows only to a
// caller that may update the PhysicalHost's finalizers.
func TestCallbackOnlyRoleServesEveryRoute(t *testing.T) {
	restCfg := startEnvtest(t, func(env *envtest.Environment) {
		env.ControlPlane.GetAPIServer().Configure().Append("enable-admission-plugins", "OwnerReferencesPermissionEnforcement")
	})
	ctx := context.Background()
	admin, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}

	watched := createNamespace(t, admin, "callback-rbac-")
	sa := applyCallbackOnlyRBAC(t, admin, watched)
	fx := createCallbackFixture(t, admin, watched)

	// The identity the docs give the instance: a token for the component's
	// ServiceAccount, nothing else.
	tokenRequest := &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: ptr.To[int64](3600)}}
	if err := admin.SubResource("token").Create(ctx, sa, tokenRequest); err != nil {
		t.Fatalf("request a token for %s/%s: %v", sa.Namespace, sa.Name, err)
	}
	callbackCfg := rest.AnonymousClientConfig(restCfg)
	callbackCfg.BearerToken = tokenRequest.Status.Token
	callbackCfg.QPS, callbackCfg.Burst, callbackCfg.RateLimiter = restCfg.QPS, restCfg.Burst, restCfg.RateLimiter
	requireSecretAccess(t, callbackCfg, watched, map[string]bool{"get": true, "list": false, "watch": false})

	certDir := t.TempDir()
	pool := writeServingCert(t, certDir)
	base := runCallbackOnlyManager(t, callbackCfg, certDir, watched)
	c := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
	}
	hostKey := types.NamespacedName{Namespace: fx.namespace, Name: fx.host}
	host := func() *infrav1.PhysicalHost {
		t.Helper()
		h := &infrav1.PhysicalHost{}
		if err := admin.Get(ctx, hostKey, h); err != nil {
			t.Fatalf("get PhysicalHost: %v", err)
		}
		return h
	}

	// /boot: reads the host, its credentials Secret and its Beskar7Machine, and
	// records the nonce consume on the host's status. The objects were in place
	// before the cache synced, so one request settles it — and /boot is
	// rate-limited, so retrying would only turn a failure into a 429.
	if status, _ := callbackRequest(t, c, http.MethodGet, fx.route(base, "boot"), "", ""); status != http.StatusOK {
		t.Fatalf("GET /boot: status %d, want 200", status)
	}
	if bs := host().Status.Bootstrap; bs == nil || bs.BootNonceConsumedAt == nil {
		t.Fatal("GET /boot succeeded without recording the nonce consume")
	}

	// /inspection: creates the result ConfigMap, owned by the host, and
	// annotates the host.
	const report = `{"manufacturer":"Acme","model":"X1"}`
	if status, _ := callbackRequest(t, c, http.MethodPost, fx.route(base, "inspection"), fx.token, report); status != http.StatusAccepted {
		t.Fatalf("POST /inspection: status %d, want 202", status)
	}
	cm := &corev1.ConfigMap{}
	if err := admin.Get(ctx, types.NamespacedName{Namespace: fx.namespace, Name: fx.host + "-inspection-result"}, cm); err != nil {
		t.Fatalf("POST /inspection succeeded without the result ConfigMap: %v", err)
	}
	if host().Annotations[controllers.InspectionResultAnnotation] == "" {
		t.Fatal("POST /inspection succeeded without annotating the host")
	}
	// Each signal carries its binding in a sibling annotation (D-034): the
	// instance computes it from the host's credentials Secret, which the Role
	// already lets it read, and the controllers act on the signal only with it.
	requireBinding := func(signal string) {
		t.Helper()
		if host().Annotations[signal+"-binding"] == "" {
			t.Fatalf("the callback annotated the host with %s but without its binding", signal)
		}
	}
	requireBinding(controllers.InspectionResultAnnotation)
	// A second, different report before the controller consumes the first (an
	// inspector retry) updates the ConfigMap. It must differ: CreateOrUpdate
	// skips the update when nothing changed. Until the cache has seen the
	// first, the handler tries to create it again and fails, so poll.
	deadline := time.Now().Add(10 * time.Second)
	for {
		status, _ := callbackRequest(t, c, http.MethodPost, fx.route(base, "inspection"), fx.token, `{"manufacturer":"Acme","model":"X2"}`)
		if status == http.StatusAccepted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("second POST /inspection: status %d, want 202 (the ConfigMap update path)", status)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err := admin.Get(ctx, client.ObjectKeyFromObject(cm), cm); err != nil {
		t.Fatalf("get the result ConfigMap: %v", err)
	}
	if !strings.Contains(cm.Data["report.json"], `"model":"X2"`) {
		t.Fatal("second POST /inspection succeeded without updating the stored report")
	}

	// /bootstrap: walks Beskar7Machine → owner Machine → bootstrap data Secret.
	status, body := callbackRequest(t, c, http.MethodGet, fx.route(base, "bootstrap"), fx.token, "")
	if status != http.StatusOK {
		t.Fatalf("GET /bootstrap: status %d, want 200", status)
	}
	if string(body) != fx.bootstrapData {
		t.Fatal("GET /bootstrap did not serve the bootstrap data Secret's value")
	}

	// /network-data (v4.3): reads the Secret spec.networkDataSecretRef names.
	status, body = callbackRequest(t, c, http.MethodGet, fx.route(base, "network-data"), fx.token, "")
	if status != http.StatusOK {
		t.Fatalf("GET /network-data: status %d, want 200", status)
	}
	if string(body) != fx.networkData {
		t.Fatal("GET /network-data did not serve the network data Secret's value")
	}

	// /provisioned and /provision-failed: annotate the host.
	if status, _ := callbackRequest(t, c, http.MethodPost, fx.route(base, "provisioned"), fx.token, ""); status != http.StatusAccepted {
		t.Fatalf("POST /provisioned: status %d, want 202", status)
	}
	if host().Annotations[controllers.ProvisionedRequestAnnotation] == "" {
		t.Fatal("POST /provisioned succeeded without annotating the host")
	}
	requireBinding(controllers.ProvisionedRequestAnnotation)
	if status, _ := callbackRequest(t, c, http.MethodPost, fx.route(base, "provision-failed"), fx.token, `{"reason":"disk write failed"}`); status != http.StatusAccepted {
		t.Fatalf("POST /provision-failed: status %d, want 202", status)
	}
	if host().Annotations[controllers.ProvisionFailedRequestAnnotation] == "" {
		t.Fatal("POST /provision-failed succeeded without annotating the host")
	}
	requireBinding(controllers.ProvisionFailedRequestAnnotation)

	// /cleaned and /clean-failed (v4.4): the host is released into a cleaning
	// run whose own token is the only one these routes accept.
	cleaningToken := startCleaningRun(t, admin, hostKey)
	for _, report := range []struct{ route, annotation string }{
		{"cleaned", controllers.CleanedRequestAnnotation},
		{"clean-failed", controllers.CleanFailedRequestAnnotation},
	} {
		// The handler ignores a host its cache does not show Cleaning yet, so poll.
		deadline := time.Now().Add(10 * time.Second)
		for host().Annotations[report.annotation] == "" {
			if status, _ := callbackRequest(t, c, http.MethodPost, fx.route(base, report.route), cleaningToken, ""); status != http.StatusAccepted {
				t.Fatalf("POST /%s: status %d, want 202", report.route, status)
			}
			if time.Now().After(deadline) {
				t.Fatalf("POST /%s never annotated the host", report.route)
			}
			time.Sleep(200 * time.Millisecond)
		}
		requireBinding(report.annotation)
	}
}

// startCleaningRun releases the host into a booted cleaning run, as the
// PhysicalHost controller leaves it, and returns the run's token.
func startCleaningRun(t *testing.T, admin client.Client, hostKey types.NamespacedName) string {
	t.Helper()
	ctx := context.Background()
	host := &infrav1.PhysicalHost{}
	if err := admin.Get(ctx, hostKey, host); err != nil {
		t.Fatalf("get PhysicalHost: %v", err)
	}
	host.Spec.ConsumerRef = nil
	host.Spec.CleaningMode = infrav1.CleaningModeMetadata
	if err := admin.Update(ctx, host); err != nil {
		t.Fatalf("release PhysicalHost: %v", err)
	}
	now := metav1.Now()
	host.Status.State = infrav1.StateCleaning
	host.Status.CleaningTimestamp = &now
	if err := admin.Status().Update(ctx, host); err != nil {
		t.Fatalf("set PhysicalHost Cleaning: %v", err)
	}
	token, _, err := auth.MintToken()
	if err != nil {
		t.Fatalf("mint cleaning token: %v", err)
	}
	nonce, _, err := auth.MintToken()
	if err != nil {
		t.Fatalf("mint cleaning nonce: %v", err)
	}
	secret := &corev1.Secret{}
	if err := admin.Get(ctx, types.NamespacedName{Namespace: hostKey.Namespace, Name: hostKey.Name + "-bootstrap-token"}, secret); err != nil {
		t.Fatalf("get bootstrap-token Secret: %v", err)
	}
	expires := []byte(time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	secret.Data = map[string][]byte{
		"plaintext-token":       []byte(token),
		"token-expires-at":      expires,
		"plaintext-boot-nonce":  []byte(nonce),
		"boot-nonce-expires-at": expires,
		"purpose":               []byte("clean"),
	}
	if err := admin.Update(ctx, secret); err != nil {
		t.Fatalf("store cleaning credentials: %v", err)
	}
	return token
}

// requireSecretAccess asks the apiserver, as the identity cfg authenticates as,
// whether it may perform each verb on Secrets in namespace, and waits for the
// answers to match want. It holds the test to the Role it claims to run under,
// and the wait covers the authorizer's RBAC informers catching up with a Role
// created a moment ago.
func requireSecretAccess(t *testing.T, cfg *rest.Config, namespace string, want map[string]bool) {
	t.Helper()
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("create client for the callback identity: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		var mismatch string
		for verb, allowed := range want {
			review := &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{
				ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: namespace, Verb: verb, Resource: "secrets"},
			}}
			if err := c.Create(context.Background(), review); err != nil {
				t.Fatalf("review %s on secrets: %v", verb, err)
			}
			if review.Status.Allowed != allowed {
				mismatch = fmt.Sprintf("%s on secrets in %s: allowed=%t, want %t", verb, namespace, review.Status.Allowed, allowed)
			}
		}
		if mismatch == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the callback identity does not hold the Secret access this test assumes: %s", mismatch)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// applyCallbackOnlyRBAC applies the component as docs/ipxe-setup.md does:
// config/rbac/callback-only as written, and its watched-namespace part with
// the namespace set to watched. It creates the ServiceAccount's namespace
// first and returns the ServiceAccount.
func applyCallbackOnlyRBAC(t *testing.T, c client.Client, watched string) *corev1.ServiceAccount {
	t.Helper()
	var sa *corev1.ServiceAccount
	for _, obj := range kustomizationObjects(t, callbackOnlyRBACDir) {
		if obj.GetKind() != "ServiceAccount" {
			t.Fatalf("%s: unexpected %s %s; only the ServiceAccount belongs there", callbackOnlyRBACDir, obj.GetKind(), obj.GetName())
		}
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: obj.GetNamespace()}}
		if err := c.Create(context.Background(), ns); err != nil {
			t.Fatalf("create namespace %s: %v", ns.Name, err)
		}
		mustCreate(t, c, obj)
		sa = &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: obj.GetName(), Namespace: obj.GetNamespace()}}
	}
	if sa == nil {
		t.Fatalf("%s creates no ServiceAccount", callbackOnlyRBACDir)
	}
	for _, obj := range kustomizationObjects(t, filepath.Join(callbackOnlyRBACDir, "watched-namespace")) {
		obj.SetNamespace(watched)
		mustCreate(t, c, obj)
	}
	return sa
}

var manifestSeparator = regexp.MustCompile(`(?m)^---[ \t]*$`)

// kustomizationObjects decodes every document of every file dir's
// kustomization.yaml lists under resources.
func kustomizationObjects(t *testing.T, dir string) []*unstructured.Unstructured {
	t.Helper()
	read := func(path string) []byte {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return b
	}
	var k struct {
		Resources []string `json:"resources"`
	}
	if err := yaml.Unmarshal(read(filepath.Join(dir, "kustomization.yaml")), &k); err != nil {
		t.Fatalf("parse %s/kustomization.yaml: %v", dir, err)
	}
	var out []*unstructured.Unstructured
	for _, r := range k.Resources {
		for _, doc := range manifestSeparator.Split(string(read(filepath.Join(dir, r))), -1) {
			if strings.TrimSpace(doc) == "" {
				continue
			}
			obj := &unstructured.Unstructured{}
			if err := yaml.Unmarshal([]byte(doc), &obj.Object); err != nil {
				t.Fatalf("decode %s/%s: %v", dir, r, err)
			}
			if len(obj.Object) == 0 {
				continue // a document holding only comments
			}
			out = append(out, obj)
		}
	}
	return out
}
