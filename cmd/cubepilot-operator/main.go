// Command cubepilot-operator runs the CubePilot platform controllers
// (AgentInstance / builtin bootstrap / task scheduler) as a controller-runtime
// manager. It has no HTTP listener and no data PVC: it owns the control plane
// (CRDs + per-user agent Pod/PVC/Service lifecycle) and is the only component
// that runs leader election. Design doc §4.1 renders the Instance Manager as
// a controller; §9 splits the components (operator vs API/Portal).
package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/config"
	"github.com/suanova/cubepilot/internal/controller"
	"github.com/suanova/cubepilot/internal/instances"
	"github.com/suanova/cubepilot/internal/k8s"
	"github.com/suanova/cubepilot/internal/logrlog"
	"github.com/suanova/cubepilot/internal/runner"
	"github.com/suanova/cubepilot/internal/scheduler"
)

func main() {
	cfg := config.Load()

	ctrllog.SetLogger(logrlog.New())

	restCfg, err := k8s.NewRestConfig()
	if err != nil {
		log.Fatalf("k8s rest config: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))

	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme: scheme,
		// Namespace-scoped resources (agent Pods / Services / PVCs) live in the
		// cubepilot namespace only; scope the cache there so the manager's RBAC
		// (namespace Role) suffices. Cluster-scoped platform CRDs are cached
		// cluster-wide via the ClusterRole.
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{cfg.Namespace: {}},
		},
		// Health/readiness probes and standard controller-runtime metrics
		// (leader-election + reconcile counters). Ports are configurable via
		// the chart; both default off so a single-replica control plane does
		// not conflict with the API listener.
		Metrics:                metricsserver.Options{BindAddress: cfg.MetricsAddr},
		HealthProbeBindAddress: cfg.ProbeAddr,
		// Leader election: the operator is the only component that elects.
		// The scheduler fires non-idempotent actions (TaskRun creation +
		// a real agent turn), so exactly one replica must run it (design
		// §3.3 Active/Standby). AgentInstance reconcile is idempotent but
		// follows the same leader for consistency.
		LeaderElection:   true,
		LeaderElectionID: "cubepilot-operator.suanova.io",
	})
	if err != nil {
		log.Fatalf("controller manager: %v", err)
	}

	// Probes: /healthz and /readyz (controller-runtime built-ins, no auth;
	// network-scoped by the chart's probes).
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		log.Fatalf("healthz: %v", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		log.Fatalf("readyz: %v", err)
	}

	// Instance facade + task runner (direct OpenClaw gateway, no API process).
	mgrInstances := instances.New(mgr.GetClient(), cfg)
	taskRunner := runner.New(mgrInstances, cfg.GatewayToken)

	// Controllers.
	if err := (&controller.AgentInstanceReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Cfg:    cfg,
	}).SetupWithManager(mgr); err != nil {
		log.Fatalf("agentinstance controller: %v", err)
	}
	if err := (&controller.BuiltinBootstrapReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Cfg:    cfg,
	}).SetupWithManager(mgr); err != nil {
		log.Fatalf("builtin bootstrap controller: %v", err)
	}
	if err := (&scheduler.ReconcileScheduler{
		Client: mgr.GetClient(),
		Cfg:    cfg,
		Runner: taskRunner,
	}).SetupWithManager(mgr); err != nil {
		log.Fatalf("scheduler controller: %v", err)
	}

	// Bootstrap the builtin objects (agent-for-cloud + skills + template
	// + per-user instances) once the manager's cache is ready. The cache-backed
	// client cannot read before the manager starts, so we start the manager
	// first, wait for the cache sync, then run the initial ensure (the
	// bootstrap controller's periodic requeue keeps it converged even if this
	// first pass is interrupted).
	go func() {
		if err := mgr.Start(ctx); err != nil {
			log.Fatalf("controller manager: %v", err)
		}
	}()
	if ok := mgr.GetCache().WaitForCacheSync(ctx); !ok {
		log.Printf("bootstrap: cache sync failed (controller will retry)")
	} else if err := bootstrapEnsure(ctx, mgr, cfg); err != nil {
		log.Printf("bootstrap ensure: %v (controller will retry)", err)
	}

	log.Printf("cubepilot-operator started (namespace=%s, replicas=%d, reclaim=%v)",
		cfg.Namespace, cfg.Replicas, cfg.ReclaimEnabled)
	<-ctx.Done()
	log.Println("shutting down")
}

// bootstrapEnsure runs the builtin bootstrap once (see the controller's
// Ensure; kept here so the operator performs the initial create directly).
func bootstrapEnsure(ctx context.Context, mgr ctrl.Manager, cfg config.Config) error {
	b := &controller.BuiltinBootstrapReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Cfg:    cfg,
	}
	return b.Ensure(ctx)
}
