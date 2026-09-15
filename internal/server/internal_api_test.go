package server

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/config"
	"github.com/suanova/cubepilot/internal/k8s"
	"github.com/suanova/cubepilot/internal/resolver"
	"github.com/suanova/cubepilot/internal/skill"
	"github.com/suanova/cubepilot/internal/store"
)

func internalTestAgent(name string) *v1alpha1.AgentTemplate {
	return &v1alpha1.AgentTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.AgentTemplateSpec{
			DefaultModel: "platform/deepseek-v4-flash",
			Providers: []v1alpha1.TemplateProviderSpec{
				{Name: "platform", Endpoint: "https://api.deepseek.com", Models: []string{"deepseek-v4-flash"}},
			},
			ApprovalPolicy: v1alpha1.ApprovalPolicyAllowlist,
			Instructions:   "You are the platform assistant.",
		},
	}
}

func internalTestInstance(user, agentRef string) *v1alpha1.AgentInstance {
	return &v1alpha1.AgentInstance{
		ObjectMeta: metav1.ObjectMeta{Name: k8s.InstanceName(user, agentRef)},
		Spec: v1alpha1.AgentInstanceSpec{
			TemplateRef: agentRef,
			Owner:       user,
		},
	}
}

func internalTestCap(name, path string) *v1alpha1.Skill {
	return &v1alpha1.Skill{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.SkillSpec{
			DisplayName: "Skill " + name,
			Description: "Description " + name,
			Visibility:  v1alpha1.SkillVisibilityPlatform,
			Source:      v1alpha1.SkillSource{Type: v1alpha1.SkillSourcePath, Path: path},
		},
	}
}

// internalConfigResponse is the {"config": {...}} envelope the cluster-internal
// endpoint wraps the resolved config in.
type internalConfigResponse struct {
	Config resolver.ResolvedAgentConfig `json:"config"`
}

