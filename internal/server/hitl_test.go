package server

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/suanova/cubepilot/internal/allowlist"
	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/openclaw"
	"github.com/suanova/cubepilot/internal/openclaw/ws"
	agentruntime "github.com/suanova/cubepilot/internal/runtime"
)

// fakeHitlGateway implements hitlGateway in memory.
//
// mu serialises the recorded state against the goroutine running the code under
// test. Every method that records takes it. A test that inspects the fake while
// a call is still in flight must go through an accessor (the receiver of a
// channel the fake signals orders that read correctly); reading a field
// directly is only valid once the call under test has returned, which is how
// the sequential tests use it.
type fakeHitlGateway struct {
	mu           sync.Mutex
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
	// sendRecorded, when set, is signalled once a send has been recorded (just
	// before sendBlock parks it). A test that drives a turn from another
	// goroutine waits on it instead of polling the fake's fields: the receive
	// is what orders that goroutine against the turn's.
	sendRecorded chan struct{}
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

	// chat.abort / chat.history (issue #166)
	aborts   []string // "sessionKey|runID"; empty runID means the session-scoped form
	abortErr error
	// abortAborted is what chat.abort's success payload reports: false is a
	// successful RPC that stopped nothing (the run id matched no abortable run).
	// Zero value is true, so a fixture that does not set it keeps meaning "the
	// RPC stopped the run".
	abortAborted   *bool
	sessionBuses   []string // sessionKeys passed to chat.history
	sessionBusy    bool
	sessionBusyErr error
	// inFlightRun is the runId chat.history reports as in flight. inFlightActive
	// is the separate "a run is in flight at all" answer: it is what a descriptor
	// that carries no runId produces, and a fixture sets it to model a run that
	// exists but cannot be named.
	inFlightRun    string
	inFlightActive bool
	inFlightRunErr error
	inFlightReads  []string // sessionKeys passed to the in-flight-run read
	// connectCtxDeadline records the bound the last connect ran under, which is
	// how /turn's bounded probe is observed.
	connectCtxDeadline    time.Time
	connectCtxHasDeadline bool
}

func (f *fakeHitlGateway) Connected() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected
}
func (f *fakeHitlGateway) Connect(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connectCtxDeadline, f.connectCtxHasDeadline = ctx.Deadline()
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
func (f *fakeHitlGateway) OnApprovalRequested(cb func(ws.ApprovalRequested)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onRequested = cb
}
func (f *fakeHitlGateway) OnEvent(cb func(evName string, payload []byte)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onEvent = cb
}
func (f *fakeHitlGateway) SubscribeSessionMessages(ctx context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subscribes = append(f.subscribes, key)
	return f.subscribeErr
}
func (f *fakeHitlGateway) UnsubscribeSessionMessages(ctx context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unsubscribes = append(f.unsubscribes, key)
	return nil
}
func (f *fakeHitlGateway) SendSessionMessage(ctx context.Context, key, message, idempotencyKey string) (string, error) {
	f.mu.Lock()
	f.sends = append(f.sends, key+"|"+message)
	f.lastIdem = idempotencyKey
	sendErr := f.sendErr
	block := f.sendBlock
	recorded := f.sendRecorded
	// Release the lock before signalling or parking: a test inspecting the fake
	// while the send is outstanding would otherwise block on mu.
	f.mu.Unlock()

	if recorded != nil {
		select {
		case recorded <- struct{}{}:
		default:
		}
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return idempotencyKey, sendErr
}
func (f *fakeHitlGateway) AgentWait(ctx context.Context, runID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.waits = append(f.waits, runID)
	return f.waitErr
}
func (f *fakeHitlGateway) CreateSession(ctx context.Context, key string) (ws.SessionState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
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
	f.mu.Lock()
	defer f.mu.Unlock()
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
	f.mu.Lock()
	defer f.mu.Unlock()
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
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setErr != nil {
		return nil, f.setErr
	}
	f.policySets = append(f.policySets, file)
	return &ws.ApprovalsSnapshot{}, nil
}
func (f *fakeHitlGateway) ResolveApproval(ctx context.Context, id, decision string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolves = append(f.resolves, id+"|"+decision)
	return nil
}
func (f *fakeHitlGateway) OnQuestionRequested(cb func(ws.QuestionRecord)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onQuestionRequested = cb
}
func (f *fakeHitlGateway) OnQuestionResolved(cb func(ws.QuestionResolved)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onQuestionResolved = cb
}
func (f *fakeHitlGateway) ResolveQuestion(ctx context.Context, id string, answers map[string][]string, resolvedBy string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.questionResolves = append(f.questionResolves, id+"|"+resolvedBy+"|"+flattenAnswers(answers))
	return f.questionResolveErr
}
func (f *fakeHitlGateway) CancelQuestion(ctx context.Context, id, resolvedBy string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.questionCancels = append(f.questionCancels, id+"|"+resolvedBy)
	return f.questionResolveErr
}
func (f *fakeHitlGateway) GetQuestion(ctx context.Context, id string) (*ws.QuestionRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getQuestionErr != nil {
		return nil, f.getQuestionErr
	}
	rec, ok := f.questionRecords[id]
	if !ok {
		return nil, fmt.Errorf("question %q not found", id)
	}
	return &rec, nil
}
func (f *fakeHitlGateway) ListQuestions(ctx context.Context) ([]ws.QuestionRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listQuestionsErr != nil {
		return nil, f.listQuestionsErr
	}
	return f.pendingQuestions, nil
}
func (f *fakeHitlGateway) AbortChat(ctx context.Context, key, runID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.aborts = append(f.aborts, key+"|"+runID)
	// A failed RPC carries no payload, so there is no abort to report -- the real
	// client answers (false, err) there. A fixture that does not set abortAborted
	// means "the RPC aborted the run", so the existing callers keep exercising
	// the aborted=true path.
	if f.abortErr != nil {
		return false, f.abortErr
	}
	if f.abortAborted != nil {
		return *f.abortAborted, nil
	}
	return true, nil
}
func (f *fakeHitlGateway) SessionBusy(ctx context.Context, key string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessionBuses = append(f.sessionBuses, key)
	return f.sessionBusy, f.sessionBusyErr
}
func (f *fakeHitlGateway) SessionInFlightRun(ctx context.Context, key string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inFlightReads = append(f.inFlightReads, key)
	if f.inFlightRunErr != nil {
		return "", false, f.inFlightRunErr
	}
	return f.inFlightRun, f.inFlightActive, nil
}
func (f *fakeHitlGateway) Close() {}

// --- accessors for tests that observe the fake while a call is in flight ---

// sentMessages returns a copy of the recorded sends ("sessionKey|message").
func (f *fakeHitlGateway) sentMessages() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sends...)
}

