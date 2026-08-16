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

	mgr.Run(ctx)

	s := server.New(cfg, mgr, st)
	go s.StartScheduler(ctx)

	srv := &http.Server{
		Addr:    cfg.Listen,
		Handler: s.Handler(),
	}

	go func() {
		log.Printf("cubepilot listening on %s (namespace=%s)", cfg.Listen, cfg.Namespace)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down")
	_ = srv.Shutdown(context.Background())
}
