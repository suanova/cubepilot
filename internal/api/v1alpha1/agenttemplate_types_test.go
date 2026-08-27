package v1alpha1

import (
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestAgentTemplateSerializationRoundTrip verifies JSON round-trip of an
// AgentTemplate with inline models (design §3.1/§3.3).
func TestAgentTemplateSerializationRoundTrip(t *testing.T) {
	in := &AgentTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-for-cloud"},
		Spec: AgentTemplateSpec{
			Runtime:       RuntimeOpenClaw,
			DefaultModel:  "deepseek-v4-flash",
			ConfirmPolicy: ConfirmPolicyConfirmWrites,
			Models: []TemplateModelSpec{
				{Name: "deepseek-v4-flash", Endpoint: "https://api.deepseek.com", CredentialRef: &corev1.LocalObjectReference{Name: "cubepilot-llm"}},
			},
			Skills: []string{"dev-environment", "inference-service"},
		},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out AgentTemplate
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Spec.Runtime != RuntimeOpenClaw || out.Spec.ConfirmPolicy != ConfirmPolicyConfirmWrites {
		t.Errorf("scalar round-trip mismatch: %+v", out.Spec)
	}
	if len(out.Spec.Models) != 1 || out.Spec.Models[0].Endpoint == "" ||
		out.Spec.Models[0].CredentialRef == nil || out.Spec.Models[0].CredentialRef.Name != "cubepilot-llm" {
		t.Errorf("inline models not round-tripped: %+v", out.Spec.Models)
	}
}

// TestAgentTemplateRevision verifies the revision is a spec-only content hash:
// deterministic across re-creation, unchanged by status, changed by spec.
func TestAgentTemplateRevision(t *testing.T) {
	a := &AgentTemplate{ObjectMeta: metav1.ObjectMeta{Name: "agent-for-cloud"}, Spec: AgentTemplateSpec{DefaultModel: "deepseek-v4-flash"}}
	base := a.Revision()
	if len(base) != 12 {
		t.Fatalf("revision = %q, want 12 hex chars", base)
	}
	// Deterministic across re-creation (metadata ignored).
	b := &AgentTemplate{ObjectMeta: metav1.ObjectMeta{Name: "renamed"}, Spec: AgentTemplateSpec{DefaultModel: "deepseek-v4-flash"}}
	if b.Revision() != base {
		t.Errorf("revision depends on metadata: %q != %q", b.Revision(), base)
	}
	// Status change does not alter the revision.
	a.Status.ObservedGeneration = 42
	if a.Revision() != base {
		t.Errorf("status change altered revision: %q != %q", a.Revision(), base)
	}
	// Spec change does.
	a.Spec.ConfirmPolicy = ConfirmPolicyConfirmWrites
	if a.Revision() == base {
		t.Error("spec change did not alter revision")
	}
}

// TestTemplateModelValidate rejects invalid combinations (design §3.3):
// every model needs an endpoint; a missing name is rejected.
func TestTemplateModelValidate(t *testing.T) {
	ok := []TemplateModelSpec{
		{Name: "deepseek-v4-flash", Endpoint: "https://api.deepseek.com"},
	}
	for _, m := range ok {
		if err := m.Validate(); err != nil {
			t.Errorf("Validate(%s) = %v, want nil", m.Name, err)
		}
	}
	bad := []TemplateModelSpec{
		{Name: "no-endpoint"},
		{Name: ""},
		{Name: "bad-cred", Endpoint: "https://x", CredentialRef: &corev1.LocalObjectReference{}},
	}
	for _, m := range bad {
		if err := m.Validate(); err == nil {
			t.Errorf("Validate(%s) = nil, want error", m.Name)
		}
	}
}