// subscribedSessions returns a copy of the recorded SubscribeSessionMessages keys.
func (f *fakeHitlGateway) subscribedSessions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.subscribes...)
}

// unsubscribedSessions returns a copy of the recorded UnsubscribeSessionMessages keys.
func (f *fakeHitlGateway) unsubscribedSessions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.unsubscribes...)
}

// idempotencyKey returns the key the last send carried.
func (f *fakeHitlGateway) idempotencyKey() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastIdem
}

// eventSink returns the live-stream router the connection registered, or nil
// before one is installed.
func (f *fakeHitlGateway) eventSink() func(string, []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.onEvent
}

// setConnected flips the connection state, for tests that fake the handshake.
func (f *fakeHitlGateway) setConnected(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connected = v
}

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
	gw := &fakeHitlGateway{sendBlock: make(chan struct{}), sendRecorded: make(chan struct{}, 1)}
	m := newTestHitl(v1alpha1.ConfirmPolicyAllowlist, "rev-1", gw)

	var got []openclaw.Event
	done := make(chan error, 1)
	runner := &openClawLiveRunner{manager: m, user: "alice"}
	go func() {
		_, err := runner.RunLiveTurn(context.Background(), "conv-1", agentruntime.LiveTurnParams{
			Message: "hi",
			Model:   "provider/model",
		}, func(ev openclaw.Event) error {
			got = append(got, ev)
			return nil
		})
		done <- err
	}()

	// Wait for the fake to report the send rather than polling its fields: the
	// receive orders this goroutine against everything the turn did before the
	// send (the subscribe and the OnEvent registration included).
	select {
	case <-gw.sendRecorded:
	case <-time.After(5 * time.Second):
		t.Fatal("sessions.send was never called")
	}
	if sent := gw.sentMessages(); len(sent) != 1 || sent[0] != "conv-1|hi" {
		t.Fatalf("sends = %v, want [conv-1|hi]", sent)
	}
	onEvent := gw.eventSink()
	if onEvent == nil {
		t.Fatal("conn did not register an OnEvent router")
	}
	run := gw.idempotencyKey()
	if run == "" {
		t.Fatal("sessions.send did not carry an idempotencyKey")
	}

	onEvent("agent", []byte(`{"sessionKey":"conv-1","runId":"`+run+`","stream":"item","data":{"kind":"tool","phase":"start","name":"exec","title":"exec kubectl get pods","meta":"kubectl get pods","toolCallId":"c1"}}`))
	onEvent("chat", []byte(`{"sessionKey":"conv-1","runId":"`+run+`","state":"delta","deltaText":"正在查询…"}`))
	onEvent("agent", []byte(`{"sessionKey":"conv-1","runId":"`+run+`","stream":"command_output","data":{"phase":"end","toolCallId":"c1","output":"ok","exitCode":0}}`))
	// A foreign run must not leak through.
	onEvent("agent", []byte(`{"sessionKey":"conv-1","runId":"foreign","stream":"item","data":{"kind":"tool","phase":"start","toolCallId":"x"}}`))
	onEvent("agent", []byte(`{"sessionKey":"conv-1","runId":"`+run+`","stream":"lifecycle","data":{"phase":"end"}}`))
	// The projector's terminal frame (chat final) closes the turn.
	onEvent("chat", []byte(`{"sessionKey":"conv-1","runId":"`+run+`","state":"final","deltaText":"ok"}`))

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
	if _, err := m.RunLiveTurn(context.Background(), "alice", "conv-1", "hi", "", false, func(openclaw.Event) error { return nil }); err == nil {
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
	_, err := m.RunLiveTurn(context.Background(), "alice", "conv-1", "hi", "provider/model", true, func(openclaw.Event) error { return nil })
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
	_, err := m.RunLiveTurn(context.Background(), "alice", "conv-1", "hi", "", false, func(openclaw.Event) error { return nil })
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
	_, _ = runner.RunLiveTurn(context.Background(), "conv-1", agentruntime.LiveTurnParams{Message: "hi"}, func(openclaw.Event) error { return nil })
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
	_, _ = m.RunLiveTurn(context.Background(), "alice", "conv-1", "hi", "provider/model", true, func(openclaw.Event) error { return nil })
	if len(gw.models) != 0 || len(gw.guarded) != 0 || len(gw.unguarded) != 0 {
		t.Fatalf("unchanged session settings were patched: models=%v guarded=%v unguarded=%v", gw.models, gw.guarded, gw.unguarded)
	}
}

