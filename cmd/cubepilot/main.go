// Command cubepilot runs the CubePilot assistant service and the platform
// controllers (AgentInstance / builtin bootstrap / scheduler) as a single Go
// process (PoC: the production components are merged here; the controller
// wiring follows the controller-runtime manager pattern — CNCF kubebuilder
// convention, design doc §4.1 Instance Manager 控制器化).
package main

import (
	"context"
	"log"
	"net/http"
	"os/signal"
	"syscall"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/cache"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/capability"
	"github.com/suanova/cubepilot/internal/config"
	"github.com/suanova/cubepilot/internal/controller"
	"github.com/suanova/cubepilot/internal/instances"
	"github.com/suanova/cubepilot/internal/k8s"
	"github.com/suanova/cubepilot/internal/leader"
	"github.com/suanova/cubepilot/internal/scheduler"
	"github.com/suanova/cubepilot/internal/server"
	"github.com/suanova/cubepilot/internal/store"
)

func main() {
	cfg := config.Load()

	// Route controller-runtime logs to the standard logger (visible in pod logs).
	ctrllog.SetLogger(stdLogger())

	restCfg, err := k8s.NewRestConfig()
	if err != nil {
		log.Fatalf("k8s rest config: %v", err)
	}
	client, err := k8s.NewClient()
	if err != nil {
		log.Fatalf("k8s client: %v", err)
	}
	st, err := store.New(cfg.DataDir)
	if err != nil {
		log.Fatalf("store: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// ---- controller-runtime manager (设计 §4.1: Instance Manager 控制器化) ----
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
		Metrics:                metricsserver.Options{BindAddress: "0"}, // 服务自带 /metrics; 禁用框架默认 8080
		HealthProbeBindAddress: "0",
		LeaderElection:         cfg.Replicas > 1, // 单副本起步; 多副本随框架选举 (设计 §3.3)
		LeaderElectionID:       "cubepilot-assistant.suanova.io",
	})
	if err != nil {
		log.Fatalf("controller manager: %v", err)
	}

	// Capability catalog (三层能力模型: generic 自动 + Capability atomic/domain).
	catalog, err := capability.NewCatalog(restCfg)
	if err != nil {
		log.Fatalf("capability catalog: %v", err)
	}
	if err := catalog.Refresh(ctx); err != nil {
		log.Printf("capability catalog refresh: %v (continuing)", err)
	}

	// Instance Manager facade (CRD-backed).
	mgrInstances := instances.New(client, mgr.GetClient(), cfg)

	// Leader election for the legacy in-process reconcile/GC path.
	elector := leader.New(client, cfg.Namespace, leader.LeaseResourceName("instance-manager"), cfg.Replicas)
	elector.Run(ctx)
	mgrInstances.SetElector(elector)

	// Scheduler runner: the server owns the agent-instance wiring.
	srv := server.New(cfg, mgrInstances, st, catalog, mgr.GetClient())
	srv.SetSchedulerLeader(elector)
	srv.SetTaskRunner(srv) // server implements scheduler.Runner

	// Controllers.
	bootstrap := &controller.BuiltinBootstrapReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Cfg:    cfg,
	}
	if err := (&controller.AgentInstanceReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Cfg:    cfg,
	}).SetupWithManager(mgr); err != nil {
		log.Fatalf("agentinstance controller: %v", err)
	}
	if err := bootstrap.SetupWithManager(mgr); err != nil {
		log.Fatalf("builtin bootstrap controller: %v", err)
	}
	if err := (&scheduler.ReconcileScheduler{
		Client: mgr.GetClient(),
		Cfg:    cfg,
		Runner: srv,
	}).SetupWithManager(mgr); err != nil {
		log.Fatalf("scheduler controller: %v", err)
	}

	go func() {
		if err := mgr.Start(ctx); err != nil {
			log.Fatalf("controller manager: %v", err)
		}
	}()

	// Bootstrap the builtin objects (agent-for-cloud + capabilities + template
	// + per-user instances) once the manager's cache is ready. The cache-backed
	// client cannot read before the manager starts, so we wait for the cache
	// sync first (the bootstrap controller's periodic requeue keeps it converged
	// even if this first pass is interrupted).
	if ok := mgr.GetCache().WaitForCacheSync(ctx); !ok {
		log.Printf("bootstrap: cache sync failed (controller will retry)")
	} else if err := bootstrap.Ensure(ctx); err != nil {
		log.Printf("bootstrap ensure: %v (controller will retry)", err)
	}

	srv.StartLegacyScheduler(ctx)

	httpServer := &http.Server{
		Addr:    cfg.Listen,
		Handler: srv.Handler(),
	}
	go func() {
		log.Printf("cubepilot listening on %s (namespace=%s, replicas=%d, reclaim=%v, crd-path=%v)",
			cfg.Listen, cfg.Namespace, cfg.Replicas, cfg.ReclaimEnabled, true)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down")
	_ = httpServer.Shutdown(context.Background())
}
