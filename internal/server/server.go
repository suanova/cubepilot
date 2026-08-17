// Package server exposes the CubePilot Portal and REST/SSE API, routing chat
// turns to per-user OpenClaw instances via the Instance Manager.
package server

import (
	"net/http"
	"strings"

	"github.com/suanova/cubepilot/internal/config"
	"github.com/suanova/cubepilot/internal/instances"
	"github.com/suanova/cubepilot/internal/metrics"
	"github.com/suanova/cubepilot/internal/store"
	"github.com/suanova/cubepilot/ui"
)

// Server holds shared dependencies for HTTP handlers.
type Server struct {
	cfg   config.Config
	mgr   *instances.Manager
	store *store.Store

	schedulerLeader schedulerLeader
}

// schedulerLeader is the minimal leader-check interface the scheduler needs
// (implemented by *leader.Elector). Multi-replica deployments only run due
// tasks on the leader (design §3.3 Active/Standby).
type schedulerLeader interface {
	IsLeader() bool
}

// New builds the HTTP handler for the assistant service.
func New(cfg config.Config, mgr *instances.Manager, st *store.Store) *Server {
	return &Server{cfg: cfg, mgr: mgr, store: st}
}

// SetSchedulerLeader attaches the leader elector that gates the scheduler.
func (s *Server) SetSchedulerLeader(e schedulerLeader) { s.schedulerLeader = e }

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
	mux.HandleFunc("/", s.handleStatic)
	return logRequests(mux)
}

// handleSessionSubresource dispatches /api/sessions/{key}/{messages|ledger|seed}.
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
