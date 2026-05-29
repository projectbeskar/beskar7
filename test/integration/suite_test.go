//go:build integration

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

// Package integration contains manager-level envtest integration tests for Beskar7.
//
// These tests start a real ctrl.Manager with all three reconcilers wired (PhysicalHost,
// Beskar7Machine, and Beskar7Cluster), exercise cross-controller annotation/ConfigMap
// handoffs under real eventual-consistency timing, and assert the full provision-,
// secret-rotation-, and delete-release flows. They run under the CI integration job
// (go test -v -tags=integration ./test/integration/... -timeout=30m) which already
// provisions envtest assets.
//
// We wire Beskar7Cluster as well as the two core reconcilers because:
//   - Its SetupWithManager registers the PhysicalHostToBeskar7Clusters watch mapper.
//   - Omitting it would leave that code path entirely untested at this tier.
//   - The controller is stateless and fast; it adds negligible suite overhead.
//
// The suite shares one envtest / one Manager across all specs for speed. Each spec
// creates its own GenerateName namespace so there is no shared state between specs.
package integration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	infrastructurev1beta1 "github.com/projectbeskar/beskar7/api/v1beta1"
	"github.com/projectbeskar/beskar7/controllers"
	internalmetrics "github.com/projectbeskar/beskar7/internal/metrics"
	internalredfish "github.com/projectbeskar/beskar7/internal/redfish"
)

var (
	cfg         *rest.Config
	k8sClient   client.Client
	testEnv     *envtest.Environment
	mgr         ctrl.Manager
	mgrCtx      context.Context
	mgrCancel   context.CancelFunc
	suiteCtx    context.Context
	suiteCancel context.CancelFunc
)

