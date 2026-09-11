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

// A stop the gateway refuses is a failure, never a success: the run is still in
// flight, and answering 200 would tell the Portal to send the follow-up into a
// session that is still busy -- the 409 this endpoint exists to remove, plus a
// turn the user believes they stopped. The reconciliation read is what tells the
// two failed-RPC cases apart, and here it confirms the worst one.
func TestHandleAbortReportsAbortFailure(t *testing.T) {
	h := NewHub()
	gw := &fakeAbortGateway{abortErr: errors.New("gateway gone"), busy: true}
	m := &hitlManager{conns: map[string]*userHitlConn{"admin": {user: "admin", gw: gw}}}
	s := newAbortTestServer(h, m)

	rec := httptest.NewRecorder()
	s.handleAbort(rec, httptest.NewRequest(http.MethodPost, "/api/sessions/conv-1/abort", nil))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code = %d, want 502", rec.Code)
	}
	if gw.lastBusySession != abortTestKey {
		t.Fatalf("reconcile read key = %q, want %q: a failed abort must be reconciled against the gateway", gw.lastBusySession, abortTestKey)
	}
}

// A failed abort RPC does not prove the stop did not happen. The frame is
// written before the response is waited for, so an abort that errors on its
// deadline can still have been delivered and honoured -- and treating that as
// "nothing happened" leaves the session's cards pending for a run that is
// already dead, to resurface on reload as cards nothing can answer. When the
// reconciliation read says the run is gone, the stop landed: settle, answer 200.
func TestHandleAbortFailedRPCThatLandedIsSuccess(t *testing.T) {
	h := NewHub()
	gw := &fakeAbortGateway{abortErr: errors.New("ws write chat.abort: context deadline exceeded")}
	m := &hitlManager{conns: map[string]*userHitlConn{"admin": {user: "admin", gw: gw}}}
	s := newAbortTestServer(h, m)
	s.approvals.Begin("admin", pendingApproval{ApprovalID: "ap-1", SessionKey: abortTestKey, User: "admin"})

	rec := httptest.NewRecorder()
	s.handleAbort(rec, httptest.NewRequest(http.MethodPost, "/api/sessions/conv-1/abort", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200: the gateway no longer has the run, so the stop landed", rec.Code)
	}
	// The settle is the consequence of the decision: a landed stop must clear the
	// records the run left behind, exactly as a successful RPC does.
	if _, ok := s.approvals.Pending("admin", abortTestKey); ok {
		t.Fatal("the run is gone but its pending confirmation survived: a reload resurrects a card for a dead run")
	}
}

// The reconciliation is not a licence to guess. If it cannot be answered -- no
// channel, a wedged read -- the run's fate is unknown, and settling on an
// unknown result would delete a live, answerable card for a run that never
// stopped. The records stay pending and the caller gets the 502.
func TestHandleAbortUnansweredReconcileKeepsRecords(t *testing.T) {
	h := NewHub()
	gw := &fakeAbortGateway{
		abortErr: errors.New("ws write chat.abort: context deadline exceeded"),
		busyErr:  errors.New("chat.history: connection closed"),
		busy:     true,
	}
	m := &hitlManager{conns: map[string]*userHitlConn{"admin": {user: "admin", gw: gw}}}
	s := newAbortTestServer(h, m)
	s.approvals.Begin("admin", pendingApproval{ApprovalID: "ap-1", SessionKey: abortTestKey, User: "admin"})

	rec := httptest.NewRecorder()
	s.handleAbort(rec, httptest.NewRequest(http.MethodPost, "/api/sessions/conv-1/abort", nil))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code = %d, want 502", rec.Code)
	}
	if _, ok := s.approvals.Pending("admin", abortTestKey); !ok {
		t.Fatal("the records were settled on an unknown result: the card for a run that may still be going is gone")
	}
}

