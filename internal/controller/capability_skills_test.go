package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/config"
	"github.com/suanova/cubepilot/internal/k8s"
)

// TestRenderSkill verifies a domain Capability renders into a valid SKILL.md:
// name + description frontmatter, title H1, instructions body.
func TestRenderSkill(t *testing.T) {
	cap := &v1alpha1.Capability{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-inspection"},
		Spec: v1alpha1.CapabilitySpec{
			Type:        v1alpha1.CapabilityDomain,
			Title:       "集群智能巡检",
			Description: "按「查节点 → 查异常 Pod → 查事件 → 分级归因」巡检集群健康",
			Instructions: `对当前集群执行一次只读巡检：
1. 检查节点 Ready 与压力；
2. 查找异常 Pod；
按 P0/P1/P2 分级输出结构化报告。`,
			Uses: []string{"resource-manager", "kubectl-platform"},
		},
	}
	skill, err := RenderSkill(cap)
	if err != nil {
		t.Fatalf("RenderSkill: %v", err)
	}
	for _, want := range []string{
		"---\n",
		"name: cluster-inspection\n",
		"description: 按「查节点",
		"# 集群智能巡检\n",
		"1. 检查节点 Ready",
		"requires:",
		"      - resource-manager\n",
	} {
		if !strings.Contains(skill, want) {
			t.Errorf("rendered skill missing %q\n---\n%s", want, skill)
		}
	}
}

// TestCapabilitySkillsSync verifies the reconciler renders Capability CRDs
// into the ConfigMap and restarts agent Pods on change.
func TestCapabilitySkillsSync(t *testing.T) {
	scheme := testScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&v1alpha1.Capability{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-inspection"},
			Spec: v1alpha1.CapabilitySpec{
				Type:         v1alpha1.CapabilityDomain,
				Title:        "集群智能巡检",
				Description:  "巡检集群健康",
				Instructions: "只读巡检集群。",
				OwnerModule:  "platform",
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "agent-li-ming-agent-for-cloud",
				Namespace: "cubepilot",
				Labels:    map[string]string{k8s.AgentLabelApp: "true"},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "gateway", Image: "cubepilot-openclaw:local"}}},
		},
	).Build()

	r := &CapabilitySkillsReconciler{
		Client: cl,
		Cfg:    config.Config{Namespace: "cubepilot"},
	}
	if err := r.sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	// ConfigMap created with the rendered skill.
	var cm corev1.ConfigMap
	if err := cl.Get(context.Background(), types.NamespacedName{Name: k8s.SkillsConfigMapName, Namespace: "cubepilot"}, &cm); err != nil {
		t.Fatalf("get ConfigMap: %v", err)
	}
	skill, ok := cm.Data["cluster-inspection"]
	if !ok {
		t.Fatalf("ConfigMap missing key cluster-inspection: %v", cm.Data)
	}
	if !strings.Contains(skill, "name: cluster-inspection") {
		t.Errorf("rendered skill wrong: %s", skill)
	}

	// Agent Pod was deleted (restart to load skills).
	var pod corev1.Pod
	err := cl.Get(context.Background(), types.NamespacedName{Name: "agent-li-ming-agent-for-cloud", Namespace: "cubepilot"}, &pod)
	if !apierrors.IsNotFound(err) {
		t.Errorf("agent pod should be deleted for restart, got err=%v", err)
	}

	// Idempotent: second sync with same capabilities must not churn.
	if err := r.sync(context.Background()); err != nil {
		t.Fatalf("sync #2: %v", err)
	}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "agent-li-ming-agent-for-cloud", Namespace: "cubepilot"}, &pod); !apierrors.IsNotFound(err) {
		t.Errorf("idempotent sync should not delete pod again (recreate happened only via controller), got err=%v", err)
	}
}

// TestSkillSourceParsing verifies the embedded SKILL.md files parse and
// generate the builtin capability definitions (single source of truth).
func TestSkillSourceParsing(t *testing.T) {
	names, err := presetCapabilityNames()
	if err != nil {
		t.Fatalf("presetCapabilityNames: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("no embedded capabilities")
	}
	caps, err := BuiltinCapabilityDefinitions()
	if err != nil {
		t.Fatalf("BuiltinCapabilityDefinitions: %v", err)
	}
	if len(caps) != len(names) {
		t.Errorf("capabilities = %d, want %d (names: %v)", len(caps), len(names), names)
	}
	for _, cap := range caps {
		if cap.Name == "" {
			t.Error("capability with empty name")
		}
		if cap.Spec.Type != v1alpha1.CapabilityDomain {
			t.Errorf("%s: type = %s, want domain", cap.Name, cap.Spec.Type)
		}
		if cap.Spec.Instructions == "" {
			t.Errorf("%s: empty instructions", cap.Name)
		}
		if cap.Spec.Title == "" {
			t.Errorf("%s: empty title (no H1 in SKILL.md?)", cap.Name)
		}
	}
	// The builtin agent references exactly the preset names.
	if len(BuiltinCapabilities) != len(names) {
		t.Errorf("BuiltinCapabilities = %v, want %v", BuiltinCapabilities, names)
	}
	for _, n := range names {
		found := false
		for _, c := range BuiltinCapabilities {
			if c == n {
				found = true
			}
		}
		if !found {
			t.Errorf("BuiltinCapabilities missing %s", n)
		}
	}
}
