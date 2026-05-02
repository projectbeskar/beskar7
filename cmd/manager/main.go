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
	"os"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	infrastructurev1beta1 "github.com/projectbeskar/beskar7/api/v1beta1"
	"github.com/projectbeskar/beskar7/api/v1beta1/webhooks"
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

	utilruntime.Must(infrastructurev1beta1.AddToScheme(scheme))
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

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8443", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.StringVar(&bootstrapURLBase, "bootstrap-url-base", "https://beskar7-controller-manager.beskar7-system.svc:8082",
		"Base URL operators expose for the bootstrap and inspection callback endpoints. "+
			"Used to compute per-machine bootstrap URLs.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", true,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.DurationVar(&leaderElectionLeaseDuration, "leader-elect-lease-duration", 15*time.Second,
		"The duration that non-leader candidates will wait to force acquire leadership.")
	flag.DurationVar(&leaderElectionRenewDeadline, "leader-elect-renew-deadline", 10*time.Second,
		"The interval between attempts by the acting master to renew a leadership slot before it stops leading.")
	flag.DurationVar(&leaderElectionRetryPeriod, "leader-elect-retry-period", 2*time.Second,
		"The duration the clients should wait between attempting acquisition and renewal of a leadership.")
	flag.BoolVar(&enableWebhook, "enable-webhook", false,
		"Enable webhook server for admission control and defaulting.")
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

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

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

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsOptions,
		WebhookServer:          webhook.NewServer(webhookServerOptions),
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "beskar7.infrastructure.cluster.x-k8s.io",
		LeaseDuration:          &leaderElectionLeaseDuration,
		RenewDeadline:          &leaderElectionRenewDeadline,
		RetryPeriod:            &leaderElectionRetryPeriod,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Setup controllers
	// RedfishClientFactory is intentionally omitted; SetupWithManager defaults it to
	// internalredfish.NewClient and returns an error if it remains nil after defaulting.
	if err = (&controllers.Beskar7MachineReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		Log:              ctrl.Log.WithName("controllers").WithName("Beskar7Machine"),
		BootstrapURLBase: bootstrapURLBase,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Beskar7Machine")
		os.Exit(1)
	}

	if err = (&controllers.Beskar7ClusterReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(context.Background(), mgr, controller.Options{}); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Beskar7Cluster")
		os.Exit(1)
	}

	if err = (&controllers.PhysicalHostReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Log:    ctrl.Log.WithName("controllers").WithName("PhysicalHost"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "PhysicalHost")
		os.Exit(1)
	}

	// Setup inspection handler. TLS is mandatory and the cert dir defaults to the
	// webhook cert dir (same Pod, same DNS name, one Certificate via cert-manager).
	if err := controllers.SetupInspectionServer(mgr, inspectionPort, inspectionCertDir); err != nil {
		setupLog.Error(err, "unable to setup inspection server")
		os.Exit(1)
	}

	// Setup webhooks if enabled
	if enableWebhook {
		setupLog.Info("Setting up webhooks")
		if err = (&webhooks.Beskar7ClusterWebhook{}).SetupWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to setup webhook", "webhook", "Beskar7Cluster")
			os.Exit(1)
		}
	}
	//+kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
