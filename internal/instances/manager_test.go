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
		WithStatusSubresource(&v1alpha1.Model{}, &v1alpha1.AgentInstance{}, &v1alpha1.Agent{}).
		WithObjects(objs...).
		Build()
	return New(cl, config.Config{DefaultUser: "zhang.wei"})
}

func model(name string, phase v1alpha1.ModelPhase, modelID string) *v1alpha1.Model {
	return &v1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.ModelSpec{Provider: v1alpha1.ModelProviderPlatform, ModelID: modelID},
		Status:     v1alpha1.ModelStatus{Phase: phase},
	}
}

func instance(user, selected string) *v1alpha1.AgentInstance {
	return &v1alpha1.AgentInstance{
		ObjectMeta: metav1.ObjectMeta{Name: k8s.InstanceName(user, v1alpha1.DefaultAgentName)},
		Spec: v1alpha1.AgentInstanceSpec{
			AgentRef:      v1alpha1.DefaultAgentName,
			Owner:         user,
			SelectedModel: selected,
		},
	}
}

// TestSelectedModelForResolvesDefault verifies the agent definition's
// defaultModel applies when the instance has no explicit selection
// (design §3.1: defaultModel → catalog → modelId).
func TestSelectedModelForResolvesDefault(t *testing.T) {
	agent := &v1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.DefaultAgentName},
		Spec: v1alpha1.AgentSpec{
			DefaultModel:    "deepseek-v4-flash",
			AvailableModels: []string{"deepseek-v4-flash"},
		},
	}
	m := testManager(t, agent, instance("li.ming", ""), model("deepseek-v4-flash", v1alpha1.ModelAvailable, "deepseek/deepseek-v4-flash"))

	got, err := m.SelectedModelFor(context.Background(), "li.ming")
	if err != nil {
		t.Fatalf("SelectedModelFor: %v", err)
	}
	if got != "deepseek/deepseek-v4-flash" {
		t.Errorf("model = %q, want deepseek/deepseek-v4-flash", got)
	}
}

// TestSelectedModelForExplicit verifies an explicit instance selection wins
// over the agent default (design §3.2: selectedModel overrides).
func TestSelectedModelForExplicit(t *testing.T) {
	agent := &v1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.DefaultAgentName},
		Spec: v1alpha1.AgentSpec{
			DefaultModel:    "deepseek-v4-flash",
			AvailableModels: []string{"deepseek-v4-flash", "deepseek-chat"},
		},
	}
	m := testManager(t, agent, instance("li.ming", "deepseek-chat"),
		model("deepseek-v4-flash", v1alpha1.ModelAvailable, "deepseek/deepseek-v4-flash"),
		model("deepseek-chat", v1alpha1.ModelAvailable, "deepseek/deepseek-chat"))

	got, err := m.SelectedModelFor(context.Background(), "li.ming")
	if err != nil {
		t.Fatalf("SelectedModelFor: %v", err)
	}
	if got != "deepseek/deepseek-chat" {
		t.Errorf("model = %q, want deepseek/deepseek-chat", got)
	}
}

// TestSelectedModelForOutsideAllowlist verifies fail-closed: a selection
// outside the agent's availableModels is an error, never a silent fallback.
func TestSelectedModelForOutsideAllowlist(t *testing.T) {
	agent := &v1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.DefaultAgentName},
		Spec: v1alpha1.AgentSpec{
			DefaultModel:    "deepseek-v4-flash",
			AvailableModels: []string{"deepseek-v4-flash"},
		},
	}
	m := testManager(t, agent, instance("li.ming", "glm-5.2"),
		model("deepseek-v4-flash", v1alpha1.ModelAvailable, "deepseek/deepseek-v4-flash"),
		model("glm-5.2", v1alpha1.ModelAvailable, "glm/glm-5.2"))

	if _, err := m.SelectedModelFor(context.Background(), "li.ming"); err == nil {
		t.Error("selection outside availableModels should fail (fail-closed)")
	}
}

// TestSelectedModelForUnreachable verifies fail-closed: an Unreachable model
// is an error even when it is the agent default.
func TestSelectedModelForUnreachable(t *testing.T) {
	agent := &v1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.DefaultAgentName},
		Spec:       v1alpha1.AgentSpec{DefaultModel: "deepseek-v4-flash", AvailableModels: []string{"deepseek-v4-flash"}},
	}
	m := testManager(t, agent, instance("li.ming", ""),
		model("deepseek-v4-flash", v1alpha1.ModelUnreachable, "deepseek/deepseek-v4-flash"))

	if _, err := m.SelectedModelFor(context.Background(), "li.ming"); err == nil {
		t.Error("Unreachable model should fail (fail-closed)")
	}
}

// TestSelectedModelForNoConfig verifies no instance / no selection / no
// default → empty (runtime default), which is not an error.
func TestSelectedModelForNoConfig(t *testing.T) {
	m := testManager(t)
	if _, err := m.SelectedModelFor(context.Background(), "nobody"); err != nil {
		t.Errorf("no instance should be empty, not error: %v", err)
	}
}
