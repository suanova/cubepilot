package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/k8s"
	"github.com/suanova/cubepilot/internal/skill"
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

// handleInternalSkillTar serves the tar for one skill: GET
// /internal/skills/{name}/tar (internal API, cluster-only). The supervisor
// pulls this to extract skills into the instance PVC workspace/skills.
func (s *Server) handleInternalSkillTar(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET required"})
		return
	}
	name := r.PathValue("name")
	if name == "" {
		http.NotFound(w, r)
		return
	}
	if s.cr == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "k8s client unavailable"})
		return
	}
	var skillCR v1alpha1.Skill
	if err := s.cr.Get(r.Context(), client.ObjectKey{Name: name}, &skillCR); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	repo := &skill.PathRepository{Root: s.cfg.SkillsDir}
	rc, err := repo.Open(r.Context(), skillCR.Spec.Source.Path)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "skill tar not found"})
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/gzip")
	if _, err := io.Copy(w, rc); err != nil {
		return
	}
}

// maxSkillTarSize caps a published skill tar (SKILL.md + scripts + references
// are text; a few MB is generous).
const maxSkillTarSize = 10 << 20

// skillNamePattern matches a DNS subdomain (a Skill CR name is cluster-scoped):
// lowercase alphanumerics with '-' / '.' separators, alphanumeric start and end.
var skillNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)

// handlePublishSkill is the user-facing skill publish endpoint: POST
// /api/skills/{name}/publish. The body is a gzip tar of the skill directory
// (packed client-side by the Portal); metadata comes via query params
// (displayName, description). Phase 1 forces visibility=Platform and records
// the publisher identity (X-CubePilot-User) on the Skill CR. The API owns the
// repository: atomic write, versioning and sha256 are the #22 publishSkill
// primitive, shared with the builtin seed.
func (s *Server) handlePublishSkill(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST required"})
		return
	}
	name := r.PathValue("name")
	q := r.URL.Query()
	displayName := q.Get("displayName")
	if name == "" || displayName == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "name and displayName are required"})
		return
	}
	// The name becomes both a repository path segment and the Skill CR name;
	// validate it as a DNS subdomain so an invalid upload is rejected before
	// any tar is persisted (the CRD would reject the name and orphan the tar).
	if len(name) > 253 || !skillNamePattern.MatchString(name) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid skill name: use a lowercase DNS-style slug"})
		return
	}
	if v := q.Get("visibility"); v != "" && v != string(v1alpha1.SkillVisibilityPlatform) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "phase 1 supports only visibility=Platform"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxSkillTarSize+1))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if len(body) > maxSkillTarSize {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "skill tar exceeds the size limit"})
		return
	}
	if len(body) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "empty skill tar"})
		return
	}
	// Reject malformed / wrong-folder archives (no root SKILL.md) before
	// anything is persisted.
	if err := skill.ValidateSkillTar(body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid skill tar: " + err.Error()})
		return
	}
	skillCR, err := s.publishSkill(r.Context(), name, displayName, q.Get("description"),
		v1alpha1.SkillVisibilityPlatform, body, publishOptions{Publisher: s.userOf(r)})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, skillCR)
}

// publishOptions carries the non-body dimensions of a publish: Builtin (seed
// only — user uploads are never builtin) and Publisher (the identity header
// for Portal uploads, "system" for the builtin seed), recorded on the CR.
type publishOptions struct {
	Builtin   bool
	Publisher string
}

// publishSkill is the shared publish primitive used by the HTTP endpoint and
// the startup seed: it writes the tar atomically (versioned, sha256) into the
// repository and upserts the Skill CRD (status.phase=Available).
func (s *Server) publishSkill(ctx context.Context, name, displayName, description string,
	visibility v1alpha1.SkillVisibility, tar []byte, opts publishOptions) (*v1alpha1.Skill, error) {
	if s.cr == nil {
		return nil, fmt.Errorf("k8s client unavailable")
	}
	repo := &skill.PathRepository{Root: s.cfg.SkillsDir}
	sha := sha256Hex(tar)
	ver, stored, err := skill.ResolveVersion(ctx, repo, name, sha)
	if err != nil {
		return nil, err
	}
	if !stored {
		if _, err := repo.WriteBytes(ctx, fmt.Sprintf("skills/%s/%s.tar.gz", name, ver), tar); err != nil {
			return nil, err
		}
	}

	// Upsert the Skill CRD.
	key := client.ObjectKey{Name: name}
	var skillCR v1alpha1.Skill
	err = s.cr.Get(ctx, key, &skillCR)
	switch {
	case err == nil:
		skillCR.Spec = publishSpec(displayName, description, visibility, name, ver, sha)
		if opts.Builtin {
			addBuiltinLabels(&skillCR)
		} else if skillCR.Labels != nil {
			// A user re-publish of a builtin name must not keep the builtin marker.
			delete(skillCR.Labels, "cubepilot/builtin")
		}
		addPublisherAnnotation(&skillCR, opts.Publisher)
		if err := s.cr.Update(ctx, &skillCR); err != nil {
			return nil, err
		}
	case apierrors.IsNotFound(err):
		skillCR = v1alpha1.Skill{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if opts.Builtin {
			addBuiltinLabels(&skillCR)
		}
		addPublisherAnnotation(&skillCR, opts.Publisher)
		skillCR.Spec = publishSpec(displayName, description, visibility, name, ver, sha)
		if err := s.cr.Create(ctx, &skillCR); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}
	// status subresource: mark Available.
	if err := s.patchSkillPhase(ctx, name, v1alpha1.SkillPhaseAvailable); err != nil {
		return nil, err
	}
	skillCR.Status.Phase = v1alpha1.SkillPhaseAvailable
	return &skillCR, nil
}

// addPublisherAnnotation records who published the skill (the Portal identity
// header, or "system" for the builtin seed) on the Skill CR.
func addPublisherAnnotation(skillCR *v1alpha1.Skill, publisher string) {
	if skillCR.Annotations == nil {
		skillCR.Annotations = map[string]string{}
	}
	skillCR.Annotations["cubepilot/publisher"] = publisher
}

func publishSpec(displayName, description string, visibility v1alpha1.SkillVisibility, name, ver, sha string) v1alpha1.SkillSpec {
	return v1alpha1.SkillSpec{
		DisplayName: displayName,
		Description: description,
		Visibility:  visibility,
		Source: v1alpha1.SkillSource{
			Type:   v1alpha1.SkillSourcePath,
			Path:   fmt.Sprintf("skills/%s/%s.tar.gz", name, ver),
			Sha256: sha,
		},
	}
}

func addBuiltinLabels(skillCR *v1alpha1.Skill) {
	if skillCR.Labels == nil {
		skillCR.Labels = map[string]string{}
	}
	skillCR.Labels["app.kubernetes.io/part-of"] = "cubepilot"
	skillCR.Labels["cubepilot/builtin"] = "true"
}

func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// patchSkillPhase sets status.phase via the status subresource (idempotent).
func (s *Server) patchSkillPhase(ctx context.Context, name string, phase v1alpha1.SkillPhase) error {
	skillCR := &v1alpha1.Skill{ObjectMeta: metav1.ObjectMeta{Name: name}}
	patch := fmt.Sprintf(`{"status":{"phase":%q}}`, phase)
	return s.cr.Status().Patch(ctx, skillCR, client.RawPatch(types.MergePatchType, []byte(patch)))
}
