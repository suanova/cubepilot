package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/k8s"
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

// handleInstances serves GET /api/instances (own instances only) and
// POST /api/instances (self-service provisioning of the caller's own
// AgentInstance CR; the operator's controller owns the Pod/PVC/Service
// lifecycle). design §3.2: instance key = user + agent, one per user.
// Access control: the owner is always the caller — both reads and writes
// are scoped to the request identity (a user can never see or touch another
// user's instance).
func (s *Server) handleInstances(w http.ResponseWriter, r *http.Request) {
	if s.cr == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "CRD path disabled"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		// Only the caller's own instances. The ?user= filter is honored only
		// when it matches the caller; any other value is ignored — it can
		// never be used to read another user's instances.
		me := s.userOf(r)
		var list v1alpha1.AgentInstanceList
		if err := s.cr.List(r.Context(), &list); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		out := make([]v1alpha1.AgentInstance, 0, len(list.Items))
		for _, inst := range list.Items {
			if inst.Spec.Owner == me {
				out = append(out, inst)
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		writeJSON(w, http.StatusOK, map[string]any{"instances": out})
	case http.MethodPost:
		// Self-service provisioning: the instance owner is always the caller
		// (never taken from the body), so a user can only create their own.
		var body struct {
			AgentRef      string `json:"agentRef"`
			SelectedModel string `json:"selectedModel"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad JSON body"})
			return
		}
		agentRef := strings.TrimSpace(body.AgentRef)
		if agentRef == "" {
			agentRef = v1alpha1.DefaultAgentName
		}
		// The Agent definition must exist (design §3.1: instances reference
		// registered agents; an unknown ref would create a permanently Failed
		// instance that the controller cannot converge).
		var agent v1alpha1.Agent
		if err := s.cr.Get(r.Context(), types.NamespacedName{Name: agentRef}, &agent); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("unknown agent %q", agentRef)})
			return
		}
		owner := s.userOf(r)
		name := k8s.InstanceName(owner, agentRef)

		// Idempotent: an existing instance owned by the caller is returned as-is
		// (the controller converges it; no duplicate Pod/PVC churn). An instance
		// with the same name but a different owner is a conflict — never leak it.
		var existing v1alpha1.AgentInstance
		if err := s.cr.Get(r.Context(), types.NamespacedName{Name: name}, &existing); err == nil {
			if existing.Spec.Owner != owner {
				writeJSON(w, http.StatusConflict, map[string]any{"error": "instance name already taken"})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"instance": existing, "alreadyExists": true})
			return
		}

		inst := &v1alpha1.AgentInstance{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: v1alpha1.AgentInstanceSpec{
				AgentRef: agentRef,
				Owner:    owner,
				Identity: v1alpha1.IdentitySpec{
					Mode: v1alpha1.IdentityModeUser,
					PrincipalRef: v1alpha1.PrincipalRef{
						UserRef: owner,
					},
				},
				Lifecycle:     &v1alpha1.LifecycleSpec{Strategy: "resident"},
				SelectedModel: strings.TrimSpace(body.SelectedModel),
			},
		}
		if err := s.cr.Create(r.Context(), inst); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"instance": inst})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET or POST required"})
	}
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

// ---- Model catalog (design §3.3: platform model directory, admin-maintained)
// ----

// handleModels serves GET /api/models — the platform Model catalog CRs with
// their probed availability (status.phase).
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if s.cr == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "CRD path disabled"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		var list v1alpha1.ModelList
		if err := s.cr.List(r.Context(), &list); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
		writeJSON(w, http.StatusOK, map[string]any{"models": list.Items})
	case http.MethodPost:
		// Administrator adds a model catalog entry (design §3.3: admins add
		// models by applying a Model CR; the Portal provides the same path).
		var body struct {
			DisplayName   string `json:"displayName"`
			Provider      string `json:"provider"`
			Endpoint      string `json:"endpoint"`
			CredentialRef string `json:"credentialRef"`
			ModelID       string `json:"modelId"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad JSON body"})
			return
		}
		body.DisplayName = strings.TrimSpace(body.DisplayName)
		body.Provider = strings.TrimSpace(strings.ToLower(body.Provider))
		body.Endpoint = strings.TrimSpace(body.Endpoint)
		body.ModelID = strings.TrimSpace(body.ModelID)
		if body.DisplayName == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "displayName is required"})
			return
		}
		provider := v1alpha1.ModelProvider(body.Provider)
		switch provider {
		case v1alpha1.ModelProviderPlatform:
			// platform: endpoint optional (empty = builtin runtime model).
		case v1alpha1.ModelProviderExternal:
			if body.Endpoint == "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "external provider requires endpoint"})
				return
			}
			if body.CredentialRef == "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "external provider requires credentialRef"})
				return
			}
		default:
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("provider must be %q or %q", v1alpha1.ModelProviderPlatform, v1alpha1.ModelProviderExternal)})
			return
		}
		model := &v1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{
				// CR name must be DNS-1123; derive a stable slug from the
				// display name (admin may also apply a Model CR directly).
				Name: k8s.Sanitize(body.DisplayName),
			},
			Spec: v1alpha1.ModelSpec{
				DisplayName:   body.DisplayName,
				Provider:      provider,
				Endpoint:      body.Endpoint,
				CredentialRef: body.CredentialRef,
				ModelID:       body.ModelID,
			},
		}
		if err := s.cr.Create(r.Context(), model); err != nil {
			// AlreadyExists: the slug collided with an existing entry (same or
			// different display name) — return the existing entry instead of a
			// 500, so double-submit is harmless and name clashes are visible.
			if apierrors.IsAlreadyExists(err) {
				var existing v1alpha1.Model
				if getErr := s.cr.Get(r.Context(), types.NamespacedName{Name: model.Name}, &existing); getErr == nil {
					writeJSON(w, http.StatusConflict, map[string]any{"model": existing, "error": "model already exists"})
					return
				}
			}
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"model": model})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET or POST required"})
	}
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
	me := s.userOf(r)
	out := make([]v1alpha1.TaskRun, 0, len(list.Items))
	for _, run := range list.Items {
		// Owner-scoped: a user sees only runs of their own tasks (same
		// isolation as tasks/instances).
		if run.Spec.Owner != me {
			continue
		}
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
	if run.Spec.Owner != s.userOf(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "not your taskrun"})
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

// handleInternalAgentConfig serves the resolved agent config for the agent
// supervisor (internal API, cluster-only): GET /internal/agents/{user}/config.
// The supervisor polls this to render skills and detect reloads; the response
// is the immutable ResolvedAgentConfig (revision = change signal).
func (s *Server) handleInternalAgentConfig(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/internal/agents/")
	user, tail, ok := strings.Cut(rest, "/")
	if !ok || tail != "config" || user == "" {
		http.NotFound(w, r)
		return
	}
	cfg, err := s.mgr.ResolvedConfigForUser(r.Context(), user)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, cfg)
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
