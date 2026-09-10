package server

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/suanova/cubepilot/internal/allowlist"
	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/openclaw"
	"github.com/suanova/cubepilot/internal/openclaw/ws"
	agentruntime "github.com/suanova/cubepilot/internal/runtime"
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
	guardErr     error
	initialAllow []ws.AllowlistEntry

	// live-tool channel (issue #130)
	onEvent      func(evName string, payload []byte)
	subscribes   []string
	unsubscribes []string
	sends        []string // "sessionKey|message"
	lastIdem     string
	creates      []string // sessionKeys passed to sessions.create
	models       []string // "sessionKey|model"; empty model clears the override
	unguarded    []string // sessionKeys whose explicit permission mode was cleared
	states       map[string]ws.SessionState
	sendBlock    chan struct{}
	waits        []string // runIds passed to agent.wait
	subscribeErr error
	sendErr      error
	waitErr      error
	createErr    error
	modelErr     error

	// ask_user question channel (issue #161)
	onQuestionRequested func(ws.QuestionRecord)
	onQuestionResolved  func(ws.QuestionResolved)
	questionResolves    []string // "id|resolvedBy|qid=label;qid2=a,b"
	questionCancels     []string // "id|resolvedBy"
	questionResolveErr  error
	getQuestionErr      error
	listQuestionsErr    error
	questionRecords     map[string]ws.QuestionRecord
	pendingQuestions    []ws.QuestionRecord
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
func (f *fakeHitlGateway) OnEvent(cb func(evName string, payload []byte))    { f.onEvent = cb }
func (f *fakeHitlGateway) SubscribeSessionMessages(ctx context.Context, key string) error {
	f.subscribes = append(f.subscribes, key)
	return f.subscribeErr
}
func (f *fakeHitlGateway) UnsubscribeSessionMessages(ctx context.Context, key string) error {
	f.unsubscribes = append(f.unsubscribes, key)
	return nil
}
func (f *fakeHitlGateway) SendSessionMessage(ctx context.Context, key, message, idempotencyKey string) (string, error) {
	f.sends = append(f.sends, key+"|"+message)
	f.lastIdem = idempotencyKey
	if f.sendBlock != nil {
		select {
		case <-f.sendBlock:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return idempotencyKey, f.sendErr
}
func (f *fakeHitlGateway) AgentWait(ctx context.Context, runID string) error {
	f.waits = append(f.waits, runID)
	return f.waitErr
}
func (f *fakeHitlGateway) CreateSession(ctx context.Context, key string) (ws.SessionState, error) {
	f.creates = append(f.creates, key)
	if f.createErr != nil {
		return ws.SessionState{}, f.createErr
	}
	if f.states == nil {
		f.states = map[string]ws.SessionState{}
	}
	state, ok := f.states[key]
	if !ok {
		state = ws.SessionState{}
		f.states[key] = state
	}
	return state, nil
}
func (f *fakeHitlGateway) PatchSessionSettings(ctx context.Context, key string, patch ws.SessionSettingsPatch) error {
	if f.modelErr != nil {
		return f.modelErr
	}
	if f.guardErr != nil {
		return f.guardErr
	}
	state := f.states[key]
	if patch.Model.Set {
		f.models = append(f.models, key+"|"+patch.Model.Value)
		state.Model = patch.Model.Value
	}
	if patch.PermissionMode.Set {
		if patch.PermissionMode.Value == "guarded" {
			f.guarded = append(f.guarded, key)
		} else {
			f.unguarded = append(f.unguarded, key)
		}
		state.PermissionMode = patch.PermissionMode.Value
	}
	f.states[key] = state
	return nil
}
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
func (f *fakeHitlGateway) ResolveApproval(ctx context.Context, id, decision string) error {
	f.resolves = append(f.resolves, id+"|"+decision)
	return nil
}
func (f *fakeHitlGateway) OnQuestionRequested(cb func(ws.QuestionRecord)) { f.onQuestionRequested = cb }
func (f *fakeHitlGateway) OnQuestionResolved(cb func(ws.QuestionResolved)) {
	f.onQuestionResolved = cb
}
func (f *fakeHitlGateway) ResolveQuestion(ctx context.Context, id string, answers map[string][]string, resolvedBy string) error {
	f.questionResolves = append(f.questionResolves, id+"|"+resolvedBy+"|"+flattenAnswers(answers))
	return f.questionResolveErr
}
func (f *fakeHitlGateway) CancelQuestion(ctx context.Context, id, resolvedBy string) error {
	f.questionCancels = append(f.questionCancels, id+"|"+resolvedBy)
	return f.questionResolveErr
}
func (f *fakeHitlGateway) GetQuestion(ctx context.Context, id string) (*ws.QuestionRecord, error) {
	if f.getQuestionErr != nil {
		return nil, f.getQuestionErr
	}
	if f.questionRecords == nil {
		return nil, fmt.Errorf("question %q not found", id)
	}
	rec, ok := f.questionRecords[id]
	if !ok {
		return nil, fmt.Errorf("question %q not found", id)
	}
	return &rec, nil
}
func (f *fakeHitlGateway) ListQuestions(ctx context.Context) ([]ws.QuestionRecord, error) {
	if f.listQuestionsErr != nil {
		return nil, f.listQuestionsErr
	}
	return f.pendingQuestions, nil
}
func (f *fakeHitlGateway) Close() {}

// flattenAnswers renders an answer map deterministically ("where=workspace,home").
func flattenAnswers(answers map[string][]string) string {
	keys := make([]string, 0, len(answers))
	for k := range answers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+strings.Join(answers[k], ","))
	}
	return strings.Join(parts, ";")
}

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

