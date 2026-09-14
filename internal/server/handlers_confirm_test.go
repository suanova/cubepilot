package server

import (
	"context"
	"net/http"
	"testing"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
)

// TestAgentConfirmRoundTrip covers GET/PUT /api/agent/approval: override
// approvalPolicy + owned allowlist round trip, and clearing both returns the
// instance to inheriting the template default.
func TestAgentConfirmRoundTrip(t *testing.T) {
	s := platformTestServer(t,
		internalTestAgent(v1alpha1.DefaultAgentName),
		internalTestInstance("li.ming", v1alpha1.DefaultAgentName),
	)

	// Initial: inheriting -> effective Allowlist with the platform default list.
	view := decode[approvalView](t, doReq(t, s.Handler(), http.MethodGet, "/api/v1/agent/approval", "li.ming", nil))
	if !view.Exists || view.Override != "" || view.ApprovalPolicy != v1alpha1.ApprovalPolicyAllowlist {
		t.Fatalf("initial view = %+v, want exists Allowlist with no override", view)
	}
	// No HITL manager in the test server -> the approval channel is unconfigured.
	if view.Channel != approvalChannelUnconfigured {
		t.Errorf("channel = %q, want %q", view.Channel, approvalChannelUnconfigured)
	}
	if len(view.Allowlist) == 0 {
		t.Fatalf("initial effective allowlist empty, want platform defaults: %+v", view)
	}

	// Override to AlwaysAsk + own an allowlist.
	owned := []v1alpha1.AllowlistRule{{Pattern: "git", ArgPattern: `^(log|status)(\s|$)`}}
	view = decode[approvalView](t, doReq(t, s.Handler(), http.MethodPut, "/api/v1/agent/approval", "li.ming",
		map[string]any{"approvalPolicy": "AlwaysAsk", "allowlist": owned}))
	if view.ApprovalPolicy != v1alpha1.ApprovalPolicyAlwaysAsk || view.Override != v1alpha1.ApprovalPolicyAlwaysAsk {
		t.Errorf("after override view.ApprovalPolicy = %q override = %q", view.ApprovalPolicy, view.Override)
	}
	if len(view.AllowlistOwned) != 1 || view.AllowlistOwned[0].Pattern != "git" {
		t.Errorf("owned allowlist = %+v, want the git entry", view.AllowlistOwned)
	}

	// Clear both -> back to inheriting the template default.
	view = decode[approvalView](t, doReq(t, s.Handler(), http.MethodPut, "/api/v1/agent/approval", "li.ming",
		map[string]any{"approvalPolicy": "", "allowlist": []any{}}))
	if view.Override != "" || view.ApprovalPolicy != v1alpha1.ApprovalPolicyAllowlist {
		t.Errorf("after clear override = %q approvalPolicy = %q", view.Override, view.ApprovalPolicy)
	}
	if len(view.AllowlistOwned) != 0 || len(view.Allowlist) == 0 {
		t.Errorf("after clear owned=%v effective=%d, want inherited platform defaults", view.AllowlistOwned, len(view.Allowlist))
	}
}

