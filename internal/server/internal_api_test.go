package server

import (
	"net/http"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/k8s"
	"github.com/suanova/cubepilot/internal/resolver"
)

func internalTestAgent(name string) *v1alpha1.Agent {
	return &v1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.AgentSpec{
			DefaultModel:    "deepseek-v4-flash",
			AvailableModels: []string{"deepseek-v4-flash"},
			ConfirmPolicy:   v1alpha1.ConfirmPolicyConfirmWrites,
			Instructions:    "你是平台助手。",
		},
	}
}

func internalTestInstance(user, agentRef string) *v1alpha1.AgentInstance {
	return &v1alpha1.AgentInstance{
		ObjectMeta: metav1.ObjectMeta{Name: k8s.InstanceName(user, agentRef)},
		Spec: v1alpha1.AgentInstanceSpec{
			AgentRef: agentRef,
			Owner:    user,
		},
	}
}

func internalTestModel(name, modelID string) *v1alpha1.Model {
	return &v1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.ModelSpec{Provider: v1alpha1.ModelProviderPlatform, ModelID: modelID},
		Status:     v1alpha1.ModelStatus{Phase: v1alpha1.ModelAvailable},
	}
}

func internalTestCap(name, instructions string) *v1alpha1.Capability {
	return &v1alpha1.Capability{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.CapabilitySpec{
			Type:         v1alpha1.CapabilityDomain,
			Title:        "能力 " + name,
			Description:  "描述 " + name,
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
		internalTestModel("deepseek-v4-flash", "deepseek/deepseek-v4-flash"),
		internalTestCap("cluster-inspection", "只读巡检集群。"),
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
	if cfg.SelectedModel != "deepseek/deepseek-v4-flash" {
		t.Errorf("selectedModel = %q", cfg.SelectedModel)
	}
	if cfg.ConfirmPolicy != v1alpha1.ConfirmPolicyConfirmWrites {
		t.Errorf("confirmPolicy = %q", cfg.ConfirmPolicy)
	}
	if len(cfg.Capabilities) != 1 || cfg.Capabilities[0].Name != "cluster-inspection" {
		t.Errorf("capabilities = %+v", cfg.Capabilities)
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
// fingerprint: a capability content change produces a different revision.
func TestInternalAgentConfigRevisionChanges(t *testing.T) {
	s1 := platformTestServer(t,
		internalTestAgent(v1alpha1.DefaultAgentName),
		internalTestInstance("li.ming", v1alpha1.DefaultAgentName),
		internalTestModel("deepseek-v4-flash", "deepseek/deepseek-v4-flash"),
		internalTestCap("cluster-inspection", "只读巡检集群。"),
	)
	rec1 := doReq(t, s1.Handler(), http.MethodGet, "/internal/agents/li.ming/config", "", nil)
	cfg1 := decode[resolver.ResolvedAgentConfig](t, rec1)

	s2 := platformTestServer(t,
		internalTestAgent(v1alpha1.DefaultAgentName),
		internalTestInstance("li.ming", v1alpha1.DefaultAgentName),
		internalTestModel("deepseek-v4-flash", "deepseek/deepseek-v4-flash"),
		internalTestCap("cluster-inspection", "只读巡检集群——新增检查。"),
	)
	rec2 := doReq(t, s2.Handler(), http.MethodGet, "/internal/agents/li.ming/config", "", nil)
	cfg2 := decode[resolver.ResolvedAgentConfig](t, rec2)

	if cfg1.Revision == cfg2.Revision {
		t.Errorf("capability change should change revision: %q", cfg1.Revision)
	}
	if cfg1.Revision == "" {
		t.Error("empty revision")
	}
}