func TestIntegration(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Integration Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	suiteCtx, suiteCancel = context.WithCancel(context.Background())

	// Register types before starting envtest so the CRD watcher knows the GVKs.
	Expect(infrastructurev1beta1.AddToScheme(scheme.Scheme)).To(Succeed())
	Expect(clusterv1.AddToScheme(scheme.Scheme)).To(Succeed())

	// Auto-resolve envtest assets when KUBEBUILDER_ASSETS is unset. Mirrors the
	// pattern in controllers/suite_test.go — release-0.20 matches controller-runtime
	// v0.20.x; @latest currently resolves a Go 1.26-requiring build that ships
	// assets without the etcd binary so we pin the release tag explicitly.
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		By("resolving envtest assets via setup-envtest")
		cmd := exec.Command("bash", "-lc",
			"go run sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.20 use 1.31.x -p path")
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
			// CRD path is relative to this file: test/integration/ → ../../config/
			filepath.Join("..", "..", "config", "crd", "bases"),
			filepath.Join("..", "..", "config", "test-external-crds"),
		},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: os.Getenv("KUBEBUILDER_ASSETS"),
	}

	var err error
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	// Disable rate limiting entirely in tests to prevent throttling timeouts
	// when the suite creates many objects across multiple specs.
	cfg.QPS = 1000.0
	cfg.Burst = 2000
	cfg.RateLimiter = flowcontrol.NewFakeAlwaysRateLimiter()

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())

	// Initialise the global metrics registry once for the suite. This is safe
	// to call multiple times (it is a no-op after the first call).
	internalmetrics.Init()

	// SkipNameValidation prevents the global controller-runtime metric registry
	// from rejecting duplicate controller names when the manager is shared across
	// specs. Required because both the PhysicalHost and Beskar7Machine controllers
	// register named metric collectors that share global Prometheus label space.
	skipNameValidation := true
	mgr, err = ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme.Scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Controller: config.Controller{
			SkipNameValidation: &skipNameValidation,
		},
	})
	Expect(err).NotTo(HaveOccurred())

	// Mock Redfish factory: returns a MockClient for every NewClient call. This
	// lets both the PhysicalHostReconciler and the Beskar7MachineReconciler boot
	// without a real BMC, while SetPowerState / SetBootSourcePXE / GetPowerState
	// calls succeed and the MockClient tracks call state for assertions.
	mockFactory := internalredfish.RedfishClientFactory(
		func(_ context.Context, _, _, _ string, _ bool, _ []byte) (internalredfish.Client, error) {
			return internalredfish.NewMockClient(), nil
		},
	)

	// Wire PhysicalHostReconciler — owns BMC connection lifecycle, inspection
	// report persistence, and the SecretToPhysicalHosts watch mapper.
	phReconciler := &controllers.PhysicalHostReconciler{
		Client:               mgr.GetClient(),
		Scheme:               mgr.GetScheme(),
		Log:                  ctrl.Log.WithName("integration-physicalhost"),
		Recorder:             mgr.GetEventRecorderFor("beskar7-physicalhost-controller"),
		RedfishClientFactory: mockFactory,
	}
	Expect(phReconciler.SetupWithManager(mgr)).To(Succeed())

	// Wire Beskar7MachineReconciler — claims PhysicalHosts, drives inspection,
	// and exposes PhysicalHostToBeskar7Machine watch mapper.
	b7mReconciler := &controllers.Beskar7MachineReconciler{
		Client:               mgr.GetClient(),
		Scheme:               mgr.GetScheme(),
		Log:                  ctrl.Log.WithName("integration-beskar7machine"),
		RedfishClientFactory: mockFactory,
		// Use a stable local URL for bootstrap URL computation; the callback
		// server is not started in this suite (we simulate the inspector
		// directly), but the URL must be non-empty to pass validateAndDefault.
		BootstrapURLBase: "https://test-mgr.beskar7-system.svc:8082",
	}
	Expect(b7mReconciler.SetupWithManager(mgr)).To(Succeed())

	// Wire Beskar7ClusterReconciler — exercises the PhysicalHostToBeskar7Clusters
	// mapper and is present in the production manager; its absence here would
	// leave that mapper path entirely untested at this tier. The reconciler is
	// stateless and fast, so it adds negligible suite overhead.
	b7cReconciler := &controllers.Beskar7ClusterReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Log:    ctrl.Log.WithName("integration-beskar7cluster"),
	}
	Expect(b7cReconciler.SetupWithManager(suiteCtx, mgr, controller.Options{})).To(Succeed())

	// Start the manager goroutine. mgrCtx is the shared cancellation context;
	// AfterSuite cancels it to stop the manager cleanly.
	mgrCtx, mgrCancel = context.WithCancel(suiteCtx)
	go func() {
		defer GinkgoRecover()
		Expect(mgr.Start(mgrCtx)).To(Succeed())
	}()

	// Wait until all informers are synced before specs run. This prevents
	// "object not found" races on the first spec that creates objects.
	Expect(mgr.GetCache().WaitForCacheSync(mgrCtx)).To(BeTrue())
})

var _ = AfterSuite(func() {
	By("stopping the controller-runtime manager")
	mgrCancel()

	By("stopping the test environment")
	suiteCancel()
	if testEnv != nil {
		Expect(testEnv.Stop()).To(Succeed())
	}
})

// --- Shared fixture helpers ---

// createNamespace creates a fresh namespace with GenerateName and returns it.
// Callers should delete the namespace in AfterEach for isolation.
func createNamespace(ctx context.Context) *corev1.Namespace {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "integration-test-",
		},
	}
	Expect(k8sClient.Create(ctx, ns)).To(Succeed())
	return ns
}

// createBMCSecret creates a Secret with username/password data that satisfies
// PhysicalHostReconciler.getRedfishCredentials. Returns the Secret.
func createBMCSecret(ctx context.Context, ns, name string) *corev1.Secret {
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Data: map[string][]byte{
			"username": []byte("admin"),
			"password": []byte("password"),
		},
	}
	Expect(k8sClient.Create(ctx, s)).To(Succeed())
	return s
}

// createBootstrapDataSecret creates a Secret that satisfies the check in
// ensureBootstrapDataReady (the reconciler confirms the Secret exists; it does
// not read its bytes until boot time). Returns the Secret.
func createBootstrapDataSecret(ctx context.Context, ns, name string) *corev1.Secret {
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Data: map[string][]byte{
			// The value key is used by the bootstrap GET endpoint at boot time.
			// The reconciler only verifies the Secret exists.
			"value": []byte("#cloud-config\n{}"),
		},
	}
	Expect(k8sClient.Create(ctx, s)).To(Succeed())
	return s
}