func TestChatTerminalOutcome(t *testing.T) {
	cases := []struct {
		name        string
		payload     string
		wantErr     string
		wantStopped bool
	}{
		{"rpc stop is an outcome", `{"state":"aborted","stopReason":"rpc"}`, "", true},
		{"slash stop is an outcome", `{"state":"aborted","stopReason":"stop"}`, "", true},
		{"stop keeps no error text", `{"state":"error","stopReason":"rpc","errorMessage":"ignored"}`, "", true},
		{"timeout abort stays an error", `{"state":"aborted","stopReason":"timeout","errorMessage":"cancelled by user"}`, "cancelled by user", false},
		{"error state stays an error", `{"state":"error","errorMessage":"provider boom"}`, "provider boom", false},
		{"errorMessage aborted", `{"state":"aborted","errorMessage":"cancelled by user"}`, "cancelled by user", false},
		{"error field is the fallback", `{"state":"error","error":"legacy msg"}`, "legacy msg", false},
		{"no diagnostic gets a default", `{"state":"aborted"}`, "agent run aborted", false},
		{"non-terminal state is ignored", `{"state":"final"}`, "", false},
	}
	for _, c := range cases {
		err, stopped := chatTerminalOutcome([]byte(c.payload))
		got := ""
		if err != nil {
			got = err.Error()
		}
		if got != c.wantErr || stopped != c.wantStopped {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", c.name, got, stopped, c.wantErr, c.wantStopped)
		}
	}
}

// TestLiveTurnOutcomeIsOneGuardedRead pins the accessor contract that makes the
// agent.wait tail path safe: the outcome and the terminal error come back
// together, so a stopped turn can never be reported as an unqualified
// completion.
func TestLiveTurnOutcomeIsOneGuardedRead(t *testing.T) {
	stopped := &liveTurn{done: make(chan struct{})}
	stopped.finishWith(nil, true)
	if outcome, err := stopped.outcome(); !outcome.Stopped || err != nil {
		t.Fatalf("stopped turn = (%+v, %v), want (Stopped:true, nil)", outcome, err)
	}

	failed := &liveTurn{done: make(chan struct{})}
	failed.finishWith(fmt.Errorf("boom"), false)
	outcome, err := failed.outcome()
	if outcome.Stopped || err == nil || err.Error() != "boom" {
		t.Fatalf("failed turn = (%+v, %v), want (Stopped:false, boom)", outcome, err)
	}
}