func TestHitl_PreTurnAppliesAllowlistOncePerRevision(t *testing.T) {
	gw := &fakeHitlGateway{}
	m := newTestHitl(v1alpha1.ConfirmPolicyAllowlist, "rev-1", gw)

	guarded, _ := m.PreTurn(context.Background(), "alice")
	if !guarded {
		t.Fatal("Allowlist policy must require a guarded session")
	}
	// The allowlist is applied exactly once for the revision...
	if len(gw.policySets) != 1 {
		t.Fatalf("policy sets = %d, want 1", len(gw.policySets))
	}
	agent, ok := gw.policySets[0].Agents["main"]
	if !ok || len(agent.Allowlist) == 0 {
		t.Fatalf("allowlist not written to agents.main: %+v", gw.policySets[0])
	}

	// ...without re-applying the allowlist on the next turn.
	_, _ = m.PreTurn(context.Background(), "alice")
	if len(gw.policySets) != 1 {
		t.Errorf("policy sets = %d after second turn, want 1", len(gw.policySets))
	}
	// A policy change applies the allowlist again.
	m.revPol["alice"] = "rev-1-old"
	_, _ = m.PreTurn(context.Background(), "alice")
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
	guarded, _ := m.PreTurn(context.Background(), "alice")
	if !guarded {
		t.Fatal("Allowlist policy must require a guarded session")
	}
	if !gw.connected {
		t.Fatal("expected the gateway to connect after the NOT_PAIRED retry")
	}
}

func TestHitl_PreTurnNoopWithoutAllowlist(t *testing.T) {
	for _, pol := range []v1alpha1.ConfirmPolicy{"", v1alpha1.ConfirmPolicyNone} {
		gw := &fakeHitlGateway{}
		m := newTestHitl(pol, "rev-1", gw)
		guarded, _ := m.PreTurn(context.Background(), "alice")
		if guarded {
			t.Errorf("pol=%q: unexpectedly requested a guarded session", pol)
		}
		if len(gw.guarded) != 0 || len(gw.policySets) != 0 || gw.connected {
			t.Errorf("pol=%q: expected no-op, guarded=%v policySets=%d connected=%v", pol, gw.guarded, len(gw.policySets), gw.connected)
		}
	}
}

