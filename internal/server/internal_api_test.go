package server

import (
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/k8s"
	"github.com/suanova/cubepilot/internal/resolver"
	"github.com/suanova/cubepilot/internal/store"
)

func internalTestAgent(name string) *v1alpha1.AgentTemplate {
	return &v1alpha1.AgentTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.AgentTemplateSpec{
			DefaultModel: "deepseek-v4-flash",
			Models: []v1alpha1.TemplateModelSpec{
				{Name: "deepseek-v4-flash", Endpoint: "https://api.deepseek.com"},
			},
			ConfirmPolicy: v1alpha1.ConfirmPolicyConfirmWrites,
			Instructions:  "You are the platform assistant.",
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

func internalTestCap(name, instructions string) *v1alpha1.Skill {
	return &v1alpha1.Skill{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.SkillSpec{
			Type:         v1alpha1.SkillDomain,
			Title:        "Skill " + name,
			Description:  "Description " + name,
			Instructions: instructions,
		},
	}
}

// TestInternalAgentConfig resolves the merged config for a provisioned user
// via the internal endpoint.
func TestInternalAgentConfig(t *testing.T) {
	s := platformTestServer(t,
		internalTestAgent(v1alpha1.DefaultAgentName),
		internalTestInstance("li.ming", v1alpha1.DefaultAgentName),
		internalTestCap("cluster-inspection", "Read-only cluster inspection."),
	)

	rec := doReq(t, s.Handler(), http.MethodGet, "/internal/agents/li.ming/config", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	cfg := decode[resolver.ResolvedAgentConfig](t, rec)
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
	if cfg.ConfirmPolicy != v1alpha1.ConfirmPolicyConfirmWrites {
		t.Errorf("confirmPolicy = %q", cfg.ConfirmPolicy)
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
	cfg := decode[resolver.ResolvedAgentConfig](t, rec)
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
		internalTestCap("cluster-inspection", "Read-only cluster inspection."),
	)
	rec1 := doReq(t, s1.Handler(), http.MethodGet, "/internal/agents/li.ming/config", "", nil)
	cfg1 := decode[resolver.ResolvedAgentConfig](t, rec1)

	s2 := platformTestServer(t,
		internalTestAgent(v1alpha1.DefaultAgentName),
		internalTestInstance("li.ming", v1alpha1.DefaultAgentName),
		internalTestCap("cluster-inspection", "Read-only cluster inspection - added one more check."),
	)
	rec2 := doReq(t, s2.Handler(), http.MethodGet, "/internal/agents/li.ming/config", "", nil)
	cfg2 := decode[resolver.ResolvedAgentConfig](t, rec2)

	if cfg1.Revision == cfg2.Revision {
		t.Errorf("skill change should change revision: %q", cfg1.Revision)
	}
	if cfg1.Revision == "" {
		t.Error("empty revision")
	}
}

type configResponse struct {
	Config store.AgentConfig `json:"config"`
}

// TestAgentConfigModelOverride verifies a model switch on the Agent config page
// writes the selection to the caller's AgentInstance (selectedModel) so the
// next chat turn resolves the override (regression: switching LLM did not change
// the chat model), and that "Runtime Default" clears it back to the gateway
// primary.
func TestAgentConfigModelOverride(t *testing.T) {
	st, err := store.New(t.TempDir(), "deepseek-v4-flash")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	s := platformTestServerStore(t, st,
		internalTestAgent(v1alpha1.DefaultAgentName),
		internalTestInstance("li.ming", v1alpha1.DefaultAgentName),
	)

	// Switch to the template model -> the override is resolved for the next turn.
	rec := doReq(t, s.Handler(), http.MethodPut, "/api/agent/config", "li.ming",
		map[string]any{"config": map[string]any{"model": "deepseek-v4-flash"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("save status = %d, body = %s", rec.Code, rec.Body.String())
	}
	cfg := decode[resolver.ResolvedAgentConfig](t, doReq(t, s.Handler(), http.MethodGet, "/internal/agents/li.ming/config", "li.ming", nil))
	if cfg.SelectedModel != "deepseek-v4-flash/deepseek-v4-flash" {
		t.Errorf("selectedModel = %q, want deepseek-v4-flash/deepseek-v4-flash", cfg.SelectedModel)
	}

	// Back to "Runtime Default" -> the override is cleared (no header sent).
	rec = doReq(t, s.Handler(), http.MethodPut, "/api/agent/config", "li.ming",
		map[string]any{"config": map[string]any{"model": ""}})
	if rec.Code != http.StatusOK {
		t.Fatalf("clear status = %d, body = %s", rec.Code, rec.Body.String())
	}
	cfg = decode[resolver.ResolvedAgentConfig](t, doReq(t, s.Handler(), http.MethodGet, "/internal/agents/li.ming/config", "li.ming", nil))
	if cfg.SelectedModel != "" {
		t.Errorf("selectedModel = %q, want empty after Runtime Default", cfg.SelectedModel)
	}
}

// TestAgentConfigDefaultModelAligned verifies a fresh store seeds the portal's
// model selector with the operator-configured default LLM (a valid template
// model, not a stale hardcoded value), and that a saved "Runtime Default" is
// preserved instead of being masked back to a concrete model.
func TestAgentConfigDefaultModelAligned(t *testing.T) {
	st, err := store.New(t.TempDir(), "deepseek-v4-flash")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	s := platformTestServerStore(t, st)

	// Fresh store -> configured default, not a stale value.
	resp := decode[configResponse](t, doReq(t, s.Handler(), http.MethodGet, "/api/agent/config", "", nil))
	if resp.Config.Model != "deepseek-v4-flash" {
		t.Errorf("fresh model = %q, want deepseek-v4-flash", resp.Config.Model)
	}

	// Save "Runtime Default" -> stays empty so the portal shows Runtime Default.
	rec := doReq(t, s.Handler(), http.MethodPut, "/api/agent/config", "zhang.wei",
		map[string]any{"config": map[string]any{"model": ""}})
	if rec.Code != http.StatusOK {
		t.Fatalf("save status = %d, body = %s", rec.Code, rec.Body.String())
	}
	resp = decode[configResponse](t, doReq(t, s.Handler(), http.MethodGet, "/api/agent/config", "", nil))
	if resp.Config.Model != "" {
		t.Errorf("model = %q, want empty after Runtime Default save", resp.Config.Model)
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
	raw := []byte(`{"models":{"providers":{"deepseek-v4-pro-0813":{"api":"openai-completions"}}},"agents":{"defaults":{"model":{"primary":"deepseek-v4-flash-0731/deepseek-v4-flash-0731","models":{"deepseek-v4-flash-0731/deepseek-v4-flash-0731":{"alias":"deepseek-v4-flash-0731"}}}}}}`)
	s := platformTestServerStore(t, nil,
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: k8s.ConfigSecretName},
			Data:       map[string][]byte{"openclaw.json": raw, "gatewayToken": []byte("tok")},
		},
		// The template the instance references (must contain the selected model).
		&v1alpha1.AgentTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "agent-for-cloud"},
			Spec: v1alpha1.AgentTemplateSpec{
				DefaultModel: "deepseek-v4-flash-0731",
				Models: []v1alpha1.TemplateModelSpec{
					{Name: "deepseek-v4-flash-0731", Endpoint: "https://api.deepseek.com"},
					{Name: "deepseek-v4-pro-0813", Endpoint: "https://api.deepseek.com"},
				},
			},
		},
		// An instance with an explicit selectedModel (resolver looks it up by
		// name without a namespace).
		&v1alpha1.AgentInstance{
			ObjectMeta: metav1.ObjectMeta{Name: "li-ming-agent-for-cloud"},
			Spec: v1alpha1.AgentInstanceSpec{
				Owner:         "li.ming",
				TemplateRef:   "agent-for-cloud",
				SelectedModel: "deepseek-v4-pro-0813",
			},
		},
	)

	rec := doReq(t, s.Handler(), http.MethodGet, "/internal/gateway/config/li.ming", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"primary":"deepseek-v4-pro-0813/deepseek-v4-pro-0813"`) {
		t.Errorf("primary not overridden per user: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "deepseek-v4-flash-0731/deepseek-v4-flash-0731") {
		t.Errorf("allowlist must be preserved: %s", rec.Body.String())
	}
}
