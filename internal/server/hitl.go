package server

import (
	"context"
	"crypto/ed25519"
	"crypto/sha512"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/instances"
	"github.com/suanova/cubepilot/internal/openclaw/ws"
	agentruntime "github.com/suanova/cubepilot/internal/runtime"
)

// hitlPairRetryDelay is the pause between NOT_PAIRED connect retries while the
// in-pod supervisor approves the device pairing (overridable in tests).
var hitlPairRetryDelay = 1500 * time.Millisecond

// channelProbeTimeout bounds a single channelState connect attempt: it only
// needs to learn whether the gateway is reachable and the derived device is
// paired, so it skips conn()'s 30s pairing retry budget.
const channelProbeTimeout = 3 * time.Second

// hitlGateway is the subset of the gateway-protocol WS client the HITL glue
// depends on, so tests can substitute a fake.
type hitlGateway interface {
	Connected() bool
	Connect(ctx context.Context) error
	OnApprovalRequested(f func(ws.ApprovalRequested))
	OnQuestionRequested(f func(ws.QuestionRecord))
	OnQuestionResolved(f func(ws.QuestionResolved))
	OnEvent(f func(evName string, payload []byte))
	SubscribeSessionMessages(ctx context.Context, sessionKey string) error
	UnsubscribeSessionMessages(ctx context.Context, sessionKey string) error
	SendSessionMessage(ctx context.Context, sessionKey, message, idempotencyKey string) (runID string, err error)
	AgentWait(ctx context.Context, runID string) error
	CreateSession(ctx context.Context, sessionKey string) (ws.SessionState, error)
	PatchSessionSettings(ctx context.Context, sessionKey string, patch ws.SessionSettingsPatch) error
	GetApprovalsPolicy(ctx context.Context) (*ws.ApprovalsSnapshot, error)
	SetApprovalsPolicy(ctx context.Context, file ws.ApprovalsFile, baseHash string) (*ws.ApprovalsSnapshot, error)
	ResolveApproval(ctx context.Context, id, decision string) error
	ResolveQuestion(ctx context.Context, id string, answers map[string][]string, resolvedBy string) error
	CancelQuestion(ctx context.Context, id, resolvedBy string) error
	GetQuestion(ctx context.Context, id string) (*ws.QuestionRecord, error)
	ListQuestions(ctx context.Context) ([]ws.QuestionRecord, error)
	AbortChat(ctx context.Context, sessionKey, runID string) error
	SessionBusy(ctx context.Context, sessionKey string) (bool, error)
	Close()
}

// openClawLiveRunner adapts the user-scoped WS/HITL manager to CubePilot's
// runtime-neutral interactive-turn contract. PreTurn is deliberately inside
// this adapter: confirmation setup is part of running an interactive turn, not
// a responsibility every HTTP handler or future runtime must know about.
type openClawLiveRunner struct {
	manager *hitlManager
	user    string
}

var _ agentruntime.LiveTurnRunner = (*openClawLiveRunner)(nil)

func (r *openClawLiveRunner) RunLiveTurn(ctx context.Context, sessionKey string, params agentruntime.LiveTurnParams, emit func(agentruntime.Event) error) (agentruntime.TurnOutcome, error) {
	if r.manager == nil {
		return agentruntime.TurnOutcome{}, fmt.Errorf("live chat unavailable: runtime live channel is not configured")
	}
	guarded, err := r.manager.PreTurn(ctx, r.user)
	if err != nil {
		return agentruntime.TurnOutcome{}, fmt.Errorf("confirmation gating failed: %w", err)
	}
	return r.manager.RunLiveTurn(ctx, r.user, sessionKey, params.Message, params.Model, guarded, emit)
}