func TestHitl_ResolveApprovalMapsDecision(t *testing.T) {
	gw := &fakeHitlGateway{}
	m := newTestHitl(v1alpha1.ConfirmPolicyAllowlist, "rev-1", gw)
	_, _ = m.PreTurn(context.Background(), "alice") // establishes the conn

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
	_, _ = m.PreTurn(context.Background(), "alice")
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
	if _, err := m.PreTurn(context.Background(), "alice"); err == nil {
		t.Fatal("AlwaysAsk PreTurn should fail closed when the policy cannot be applied")
	}
}

// TestHitl_AllowlistFailsClosedOnPolicyError verifies Allowlist, like
// AlwaysAsk, fails closed when the exec policy cannot be applied (issue #127):
// the turn must not run on a session that cannot ask, instead of proceeding
// ungated.
func TestHitl_AllowlistFailsClosedOnPolicyError(t *testing.T) {
	gw := &fakeHitlGateway{setErr: fmt.Errorf("exec.approvals.set: boom")}
	m := newTestHitl(v1alpha1.ConfirmPolicyAllowlist, "rev-1", gw)
	if _, err := m.PreTurn(context.Background(), "alice"); err == nil {
		t.Fatal("Allowlist PreTurn should fail closed when the policy cannot be applied")
	}
	// The failed apply must not advance the revision watermark (retried next turn).
	if m.revPol["alice"] != "" {
		t.Errorf("revPol advanced despite a failed apply: %q", m.revPol["alice"])
	}
}

// TestHitl_GatedPoliciesFailClosedOnChannelDown verifies both Allowlist and
// AlwaysAsk refuse to start a turn when the approval connection cannot be
// established (issue #127).
func TestHitl_GatedPoliciesFailClosedOnChannelDown(t *testing.T) {
	for _, pol := range []v1alpha1.ConfirmPolicy{v1alpha1.ConfirmPolicyAllowlist, v1alpha1.ConfirmPolicyAlwaysAsk} {
		gw := &fakeHitlGateway{connectErr: fmt.Errorf("ws dial: connection refused")}
		m := newTestHitl(pol, "rev-1", gw)
		if _, err := m.PreTurn(context.Background(), "alice"); err == nil {
			t.Errorf("%s: PreTurn should fail closed when the channel is down", pol)
		}
		if len(gw.guarded) != 0 {
			t.Errorf("%s: session guarded despite the failed connect: %v", pol, gw.guarded)
		}
	}
}

// TestHitl_FailsClosedOnPolicyResolutionError verifies PreTurn fails closed when
// the effective policy cannot be resolved: it must not guess None and start a
// turn that might need asking (issue #127).
func TestHitl_FailsClosedOnPolicyResolutionError(t *testing.T) {
	m := newTestHitl(v1alpha1.ConfirmPolicyNone, "", &fakeHitlGateway{})
	m.resolved = func(ctx context.Context, user string) (v1alpha1.ConfirmPolicy, []v1alpha1.AllowlistRule, string, error) {
		return "", nil, "", fmt.Errorf("resolver: boom")
	}
	if _, err := m.PreTurn(context.Background(), "alice"); err == nil {
		t.Fatal("PreTurn should fail closed when the policy cannot be resolved")
	}
}

