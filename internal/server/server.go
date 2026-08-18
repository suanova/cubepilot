// Package server exposes the CubePilot Portal and REST/SSE API, routing chat
// turns to per-user OpenClaw instances via the Instance Manager. With the
// CRD path enabled, instance state comes from AgentInstance CRs; the server
// also serves the platform objects (Agent / Capability / Task / TaskRun)
// over the API (design doc CubePilot-Cloud-for-Agents-Design.md §2.1).
package server

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/suanova/cubepilot/internal/capability"
	"github.com/suanova/cubepilot/internal/config"
	"github.com/suanova/cubepilot/internal/instances"
	"github.com/suanova/cubepilot/internal/metrics"
	"github.com/suanova/cubepilot/internal/store"
	"github.com/suanova/cubepilot/ui"
)

// Server holds shared dependencies for HTTP handlers.
type Server struct {
	cfg     config.Config
	mgr     *instances.Manager
	store   *store.Store
	catalog *capability.Catalog
	cr      client.Client

	schedulerLeader schedulerLeader
	taskRunner      schedulerRunner
}

// schedulerLeader is the minimal leader-check interface the scheduler needs
// (implemented by *leader.Elector). Multi-replica deployments only run due
// tasks on the leader (design §3.3 Active/Standby).
type schedulerLeader interface {
	IsLeader() bool
}

// schedulerRunner executes one task turn (implemented by *Server; used by the
// CRD scheduler controller).
type schedulerRunner interface {
	RunTask(ctx context.Context, creator, sessionKey, prompt string) (string, error)
}

// New builds the HTTP handler for the assistant service.
func New(cfg config.Config, mgr *instances.Manager, st *store.Store, catalog *capability.Catalog, cr client.Client) *Server {
	return &Server{cfg: cfg, mgr: mgr, store: st, catalog: catalog, cr: cr}
}

// SetSchedulerLeader attaches the leader elector that gates the scheduler.
func (s *Server) SetSchedulerLeader(e schedulerLeader) { s.schedulerLeader = e }

// SetTaskRunner attaches the task runner (the server itself implements it).
func (s *Server) SetTaskRunner(r schedulerRunner) { s.taskRunner = r }

// Handler returns the fully wired HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/metrics", metrics.Handler())
	mux.HandleFunc("/api/sessions", s.handleSessions)
	mux.HandleFunc("/api/sessions/", s.handleSessionSubresource)
	mux.HandleFunc("/api/messages", s.handleMessages)
	mux.HandleFunc("/api/inspect", s.handleInspect)
	mux.HandleFunc("/api/tasks", s.handleTasks)
	mux.HandleFunc("/api/tasks/", s.handleTaskByID)
	mux.HandleFunc("/api/audit", s.handleAudit)
	mux.HandleFunc("/api/agent/config", s.handleAgentConfig)
	mux.HandleFunc("/api/agent/status", s.handleAgentStatus)
	mux.HandleFunc("/api/agents", s.handleAgents)
	mux.HandleFunc("/api/agents/", s.handleAgentByID)
	mux.HandleFunc("/api/instances", s.handleInstances)
	mux.HandleFunc("/api/capabilities", s.handleCapabilities)
	mux.HandleFunc("/api/taskruns", s.handleTaskRuns)
	mux.HandleFunc("/api/taskruns/", s.handleTaskRunByID)
	mux.HandleFunc("/api/kinds", s.handleKinds)
	mux.HandleFunc("/", s.handleStatic)
	return logRequests(mux)
}

// StartLegacyScheduler launches the legacy FR-M4 cron loop over the JSON task
// store (kept for non-CRD deployments; the CRD scheduler controller replaces
// it when the CRD path is active).
func (s *Server) StartLegacyScheduler(ctx context.Context) {
	go func() {
		tick := time.NewTicker(30 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if s.schedulerLeader != nil && !s.schedulerLeader.IsLeader() {
					continue
				}
				s.runDue(ctx)
			}
		}
	}()
}

func (s *Server) handleSessionSubresource(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasSuffix(r.URL.Path, "/messages"):
		s.handleHistory(w, r)
	case strings.HasSuffix(r.URL.Path, "/ledger"):
		s.handleLedger(w, r)
	case strings.HasSuffix(r.URL.Path, "/seed"):
		s.handleSeed(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(ui.IndexHTML))
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
	})
}

// logf is a small helper for handler-side logging.
func (s *Server) logf(format string, args ...any) {
	log.Printf(format, args...)
}