// hitlManager owns the per-user approval connections (issue #20). It is inert
// until the API is configured with a device master key (see ConfiguredHITL);
// with no device, confirmPolicy stays declarative and chat is unchanged.
type hitlManager struct {
	mgr       *instances.Manager
	token     string
	masterKey []byte
	newClient func(url string, dev *ws.Device) hitlGateway
	logf      func(format string, args ...any)

	// bridge is set by the server so a gateway approval can reach the
	// ApprovalService (which resolves Portal decisions and injects SSE).
	bridge func(user string, ev ws.ApprovalRequested)

	// questionRequested / questionResolved are set by the server so gateway
	// question broadcasts reach the parked turn's SSE stream (issue #161).
	questionRequested func(user string, rec ws.QuestionRecord)
	questionResolved  func(user string, res ws.QuestionResolved)

	// resolved returns the user's confirm policy, effective allowlist and
	// config revision. Overridable in tests; the default reads the resolved
	// config via the instance manager.
	resolved func(ctx context.Context, user string) (v1alpha1.ConfirmPolicy, []v1alpha1.AllowlistRule, string, error)

	// wsURLOf returns the gateway WS endpoint for a user. Overridable in tests.
	wsURLOf func(user string) string

	mu         sync.Mutex
	conns      map[string]*userHitlConn
	connecting map[string]*sync.Mutex // serializes first connect per user
	revPol     map[string]string      // user -> applied policy revision

	liveMu sync.Mutex
	live   map[string]*liveTurn // sessionKey -> active live-tool turn (issue #130)
}

// liveTurn is one chat turn driving and observing a session over its live
// message stream (issue #130 WS-only chat). The projector folds the gateway's
// agent (tool/lifecycle) and chat (text) frames into SSE events that sink
// writes to the browser; when the run goes terminal, done is closed so the
// RunLiveTurn caller knows the turn is over.
type liveTurn struct {
	user string
	sink func(agentruntime.Event) error
	proj *liveProjector

	runMu sync.Mutex
	runID string // runId of the turn's run; set from the send ACK, filters events

	done chan struct{}
	once sync.Once
	// doneErr and doneStopped are written together inside once.Do and read only
	// through outcome(), which returns both under resMu so no caller can observe
	// a torn pair (a stopped turn that looks completed, or vice versa).
	resMu       sync.Mutex
	doneErr     error
	doneStopped bool
}

// setRunID records the run this turn is projecting (from sessions.send's ACK).
func (t *liveTurn) setRunID(id string) {
	t.runMu.Lock()
	t.runID = id
	t.runMu.Unlock()
}

// acceptRun reports whether an event whose payload runId belongs to this turn.
// Until the ACK arrives runID is unknown, so everything is accepted (the window
// is before the gateway starts producing content); afterwards only that run's
// events project, so a parallel/older run on the same session cannot leak into
// this SSE or terminate this turn.
func (t *liveTurn) acceptRun(id string) bool {
	t.runMu.Lock()
	defer t.runMu.Unlock()
	// Until an expected run is installed everything is accepted (nothing has been
	// produced yet); afterwards only that run's frames project -- a missing or
	// foreign runId on run-scoped content is dropped.
	return t.runID == "" || (id != "" && t.runID == id)
}

// finish marks the turn terminal as neither stopped nor failed (idempotent).
func (t *liveTurn) finish(err error) {
	t.finishWith(err, false)
}

// finishWith marks the turn terminal with its outcome (idempotent). stopped
// records that the terminal frame was a request-initiated abort.
func (t *liveTurn) finishWith(err error, stopped bool) {
	t.once.Do(func() {
		t.resMu.Lock()
		t.doneErr = err
		t.doneStopped = stopped
		t.resMu.Unlock()
		close(t.done)
	})
}

// outcome reports how the turn ended -- both the outcome and the terminal
// error -- from a single guarded read, so the agent.wait tail path (where
// t.done never closed) can never see the two halves inconsistently and report
// a stopped turn as (Stopped:false, err:nil), i.e. a plain completion. Safe to
// call after terminal.
func (t *liveTurn) outcome() (agentruntime.TurnOutcome, error) {
	t.resMu.Lock()
	defer t.resMu.Unlock()
	return agentruntime.TurnOutcome{Stopped: t.doneStopped}, t.doneErr
}

type userHitlConn struct {
	user string
	gw   hitlGateway
}