// createCluster creates a minimal non-paused CAPI Cluster in the given namespace.
// The Cluster is needed because util.GetClusterFromMetadata (called by the
// Beskar7MachineReconciler) expects the cluster to exist and be non-paused.
func createCluster(ctx context.Context, ns, clusterName string) *clusterv1.Cluster {
	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterName,
			Namespace: ns,
		},
		Spec: clusterv1.ClusterSpec{},
	}
	Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
	return cluster
}

// createMachine creates a CAPI Machine in the given namespace, carrying the
// cluster-name label and a pointer to the bootstrap data Secret. Both are
// required for util.GetClusterFromMetadata and ensureBootstrapDataReady to
// advance past their early-return gates.
func createMachine(ctx context.Context, ns, machineName, clusterName, bootstrapSecretName string) *clusterv1.Machine {
	m := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      machineName,
			Namespace: ns,
			Labels: map[string]string{
				clusterv1.ClusterNameLabel: clusterName,
			},
		},
		Spec: clusterv1.MachineSpec{
			ClusterName: clusterName,
			Bootstrap: clusterv1.Bootstrap{
				DataSecretName: &bootstrapSecretName,
			},
			InfrastructureRef: corev1.ObjectReference{
				APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1",
				Kind:       "Beskar7Machine",
				Namespace:  ns,
			},
		},
	}
	Expect(k8sClient.Create(ctx, m)).To(Succeed())
	// Re-fetch to obtain the server-assigned UID (needed for ownerRef below).
	Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(m), m)).To(Succeed())
	return m
}

// createPhysicalHost creates a PhysicalHost referencing the given credentials Secret.
// The host starts with no status; the PhysicalHostReconciler will set it to Available
// after connecting to the mock BMC.
func createPhysicalHost(ctx context.Context, ns, hostName, credsSecretName string) *infrastructurev1beta1.PhysicalHost {
	h := &infrastructurev1beta1.PhysicalHost{
		ObjectMeta: metav1.ObjectMeta{
			Name:      hostName,
			Namespace: ns,
		},
		Spec: infrastructurev1beta1.PhysicalHostSpec{
			RedfishConnection: infrastructurev1beta1.RedfishConnection{
				Address:              "https://192.168.100.1",
				CredentialsSecretRef: credsSecretName,
			},
		},
	}
	Expect(k8sClient.Create(ctx, h)).To(Succeed())
	return h
}

// createBeskar7Machine creates a Beskar7Machine with an OwnerReference pointing
// at the given CAPI Machine. The OwnerRef is required for util.GetOwnerMachine
// (the first gate in Beskar7MachineReconciler.Reconcile).
func createBeskar7Machine(ctx context.Context, ns, b7mName string, ownerMachine *clusterv1.Machine) *infrastructurev1beta1.Beskar7Machine {
	b7m := &infrastructurev1beta1.Beskar7Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      b7mName,
			Namespace: ns,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "cluster.x-k8s.io/v1beta1",
					Kind:       "Machine",
					Name:       ownerMachine.Name,
					UID:        ownerMachine.UID,
					Controller: func() *bool { b := true; return &b }(),
				},
			},
			Labels: map[string]string{
				clusterv1.ClusterNameLabel: ownerMachine.Labels[clusterv1.ClusterNameLabel],
			},
		},
		Spec: infrastructurev1beta1.Beskar7MachineSpec{
			InspectionImageURL: "http://boot-server/inspect.ipxe",
			TargetImageURL:     "http://boot-server/kairos.tar.gz",
			// HardwareRequirements is nil intentionally: with no requirements the
			// validateInspectionReport fast-paths to "passed" for any report, which
			// keeps the fixture minimal.
		},
	}
	Expect(k8sClient.Create(ctx, b7m)).To(Succeed())
	return b7m
}
