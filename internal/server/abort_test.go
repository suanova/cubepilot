package server

import (
	"context"
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

// fakeAbortGateway is the hitlGateway slice /abort exercises.
type fakeAbortGateway struct {
	hitlGateway      // embed for the methods this test never calls
	busy             bool
	busyErr          error
	abortErr         error
	lastAbortSession string
	lastAbortRunID   string
}

func (f *fakeAbortGateway) AbortChat(_ context.Context, sessionKey, runID string) error {
	f.lastAbortSession, f.lastAbortRunID = sessionKey, runID
	return f.abortErr
}

func (f *fakeAbortGateway) SessionBusy(context.Context, string) (bool, error) {
	return f.busy, f.busyErr
}

// Connected and ListQuestions are not incidental: the settle step reaches the
// gateway through hitlManager.liveConn, which drops a connection that is not
// usable, and then lists its questions. Without an explicit Connected the
// promoted method would run on the embedded nil interface and panic; without
// ListQuestions the settle would panic one call later. This session has no open
// questions, which is the state the happy path settles in.
func (f *fakeAbortGateway) Connected() bool { return true }

func (f *fakeAbortGateway) ListQuestions(context.Context) ([]ws.QuestionRecord, error) {
	return nil, nil
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
