package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/suanova/cubepilot/internal/config"
	"github.com/suanova/cubepilot/internal/openclaw/ws"
	agentruntime "github.com/suanova/cubepilot/internal/runtime"
)

// abortTestKey is the gateway-canonical form of the "conv-1" segment the client
// posts in the URL. It is not cosmetic: handleMessages canonicalizes before
// hub.Open and before the live turn registers, and settle compares a record's
// canonical key, so /abort must canonicalize for all three to line up. The
// tests below post the RAW path and assert the canonical key comes out the far
// end -- a handler passing the raw key would miss the session's stream, its
// live turn and its pending records all at once.
const abortTestKey = "agent:main:conv-1"

// Stop must not return until the session is genuinely idle, or the client's
// next POST /api/messages races hub.Open and 409s. It must also settle pending
// HITL records while the stream is still open, and report a timeout as 504
// rather than pretending the session is free.
func TestHandleAbortWaitsForIdle(t *testing.T) {
	h := NewHub()
	w := httptest.NewRecorder()
	stream, err := h.Open(abortTestKey, w, w)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	gw := &fakeAbortGateway{}
	m := &hitlManager{
		newClient: func(string, *ws.Device) hitlGateway { return gw },
		conns:     map[string]*userHitlConn{"admin": {user: "admin", gw: gw}},
	}
	// A live turn is what supplies the run id; without one the handler falls
	// back to the session-scoped abort.
	live := m.registerLive("admin", abortTestKey, func(agentruntime.Event) error { return nil })
	live.setRunID("run-9")
	gw.busy = false

	// Build the Server through a helper, not a bare literal: settle touches
	// s.approvals and s.qroutes as well as s.hitl, and a literal that omits them
	// nil-panics on the test goroutine.
	s := newAbortTestServer(h, m)

	done := make(chan int, 1)
	rec := httptest.NewRecorder()
	go func() {
		s.handleAbort(rec, httptest.NewRequest(http.MethodPost, "/api/sessions/conv-1/abort", nil))
		done <- rec.Code
	}()

	select {
	case code := <-done:
		t.Fatalf("handleAbort returned %d while the stream was open", code)
	case <-time.After(50 * time.Millisecond):
	}

	stream.Close()
	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Fatalf("code = %d, want 200", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handleAbort did not return after the stream closed")
	}

	if gw.lastAbortSession != abortTestKey {
		t.Fatalf("abort session = %q, want the canonical key %q", gw.lastAbortSession, abortTestKey)
	}
	if gw.lastAbortRunID != "run-9" {
		t.Fatalf("abort runID = %q, want the live turn's run id", gw.lastAbortRunID)
	}
}

// A stop that fails at the gateway is a failure, never a success: an abort RPC
// that errored means the run may still be going, and answering 200 would tell
// the Portal to send the follow-up into a session that is still busy -- the 409
// this endpoint exists to remove, plus a turn the user believes they stopped.
func TestHandleAbortReportsAbortFailure(t *testing.T) {
	h := NewHub()
	gw := &fakeAbortGateway{abortErr: errors.New("gateway gone")}
	m := &hitlManager{conns: map[string]*userHitlConn{"admin": {user: "admin", gw: gw}}}
	s := newAbortTestServer(h, m)

	rec := httptest.NewRecorder()
	s.handleAbort(rec, httptest.NewRequest(http.MethodPost, "/api/sessions/conv-1/abort", nil))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code = %d, want 502", rec.Code)
	}
}

// "Cannot determine" is not "not busy". SessionBusy failing (a missing or
// mid-handshake gateway connection) must not be swallowed into a false idle: a
// false positive here reports a stop that never happened, which is the one
// outcome the endpoint must never produce.
func TestHandleAbortBusyErrorIsNotIdle(t *testing.T) {
	h := NewHub()
	gw := &fakeAbortGateway{busyErr: errors.New("no live gateway channel")}
	m := &hitlManager{conns: map[string]*userHitlConn{"admin": {user: "admin", gw: gw}}}
	s := newAbortTestServer(h, m)

	rec := httptest.NewRecorder()
	s.handleAbort(rec, httptest.NewRequest(http.MethodPost, "/api/sessions/conv-1/abort", nil))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code = %d, want 502: an undeterminable busy state must not read as idle", rec.Code)
	}
}

