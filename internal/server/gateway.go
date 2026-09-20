package server

import (
	"context"
	"crypto/ed25519"
	"crypto/sha512"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/instances"
	"github.com/suanova/cubepilot/internal/openclaw/ws"
	agentruntime "github.com/suanova/cubepilot/internal/runtime"
)

// errNoGatewayChannel reports that the caller has no usable gateway connection,
// so the request never reached the gateway at all.
//
// Callers have to tell it apart from a gateway call that failed on a live
// connection. The connection is dialled lazily on first use, so "none yet" is
// the ordinary state of a fresh API process or a user who has not sent a
// message -- there, a read that says "cannot determine" is a false alarm, and a
// command (the abort) cannot succeed either way. A failure with a channel up is
// the opposite: the answer really is unknown.
//
// The sentinel spans two states that a caller must not collapse either, and it
// cannot tell them apart on its own: this process has no usable channel for the
// user, and an established connection that is down right now. They differ in
// whether a turn this process started can still be running -- see
// gatewayConnected, which records that difference for the log even though no
// caller branches on it any more: /turn establishes a channel rather than
// reasoning from the local state, so both states now end as the same honest
// "cannot determine" rather than as an idle answer.
var errNoGatewayChannel = errors.New("no live gateway channel")

// pairRetryDelay is the pause between NOT_PAIRED connect retries while the
// in-pod supervisor approves the device pairing (overridable in tests).
var pairRetryDelay = 1500 * time.Millisecond

// channelProbeTimeout bounds a single channelState connect attempt: it only
// needs to learn whether the gateway is reachable and the derived device is
// paired, so it skips conn()'s 30s pairing retry budget.
const channelProbeTimeout = 3 * time.Second

// gatewayClient is the subset of the gateway-protocol WS client the gateway
// channel depends on, so tests can substitute a fake.
type gatewayClient interface {
	Connected() bool
	Connect(ctx context.Context) error
	OnApprovalRequested(f func(ws.ApprovalRequested))
	OnApprovalResolved(f func(ws.ApprovalResolved))
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
	AbortChat(ctx context.Context, sessionKey, runID string) (bool, error)
	SessionBusy(ctx context.Context, sessionKey string) (bool, error)
	SessionInFlightRun(ctx context.Context, sessionKey string) (string, bool, error)
	DeleteSession(ctx context.Context, sessionKey string) (ws.SessionDeleteResult, error)
	Close()
}