// runLiveTurnToTerminalFrame drives one turn through the real call path
// (openClawLiveRunner -> hitlManager.RunLiveTurn -> routeLive) and feeds it a
// single terminal chat frame before releasing the send, returning what
// RunLiveTurn reported to its caller: the observable result the SSE handler
// turns into the terminal message_done.
func runLiveTurnToTerminalFrame(t *testing.T, frame string) (agentruntime.TurnOutcome, error) {
	t.Helper()
	gw := &fakeHitlGateway{sendBlock: make(chan struct{}), sendRecorded: make(chan struct{}, 1)}
	m := newTestHitl(v1alpha1.ConfirmPolicyAllowlist, "rev-1", gw)

	type result struct {
		outcome agentruntime.TurnOutcome
		err     error
	}
	res := make(chan result, 1)
	runner := &openClawLiveRunner{manager: m, user: "alice"}
	go func() {
		outcome, err := runner.RunLiveTurn(context.Background(), "conv-1", agentruntime.LiveTurnParams{Message: "hi"}, func(openclaw.Event) error { return nil })
		res <- result{outcome, err}
	}()

	// Wait for the fake to report the send rather than polling its fields: the
	// receive orders this goroutine against everything the turn did before the
	// send (the subscribe and the OnEvent registration included).
	select {
	case <-gw.sendRecorded:
	case <-time.After(5 * time.Second):
		t.Fatal("sessions.send was never called")
	}
	onEvent := gw.eventSink()
	run := gw.idempotencyKey()
	if onEvent == nil || run == "" {
		t.Fatal("turn did not register an event router for its run id")
	}
	onEvent("chat", []byte(`{"sessionKey":"conv-1","runId":"`+run+`",`+frame+`}`))
	// Release the send only once the turn is already terminal, so RunLiveTurn
	// takes its terminal path (not the agent.wait tail) and reports the outcome
	// the frame produced.
	close(gw.sendBlock)

	select {
	case r := <-res:
		return r.outcome, r.err
	case <-time.After(5 * time.Second):
		t.Fatal("RunLiveTurn did not return after the terminal frame")
		return agentruntime.TurnOutcome{}, nil
	}
}

// TestRunLiveTurnReportsRequestStoppedTurn closes the path from a terminal
// frame to the caller's result for a request-initiated stop: the outcome must
// report stopped, with no error. Without this, routeLive could classify a stop
// as an ordinary completion and every turn would still look fine.
func TestRunLiveTurnReportsRequestStoppedTurn(t *testing.T) {
	for _, stopReason := range []string{"rpc", "stop"} {
		t.Run(stopReason, func(t *testing.T) {
			outcome, err := runLiveTurnToTerminalFrame(t, `"state":"aborted","stopReason":"`+stopReason+`"`)
			if err != nil {
				t.Fatalf("RunLiveTurn error = %v, want nil for a request-initiated stop", err)
			}
			if !outcome.Stopped {
				t.Fatalf("stopReason=%q frame reported Stopped=false: a stopped turn is indistinguishable from a completed one", stopReason)
			}
		})
	}
}

// TestRunLiveTurnReportsNonRequestAbortAsError closes the other half: an abort
// the user did not ask for stays a failure, so the browser is told the turn
// failed rather than that it was stopped.
func TestRunLiveTurnReportsNonRequestAbortAsError(t *testing.T) {
	outcome, err := runLiveTurnToTerminalFrame(t, `"state":"aborted","stopReason":"timeout","errorMessage":"run timed out"`)
	if err == nil {
		t.Fatal("RunLiveTurn error = nil for a timeout abort, want a failure")
	}
	if !strings.Contains(err.Error(), "run timed out") {
		t.Fatalf("RunLiveTurn error = %v, want the frame's diagnostic", err)
	}
	if outcome.Stopped {
		t.Fatal("RunLiveTurn reported Stopped=true for a non-request abort")
	}
}

