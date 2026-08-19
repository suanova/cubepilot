package server

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
)

// ---- Agent definitions (design §3.1 / §4.6: Agent Registry phase one =
// builtin list) ----

// handleAgents serves GET /api/agents — the Agent Registry (phase one: the
// builtin agent-for-cloud list; phase two opens user creation / review and
// publish).
func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET required"})
		return
	}
	if s.cr == nil {
		writeJSON(w, http.StatusOK, map[string]any{"agents": []any{}})
		return
	}
	var list v1alpha1.AgentList
	if err := s.cr.List(r.Context(), &list); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": list.Items})
}

// handleAgentByID serves GET /api/agents/{name}.
func (s *Server) handleAgentByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET required"})
		return
	}
	name := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/agents/"), "/")
	if name == "" || s.cr == nil {
		http.NotFound(w, r)
		return
	}
	var agent v1alpha1.Agent
	if err := s.cr.Get(r.Context(), types.NamespacedName{Name: name}, &agent); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agent": agent})
}

// ---- AgentInstance (design §3.2: instance list, one per user per agent)
// ----

// handleInstances serves GET /api/instances?user=... — AgentInstance CRs
// (phase one: the builtin agent's per-user instance;status is maintained by
// the controller).
func (s *Server) handleInstances(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET required"})
		return
	}
	if s.cr == nil {
		writeJSON(w, http.StatusOK, map[string]any{"instances": []any{}})
		return
	}
	var list v1alpha1.AgentInstanceList
	if err := s.cr.List(r.Context(), &list); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	user := r.URL.Query().Get("user")
	out := make([]v1alpha1.AgentInstance, 0, len(list.Items))
	for _, inst := range list.Items {
		if user == "" || inst.Spec.Owner == user {
			out = append(out, inst)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, map[string]any{"instances": out})
}

// ---- Capability catalog (design §3.3.1: three-layer capabilities;
// atomic/domain registration) ----

// handleCapabilities serves GET /api/capabilities — the registered Capability
// CRs (atomic thin overrides + domain knowledge; the generic layer is
// platform-provided and not listed here).
func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET required"})
		return
	}
	if s.cr == nil {
		writeJSON(w, http.StatusOK, map[string]any{"capabilities": []any{}})
		return
	}
	var list v1alpha1.CapabilityList
	if err := s.cr.List(r.Context(), &list); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"capabilities": list.Items})
}

// ---- TaskRun (design §3.3.4: run report, written with the platform
// identity) ----

// handleTaskRuns serves GET /api/taskruns?task=... — TaskRun CRs newest-first.
func (s *Server) handleTaskRuns(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET required"})
		return
	}
	if s.cr == nil {
		writeJSON(w, http.StatusOK, map[string]any{"taskruns": []any{}})
		return
	}
	var list v1alpha1.TaskRunList
	if err := s.cr.List(r.Context(), &list); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	taskFilter := r.URL.Query().Get("task")
	out := make([]v1alpha1.TaskRun, 0, len(list.Items))
	for _, run := range list.Items {
		if taskFilter == "" || run.Spec.CreatorTaskRef.Name == taskFilter {
			out = append(out, run)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		ti, tj := out[i].CreationTimestamp, out[j].CreationTimestamp
		return ti.After(tj.Time)
	})
	writeJSON(w, http.StatusOK, map[string]any{"taskruns": out})
}

// handleTaskRunByID serves GET /api/taskruns/{name}.
func (s *Server) handleTaskRunByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET required"})
		return
	}
	name := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/taskruns/"), "/")
	if name == "" || s.cr == nil {
		http.NotFound(w, r)
		return
	}
	var run v1alpha1.TaskRun
	if err := s.cr.Get(r.Context(), types.NamespacedName{Name: name}, &run); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"taskrun": run})
}

// ---- Kinds / generic tools (design §3.3.1 generic layer: the HTTP face of
// list-kinds) ----

// handleKinds serves GET /api/kinds — the discovered CRD schema table
// (the data source for the generic layer's list-kinds / describe-kind; the
// platform reads all CRD schemas at startup).
func (s *Server) handleKinds(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET required"})
		return
	}
	if s.catalog == nil {
		writeJSON(w, http.StatusOK, map[string]any{"kinds": []any{}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"kinds": s.catalog.Schemas()})
}

// ---- shared helpers ----

func (s *Server) listCapabilities(ctx context.Context) ([]v1alpha1.Capability, error) {
	if s.cr == nil {
		return nil, nil
	}
	var list v1alpha1.CapabilityList
	if err := s.cr.List(ctx, &list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (s *Server) getAgent(ctx context.Context, name string) (*v1alpha1.Agent, error) {
	if s.cr == nil {
		return nil, fmt.Errorf("CRD path disabled")
	}
	var agent v1alpha1.Agent
	if err := s.cr.Get(ctx, types.NamespacedName{Name: name}, &agent); err != nil {
		return nil, err
	}
	return &agent, nil
}

// writeObjectJSON marshals a metav1 object's JSON for API responses.
func writeObjectJSON(w http.ResponseWriter, status int, v any) {
	writeJSON(w, status, v)
}

var _ = metav1.Now
