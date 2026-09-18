package server

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/suanova/cubepilot/internal/openclaw/ws"
	agentruntime "github.com/suanova/cubepilot/internal/runtime"
)

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

// wsRunTail is how long RunLiveTurn waits after agent.wait for a terminal chat
// frame (final/aborted/error) to reflect in doneErr before returning. Content
// frames are TCP-ordered before the agent.wait response, so this is normally
// already resolved and returns immediately.
const wsRunTail = 500 * time.Millisecond

// LiveRunID returns the run id of the user's active live turn on this session.
// The browser never learns the run id; the server does, and passing it scopes an
// abort so it cannot kill a run promoted after this one settles.
//
// The user is part of the key on purpose. m.live is indexed by session key
// alone, and a session key can be client-supplied, so without the ownership
// check one user's /abort could pick up another user's run id. routeLive checks
// the same thing when it routes; keep them together.
func (m *gatewayConns) LiveRunID(user, sessionKey string) (string, bool) {
	m.liveMu.Lock()
	t := m.live[sessionKey]
	m.liveMu.Unlock()
	if t == nil || t.user != user {
		return "", false
	}
	t.runMu.Lock()
	defer t.runMu.Unlock()
	return t.runID, t.runID != ""
}

// registerLive registers the live turn for a session so connection events route
// to it. The turn stays registered until releaseLive.
func (m *gatewayConns) registerLive(user, sessionKey string, sink func(agentruntime.Event) error) *liveTurn {
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
func (m *gatewayConns) releaseLive(user, sessionKey string, gw gatewayClient) {
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
func (m *gatewayConns) RunLiveTurn(ctx context.Context, user, sessionKey, message, model string, guarded bool, sink func(agentruntime.Event) error) (agentruntime.TurnOutcome, error) {
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

// attachCap bounds how long a re-attached stream may live. A parked run always
// ends by itself (the gateway expires the question and the run continues to a
// terminal frame), so this only catches a run that never terminates -- a gateway
// restarting under it, say. Without a cap the observer would outlive every reason
// to keep observing.
const attachCap = time.Hour

// AttachLiveTurn subscribes the session's live message stream and registers a
// live turn for it WITHOUT sending a message (issue #167): the caller observes a
// run that is already in flight and parked on a human decision, so the browser
// answering a question or approval restored after a reload sees the
// continuation, even though the request that started the turn is long gone.
//
// runID is the run the parked decision belongs to (a gateway question carries
// it); it seeds the run filter so an unrelated run of the same session cannot
// leak into this stream. An empty runID -- a write approval carries no run
// id -- accepts any run of the session.
//
// Unlike RunLiveTurn there is no agent.wait backstop: the run was not started
// here, and its terminal chat frame is the authoritative end, which the gateway
// broadcasts to every session subscriber including one that joined late.
//
// revalidate, when non-nil, runs once the subscription is in place and before
// anything is observed: it is the caller's chance to re-check the state it
// gated the attach on, because that state can change while the subscription is
// being established. A non-nil result ends the attach there -- the caller is
// reporting that the events this stream was opened for have already been
// broadcast, which no late subscription can undo.
//
// It reports the same TurnOutcome RunLiveTurn does, so an observing caller
// terminal-writes through liveTurnDone and cannot report a run another tab
// stopped as a plain completion.
func (m *gatewayConns) AttachLiveTurn(ctx context.Context, user, sessionKey, runID string, sink func(agentruntime.Event) error, revalidate func() error) (agentruntime.TurnOutcome, error) {
	gw, err := m.conn(ctx, user)
	if err != nil {
		return agentruntime.TurnOutcome{}, err
	}
	t := m.registerLive(user, sessionKey, sink)
	defer m.releaseLive(user, sessionKey, gw)
	t.setRunID(runID)
	if err := gw.SubscribeSessionMessages(ctx, sessionKey); err != nil {
		m.sayf("attach %s: %s: subscribe: %v", user, sessionKey, err)
		return agentruntime.TurnOutcome{}, err
	}
	if revalidate != nil {
		if err := revalidate(); err != nil {
			return agentruntime.TurnOutcome{}, err
		}
	}
	select {
	case <-t.done:
		return t.outcome()
	case <-time.After(attachCap):
		m.sayf("attach %s: %s: no terminal event within %s", user, sessionKey, attachCap)
		return agentruntime.TurnOutcome{}, fmt.Errorf("the parked run did not finish within %s", attachCap)
	case <-ctx.Done():
		return agentruntime.TurnOutcome{}, ctx.Err()
	}
}

// routeLive fans a gateway session-message event to the active live turn for
// its session (invoked from the connection's event sink). Events without a
// session key (heartbeats, ticks) and events for sessions with no active turn
// are ignored. A write error from the sink stops the turn (the browser went
// away); the terminal lifecycle event then releases the RunLiveTurn caller.
func (m *gatewayConns) routeLive(user, evName string, payload []byte) {
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