// TestHitl_ChannelState verifies the approval-channel status the confirm view
// surfaces: up for an established/reachable connection, pairing while the
// supervisor has not yet approved the derived device, and down when the gateway
// cannot be reached (issue #127).
func TestHitl_ChannelState(t *testing.T) {
	t.Run("up when cached connection is live", func(t *testing.T) {
		gw := &fakeHitlGateway{connected: true}
		m := newTestHitl(v1alpha1.ConfirmPolicyAllowlist, "rev-1", gw)
		m.conns["alice"] = &userHitlConn{user: "alice", gw: gw}
		if got := m.channelState(context.Background(), "alice"); got != confirmChannelUp {
			t.Fatalf("channelState = %q, want %q", got, confirmChannelUp)
		}
	})
	t.Run("up when a fresh connect succeeds", func(t *testing.T) {
		gw := &fakeHitlGateway{}
		m := newTestHitl(v1alpha1.ConfirmPolicyAllowlist, "rev-1", gw)
		if got := m.channelState(context.Background(), "alice"); got != confirmChannelUp {
			t.Fatalf("channelState = %q, want %q", got, confirmChannelUp)
		}
	})
	t.Run("pairing while the supervisor approves the device", func(t *testing.T) {
		gw := &fakeHitlGateway{connectErr: fmt.Errorf("NOT_PAIRED: device is not approved yet")}
		m := newTestHitl(v1alpha1.ConfirmPolicyAllowlist, "rev-1", gw)
		if got := m.channelState(context.Background(), "alice"); got != confirmChannelPairing {
			t.Fatalf("channelState = %q, want %q", got, confirmChannelPairing)
		}
	})
	t.Run("down when the gateway is unreachable", func(t *testing.T) {
		gw := &fakeHitlGateway{connectErr: fmt.Errorf("ws dial: connection refused")}
		m := newTestHitl(v1alpha1.ConfirmPolicyAllowlist, "rev-1", gw)
		if got := m.channelState(context.Background(), "alice"); got != confirmChannelDown {
			t.Fatalf("channelState = %q, want %q", got, confirmChannelDown)
		}
	})
}