// TestHitl_LiveRunID tracks the run id the server believes is live for a
// session: absent while no turn is registered, then present from the moment the
// turn registers.
//
// The id is installed *before* RunLiveTurn subscribes and sends (hitl.go:691-699
// calls setRunID right after registerLive), and the gateway's client run id is
// the send's idempotency key. So the id-less window is those two statements
// wide, with no I/O between them -- it is not a pre-ACK window stretching across
// the subscribe/send round trip. An empty LiveRunID therefore means no turn is
// registered at all -- which is why /abort only treats it as a miss to be
// resolved elsewhere, never as a reason to stop aborting.
//
// The lookup is scoped to the requesting user: m.live is indexed by session key
// alone, and a session key can be client-supplied, so an unscoped read would let
// one user's /abort pick up another user's run id. routeLive applies the same
// ownership rule when it routes.
func TestHitl_LiveRunID(t *testing.T) {
	m := newTestHitl(v1alpha1.ConfirmPolicyNone, "", &fakeHitlGateway{})
	if id, ok := m.LiveRunID("alice", "conv-1"); ok || id != "" {
		t.Fatalf("LiveRunID with no turn = (%q, %v), want empty", id, ok)
	}

	turn := m.registerLive("alice", "conv-1", nil)
	if id, ok := m.LiveRunID("alice", "conv-1"); ok || id != "" {
		t.Fatalf("LiveRunID in the registerLive..setRunID gap = (%q, %v), want empty", id, ok)
	}
	turn.setRunID("run-7")
	if id, ok := m.LiveRunID("alice", "conv-1"); !ok || id != "run-7" {
		t.Fatalf("LiveRunID = (%q, %v), want run-7", id, ok)
	}

	// Another user asking about the same session key must not see alice's run
	// id: reading it is what would let their /abort kill her run.
	if id, ok := m.LiveRunID("bob", "conv-1"); ok || id != "" {
		t.Fatalf("LiveRunID for another user = (%q, %v), want empty", id, ok)
	}
	// The owner still reads it, so the check rejects only outsiders.
	if id, ok := m.LiveRunID("alice", "conv-1"); !ok || id != "run-7" {
		t.Fatalf("LiveRunID for the owner after another user read it = (%q, %v), want run-7", id, ok)
	}
}

// TestHitl_AbortDelegatesToGateway pins the two things Task 5 depends on: the
// run id the server holds reaches the gateway, and the empty-runID fallback
// stays empty rather than being filled with something invented.
func TestHitl_AbortDelegatesToGateway(t *testing.T) {
	gw := &fakeHitlGateway{connected: true}
	m := newTestHitl(v1alpha1.ConfirmPolicyNone, "", gw)
	m.conns["alice"] = &userHitlConn{user: "alice", gw: gw}

	if aborted, err := m.Abort(context.Background(), "alice", "conv-1", "run-7"); err != nil || !aborted {
		t.Fatalf("Abort = (%v, %v), want (true, nil)", aborted, err)
	}
	if aborted, err := m.Abort(context.Background(), "alice", "conv-1", ""); err != nil || !aborted {
		t.Fatalf("Abort without a run id = (%v, %v), want (true, nil)", aborted, err)
	}
	want := []string{"conv-1|run-7", "conv-1|"}
	if len(gw.aborts) != 2 || gw.aborts[0] != want[0] || gw.aborts[1] != want[1] {
		t.Fatalf("aborts = %v, want %v", gw.aborts, want)
	}
}

// TestHitl_AbortReportsAnAbortThatStoppedNothing: the gateway can answer ok with
// aborted=false -- the run id matched no abortable run. That must reach the
// caller as aborted=false, because it is the one thing /abort cannot read as a
// stop: settling on it deletes the records of a run that is still going.
func TestHitl_AbortReportsAnAbortThatStoppedNothing(t *testing.T) {
	no := false
	gw := &fakeHitlGateway{connected: true, abortAborted: &no}
	m := newTestHitl(v1alpha1.ConfirmPolicyNone, "", gw)
	m.conns["alice"] = &userHitlConn{user: "alice", gw: gw}

	aborted, err := m.Abort(context.Background(), "alice", "conv-1", "run-7")
	if err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if aborted {
		t.Fatal("Abort reported a stop for a gateway that answered aborted=false")
	}
}

func TestHitl_AbortRequiresAChannel(t *testing.T) {
	m := newTestHitl(v1alpha1.ConfirmPolicyNone, "", &fakeHitlGateway{})
	if aborted, err := m.Abort(context.Background(), "alice", "conv-1", "run-7"); err == nil || aborted {
		t.Fatal("Abort without a live gateway channel must fail, and must not claim a stop")
	}
}

