package resolver

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/k8s"
)

func testResolver(t *testing.T, objs ...client.Object) *Resolver {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.Model{}, &v1alpha1.AgentInstance{}, &v1alpha1.Agent{}).
		WithObjects(objs...).
		Build()
	return New(cl)
}

func agent(name string, mod func(*v1alpha1.Agent)) *v1alpha1.Agent {
	a := &v1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.AgentSpec{
			ConfirmPolicy: v1alpha1.ConfirmPolicyConfirmWrites,
			Instructions:  "你是平台助手。",
		},
	}
	if mod != nil {
		mod(a)
	}
	return a
}

func instance(user, agentRef, selected string) *v1alpha1.AgentInstance {
	return &v1alpha1.AgentInstance{
		ObjectMeta: metav1.ObjectMeta{Name: k8s.InstanceName(user, agentRef)},
		Spec: v1alpha1.AgentInstanceSpec{
			AgentRef:      agentRef,
			Owner:         user,
			SelectedModel: selected,
		},
	}
}

func model(name string, phase v1alpha1.ModelPhase, modelID string) *v1alpha1.Model {
	return &v1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.ModelSpec{Provider: v1alpha1.ModelProviderPlatform, ModelID: modelID},
		Status:     v1alpha1.ModelStatus{Phase: phase},
	}
}

func domainCap(name, instructions string) *v1alpha1.Capability {
	return &v1alpha1.Capability{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.CapabilitySpec{
			Type:         v1alpha1.CapabilityDomain,
			Title:        "能力 " + name,
			Description:  "描述 " + name,
			Instructions: instructions,
			Uses:         []string{"kubectl-platform"},
		},
	}
}

// TestResolveNoInstance verifies a missing instance resolves to an empty
// config (runtime default), not an error.
func TestResolveNoInstance(t *testing.T) {
	r := testResolver(t)
	cfg, err := r.ResolveForUser(context.Background(), "nobody")
	if err != nil {
		t.Fatalf("ResolveForUser: %v", err)
	}
	if !cfg.Empty() {
		t.Errorf("expected empty config, got %+v", cfg)
	}
}

// TestResolveMergesFields verifies agent definition fields and domain
// capabilities land in the resolved config.
func TestResolveMergesFields(t *testing.T) {
	r := testResolver(t,
		agent(v1alpha1.DefaultAgentName, func(a *v1alpha1.Agent) {
			a.Spec.DefaultModel = "deepseek-v4-flash"
			a.Spec.AvailableModels = []string{"deepseek-v4-flash"}
		}),
		instance("li.ming", v1alpha1.DefaultAgentName, ""),
		model("deepseek-v4-flash", v1alpha1.ModelAvailable, "deepseek/deepseek-v4-flash"),
		domainCap("cluster-inspection", "只读巡检集群。"),
		&v1alpha1.Capability{ // atomic — must NOT appear in capabilities
			ObjectMeta: metav1.ObjectMeta{Name: "some-atomic"},
			Spec:       v1alpha1.CapabilitySpec{Type: v1alpha1.CapabilityAtomic},
		},
	)

	cfg, err := r.ResolveForUser(context.Background(), "li.ming")
	if err != nil {
		t.Fatalf("ResolveForUser: %v", err)
	}
	if cfg.Instance != k8s.InstanceName("li.ming", v1alpha1.DefaultAgentName) {
		t.Errorf("instance = %q", cfg.Instance)
	}
	if cfg.Owner != "li.ming" {
		t.Errorf("owner = %q", cfg.Owner)
	}
	if cfg.SelectedModel != "deepseek/deepseek-v4-flash" {
		t.Errorf("selectedModel = %q, want deepseek/deepseek-v4-flash", cfg.SelectedModel)
	}
	if cfg.ModelName != "deepseek-v4-flash" {
		t.Errorf("modelName = %q", cfg.ModelName)
	}
	if cfg.ConfirmPolicy != v1alpha1.ConfirmPolicyConfirmWrites {
		t.Errorf("confirmPolicy = %q", cfg.ConfirmPolicy)
	}
	if len(cfg.Capabilities) != 1 {
		t.Fatalf("capabilities = %d, want 1 (atomic excluded)", len(cfg.Capabilities))
	}
	cap := cfg.Capabilities[0]
	if cap.Name != "cluster-inspection" || cap.Revision == "" {
		t.Errorf("capability = %+v", cap)
	}
	if cfg.Revision == "" {
		t.Error("empty revision")
	}
}

// TestResolveExplicitSelection verifies the instance override wins over the
// agent default.
func TestResolveExplicitSelection(t *testing.T) {
	r := testResolver(t,
		agent(v1alpha1.DefaultAgentName, func(a *v1alpha1.Agent) {
			a.Spec.DefaultModel = "deepseek-v4-flash"
			a.Spec.AvailableModels = []string{"deepseek-v4-flash", "deepseek-chat"}
		}),
		instance("li.ming", v1alpha1.DefaultAgentName, "deepseek-chat"),
		model("deepseek-v4-flash", v1alpha1.ModelAvailable, "deepseek/deepseek-v4-flash"),
		model("deepseek-chat", v1alpha1.ModelAvailable, "deepseek/deepseek-chat"),
	)
	cfg, err := r.ResolveForUser(context.Background(), "li.ming")
	if err != nil {
		t.Fatalf("ResolveForUser: %v", err)
	}
	if cfg.SelectedModel != "deepseek/deepseek-chat" {
		t.Errorf("selectedModel = %q, want deepseek/deepseek-chat", cfg.SelectedModel)
	}
}

