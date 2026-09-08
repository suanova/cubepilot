package server

import (
	"context"
	"net/http"
	"testing"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
)

// TestAgentConfirmRoundTrip covers GET/PUT /api/agent/confirm: override
// confirmPolicy + owned allowlist round trip, and clearing both returns the
// instance to inheriting the template default.
func TestAgentConfirmRoundTrip(t *testing.T) {
	s := platformTestServer(t,
		internalTestAgent(v1alpha1.DefaultAgentName),
		internalTestInstance("li.ming", v1alpha1.DefaultAgentName),
	)

	// Initial: inheriting -> effective Allowlist with the platform default list.
	view := decode[confirmView](t, doReq(t, s.Handler(), http.MethodGet, "/api/agent/confirm", "li.ming", nil))
	if !view.Exists || view.Override != "" || view.ConfirmPolicy != v1alpha1.ConfirmPolicyAllowlist {
		t.Fatalf("initial view = %+v, want exists Allowlist with no override", view)
	}
	// No HITL manager in the test server -> the approval channel is unconfigured.
	if view.Channel != confirmChannelUnconfigured {
		t.Errorf("channel = %q, want %q", view.Channel, confirmChannelUnconfigured)
	}
	if len(view.Allowlist) == 0 {
		t.Fatalf("initial effective allowlist empty, want platform defaults: %+v", view)
	}

	// Override to AlwaysAsk + own an allowlist.
	owned := []v1alpha1.AllowlistRule{{Pattern: "git", ArgPattern: `^(log|status)(\s|$)`}}
	view = decode[confirmView](t, doReq(t, s.Handler(), http.MethodPut, "/api/agent/confirm", "li.ming",
		map[string]any{"confirmPolicy": "AlwaysAsk", "allowlist": owned}))
	if view.ConfirmPolicy != v1alpha1.ConfirmPolicyAlwaysAsk || view.Override != v1alpha1.ConfirmPolicyAlwaysAsk {
		t.Errorf("after override view.ConfirmPolicy = %q override = %q", view.ConfirmPolicy, view.Override)
	}
	if len(view.AllowlistOwned) != 1 || view.AllowlistOwned[0].Pattern != "git" {
		t.Errorf("owned allowlist = %+v, want the git entry", view.AllowlistOwned)
	}

	// Clear both -> back to inheriting the template default.
	view = decode[confirmView](t, doReq(t, s.Handler(), http.MethodPut, "/api/agent/confirm", "li.ming",
		map[string]any{"confirmPolicy": "", "allowlist": []any{}}))
	if view.Override != "" || view.ConfirmPolicy != v1alpha1.ConfirmPolicyAllowlist {
		t.Errorf("after clear override = %q confirmPolicy = %q", view.Override, view.ConfirmPolicy)
	}
	if len(view.AllowlistOwned) != 0 || len(view.Allowlist) == 0 {
		t.Errorf("after clear owned=%v effective=%d, want inherited platform defaults", view.AllowlistOwned, len(view.Allowlist))
	}
}

// TestAgentConfirmInvalidPolicy verifies a bad confirmPolicy is rejected.
func TestAgentConfirmInvalidPolicy(t *testing.T) {
	s := platformTestServer(t,
		internalTestAgent(v1alpha1.DefaultAgentName),
		internalTestInstance("li.ming", v1alpha1.DefaultAgentName),
	)
	rec := doReq(t, s.Handler(), http.MethodPut, "/api/agent/confirm", "li.ming",
		map[string]any{"confirmPolicy": "Sometimes"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

// TestAgentConfirmNoInstance verifies a user without an instance cannot save a
// confirmation posture and GET reports not-provisioned.
func TestAgentConfirmNoInstance(t *testing.T) {
	s := platformTestServer(t, internalTestAgent(v1alpha1.DefaultAgentName))
	view := decode[confirmView](t, doReq(t, s.Handler(), http.MethodGet, "/api/agent/confirm", "nobody", nil))
	if view.Exists {
		t.Fatalf("view.Exists = true for a user with no instance: %+v", view)
	}
	rec := doReq(t, s.Handler(), http.MethodPut, "/api/agent/confirm", "nobody",
		map[string]any{"confirmPolicy": "AlwaysAsk"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
}

// TestAllowlistAlwaysMaterializes verifies allow-always appends to the instance
// allowlist, materializing the inherited default on first ownership; and that
// under AlwaysAsk policy the append is a no-op.
func TestAllowlistAlwaysMaterializes(t *testing.T) {
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
	view := decode[confirmView](t, doReq(t, s.Handler(), http.MethodGet, "/api/agent/confirm", "li.ming", nil))
	var sawKubectl, sawHelm bool
	for _, e := range view.AllowlistOwned {
		switch e.Pattern {
		case "kubectl":
			sawKubectl = true
		case "helm":
			sawHelm = true
		}
	}
	if !sawKubectl || !sawHelm {
		t.Errorf("owned allowlist = %+v, want materialized kubectl default + helm", view.AllowlistOwned)
	}
}

// TestAllowlistAlwaysSkippedUnderAlwaysAsk verifies the durable grant is not
// appended when the effective policy is AlwaysAsk.
func TestAllowlistAlwaysSkippedUnderAlwaysAsk(t *testing.T) {
	inst := internalTestInstance("li.ming", v1alpha1.DefaultAgentName)
	inst.Spec.ConfirmPolicy = v1alpha1.ConfirmPolicyAlwaysAsk
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
	view := decode[confirmView](t, doReq(t, s.Handler(), http.MethodGet, "/api/agent/confirm", "li.ming", nil))
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
	view := decode[confirmView](t, doReq(t, s.Handler(), http.MethodGet, "/api/agent/confirm", "zhang.wei", nil))
	if view.Exists {
		t.Fatalf("zhang.wei should have no instance, got %+v", view)
	}
}