// TestInternalAgentConfig resolves the merged config for a provisioned user
// via the internal endpoint.
func TestInternalAgentConfig(t *testing.T) {
	s := platformTestServer(t,
		internalTestAgent(v1alpha1.DefaultAgentName),
		internalTestInstance("li.ming", v1alpha1.DefaultAgentName),
		internalTestCap("cluster-inspection", "skills/cluster-inspection/v1.tar.gz"),
	)

	rec := doReq(t, s.Handler(), http.MethodGet, "/internal/agents/li.ming/config", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	cfg := decode[internalConfigResponse](t, rec).Config
	if cfg.Instance != k8s.InstanceName("li.ming", v1alpha1.DefaultAgentName) {
		t.Errorf("instance = %q", cfg.Instance)
	}
	if cfg.Owner != "li.ming" {
		t.Errorf("owner = %q", cfg.Owner)
	}
	// No explicit selection -> no override; the runtime uses its primary.
	if cfg.SelectedModel != "" {
		t.Errorf("selectedModel = %q, want empty (no override for default)", cfg.SelectedModel)
	}
	if cfg.ApprovalPolicy != v1alpha1.ApprovalPolicyAllowlist {
		t.Errorf("approvalPolicy = %q", cfg.ApprovalPolicy)
	}
	if len(cfg.Skills) != 1 || cfg.Skills[0].Name != "cluster-inspection" {
		t.Errorf("skills = %+v", cfg.Skills)
	}
	if cfg.Revision == "" {
		t.Error("empty revision")
	}
}

// TestInternalAgentConfigNoInstance verifies a user without an instance gets
// an empty config (200), not an error.
func TestInternalAgentConfigNoInstance(t *testing.T) {
	s := platformTestServer(t)
	rec := doReq(t, s.Handler(), http.MethodGet, "/internal/agents/nobody/config", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	cfg := decode[internalConfigResponse](t, rec).Config
	if !cfg.Empty() {
		t.Errorf("expected empty config, got %+v", cfg)
	}
}

// TestInternalAgentConfigRevisionChanges verifies the revision is a content
// fingerprint: a skill content change produces a different revision.
func TestInternalAgentConfigRevisionChanges(t *testing.T) {
	s1 := platformTestServer(t,
		internalTestAgent(v1alpha1.DefaultAgentName),
		internalTestInstance("li.ming", v1alpha1.DefaultAgentName),
		internalTestCap("cluster-inspection", "skills/cluster-inspection/v1.tar.gz"),
	)
	rec1 := doReq(t, s1.Handler(), http.MethodGet, "/internal/agents/li.ming/config", "", nil)
	cfg1 := decode[internalConfigResponse](t, rec1).Config

	s2 := platformTestServer(t,
		internalTestAgent(v1alpha1.DefaultAgentName),
		internalTestInstance("li.ming", v1alpha1.DefaultAgentName),
		internalTestCap("cluster-inspection", "skills/cluster-inspection/v2.tar.gz"),
	)
	rec2 := doReq(t, s2.Handler(), http.MethodGet, "/internal/agents/li.ming/config", "", nil)
	cfg2 := decode[internalConfigResponse](t, rec2).Config

	if cfg1.Revision == cfg2.Revision {
		t.Errorf("skill change should change revision: %q", cfg1.Revision)
	}
	if cfg1.Revision == "" {
		t.Error("empty revision")
	}
}

// configResponse is the flat agent-config payload (no "config" wrapper: the
// response is two fields of the instance, not a nested object).
type configResponse = agentConfigView

// TestAgentConfigWithoutCRClient verifies PUT /api/v1/agent/config answers a
// controlled 503 (not a panic) when no Kubernetes client is configured,
// mirroring the nil-cr guard the GET path and model validator already have.
func TestAgentConfigWithoutCRClient(t *testing.T) {
	s := New(config.Config{DefaultUser: "zhang.wei"}, nil, nil, nil, nil)
	rec := doReq(t, s.Handler(), http.MethodPut, "/api/v1/agent/config", "zhang.wei",
		map[string]any{"selectedModel": "platform/deepseek-v4-flash"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body.String())
	}
}

// TestAgentConfigModelOverride verifies a model switch on the Agent config page
// writes the selection to the caller's AgentInstance (selectedModel) so the
// next chat turn resolves the override (regression: switching LLM did not change
// the chat model), and that "Runtime Default" clears it back to the gateway
// primary.
func TestAgentConfigModelOverride(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	s := platformTestServerStore(t, st,
		internalTestAgent(v1alpha1.DefaultAgentName),
		internalTestInstance("li.ming", v1alpha1.DefaultAgentName),
	)

	// Switch to the template model -> the override is resolved for the next turn.
	rec := doReq(t, s.Handler(), http.MethodPut, "/api/v1/agent/config", "li.ming",
		map[string]any{"selectedModel": "platform/deepseek-v4-flash"})
	if rec.Code != http.StatusOK {
		t.Fatalf("save status = %d, body = %s", rec.Code, rec.Body.String())
	}
	cfg := decode[internalConfigResponse](t, doReq(t, s.Handler(), http.MethodGet, "/internal/agents/li.ming/config", "li.ming", nil)).Config
	if cfg.SelectedModel != "platform/deepseek-v4-flash" {
		t.Errorf("selectedModel = %q, want platform/deepseek-v4-flash", cfg.SelectedModel)
	}

	// Back to "Runtime Default" -> the override is cleared (no header sent).
	rec = doReq(t, s.Handler(), http.MethodPut, "/api/v1/agent/config", "li.ming",
		map[string]any{"selectedModel": ""})
	if rec.Code != http.StatusOK {
		t.Fatalf("clear status = %d, body = %s", rec.Code, rec.Body.String())
	}
	cfg = decode[internalConfigResponse](t, doReq(t, s.Handler(), http.MethodGet, "/internal/agents/li.ming/config", "li.ming", nil)).Config
	if cfg.SelectedModel != "" {
		t.Errorf("selectedModel = %q, want empty after Runtime Default", cfg.SelectedModel)
	}
}

// TestAgentConfigReadsAndWritesInstance verifies GET/PUT /api/v1/agent/config now
// serve the caller's AgentInstance CR (design §3.2): a system-prompt edit lands
// on UserInstructions (the field the resolver actually reads) and the model
// stays on selectedModel, instead of a global store file.
func TestAgentConfigReadsAndWritesInstance(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	s := platformTestServerStore(t, st,
		internalTestAgent(v1alpha1.DefaultAgentName),
		internalTestInstance("zhang.wei", v1alpha1.DefaultAgentName),
	)

	// Fresh instance -> present with empty selections (Runtime Default / template
	// instructions only).
	resp := decode[configResponse](t, doReq(t, s.Handler(), http.MethodGet, "/api/v1/agent/config", "", nil))
	if !resp.Exists || resp.SelectedModel != "" || resp.UserInstructions != "" {
		t.Fatalf("fresh config = %+v, want exists with empty selections", resp)
	}

	// Save model + system prompt -> both land on the instance CR.
	rec := doReq(t, s.Handler(), http.MethodPut, "/api/v1/agent/config", "zhang.wei",
		map[string]any{"selectedModel": "platform/deepseek-v4-flash", "userInstructions": "You are helpful."})
	if rec.Code != http.StatusOK {
		t.Fatalf("save status = %d, body = %s", rec.Code, rec.Body.String())
	}
	resp = decode[configResponse](t, doReq(t, s.Handler(), http.MethodGet, "/api/v1/agent/config", "", nil))
	if resp.SelectedModel != "platform/deepseek-v4-flash" || resp.UserInstructions != "You are helpful." {
		t.Fatalf("config after save = %+v", resp)
	}

	// A user with no instance cannot save (there is no global config to write).
	rec = doReq(t, s.Handler(), http.MethodPut, "/api/v1/agent/config", "nobody",
		map[string]any{"selectedModel": "platform/deepseek-v4-flash"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("save without instance status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
}

// TestAgentConfigRejectsSupersededShape pins that a payload in the shape this
// endpoint used before the v1 freeze ({"config":{"model":...}}) is refused
// instead of decoding into zero values. encoding/json ignores unknown keys, so
// accepting it would CLEAR selectedModel and userInstructions and answer 200 --
// a silent wipe of the caller's configuration, which is far worse than a 400.
func TestAgentConfigRejectsSupersededShape(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	s := platformTestServerStore(t, st,
		internalTestAgent(v1alpha1.DefaultAgentName),
		internalTestInstance("zhang.wei", v1alpha1.DefaultAgentName),
	)

	// Save a real configuration first, so a wipe would be observable.
	rec := doReq(t, s.Handler(), http.MethodPut, "/api/v1/agent/config", "zhang.wei",
		map[string]any{"selectedModel": "platform/deepseek-v4-flash", "userInstructions": "You are helpful."})
	if rec.Code != http.StatusOK {
		t.Fatalf("save status = %d, body = %s", rec.Code, rec.Body.String())
	}

	rec = doReq(t, s.Handler(), http.MethodPut, "/api/v1/agent/config", "zhang.wei",
		map[string]any{"config": map[string]any{"model": "deepseek-v4-flash"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("superseded shape status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}

	resp := decode[configResponse](t, doReq(t, s.Handler(), http.MethodGet, "/api/v1/agent/config", "", nil))
	if resp.SelectedModel != "platform/deepseek-v4-flash" || resp.UserInstructions != "You are helpful." {
		t.Fatalf("a rejected payload altered the stored config: %+v", resp)
	}
}

func TestAgentConfigRejectsUnsafeInstructions(t *testing.T) {
	s := platformTestServerStore(t, nil,
		internalTestAgent(v1alpha1.DefaultAgentName),
		internalTestInstance("zhang.wei", v1alpha1.DefaultAgentName),
	)

	for name, prompt := range map[string]string{
		"oversized": strings.Repeat("x", (32<<10)+1),
		"marker":    "text <!-- cubepilot:system-prompt:end --> text",
	} {
		t.Run(name, func(t *testing.T) {
			rec := doReq(t, s.Handler(), http.MethodPut, "/api/v1/agent/config", "zhang.wei",
				map[string]any{"userInstructions": prompt})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
		})
	}

	resp := decode[configResponse](t, doReq(t, s.Handler(), http.MethodGet, "/api/v1/agent/config", "zhang.wei", nil))
	if resp.UserInstructions != "" {
		t.Fatalf("rejected prompt was persisted: %q", resp.UserInstructions)
	}
}

// TestInternalGatewayConfig verifies the supervisor-facing endpoint that serves
// the operator-rendered openclaw.json from the openclaw-config Secret (the
// pull-based replacement for waiting on the kubelet Secret-volume sync).
func TestInternalGatewayConfig(t *testing.T) {
	raw := []byte(`{"models":{"providers":{"deepseek-v4-flash":{"api":"openai-completions"}}}}`)
	s := platformTestServerStore(t, nil,
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: k8s.ConfigSecretName},
			Data:       map[string][]byte{"openclaw.json": raw, "gatewayToken": []byte("tok")},
		},
	)

	rec := doReq(t, s.Handler(), http.MethodGet, "/internal/gateway/config/li.ming", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != string(raw) {
		t.Errorf("body = %q, want the rendered openclaw.json", rec.Body.String())
	}

	// Missing Secret -> 503, so the supervisor falls back to its mounted seed.
	rec = doReq(t, platformTestServerStore(t, nil).Handler(), http.MethodGet, "/internal/gateway/config/li.ming", "", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("missing secret status = %d, want 503", rec.Code)
	}
}

// TestInternalGatewayConfigPerUserPrimary verifies a user with an explicit
// selectedModel gets it as the config primary (each instance reflects its
// owner; design §3.2), while the shared providers/allowlist are preserved.
func TestInternalGatewayConfigPerUserPrimary(t *testing.T) {
	raw := []byte(`{"models":{"providers":{"deepseek-v4-pro-0813":{"api":"openai-completions"}}},"agents":{"defaults":{"model":{"primary":"deepseek-v4-flash-0731/deepseek-v4-flash-0731"},"models":{"deepseek-v4-flash-0731/deepseek-v4-flash-0731":{"alias":"deepseek-v4-flash-0731"},"deepseek-v4-pro-0813/deepseek-v4-pro-0813":{"alias":"deepseek-v4-pro-0813"}}}}}`)
	s := platformTestServerStore(t, nil,
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: k8s.ConfigSecretName},
			Data:       map[string][]byte{"openclaw.json": raw, "gatewayToken": []byte("tok")},
		},
		// The template the instance references (must serve the selected ref).
		&v1alpha1.AgentTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "cubepilot"},
			Spec: v1alpha1.AgentTemplateSpec{
				DefaultModel: "deepseek-v4-flash-0731/deepseek-v4-flash-0731",
				Providers: []v1alpha1.TemplateProviderSpec{
					{Name: "deepseek-v4-flash-0731", Endpoint: "https://api.deepseek.com", Models: []string{"deepseek-v4-flash-0731"}},
					{Name: "deepseek-v4-pro-0813", Endpoint: "https://api.deepseek.com", Models: []string{"deepseek-v4-pro-0813"}},
				},
			},
		},
		// An instance with an explicit selectedModel (resolver looks it up by
		// ref without a namespace).
		&v1alpha1.AgentInstance{
			ObjectMeta: metav1.ObjectMeta{Name: "li-ming-cubepilot"},
			Spec: v1alpha1.AgentInstanceSpec{
				Owner:         "li.ming",
				TemplateRef:   "cubepilot",
				SelectedModel: "deepseek-v4-pro-0813/deepseek-v4-pro-0813",
			},
		},
	)

	rec := doReq(t, s.Handler(), http.MethodGet, "/internal/gateway/config/li.ming", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"primary": "deepseek-v4-pro-0813/deepseek-v4-pro-0813"`) {
		t.Errorf("primary not overridden per user: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "deepseek-v4-flash-0731/deepseek-v4-flash-0731") {
		t.Errorf("allowlist must be preserved: %s", rec.Body.String())
	}
}

// TestInternalGatewayConfigPrimaryNotInAllowlist verifies a selection pointing
// at a model the operator dropped (empty endpoint, so absent from the rendered
// allowlist) leaves primary unchanged instead of referencing a missing provider.
func TestInternalGatewayConfigPrimaryNotInAllowlist(t *testing.T) {
	raw := []byte(`{"models":{"providers":{"deepseek-v4-flash-0731":{"api":"openai-completions"}}},"agents":{"defaults":{"model":{"primary":"deepseek-v4-flash-0731/deepseek-v4-flash-0731"},"models":{"deepseek-v4-flash-0731/deepseek-v4-flash-0731":{"alias":"deepseek-v4-flash-0731"}}}}}`)
	s := platformTestServerStore(t, nil,
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: k8s.ConfigSecretName},
			Data:       map[string][]byte{"openclaw.json": raw, "gatewayToken": []byte("tok")},
		},
		// The dropped provider IS in the template (so the resolver accepts the
		// selection) but has an empty endpoint, so the operator's renderer never
		// puts its ref in the allowlist.
		&v1alpha1.AgentTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "cubepilot"},
			Spec: v1alpha1.AgentTemplateSpec{
				DefaultModel: "deepseek-v4-flash-0731/deepseek-v4-flash-0731",
				Providers: []v1alpha1.TemplateProviderSpec{
					{Name: "deepseek-v4-flash-0731", Endpoint: "https://api.deepseek.com", Models: []string{"deepseek-v4-flash-0731"}},
					{Name: "deepseek-v4-dropped", Endpoint: "", Models: []string{"deepseek-v4-dropped"}},
				},
			},
		},
		&v1alpha1.AgentInstance{
			ObjectMeta: metav1.ObjectMeta{Name: "li-ming-cubepilot"},
			Spec: v1alpha1.AgentInstanceSpec{
				Owner:         "li.ming",
				TemplateRef:   "cubepilot",
				SelectedModel: "deepseek-v4-dropped/deepseek-v4-dropped",
			},
		},
	)

	rec := doReq(t, s.Handler(), http.MethodGet, "/internal/gateway/config/li.ming", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "deepseek-v4-dropped") {
		t.Errorf("primary must not point at a dropped model: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"primary":"deepseek-v4-flash-0731/deepseek-v4-flash-0731"`) {
		t.Errorf("primary should stay the operator default: %s", rec.Body.String())
	}
}

// TestInternalSkillTar verifies the supervisor-facing endpoint that serves a
// skill's tar from the repository (for extract-into-PVC loading).
func TestInternalSkillTar(t *testing.T) {
	skillsDir := t.TempDir()
	repo := &skill.PathRepository{Root: skillsDir}
	if _, err := repo.WriteBytes(t.Context(), "skills/harbor/v1.tar.gz", mustPackBytes(t, "# Harbor\n")); err != nil {
		t.Fatalf("seed tar: %v", err)
	}
	s := platformTestServerSkillsDir(t, skillsDir, internalTestCap("harbor", "skills/harbor/v1.tar.gz"))

	rec := doReq(t, s.Handler(), http.MethodGet, "/internal/skills/harbor/tar", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/gzip" {
		t.Errorf("content-type = %q, want application/gzip", ct)
	}
	// The body must be a valid gzip tar that extracts to SKILL.md.
	tmp := t.TempDir()
	if err := skill.ExtractTar(rec.Body, tmp); err != nil {
		t.Fatalf("extract tar body: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "SKILL.md")); err != nil {
		t.Errorf("SKILL.md missing after extract: %v", err)
	}

	// Missing skill CR -> 404.
	rec = doReq(t, s.Handler(), http.MethodGet, "/internal/skills/missing/tar", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing skill status = %d, want 404", rec.Code)
	}
}

// skillEnvelope is the {"skill": {...}} wrapper the publish endpoint returns.
type skillEnvelope struct {
	Skill v1alpha1.Skill `json:"skill"`
}

// TestPublishSkill verifies the user-facing publish endpoint: it stores the
// tar atomically (versioned), upserts the Skill CRD (source.path + sha256 +
// publisher annotation), marks phase Available, and is idempotent per content.
func TestPublishSkill(t *testing.T) {
	skillsDir := t.TempDir()
	s := platformTestServerSkillsDir(t, skillsDir)

	// The identity header is recorded as the publisher on the Skill CR.
	tar1 := mustPackBytes(t, "# Harbor v1\n")
	rec := doRawPostAs(t, s.Handler(), "/api/v1/skills/harbor/publish?displayName=Harbor", tar1, "li.ming")
	if rec.Code != http.StatusCreated {
		t.Fatalf("publish #1: status = %d, want 201, body = %s", rec.Code, rec.Body.String())
	}
	published := decode[skillEnvelope](t, rec).Skill
	if published.Spec.Source.Path != "skills/harbor/v1.tar.gz" {
		t.Errorf("source.path = %q, want skills/harbor/v1.tar.gz", published.Spec.Source.Path)
	}
	if published.Spec.Source.Sha256 == "" {
		t.Error("sha256 not backfilled")
	}
	if published.Spec.Visibility != v1alpha1.SkillVisibilityPlatform {
		t.Errorf("visibility = %q, want Platform", published.Spec.Visibility)
	}
	if published.Annotations["cubepilot/publisher"] != "li.ming" {
		t.Errorf("publisher = %q, want li.ming", published.Annotations["cubepilot/publisher"])
	}
	if published.Status.Phase != v1alpha1.SkillPhaseAvailable {
		t.Errorf("phase = %q, want Available", published.Status.Phase)
	}
	if _, err := os.Stat(filepath.Join(skillsDir, "skills", "harbor", "v1.tar.gz")); err != nil {
		t.Fatalf("tar not stored: %v", err)
	}

	// Same content -> same version, no rewrite.
	rec = doRawPostAs(t, s.Handler(), "/api/v1/skills/harbor/publish?displayName=Harbor", tar1, "li.ming")
	if rec.Code != http.StatusCreated {
		t.Fatalf("publish #2: status = %d, want 201", rec.Code)
	}
	again := decode[skillEnvelope](t, rec).Skill
	if again.Spec.Source.Path != "skills/harbor/v1.tar.gz" {
		t.Errorf("re-publish changed version: %q", again.Spec.Source.Path)
	}

	// New content -> v2.
	tar2 := mustPackBytes(t, "# Harbor v2\n")
	rec = doRawPostAs(t, s.Handler(), "/api/v1/skills/harbor/publish?displayName=Harbor", tar2, "li.ming")
	if rec.Code != http.StatusCreated {
		t.Fatalf("publish v2: status = %d, want 201", rec.Code)
	}
	v2 := decode[skillEnvelope](t, rec).Skill
	if v2.Spec.Source.Path != "skills/harbor/v2.tar.gz" {
		t.Errorf("new content version = %q, want v2", v2.Spec.Source.Path)
	}

	// Missing displayName -> 400.
	rec = doRawPostAs(t, s.Handler(), "/api/v1/skills/harbor/publish", tar1, "li.ming")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing displayName status = %d, want 400", rec.Code)
	}

	// A tar without root SKILL.md -> 400, nothing persisted.
	rec = doRawPostAs(t, s.Handler(), "/api/v1/skills/harbor/publish?displayName=Harbor", mustPackBytesNoSkill(t, "# scripts only\n"), "li.ming")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no-SKILL.md status = %d, want 400", rec.Code)
	}

	// Malformed / non-gzip body -> 400, nothing persisted.
	rec = doRawPostAs(t, s.Handler(), "/api/v1/skills/harbor/publish?displayName=Harbor", []byte("not a tar"), "li.ming")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid tar status = %d, want 400", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(skillsDir, "skills", "harbor", "v3.tar.gz")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("invalid tar should not be persisted: %v", err)
	}

	// Oversized body -> 413.
	rec = doRawPostAs(t, s.Handler(), "/api/v1/skills/harbor/publish?displayName=Harbor", make([]byte, maxSkillTarSize+1), "li.ming")
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized status = %d, want 413", rec.Code)
	}

	// Non-Platform visibility -> 400 (phase 1).
	rec = doRawPostAs(t, s.Handler(), "/api/v1/skills/harbor/publish?displayName=Harbor&visibility=User", tar1, "li.ming")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("visibility=User status = %d, want 400", rec.Code)
	}

	// Invalid (non-DNS) skill name -> 400, nothing persisted.
	rec = doRawPostAs(t, s.Handler(), "/api/v1/skills/Bad_Name/publish?displayName=Bad", tar1, "li.ming")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid name status = %d, want 400", rec.Code)
	}
}