// ConfiguredHITL builds the manager, or returns nil (HITL disabled) when no
// master key is provided.
func ConfiguredHITL(mgr *instances.Manager, token string, masterKey []byte, logf func(string, ...any)) *hitlManager {
	if mgr == nil || len(masterKey) == 0 || token == "" {
		return nil
	}
	m := &hitlManager{
		mgr:        mgr,
		token:      token,
		masterKey:  masterKey,
		logf:       logf,
		conns:      map[string]*userHitlConn{},
		connecting: map[string]*sync.Mutex{},
		revPol:     map[string]string{},
		live:       map[string]*liveTurn{},
	}
	m.newClient = func(url string, dev *ws.Device) hitlGateway {
		return ws.NewClient(url, token, dev)
	}
	m.resolved = func(ctx context.Context, user string) (v1alpha1.ConfirmPolicy, []v1alpha1.AllowlistRule, string, error) {
		cfg, err := m.mgr.ResolvedConfigForUser(ctx, user)
		if err != nil {
			return "", nil, "", err
		}
		if cfg == nil || cfg.Empty() {
			return "", nil, cfg.Revision, nil
		}
		return cfg.ConfirmPolicy, cfg.Allowlist, cfg.Revision, nil
	}
	m.wsURLOf = func(user string) string {
		return wsURL(m.mgr.BaseURL(user))
	}
	return m
}

// SetConnFactory overrides connection construction (tests).
func (m *hitlManager) SetConnFactory(f func(url string, dev *ws.Device) hitlGateway) {
	m.newClient = f
}

func (m *hitlManager) sayf(format string, args ...any) {
	if m.logf != nil {
		m.logf(format, args...)
	}
}

// deviceFor derives the deterministic per-user device identity from the master
// key (sha512(masterKey | user) as the Ed25519 seed).
func (m *hitlManager) deviceFor(user string) *ws.Device {
	sum := sha512.Sum512(append(append([]byte{}, m.masterKey...), []byte("|"+user)...))
	seed := sum[:ed25519.SeedSize]
	return mustDevice(ed25519.NewKeyFromSeed(seed))
}

// DevicePublicKeyFor returns the derived operator device public key for a user
// (served to the supervisor so it can approve this device's gateway pairing).
func (m *hitlManager) DevicePublicKeyFor(user string) string {
	return m.deviceFor(user).PublicKey
}

func mustDevice(priv ed25519.PrivateKey) *ws.Device {
	dev, err := ws.NewDevice(priv.Public().(ed25519.PublicKey), priv)
	if err != nil {
		panic(fmt.Sprintf("hitl device: %v", err)) // unreachable for a valid key
	}
	return dev
}

// wsURL derives the gateway-protocol endpoint from the instance's HTTP base
// URL (http://host:port -> ws://host:port/gateway).
func wsURL(baseURL string) string {
	return "ws" + strings.TrimPrefix(baseURL, "http") + "/gateway"
}

