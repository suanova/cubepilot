package capability

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
)

// TestToolSetForAgent verifies the effective tool set computation
// (design §3.3.1: generic tools are available by default; Agent.tools[] only
// references Capabilities; Capability.agents[] decides the visible subset).
func TestToolSetForAgent(t *testing.T) {
	agent := &v1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-for-cloud"},
		Spec: v1alpha1.AgentSpec{
			Capabilities: []string{"cluster-inspection", "dev-environment"},
		},
	}
	caps := []v1alpha1.Capability{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-inspection"},
			Spec: v1alpha1.CapabilitySpec{
				Type: v1alpha1.CapabilityDomain,
				Uses: []string{"resource-manager", "kubectl-platform"},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "dev-environment"},
			Spec: v1alpha1.CapabilitySpec{
				Type:   v1alpha1.CapabilityAtomic,
				Agents: []string{"other-agent"}, // NOT visible to agent-for-cloud
			},
		},
	}
	got := ToolSetForAgent(agent, caps)
	want := map[string]bool{
		"list-kinds": true, "describe-kind": true, "resource-manager": true,
		"kubectl-raw": true, "kubectl-platform": true, "cluster-inspection": true,
	}
	// dev-environment must be excluded (agents[] visibility).
	if len(got) != len(want) {
		t.Fatalf("tool set size = %d, want %d (%v)", len(got), len(want), got)
	}
	for _, tool := range got {
		if !want[tool] {
			t.Errorf("unexpected tool %q in set %v", tool, got)
		}
	}
}

// TestValidateCapability verifies the registration validation rules
// (design §3.3.1: atomic must have override+target; domain must have
// instructions; a missing target CRD → fail-fast).
func TestValidateCapability(t *testing.T) {
	c := &Catalog{schemas: map[string]*CRDSchema{
		"dev.suanova.io/DevEnvironment": {
			Group: "dev.suanova.io", Version: "v1alpha1", Kind: "DevEnvironment", Plural: "devenvironments",
		},
	}}

	// Valid atomic.
	ok := &v1alpha1.Capability{
		ObjectMeta: metav1.ObjectMeta{Name: "dev-environment"},
		Spec: v1alpha1.CapabilitySpec{
			Type:     v1alpha1.CapabilityAtomic,
			Override: true,
			Target:   &v1alpha1.CapabilityTarget{Group: "dev.suanova.io", Version: "v1alpha1", Kind: "DevEnvironment"},
		},
	}
	if err := c.ValidateCapability(ok); err != nil {
		t.Errorf("valid atomic rejected: %v", err)
	}

	// Atomic without override → reject.
	noOverride := ok.DeepCopy()
	noOverride.Spec.Override = false
	if err := c.ValidateCapability(noOverride); err == nil {
		t.Error("atomic without override accepted")
	}

	// Atomic targeting a missing CRD → fail-fast.
	badTarget := ok.DeepCopy()
	badTarget.Spec.Target = &v1alpha1.CapabilityTarget{Group: "x.io", Kind: "Missing"}
	if err := c.ValidateCapability(badTarget); err == nil {
		t.Error("atomic with missing target accepted")
	}

	// Domain without instructions → reject.
	dom := &v1alpha1.Capability{
		ObjectMeta: metav1.ObjectMeta{Name: "d"},
		Spec:       v1alpha1.CapabilitySpec{Type: v1alpha1.CapabilityDomain},
	}
	if err := c.ValidateCapability(dom); err == nil {
		t.Error("domain without instructions accepted")
	}
}

// TestFindKind verifies case-insensitive kind resolution.
func TestFindKind(t *testing.T) {
	c := &Catalog{schemas: map[string]*CRDSchema{
		"dev.suanova.io/DevEnvironment": {Kind: "DevEnvironment", Plural: "devenvironments"},
	}}
	if got := c.FindKind("devenvironment"); got == nil || got.Kind != "DevEnvironment" {
		t.Errorf("FindKind(devenvironment) = %v, want DevEnvironment", got)
	}
	if got := c.FindKind("nope"); got != nil {
		t.Errorf("FindKind(nope) = %v, want nil", got)
	}
}