// TestPublishSkillClearsBuiltinLabel verifies a user re-publish of a builtin
// skill name drops the cubepilot/builtin marker (the CR becomes user-owned).
func TestPublishSkillClearsBuiltinLabel(t *testing.T) {
	skillsDir := t.TempDir()
	s := platformTestServerSkillsDir(t, skillsDir)

	tar1 := mustPackBytes(t, "# v1\n")
	if _, err := s.publishSkill(t.Context(), "harbor", "Harbor", "", v1alpha1.SkillVisibilityPlatform, tar1, publishOptions{Builtin: true, Publisher: "system"}); err != nil {
		t.Fatalf("seed builtin: %v", err)
	}

	rec := doRawPostAs(t, s.Handler(), "/api/v1/skills/harbor/publish?displayName=Harbor", tar1, "li.ming")
	if rec.Code != http.StatusCreated {
		t.Fatalf("publish: status = %d, want 201, body = %s", rec.Code, rec.Body.String())
	}
	var cr v1alpha1.Skill
	if err := s.cr.Get(t.Context(), client.ObjectKey{Name: "harbor"}, &cr); err != nil {
		t.Fatalf("get: %v", err)
	}
	if cr.Labels["cubepilot/builtin"] == "true" {
		t.Error("user publish retained the cubepilot/builtin label")
	}
	if cr.Annotations["cubepilot/publisher"] != "li.ming" {
		t.Errorf("publisher = %q, want li.ming", cr.Annotations["cubepilot/publisher"])
	}
}

