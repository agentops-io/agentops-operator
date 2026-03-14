/*
Copyright 2026.

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
	"flag"
	"net/http"
	"os"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	arkonisv1alpha1 "github.com/arkonis-dev/ark-operator/api/v1alpha1"
	"github.com/arkonis-dev/ark-operator/internal/controller"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(arkonisv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// webhookRunnable wraps an http.Handler as a controller-runtime Runnable so it
// shares the manager lifecycle (started and gracefully shut down together).
type webhookRunnable struct {
	addr    string
	handler http.Handler
}

func triggerWebhookRunnable(addr string, handler http.Handler) *webhookRunnable {
	return &webhookRunnable{addr: addr, handler: handler}
}

func (r *webhookRunnable) Start(ctx context.Context) error {
	srv := &http.Server{Addr: r.addr, Handler: r.handler}
	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()
	select {
	case <-ctx.Done():
		return srv.Shutdown(context.Background()) //nolint:contextcheck
	case err := <-errCh:
		return err
	}
}

func buildTLSOptions(enableHTTP2 bool) []func(*tls.Config) {
	if enableHTTP2 {
		return nil
	}
	return []func(*tls.Config){func(c *tls.Config) {
		setupLog.Info("Disabling HTTP/2")
		c.NextProtos = []string{"http/1.1"}
	}}
}

func buildWebhookServerOptions(tlsOpts []func(*tls.Config), certPath, certName, certKey string) webhook.Options {
	opts := webhook.Options{TLSOpts: tlsOpts}
	if certPath != "" {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", certPath, "webhook-cert-name", certName, "webhook-cert-key", certKey)
		opts.CertDir = certPath
		opts.CertName = certName
		opts.KeyName = certKey
	}
	return opts
}

func buildMetricsServerOptions(bindAddr string, tlsOpts []func(*tls.Config), secureMetrics bool, certPath, certName, certKey string) metricsserver.Options {
	opts := metricsserver.Options{
		BindAddress:   bindAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}
	if secureMetrics {
		opts.FilterProvider = filters.WithAuthenticationAndAuthorization
	}
	if certPath != "" {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", certPath, "metrics-cert-name", certName, "metrics-cert-key", certKey)
		opts.CertDir = certPath
		opts.CertName = certName
		opts.KeyName = certKey
	}
	return opts
}

func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var agentImage string
	var triggerWebhookAddr string
	var triggerWebhookURL string
	var tlsOpts []func(*tls.Config)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	flag.StringVar(&agentImage, "agent-image", "ghcr.io/arkonis-dev/ark-runtime:latest",
		"Container image used for agent runtime pods.")
	flag.StringVar(&triggerWebhookAddr, "trigger-webhook-addr", ":8092",
		"Address for the ArkEvent webhook HTTP server to listen on.")
	flag.StringVar(&triggerWebhookURL, "trigger-webhook-url", "",
		"Base URL of the trigger webhook server, e.g. http://ark-operator.ark-system.svc.cluster.local:8092. "+
			"Written to webhook-type ArkEvent status.webhookURL.")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	tlsOpts = buildTLSOptions(enableHTTP2)
	webhookServer := webhook.NewServer(buildWebhookServerOptions(tlsOpts, webhookCertPath, webhookCertName, webhookCertKey))
	metricsServerOptions := buildMetricsServerOptions(metricsAddr, tlsOpts, secureMetrics, metricsCertPath, metricsCertName, metricsCertKey)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "a7754879.arkonis.dev",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	if err := (&controller.ArkAgentReconciler{
		Client:     mgr.GetClient(),
		Scheme:     mgr.GetScheme(),
		AgentImage: agentImage,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "ArkAgent")
		os.Exit(1)
	}
	if err := (&controller.ArkServiceReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "ArkService")
		os.Exit(1)
	}
	if err := (&controller.ArkSettingsReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "ArkSettings")
		os.Exit(1)
	}
	if err := (&controller.ArkFlowReconciler{
		Client:       mgr.GetClient(),
		Scheme:       mgr.GetScheme(),
		TaskQueueURL: os.Getenv("TASK_QUEUE_URL"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "ArkFlow")
		os.Exit(1)
	}
	if err := (&controller.ArkMemoryReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "ArkMemory")
		os.Exit(1)
	}

	triggerReconciler := &controller.ArkEventReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		TriggerWebhookURL: triggerWebhookURL,
	}
	if err := triggerReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "ArkEvent")
		os.Exit(1)
	}

	// Start the trigger webhook HTTP server as a manager Runnable so it
	// shares the manager's lifecycle (graceful shutdown on signal).
	webhookSrv := controller.NewTriggerWebhookServer(triggerReconciler)
	if err := mgr.Add(triggerWebhookRunnable(triggerWebhookAddr, webhookSrv)); err != nil {
		setupLog.Error(err, "Failed to add trigger webhook server to manager")
		os.Exit(1)
	}
	setupLog.Info("Trigger webhook server registered", "addr", triggerWebhookAddr)

	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}
