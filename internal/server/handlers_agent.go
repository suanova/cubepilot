package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/k8s"
	"github.com/suanova/cubepilot/internal/store"
)

// handleAudit serves GET /api/audit?limit=400 -- newest-first M5 entries.
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

// handleAgentConfig serves GET/PUT /api/agent/config -- the persisted Agent
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
		if err := s.store.SaveAgentConfig(body.Config); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		// A model switch must take effect on the next chat turn: write it to
		// the caller's AgentInstance.selectedModel (design §3.2: switching the
		// model = editing selectedModel -> re-resolve), not just the global
		// store preference. An empty model ("Runtime Default") clears the
		// override so the gateway's configured primary decides.
		if err := s.applyModelOverride(r, body.Config.Model); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": fmt.Sprintf("update instance model: %v", err)})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"config": body.Config})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET or PUT required"})
	}
}

// applyModelOverride writes the model to the caller's AgentInstance
// selectedModel so the switch takes effect on the next chat turn (the resolver
// sends the override from spec.selectedModel). An empty model clears the
// override ("Runtime Default": the gateway's configured primary decides). A
// not-yet-provisioned instance is fine -- the provisioning path carries the
// selection when the instance is created.
func (s *Server) applyModelOverride(r *http.Request, model string) error {
	if s.cr == nil {
		return nil
	}
	user := s.userOf(r)
	name := k8s.InstanceName(user, v1alpha1.DefaultAgentName)
	var inst v1alpha1.AgentInstance
	if err := s.cr.Get(r.Context(), types.NamespacedName{Name: name}, &inst); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if inst.Spec.SelectedModel == model {
		return nil
	}
	inst.Spec.SelectedModel = model
	return s.cr.Update(r.Context(), &inst)
}

// handleAgentStatus reports the live state of the caller's agent instance
// (whether the Pod exists, its phase, uptime, idle TTL) for the Agent config
// page.
func (s *Server) handleAgentStatus(w http.ResponseWriter, r *http.Request) {
	user := s.userOf(r)
	exists, phase, startedAt := s.mgr.InstanceStatus(r.Context(), user)
	resp := map[string]any{
		"user":           user,
		"id":             "agent-" + user,
		"exists":         exists,
		"phase":          phase,
		"idleTTLMinutes": int(s.cfg.IdleTTL / time.Minute),
		"idleTTLSeconds": int(s.cfg.IdleTTL / time.Second),
		"gatewayImage":   s.cfg.AgentImage,
		"gatewayPort":    s.cfg.AgentPort,
	}
	if exists && !startedAt.IsZero() {
		resp["startedAt"] = startedAt
		resp["uptimeSeconds"] = int(time.Since(startedAt).Seconds())
	}
	writeJSON(w, http.StatusOK, resp)
}