func mustPackBytes(t *testing.T, skillBody string) []byte {
	t.Helper()
	data, err := skill.Pack(fstest.MapFS{"SKILL.md": {Data: []byte(skillBody)}})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func mustPackBytesNoSkill(t *testing.T, body string) []byte {
	t.Helper()
	data, err := skill.Pack(fstest.MapFS{"scripts/x.sh": {Data: []byte(body)}})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func doRawPostAs(t *testing.T, h http.Handler, path string, data []byte, user string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/gzip")
	if user != "" {
		req.Header.Set("X-CubePilot-User", user)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestSeedBuiltinSkills verifies the API's startup seed registers the embedded
// presets into the repository + Skill CRDs (Platform/Path/Available + builtin
// label), idempotently.
func TestSeedBuiltinSkills(t *testing.T) {
	skillsDir := t.TempDir()
	s := platformTestServerSkillsDir(t, skillsDir)

	if err := s.seedBuiltinSkillsOnce(t.Context()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var list v1alpha1.SkillList
	if err := s.cr.List(t.Context(), &list); err != nil {
		t.Fatalf("list skills: %v", err)
	}
	if len(list.Items) != len(skill.BuiltinSkillNames()) {
		t.Fatalf("skills = %d, want %d", len(list.Items), len(skill.BuiltinSkillNames()))
	}
	for _, sk := range list.Items {
		if sk.Spec.Visibility != v1alpha1.SkillVisibilityPlatform {
			t.Errorf("skill %s: visibility = %q, want Platform", sk.Name, sk.Spec.Visibility)
		}
		if sk.Spec.Source.Type != v1alpha1.SkillSourcePath || sk.Spec.Source.Path == "" {
			t.Errorf("skill %s: source = %+v, want Path with a path", sk.Name, sk.Spec.Source)
		}
		if sk.Labels["cubepilot/builtin"] != "true" {
			t.Errorf("skill %s: builtin label missing", sk.Name)
		}
		if sk.Annotations["cubepilot/publisher"] != "system" {
			t.Errorf("skill %s: publisher = %q, want system", sk.Name, sk.Annotations["cubepilot/publisher"])
		}
		if _, err := os.Stat(filepath.Join(skillsDir, "skills", sk.Name, "v1.tar.gz")); err != nil {
			t.Errorf("skill %s: tar missing in repo: %v", sk.Name, err)
		}
	}

	// Idempotent: re-seeding keeps the same versioned path.
	before := list.Items[0].Spec.Source.Path
	if err := s.seedBuiltinSkillsOnce(t.Context()); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	var after v1alpha1.Skill
	if err := s.cr.Get(t.Context(), client.ObjectKey{Name: list.Items[0].Name}, &after); err != nil {
		t.Fatalf("re-get: %v", err)
	}
	if after.Spec.Source.Path != before {
		t.Errorf("re-seed changed path %q -> %q", before, after.Spec.Source.Path)
	}
}
