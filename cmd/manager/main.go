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

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
	"github.com/projectbeskar/beskar7/api/v1beta2/webhooks"
	"github.com/projectbeskar/beskar7/controllers"
	internalmetrics "github.com/projectbeskar/beskar7/internal/metrics"
	//+kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	// Register Cluster API types
	utilruntime.Must(clusterv1.AddToScheme(scheme))

	utilruntime.Must(infrav1.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
}

//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;clusterrolebindings,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch
//+kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var leaderElectionLeaseDuration time.Duration
	var leaderElectionRenewDeadline time.Duration
	var leaderElectionRetryPeriod time.Duration
	var enableWebhook bool
	var webhookPort int
	var webhookCertDir string
	var secureMetrics bool
	var bootstrapURLBase string
	var inspectionPort int
	var inspectionCertDir string
	var watchNamespacesRaw string
	var inspectionTimeout time.Duration
	var deploymentTimeout time.Duration
	var maxConcurrentReconciles int
	var trustedProxies string
	var controllersRaw string

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8443", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.StringVar(&bootstrapURLBase, "bootstrap-url-base", "https://beskar7-controller-manager.capb7-system.svc:8082",
		"Base URL operators expose for the bootstrap and inspection callback endpoints. "+
			"Used to compute per-machine bootstrap URLs.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", true,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager. "+
			"Always off with --controllers=none; passing --leader-elect=true there is rejected.")
	flag.DurationVar(&leaderElectionLeaseDuration, "leader-elect-lease-duration", 15*time.Second,
		"The duration that non-leader candidates will wait to force acquire leadership.")
	flag.DurationVar(&leaderElectionRenewDeadline, "leader-elect-renew-deadline", 10*time.Second,
		"The interval between attempts by the acting master to renew a leadership slot before it stops leading.")
	flag.DurationVar(&leaderElectionRetryPeriod, "leader-elect-retry-period", 2*time.Second,
		"The duration the clients should wait between attempting acquisition and renewal of a leadership.")
	flag.BoolVar(&enableWebhook, "enable-webhook", false,
		"Enable webhook server for admission control and defaulting. Rejected with --controllers=none.")
	flag.IntVar(&webhookPort, "webhook-port", 9443,
		"Webhook server port.")
	flag.StringVar(&webhookCertDir, "webhook-cert-dir", "/tmp/k8s-webhook-server/serving-certs",
		"Webhook server certificate directory.")
	flag.BoolVar(&secureMetrics, "secure-metrics", true,
		"Serve metrics over HTTPS with authentication and authorization via the kube-apiserver. "+
			"Set to false only for local development.")
	flag.IntVar(&inspectionPort, "inspection-port", 8082,
		"Port the inspection HTTPS endpoint binds to.")
	flag.StringVar(&inspectionCertDir, "inspection-cert-dir", "/tmp/k8s-webhook-server/serving-certs",
		"Directory containing tls.crt and tls.key for the inspection HTTPS endpoint. "+
			"Defaults to the webhook cert dir; both endpoints are served from the same Pod and "+
			"can share a cert covering the controller-manager Service DNS name.")
	flag.StringVar(&watchNamespacesRaw, "watch-namespaces", "",
		"Comma-separated list of namespaces the controller should watch. Empty "+
			"(default) means watch all namespaces (the historical behavior). When set, "+
			"informers are scoped to the listed namespaces — used together with "+
			"per-namespace Role/RoleBinding to tighten Secret RBAC (SEC-2). RBAC and "+
			"chart-side per-namespace bindings ship in follow-up PRs; this flag alone "+
			"only narrows the cache.")
	flag.DurationVar(&inspectionTimeout, "inspection-timeout", controllers.DefaultInspectionTimeout,
		"How long a host may stay in the Inspecting phase before the Beskar7Machine is "+
			"marked terminally failed (InspectionTimedOut). Raise this for hardware with "+
			"slow BIOS POST or slow first-boot inspection.")
	flag.DurationVar(&deploymentTimeout, "deployment-timeout", controllers.DefaultDeploymentTimeout,
		"How long a host may stay in the Deploying phase before the Beskar7Machine is "+
			"marked terminally failed (DeploymentTimedOut). Raise this for hardware with "+
			"slow storage or very large OS images (D-015).")
	flag.IntVar(&maxConcurrentReconciles, "max-concurrent-reconciles", controllers.DefaultMaxConcurrentReconciles,
		"Number of concurrent reconcile workers per controller. The default (1) matches "+
			"controller-runtime and preserves historical behaviour. Raise it when a fleet is "+
			"large enough that a single unreachable BMC's 30s Redfish timeout stalls reconciles "+
			"for healthy hosts; controller-runtime never reconciles the same object "+
			"concurrently, so distinct workers always act on distinct BMCs.")
	flag.StringVar(&trustedProxies, "trusted-proxies", "",
		"Comma-separated CIDRs (or bare IPs) whose X-Forwarded-For header the /boot rate "+
			"limiter will believe when identifying the client. Empty (the default) ignores "+
			"the header and rate-limits on the peer address, which is the only safe "+
			"behaviour on this ungated route. Set it when the callback server sits behind "+
			"something that rewrites the source address — a LoadBalancer or NodePort "+
			"Service with the default externalTrafficPolicy: Cluster, or an L4 proxy "+
			"without PROXY protocol — otherwise every booting host shares one bucket and a "+
			"fleet powering on together starves on it.")
	flag.StringVar(&controllersRaw, "controllers", string(controllersAll),
		"Which reconcilers this instance runs. 'all' (the default) registers the "+
			"Beskar7Machine, Beskar7Cluster and PhysicalHost controllers. 'none' is "+
			"callback-only: the instance serves the host-callback HTTPS endpoints (/boot, "+
			"/api/v1/inspection, /api/v1/bootstrap, /api/v1/provisioned, "+
			"/api/v1/provision-failed) and the health probes but registers no reconciler "+
			"or webhook. Use 'none' for a second instance placed on the provisioning "+
			"network when PXE-booting hosts cannot reach the management cluster; a second "+
			"full manager would fight the first over host claims and the bootstrap-url "+
			"annotation. 'none' turns leader election off and rejects --enable-webhook=true.")

	// Default to production-safe zap config: structured JSON output, no stack
	// traces below Error, level-based encoding. Operators who want
	// development-style output (console encoder, stack traces from Warn,
	// uncolored timestamps) opt in with --zap-devel=true. opts.BindFlags
	// registers --zap-devel and the rest of the controller-runtime zap flags;
	// the field below sets the default that flag would otherwise pick up.
	opts := zap.Options{
		Development: false,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	controllersMode, err := parseControllersMode(controllersRaw)
	if err != nil {
		setupLog.Error(err, "invalid --controllers")
		os.Exit(1)
	}

	// Parsed here rather than inside the /boot handler so a typo is a startup
	// failure instead of a silently ignored setting that only shows up as
	// unexplained 429s during a fleet boot.
	parsedTrustedProxies, err := controllers.ParseTrustedProxies(trustedProxies)
	if err != nil {
		setupLog.Error(err, "invalid --trusted-proxies")
		os.Exit(1)
	}
	if len(parsedTrustedProxies) > 0 {
		setupLog.Info("Trusting X-Forwarded-For from configured proxies for /boot rate limiting",
			"networks", len(parsedTrustedProxies))
	}

	if maxConcurrentReconciles < 1 {
		setupLog.Info("--max-concurrent-reconciles below 1; using the default",
			"requested", maxConcurrentReconciles, "effective", controllers.DefaultMaxConcurrentReconciles)
		maxConcurrentReconciles = controllers.DefaultMaxConcurrentReconciles
	}

	cfg := managerConfig{
		controllers:             controllersMode,
		enableLeaderElection:    enableLeaderElection,
		enableWebhook:           enableWebhook,
		bootstrapURLBase:        bootstrapURLBase,
		inspectionPort:          inspectionPort,
		inspectionCertDir:       inspectionCertDir,
		trustedProxies:          parsedTrustedProxies,
		inspectionTimeout:       inspectionTimeout,
		deploymentTimeout:       deploymentTimeout,
		maxConcurrentReconciles: maxConcurrentReconciles,
	}
	// flag.Visit only sees flags that were actually given, which is how
	// validate tells the --leader-elect default apart from an explicit request.
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "leader-elect" {
			cfg.leaderElectSet = true
		}
	})
	if err := cfg.validate(); err != nil {
		setupLog.Error(err, "contradictory flags")
		os.Exit(1)
	}
	if cfg.controllers == controllersNone {
		setupLog.Info("Callback-only mode: serving the host-callback endpoints and health probes only; "+
			"no reconciler or webhook will be registered and leader election is off",
			"controllers", string(cfg.controllers))
	}

	// H-1: Warn if --bootstrap-url-base is a cluster-internal .svc address.
	// Bare-metal hosts boot outside the cluster and cannot resolve cluster DNS;
	// a .svc URL forces operators toward InsecureSkipVerify and the inspector's
	// TLS verification will fail (contract §8). Operators MUST override
	// --bootstrap-url-base with an externally-reachable address whose serving
	// cert has a matching SAN.
	if strings.Contains(bootstrapURLBase, ".svc") {
		setupLog.Info("WARNING: --bootstrap-url-base contains a cluster-internal .svc address; "+
			"bare-metal hosts cannot reach cluster DNS and the inspector's TLS verification will fail. "+
			"Override --bootstrap-url-base with an externally-reachable address (LoadBalancer/NodePort/Ingress) "+
			"whose TLS certificate has a matching SAN (contract §8).",
			"bootstrapURLBase", bootstrapURLBase)
	}

	// Setup metrics registry
	internalmetrics.Init()

	// Configure webhook server
	webhookServerOptions := webhook.Options{
		Port:    webhookPort,
		CertDir: webhookCertDir,
	}

	metricsOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
	}
	if secureMetrics {
		// Authenticate and authorize /metrics via TokenReview/SubjectAccessReview delegated to
		// the kube-apiserver. Requires the manager ServiceAccount to have the
		// authentication.k8s.io:tokenreviews and authorization.k8s.io:subjectaccessreviews create
		// verbs (see config/rbac/metrics_auth_role.yaml).
		metricsOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	managerOpts := ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsOptions,
		WebhookServer:          webhook.NewServer(webhookServerOptions),
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         cfg.leaderElection(),
		LeaderElectionID:       "beskar7.infrastructure.cluster.x-k8s.io",
		LeaseDuration:          &leaderElectionLeaseDuration,
		RenewDeadline:          &leaderElectionRenewDeadline,
		RetryPeriod:            &leaderElectionRetryPeriod,
	}

	// Scope informers to the listed namespaces when --watch-namespaces is set.
	// Empty list = all namespaces (default cache.Options behavior); we deliberately
	// avoid setting DefaultNamespaces in that case so we don't accidentally lock
	// the cache to an empty map (which would cache nothing).
	if watchNamespaces := parseWatchNamespaces(watchNamespacesRaw); len(watchNamespaces) > 0 {
		defaultNamespaces := make(map[string]cache.Config, len(watchNamespaces))
		for _, ns := range watchNamespaces {
			defaultNamespaces[ns] = cache.Config{}
		}
		managerOpts.Cache = cache.Options{DefaultNamespaces: defaultNamespaces}
		setupLog.Info("Scoping informers to namespaces", "namespaces", watchNamespaces)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), managerOpts)
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := setupManager(mgr, cfg); err != nil {
		setupLog.Error(err, "unable to set up manager")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// setupManager registers what the manager runs on top of its cache: the
// reconcilers, the host-callback HTTPS server, the Beskar7Cluster webhook and
// the health probes. cfg.controllers gates the reconcilers and, through
// validate, the webhook:
//
//   - controllersAll: the normal manager — everything above.
//   - controllersNone: callback-only. No reconciler and no webhook is
//     registered; the callback server and the probes still are, on the same
//     cached client the handlers always use (the bearer verifier reads
//     PhysicalHost status through it; the /boot and bootstrap handlers read
//     Beskar7Machine, PhysicalHost, the owning Machine and Secrets).
//
// It is split out of main so a test can wire a manager against envtest and
// check what each mode registers. cfg is validated again here so the function
// is safe to call with a hand-built config.
func setupManager(mgr ctrl.Manager, cfg managerConfig) error {
	if err := cfg.validate(); err != nil {
		return err
	}

	if cfg.controllers == controllersAll {
		if err := setupControllers(mgr, cfg); err != nil {
			return err
		}
	}

	// Setup the host-callback HTTPS server. Hosts the inspection POST, bootstrap
	// GET, and the /boot iPXE endpoint on the same port (default :8082). The
	// /boot endpoint is NOT bearer-gated; it uses the single-use boot nonce
	// (D-009) and is rate-limited per source IP. TLS is mandatory and the cert
	// dir defaults to the webhook cert dir (same Pod, same DNS name, one
	// Certificate via cert-manager).
	// bootstrapURLBase is passed so the /boot handler can render beskar7.api=
	// into the iPXE cmdline (§5). It must be externally reachable from bare metal.
	if err := controllers.SetupCallbackServer(mgr, cfg.inspectionPort, cfg.inspectionCertDir, cfg.bootstrapURLBase, cfg.trustedProxies); err != nil {
		return fmt.Errorf("unable to setup callback server: %w", err)
	}

	// Setup webhooks if enabled. validate has already refused this in
	// callback-only mode.
	if cfg.enableWebhook {
		setupLog.Info("Setting up webhooks")
		if err := (&webhooks.Beskar7ClusterWebhook{}).SetupWebhookWithManager(mgr); err != nil {
			return fmt.Errorf("unable to setup webhook %s: %w", "Beskar7Cluster", err)
		}
	}
	//+kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("unable to set up health check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("unable to set up ready check: %w", err)
	}
	return nil
}