// conn returns (connecting on first use or after a drop) the user's gateway
// connection. First-connection per user is serialized so two concurrent turns
// cannot leave two live approval clients (duplicate event callbacks). A connect
// failure returns the stored client so later calls fail fast and can retry.
func (m *hitlManager) conn(ctx context.Context, user string) (hitlGateway, error) {
	m.mu.Lock()
	if c, ok := m.conns[user]; ok && c.gw.Connected() {
		gw := c.gw
		m.mu.Unlock()
		return gw, nil
	}
	if m.connecting == nil {
		m.connecting = map[string]*sync.Mutex{}
	}
	lm, ok := m.connecting[user]
	if !ok {
		lm = &sync.Mutex{}
		m.connecting[user] = lm
	}
	m.mu.Unlock()

	lm.Lock()
	defer lm.Unlock()

	// Re-check under the per-user lock: the first caller may have connected.
	m.mu.Lock()
	if c, ok := m.conns[user]; ok && c.gw.Connected() {
		gw := c.gw
		m.mu.Unlock()
		return gw, nil
	}
	m.mu.Unlock()

	dev := m.deviceFor(user)
	gw := m.newClient(m.wsURLOf(user), dev)
	gw.OnApprovalRequested(func(ev ws.ApprovalRequested) {
		if ev.Request.SessionKey == "" || ev.Request.Command == "" {
			return // cannot attribute to a conversation; leave to the gateway timeout
		}
		if m.bridge != nil {
			m.bridge(user, ev)
		}
	})
	// Asker questions (issue #161): a question the agent is blocked on is
	// relayed to its session's SSE stream. This is deliberately separate from
	// the live-turn projector below -- a question is addressed by its own
	// session key and must reach the browser even though it is not run content.
	gw.OnQuestionRequested(func(rec ws.QuestionRecord) {
		if m.questionRequested != nil {
			m.questionRequested(user, rec)
		}
	})
	gw.OnQuestionResolved(func(res ws.QuestionResolved) {
		if m.questionResolved != nil {
			m.questionResolved(user, res)
		}
	})
	// Live tool stream (issue #130): every session-message event this
	// connection receives is routed to the active live turn for its session
	// (one per chat turn, see AttachLive). The gateway fans these only to
	// subscribed sessions, so an idle connection sees no tool traffic.
	gw.OnEvent(func(evName string, payload []byte) {
		m.routeLive(user, evName, payload)
	})

	m.mu.Lock()
	m.conns[user] = &userHitlConn{user: user, gw: gw}
	m.mu.Unlock()

	// First connect may be rejected NOT_PAIRED while the in-pod supervisor
	// approves this device (device.pair.approve on its next poll). The approval
	// lands a moment after the first rejected connect creates the pending
	// device, so keep retrying within a bounded budget instead of a fixed small
	// number of attempts -- otherwise the first gated turn races the approval
	// and silently runs ungated (issue #116 flake). A best-effort caller gives
	// up once the budget is exhausted.
	pairBudget := 30 * time.Second
	pairStart := time.Now()
	var connectErr error
	for {
		aCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := gw.Connect(aCtx)
		cancel()
		if err == nil {
			return gw, nil
		}
		connectErr = err
		if !strings.Contains(err.Error(), "NOT_PAIRED") {
			break
		}
		if time.Since(pairStart) > pairBudget {
			break
		}
		select {
		case <-time.After(hitlPairRetryDelay):
		case <-ctx.Done():
			return gw, ctx.Err()
		}
	}
	return gw, fmt.Errorf("hitl connect %s: %w", user, connectErr)
}

// PreTurn is called at the start of an interactive turn. For Allowlist and
// AlwaysAsk it ensures the approval connection and applies the effective exec
// policy once per config revision. It returns whether the session must be
// guarded; RunLiveTurn reconciles that state atomically with the model.
//
// Both gated policies fail closed (issue #127): confirmPolicy is the single
// authority for whether a turn is guarded, so if the policy cannot be resolved
// or the approval channel/policy/guard cannot be applied, PreTurn returns an
// error and the caller must not start the turn. A policy that says "ask" must
// never silently run a turn that cannot ask -- that is the silent downgrade
// issue #127 exists to remove. Only a resolved None/empty policy passes through
// (an unresolvable config is treated as gated-unknown and fails closed, not as
// None).
func (m *hitlManager) PreTurn(ctx context.Context, user string) (bool, error) {
	pol, allow, rev, err := m.resolved(ctx, user)
	if err != nil {
		return false, fmt.Errorf("hitl %s: cannot resolve confirm policy: %w", user, err)
	}
	switch pol {
	case v1alpha1.ConfirmPolicyAllowlist, v1alpha1.ConfirmPolicyAlwaysAsk:
	default: // None / empty -> pass-through
		return false, nil
	}
	gw, err := m.conn(ctx, user)
	if err != nil {
		return false, fmt.Errorf("hitl %s: cannot gate %s turn (approval channel unavailable): %w", user, pol, err)
	}
	// Apply the effective exec policy when the resolved-config revision changed;
	// only a successful apply advances revPol so a transient failure is retried
	// next turn.
	m.mu.Lock()
	appliedRev := m.revPol[user]
	m.mu.Unlock()
	if rev != "" && rev != appliedRev {
		if err := m.applyPolicy(ctx, user, gw, pol, allow); err != nil {
			return false, fmt.Errorf("hitl %s: cannot apply %s exec policy: %w", user, pol, err)
		}
		m.mu.Lock()
		m.revPol[user] = rev
		m.mu.Unlock()
	}
	return true, nil
}

