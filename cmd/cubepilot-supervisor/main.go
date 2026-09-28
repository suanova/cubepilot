// Command cubepilot-supervisor is the agent-pod-side runtime supervisor
// (final architecture): it pulls the resolved agent config from the platform
// internal API, renders domain skills into the OpenClaw workspace as
// skills, and manages the OpenClaw gateway process. The gateway reloads its
// own config, so the supervisor restarts it only when the child process
// exits -- never a pod delete. It replaces the ConfigMap-based skill
// channel and the API->gateway direct client.
package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"

	"k8s.io/klog/v2"

	"github.com/suanova/cubepilot/internal/logging"
	"github.com/suanova/cubepilot/internal/supervisor"
)

func main() {
	cfg := supervisor.LoadFromEnv()

	// The supervisor runs no controller-runtime, so there is no context for a
	// logger to travel in: client-go's klog.FromContext falls back to
	// klog.Background(). Background returns the logger set here only when
	// ContextualLogger is set, so that option is load-bearing rather than
	// decorative.
	klog.SetLoggerWithOptions(logging.New(cfg.LogLevel), klog.ContextualLogger(true))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if err := supervisor.New(cfg).Run(ctx); err != nil {
		log.Fatalf("supervisor: %v", err)
	}
}
