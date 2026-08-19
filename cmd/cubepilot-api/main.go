// Command cubepilot-api runs the CubePilot Portal + REST/SSE API. It is the
// stateless front for the platform: chat turns are routed to per-user OpenClaw
// instances (AgentInstance CRs reconciled by the operator), and platform
// objects (Agent / Capability / Task / TaskRun) are served read-only from
// their CRDs. The only state it holds is the JSON metadata store (tasks /
// reports / audit / agent config) on a single RWO PVC — replicas > 1 requires
// shared storage (phase two, design §11.1). No controllers and no leader
// election run here.
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
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/capability"
	"github.com/suanova/cubepilot/internal/config"
	"github.com/suanova/cubepilot/internal/instances"
	"github.com/suanova/cubepilot/internal/k8s"
	"github.com/suanova/cubepilot/internal/server"
	"github.com/suanova/cubepilot/internal/store"
)

func main() {
	cfg := config.Load()

	restCfg, err := k8s.NewRestConfig()
	if err != nil {
		log.Fatalf("k8s rest config: %v", err)
	}

	// Read-only controller-runtime client for the platform CRDs. The API
	// process never writes CRDs — the operator owns the control plane
	// (credential minimization, design §3.3.4).
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
	cr, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		log.Fatalf("cr client: %v", err)
	}

	st, err := store.New(cfg.DataDir)
	if err != nil {
		log.Fatalf("store: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Capability catalog (three-layer capability model: auto-discovered generic
	// + Capability atomic/domain).
	catalog, err := capability.NewCatalog(restCfg)
	if err != nil {
		log.Fatalf("capability catalog: %v", err)
	}
	if err := catalog.Refresh(ctx); err != nil {
		log.Printf("capability catalog refresh: %v (continuing)", err)
	}

	// Instance Manager facade (observes AgentInstance CRs; the operator's
	// controller owns the lifecycle).
	mgrInstances := instances.New(cr, cfg)

	srv := server.New(cfg, mgrInstances, st, catalog, cr)
	srv.StartLegacyScheduler(ctx)

	httpServer := &http.Server{
		Addr:    cfg.Listen,
		Handler: srv.Handler(),
	}
	go func() {
		log.Printf("cubepilot-api listening on %s (namespace=%s, reclaim=%v)",
			cfg.Listen, cfg.Namespace, cfg.ReclaimEnabled)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down")
	_ = httpServer.Shutdown(context.Background())
}
