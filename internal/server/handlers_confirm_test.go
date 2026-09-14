package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/k8s"
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

// TestAllowlistAlwaysDoesNotMaterialize verifies allow-always records only the
// rule itself -- and now records it in the grants store rather than the
// instance spec. Copying the platform builtin into the spec would let that
// snapshot outlive a later hardening of Default() (issue #185).
func TestAllowlistAlwaysDoesNotMaterialize(t *testing.T) {
	s := platformTestServer(t,
		internalTestAgent(v1alpha1.DefaultAgentName),
		internalTestInstance("li.ming", v1alpha1.DefaultAgentName),
	)
	ok, err := s.allowlistAlways(context.Background(), "li.ming", "helm list", v1alpha1.AllowlistRule{Pattern: "helm", ArgPattern: `^list`})
	if err != nil {
		t.Fatalf("allowlistAlways: %v", err)
	}
	if !ok {
		t.Fatal("allowlistAlways returned false under Allowlist policy")
	}
	got, err := s.grantsStore().List(context.Background(), "li.ming")
	if err != nil {
		t.Fatalf("List grants: %v", err)
	}
	if len(got) != 1 || got[0].Pattern != "helm" {
		t.Errorf("grants = %+v, want exactly the helm rule", got)
	}
	view := decode[approvalView](t, doReq(t, s.Handler(), http.MethodGet, "/api/v1/agent/approval", "li.ming", nil))
	if len(view.AllowlistOwned) != 0 {
		t.Errorf("owned allowlist = %+v, want the instance spec untouched", view.AllowlistOwned)
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
	ok, err := s.allowlistAlways(context.Background(), "li.ming", "helm list", v1alpha1.AllowlistRule{Pattern: "helm", ArgPattern: `^list`})
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

// TestAllowAlwaysWritesAGrantNotTheSpec covers issue #185: the machine-written
// grant must land in the grants store, and the instance spec must stay
// untouched so a learned rule cannot be mistaken for a hand-authored one.
func TestAllowAlwaysWritesAGrantNotTheSpec(t *testing.T) {
	s := platformTestServer(t,
		internalTestAgent(v1alpha1.DefaultAgentName),
		internalTestInstance("li.ming", v1alpha1.DefaultAgentName),
	)
	ctx := context.Background()
	rule, ok := deriveAllowAlwaysRule("kubectl get pods -n foo")
	if !ok {
		t.Fatal("deriveAllowAlwaysRule returned !ok")
	}
	if _, err := s.allowlistAlways(ctx, "li.ming", "kubectl get pods -n foo", rule); err != nil {
		t.Fatalf("allowlistAlways: %v", err)
	}

	got, err := s.grantsStore().List(ctx, "li.ming")
	if err != nil {
		t.Fatalf("List grants: %v", err)
	}
	if len(got) != 1 || got[0].Pattern != "kubectl" {
		t.Fatalf("grants = %+v, want one kubectl grant", got)
	}
	if got[0].Command != "kubectl get pods -n foo" {
		t.Errorf("Command = %q, want the approved invocation", got[0].Command)
	}

	var inst v1alpha1.AgentInstance
	name := types.NamespacedName{Namespace: s.cfg.Namespace, Name: k8s.InstanceName("li.ming", v1alpha1.DefaultAgentName)}
	if err := s.cr.Get(ctx, name, &inst); err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if len(inst.Spec.Allowlist) != 0 {
		t.Errorf("instance spec was written: %+v", inst.Spec.Allowlist)
	}
}

// TestClearOwnedAllowlistKeepsGrants: the Reset button must not discard what the
// user approved in chat -- the two stores are separate.
func TestClearOwnedAllowlistKeepsGrants(t *testing.T) {
	s := platformTestServer(t,
		internalTestAgent(v1alpha1.DefaultAgentName),
		internalTestInstance("li.ming", v1alpha1.DefaultAgentName),
	)
	ctx := context.Background()
	rule, _ := deriveAllowAlwaysRule("helm install x")
	if err := s.grantsStore().Add(ctx, "li.ming", rule, "helm install x", time.Now()); err != nil {
		t.Fatalf("Add grant: %v", err)
	}

	if err := s.saveConfirm(ctx, "li.ming", v1alpha1.ApprovalPolicyAllowlist, nil); err != nil {
		t.Fatalf("saveConfirm: %v", err)
	}

	got, err := s.grantsStore().List(ctx, "li.ming")
	if err != nil {
		t.Fatalf("List grants: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("Reset discarded learned grants: %+v", got)
	}
}

// TestAgentConfirmRevokesLearnedGrant covers the revoke path added in issue
// #185: a learned grant is dropped from the grants store without the
// hand-authored list being rewritten.
func TestAgentConfirmRevokesLearnedGrant(t *testing.T) {
	s := platformTestServer(t,
		internalTestAgent(v1alpha1.DefaultAgentName),
		internalTestInstance("li.ming", v1alpha1.DefaultAgentName),
	)
	ctx := context.Background()
	rule, _ := deriveAllowAlwaysRule("helm install x")
	if err := s.grantsStore().Add(ctx, "li.ming", rule, "helm install x", time.Now()); err != nil {
		t.Fatalf("Add grant: %v", err)
	}

	rec := doReq(t, s.Handler(), http.MethodPut, "/api/v1/agent/approval", "li.ming",
		map[string]any{
			"approvalPolicy": "Allowlist",
			"allowlist":      []any{},
			"revokeGrants":   []map[string]any{{"pattern": "helm", "argPattern": "^install x$"}},
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	got, err := s.grantsStore().List(ctx, "li.ming")
	if err != nil {
		t.Fatalf("List grants: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("grant not revoked: %+v", got)
	}
}
