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
		WithStatusSubresource(&v1alpha1.AgentInstance{}, &v1alpha1.AgentTemplate{}).
		WithObjects(objs...).
		Build()
	return New(cl)
}

func template(name string, mod func(*v1alpha1.AgentTemplate)) *v1alpha1.AgentTemplate {
	t := &v1alpha1.AgentTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.AgentTemplateSpec{
			ConfirmPolicy: v1alpha1.ConfirmPolicyConfirmWrites,
			Instructions:  "You are the platform assistant.",
		},
	}
	if mod != nil {
		mod(t)
	}
	return t
}

func instance(user, templateRef, selected string) *v1alpha1.AgentInstance {
	return &v1alpha1.AgentInstance{
		ObjectMeta: metav1.ObjectMeta{Name: k8s.InstanceName(user, templateRef)},
		Spec: v1alpha1.AgentInstanceSpec{
			TemplateRef:   templateRef,
			Owner:         user,
			SelectedModel: selected,
		},
	}
}

func domainCap(name, instructions string) *v1alpha1.Capability {
	return &v1alpha1.Capability{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.CapabilitySpec{
			Type:         v1alpha1.CapabilityDomain,
			Title:        "Capability " + name,
			Description:  "Description " + name,
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
		template(v1alpha1.DefaultAgentName, func(a *v1alpha1.AgentTemplate) {
			a.Spec.DefaultModel = "deepseek-v4-flash"
			a.Spec.Models = []v1alpha1.TemplateModelSpec{
				{Name: "deepseek-v4-flash", Provider: v1alpha1.ModelProviderPlatform, ModelID: "cuberouter/deepseek-v4-flash-0731"},
			}
		}),
		instance("li.ming", v1alpha1.DefaultAgentName, ""),
		domainCap("cluster-inspection", "Read-only cluster inspection."),
		&v1alpha1.Capability{ // atomic -- must NOT appear in capabilities
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
	// No explicit selection -> no override; the template default is the
	// display name only (the runtime's configured primary decides the model).
	if cfg.SelectedModel != "" {
		t.Errorf("selectedModel = %q, want empty (no override for default)", cfg.SelectedModel)
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
		template(v1alpha1.DefaultAgentName, func(a *v1alpha1.AgentTemplate) {
			a.Spec.DefaultModel = "deepseek-v4-flash"
			a.Spec.Models = []v1alpha1.TemplateModelSpec{
				{Name: "deepseek-v4-flash", Provider: v1alpha1.ModelProviderPlatform, ModelID: "cuberouter/deepseek-v4-flash-0731"},
				{Name: "deepseek-chat", Provider: v1alpha1.ModelProviderPlatform, ModelID: "deepseek/deepseek-chat"},
			}
		}),
		instance("li.ming", v1alpha1.DefaultAgentName, "deepseek-chat"),
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
// template's inline models is an error.
func TestResolveOutsideAllowlist(t *testing.T) {
	r := testResolver(t,
		template(v1alpha1.DefaultAgentName, func(a *v1alpha1.AgentTemplate) {
			a.Spec.DefaultModel = "deepseek-v4-flash"
			a.Spec.Models = []v1alpha1.TemplateModelSpec{
				{Name: "deepseek-v4-flash", Provider: v1alpha1.ModelProviderPlatform, ModelID: "cuberouter/deepseek-v4-flash-0731"},
			}
		}),
		instance("li.ming", v1alpha1.DefaultAgentName, "glm-5.2"),
	)
	if _, err := r.ResolveForUser(context.Background(), "li.ming"); err == nil {
		t.Error("selection outside inline models should fail (fail-closed)")
	}
}

// TestResolveCapabilityScopedByAgents verifies capability.Agents restricts
// visibility.
func TestResolveCapabilityScopedByAgents(t *testing.T) {
	scoped := domainCap("scoped", "Visible only to a specific agent.")
	scoped.Spec.Agents = []string{"other-agent"}
	r := testResolver(t,
		instance("li.ming", v1alpha1.DefaultAgentName, ""),
		domainCap("open", "Visible to everyone."),
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

// TestResolveEnabledSkills verifies the instance-level skill subset
// (design §3.2: the instance may restrict to enabledSkills; empty = all
// visible).
func TestResolveEnabledSkills(t *testing.T) {
	inst := instance("li.ming", v1alpha1.DefaultAgentName, "")
	inst.Spec.EnabledSkills = []string{"cluster-inspection"}
	r := testResolver(t,
		inst,
		domainCap("cluster-inspection", "Read-only cluster inspection."),
		domainCap("dev-environment", "Development environment management."),
	)
	cfg, err := r.ResolveForUser(context.Background(), "li.ming")
	if err != nil {
		t.Fatalf("ResolveForUser: %v", err)
	}
	if len(cfg.Capabilities) != 1 || cfg.Capabilities[0].Name != "cluster-inspection" {
		t.Errorf("capabilities = %+v, want only cluster-inspection", cfg.Capabilities)
	}
}

// TestResolveRevisionStable verifies the revision is a content hash: equal
// inputs -> equal revision; a capability change -> different revision.
func TestResolveRevisionStable(t *testing.T) {
	base := []client.Object{
		instance("li.ming", v1alpha1.DefaultAgentName, ""),
		domainCap("cluster-inspection", "Read-only cluster inspection."),
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

	changed := domainCap("cluster-inspection", "Read-only cluster inspection - added one more check.")
	r3 := testResolver(t, instance("li.ming", v1alpha1.DefaultAgentName, ""), changed)
	cfg3, err := r3.ResolveForUser(context.Background(), "li.ming")
	if err != nil {
		t.Fatalf("resolve #3: %v", err)
	}
	if cfg1.Revision == cfg3.Revision {
		t.Errorf("capability change should change revision: %q", cfg1.Revision)
	}
}

// TestResolveNoModelOverride verifies a selection whose model has no modelId
// (platform default) resolves to empty SelectedModel, not an error.
func TestResolveNoModelOverride(t *testing.T) {
	r := testResolver(t,
		template(v1alpha1.DefaultAgentName, func(a *v1alpha1.AgentTemplate) {
			a.Spec.DefaultModel = "builtin-default"
			a.Spec.Models = []v1alpha1.TemplateModelSpec{
				{Name: "builtin-default", Provider: v1alpha1.ModelProviderPlatform, ModelID: ""},
			}
		}),
		instance("li.ming", v1alpha1.DefaultAgentName, ""),
	)
	cfg, err := r.ResolveForUser(context.Background(), "li.ming")
	if err != nil {
		t.Fatalf("ResolveForUser: %v", err)
	}
	if cfg.SelectedModel != "" || cfg.ModelName != "builtin-default" {
		t.Errorf("selectedModel = %q modelName = %q, want empty override with name", cfg.SelectedModel, cfg.ModelName)
	}
}
