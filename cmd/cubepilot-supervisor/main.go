// Command cubepilot-supervisor is the agent-pod-side runtime supervisor
// (final architecture): it pulls the resolved agent config from the platform
// internal API, renders domain skills into the OpenClaw workspace as
// skills, and manages the OpenClaw gateway process -- graceful restart on
// config change, never a pod delete. It replaces the ConfigMap-based skill
// channel and the API->gateway direct client.
package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"

	"github.com/suanova/cubepilot/internal/supervisor"
)

func main() {
	cfg := supervisor.LoadFromEnv()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if err := supervisor.New(cfg).Run(ctx); err != nil {
		log.Fatalf("supervisor: %v", err)
	}
}
