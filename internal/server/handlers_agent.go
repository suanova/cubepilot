package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/suanova/cubepilot/internal/store"
)

// handleAudit serves GET /api/audit?limit=400 — newest-first M5 entries.
func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET required"})
		return
	}
	limit := 400
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	entries, err := s.store.ListAudit(limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

// handleAgentConfig serves GET/PUT /api/agent/config — the persisted Agent
// config desired state. systemPrompt is applied live to subsequent chat turns;
// model/skills are stored preferences (applied on instance rebuild).
func (s *Server) handleAgentConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg, err := s.store.GetAgentConfig()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"config": cfg})
	case http.MethodPut:
		var body struct {
			Config store.AgentConfig `json:"config"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad JSON body"})
			return
		}
		if body.Config.Model == "" {
			body.Config.Model = store.DefaultAgentConfig().Model
		}
		if err := s.store.SaveAgentConfig(body.Config); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"config": body.Config})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET or PUT required"})
	}
}

// handleAgentStatus reports the live state of the caller's agent instance
// (whether the Pod exists, its phase, uptime, idle TTL) for the Agent config
// page.
func (s *Server) handleAgentStatus(w http.ResponseWriter, r *http.Request) {
	user := s.userOf(r)
	exists, phase, startedAt := s.mgr.InstanceStatus(r.Context(), user)
	resp := map[string]any{
		"user":            user,
		"id":              "agent-" + user,
		"exists":          exists,
		"phase":           phase,
		"idleTTLMinutes":  int(s.cfg.IdleTTL / time.Minute),
		"idleTTLSeconds":  int(s.cfg.IdleTTL / time.Second),
		"gatewayImage":    s.cfg.AgentImage,
		"gatewayPort":     s.cfg.AgentPort,
	}
	if exists && !startedAt.IsZero() {
		resp["startedAt"] = startedAt
		resp["uptimeSeconds"] = int(time.Since(startedAt).Seconds())
	}
	writeJSON(w, http.StatusOK, resp)
}
