package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/k8s"
)

// ---- AgentTemplate definitions (design §3.1 / §4.6: phase one = builtin
// list) ----

// handleAgentTemplates serves GET /api/agenttemplates -- the template
// registry (phase one: the builtin agent-for-cloud list; phase two opens
// user creation / review and publish).
func (s *Server) handleAgentTemplates(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET required"})
		return
	}
	if s.cr == nil {
		writeJSON(w, http.StatusOK, map[string]any{"agentTemplates": []any{}})
		return
	}
	var list v1alpha1.AgentTemplateList
	if err := s.cr.List(r.Context(), &list); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agentTemplates": list.Items})
}

// handleAgentTemplateByID serves GET /api/agenttemplates/{name}.
func (s *Server) handleAgentTemplateByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET required"})
		return
	}
	name := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/agenttemplates/"), "/")
	if name == "" || s.cr == nil {
		http.NotFound(w, r)
		return
	}
	var tmpl v1alpha1.AgentTemplate
	if err := s.cr.Get(r.Context(), types.NamespacedName{Name: name}, &tmpl); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agentTemplate": tmpl})
}

// ---- AgentInstance (design §3.2: instance list, one per user per template)
// ----

// handleInstances serves GET /api/instances (own instances only) and
// POST /api/instances (self-service provisioning of the caller's own
// AgentInstance CR; the operator's controller owns the Pod/PVC/Service
// lifecycle). design §3.2: instance key = user + template, one per user.
// Access control: the owner is always the caller -- both reads and writes
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
		// when it matches the caller; any other value is ignored -- it can
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
			TemplateRef      string   `json:"templateRef"`
			SelectedModel    string   `json:"selectedModel"`
			EnabledSkills    []string `json:"enabledSkills"`
			UserInstructions string   `json:"userInstructions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad JSON body"})
			return
		}
		templateRef := strings.TrimSpace(body.TemplateRef)
		if templateRef == "" {
			templateRef = v1alpha1.DefaultAgentName
		}
		// The AgentTemplate definition must exist (design §3.1: instances
		// reference registered templates; an unknown ref would create a
		// permanently Failed instance that the controller cannot converge).
		var tmpl v1alpha1.AgentTemplate
		if err := s.cr.Get(r.Context(), types.NamespacedName{Name: templateRef}, &tmpl); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("unknown template %q", templateRef)})
			return
		}
		owner := s.userOf(r)
		name := k8s.InstanceName(owner, templateRef)

		// Idempotent: an existing instance owned by the caller is returned
		// as-is (the controller converges it; no duplicate Pod/PVC churn).
		// An instance with the same name but a different owner is a conflict
		// -- never leak it.
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
				TemplateRef: templateRef,
				Owner:       owner,
				Identity: v1alpha1.IdentitySpec{
					Mode: v1alpha1.IdentityModeUser,
					PrincipalRef: v1alpha1.PrincipalRef{
						UserRef: owner,
					},
				},
				Lifecycle:        &v1alpha1.LifecycleSpec{Strategy: "resident"},
				SelectedModel:    strings.TrimSpace(body.SelectedModel),
				EnabledSkills:    body.EnabledSkills,
				UserInstructions: strings.TrimSpace(body.UserInstructions),
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

// ---- Skill catalog (design §3.3.1: three-layer skills;
// atomic/domain registration) ----

// handleSkills serves GET /api/skills -- the registered Skill
// CRs (atomic thin overrides + domain knowledge; the generic layer is
// platform-provided and not listed here).
func (s *Server) handleSkills(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET required"})
		return
	}
	if s.cr == nil {
		writeJSON(w, http.StatusOK, map[string]any{"skills": []any{}})
		return
	}
	var list v1alpha1.SkillList
	if err := s.cr.List(r.Context(), &list); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"skills": list.Items})
}

// ---- TaskRun (design §3.3.4: run report, written with the platform
// identity) ----

// handleTaskRuns serves GET /api/taskruns?task=... -- TaskRun CRs newest-first.
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

// handleKinds serves GET /api/kinds -- the discovered CRD schema table
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

// handleInternalGatewayConfig serves the rendered gateway config (openclaw.json)
// handleInternalGatewayConfig serves the rendered gateway config (openclaw.json)
// for one instance's supervisor: GET /internal/gateway/config/{user}. The
// operator renders the shared config (providers + allowlist; apiKey as file
// SecretRefs into the emptyDir keys.json, never literal) into the openclaw-config
// Secret; the API overrides the
// primary model with the user's selectedModel so each instance's config
// reflects its owner (design §3.2). Pulled here so provider/model changes
// apply without waiting on the kubelet Secret-volume sync (issue #6).
func (s *Server) handleInternalGatewayConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET required"})
		return
	}
	user := r.PathValue("user")
	if user == "" {
		http.NotFound(w, r)
		return
	}
	if s.cr == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "k8s client unavailable"})
		return
	}
	var sec corev1.Secret
	if err := s.cr.Get(r.Context(), types.NamespacedName{Namespace: s.cfg.Namespace, Name: k8s.ConfigSecretName}, &sec); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": fmt.Sprintf("gateway config not ready: %v", err)})
		return
	}
	raw := sec.Data["openclaw.json"]
	if len(raw) == 0 {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "gateway config empty"})
		return
	}
	// Per-user primary: the shared config carries the template default; a user
	// with an explicit selectedModel gets it as their instance's primary. A
	// resolution error is logged and the default kept (chat stays fail-closed
	// per request; the config must not fail the pod boot).
	if model, err := s.mgr.SelectedModelFor(r.Context(), user); err != nil {
		s.logf("gateway config primary for %s: %v", user, err)
	} else if model != "" {
		// SelectedModelFor already returns the <provider>/<model> primary ref.
		if over := gatewayConfigWithPrimary(raw, model); over != nil {
			raw = over
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

// gatewayConfigWithPrimary returns the openclaw.json bytes with
// agents.defaults.model.primary set to primary (the <provider>/<model> ref),
// preserving the shared allowlist and everything else. nil when the path is
// absent or the JSON is malformed (caller keeps the original).
func gatewayConfigWithPrimary(raw []byte, primary string) []byte {
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil
	}
	agents, ok := cfg["agents"].(map[string]any)
	if !ok {
		return nil
	}
	def, ok := agents["defaults"].(map[string]any)
	if !ok {
		return nil
	}
	// Fail closed: only override when the allowlist is present and the target is
	// actually in it. The operator drops models with an empty endpoint or a
	// missing credential Secret, so a selection validated only against the
	// template model list could otherwise point primary at a provider that was
	// never rendered (chat would fail); a stale/malformed Secret must keep its
	// original bytes too.
	allow, ok := def["models"].(map[string]any)
	if !ok {
		return nil
	}
	if _, ok := allow[primary]; !ok {
		return nil
	}
	if modelCfg, ok := def["model"].(map[string]any); ok {
		modelCfg["primary"] = primary
	} else {
		def["model"] = map[string]any{"primary": primary}
	}
	// encoding/json sorts map keys deterministically (Go 1.12+), so the bytes
	// are stable for the same semantic config; json.MarshalIndent keeps the file
	// human-readable.
	if b, err := json.MarshalIndent(cfg, "", "  "); err == nil {
		return b
	}
	return nil
}