// A chat.abort that answers *success* with aborted=false stopped nothing: the
// run id matched no abortable run, which a session-abortable-only run or one
// promoted between the in-flight read and the RPC both produce. The RPC's ok is
// not proof the run is gone, so the session must not be settled -- settling
// deletes the live run's cards and the 200 tells the Portal its follow-up send
// can go into a session that is still busy. The reconcile read is what tells the
// two apart, and here it confirms the worst one.
func TestHandleAbortAbortedNothingKeepsRecords(t *testing.T) {
	no := false
	h := NewHub()
	gw := &fakeAbortGateway{abortAborted: &no, busy: true}
	m := &hitlManager{conns: map[string]*userHitlConn{"admin": {user: "admin", gw: gw}}}
	s := newAbortTestServer(h, m)
	s.approvals.Begin("admin", pendingApproval{ApprovalID: "ap-1", SessionKey: abortTestKey, User: "admin"})

	rec := httptest.NewRecorder()
	s.handleAbort(rec, httptest.NewRequest(http.MethodPost, "/api/sessions/conv-1/abort", nil))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code = %d, want 502: the gateway aborted nothing and the session is still busy", rec.Code)
	}
	if gw.lastBusySession != abortTestKey {
		t.Fatalf("reconcile read key = %q, want %q: an abort that stopped nothing must be reconciled", gw.lastBusySession, abortTestKey)
	}
	if gw.listed {
		t.Fatal("settle ran against a run that is still in flight: the live run's cards are gone")
	}
	if _, ok := s.approvals.Pending("admin", abortTestKey); !ok {
		t.Fatal("the records were settled for a run that is still going: a card the user still needs was deleted")
	}
}

