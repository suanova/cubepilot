// Command cubepilot runs the CubePilot assistant service and Instance Manager
// as a single Go process (PoC: the two production components are merged here).
package main

import (
	"context"
	"log"
	"net/http"
	"os/signal"
	"syscall"

	"github.com/suanova/cubepilot/internal/config"
	"github.com/suanova/cubepilot/internal/instances"
	"github.com/suanova/cubepilot/internal/k8s"
	"github.com/suanova/cubepilot/internal/leader"
	"github.com/suanova/cubepilot/internal/server"
	"github.com/suanova/cubepilot/internal/store"
)

func main() {
	cfg := config.Load()

	client, err := k8s.NewClient()
	if err != nil {
		log.Fatalf("k8s client: %v", err)
	}
	mgr := instances.New(client, cfg)

	st, err := store.New(cfg.DataDir)
	if err != nil {
		log.Fatalf("store: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Leader election (design §3.3 多副本 Active/Standby). Single-replica
	// deployments are leader by construction; multi-replica elect among
	// themselves via Lease. The Instance Manager reconcile / GC and the
	// scheduler only act on the leader.
	elector := leader.New(client, cfg.Namespace, leader.LeaseResourceName("instance-manager"), cfg.Replicas)
	elector.Run(ctx)
	mgr.SetElector(elector)
	mgr.Run(ctx)

	s := server.New(cfg, mgr, st)
	s.SetSchedulerLeader(elector)
	go s.StartScheduler(ctx)

	srv := &http.Server{
		Addr:    cfg.Listen,
		Handler: s.Handler(),
	}

	go func() {
		log.Printf("cubepilot listening on %s (namespace=%s, replicas=%d, reclaim=%v)",
			cfg.Listen, cfg.Namespace, cfg.Replicas, cfg.ReclaimEnabled)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down")
	_ = srv.Shutdown(context.Background())
}
