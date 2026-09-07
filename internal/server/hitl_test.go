package server

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/suanova/cubepilot/internal/allowlist"
	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/openclaw/ws"
)

// fakeHitlGateway implements hitlGateway in memory.
type fakeHitlGateway struct {
	connected    bool
	guarded      []string
	policySets   []ws.ApprovalsFile
	resolves     []string // "id|decision"
	onRequested  func(ws.ApprovalRequested)
	connectErr   error
	connectSeq   []error // optional per-connect results, consumed in order
	getErr       error
	setErr       error
	initialAllow []ws.AllowlistEntry
}

func (f *fakeHitlGateway) Connected() bool { return f.connected }
func (f *fakeHitlGateway) Connect(ctx context.Context) error {
	if len(f.connectSeq) > 0 {
		err := f.connectSeq[0]
		f.connectSeq = f.connectSeq[1:]
		if err != nil {
			return err
		}
		f.connected = true
		return nil
	}
	if f.connectErr != nil {
		return f.connectErr
	}
	f.connected = true
	return nil
}
func (f *fakeHitlGateway) OnApprovalRequested(cb func(ws.ApprovalRequested)) { f.onRequested = cb }
func (f *fakeHitlGateway) GetApprovalsPolicy(ctx context.Context) (*ws.ApprovalsSnapshot, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return &ws.ApprovalsSnapshot{
		Exists: true,
		Hash:   "hash-1",
		File: ws.ApprovalsFile{
			Version: 1,
			Agents: map[string]ws.ApprovalAgentPolicy{
				"main": {Allowlist: f.initialAllow},
			},
		},
	}, nil
}
func (f *fakeHitlGateway) SetApprovalsPolicy(ctx context.Context, file ws.ApprovalsFile, baseHash string) (*ws.ApprovalsSnapshot, error) {
	if f.setErr != nil {
		return nil, f.setErr
	}
	f.policySets = append(f.policySets, file)
	return &ws.ApprovalsSnapshot{}, nil
}
func (f *fakeHitlGateway) EnsureSessionGuarded(ctx context.Context, key string) error {
	f.guarded = append(f.guarded, key)
	return nil
}
func (f *fakeHitlGateway) ResolveApproval(ctx context.Context, id, decision string) error {
	f.resolves = append(f.resolves, id+"|"+decision)
	return nil
}
func (f *fakeHitlGateway) Close() {}

// newTestHitl returns a manager whose gateway is a fresh fake per connect and
// whose policy resolution is fixed. With no explicit allowlist, Allowlist
// policy resolves to the platform builtin default (so its entries are
// non-empty in tests).
func newTestHitl(pol v1alpha1.ConfirmPolicy, rev string, gw *fakeHitlGateway, allow ...[]v1alpha1.AllowlistRule) *hitlManager {
	m := &hitlManager{
		masterKey: []byte("test-master"),
		logf:      tLogf,
		conns:     map[string]*userHitlConn{},
		revPol:    map[string]string{},
	}
	m.newClient = func(url string, dev *ws.Device) hitlGateway { return gw }
	m.resolved = func(ctx context.Context, user string) (v1alpha1.ConfirmPolicy, []v1alpha1.AllowlistRule, string, error) {
		var al []v1alpha1.AllowlistRule
		if len(allow) > 0 {
			al = allow[0]
		} else if pol == v1alpha1.ConfirmPolicyAllowlist {
			al = allowlist.Default()
		}
		return pol, al, rev, nil
	}
	m.wsURLOf = func(user string) string { return "ws://fake/gateway" }
	return m
}

var tLogf = func(format string, args ...any) {}