// channelState reports whether the per-user approval channel can currently
// carry a gated turn (issue #127). An established connection answers "up" from
// cache; otherwise one bounded connect attempt is made and immediately dropped.
// A NOT_PAIRED rejection means the supervisor has not yet approved this user's
// derived device (first use) and reports "pairing" -- the rejected connect
// seeds the pending device the supervisor auto-approves on its next poll, so
// the state self-heals. Surfaced on the confirm view so a policy edit is never
// a silent no-op: with a "down"/"unconfigured" channel a gated turn fails
// closed rather than running ungated.
func (m *hitlManager) channelState(ctx context.Context, user string) string {
	m.mu.Lock()
	if c, ok := m.conns[user]; ok && c.gw != nil && c.gw.Connected() {
		m.mu.Unlock()
		return confirmChannelUp
	}
	m.mu.Unlock()

	dev := m.deviceFor(user)
	gw := m.newClient(m.wsURLOf(user), dev)
	defer gw.Close()
	aCtx, cancel := context.WithTimeout(ctx, channelProbeTimeout)
	defer cancel()
	if err := gw.Connect(aCtx); err != nil {
		if strings.Contains(err.Error(), "NOT_PAIRED") {
			return confirmChannelPairing
		}
		m.sayf("hitl %s: channel probe: %v", user, err)
		return confirmChannelDown
	}
	return confirmChannelUp
}

// applyPolicy writes the effective exec-approvals policy into agents."main" of
// the gateway (get -> set, CAS). The allowlist is rewritten wholesale from the
// resolved config (issue #116): the platform bookkeeping is the instance
// allowlist, so a removed entry really disappears. AlwaysAsk runs a guarded,
// on-miss session with an empty allowlist -- every command misses and therefore
// asks -- which is the strictest posture and needs no unverified ask:always
// semantics. It reports failure so the caller can defer advancing the
// applied-revision watermark.
func (m *hitlManager) applyPolicy(ctx context.Context, user string, gw hitlGateway, pol v1alpha1.ConfirmPolicy, allow []v1alpha1.AllowlistRule) error {
	snap, err := gw.GetApprovalsPolicy(ctx)
	if err != nil {
		m.sayf("hitl %s: exec.approvals.get: %v", user, err)
		return err
	}
	file := snap.File
	if file.Agents == nil {
		file.Agents = map[string]ws.ApprovalAgentPolicy{}
	}
	agent := file.Agents["main"]
	switch pol {
	case v1alpha1.ConfirmPolicyAlwaysAsk:
		// Clear any allowlist a prior Allowlist mode wrote: on-miss with an
		// empty allowlist asks on everything.
		agent.Allowlist = nil
	default: // ConfirmPolicyAllowlist
		agent.Allowlist = toWSEntries(allow)
	}
	file.Agents["main"] = agent
	base := ""
	if snap.Exists {
		base = snap.Hash
	}
	if _, err := gw.SetApprovalsPolicy(ctx, file, base); err != nil {
		m.sayf("hitl %s: exec.approvals.set: %v", user, err)
		return err
	}
	return nil
}

// toWSEntries converts the resolved (public-API) allowlist rules into the
// gateway's exec-approvals entry shape.
func toWSEntries(rules []v1alpha1.AllowlistRule) []ws.AllowlistEntry {
	out := make([]ws.AllowlistEntry, 0, len(rules))
	for _, r := range rules {
		if r.Pattern == "" {
			continue
		}
		out = append(out, ws.AllowlistEntry{Pattern: r.Pattern, ArgPattern: r.ArgPattern})
	}
	return out
}

// liveConn returns the user's established gateway connection without dialing a
// new one. Resolution and pending-state reads must not open a connection (and
// trigger a device pairing) as a side effect of a status request.
//
// The connection is registered before its handshake completes (conn stores it
// up front so the pairing retry loop can hold the per-user lock), so a stored
// gateway is not necessarily a usable one: without the Connected check a call
// made mid-pairing would fail inside Client.Call with "not connected" and
// surface as a gateway error instead of the unavailable-channel status.
func (m *hitlManager) liveConn(user string) (hitlGateway, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.conns[user]
	if !ok || c == nil || c.gw == nil || !c.gw.Connected() {
		return nil, false
	}
	return c.gw, true
}