// openClawLiveRunner adapts the user-scoped gateway connection manager to
// CubePilot's runtime-neutral interactive-turn contract. PreTurn is deliberately
// inside this adapter: confirmation setup is part of running an interactive
// turn, not a responsibility every HTTP handler or future runtime must know
// about.
type openClawLiveRunner struct {
	manager *gatewayConns
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

// gatewayConns owns the per-user gateway connections (issue #20). One
// connection per user carries everything addressed to that user's instance:
// live chat turns, write approvals, ask-user questions and aborts. It is built
// by ConfigureGateway once the API has a device root key; without a manager the
// API has no channel at all, which StartGatewayChannel treats as fatal.
type gatewayConns struct {
	mgr       *instances.Manager
	token     string
	rootKey   []byte
	newClient func(url string, dev *ws.Device) gatewayClient
	logf      func(format string, args ...any)

	// bridge is set by the server so a gateway approval can reach the
	// ApprovalService (which resolves Portal decisions and injects SSE).
	bridge func(user string, ev ws.ApprovalRequested)

	// approvalResolved is set by the server so an approval the gateway ends by
	// itself -- expiry, or a run aborted or lost gateway-side -- drops the
	// platform's record of it. Without it the record outlives the approval and
	// reload recovery resurrects a card that cannot be answered.
	approvalResolved func(user string, ev ws.ApprovalResolved)

	// questionRequested / questionResolved are set by the server so gateway
	// question broadcasts reach the parked turn's SSE stream (issue #161).
	questionRequested func(user string, rec ws.QuestionRecord)
	questionResolved  func(user string, res ws.QuestionResolved)

	// resolved returns the user's confirm policy, effective allowlist and
	// config revision. Overridable in tests; the default reads the resolved
	// config via the instance manager.
	resolved func(ctx context.Context, user string) (v1alpha1.ApprovalPolicy, []v1alpha1.AllowlistRule, string, error)

	// wsURLOf returns the gateway WS endpoint for a user. Overridable in tests.
	wsURLOf func(user string) string

	mu    sync.Mutex
	conns map[string]*userGatewayConn
	// connecting serializes the first connect per user. Each entry is a cap-1
	// channel rather than a sync.Mutex so waiting for it is context-aware: the
	// holder may be inside a dial or the NOT_PAIRED pairing retry, and a caller
	// with a bounded context -- /turn's channel probe -- has to give up at its
	// own deadline instead of parking until that dial returns.
	connecting map[string]chan struct{}
	revPol     map[string]string // user -> applied policy revision

	liveMu sync.Mutex
	live   map[string]*liveTurn // sessionKey -> active live-tool turn (issue #130)
}

type userGatewayConn struct {
	user string
	gw   gatewayClient
	// connected records that this entry's handshake has succeeded at least once.
	// It is monotonic: a connection that comes and goes keeps it, and a re-dial
	// carries it into the replacement entry. It is what gatewayConnected reads,
	// and it must NOT be set when the entry is merely registered -- conn stores
	// the entry up front and the pairing retry loop can hold it for up to 30s, so
	// treating "registered" as "has a channel" made a /turn during a user's first
	// connect answer 502 and flash the Portal's cannot-check banner at a session
	// that was idle.
	connected bool
}

// ConfigureGateway builds the manager, or returns nil when the API is missing
// something it cannot serve turns without (no instance manager, no device root
// key, no gateway token) -- StartGatewayChannel turns that into a fatal error.
func ConfigureGateway(mgr *instances.Manager, token string, rootKey []byte, logf func(string, ...any)) *gatewayConns {
	if mgr == nil || len(rootKey) == 0 || token == "" {
		return nil
	}
	m := &gatewayConns{
		mgr:        mgr,
		token:      token,
		rootKey:    rootKey,
		logf:       logf,
		conns:      map[string]*userGatewayConn{},
		connecting: map[string]chan struct{}{},
		revPol:     map[string]string{},
		live:       map[string]*liveTurn{},
	}
	m.newClient = func(url string, dev *ws.Device) gatewayClient {
		return ws.NewClient(url, token, dev)
	}
	m.resolved = func(ctx context.Context, user string) (v1alpha1.ApprovalPolicy, []v1alpha1.AllowlistRule, string, error) {
		cfg, err := m.mgr.ResolvedConfigForUser(ctx, user)
		if err != nil {
			return "", nil, "", err
		}
		if cfg == nil || cfg.Empty() {
			return "", nil, cfg.Revision, nil
		}
		return cfg.ApprovalPolicy, cfg.Allowlist, cfg.Revision, nil
	}
	m.wsURLOf = func(user string) string {
		return wsURL(m.mgr.BaseURL(user))
	}
	return m
}

// SetConnFactory overrides connection construction (tests).
func (m *gatewayConns) SetConnFactory(f func(url string, dev *ws.Device) gatewayClient) {
	m.newClient = f
}

func (m *gatewayConns) sayf(format string, args ...any) {
	if m.logf != nil {
		m.logf(format, args...)
	}
}

// deviceFor derives the deterministic per-user device identity from the root
// key (sha512(rootKey | user) as the Ed25519 seed).
func (m *gatewayConns) deviceFor(user string) *ws.Device {
	sum := sha512.Sum512(append(append([]byte{}, m.rootKey...), []byte("|"+user)...))
	seed := sum[:ed25519.SeedSize]
	return mustDevice(ed25519.NewKeyFromSeed(seed))
}

// DevicePublicKeyFor returns the derived operator device public key for a user
// (served to the supervisor so it can approve this device's gateway pairing).
func (m *gatewayConns) DevicePublicKeyFor(user string) string {
	return m.deviceFor(user).PublicKey
}

func mustDevice(priv ed25519.PrivateKey) *ws.Device {
	dev, err := ws.NewDevice(priv.Public().(ed25519.PublicKey), priv)
	if err != nil {
		panic(fmt.Sprintf("gateway device: %v", err)) // unreachable for a valid key
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
func (m *gatewayConns) conn(ctx context.Context, user string) (gatewayClient, error) {
	m.mu.Lock()
	if c, ok := m.conns[user]; ok && c.gw.Connected() {
		gw := c.gw
		m.mu.Unlock()
		return gw, nil
	}
	if m.connecting == nil {
		m.connecting = map[string]chan struct{}{}
	}
	lm, ok := m.connecting[user]
	if !ok {
		lm = make(chan struct{}, 1)
		m.connecting[user] = lm
	}
	m.mu.Unlock()

	// Context-aware, so a bounded caller does not wait out another goroutine's
	// dial (up to the 30s pairing budget) before its own deadline can fire.
	select {
	case lm <- struct{}{}:
		defer func() { <-lm }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

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
	// An approval the gateway resolved on its own: drop the platform's record so
	// it cannot resurface as a card on reload. Registered alongside the requested
	// hook and for the same reason -- both are broadcast, so a connection that is
	// not listening at the moment an approval ends misses it for good.
	gw.OnApprovalResolved(func(ev ws.ApprovalResolved) {
		if m.approvalResolved != nil {
			m.approvalResolved(user, ev)
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
	// The entry is registered before the handshake, but it is NOT yet
	// "connected": until a handshake succeeds there is no channel, which is what
	// gatewayConnected reports. A re-dial replaces the entry and carries the
	// earlier success forward, because a turn started over that connection can
	// still be running through the break.
	entry := &userGatewayConn{user: user, gw: gw, connected: m.hasConnectedLocked(user)}
	m.conns[user] = entry
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
			m.markConnected(user)
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
		case <-time.After(pairRetryDelay):
		case <-ctx.Done():
			return gw, ctx.Err()
		}
	}
	return gw, fmt.Errorf("gateway connect %s: %w", user, connectErr)
}

// gatewayConnected reports whether this process has a gateway channel for the
// user: one that is usable now, or one it opened and has since lost.
//
// It is deliberately NOT "an entry exists in m.conns". conn registers the entry
// before the handshake -- there is no earlier moment at which a second conn()
// could be told to wait on the first -- and the NOT_PAIRED pairing retry can
// hold that state for up to 30 seconds. Reading a registration as a channel made
// a /turn issued during a user's first connect answer "cannot determine" and
// raise the Portal's cannot-check banner over a session that was in fact idle;
// the state that matters is a *successful* connect, so the flag is set when a
// handshake returns. It is monotonic: a connection that drops does not un-dial
// the process, and a turn started over it can still be running.
//
// /turn no longer classifies by it -- that handler establishes a channel and
// asks the gateway directly (see SessionBusyEstablished), which answers the same
// case without the banner. The flag is kept because it still records, for the
// log, whether a failure to re-establish came on a process that had a channel at
// all, and because "an entry exists" must never be read as "there is a channel":
// the same reasoning applies to any future caller. See the sentinel's comment
// for the two states liveConn collapses into one error.
func (m *gatewayConns) gatewayConnected(user string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hasConnectedLocked(user)
}

// hasConnectedLocked is gatewayConnected's body, for callers already holding
// m.mu (conn carries the flag into a replacement entry while it has the lock).
func (m *gatewayConns) hasConnectedLocked(user string) bool {
	c := m.conns[user]
	if c == nil {
		return false
	}
	// A usable connection is a successful connect by definition -- this covers
	// the instant between Connect returning and the flag being set -- and the
	// flag covers the channel that has since gone down.
	return c.connected || (c.gw != nil && c.gw.Connected())
}

// markConnected records that the user's handshake succeeded.
func (m *gatewayConns) markConnected(user string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c := m.conns[user]; c != nil {
		c.connected = true
	}
}

// liveConn returns the user's established gateway connection without dialing a
// new one. The reads made while a turn is in flight -- question resolution and
// the pending-state lookups -- must not open a connection (and trigger a device
// pairing) as a side effect: they are made on behalf of a run that already has
// a channel, so a missing one is genuinely "no channel", not a reason to dial.
// The one status read that does establish a channel is /turn, through
// SessionBusyEstablished, where the dial is how the question gets answered at
// all after a restart.
//
// The connection is registered before its handshake completes (conn stores it
// up front so the pairing retry loop can hold the per-user lock), so a stored
// gateway is not necessarily a usable one: without the Connected check a call
// made mid-pairing would fail inside Client.Call with "not connected" and
// surface as a gateway error instead of the unavailable-channel status.
func (m *gatewayConns) liveConn(user string) (gatewayClient, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.conns[user]
	if !ok || c == nil || c.gw == nil || !c.gw.Connected() {
		return nil, false
	}
	return c.gw, true
}

// DeleteSession removes the session and its transcript on the user's gateway
// connection, so the next turn under the same key starts a fresh conversation.
//
// It uses the user's existing connection rather than dialing one, like the
// question endpoints and the abort, and for the same reason: a delete is issued
// on behalf of a client that has been talking to this session, so a missing
// connection is genuinely "no channel" rather than a reason to open one -- and
// opening one can trigger a device pairing, which a button press must not do as
// a side effect. The missing channel is reported as errNoGatewayChannel so the
// handler answers 503 rather than blaming the gateway round trip.
func (m *gatewayConns) DeleteSession(ctx context.Context, user, sessionKey string) (ws.SessionDeleteResult, error) {
	gw, ok := m.liveConn(user)
	if !ok {
		return ws.SessionDeleteResult{}, fmt.Errorf("session delete %q: %w", sessionKey, errNoGatewayChannel)
	}
	return gw.DeleteSession(ctx, sessionKey)
}