func TestHitl_PreTurnGuardsAllowlistOncePerRevision(t *testing.T) {
	gw := &fakeHitlGateway{}
	m := newTestHitl(v1alpha1.ConfirmPolicyAllowlist, "rev-1", gw)

	m.PreTurn(context.Background(), "alice", "conv-1")
	if len(gw.guarded) != 1 || gw.guarded[0] != "conv-1" {
		t.Fatalf("guarded = %v, want [conv-1]", gw.guarded)
	}
	// The allowlist is applied exactly once for the revision...
	if len(gw.policySets) != 1 {
		t.Fatalf("policy sets = %d, want 1", len(gw.policySets))
	}
	agent, ok := gw.policySets[0].Agents["main"]
	if !ok || len(agent.Allowlist) == 0 {
		t.Fatalf("allowlist not written to agents.main: %+v", gw.policySets[0])
	}

	// ...and again guards (idempotent) without re-applying the allowlist.
	m.PreTurn(context.Background(), "alice", "conv-1")
	if len(gw.policySets) != 1 {
		t.Errorf("policy sets = %d after second turn, want 1", len(gw.policySets))
	}
	if len(gw.guarded) != 2 {
		t.Errorf("guarded calls = %d, want 2 (per turn)", len(gw.guarded))
	}

	// A policy change applies the allowlist again.
	m.revPol["alice"] = "rev-1-old"
	m.PreTurn(context.Background(), "alice", "conv-2")
	if len(gw.policySets) != 2 {
		t.Errorf("policy sets = %d after revision change, want 2", len(gw.policySets))
	}
}

// TestHitl_ConnectRetriesAfterNotPaired verifies a Allowlist turn survives
// an initial NOT_PAIRED rejection while the supervisor approves the pairing.
func TestHitl_ConnectRetriesAfterNotPaired(t *testing.T) {
	old := hitlPairRetryDelay
	hitlPairRetryDelay = time.Millisecond
	defer func() { hitlPairRetryDelay = old }()

	gw := &fakeHitlGateway{connectSeq: []error{fmt.Errorf("NOT_PAIRED: device is not approved yet"), nil}}
	m := newTestHitl(v1alpha1.ConfirmPolicyAllowlist, "rev-1", gw)
	m.PreTurn(context.Background(), "alice", "conv-1")
	if len(gw.guarded) != 1 || gw.guarded[0] != "conv-1" {
		t.Fatalf("guarded = %v after retry, want [conv-1]", gw.guarded)
	}
	if !gw.connected {
		t.Fatal("expected the gateway to connect after the NOT_PAIRED retry")
	}
}

func TestHitl_PreTurnNoopWithoutAllowlist(t *testing.T) {
	for _, pol := range []v1alpha1.ConfirmPolicy{"", v1alpha1.ConfirmPolicyNone} {
		gw := &fakeHitlGateway{}
		m := newTestHitl(pol, "rev-1", gw)
		m.PreTurn(context.Background(), "alice", "conv-1")
		if len(gw.guarded) != 0 || len(gw.policySets) != 0 || gw.connected {
			t.Errorf("pol=%q: expected no-op, guarded=%v policySets=%d connected=%v", pol, gw.guarded, len(gw.policySets), gw.connected)
		}
	}
}

func TestHitl_ResolveApprovalMapsDecision(t *testing.T) {
	gw := &fakeHitlGateway{}
	m := newTestHitl(v1alpha1.ConfirmPolicyAllowlist, "rev-1", gw)
	m.PreTurn(context.Background(), "alice", "conv-1") // establishes the conn

	if err := m.ResolveApproval(context.Background(), "alice", "appr-1", "approve"); err != nil {
		t.Fatalf("resolve approve: %v", err)
	}
	if err := m.ResolveApproval(context.Background(), "alice", "appr-2", "reject"); err != nil {
		t.Fatalf("resolve reject: %v", err)
	}
	want := []string{"appr-1|allow-once", "appr-2|deny"}
	if len(gw.resolves) != 2 || gw.resolves[0] != want[0] || gw.resolves[1] != want[1] {
		t.Errorf("resolves = %v, want %v", gw.resolves, want)
	}

	// No conn for an unknown user -> error.
	if err := m.ResolveApproval(context.Background(), "bob", "appr-3", "approve"); err == nil {
		t.Error("expected an error resolving for a user with no connection")
	}
}

func TestHitl_BridgeFeedsApprovalService(t *testing.T) {
	gw := &fakeHitlGateway{}
	m := newTestHitl(v1alpha1.ConfirmPolicyAllowlist, "rev-1", gw)
	var fed []string
	m.bridge = func(user string, ev ws.ApprovalRequested) {
		fed = append(fed, user+"|"+ev.ID+"|"+ev.Request.SessionKey+"|"+ev.Request.Command)
	}
	m.PreTurn(context.Background(), "alice", "conv-1")
	if gw.onRequested == nil {
		t.Fatal("expected the gateway to have an approval callback")
	}
	var ev ws.ApprovalRequested
	ev.ID = "appr-9"
	ev.Request.Command = "kubectl delete pod x"
	ev.Request.SessionKey = "conv-1"
	gw.onRequested(ev)
	if len(fed) != 1 || fed[0] != "alice|appr-9|conv-1|kubectl delete pod x" {
		t.Errorf("bridge fed = %v", fed)
	}
}