// GetQuestion reads one question record from the user's gateway (question.get).
func (m *hitlManager) GetQuestion(ctx context.Context, user, id string) (*ws.QuestionRecord, error) {
	gw, ok := m.liveConn(user)
	if !ok {
		return nil, errNoQuestionChannel
	}
	return gw.GetQuestion(ctx, id)
}

// ListQuestions returns the user's gateway's pending questions (question.list).
func (m *hitlManager) ListQuestions(ctx context.Context, user string) ([]ws.QuestionRecord, error) {
	gw, ok := m.liveConn(user)
	if !ok {
		return nil, errNoQuestionChannel
	}
	return gw.ListQuestions(ctx)
}

// ResolveQuestion answers a pending question on the user's gateway connection.
// answers maps each question id to the selected option labels.
func (m *hitlManager) ResolveQuestion(ctx context.Context, user, id string, answers map[string][]string) error {
	gw, ok := m.liveConn(user)
	if !ok {
		return errNoQuestionChannel
	}
	return gw.ResolveQuestion(ctx, id, answers, user)
}

// CancelQuestion dismisses a pending question so the agent continues instead of
// waiting out its own timeout.
func (m *hitlManager) CancelQuestion(ctx context.Context, user, id string) error {
	gw, ok := m.liveConn(user)
	if !ok {
		return errNoQuestionChannel
	}
	return gw.CancelQuestion(ctx, id, user)
}

// Abort cancels the session's active run over the user's gateway connection.
// runID scopes the abort to the run the server believes is live so it cannot
// kill a run promoted after that one settles; an empty runID falls back to the
// session-scoped abort.
func (m *hitlManager) Abort(ctx context.Context, user, sessionKey, runID string) error {
	gw, ok := m.liveConn(user)
	if !ok {
		return fmt.Errorf("abort %q: no live gateway channel", sessionKey)
	}
	return gw.AbortChat(ctx, sessionKey, runID)
}

// SessionBusy reports whether the gateway still has an in-flight run for the
// session. Unlike the SSE hub this survives a browser disconnect, so it is the
// only signal that means anything on the reload-takeover path.
func (m *hitlManager) SessionBusy(ctx context.Context, user, sessionKey string) (bool, error) {
	gw, ok := m.liveConn(user)
	if !ok {
		return false, fmt.Errorf("session busy %q: no live gateway channel", sessionKey)
	}
	return gw.SessionBusy(ctx, sessionKey)
}

// ResolveApproval implements ApprovalResolver: the Portal decision is applied
// to the user's gateway connection.
func (m *hitlManager) ResolveApproval(ctx context.Context, user, approvalID, decision string) error {
	gw, ok := m.liveConn(user)
	if !ok {
		return fmt.Errorf("no approval connection for %s", user)
	}
	gwDecision := "deny"
	if decision == "approve" {
		gwDecision = "allow-once"
	}
	return gw.ResolveApproval(ctx, approvalID, gwDecision)
}

// wsRunTail is how long RunLiveTurn waits after agent.wait for a terminal chat
// frame (final/aborted/error) to reflect in doneErr before returning. Content
// frames are TCP-ordered before the agent.wait response, so this is normally
// already resolved and returns immediately.
const wsRunTail = 500 * time.Millisecond

// LiveRunID returns the run id of the session's active live turn, if any. The
// browser never learns the run id; the server does, and passing it scopes an
// abort so it cannot kill a run promoted after this one settles.
func (m *hitlManager) LiveRunID(sessionKey string) (string, bool) {
	m.liveMu.Lock()
	t := m.live[sessionKey]
	m.liveMu.Unlock()
	if t == nil {
		return "", false
	}
	t.runMu.Lock()
	defer t.runMu.Unlock()
	return t.runID, t.runID != ""
}