// TestHitl_AbortRejectsUnconnectedChannel: liveConn reports ok=false for two
// distinct reasons -- there is no entry, and there is an entry whose gateway is
// not connected. Only the first is covered by TestHitl_AbortRequiresAChannel.
// A connection registered before its handshake completes must fail here rather
// than be handed to the gateway, which would surface as a confusing
// "ws: not connected" from inside Client.Call.
func TestHitl_AbortRejectsUnconnectedChannel(t *testing.T) {
	gw := &fakeHitlGateway{} // connected defaults to false
	m := newTestHitl(v1alpha1.ConfirmPolicyNone, "", gw)
	m.conns["alice"] = &userHitlConn{user: "alice", gw: gw}

	if aborted, err := m.Abort(context.Background(), "alice", "conv-1", "run-7"); err == nil || aborted {
		t.Fatal("Abort with a stored but unconnected gateway must fail, and must not claim a stop")
	}
	if len(gw.aborts) != 0 {
		t.Fatalf("aborts = %v, want the gateway untouched", gw.aborts)
	}
}

// TestHitl_AbortPropagatesGatewayError: an error from the gateway means the
// abort's fate is unknown. It must reach the caller rather than be swallowed
// into a nil success.
func TestHitl_AbortPropagatesGatewayError(t *testing.T) {
	wantErr := errors.New("gateway down")
	gw := &fakeHitlGateway{connected: true, abortErr: wantErr}
	m := newTestHitl(v1alpha1.ConfirmPolicyNone, "", gw)
	m.conns["alice"] = &userHitlConn{user: "alice", gw: gw}

	if aborted, err := m.Abort(context.Background(), "alice", "conv-1", "run-7"); !errors.Is(err, wantErr) || aborted {
		t.Fatalf("Abort = (%v, %v), want (false, %v)", aborted, err, wantErr)
	}
	if len(gw.aborts) != 1 {
		t.Fatalf("aborts = %v, want the call attempted once", gw.aborts)
	}
}

// TestHitl_SessionBusyDelegatesToGateway: the manager must report the gateway's
// answer untouched, since it is the only busy signal that survives a reload.
func TestHitl_SessionBusyDelegatesToGateway(t *testing.T) {
	gw := &fakeHitlGateway{connected: true, sessionBusy: true}
	m := newTestHitl(v1alpha1.ConfirmPolicyNone, "", gw)
	m.conns["alice"] = &userHitlConn{user: "alice", gw: gw}

	busy, err := m.SessionBusy(context.Background(), "alice", "conv-1")
	if err != nil {
		t.Fatalf("SessionBusy: %v", err)
	}
	if !busy {
		t.Fatal("SessionBusy = false, want the gateway's true")
	}
	if len(gw.sessionBuses) != 1 || gw.sessionBuses[0] != "conv-1" {
		t.Fatalf("sessionBuses = %v, want [conv-1]", gw.sessionBuses)
	}
}

func TestHitl_SessionBusyRequiresAChannel(t *testing.T) {
	m := newTestHitl(v1alpha1.ConfirmPolicyNone, "", &fakeHitlGateway{})
	if _, err := m.SessionBusy(context.Background(), "alice", "conv-1"); err == nil {
		t.Fatal("SessionBusy without a live gateway channel must fail")
	}
}

// TestHitl_SessionBusyRejectsUnconnectedChannel covers the second reason
// liveConn returns ok=false: an entry exists but its gateway has not finished
// connecting. The gateway must not be probed.
func TestHitl_SessionBusyRejectsUnconnectedChannel(t *testing.T) {
	gw := &fakeHitlGateway{} // connected defaults to false
	m := newTestHitl(v1alpha1.ConfirmPolicyNone, "", gw)
	m.conns["alice"] = &userHitlConn{user: "alice", gw: gw}

	if _, err := m.SessionBusy(context.Background(), "alice", "conv-1"); err == nil {
		t.Fatal("SessionBusy with a stored but unconnected gateway must fail")
	}
	if len(gw.sessionBuses) != 0 {
		t.Fatalf("sessionBuses = %v, want the gateway untouched", gw.sessionBuses)
	}
}