// TestHitl_AlwaysAskFailsClosedOnPolicyError verifies AlwaysAsk returns an
// error (fail closed) when the exec policy cannot be applied, so the caller
// does not start a turn that would not ask.
func TestHitl_AlwaysAskFailsClosedOnPolicyError(t *testing.T) {
	gw := &fakeHitlGateway{setErr: fmt.Errorf("exec.approvals.set: boom")}
	m := newTestHitl(v1alpha1.ConfirmPolicyAlwaysAsk, "rev-1", gw)
	if err := m.PreTurn(context.Background(), "alice", "conv-1"); err == nil {
		t.Fatal("AlwaysAsk PreTurn should fail closed when the policy cannot be applied")
	}
}

// TestHitl_AllowlistBestEffortOnPolicyError verifies Allowlist keeps the issue
// #20 best-effort posture: a policy-apply failure is logged but the turn still
// proceeds (nil error), so transient channel hiccups do not block chat.
func TestHitl_AllowlistBestEffortOnPolicyError(t *testing.T) {
	gw := &fakeHitlGateway{setErr: fmt.Errorf("exec.approvals.set: boom")}
	m := newTestHitl(v1alpha1.ConfirmPolicyAllowlist, "rev-1", gw)
	if err := m.PreTurn(context.Background(), "alice", "conv-1"); err != nil {
		t.Fatalf("Allowlist PreTurn should be best-effort (nil error), got %v", err)
	}
	// The failed apply must not advance the revision watermark (retried next turn).
	if m.revPol["alice"] != "" {
		t.Errorf("revPol advanced despite a failed apply: %q", m.revPol["alice"])
	}
}

// TestHitl_AlwaysAskGuardsAndClearsAllowlist verifies AlwaysAsk writes an empty
// allowlist (so every command misses and asks) and still guards the session.
func TestHitl_AlwaysAskGuardsAndClearsAllowlist(t *testing.T) {
	gw := &fakeHitlGateway{initialAllow: []ws.AllowlistEntry{{Pattern: "kubectl", ArgPattern: "^get "}}}
	m := newTestHitl(v1alpha1.ConfirmPolicyAlwaysAsk, "rev-1", gw)

	m.PreTurn(context.Background(), "alice", "conv-1")
	if len(gw.guarded) != 1 || gw.guarded[0] != "conv-1" {
		t.Fatalf("guarded = %v, want [conv-1]", gw.guarded)
	}
	if len(gw.policySets) != 1 {
		t.Fatalf("policy sets = %d, want 1", len(gw.policySets))
	}
	agent, ok := gw.policySets[0].Agents["main"]
	if !ok {
		t.Fatalf("agents.main missing: %+v", gw.policySets[0])
	}
	if len(agent.Allowlist) != 0 {
		t.Errorf("AlwaysAsk allowlist = %+v, want empty (everything asks)", agent.Allowlist)
	}
}

// TestHitl_AllowlistRewritesEffectiveEntries verifies the gateway allowlist is
// rewritten from the resolved (effective) entries -- not merged with whatever
// the gateway already held -- so a removed entry really disappears.
func TestHitl_AllowlistRewritesEffectiveEntries(t *testing.T) {
	gw := &fakeHitlGateway{initialAllow: []ws.AllowlistEntry{{Pattern: "stale"}}}
	effective := []v1alpha1.AllowlistRule{{Pattern: "helm", ArgPattern: `^list`}}
	m := newTestHitl(v1alpha1.ConfirmPolicyAllowlist, "rev-1", gw, effective)

	m.PreTurn(context.Background(), "alice", "conv-1")
	if len(gw.policySets) != 1 {
		t.Fatalf("policy sets = %d, want 1", len(gw.policySets))
	}
	agent, ok := gw.policySets[0].Agents["main"]
	if !ok {
		t.Fatalf("agents.main missing: %+v", gw.policySets[0])
	}
	if len(agent.Allowlist) != 1 || agent.Allowlist[0].Pattern != "helm" {
		t.Errorf("allowlist = %+v, want exactly the effective helm entry", agent.Allowlist)
	}
}