// TestAgentConfirmInvalidPolicy verifies a bad approvalPolicy is rejected.
func TestAgentConfirmInvalidPolicy(t *testing.T) {
	s := platformTestServer(t,
		internalTestAgent(v1alpha1.DefaultAgentName),
		internalTestInstance("li.ming", v1alpha1.DefaultAgentName),
	)
	rec := doReq(t, s.Handler(), http.MethodPut, "/api/v1/agent/approval", "li.ming",
		map[string]any{"approvalPolicy": "Sometimes"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

// TestAgentConfirmNoInstance verifies a user without an instance cannot save a
// confirmation posture and GET reports not-provisioned.
func TestAgentConfirmNoInstance(t *testing.T) {
	s := platformTestServer(t, internalTestAgent(v1alpha1.DefaultAgentName))
	view := decode[approvalView](t, doReq(t, s.Handler(), http.MethodGet, "/api/v1/agent/approval", "nobody", nil))
	if view.Exists {
		t.Fatalf("view.Exists = true for a user with no instance: %+v", view)
	}
	rec := doReq(t, s.Handler(), http.MethodPut, "/api/v1/agent/approval", "nobody",
		map[string]any{"approvalPolicy": "AlwaysAsk"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
}

// TestAllowlistAlwaysDoesNotMaterialize verifies allow-always appends only the
// rule itself. Copying the platform builtin into the spec would let that
// snapshot outlive a later hardening of Default() (issue #185).
func TestAllowlistAlwaysDoesNotMaterialize(t *testing.T) {
	s := platformTestServer(t,
		internalTestAgent(v1alpha1.DefaultAgentName),
		internalTestInstance("li.ming", v1alpha1.DefaultAgentName),
	)
	ok, err := s.allowlistAlways(context.Background(), "li.ming", v1alpha1.AllowlistRule{Pattern: "helm", ArgPattern: `^list`})
	if err != nil {
		t.Fatalf("allowlistAlways: %v", err)
	}
	if !ok {
		t.Fatal("allowlistAlways returned false under Allowlist policy")
	}
	view := decode[approvalView](t, doReq(t, s.Handler(), http.MethodGet, "/api/v1/agent/approval", "li.ming", nil))
	if len(view.AllowlistOwned) != 1 || view.AllowlistOwned[0].Pattern != "helm" {
		t.Errorf("owned allowlist = %+v, want exactly the helm rule", view.AllowlistOwned)
	}
}

// TestAllowlistAlwaysSkippedUnderAlwaysAsk verifies the durable grant is not
// appended when the effective policy is AlwaysAsk.
func TestAllowlistAlwaysSkippedUnderAlwaysAsk(t *testing.T) {
	inst := internalTestInstance("li.ming", v1alpha1.DefaultAgentName)
	inst.Spec.ApprovalPolicy = v1alpha1.ApprovalPolicyAlwaysAsk
	s := platformTestServer(t,
		internalTestAgent(v1alpha1.DefaultAgentName),
		inst,
	)
	ok, err := s.allowlistAlways(context.Background(), "li.ming", v1alpha1.AllowlistRule{Pattern: "helm", ArgPattern: `^list`})
	if err != nil {
		t.Fatalf("allowlistAlways: %v", err)
	}
	if ok {
		t.Fatal("allowlistAlways should be a no-op under AlwaysAsk")
	}
	view := decode[approvalView](t, doReq(t, s.Handler(), http.MethodGet, "/api/v1/agent/approval", "li.ming", nil))
	if len(view.AllowlistOwned) != 0 {
		t.Errorf("owned allowlist should stay empty under AlwaysAsk, got %+v", view.AllowlistOwned)
	}
}

func TestDeriveAllowAlwaysRule(t *testing.T) {
	rule, ok := deriveAllowAlwaysRule("kubectl delete pod foo")
	if !ok || rule.Pattern != "kubectl" || rule.ArgPattern != `^delete pod foo$` {
		t.Errorf("derive kubectl = %+v ok=%v", rule, ok)
	}
	rule, ok = deriveAllowAlwaysRule("ls")
	if !ok || rule.Pattern != "ls" || rule.ArgPattern != `^$` {
		t.Errorf("derive bare = %+v ok=%v", rule, ok)
	}
	if _, ok := deriveAllowAlwaysRule("   "); ok {
		t.Error("blank command should not derive a rule")
	}
}

// TestAgentConfirmUserScoped ensures a user's posture is not readable as
// another user's (no instance for them -> not provisioned).
func TestAgentConfirmUserScoped(t *testing.T) {
	s := platformTestServer(t,
		internalTestAgent(v1alpha1.DefaultAgentName),
		internalTestInstance("li.ming", v1alpha1.DefaultAgentName),
	)
	view := decode[approvalView](t, doReq(t, s.Handler(), http.MethodGet, "/api/v1/agent/approval", "zhang.wei", nil))
	if view.Exists {
		t.Fatalf("zhang.wei should have no instance, got %+v", view)
	}
}

// TestAgentConfirmRejectsInvalidArgPattern covers issue #185: argPattern is free
// text from the form, shipped to the gateway unvalidated, so a typo was stored
// and pushed and the user never heard about it.
func TestAgentConfirmRejectsInvalidArgPattern(t *testing.T) {
	s := platformTestServer(t,
		internalTestAgent(v1alpha1.DefaultAgentName),
		internalTestInstance("li.ming", v1alpha1.DefaultAgentName),
	)
	rec := doReq(t, s.Handler(), http.MethodPut, "/api/v1/agent/approval", "li.ming",
		map[string]any{
			"approvalPolicy": "Allowlist",
			"allowlist":      []map[string]any{{"pattern": "ls", "argPattern": "^(.*$"}},
		})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}
