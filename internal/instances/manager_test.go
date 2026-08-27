package instances

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/config"
	"github.com/suanova/cubepilot/internal/k8s"
)

func testManager(t *testing.T, objs ...client.Object) *Manager {
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
	return New(cl, config.Config{DefaultUser: "zhang.wei"})
}

func template(name string, models []v1alpha1.TemplateModelSpec) *v1alpha1.AgentTemplate {
	return &v1alpha1.AgentTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.AgentTemplateSpec{
			Models: models,
		},
	}
}

func instance(user, selected string) *v1alpha1.AgentInstance {
	return &v1alpha1.AgentInstance{
		ObjectMeta: metav1.ObjectMeta{Name: k8s.InstanceName(user, v1alpha1.DefaultAgentName)},
		Spec: v1alpha1.AgentInstanceSpec{
			TemplateRef:   v1alpha1.DefaultAgentName,
			Owner:         user,
			SelectedModel: selected,
		},
	}
}

// TestSelectedModelForResolvesDefault verifies that with no explicit
// instance selection there is no override: the runtime uses its configured
// primary (the deployer's provider config decides the default).
func TestSelectedModelForResolvesDefault(t *testing.T) {
	agent := &v1alpha1.AgentTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.DefaultAgentName},
		Spec: v1alpha1.AgentTemplateSpec{
			DefaultModel: "deepseek-v4-flash",
			Models: []v1alpha1.TemplateModelSpec{
				{Name: "deepseek-v4-flash", Endpoint: "https://api.deepseek.com"},
			},
		},
	}
	m := testManager(t, agent, instance("li.ming", ""))

	got, err := m.SelectedModelFor(context.Background(), "li.ming")
	if err != nil {
		t.Fatalf("SelectedModelFor: %v", err)
	}
	if got != "" {
		t.Errorf("model = %q, want empty (no override for default)", got)
	}
}

// TestSelectedModelForExplicit verifies an explicit instance selection wins
// over the agent default (design §3.2: selectedModel overrides).
func TestSelectedModelForExplicit(t *testing.T) {
	agent := &v1alpha1.AgentTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.DefaultAgentName},
		Spec: v1alpha1.AgentTemplateSpec{
			DefaultModel: "deepseek-v4-flash",
			Models: []v1alpha1.TemplateModelSpec{
				{Name: "deepseek-v4-flash", Endpoint: "https://api.deepseek.com"},
				{Name: "deepseek-chat", Endpoint: "https://api.deepseek.com"},
			},
		},
	}
	m := testManager(t, agent, instance("li.ming", "deepseek-chat"))

	got, err := m.SelectedModelFor(context.Background(), "li.ming")
	if err != nil {
		t.Fatalf("SelectedModelFor: %v", err)
	}
	if got != "deepseek-chat/deepseek-chat" {
		t.Errorf("model = %q, want deepseek-chat/deepseek-chat", got)
	}
}

// TestSelectedModelForOutsideAllowlist verifies fail-closed: a selection
// outside the agent's inline models is an error, never a silent fallback.
func TestSelectedModelForOutsideAllowlist(t *testing.T) {
	agent := &v1alpha1.AgentTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.DefaultAgentName},
		Spec: v1alpha1.AgentTemplateSpec{
			DefaultModel: "deepseek-v4-flash",
			Models: []v1alpha1.TemplateModelSpec{
				{Name: "deepseek-v4-flash", Endpoint: "https://api.deepseek.com"},
			},
		},
	}
	m := testManager(t, agent, instance("li.ming", "glm-5.2"))

	if _, err := m.SelectedModelFor(context.Background(), "li.ming"); err == nil {
		t.Error("selection outside inline models should fail (fail-closed)")
	}
}

// TestSelectedModelForNoConfig verifies no instance / no selection / no
// default -> empty (runtime default), which is not an error.
func TestSelectedModelForNoConfig(t *testing.T) {
	m := testManager(t)
	if _, err := m.SelectedModelFor(context.Background(), "nobody"); err != nil {
		t.Errorf("no instance should be empty, not error: %v", err)
	}
}