// registerLive registers the live turn for a session so connection events route
// to it. The turn stays registered until releaseLive.
func (m *hitlManager) registerLive(user, sessionKey string, sink func(agentruntime.Event) error) *liveTurn {
	t := &liveTurn{user: user, sink: sink, proj: newLiveProjector(), done: make(chan struct{})}
	m.liveMu.Lock()
	if m.live == nil {
		m.live = map[string]*liveTurn{}
	}
	m.live[sessionKey] = t
	m.liveMu.Unlock()
	return t
}

// releaseLive unregisters the session's live turn and, best-effort, unsubscribes
// its message stream.
func (m *hitlManager) releaseLive(user, sessionKey string, gw hitlGateway) {
	m.liveMu.Lock()
	if cur, ok := m.live[sessionKey]; ok && cur.user == user {
		delete(m.live, sessionKey)
	}
	m.liveMu.Unlock()
	// Best-effort unsubscribe on a short deadline: the conn may already be gone,
	// and Call blocks until the pump exits if it is.
	uctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = gw.UnsubscribeSessionMessages(uctx, sessionKey)
}

// RunLiveTurn drives one full chat turn entirely over the user's gateway
// WebSocket (issue #130 WS-only chat): it subscribes the session's live message
// stream, sends the user message with sessions.send, and streams every event
// the run produces into sink (assistant text deltas, tool calls/results) as it
// happens. sink is invoked from the WS read goroutine, so it must be safe for
// concurrent calls with the caller (stream.Send is).
//
// sessions.send only ACKs the run start ({status:"started", runId}); the run's
// content arrives on the subscription afterwards. agent.wait(runId) blocks until
// the run is terminal and is the authoritative completion signal, so the turn
// stays subscribed long enough to receive everything (fix: a previous version
// returned on the start ACK and unsubscribed ~3s in, before the first token).
func (m *hitlManager) RunLiveTurn(ctx context.Context, user, sessionKey, message, model string, guarded bool, sink func(agentruntime.Event) error) (agentruntime.TurnOutcome, error) {
	gw, err := m.conn(ctx, user)
	if err != nil {
		m.sayf("chat %s: %s: gateway connect: %v", user, sessionKey, err)
		return agentruntime.TurnOutcome{}, err
	}
	// sessions.send only auto-creates the agent's main session, not arbitrary
	// conversation keys (the OpenAI-compat HTTP surface created those on the
	// fly; the WS surface does not). sessions.create adopts an existing key, so
	// every error is a real preparation failure and must be surfaced.
	state, err := gw.CreateSession(ctx, sessionKey)
	if err != nil {
		m.sayf("chat %s: %s: ensure session: %v", user, sessionKey, err)
		return agentruntime.TurnOutcome{}, err
	}
	// sessions.send has no model or permission fields. Reconcile both through
	// one typed sessions.patch so a failed update cannot leave only half of the
	// desired turn state applied. None policy explicitly clears an old guarded
	// mode; unchanged fields are omitted to avoid a redundant hot-path RPC.
	desiredPermission := ""
	if guarded {
		desiredPermission = "guarded"
	}
	patch := ws.SessionSettingsPatch{
		Model: ws.OptionalString{Set: state.Model != model, Value: model},
		PermissionMode: ws.OptionalString{
			Set:   state.PermissionMode != desiredPermission,
			Value: desiredPermission,
		},
	}
	if patch.Model.Set || patch.PermissionMode.Set {
		if err := gw.PatchSessionSettings(ctx, sessionKey, patch); err != nil {
			m.sayf("chat %s: %s: apply session settings: %v", user, sessionKey, err)
			return agentruntime.TurnOutcome{}, err
		}
	}
	t := m.registerLive(user, sessionKey, sink)
	defer m.releaseLive(user, sessionKey, gw)

	// Pre-generate the idempotency key and install it as the expected run BEFORE
	// subscribing/sending: sessions.send returns it as the run id, so content and
	// terminal frames are correlated (and unrelated or pre-ACK runs rejected)
	// from the first event, before any run of the session can leak in.
	idem := uuid.NewString()
	t.setRunID(idem)

	if err := gw.SubscribeSessionMessages(ctx, sessionKey); err != nil {
		m.sayf("chat %s: %s: subscribe: %v", user, sessionKey, err)
		return agentruntime.TurnOutcome{}, err
	}
	runID, err := gw.SendSessionMessage(ctx, sessionKey, message, idem)
	if err != nil {
		return agentruntime.TurnOutcome{}, err
	}
	if runID != "" && runID != idem {
		t.setRunID(runID)
	}
	if runID == "" {
		runID = idem
	}
	// Terminal is driven by the subscribed event stream (chat final/aborted/
	// error closes t.done) -- agent.wait metadata cannot reliably distinguish a
	// bounded-wait timeout from a terminal one (v2026.8.2 stamps timeoutPhase on
	// both paths). agent.wait runs as a backstop notifier on the same WS and is
	// cancelled when the events say the turn is over.
	waitCtx, cancelWait := context.WithCancel(ctx)
	defer cancelWait()
	waitDone := make(chan error, 1)
	go func() {
		waitDone <- gw.AgentWait(waitCtx, runID)
	}()
	select {
	case <-t.done:
		return t.outcome()
	case err := <-waitDone:
		if err != nil {
			m.sayf("chat %s: %s: agent.wait %s: %v", user, sessionKey, runID, err)
			return agentruntime.TurnOutcome{}, err
		}
		// agent.wait returned ok; settle a moment for any trailing terminal frame
		// already queued before returning.
		select {
		case <-t.done:
		case <-time.After(wsRunTail):
		case <-ctx.Done():
			return agentruntime.TurnOutcome{}, ctx.Err()
		}
		return t.outcome()
	case <-ctx.Done():
		return agentruntime.TurnOutcome{}, ctx.Err()
	}
}

