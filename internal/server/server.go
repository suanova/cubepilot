// Package server exposes the CubePilot Portal and REST/SSE API, routing chat
// turns to per-user OpenClaw instances via the Instance Manager.
package server

import (
	"net/http"

	"github.com/suanova/cubepilot/internal/config"
	"github.com/suanova/cubepilot/internal/instances"
	"github.com/suanova/cubepilot/internal/store"
	"github.com/suanova/cubepilot/ui"
)

// Server holds shared dependencies for HTTP handlers.
type Server struct {
	cfg   config.Config
	mgr   *instances.Manager
	store *store.Store
}

// New builds the HTTP handler for the assistant service.
func New(cfg config.Config, mgr *instances.Manager, st *store.Store) *Server {
	return &Server{cfg: cfg, mgr: mgr, store: st}
}

// Handler returns the fully wired HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/api/sessions", s.handleSessions)
	mux.HandleFunc("/api/sessions/", s.handleHistory)
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