// TestHitl_SessionBusyEstablishedDialsBounded: with no channel the read
// establishes one, and the dial is bounded by channelProbeTimeout. Inheriting
// conn()'s 30s NOT_PAIRED pairing budget would let one /turn hold a Portal
// refresh open for half a minute, and the caller's "could not check" would
// arrive long after the user gave up.
func TestHitl_SessionBusyEstablishedDialsBounded(t *testing.T) {
	gw := &fakeHitlGateway{sessionBusy: true}
	m := newTestHitl(v1alpha1.ConfirmPolicyNone, "", gw)

	busy, err := m.SessionBusyEstablished(context.Background(), "alice", "conv-1")
	if err != nil {
		t.Fatalf("SessionBusyEstablished: %v", err)
	}
	if !busy {
		t.Fatal("SessionBusyEstablished = false, want the gateway's true")
	}
	if !gw.connectCtxHasDeadline {
		t.Fatal("the establishing dial ran on a context with no deadline: a hung connect would hold the read open indefinitely")
	}
	if d := time.Until(gw.connectCtxDeadline); d <= 0 || d > channelProbeTimeout+time.Second {
		t.Fatalf("dial deadline = now+%v, want (0, %v]", d, channelProbeTimeout)
	}
	// The channel is kept, not probed and dropped: the Stop the answer offers is
	// issued over it.
	if _, ok := m.liveConn("alice"); !ok {
		t.Fatal("the established channel was not kept: the Stop offered over it would fail with no channel")
	}
}

// TestHitl_SessionBusyEstablishedKeepsLiveChannel: an existing channel is used
// as it is. Re-dialling on every status read would churn the connection (and
// possibly the device pairing) that the running turn is observed over.
func TestHitl_SessionBusyEstablishedKeepsLiveChannel(t *testing.T) {
	gw := &fakeHitlGateway{connected: true, sessionBusy: true}
	m := newTestHitl(v1alpha1.ConfirmPolicyNone, "", gw)
	m.conns["alice"] = &userHitlConn{user: "alice", gw: gw, connected: true}

	if _, err := m.SessionBusyEstablished(context.Background(), "alice", "conv-1"); err != nil {
		t.Fatalf("SessionBusyEstablished: %v", err)
	}
	if gw.connectCtxHasDeadline {
		t.Fatal("the read dialled although a live channel existed")
	}
}

// TestHitl_SessionBusyEstablishedDoesNotWaitOutAnotherDial: a second connect for
// the same user waits for the first, which can be inside a dial (or the pairing
// retry) for up to its budget. The wait has to be context-aware too, or the
// bound above is nominal -- the probe would sit behind that dial and only then
// start its own.
func TestHitl_SessionBusyEstablishedDoesNotWaitOutAnotherDial(t *testing.T) {
	blocking := &blockingHitlGateway{entered: make(chan struct{}), release: make(chan struct{})}
	m := newTestHitl(v1alpha1.ConfirmPolicyNone, "", &blocking.fakeHitlGateway)
	m.newClient = func(string, *ws.Device) hitlGateway { return blocking }

	first := make(chan struct{})
	go func() {
		_, _ = m.conn(context.Background(), "alice")
		close(first)
	}()
	<-blocking.entered

	start := time.Now()
	if _, err := m.SessionBusyEstablished(context.Background(), "alice", "conv-1"); err == nil {
		t.Fatal("SessionBusyEstablished succeeded while the only channel was still dialling")
	}
	if d := time.Since(start); d > channelProbeTimeout+time.Second {
		t.Fatalf("the probe waited %v for another dial, want it bounded by channelProbeTimeout", d)
	}

	close(blocking.release)
	<-first
}

// TestHitl_InFlightRunIDDelegatesAndRequiresAChannel: the run an abort is scoped
// to comes from the gateway, and a missing channel must be an error rather than
// an empty id -- which is a different claim from "could not ask".
//
// active comes back as its own answer, and the three states are pinned here
// because the caller branches on them: idle (no run at all) is an idempotent
// no-op, a named run is aborted, and a run that is active but unnamed is a
// failure. A fixture whose descriptor carries no run id is exactly that last
// state, and it must not arrive as idle.
func TestHitl_InFlightRunIDDelegatesAndRequiresAChannel(t *testing.T) {
	gw := &fakeHitlGateway{connected: true, inFlightRun: "run-7", inFlightActive: true}
	m := newTestHitl(v1alpha1.ConfirmPolicyNone, "", gw)
	m.conns["alice"] = &userHitlConn{user: "alice", gw: gw}

	id, active, err := m.InFlightRunID(context.Background(), "alice", "conv-1")
	if err != nil {
		t.Fatalf("InFlightRunID: %v", err)
	}
	if id != "run-7" || !active {
		t.Fatalf("InFlightRunID = (%q, %v), want (run-7, true)", id, active)
	}

	unnamed := &fakeHitlGateway{connected: true, inFlightActive: true}
	unnamedM := newTestHitl(v1alpha1.ConfirmPolicyNone, "", unnamed)
	unnamedM.conns["alice"] = &userHitlConn{user: "alice", gw: unnamed}
	id, active, err = unnamedM.InFlightRunID(context.Background(), "alice", "conv-1")
	if err != nil {
		t.Fatalf("InFlightRunID on an unnamed run: %v", err)
	}
	if id != "" || !active {
		t.Fatalf("InFlightRunID on an unnamed run = (%q, %v), want (\"\", true): a run with no id is running, not idle", id, active)
	}

	noChannel := newTestHitl(v1alpha1.ConfirmPolicyNone, "", &fakeHitlGateway{})
	if _, _, err := noChannel.InFlightRunID(context.Background(), "alice", "conv-1"); !errors.Is(err, errNoGatewayChannel) {
		t.Fatalf("InFlightRunID without a channel = %v, want errNoGatewayChannel", err)
	}
}

