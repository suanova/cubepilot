package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/config"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add to scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 to scheme: %v", err)
	}
	return scheme
}

// TestBuiltinAgentShape verifies the builtin agent-for-cloud definition
// (design §3.1: builtin: true, auto-instantiated per user, non-deletable;
// model[0] primary).
func TestBuiltinAgentShape(t *testing.T) {
	agent := BuiltinAgentTemplate()
	if agent.Name != "agent-for-cloud" {
		t.Errorf("name = %s, want agent-for-cloud", agent.Name)
	}
	if agent.Spec.Registry == nil || !agent.Spec.Registry.Builtin {
		t.Error("builtin flag missing")
	}
	if agent.Spec.DefaultModel != "deepseek-v4-flash" {
		t.Errorf("defaultModel = %q, want deepseek-v4-flash (design §3.1)", agent.Spec.DefaultModel)
	}
	if len(agent.Spec.Models) != 1 || agent.Spec.Models[0].Name != "deepseek-v4-flash" {
		t.Errorf("inline models = %v, want [deepseek-v4-flash]", agent.Spec.Models)
	}
	if agent.Spec.ConfirmPolicy != v1alpha1.ConfirmPolicyConfirmWrites {
		t.Errorf("confirmPolicy = %q, want ConfirmWrites (design §3.1)", agent.Spec.ConfirmPolicy)
	}
	if len(agent.Spec.Models) == 0 || agent.Spec.Models[0].Name == "" {
		t.Error("primary model missing")
	}
	if agent.Spec.Identity == nil || agent.Spec.Identity.Mode != v1alpha1.IdentityModeUser {
		t.Error("identity mode should default to user")
	}
	if len(agent.Spec.Skills) == 0 {
		t.Error("builtin agent should reference capabilities/skills")
	}
}

// TestInstanceNameFor verifies the instance key = user + agent (design §3.2),
// with
// DNS-1123 sanitization.
func TestInstanceNameFor(t *testing.T) {
	cases := []struct{ user, agent, want string }{
		{"zhang.wei", "agent-for-cloud", "zhang-wei-agent-for-cloud"},
		{"Zhang Wei", "agent-for-cloud", "zhang-wei-agent-for-cloud"},
		{"li.ming", "agent-for-cloud", "li-ming-agent-for-cloud"},
	}
	for _, c := range cases {
		if got := InstanceNameFor(c.user, c.agent); got != c.want {
			t.Errorf("InstanceNameFor(%q, %q) = %q, want %q", c.user, c.agent, got, c.want)
		}
	}
}

// TestBootstrapEnsure verifies the builtin bootstrap creates the Agent,
// Capabilities, TaskTemplate and per-user instances idempotently
// (design §3.1 / §5.3: the builtin agent is auto-instantiated per user).
func TestBootstrapEnsure(t *testing.T) {
	scheme := testScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &BuiltinBootstrapReconciler{
		Client: cl,
		Scheme: scheme,
		Cfg: config.Config{
			Users: []string{"zhang.wei", "li.ming"},
		},
	}
	if err := r.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	// Agent definition exists.
	var agent v1alpha1.AgentTemplate
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "agent-for-cloud"}, &agent); err != nil {
		t.Fatalf("agent-for-cloud not created: %v", err)
	}

	// Capabilities exist.
	var caps v1alpha1.CapabilityList
	if err := cl.List(context.Background(), &caps); err != nil {
		t.Fatalf("list capabilities: %v", err)
	}
	if len(caps.Items) != len(BuiltinCapabilities) {
		t.Errorf("capabilities = %d, want %d", len(caps.Items), len(BuiltinCapabilities))
	}

	// TaskTemplate exists.
	var tpl v1alpha1.TaskTemplate
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "daily-inspection"}, &tpl); err != nil {
		t.Fatalf("daily-inspection template not created: %v", err)
	}

	// Models are inlined in the template (design §3.3): the builtin template
	// carries the preset inline model entries.
	tmpl := v1alpha1.AgentTemplate{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "agent-for-cloud"}, &tmpl); err != nil {
		t.Fatalf("agent-for-cloud template not found: %v", err)
	}
	if len(tmpl.Spec.Models) != len(BuiltinModels()) {
		t.Errorf("inline models = %d, want %d", len(tmpl.Spec.Models), len(BuiltinModels()))
	}

	// Per-user instances exist (auto-instantiated per user).
	var insts v1alpha1.AgentInstanceList
	if err := cl.List(context.Background(), &insts); err != nil {
		t.Fatalf("list instances: %v", err)
	}
	if len(insts.Items) != 2 {
		t.Fatalf("instances = %d, want 2", len(insts.Items))
	}
	for _, inst := range insts.Items {
		if inst.Spec.TemplateRef != "agent-for-cloud" {
			t.Errorf("instance %s templateRef = %s", inst.Name, inst.Spec.TemplateRef)
		}
		if inst.Spec.Identity.Mode != v1alpha1.IdentityModeUser || inst.Spec.Identity.PrincipalRef.UserRef == "" {
			t.Errorf("instance %s identity not bound to user", inst.Name)
		}
	}

	// Idempotent: a second Ensure must not fail or duplicate.
	if err := r.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure #2: %v", err)
	}
	var insts2 v1alpha1.AgentInstanceList
	if err := cl.List(context.Background(), &insts2); err != nil {
		t.Fatal(err)
	}
	if len(insts2.Items) != 2 {
		t.Errorf("instances after re-ensure = %d, want 2 (idempotent)", len(insts2.Items))
	}
}

func objectKey(name string) types.NamespacedName {
	return types.NamespacedName{Name: name}
}
