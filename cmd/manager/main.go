// Quartet operator manager entry point.
//
// Contract highlights:
// - non-root (arbitrary UID — image USER, no fixed runAsUser; see
// config/manager/manager.yaml), readOnlyRootFilesystem;
// - leader election on a coordination lease in QUARTET_NAMESPACE, falling
// back to POD_NAMESPACE, then quartet-system (replica=1 + mandatory
// lease election);
// - health/readiness probes on 0.0.0.0:8081 by contract (all pod
// interfaces — 10.0.0.0 is NOT a wildcard and fails to bind unless that
// IP was assigned to the pod), overridable;
// - logging follows the organizational go-logutils contract (same
// mechanism as the CSI drivers): logutils.New + Instance.Logger wrapped
// into logr via zapr for controller-runtime. The --log flag value is
// passed to logutils.ParseLevel UNQUOTED — quoted values silently fall
// back to info (the same trap as the Helm | quote bug). Secret
// redaction discipline: the operator never reads Secret values;
// logs carry Secret NAMES only.
// - metrics TLS is opt-in via --metrics-cert-dir (plain HTTP otherwise).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/go-logr/zapr"
	logutils "github.com/ngxstorage/go-logutils"

	"k8s.io/client-go/discovery"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/ngxstorage/quartet-operator/api/v1alpha1"
	"github.com/ngxstorage/quartet-operator/internal/controller"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		logLevel             string
		logDirectory         string
		metricsAddr          string
		probeAddr            string
		enableLeaderElection bool
		certDir              string
	)
	// --log takes the bare level (debug|info|warn|error|...); do not quote it.
	flag.StringVar(&logLevel, "log", "info",
		"Console log level; parsed by logutils.ParseLevel (quoted values silently fall back to info).")
	// Directory normally comes from the deployment env (NGXSTORAGE_CSI_LOG_DIR
	// per the go-logutils contract); the flag exists for override/testing.
	// An empty directory logs stdout-only.
	flag.StringVar(&logDirectory, "log-dir", "",
		"Directory for rotating Quartet log files; empty means stdout only.")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8443",
		"The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", "0.0.0.0:8081",
		"The address the health and readiness probe endpoints bind to. Default 0.0.0.0:8081 listens on all pod interfaces; a literal IP (e.g. 10.0.0.0) is refused by the kernel unless that IP is assigned to the pod.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", true,
		"Enable leader election for controller manager (mandatory, lease-based).")
	flag.StringVar(&certDir, "metrics-cert-dir", "",
		"Opt-in TLS for the metrics endpoint: directory holding tls.crt/tls.key. Empty serves plain HTTP.")
	flag.Parse()

	// go-logutils contract: FilePrefix identifies the process, NodeID the
	// instance (POD name when running in-cluster, else quartet-manager).
	nodeID := os.Getenv("POD_NAME")
	if nodeID == "" {
		nodeID = "quartet-manager"
	}
	if logDirectory == "" {
		logDirectory = os.Getenv(logutils.LogDirectoryEnv)
	}
	logInstance, err := logutils.New(logutils.Config{
		ConsoleLevel: logLevel, // UNQUOTED by contract
		Directory:    logDirectory,
		NodeID:       nodeID,
		FilePrefix:   "quartet-operator",
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "quartet-operator: failed to initialize logging: %v\n", err)
		os.Exit(1)
	}
	defer logInstance.Close()

	// Instance.Logger (*zap.Logger) → logr.Logger for controller-runtime.
	// (The manager performs no SDK calls; when SDK integration lands, wire it
	// with logutils.NewSDKLogger(logInstance.Logger) as the reference.)
	ctrl.SetLogger(zapr.NewLogger(logInstance.Logger))
	setupLog := ctrl.Log.WithName("setup")

	// Leader election namespace: QUARTET_NAMESPACE per contract, with
	// POD_NAMESPACE as the kustomize-conventional fallback and quartet-system
	// as the final default, matching the explicit namespace wiring in
	// config/rbac (the rendered RoleBinding grants the lease in quartet-system
	// for the quartet-manager SA — an unset env would otherwise place the
	// lease where the binding does not grant it).
	leaderElectionNamespace := os.Getenv("QUARTET_NAMESPACE")
	if leaderElectionNamespace == "" {
		leaderElectionNamespace = os.Getenv("POD_NAMESPACE")
	}
	if leaderElectionNamespace == "" {
		leaderElectionNamespace = "quartet-system"
	}

	restCfg := ctrl.GetConfigOrDie()

	// Platform discovery (config.openshift.io group presence).
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(restCfg)
	if err != nil {
		setupLog.Error(err, "unable to create discovery client")
		os.Exit(1)
	}

	// Uncached client for the reconciler's safety-critical fresh reads: the
	// ownership gate and finalizer cleanup enumeration must never read a
	// possibly-stale cache. Everything else goes through the manager's
	// cached client.
	directClient, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		setupLog.Error(err, "unable to create direct (uncached) client")
		os.Exit(1)
	}

	// Since controller-runtime v0.20 the metrics endpoint serves HTTPS by
	// default (self-signed); --metrics-cert-dir opts in to operator-managed
	// certificates from a directory holding tls.crt/tls.key.
	metricsOpts := metricsserver.Options{BindAddress: metricsAddr}
	if certDir != "" {
		metricsOpts.CertDir = certDir
	}

	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsOpts,
		HealthProbeBindAddress:  probeAddr,
		LeaderElection:          enableLeaderElection,
		LeaderElectionID:        "quartet.ngxstorage.com",
		LeaderElectionNamespace: leaderElectionNamespace,
		// Lease timings: controller-runtime defaults (15s/10s/2s) already
		// match the driver coordination convention; controller-runtime
		// v0.21 does not expose per-manager lease overrides.
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	if err := (&controller.QuartetReconciler{
		Client:            mgr.GetClient(),
		DirectClient:      directClient,
		Scheme:            mgr.GetScheme(),
		Log:               ctrl.Log.WithName("controllers").WithName("Quartet"),
		PlatformDiscovery: discoveryClient,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Quartet")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	setupLog.Info("starting manager",
		"metricsBindAddress", metricsAddr,
		"healthProbeBindAddress", probeAddr,
		"leaderElection", enableLeaderElection,
		"metricsTLS", certDir != "")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