// setupControllers registers the three reconcilers. RedfishClientFactory is
// intentionally omitted; SetupWithManager defaults it to
// internalredfish.NewClient and returns an error if it remains nil after
// defaulting.
func setupControllers(mgr ctrl.Manager, cfg managerConfig) error {
	setupLog.Info("Reconciler concurrency", "maxConcurrentReconciles", cfg.maxConcurrentReconciles)

	if err := (&controllers.Beskar7MachineReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		Log:               ctrl.Log.WithName("controllers").WithName("Beskar7Machine"),
		BootstrapURLBase:  cfg.bootstrapURLBase,
		InspectionTimeout: cfg.inspectionTimeout,
		DeploymentTimeout: cfg.deploymentTimeout,

		MaxConcurrentReconciles: cfg.maxConcurrentReconciles,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("unable to create controller %s: %w", "Beskar7Machine", err)
	}

	if err := (&controllers.Beskar7ClusterReconciler{
		Client:                  mgr.GetClient(),
		Scheme:                  mgr.GetScheme(),
		MaxConcurrentReconciles: cfg.maxConcurrentReconciles,
	}).SetupWithManager(context.Background(), mgr, controller.Options{}); err != nil {
		return fmt.Errorf("unable to create controller %s: %w", "Beskar7Cluster", err)
	}

	if err := (&controllers.PhysicalHostReconciler{
		Client:                  mgr.GetClient(),
		Scheme:                  mgr.GetScheme(),
		Log:                     ctrl.Log.WithName("controllers").WithName("PhysicalHost"),
		Recorder:                mgr.GetEventRecorderFor("beskar7-physicalhost-controller"), //nolint:staticcheck // legacy recorder: moving to events.EventRecorder changes every Eventf call site; follow-up to D-023
		MaxConcurrentReconciles: cfg.maxConcurrentReconciles,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("unable to create controller %s: %w", "PhysicalHost", err)
	}
	return nil
}
