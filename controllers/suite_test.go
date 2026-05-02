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
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	infrastructurev1beta1 "github.com/projectbeskar/beskar7/api/v1beta1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"

	//+kubebuilder:scaffold:imports
	"k8s.io/client-go/util/flowcontrol"
)

// These tests use Ginkgo (BDD-style Go testing framework). Refer to
// http://onsi.github.io/ginkgo/ to learn more about Ginkgo.

var cfg *rest.Config
var k8sClient client.Client
var testEnv *envtest.Environment
var ctx context.Context
var cancel context.CancelFunc

func TestAPIs(t *testing.T) {
	RegisterFailHandler(Fail)

	RunSpecs(t, "Controller Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	ctx, cancel = context.WithCancel(context.TODO())

	// === Setup Scheme FIRST ===
	// Add Beskar7 types to scheme
	Expect(infrastructurev1beta1.AddToScheme(scheme.Scheme)).To(Succeed())
	// Add CAPI types to scheme
	Expect(clusterv1.AddToScheme(scheme.Scheme)).To(Succeed())
	//+kubebuilder:scaffold:scheme

	// Ensure envtest assets are available
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		By("resolving envtest assets via setup-envtest")
		// Pinned to v0.23.3: v0.24.x requires Go 1.26+ at toolchain-resolution
		// time and the resulting envtest assets path may end up missing the etcd
		// binary. Project is on Go 1.25; controller-runtime v0.20.4.
		cmd := exec.Command("bash", "-lc", "go run sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.23.3 use 1.31.x -p path")
		cmd.Env = os.Environ()
		output, err := cmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), string(output))
		assetsPath := strings.TrimSpace(string(output))
		Expect(assetsPath).NotTo(BeEmpty())
		Expect(os.Setenv("KUBEBUILDER_ASSETS", assetsPath)).To(Succeed())
	}

	By("bootstrapping test environment")
	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "config", "crd", "bases"),       // Beskar7 CRDs
			filepath.Join("..", "config", "test-external-crds"), // Minimal external CRDs (e.g., CAPI Cluster/Machine)
		},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: os.Getenv("KUBEBUILDER_ASSETS"),
	}

	var err error
	// cfg is defined in this file globally.
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	// Configure higher rate limits for test environment to prevent timeouts
	// Default values are QPS=5, Burst=10 which are too low for concurrent tests
	// Disable rate limiting entirely for tests to prevent timeouts
	cfg.QPS = 1000.0 // Allow 1000 requests per second
	cfg.Burst = 2000 // Allow bursts of up to 2000 requests

	// Disable rate limiter entirely for tests to prevent any throttling
	cfg.RateLimiter = flowcontrol.NewFakeAlwaysRateLimiter()

	// === Scheme setup can remain here or move back, order doesn't matter as much now ===

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())

	// Setup manager and reconcilers for integration tests (optional, usually done per-spec)
	// k8sManager, err := ctrl.NewManager(cfg, ctrl.Options{
	// 	Scheme: scheme.Scheme,
	// })
	// Expect(err).ToNot(HaveOccurred())

	// err = (&PhysicalHostReconciler{
	// 	Client: k8sManager.GetClient(),
	// 	Scheme: k8sManager.GetScheme(),
	// }).SetupWithManager(k8sManager)
	// Expect(err).ToNot(HaveOccurred())
	// ... add other reconcilers

	// go func() {
	// 	defer GinkgoRecover()
	// 	err = k8sManager.Start(ctx)
	// 	Expect(err).ToNot(HaveOccurred(), "failed to run manager")
	// }()

})

var _ = AfterSuite(func() {
	cancel()
	By("tearing down the test environment")
	if testEnv != nil {
		Expect(testEnv.Stop()).To(Succeed())
	}
})
