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

func skillWithSource(name, path string) *v1alpha1.Skill {
	return &v1alpha1.Skill{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.SkillSpec{
			DisplayName: "Skill " + name,
			Description: "Description " + name,
			Visibility:  v1alpha1.SkillVisibilityPlatform,
			Source:      v1alpha1.SkillSource{Type: v1alpha1.SkillSourcePath, Path: path, Sha256: "sha-" + name},
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

// TestResolveMergesFields verifies agent definition fields and skills (as
// content references) land in the resolved config.
func TestResolveMergesFields(t *testing.T) {
	r := testResolver(t,
		template(v1alpha1.DefaultAgentName, func(a *v1alpha1.AgentTemplate) {
			a.Spec.DefaultModel = "deepseek-v4-flash"
			a.Spec.Models = []v1alpha1.TemplateModelSpec{
				{Name: "deepseek-v4-flash", Endpoint: "https://api.deepseek.com"},
			}
		}),
		instance("li.ming", v1alpha1.DefaultAgentName, ""),
		skillWithSource("cluster-inspection", "skills/cluster-inspection/v1.tar.gz"),
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
	if len(cfg.Skills) != 1 {
		t.Fatalf("skills = %d, want 1", len(cfg.Skills))
	}
	cap := cfg.Skills[0]
	if cap.Name != "cluster-inspection" || cap.Revision == "" {
		t.Errorf("skill = %+v", cap)
	}
	if cap.Path != "skills/cluster-inspection/v1.tar.gz" {
		t.Errorf("skill path = %q, want skills/cluster-inspection/v1.tar.gz", cap.Path)
	}
	if cap.Sha256 != "sha-cluster-inspection" {
		t.Errorf("skill sha256 = %q, want sha-cluster-inspection", cap.Sha256)
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
				{Name: "deepseek-v4-flash", Endpoint: "https://api.deepseek.com"},
				{Name: "deepseek-chat", Endpoint: "https://api.deepseek.com"},
			}
		}),
		instance("li.ming", v1alpha1.DefaultAgentName, "deepseek-chat"),
	)
	cfg, err := r.ResolveForUser(context.Background(), "li.ming")
	if err != nil {
		t.Fatalf("ResolveForUser: %v", err)
	}
	if cfg.SelectedModel != "deepseek-chat/deepseek-chat" {
		t.Errorf("selectedModel = %q, want deepseek-chat/deepseek-chat", cfg.SelectedModel)
	}
}

// TestResolveOutsideAllowlist verifies fail-closed: a selection outside the
// template's inline models is an error.
func TestResolveOutsideAllowlist(t *testing.T) {
	r := testResolver(t,
		template(v1alpha1.DefaultAgentName, func(a *v1alpha1.AgentTemplate) {
			a.Spec.DefaultModel = "deepseek-v4-flash"
			a.Spec.Models = []v1alpha1.TemplateModelSpec{
				{Name: "deepseek-v4-flash", Endpoint: "https://api.deepseek.com"},
			}
		}),
		instance("li.ming", v1alpha1.DefaultAgentName, "glm-5.2"),
	)
	if _, err := r.ResolveForUser(context.Background(), "li.ming"); err == nil {
		t.Error("selection outside inline models should fail (fail-closed)")
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
		skillWithSource("cluster-inspection", "skills/cluster-inspection/v1.tar.gz"),
		skillWithSource("kubectl-platform", "skills/kubectl-platform/v1.tar.gz"),
	)
	cfg, err := r.ResolveForUser(context.Background(), "li.ming")
	if err != nil {
		t.Fatalf("ResolveForUser: %v", err)
	}
	if len(cfg.Skills) != 1 || cfg.Skills[0].Name != "cluster-inspection" {
		t.Errorf("skills = %+v, want only cluster-inspection", cfg.Skills)
	}
}

// TestResolveRevisionStable verifies the revision is a content hash: equal
// inputs -> equal revision; a skill change -> different revision.
func TestResolveRevisionStable(t *testing.T) {
	base := []client.Object{
		instance("li.ming", v1alpha1.DefaultAgentName, ""),
		skillWithSource("cluster-inspection", "skills/cluster-inspection/v1.tar.gz"),
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

	changed := skillWithSource("cluster-inspection", "skills/cluster-inspection/v2.tar.gz")
	r3 := testResolver(t, instance("li.ming", v1alpha1.DefaultAgentName, ""), changed)
	cfg3, err := r3.ResolveForUser(context.Background(), "li.ming")
	if err != nil {
		t.Fatalf("resolve #3: %v", err)
	}
	if cfg1.Revision == cfg3.Revision {
		t.Errorf("skill change should change revision: %q", cfg1.Revision)
	}
}

// TestResolveNoModelOverride verifies that with no explicit selection there is
// no override: the gateway runs its configured primary.
func TestResolveNoModelOverride(t *testing.T) {
	r := testResolver(t,
		template(v1alpha1.DefaultAgentName, func(a *v1alpha1.AgentTemplate) {
			a.Spec.DefaultModel = "builtin-default"
			a.Spec.Models = []v1alpha1.TemplateModelSpec{
				{Name: "builtin-default", Endpoint: "https://api.deepseek.com"},
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

// TestResolveUserInstructionsComposition verifies the final system prompt is
// composed in order (design §3.2 / issue #17): the template instructions come
// first, then the instance's user instructions are appended with a blank line
// separator.
func TestResolveUserInstructionsComposition(t *testing.T) {
	inst := instance("li.ming", v1alpha1.DefaultAgentName, "")
	inst.Spec.UserInstructions = "answer in Chinese, be concise"
	r := testResolver(t,
		template(v1alpha1.DefaultAgentName, nil),
		inst,
	)
	cfg, err := r.ResolveForUser(context.Background(), "li.ming")
	if err != nil {
		t.Fatalf("ResolveForUser: %v", err)
	}
	want := "You are the platform assistant.\n\nanswer in Chinese, be concise"
	if cfg.Instructions != want {
		t.Errorf("instructions = %q, want %q (template first, user appended)", cfg.Instructions, want)
	}
}

// TestResolveUserInstructionsOnly verifies that a user instruction without any
// template instructions is used verbatim (no stray leading separator), and an
// empty user instruction leaves the template prompt untouched.
func TestResolveUserInstructionsOnly(t *testing.T) {
	// No template instructions: user text is used verbatim (no leading "\n\n").
	inst := instance("li.ming", v1alpha1.DefaultAgentName, "")
	inst.Spec.UserInstructions = "only user text"
	r := testResolver(t,
		template(v1alpha1.DefaultAgentName, func(a *v1alpha1.AgentTemplate) {
			a.Spec.Instructions = ""
		}),
		inst,
	)
	cfg, err := r.ResolveForUser(context.Background(), "li.ming")
	if err != nil {
		t.Fatalf("ResolveForUser: %v", err)
	}
	if cfg.Instructions != "only user text" {
		t.Errorf("instructions = %q, want %q (no template prompt)", cfg.Instructions, "only user text")
	}

	// Empty user instructions: the template prompt is preserved untouched.
	inst2 := instance("zhang.wei", v1alpha1.DefaultAgentName, "")
	r2 := testResolver(t,
		template(v1alpha1.DefaultAgentName, nil),
		inst2,
	)
	cfg2, err := r2.ResolveForUser(context.Background(), "zhang.wei")
	if err != nil {
		t.Fatalf("ResolveForUser: %v", err)
	}
	if cfg2.Instructions != "You are the platform assistant." {
		t.Errorf("instructions = %q, want the untouched template prompt", cfg2.Instructions)
	}
}
