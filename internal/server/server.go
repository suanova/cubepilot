// Package server exposes the CubePilot Portal and REST/SSE API, routing chat
// turns to per-user OpenClaw instances via the Instance Manager. Instance
// state comes from AgentInstance CRs; the server also serves the platform
// objects (Agent / Capability / Model / Task / TaskRun) over the API (design
// CubePilot-Cloud-for-Agents-Simplified-Design.md §2.1). The API process is
// stateless except for the JSON metadata store (message ledger / audit /
// inspect reports / agent config) on a single RWO PVC. Task scheduling is
// owned by the operator's CRD scheduler — the API never runs cron loops.
package server

import (
	"log"
	"net/http"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/suanova/cubepilot/internal/capability"
	"github.com/suanova/cubepilot/internal/config"
	"github.com/suanova/cubepilot/internal/instances"
	"github.com/suanova/cubepilot/internal/metrics"
	"github.com/suanova/cubepilot/internal/store"
)

// Server holds shared dependencies for HTTP handlers.
type Server struct {
	cfg     config.Config
	mgr     *instances.Manager
	store   *store.Store
	catalog *capability.Catalog
	cr      client.Client
}

// New builds the HTTP handler for the assistant service.
func New(cfg config.Config, mgr *instances.Manager, st *store.Store, catalog *capability.Catalog, cr client.Client) *Server {
	return &Server{cfg: cfg, mgr: mgr, store: st, catalog: catalog, cr: cr}
}

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
	mux.HandleFunc("/api/models", s.handleModels)
	mux.HandleFunc("/api/taskruns", s.handleTaskRuns)
	mux.HandleFunc("/api/taskruns/", s.handleTaskRunByID)
	mux.HandleFunc("/api/kinds", s.handleKinds)
	return logRequests(mux)
}

// handleSessionSubresource routes /api/sessions/{key}/{messages|ledger|seed}.
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

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
	})
}

// logf is a small helper for handler-side logging.
func (s *Server) logf(format string, args ...any) {
	log.Printf(format, args...)
}