// routeLive fans a gateway session-message event to the active live turn for
// its session (invoked from the connection's event sink). Events without a
// session key (heartbeats, ticks) and events for sessions with no active turn
// are ignored. A write error from the sink stops the turn (the browser went
// away); the terminal lifecycle event then releases the RunLiveTurn caller.
func (m *hitlManager) routeLive(user, evName string, payload []byte) {
	var hdr struct {
		SessionKey string `json:"sessionKey"`
		RunID      string `json:"runId"`
	}
	if err := json.Unmarshal(payload, &hdr); err != nil || hdr.SessionKey == "" {
		return
	}
	m.liveMu.Lock()
	t := m.live[hdr.SessionKey]
	m.liveMu.Unlock()
	if t == nil || t.user != user || !t.acceptRun(hdr.RunID) {
		return
	}
	evs, terminal := t.proj.feed(hdr.SessionKey, evName, payload)
	for _, ev := range evs {
		if err := t.sink(ev); err != nil {
			t.finish(err)
			return
		}
	}
	if terminal {
		// A chat "error"/"aborted" frame is terminal. A request-initiated abort
		// is not a failure, so it is reported as an outcome rather than an error.
		var (
			err     error
			stopped bool
		)
		if evName == "chat" {
			err, stopped = chatTerminalOutcome(payload)
		}
		t.finishWith(err, stopped)
	}
}

// chatTerminalOutcome classifies a terminal chat frame (state "error"/"aborted").
// OpenClaw carries the diagnostic under errorMessage (and sometimes error) and
// the cancel origin under stopReason. A request-initiated stop -- stopReason
// "rpc" for the chat.abort RPC, "stop" for the /stop command path -- is an
// outcome, not a failure: it returns a nil error and stopped=true, so the
// caller emits message_done{stopped:true} instead of message_done{error}.
// Any other abort (timeout, restart, auth-revoked) stays an error.
func chatTerminalOutcome(payload []byte) (error, bool) {
	var st struct {
		State        string `json:"state"`
		Error        string `json:"error"`
		ErrorMessage string `json:"errorMessage"`
		StopReason   string `json:"stopReason"`
	}
	if json.Unmarshal(payload, &st) != nil || (st.State != "error" && st.State != "aborted") {
		return nil, false
	}
	if st.StopReason == "rpc" || st.StopReason == "stop" {
		return nil, true
	}
	msg := st.ErrorMessage
	if msg == "" {
		msg = st.Error
	}
	if msg == "" {
		msg = "agent run " + st.State
	}
	return fmt.Errorf("%s", msg), false
}
