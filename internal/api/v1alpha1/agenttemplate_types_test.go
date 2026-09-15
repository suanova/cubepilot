package v1alpha1

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestAgentTemplateSerializationRoundTrip verifies JSON round-trip of an
// AgentTemplate with inline providers (design §3.1/§3.3).
func TestAgentTemplateSerializationRoundTrip(t *testing.T) {
	in := &AgentTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "cubepilot"},
		Spec: AgentTemplateSpec{
			Runtime:        RuntimeOpenClaw,
			DefaultModel:   "deepseek/deepseek-v4-flash",
			ApprovalPolicy: ApprovalPolicyAllowlist,
			Providers: []TemplateProviderSpec{
				{
					Name:          "deepseek",
					Endpoint:      "https://api.deepseek.com",
					CredentialRef: &corev1.LocalObjectReference{Name: "cubepilot-llm"},
					Models:        []string{"deepseek-v4-flash"},
				},
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
	if out.Spec.Runtime != RuntimeOpenClaw || out.Spec.ApprovalPolicy != ApprovalPolicyAllowlist {
		t.Errorf("scalar round-trip mismatch: %+v", out.Spec)
	}
	if len(out.Spec.Providers) != 1 || out.Spec.Providers[0].Endpoint == "" ||
		out.Spec.Providers[0].CredentialRef == nil || out.Spec.Providers[0].CredentialRef.Name != "cubepilot-llm" ||
		len(out.Spec.Providers[0].Models) != 1 || out.Spec.Providers[0].Models[0] != "deepseek-v4-flash" {
		t.Errorf("inline providers not round-tripped: %+v", out.Spec.Providers)
	}
}

// TestAgentTemplateRevision verifies the revision is a spec-only content hash:
// deterministic across re-creation, unchanged by status, changed by spec.
func TestAgentTemplateRevision(t *testing.T) {
	a := &AgentTemplate{ObjectMeta: metav1.ObjectMeta{Name: "cubepilot"}, Spec: AgentTemplateSpec{DefaultModel: "platform/deepseek-v4-flash"}}
	base := a.Revision()
	if len(base) != 12 {
		t.Fatalf("revision = %q, want 12 hex chars", base)
	}
	// Deterministic across re-creation (metadata ignored).
	b := &AgentTemplate{ObjectMeta: metav1.ObjectMeta{Name: "renamed"}, Spec: AgentTemplateSpec{DefaultModel: "platform/deepseek-v4-flash"}}
	if b.Revision() != base {
		t.Errorf("revision depends on metadata: %q != %q", b.Revision(), base)
	}
	// Status change does not alter the revision.
	a.Status.ObservedGeneration = 42
	if a.Revision() != base {
		t.Errorf("status change altered revision: %q != %q", a.Revision(), base)
	}
	// Spec change does.
	a.Spec.ApprovalPolicy = ApprovalPolicyAllowlist
	if a.Revision() == base {
		t.Error("spec change did not alter revision")
	}
}

// modelIDs returns n distinct ids, for the bound cases below.
func modelIDs(n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("m%d", i)
	}
	return ids
}

// TestTemplateProviderValidate pins the single Go-side grammar validator the
// write API delegates to. Its bounds are the CRD's markers and CEL rules
// restated, so a request it accepts is not refused by the API server afterwards
// -- which the handler could only answer as a 500.
func TestTemplateProviderValidate(t *testing.T) {
	ok := []TemplateProviderSpec{
		{Name: "vllm", Endpoint: "http://vllm.ai.svc:8000/v1", Models: []string{"qwen3-32b"}},
		{Name: "openrouter", Endpoint: "https://openrouter.ai/api/v1", Models: []string{"anthropic/claude-sonnet-4.5", "openai/gpt-5-mini"}},
		{Name: "a", Endpoint: "https://x", Models: []string{"m"}},
		// The bounds themselves are inclusive: one over is the bad case below.
		{Name: "max-models", Endpoint: "https://x", Models: modelIDs(64)},
		{Name: "max-id", Endpoint: "https://x", Models: []string{strings.Repeat("m", 256)}},
		{Name: "max-endpoint", Endpoint: "https://x/" + strings.Repeat("p", 2038), Models: []string{"m"}},
	}
	for _, p := range ok {
		if err := p.Validate(); err != nil {
			t.Errorf("Validate(%s) = %v, want nil", p.Name, err)
		}
	}
	bad := []TemplateProviderSpec{
		{Name: "", Endpoint: "https://x", Models: []string{"m"}},
		{Name: "Upper", Endpoint: "https://x", Models: []string{"m"}},
		{Name: "has/slash", Endpoint: "https://x", Models: []string{"m"}},
		{Name: "-leading", Endpoint: "https://x", Models: []string{"m"}},
		{Name: "trailing-", Endpoint: "https://x", Models: []string{"m"}},
		{Name: "no-endpoint", Models: []string{"m"}},
		{Name: "no-models", Endpoint: "https://x"},
		{Name: "empty-id", Endpoint: "https://x", Models: []string{""}},
		{Name: "wildcard", Endpoint: "https://x", Models: []string{"*"}},
		{Name: "double-slash", Endpoint: "https://x", Models: []string{"a//b"}},
		{Name: "leading-slash", Endpoint: "https://x", Models: []string{"/a"}},
		{Name: "trailing-slash", Endpoint: "https://x", Models: []string{"a/"}},
		{Name: "space", Endpoint: "https://x", Models: []string{"a b"}},
		// Models is +listType=set, so the API server refuses a repeated id; the
		// Go mirror must refuse it too.
		{Name: "duplicate-ids", Endpoint: "https://x", Models: []string{"m", "m"}},
		// The Go counterpart of the credentialRef CEL rule on spec.providers:
		// present but unnamed is refused, absent (every ok case above) is not.
		{Name: "bad-cred", Endpoint: "https://x", Models: []string{"m"}, CredentialRef: &corev1.LocalObjectReference{}},
		{Name: "too-many-models", Endpoint: "https://x", Models: modelIDs(65)},
		{Name: "long-id", Endpoint: "https://x", Models: []string{strings.Repeat("m", 257)}},
		{Name: "long-endpoint", Endpoint: "https://x/" + strings.Repeat("p", 2040), Models: []string{"m"}},
	}
	for _, p := range bad {
		if err := p.Validate(); err == nil {
			t.Errorf("Validate(%+v) = nil, want error", p)
		}
	}
}