// A session that never goes idle must be reported as a gateway timeout, not as
// a successful stop. The request context is already cancelled, which is exactly
// what the waiter sees when its deadline expires; the open stream keeps the hub
// non-idle so the wait is what fails, not the check.
func TestHandleAbortTimeoutIsGatewayTimeout(t *testing.T) {
	h := NewHub()
	w := httptest.NewRecorder()
	stream, err := h.Open(abortTestKey, w, w)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer stream.Close()

	gw := &fakeAbortGateway{}
	m := &hitlManager{conns: map[string]*userHitlConn{"admin": {user: "admin", gw: gw}}}
	s := newAbortTestServer(h, m)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	s.handleAbort(rec, httptest.NewRequest(http.MethodPost, "/api/sessions/conv-1/abort", nil).WithContext(ctx))

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("code = %d, want 504", rec.Code)
	}
}

// The endpoint's input guards: the wrong method, a URL that carries no session
// key, and a Server with no HITL channel at all.
func TestHandleAbortRejectsBadRequests(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		hitl   bool
		want   int
	}{
		{name: "method", method: http.MethodGet, path: "/api/sessions/conv-1/abort", hitl: true, want: http.StatusMethodNotAllowed},
		{name: "empty key", method: http.MethodPost, path: "/api/sessions//abort", hitl: true, want: http.StatusBadRequest},
		{name: "main key", method: http.MethodPost, path: "/api/sessions/agent:main:/abort", hitl: true, want: http.StatusBadRequest},
		{name: "no hitl", method: http.MethodPost, path: "/api/sessions/conv-1/abort", hitl: false, want: http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var m *hitlManager
			if tc.hitl {
				m = &hitlManager{conns: map[string]*userHitlConn{"admin": {user: "admin", gw: &fakeAbortGateway{}}}}
			}
			s := newAbortTestServer(NewHub(), m)

			rec := httptest.NewRecorder()
			s.handleAbort(rec, httptest.NewRequest(tc.method, tc.path, nil))

			if rec.Code != tc.want {
				t.Fatalf("code = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

// With the hub idle, the gateway's busy flag is the only gate left -- and it is
// the gate that matters on the reload-takeover path, where no stream exists at
// all. A SessionBusy that ERRORS is covered above; this pins the VALUE: a
// gateway that keeps reporting a run in flight, with nothing for the hub to
// wait on, must end as a 504, never as a 200 claiming a stop that did not
// happen.
func TestHandleAbortBusyGatewayIsNotSuccess(t *testing.T) {
	// Long enough that the check is certainly consulted before the bound
	// expires: a bound shorter than the loop's own cadence would 504 without
	// ever asking the gateway, and then this test would pass even against a
	// handler that ignores the answer.
	shortenAbortSettleTimeout(t, 500*time.Millisecond)

	h := NewHub() // no stream: the hub reports idle immediately
	checked := make(chan struct{})
	gw := &fakeAbortGateway{busyFunc: func(context.Context) (bool, error) {
		select {
		case <-checked:
		default:
			close(checked)
		}
		return true, nil // a run that never ends
	}}
	m := &hitlManager{conns: map[string]*userHitlConn{"admin": {user: "admin", gw: gw}}}
	s := newAbortTestServer(h, m)

	done := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		s.handleAbort(rec, httptest.NewRequest(http.MethodPost, "/api/sessions/conv-1/abort", nil))
		done <- rec.Code
	}()

	select {
	case <-checked:
	case <-time.After(2 * time.Second):
		t.Fatal("handleAbort never consulted the gateway's busy state")
	}

	select {
	case code := <-done:
		if code != http.StatusGatewayTimeout {
			t.Fatalf("code = %d, want 504: the gateway still has a run for this session", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handleAbort never returned while the gateway stayed busy")
	}
}

// The wait must be ended by its own bound, not only by a client that gave up.
// Here the request context stays live and the session never goes idle, so the
// ONLY thing that can end the wait is abortSettleTimeout -- shortened so the
// test is fast. With the bound removed the handler never returns at all, which
// is what the watchdog below catches.
func TestHandleAbortSettleTimeoutIsBounded(t *testing.T) {
	shortenAbortSettleTimeout(t, 500*time.Millisecond)

	h := NewHub()
	w := httptest.NewRecorder()
	stream, err := h.Open(abortTestKey, w, w)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer stream.Close()

	gw := &fakeAbortGateway{}
	m := &hitlManager{conns: map[string]*userHitlConn{"admin": {user: "admin", gw: gw}}}
	s := newAbortTestServer(h, m)

	start := time.Now()
	done := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		s.handleAbort(rec, httptest.NewRequest(http.MethodPost, "/api/sessions/conv-1/abort", nil))
		done <- rec.Code
	}()

	select {
	case code := <-done:
		if code != http.StatusGatewayTimeout {
			t.Fatalf("code = %d, want 504", code)
		}
		if d := time.Since(start); d > time.Second {
			t.Fatalf("the wait ran %v past its shortened bound", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handleAbort never returned: the settle wait is not bounded")
	}
}

// A client that is gone must not turn its Stop into a no-op. Both gateway steps
// are commands, not replies: run on the request context, the abort RPC would be
// cancelled mid-flight (after ws.Client.Call has already written the frame, so
// the stop happens while the handler reports failure) and the settle would
// silently skip the pending record the reload path resurrects as a stale card.
// The response cannot be delivered either way; doing the work is the point.
func TestHandleAbortSettlesAfterClientDisconnect(t *testing.T) {
	h := NewHub()
	gw := &fakeAbortGateway{}
	m := &hitlManager{conns: map[string]*userHitlConn{"admin": {user: "admin", gw: gw}}}
	s := newAbortTestServer(h, m)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	s.handleAbort(rec, httptest.NewRequest(http.MethodPost, "/api/sessions/conv-1/abort", nil).WithContext(ctx))

	if gw.abortCtxErr != nil {
		t.Fatalf("the abort RPC ran on a dead context (%v): the frame is written before the context is consulted, so the stop happens and is then reported as failed", gw.abortCtxErr)
	}
	if !gw.listed {
		t.Fatal("settle never asked the gateway for the session's pending records")
	}
	if gw.listCtxErr != nil {
		t.Fatalf("settle ran on a dead context (%v): the pending record would survive on reload", gw.listCtxErr)
	}
}

// "Idle" is an observation about an instant, not a latch: a second tab can open
// a stream for the same session right after the hub reports idle, and answering
// 200 with that stream open is the 409 this endpoint exists to remove. The fake
// gateway holds the busy check open so the second stream opens in the window
// between the hub report and the final answer -- no scheduling luck involved.
func TestHandleAbortRechecksHubAfterIdle(t *testing.T) {
	h := NewHub()
	w1 := httptest.NewRecorder()
	stream1, err := h.Open(abortTestKey, w1, w1)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	firstCheck := make(chan struct{})
	releaseFirst := make(chan struct{})
	first := true
	gw := &fakeAbortGateway{busyFunc: func(context.Context) (bool, error) {
		if first {
			first = false
			close(firstCheck)
			<-releaseFirst
		}
		return false, nil
	}}
	m := &hitlManager{conns: map[string]*userHitlConn{"admin": {user: "admin", gw: gw}}}
	s := newAbortTestServer(h, m)

	done := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		s.handleAbort(rec, httptest.NewRequest(http.MethodPost, "/api/sessions/conv-1/abort", nil))
		done <- rec.Code
	}()

	select {
	case code := <-done:
		t.Fatalf("handleAbort returned %d while the stream was open", code)
	case <-time.After(50 * time.Millisecond):
	}

	// The first stream closes, the hub reports idle, and the handler parks
	// inside the busy check.
	stream1.Close()
	select {
	case <-firstCheck:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never reached the gateway busy check")
	}

	// A second turn opens before the handler reads the hub again.
	w2 := httptest.NewRecorder()
	stream2, err := h.Open(abortTestKey, w2, w2)
	if err != nil {
		t.Fatalf("open second stream: %v", err)
	}
	close(releaseFirst)

	select {
	case code := <-done:
		t.Fatalf("handleAbort returned %d with a second stream open", code)
	case <-time.After(300 * time.Millisecond):
	}

	stream2.Close()
	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Fatalf("code = %d, want 200", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handleAbort did not return after the second stream closed")
	}

	if gw.lastAbortSession != abortTestKey {
		t.Fatalf("abort session = %q, want the canonical key %q", gw.lastAbortSession, abortTestKey)
	}
}

// The reload-takeover path has no stream, so /turn must answer from the gateway
// and must not be cached: it is per-session liveness, not a shareable resource.
func TestHandleTurnStatus(t *testing.T) {
	gw := &fakeAbortGateway{busy: true}
	m := &hitlManager{conns: map[string]*userHitlConn{"admin": {user: "admin", gw: gw}}}
	s := newAbortTestServer(NewHub(), m)

	rec := httptest.NewRecorder()
	s.handleTurnStatus(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/conv-1/turn", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	var body struct {
		Active bool `json:"active"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Active {
		t.Fatal("active = false, want true")
	}
	// The gateway matches on the canonical key. A handler that skipped
	// canonicalSessionKey would query with the raw segment, match no in-flight
	// run, and answer a 200 false idle -- hiding the Stop control on a turn that
	// is actually running, which is the one answer this endpoint must never give.
	if gw.lastBusySession != abortTestKey {
		t.Fatalf("busy session = %q, want the canonical key %q", gw.lastBusySession, abortTestKey)
	}
}

// The gateway read is bounded, exactly as /abort's RPC is. Nothing else bounds
// it: ws.Client.Call takes the connection's write mutex and writes the frame
// before it selects on the context, and the HTTP server sets no timeouts, so a
// half-open gateway connection would park this handler forever and leak one
// blocked request per Portal refresh. The fake reports the context it was
// handed, which is the bound's only observable (an unbounded read simply never
// returns, so no fast test can wait for it).
func TestHandleTurnStatusBoundsGatewayRead(t *testing.T) {
	gw := &fakeAbortGateway{busy: true}
	m := &hitlManager{conns: map[string]*userHitlConn{"admin": {user: "admin", gw: gw}}}
	s := newAbortTestServer(NewHub(), m)

	rec := httptest.NewRecorder()
	s.handleTurnStatus(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/conv-1/turn", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if !gw.busyCtxHasDeadline {
		t.Fatal("the busy read ran on a context with no deadline: a wedged gateway would hold this handler open indefinitely")
	}
	// The slack is for the moment between this test's clock and the handler
	// setting its bound. It is small on purpose: the point is that the bound is
	// abortRPCDeadline's, not some longer one invented for /turn.
	if d := time.Until(gw.busyCtxDeadline); d <= 0 || d > abortRPCDeadline+time.Second {
		t.Fatalf("busy read deadline = now+%v, want (0, %v]", d, abortRPCDeadline)
	}
}

// A read whose bound runs out is an error, never an idle. "Cannot determine" is
// not "not busy": a 200 {"active": false} here would hide a running turn and
// remove the Stop the endpoint exists to offer. The fake behaves like a wedged
// ws.Client.Call -- it waits for the context and reports what it saw instead of
// returning a value it never received.
func TestHandleTurnStatusDeadlineIsErrorNotIdle(t *testing.T) {
	gw := &fakeAbortGateway{busyFunc: func(ctx context.Context) (bool, error) {
		<-ctx.Done()
		return false, ctx.Err()
	}}
	m := &hitlManager{conns: map[string]*userHitlConn{"admin": {user: "admin", gw: gw}}}
	s := newAbortTestServer(NewHub(), m)

	// A request context whose deadline has already passed is the state the
	// handler's own bound is in the instant it expires.
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	rec := httptest.NewRecorder()
	s.handleTurnStatus(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/conv-1/turn", nil).WithContext(expired))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code = %d, want 502: an expired busy read must not be answered as idle", rec.Code)
	}
	assertNoActiveClaim(t, rec.Body.Bytes())
}

// The endpoint's input guards. A Server with no HITL channel is not an error
// here as it is for /abort: "no channel" is a state the handler can answer
// truthfully -- nothing can be running that we would have a channel for -- and
// the caller gets the same idle answer it would get from an idle gateway.
func TestHandleTurnStatusRejectsBadRequests(t *testing.T) {
	// The nil-HITL answer is a determination, not just a status code: with no
	// channel there is nothing that could be running, so the body must say idle.
	// A status-only assertion would accept a handler answering active=true here.
	activeFalse := false
	cases := []struct {
		name       string
		method     string
		path       string
		hitl       bool
		want       int
		wantActive *bool
	}{
		{name: "method", method: http.MethodPost, path: "/api/sessions/conv-1/turn", hitl: true, want: http.StatusMethodNotAllowed},
		{name: "empty key", method: http.MethodGet, path: "/api/sessions//turn", hitl: true, want: http.StatusBadRequest},
		{name: "main key", method: http.MethodGet, path: "/api/sessions/agent:main:/turn", hitl: true, want: http.StatusBadRequest},
		{name: "no hitl", method: http.MethodGet, path: "/api/sessions/conv-1/turn", hitl: false, want: http.StatusOK, wantActive: &activeFalse},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var m *hitlManager
			if tc.hitl {
				m = &hitlManager{conns: map[string]*userHitlConn{"admin": {user: "admin", gw: &fakeAbortGateway{}}}}
			}
			s := newAbortTestServer(NewHub(), m)

			rec := httptest.NewRecorder()
			s.handleTurnStatus(rec, httptest.NewRequest(tc.method, tc.path, nil))

			if rec.Code != tc.want {
				t.Fatalf("code = %d, want %d", rec.Code, tc.want)
			}
			if tc.wantActive != nil {
				var body struct {
					Active bool `json:"active"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if body.Active != *tc.wantActive {
					t.Fatalf("active = %v, want %v: no HITL channel is an idle answer, not a hidden running turn", body.Active, *tc.wantActive)
				}
			}
		})
	}
}

// "Cannot determine" is not "not busy", for the same reason it is not in
// /abort: answering idle here would hide a running turn and remove the Stop the
// UI is supposed to offer. The caller gets an error it can report instead.
func TestHandleTurnStatusBusyErrorIsNotIdle(t *testing.T) {
	gw := &fakeAbortGateway{busyErr: errors.New("no live gateway channel")}
	m := &hitlManager{conns: map[string]*userHitlConn{"admin": {user: "admin", gw: gw}}}
	s := newAbortTestServer(NewHub(), m)

	rec := httptest.NewRecorder()
	s.handleTurnStatus(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/conv-1/turn", nil))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code = %d, want 502: an undeterminable busy state must not read as idle", rec.Code)
	}
	assertNoActiveClaim(t, rec.Body.Bytes())
}

// A correct handler the switch never reaches is no feature at all: /turn would
// fall through to the mux's 404 and a reloaded Portal would have no way to
// learn the run is still going. This drives the real Handler chain. The server
// has no HITL channel, which is the truthful answer for "no channel, nothing
// running" -- and it keeps the assertion on routing, not on the gateway.
func TestTurnRouteIsWired(t *testing.T) {
	srv := New(config.Config{DefaultUser: "alice"}, nil, nil, nil, nil)

	rec := doReq(t, srv.Handler(), http.MethodGet, "/api/sessions/conv-1/turn", "alice", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200: /turn is not wired into handleSessionSubresource", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	var body struct {
		Active bool `json:"active"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Active {
		t.Fatal("active = true, want false with no HITL channel")
	}
}

// fakeAbortGateway is the hitlGateway slice /abort exercises.
type fakeAbortGateway struct {
	hitlGateway // embed for the methods this test never calls
	busy        bool
	busyErr     error
	// busyFunc, when set, replaces the fixed busy/busyErr answer so a test can
	// hold the handler inside the busy check (see TestHandleAbortRechecksHubAfterIdle).
	busyFunc         func(context.Context) (bool, error)
	abortErr         error
	abortCtxErr      error
	lastAbortSession string
	lastAbortRunID   string
	// lastBusySession records the key the busy read was made with. The gateway
	// matches on the canonical form, so a handler passing the raw URL segment
	// gets no in-flight run back -- a false idle that hides the Stop control.
	// This is the /turn counterpart of lastAbortSession.
	lastBusySession string
	// busyCtxDeadline and busyCtxHasDeadline record the context the busy read
	// was handed, which is how the bound on that read is observed.
	busyCtxDeadline    time.Time
	busyCtxHasDeadline bool
	// listed and listCtxErr record the context the settle step handed to
	// question.list, which is how the settle's detachment is observed.
	listed     bool
	listCtxErr error
}

func (f *fakeAbortGateway) AbortChat(ctx context.Context, sessionKey, runID string) error {
	f.lastAbortSession, f.lastAbortRunID = sessionKey, runID
	f.abortCtxErr = ctx.Err()
	return f.abortErr
}

func (f *fakeAbortGateway) SessionBusy(ctx context.Context, sessionKey string) (bool, error) {
	f.lastBusySession = sessionKey
	f.busyCtxDeadline, f.busyCtxHasDeadline = ctx.Deadline()
	if f.busyFunc != nil {
		return f.busyFunc(ctx)
	}
	return f.busy, f.busyErr
}

// Connected and ListQuestions are not incidental: the settle step reaches the
// gateway through hitlManager.liveConn, which drops a connection that is not
// usable, and then lists its questions. Without an explicit Connected the
// promoted method would run on the embedded nil interface and panic; without
// ListQuestions the settle would panic one call later. This session has no open
// questions, which is the state the happy path settles in.
func (f *fakeAbortGateway) Connected() bool { return true }

func (f *fakeAbortGateway) ListQuestions(ctx context.Context) ([]ws.QuestionRecord, error) {
	f.listed = true
	f.listCtxErr = ctx.Err()
	return nil, nil
}

// assertNoActiveClaim pins that a failure response carries no `active` field at
// all. The status code says "cannot determine"; a body that also carried
// {"active": false} would give a client reading the payload defensively a
// determination to fall back on, and the only determination available there is
// the false idle this endpoint must never produce.
func assertNoActiveClaim(t *testing.T, body []byte) {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if active, ok := decoded["active"]; ok {
		t.Fatalf("body %s claims active=%v, want no active field: an undeterminable busy state must not look like a determination", body, active)
	}
}

// shortenAbortSettleTimeout replaces the settle bound for one test. The 504
// branch is otherwise reachable only through an already-cancelled request
// context, which cannot tell a bounded wait from an unbounded one.
func shortenAbortSettleTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := abortSettleTimeout
	abortSettleTimeout = d
	t.Cleanup(func() { abortSettleTimeout = prev })
}

// newAbortTestServer builds a Server with every collaborator settle and the
// abort path touch. A bare &Server{hub: h, hitl: m} literal is not enough:
// settlePendingForSession dereferences s.approvals and s.qroutes too, and a
// missing one nil-panics on the test goroutine. cfg carries the default user so
// userOf resolves the same identity the fixtures register their gateway
// connection under -- the ownership check on LiveRunID and the connection
// lookup are both keyed by it.
func newAbortTestServer(h *Hub, m *hitlManager) *Server {
	return &Server{
		cfg:       config.Config{DefaultUser: "admin"},
		hub:       h,
		hitl:      m,
		approvals: NewApprovalService(h, nil, func(string, ...any) {}),
		qroutes:   newQuestionRoutes(),
	}
}