// TestResolveOutsideAllowlist verifies fail-closed: a selection outside the
// agent's availableModels is an error.
func TestResolveOutsideAllowlist(t *testing.T) {
	r := testResolver(t,
		agent(v1alpha1.DefaultAgentName, func(a *v1alpha1.Agent) {
			a.Spec.DefaultModel = "deepseek-v4-flash"
			a.Spec.AvailableModels = []string{"deepseek-v4-flash"}
		}),
		instance("li.ming", v1alpha1.DefaultAgentName, "glm-5.2"),
		model("glm-5.2", v1alpha1.ModelAvailable, "glm/glm-5.2"),
	)
	if _, err := r.ResolveForUser(context.Background(), "li.ming"); err == nil {
		t.Error("selection outside availableModels should fail (fail-closed)")
	}
}

// TestResolveUnreachableModel verifies fail-closed on an Unreachable model.
func TestResolveUnreachableModel(t *testing.T) {
	r := testResolver(t,
		agent(v1alpha1.DefaultAgentName, func(a *v1alpha1.Agent) {
			a.Spec.DefaultModel = "deepseek-v4-flash"
			a.Spec.AvailableModels = []string{"deepseek-v4-flash"}
		}),
		instance("li.ming", v1alpha1.DefaultAgentName, ""),
		model("deepseek-v4-flash", v1alpha1.ModelUnreachable, "deepseek/deepseek-v4-flash"),
	)
	if _, err := r.ResolveForUser(context.Background(), "li.ming"); err == nil {
		t.Error("Unreachable model should fail (fail-closed)")
	}
}

// TestResolveCapabilityScopedByAgents verifies capability.Agents restricts
// visibility.
func TestResolveCapabilityScopedByAgents(t *testing.T) {
	scoped := domainCap("scoped", "只给特定 agent")
	scoped.Spec.Agents = []string{"other-agent"}
	r := testResolver(t,
		instance("li.ming", v1alpha1.DefaultAgentName, ""),
		domainCap("open", "所有人可见"),
		scoped,
	)
	cfg, err := r.ResolveForUser(context.Background(), "li.ming")
	if err != nil {
		t.Fatalf("ResolveForUser: %v", err)
	}
	if len(cfg.Capabilities) != 1 || cfg.Capabilities[0].Name != "open" {
		t.Errorf("capabilities = %+v, want only open", cfg.Capabilities)
	}
}

// TestResolveRevisionStable verifies the revision is a content hash: equal
// inputs → equal revision; a capability change → different revision.
func TestResolveRevisionStable(t *testing.T) {
	base := []client.Object{
		instance("li.ming", v1alpha1.DefaultAgentName, ""),
		domainCap("cluster-inspection", "只读巡检集群。"),
	}
	r1 := testResolver(t, base...)
	cfg1, err := r1.ResolveForUser(context.Background(), "li.ming")
	if err != nil {
		t.Fatalf("resolve #1: %v", err)
	}

	r2 := testResolver(t, base...)
	cfg2, err := r2.ResolveForUser(context.Background(), "li.ming")
	if err != nil {
		t.Fatalf("resolve #2: %v", err)
	}
	if cfg1.Revision != cfg2.Revision {
		t.Errorf("revision not stable: %q vs %q", cfg1.Revision, cfg2.Revision)
	}

	changed := domainCap("cluster-inspection", "只读巡检集群——新增一条检查。")
	r3 := testResolver(t, instance("li.ming", v1alpha1.DefaultAgentName, ""), changed)
	cfg3, err := r3.ResolveForUser(context.Background(), "li.ming")
	if err != nil {
		t.Fatalf("resolve #3: %v", err)
	}
	if cfg1.Revision == cfg3.Revision {
		t.Errorf("capability change should change revision: %q", cfg1.Revision)
	}
}

// TestResolveNoModelOverride verifies a selection whose Model has no modelId
// (platform default) resolves to empty SelectedModel, not an error.
func TestResolveNoModelOverride(t *testing.T) {
	r := testResolver(t,
		agent(v1alpha1.DefaultAgentName, func(a *v1alpha1.Agent) {
			a.Spec.DefaultModel = "builtin-default"
			a.Spec.AvailableModels = []string{"builtin-default"}
		}),
		instance("li.ming", v1alpha1.DefaultAgentName, ""),
		model("builtin-default", v1alpha1.ModelAvailable, ""),
	)
	cfg, err := r.ResolveForUser(context.Background(), "li.ming")
	if err != nil {
		t.Fatalf("ResolveForUser: %v", err)
	}
	if cfg.SelectedModel != "" || cfg.ModelName != "builtin-default" {
		t.Errorf("selectedModel = %q modelName = %q, want empty override with name", cfg.SelectedModel, cfg.ModelName)
	}
}