// TestHitl_SessionBusyDoesNotSwallowError pins the contract the reload-takeover
// path depends on: a failed probe means "cannot determine", never "not busy". A
// (false, nil) here would strand a turn that is still running.
func TestHitl_SessionBusyDoesNotSwallowError(t *testing.T) {
	wantErr := errors.New("chat.history failed")
	gw := &fakeHitlGateway{connected: true, sessionBusyErr: wantErr}
	m := newTestHitl(v1alpha1.ConfirmPolicyNone, "", gw)
	m.conns["alice"] = &userHitlConn{user: "alice", gw: gw}

	busy, err := m.SessionBusy(context.Background(), "alice", "conv-1")
	if !errors.Is(err, wantErr) {
		t.Fatalf("SessionBusy error = %v, want %v", err, wantErr)
	}
	if busy {
		t.Fatal("SessionBusy = true alongside an error")
	}
}

// blockingHitlGateway is a gateway whose handshake parks until the test releases
// it, so the state in the middle of a dial can be observed. That window is the
// one the Portal used to read as "cannot determine": conn registers the entry
// before dialling, and the NOT_PAIRED pairing retry can hold it there for up to
// 30 seconds.
type blockingHitlGateway struct {
	fakeHitlGateway
	entered chan struct{}
	release chan struct{}
}

func (f *blockingHitlGateway) Connect(ctx context.Context) error {
	close(f.entered)
	<-f.release
	f.mu.Lock()
	f.connected = true
	f.mu.Unlock()
	return nil
}

// TestHitlGatewayConnectedNeedsASuccessfulHandshake pins the flag /turn's
// classification reads: it must be false for the whole of a dial that has not
// finished -- an idle session then answers idle instead of raising the
// cannot-check banner -- and true afterwards, including after the entry has been
// replaced by a re-dial, because a turn started over the old connection can
// still be running.
func TestHitlGatewayConnectedNeedsASuccessfulHandshake(t *testing.T) {
	gw := &blockingHitlGateway{entered: make(chan struct{}), release: make(chan struct{})}
	m := newTestHitl(v1alpha1.ConfirmPolicyNone, "", &gw.fakeHitlGateway)
	m.newClient = func(url string, dev *ws.Device) hitlGateway { return gw }

	done := make(chan error, 1)
	go func() {
		_, err := m.conn(context.Background(), "alice")
		done <- err
	}()

	<-gw.entered
	// The entry is registered (that is how the dial is tracked) and its gateway
	// is not connected yet: registered must not read as a channel.
	m.mu.Lock()
	entry := m.conns["alice"]
	m.mu.Unlock()
	if entry == nil {
		t.Fatal("no entry is registered during the dial; this test is not observing the state it claims to")
	}
	if m.gatewayConnected("alice") {
		t.Fatal("gatewayConnected = true while the handshake is still in flight: /turn would answer 502 and the Portal would flash its cannot-check banner over an idle session")
	}

	close(gw.release)
	if err := <-done; err != nil {
		t.Fatalf("conn: %v", err)
	}
	if !m.gatewayConnected("alice") {
		t.Fatal("gatewayConnected = false after a successful handshake")
	}

	// The connection then drops and is re-dialled: the replacement entry starts
	// out unmarked, and must inherit the fact that this process once had a
	// channel.
	gw.mu.Lock()
	gw.connected = false
	gw.mu.Unlock()
	gw.release = make(chan struct{})
	gw.entered = make(chan struct{})
	done2 := make(chan error, 1)
	go func() {
		_, err := m.conn(context.Background(), "alice")
		done2 <- err
	}()
	<-gw.entered
	if !m.gatewayConnected("alice") {
		t.Fatal("gatewayConnected = false during a re-dial after a drop: a turn started over the old connection may still be running")
	}
	close(gw.release)
	if err := <-done2; err != nil {
		t.Fatalf("re-dial: %v", err)
	}
}