// The aborted=false case that is benign, and the reason the reconcile is a read
// rather than a refusal: chat.abort stopped nothing because there was nothing
// left to stop -- the run settled naturally in the window. The session is
// provably idle, so the ordinary path applies and the records are settled
// exactly as for an aborted=true.
func TestHandleAbortAbortedNothingOnAnIdleSessionSettles(t *testing.T) {
	no := false
	h := NewHub()
	gw := &fakeAbortGateway{abortAborted: &no}
	m := &hitlManager{conns: map[string]*userHitlConn{"admin": {user: "admin", gw: gw}}}
	s := newAbortTestServer(h, m)
	s.approvals.Begin("admin", pendingApproval{ApprovalID: "ap-1", SessionKey: abortTestKey, User: "admin"})

	rec := httptest.NewRecorder()
	s.handleAbort(rec, httptest.NewRequest(http.MethodPost, "/api/sessions/conv-1/abort", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200: nothing was aborted because nothing was running", rec.Code)
	}
	if _, ok := s.approvals.Pending("admin", abortTestKey); ok {
		t.Fatal("the session is idle but its pending confirmation survived: a reload resurrects a card for a dead run")
	}
}

// The reload-takeover path has no live turn -- releaseLive removed it when the
// request driving the turn ended -- so a run id has to come from the gateway.
// Without it the abort is session-scoped, and a run promoted between the RPC
// being processed and the run the user meant settling is the one it kills.
func TestHandleAbortScopesToGatewaysInFlightRun(t *testing.T) {
	h := NewHub()
	gw := &fakeAbortGateway{inFlightRun: "run-gateway"}
	m := &hitlManager{conns: map[string]*userHitlConn{"admin": {user: "admin", gw: gw}}}
	s := newAbortTestServer(h, m)

	rec := httptest.NewRecorder()
	s.handleAbort(rec, httptest.NewRequest(http.MethodPost, "/api/sessions/conv-1/abort", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if gw.inFlightSession != abortTestKey {
		t.Fatalf("in-flight read key = %q, want the canonical %q", gw.inFlightSession, abortTestKey)
	}
	if gw.lastAbortRunID != "run-gateway" {
		t.Fatalf("abort runID = %q, want the gateway's in-flight run: the session-scoped fallback can kill the next run", gw.lastAbortRunID)
	}
}

// The last resort, spelled out: when the in-flight lookup answers nothing, the
// abort stays session-scoped rather than being refused. Nothing is in flight, so
// this is the idempotent no-op case -- and the only remaining reason to reach
// the session-scoped form besides an unanswered read.
func TestHandleAbortFallsBackToSessionScope(t *testing.T) {
	h := NewHub()
	gw := &fakeAbortGateway{} // no live turn, no in-flight run
	m := &hitlManager{conns: map[string]*userHitlConn{"admin": {user: "admin", gw: gw}}}
	s := newAbortTestServer(h, m)

	rec := httptest.NewRecorder()
	s.handleAbort(rec, httptest.NewRequest(http.MethodPost, "/api/sessions/conv-1/abort", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if gw.lastAbortRunID != "" {
		t.Fatalf("abort runID = %q, want the empty (session-scoped) form when nothing is in flight", gw.lastAbortRunID)
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

// The gateway read is bounded, exactly as /abort's RPC is. The HTTP server sets
// no timeouts of its own, so a half-open gateway connection would park this
// handler for as long as the read takes and leak one blocked request per Portal
// refresh; the bound is what turns that into a single reported failure. The fake
// reports the context it was handed, which is the bound's only observable (an
// unbounded read simply never returns, so no fast test can wait for it).
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

// A missing local entry is not evidence of an idle session. The per-user
// connection is dialled lazily on first use, so after an API restart or a pod
// roll the replacement process has no entry in m.conns while a run the
// *previous* process started can still be executing gateway-side. Reading that
// missing entry as idle hides the running turn and takes away its Stop, which
// is the one answer this endpoint must never give.
//
// So /turn establishes the channel -- bounded, so a user who has simply never
// chatted dials straight through and gets a true answer -- and then asks the
// gateway. The fake reports a run in flight, so the assertion is the
// determination itself, not just the status code: the pre-fix handler answered
// 200 {"active": false} here from the absent entry alone.
func TestHandleTurnStatusEstablishesTheChannel(t *testing.T) {
	gw := &fakeHitlGateway{sessionBusy: true}
	m := &hitlManager{
		conns:     map[string]*userHitlConn{},
		newClient: func(string, *ws.Device) hitlGateway { return gw },
		wsURLOf:   func(string) string { return "ws://fake/gateway" },
	}
	// The state the finding is about: no channel in this process at all.
	if _, ok := m.liveConn("admin"); ok {
		t.Fatal("fixture: the manager must start with no channel for the user")
	}
	s := newAbortTestServer(NewHub(), m)

	rec := httptest.NewRecorder()
	s.handleTurnStatus(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/conv-1/turn", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	var body struct {
		Active bool `json:"active"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Active {
		t.Fatal("active = false with a run in flight at the gateway: a missing local entry cannot be read as idle")
	}
	if len(gw.sessionBuses) != 1 || gw.sessionBuses[0] != abortTestKey {
		t.Fatalf("busy reads = %v, want one on the canonical key %q", gw.sessionBuses, abortTestKey)
	}
	// Establishing is not a probe-and-drop: the connection is kept, so the Stop
	// this answer offers can actually be issued over it. A dropped channel would
	// make the offered Stop fail with no channel at all.
	if _, ok := m.liveConn("admin"); !ok {
		t.Fatal("the handler discarded the channel it established: the Stop it just offered would fail")
	}
}

// A connection that is no longer usable is replaced by the probe, and when the
// dial cannot be completed the answer is "cannot determine", not a false idle.
// This process had a channel for the user, so a turn it started can still be
// running while the connection is broken (a gateway restart or an API pod roll
// stops observation, not the run); the caller renders the 502 as could-not-check
// with a Retry.
func TestHandleTurnStatusDownChannelIsNotIdle(t *testing.T) {
	gw := &downAbortGateway{}
	m := &hitlManager{
		conns:     map[string]*userHitlConn{"admin": {user: "admin", gw: gw, connected: true}},
		newClient: func(string, *ws.Device) hitlGateway { return gw },
		wsURLOf:   func(string) string { return "ws://fake/gateway" },
	}

	if _, err := m.SessionBusy(context.Background(), "admin", abortTestKey); !errors.Is(err, errNoGatewayChannel) {
		t.Fatalf("SessionBusy on a down connection = %v, want errNoGatewayChannel", err)
	}

	s := newAbortTestServer(NewHub(), m)
	rec := httptest.NewRecorder()
	s.handleTurnStatus(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/conv-1/turn", nil))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code = %d, want 502: a turn this process started may still be running", rec.Code)
	}
	assertNoActiveClaim(t, rec.Body.Bytes())
}

// The same for an entry that is registered but has never carried a successful
// handshake -- the state conn holds for the whole of a dial, and for up to the
// 30s pairing retry when the device is not yet approved. Such an entry is not a
// channel, so the read re-dials rather than answering from it; if that dial
// cannot be completed there is no way to know whether the gateway has a run, and
// that is an error rather than the idle the pre-fix handler inferred from the
// unusable entry.
func TestHandleTurnStatusRedialsUnusableEntry(t *testing.T) {
	gw := &fakeHitlGateway{connectErr: errors.New("dial: gateway unreachable")}
	m := &hitlManager{
		// Registered, not connected: the handshake has not completed.
		conns:     map[string]*userHitlConn{"admin": {user: "admin", gw: gw}},
		newClient: func(string, *ws.Device) hitlGateway { return gw },
		wsURLOf:   func(string) string { return "ws://fake/gateway" },
	}
	s := newAbortTestServer(NewHub(), m)

	rec := httptest.NewRecorder()
	s.handleTurnStatus(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/conv-1/turn", nil))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code = %d, want 502: a channel that cannot be established means the busy state is unknown", rec.Code)
	}
	assertNoActiveClaim(t, rec.Body.Bytes())
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
	// abortAborted is chat.abort's success payload flag. nil means "the RPC
	// aborted the run"; a pointer to false models the gateway's
	// {ok:true, aborted:false, runIds:[]} answer, which is a successful RPC that
	// stopped nothing.
	abortAborted *bool
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
	// inFlightRun is the runId chat.history reports as in flight (empty means
	// the gateway reports no run), and inFlightErr makes that read fail.
	inFlightRun string
	inFlightErr error
	// inFlightSession records the key the in-flight-run read was made with, so a
	// test can tell the reload-takeover lookup apart from a local run id.
	inFlightSession string
}

// AbortChat models the gateway's chat.abort. abortAborted is the success
// payload's `aborted` flag; a fixture that leaves it nil means the RPC stopped
// the run, which is what the abort tests written before this flag existed
// assume.
func (f *fakeAbortGateway) AbortChat(ctx context.Context, sessionKey, runID string) (bool, error) {
	f.lastAbortSession, f.lastAbortRunID = sessionKey, runID
	f.abortCtxErr = ctx.Err()
	// A failed RPC carries no payload, so there is no abort to report.
	if f.abortErr != nil {
		return false, f.abortErr
	}
	if f.abortAborted != nil {
		return *f.abortAborted, nil
	}
	return true, nil
}

func (f *fakeAbortGateway) SessionBusy(ctx context.Context, sessionKey string) (bool, error) {
	f.lastBusySession = sessionKey
	f.busyCtxDeadline, f.busyCtxHasDeadline = ctx.Deadline()
	if f.busyFunc != nil {
		return f.busyFunc(ctx)
	}
	return f.busy, f.busyErr
}

func (f *fakeAbortGateway) SessionInFlightRun(ctx context.Context, sessionKey string) (string, error) {
	f.inFlightSession = sessionKey
	return f.inFlightRun, f.inFlightErr
}

// Connected and ListQuestions are not incidental: the settle step reaches the
// gateway through hitlManager.liveConn, which drops a connection that is not
// usable, and then lists its questions. Without an explicit Connected the
// promoted method would run on the embedded nil interface and panic; without
// ListQuestions the settle would panic one call later. This session has no open
// questions, which is the state the happy path settles in.
func (f *fakeAbortGateway) Connected() bool { return true }

// downAbortGateway is a per-user connection that is not usable right now. It
// shares the fake's busy answer, which is unreachable -- a down connection is
// dropped by liveConn before any RPC, and /turn's probe replaces it with a dial,
// which fails here.
//
// On its own it models either of the two states the sentinel collapses: a dial
// still handshaking, and an established channel that has gone down. Which one it
// is depends solely on the `connected` flag of the entry it is stored in, which
// is also what decides whether the failure is reported as "a channel was once
// established" in the log -- the distinction the handler no longer branches on,
// because both end as the same honest "cannot determine".
type downAbortGateway struct{ fakeHitlGateway }

func (f *downAbortGateway) Connected() bool { return false }

// Connect fails: the re-dial /turn makes cannot reach the gateway, which is what
// keeps the answer at "cannot determine".
func (f *downAbortGateway) Connect(context.Context) error {
	return errors.New("dial: gateway unreachable")
}

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