// TestHitl_AlwaysAskRequiresGuardAndClearsAllowlist verifies AlwaysAsk writes
// an empty allowlist and marks the turn for guarded session reconciliation.
func TestHitl_AlwaysAskRequiresGuardAndClearsAllowlist(t *testing.T) {
	gw := &fakeHitlGateway{initialAllow: []ws.AllowlistEntry{{Pattern: "kubectl", ArgPattern: "^get "}}}
	m := newTestHitl(v1alpha1.ConfirmPolicyAlwaysAsk, "rev-1", gw)

	guarded, _ := m.PreTurn(context.Background(), "alice")
	if !guarded {
		t.Fatal("AlwaysAsk policy must require a guarded session")
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

	_, _ = m.PreTurn(context.Background(), "alice")
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

func TestHitl_RunLiveTurnProjectsTextAndTools(t *testing.T) {
	gw := &fakeHitlGateway{sendBlock: make(chan struct{})}
	m := newTestHitl(v1alpha1.ConfirmPolicyAllowlist, "rev-1", gw)

	var got []openclaw.Event
	done := make(chan error, 1)
	runner := &openClawLiveRunner{manager: m, user: "alice"}
	go func() {
		done <- runner.RunLiveTurn(context.Background(), "conv-1", agentruntime.LiveTurnParams{
			Message: "hi",
			Model:   "provider/model",
		}, func(ev openclaw.Event) error {
			got = append(got, ev)
			return nil
		})
	}()

	// Wait until the send is outstanding (subscribe has happened and the conn
	// event router is installed), then stream a live run: tool start/output and
	// a visible-text delta.
	for i := 0; i < 200 && len(gw.sends) == 0; i++ {
		time.Sleep(time.Millisecond)
	}
	if len(gw.sends) != 1 || gw.sends[0] != "conv-1|hi" {
		t.Fatalf("sends = %v, want [conv-1|hi]", gw.sends)
	}
	if gw.onEvent == nil {
		t.Fatal("conn did not register an OnEvent router")
	}
	if gw.lastIdem == "" {
		t.Fatal("sessions.send did not carry an idempotencyKey")
	}
	run := gw.lastIdem

	gw.onEvent("agent", []byte(`{"sessionKey":"conv-1","runId":"`+run+`","stream":"item","data":{"kind":"tool","phase":"start","name":"exec","title":"exec kubectl get pods","meta":"kubectl get pods","toolCallId":"c1"}}`))
	gw.onEvent("chat", []byte(`{"sessionKey":"conv-1","runId":"`+run+`","state":"delta","deltaText":"正在查询…"}`))
	gw.onEvent("agent", []byte(`{"sessionKey":"conv-1","runId":"`+run+`","stream":"command_output","data":{"phase":"end","toolCallId":"c1","output":"ok","exitCode":0}}`))
	// A foreign run must not leak through.
	gw.onEvent("agent", []byte(`{"sessionKey":"conv-1","runId":"foreign","stream":"item","data":{"kind":"tool","phase":"start","toolCallId":"x"}}`))
	gw.onEvent("agent", []byte(`{"sessionKey":"conv-1","runId":"`+run+`","stream":"lifecycle","data":{"phase":"end"}}`))
	// The projector's terminal frame (chat final) closes the turn.
	gw.onEvent("chat", []byte(`{"sessionKey":"conv-1","runId":"`+run+`","state":"final","deltaText":"ok"}`))

	// Release the send; the run is terminal, so RunLiveTurn should return.
	close(gw.sendBlock)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunLiveTurn error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunLiveTurn did not return after terminal event")
	}

	if len(gw.subscribes) != 1 || gw.subscribes[0] != "conv-1" {
		t.Fatalf("subscribes = %v, want [conv-1]", gw.subscribes)
	}
	if len(gw.models) != 1 || gw.models[0] != "conv-1|provider/model" {
		t.Fatalf("model patches = %v, want [conv-1|provider/model]", gw.models)
	}
	if len(gw.guarded) != 1 || gw.guarded[0] != "conv-1" {
		t.Fatalf("live adapter did not prepare confirmation gating: %v", gw.guarded)
	}
	if len(gw.unsubscribes) != 1 || gw.unsubscribes[0] != "conv-1" {
		t.Fatalf("unsubscribes = %v, want [conv-1]", gw.unsubscribes)
	}
	var hasCall, hasResult, hasDelta bool
	for _, ev := range got {
		switch ev.Type {
		case openclaw.EventToolCall:
			hasCall = ev.CallID == "c1"
		case openclaw.EventToolResult:
			hasResult = ev.CallID == "c1"
		case openclaw.EventMessageDelta:
			hasDelta = ev.Delta == "正在查询…"
		}
	}
	if !hasCall || !hasResult || !hasDelta {
		t.Fatalf("events = %+v, want a tool_call, tool_result and message_delta", got)
	}
}

func TestHitl_RunLiveTurnSendError(t *testing.T) {
	gw := &fakeHitlGateway{sendErr: fmt.Errorf("run failed")}
	m := newTestHitl(v1alpha1.ConfirmPolicyAllowlist, "rev-1", gw)
	if err := m.RunLiveTurn(context.Background(), "alice", "conv-1", "hi", "", false, func(openclaw.Event) error { return nil }); err == nil {
		t.Fatal("RunLiveTurn returned nil, want the send error")
	}
	if len(gw.subscribes) != 1 {
		t.Fatalf("subscribes = %v, want the session subscribed before send", gw.subscribes)
	}
	if len(gw.models) != 0 {
		t.Fatalf("unchanged Runtime Default must not patch the session model, got %v", gw.models)
	}
	if len(gw.unsubscribes) != 1 {
		t.Fatalf("unsubscribes = %v, want cleanup on error", gw.unsubscribes)
	}
	m.liveMu.Lock()
	_, live := m.live["conv-1"]
	m.liveMu.Unlock()
	if live {
		t.Fatal("live turn leaked after send error")
	}
}

func TestHitl_RunLiveTurnModelPatchError(t *testing.T) {
	gw := &fakeHitlGateway{modelErr: fmt.Errorf("model unavailable")}
	m := newTestHitl(v1alpha1.ConfirmPolicyAllowlist, "rev-1", gw)
	err := m.RunLiveTurn(context.Background(), "alice", "conv-1", "hi", "provider/model", true, func(openclaw.Event) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "model unavailable") {
		t.Fatalf("RunLiveTurn error = %v, want model patch failure", err)
	}
	if len(gw.subscribes) != 0 || len(gw.sends) != 0 {
		t.Fatalf("turn started after model patch failure: subscribes=%v sends=%v", gw.subscribes, gw.sends)
	}
}

func TestHitl_RunLiveTurnSurfacesSessionCreateError(t *testing.T) {
	gw := &fakeHitlGateway{createErr: fmt.Errorf("session store unavailable")}
	m := newTestHitl(v1alpha1.ConfirmPolicyNone, "rev-1", gw)
	err := m.RunLiveTurn(context.Background(), "alice", "conv-1", "hi", "", false, func(openclaw.Event) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "session store unavailable") {
		t.Fatalf("RunLiveTurn error = %v, want session creation failure", err)
	}
	if len(gw.models) != 0 || len(gw.subscribes) != 0 || len(gw.sends) != 0 {
		t.Fatalf("turn advanced after create failure: models=%v subscribes=%v sends=%v", gw.models, gw.subscribes, gw.sends)
	}
}

func TestHitl_RunLiveTurnClearsStaleGuardForNonePolicy(t *testing.T) {
	gw := &fakeHitlGateway{
		sendErr: fmt.Errorf("stop after preparation"),
		states: map[string]ws.SessionState{
			"conv-1": {PermissionMode: "guarded"},
		},
	}
	m := newTestHitl(v1alpha1.ConfirmPolicyNone, "rev-1", gw)
	runner := &openClawLiveRunner{manager: m, user: "alice"}
	_ = runner.RunLiveTurn(context.Background(), "conv-1", agentruntime.LiveTurnParams{Message: "hi"}, func(openclaw.Event) error { return nil })
	if len(gw.unguarded) != 1 || gw.unguarded[0] != "conv-1" {
		t.Fatalf("stale guarded state was not cleared: %v", gw.unguarded)
	}
}

func TestHitl_RunLiveTurnSkipsUnchangedSessionSettings(t *testing.T) {
	gw := &fakeHitlGateway{
		sendErr: fmt.Errorf("stop after preparation"),
		states: map[string]ws.SessionState{
			"conv-1": {Model: "provider/model", PermissionMode: "guarded"},
		},
	}
	m := newTestHitl(v1alpha1.ConfirmPolicyAllowlist, "rev-1", gw)
	_ = m.RunLiveTurn(context.Background(), "alice", "conv-1", "hi", "provider/model", true, func(openclaw.Event) error { return nil })
	if len(gw.models) != 0 || len(gw.guarded) != 0 || len(gw.unguarded) != 0 {
		t.Fatalf("unchanged session settings were patched: models=%v guarded=%v unguarded=%v", gw.models, gw.guarded, gw.unguarded)
	}
}

func TestChatTerminalErr(t *testing.T) {
	cases := []struct{ name, payload, want string }{
		{"errorMessage preserved", `{"state":"error","errorMessage":"provider boom","stopReason":"error"}`, "provider boom"},
		{"errorMessage aborted", `{"state":"aborted","errorMessage":"cancelled by user"}`, "cancelled by user"},
		{"error fallback", `{"state":"error","error":"legacy msg"}`, "legacy msg"},
		{"no message default", `{"state":"aborted"}`, "agent run aborted"},
		{"non-terminal state ignored", `{"state":"final"}`, ""},
	}
	for _, c := range cases {
		err := chatTerminalErr([]byte(c.payload))
		got := ""
		if err != nil {
			got = err.Error()
		}
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
