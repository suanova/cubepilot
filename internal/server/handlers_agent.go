package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/instructions"
	"github.com/suanova/cubepilot/internal/k8s"
)

// handleAudit serves GET /api/audit?limit=400 -- the caller's own newest-first
// audit entries (per-user ledger; a user never sees another user's activity).
func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET required"})
		return
	}
	if s.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "audit store is not configured"})
		return
	}
	limit := 400
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	entries, err := s.store.ListAudit(s.userOf(r), limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

// agentConfigView is the Agent Config page's read of the caller's agent: the
// instance's own selections live on the AgentInstance CR (design §3.2), not in
// any global config store. model is the explicit SelectedModel ("" = "Runtime
// Default": clear the override, the gateway's configured primary decides);
// systemPrompt is the UserInstructions appended after the template default
// ("" = template only).
type agentConfigView struct {
	Exists       bool   `json:"exists"`
	Model        string `json:"model"`
	SystemPrompt string `json:"systemPrompt"`
}

// handleAgentConfig serves GET/PUT /api/agent/config. GET reads the caller's
// instance; PUT writes the caller's instance (SelectedModel + UserInstructions)
// so a model/system-prompt edit takes effect on the next chat turn.
func (s *Server) handleAgentConfig(w http.ResponseWriter, r *http.Request) {
	user := s.userOf(r)
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"config": s.agentConfig(r.Context(), user)})
	case http.MethodPut:
		// Config lives on the AgentInstance CR; without the CR client there is
		// nowhere to write it, so answer a controlled 503 instead of panicking.
		if s.cr == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "agent config is stored on the AgentInstance CR, which is unavailable (no Kubernetes client)"})
			return
		}
		var body struct {
			Config agentConfigView `json:"config"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad JSON body"})
			return
		}
		// Fail at save time, not at chat time: the resolver is fail-closed on an
		// explicit selectedModel, so a model that is not in the builtin template
		// would brick the instance (issue #117 model-less default). Empty =
		// "Runtime Default" (clear the override).
		model := strings.TrimSpace(body.Config.Model)
		systemPrompt := strings.TrimSpace(body.Config.SystemPrompt)
		if err := instructions.Validate(systemPrompt); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if ok, err := s.agentTemplateHasModel(r.Context(), model); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		} else if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("model %q is not in the cubepilot template (add it under Agent Config -> LLM Config first)", model)})
			return
		}
		name := k8s.InstanceName(user, v1alpha1.DefaultAgentName)
		var inst v1alpha1.AgentInstance
		if err := s.cr.Get(r.Context(), types.NamespacedName{Namespace: s.cfg.Namespace, Name: name}, &inst); err != nil {
			if apierrors.IsNotFound(err) {
				writeJSON(w, http.StatusConflict, map[string]any{"error": errNoInstance.Error()})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		inst.Spec.SelectedModel = model
		inst.Spec.UserInstructions = systemPrompt
		if err := s.cr.Update(r.Context(), &inst); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"config": s.agentConfig(r.Context(), user)})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET or PUT required"})
	}
}

// agentConfig returns the caller's current selections from their AgentInstance,
// or a not-provisioned view when there is no instance yet.
func (s *Server) agentConfig(ctx context.Context, user string) agentConfigView {
	var v agentConfigView
	if s.cr == nil {
		return v
	}
	name := k8s.InstanceName(user, v1alpha1.DefaultAgentName)
	var inst v1alpha1.AgentInstance
	if err := s.cr.Get(ctx, types.NamespacedName{Namespace: s.cfg.Namespace, Name: name}, &inst); err != nil {
		return v // not provisioned
	}
	v.Exists = true
	v.Model = inst.Spec.SelectedModel
	v.SystemPrompt = inst.Spec.UserInstructions
	return v
}

// agentTemplateHasModel reports whether model is an inline model of the builtin
// cubepilot template (the template every AgentConfig applies to). Empty
// is always allowed ("Runtime Default"). With the CRD path disabled there is no
// template to validate against, so anything is accepted.
func (s *Server) agentTemplateHasModel(ctx context.Context, model string) (bool, error) {
	if model == "" || s.cr == nil {
		return true, nil
	}
	var tmpl v1alpha1.AgentTemplate
	if err := s.cr.Get(ctx, types.NamespacedName{Namespace: s.cfg.Namespace, Name: v1alpha1.DefaultAgentName}, &tmpl); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	for _, m := range tmpl.Spec.Models {
		if m.Name == model {
			return true, nil
		}
	}
	return false, nil
}

// handleAgentStatus reports the live state of the caller's agent instance
// (whether the Pod exists, its phase, uptime) for the Agent config page.
func (s *Server) handleAgentStatus(w http.ResponseWriter, r *http.Request) {
	user := s.userOf(r)
	exists, phase, startedAt := s.mgr.InstanceStatus(r.Context(), user)
	resp := map[string]any{
		"user":         user,
		"id":           "agent-" + user,
		"exists":       exists,
		"phase":        phase,
		"gatewayImage": s.cfg.AgentImage,
		"gatewayPort":  s.cfg.AgentPort,
	}
	if exists && !startedAt.IsZero() {
		resp["startedAt"] = startedAt
		resp["uptimeSeconds"] = int(time.Since(startedAt).Seconds())
	}
	writeJSON(w, http.StatusOK, resp)
}
