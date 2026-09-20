package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/suanova/cubepilot/internal/config"
	"github.com/suanova/cubepilot/internal/openclaw/ws"
)

// deleteTestKey is the gateway-canonical form of the "conv-1" segment the client
// sends in the URL, for the same reason abortTestKey is: the gateway addresses
// sessions by the canonical key, so a handler that forwarded the raw path
// segment would delete nothing and report success.
const deleteTestKey = "agent:main:conv-1"

// deleteConns registers one live gateway connection for the default user.
func deleteConns(gw gatewayClient) *gatewayConns {
	return &gatewayConns{conns: map[string]*userGatewayConn{"admin": {user: "admin", gw: gw}}}
}

// The happy path: the session is gone, and the answer is flat
// (api-conventions.md §4) -- deleted and archived beside each other, not wrapped
// in an envelope the client would have to know the name of.
func TestHandleSessionDeleteClearsTheConversation(t *testing.T) {
	gw := &fakeGatewayClient{connected: true, deleteResult: ws.SessionDeleteResult{
		OK: true, Key: deleteTestKey, Deleted: true, Archived: []string{"sessions/conv-1.jsonl"},
	}}
	s := newAbortTestServer(NewHub(), deleteConns(gw))

	rec := httptest.NewRecorder()
	s.handleSessionDelete(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/conv-1", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (body = %s)", rec.Code, rec.Body.String())
	}
	if len(gw.deletes) != 1 || gw.deletes[0] != deleteTestKey {
		t.Fatalf("deleted keys = %v, want one on the canonical key %q", gw.deletes, deleteTestKey)
	}
	var body struct {
		Deleted           bool                         `json:"deleted"`
		Archived          []string                     `json:"archived"`
		WorktreePreserved *ws.PreservedSessionWorktree `json:"worktreePreserved"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	if !body.Deleted {
		t.Error("deleted = false, want the gateway's answer")
	}
	if len(body.Archived) != 1 || body.Archived[0] != "sessions/conv-1.jsonl" {
		t.Errorf("archived = %v, want the gateway's list", body.Archived)
	}
	if body.WorktreePreserved != nil {
		t.Errorf("worktreePreserved = %+v, want it absent when nothing survived", *body.WorktreePreserved)
	}
}

// The idempotent case, and the one a fixed-key client hits on every press of
// Clear: the key is not there, the gateway answers deleted=false, and that is a
// success -- not a 404 the client would have to special-case.
func TestHandleSessionDeleteReportsANothingToDelete(t *testing.T) {
	gw := &fakeGatewayClient{connected: true, deleteResult: ws.SessionDeleteResult{
		OK: true, Key: deleteTestKey, Deleted: false, Archived: []string{},
	}}
	s := newAbortTestServer(NewHub(), deleteConns(gw))

	rec := httptest.NewRecorder()
	s.handleSessionDelete(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/conv-1", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200: a key that is not there is not an error", rec.Code)
	}
	var body struct {
		Deleted bool `json:"deleted"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.Bytes(), err)
	}
	if body.Deleted {
		t.Error("deleted = true for a session the gateway says was not there")
	}
}

// `archived` is a list, so an empty one serializes as `[]`. The gateway omits
// the field when it archived nothing; relaying that as `null` would make every
// client branch before iterating, for a value that is documented as a list.
func TestHandleSessionDeleteSendsAnEmptyListNotNull(t *testing.T) {
	gw := &fakeGatewayClient{connected: true, deleteResult: ws.SessionDeleteResult{
		OK: true, Key: deleteTestKey, Deleted: true, // Archived left nil
	}}
	s := newAbortTestServer(NewHub(), deleteConns(gw))

	rec := httptest.NewRecorder()
	s.handleSessionDelete(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/conv-1", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.Bytes(), err)
	}
	if got := string(raw["archived"]); got != "[]" {
		t.Fatalf("archived = %s, want []", got)
	}
}

// "Clear" does not promise the instance is indistinguishable from never having
// talked: the runtime can leave a worktree behind, and the answer has to say so
// rather than let the caller assume otherwise.
func TestHandleSessionDeleteReportsWhatSurvived(t *testing.T) {
	preserved := &ws.PreservedSessionWorktree{
		ID: "wt-1", Branch: "conv-1", Path: "/work/wt-1", Reason: "uncommitted changes",
	}
	gw := &fakeGatewayClient{connected: true, deleteResult: ws.SessionDeleteResult{
		OK: true, Key: deleteTestKey, Deleted: true, Archived: []string{}, WorktreePreserved: preserved,
	}}
	s := newAbortTestServer(NewHub(), deleteConns(gw))

	rec := httptest.NewRecorder()
	s.handleSessionDelete(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/conv-1", nil))

	var body struct {
		WorktreePreserved *ws.PreservedSessionWorktree `json:"worktreePreserved"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.Bytes(), err)
	}
	if body.WorktreePreserved == nil || *body.WorktreePreserved != *preserved {
		t.Fatalf("worktreePreserved = %+v, want %+v", body.WorktreePreserved, preserved)
	}
}

// The documented path detail (api.md §2.5): the key is everything after the
// prefix, so a key that itself contains a slash is reachable, and the gateway
// sees the same key the client named.
func TestHandleSessionDeleteAbsorbsExtraSegmentsIntoTheKey(t *testing.T) {
	gw := &fakeGatewayClient{connected: true, deleteResult: ws.SessionDeleteResult{OK: true}}
	s := newAbortTestServer(NewHub(), deleteConns(gw))

	rec := httptest.NewRecorder()
	s.handleSessionDelete(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/a/b", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if len(gw.deletes) != 1 || gw.deletes[0] != "agent:main:a/b" {
		t.Fatalf("deleted keys = %v, want [agent:main:a/b]", gw.deletes)
	}
}

// A session the gateway cannot safely stop is a retry, not a failure of the
// request: the caller did nothing wrong, and the client does not have to abort
// first -- the gateway stops active work itself. UNAVAILABLE is the answer the
// gateway gives for exactly that, and it carries no structured reason, so the
// code is the only thing that identifies it.
func TestHandleSessionDeleteConflictWhenTheGatewayCannotStopIt(t *testing.T) {
	gw := &fakeGatewayClient{connected: true, deleteErr: &ws.RPCError{
		Code:    "UNAVAILABLE",
		Message: "Session " + deleteTestKey + " is still active; try again.",
	}}
	s := newAbortTestServer(NewHub(), deleteConns(gw))

	rec := httptest.NewRecorder()
	s.handleSessionDelete(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/conv-1", nil))

	if rec.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409 (body = %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "still active") {
		t.Fatalf("body = %s, want the gateway's message so the client can retry", rec.Body.String())
	}
}

// The other retryable refusal: the session moved between the gateway reading it
// and deleting it. It is an INVALID_REQUEST, whose reason is the only thing that
// separates it from a malformed request -- so the classification has to consult
// the reason and not the code alone.
func TestHandleSessionDeleteConflictWhenTheSessionChanged(t *testing.T) {
	gw := &fakeGatewayClient{connected: true, deleteErr: &ws.RPCError{
		Code:    "INVALID_REQUEST",
		Message: "Session " + deleteTestKey + " changed before deletion. Retry.",
		Reason:  sessionLifecycleChangedReason,
	}}
	s := newAbortTestServer(NewHub(), deleteConns(gw))

	rec := httptest.NewRecorder()
	s.handleSessionDelete(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/conv-1", nil))

	if rec.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409 (body = %s)", rec.Code, rec.Body.String())
	}
}

// The same code with any other reason is the gateway refusing this request
// itself, not the retryable "it changed under you" case: a protected session
// (DELETEs of the bare-key route's `main` canonicalise to `agent:main:main`,
// which OpenClaw protects), or one whose model selection is locked. That is a
// request problem -- 400, per api-conventions.md §5 -- and must not be a 409
// that tells the client to retry, nor a 502 that blames the backend link and
// invites a retry that can never succeed.
func TestHandleSessionDeleteOtherInvalidRequestIsARequestProblem(t *testing.T) {
	gw := &fakeGatewayClient{connected: true, deleteErr: &ws.RPCError{
		Code:    "INVALID_REQUEST",
		Message: "Session " + deleteTestKey + " is protected and cannot be deleted",
		Reason:  "PROTECTED_SESSION",
	}}
	s := newAbortTestServer(NewHub(), deleteConns(gw))

	rec := httptest.NewRecorder()
	s.handleSessionDelete(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/conv-1", nil))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400 (body = %s)", rec.Code, rec.Body.String())
	}
	// The gateway's message is the body: it is the text that says which refusal
	// this was, so the client can tell the user rather than retry blindly.
	if !strings.Contains(rec.Body.String(), "protected") {
		t.Fatalf("body = %s, want the gateway's message", rec.Body.String())
	}
}

// FORBIDDEN is the platform's own fault, not the caller's: the paired device was
// not granted operator.admin, which no client retry can fix. It is reported as
// this process's failure to reach the gateway rather than as a conflict.
func TestHandleSessionDeleteForbiddenIsThisProcessesFault(t *testing.T) {
	gw := &fakeGatewayClient{connected: true, deleteErr: &ws.RPCError{
		Code:    "FORBIDDEN",
		Message: "missing scope operator.admin",
	}}
	s := newAbortTestServer(NewHub(), deleteConns(gw))

	rec := httptest.NewRecorder()
	s.handleSessionDelete(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/conv-1", nil))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code = %d, want 502 (body = %s)", rec.Code, rec.Body.String())
	}
}

// A call that ran out of time is a timeout, not a failed round trip:
// api-conventions.md §5 maps it to 504, which is the answer /abort's wait already
// gives these same two errors with StatusGatewayTimeout. 502 is left for a
// gateway round trip that failed on its own -- so a deadline must be classified
// before the code/reason mapping is consulted, and not by falling through it.
func TestHandleSessionDeleteTimeoutIsGatewayTimeout(t *testing.T) {
	gw := &fakeGatewayClient{connected: true, deleteErr: context.DeadlineExceeded}
	s := newAbortTestServer(NewHub(), deleteConns(gw))

	rec := httptest.NewRecorder()
	s.handleSessionDelete(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/conv-1", nil))

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("code = %d, want 504 (body = %s)", rec.Code, rec.Body.String())
	}
	// The body is client-facing. `err.Error()` on this path is
	// "context deadline exceeded", which the Portal would otherwise render.
	assertErrorBody(t, rec.Body.Bytes(), "the session delete did not finish in time; retrying it is safe and idempotent")
	if strings.Contains(rec.Body.String(), "context deadline exceeded") {
		t.Fatalf("body = %s, want a client-facing message, not the Go error text", rec.Body.String())
	}
}

// A user with no live connection gets 503, and nothing is sent: the delete is
// issued over the connection a client has been talking through, and a button
// press must not dial a new one (which can trigger a device pairing). The
// assertion on the fake is the point -- a 503 that still asked the gateway would
// be a delete that happened while the caller was told it had not.
func TestHandleSessionDeleteWithoutALiveConnection(t *testing.T) {
	gw := &fakeGatewayClient{} // registered but not connected
	m := &gatewayConns{conns: map[string]*userGatewayConn{"admin": {user: "admin", gw: gw}}}
	s := newAbortTestServer(NewHub(), m)

	rec := httptest.NewRecorder()
	s.handleSessionDelete(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/conv-1", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503 (body = %s)", rec.Code, rec.Body.String())
	}
	assertErrorBody(t, rec.Body.Bytes(), "session delete channel unavailable")
	if len(gw.deletes) != 0 {
		t.Fatalf("deletes = %v, want none: the channel was unavailable", gw.deletes)
	}
}

// The endpoint's input guards, plus the method check the conventions require of
// every route (api-conventions.md §6).
func TestHandleSessionDeleteRejectsBadRequests(t *testing.T) {
	cases := []struct {
		name    string
		method  string
		path    string
		channel bool
		want    int
	}{
		{name: "method", method: http.MethodGet, path: "/api/v1/sessions/conv-1", channel: true, want: http.StatusMethodNotAllowed},
		{name: "empty key", method: http.MethodDelete, path: "/api/v1/sessions/", channel: true, want: http.StatusBadRequest},
		{name: "main key", method: http.MethodDelete, path: "/api/v1/sessions/agent:main:", channel: true, want: http.StatusBadRequest},
		{name: "no channel", method: http.MethodDelete, path: "/api/v1/sessions/conv-1", channel: false, want: http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var m *gatewayConns
			if tc.channel {
				m = deleteConns(&fakeGatewayClient{connected: true})
			}
			s := newAbortTestServer(NewHub(), m)

			rec := httptest.NewRecorder()
			s.handleSessionDelete(rec, httptest.NewRequest(tc.method, tc.path, nil))

			if rec.Code != tc.want {
				t.Fatalf("code = %d, want %d (body = %s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// A correct handler the dispatcher never reaches is no feature at all: DELETE on
// a bare key would fall through to the mux's 404, and a fixed-key client would
// have no way to start over. This drives the real Handler chain. With no
// gateway channel the delete answers 503 -- the truthful answer for "there is
// nothing to send this over" -- which keeps the assertion on routing rather
// than on the gateway.
func TestSessionDeleteRouteIsWired(t *testing.T) {
	srv := New(config.Config{DefaultUser: "alice"}, nil, nil, nil, nil)

	rec := doReq(t, srv.Handler(), http.MethodDelete, "/api/v1/sessions/conv-1", "alice", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503: DELETE /api/v1/sessions/{key} is not wired into handleSessionSubresource (body = %s)", rec.Code, rec.Body.String())
	}
	assertErrorBody(t, rec.Body.Bytes(), "session delete channel unavailable")

	// The route exists, so the wrong method on it is a 405 -- not the 404 of a
	// path nothing serves, which would read as "this endpoint does not exist".
	rec = doReq(t, srv.Handler(), http.MethodGet, "/api/v1/sessions/conv-1", "alice", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("code = %d, want 405 for GET on the bare-key route (body = %s)", rec.Code, rec.Body.String())
	}
}

// A key that looks like a subresource is still a key. DELETE is matched before
// the reserved-suffix switch (see handleSessionSubresource), because no
// subresource under this prefix accepts it -- they are read with GET and acted
// on with POST. Driven through the real Handler chain, since the routing is the
// whole point: with the suffixes winning, this path reached history and was
// answered 405, and the session the path names was never deleted.
func TestSessionDeleteTreatsAKeyEndingInASubresourceSuffixAsAKey(t *testing.T) {
	const key = "agent:main:a/messages"
	gw := &fakeGatewayClient{connected: true, deleteResult: ws.SessionDeleteResult{OK: true, Key: key}}
	s := newAbortTestServer(NewHub(), deleteConns(gw))

	rec := doReq(t, s.Handler(), http.MethodDelete, "/api/v1/sessions/"+key, "admin", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200: the whole remainder is the key, reserved suffixes included (body = %s)", rec.Code, rec.Body.String())
	}
	if len(gw.deletes) != 1 || gw.deletes[0] != key {
		t.Fatalf("deleted keys = %v, want one on %q", gw.deletes, key)
	}
}

// The gateway call is bounded, exactly as /abort's and /turn's are. The HTTP
// server sets no timeouts of its own, so a half-open gateway connection would
// park this handler -- and the Portal's Clear button -- for as long as the
// socket lives. The fake reports the context it was handed, which is the bound's
// only observable: an unbounded call never returns at all, so no fast test can
// wait for the difference.
func TestHandleSessionDeleteBoundsTheGatewayCall(t *testing.T) {
	gw := &fakeGatewayClient{connected: true}
	s := newAbortTestServer(NewHub(), deleteConns(gw))

	rec := httptest.NewRecorder()
	s.handleSessionDelete(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/conv-1", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if !gw.deleteCtxHasDeadline {
		t.Fatal("the delete ran on a context with no deadline: a wedged gateway would hold this handler open indefinitely")
	}
	// The slack is for the moment between this test's clock and the handler
	// setting its bound.
	if d := time.Until(gw.deleteCtxDeadline); d <= 0 || d > sessionDeleteRPCTimeout+time.Second {
		t.Fatalf("delete deadline = now+%v, want (0, %v]", d, sessionDeleteRPCTimeout)
	}
}

// The delete's bound is derived from what the gateway does inside the call, not
// picked: it drains the session's active work first, a drain capped at 15s
// (SESSION_WORK_ADMISSION_DRAIN_TIMEOUT_MS in the gateway's
// session-lifecycle-admission.ts), and the deletion and worktree cleanup follow
// it inside the same call. A bound at or below that drain reports a timeout for
// a delete that is still running and going to succeed -- and the gateway
// finishes it anyway, because ws.Client.Call sends no cancellation.
//
// The gateway's source is not in this repo, so the number cannot be checked
// against it; it is checked against the ceiling that source documents. This is
// the guard for the derivation, and the reason a later "5s is plenty" edit has
// to argue with a test rather than with a comment.
func TestSessionDeleteRPCTimeoutClearsTheGatewayDrain(t *testing.T) {
	// A variable rather than a const on purpose: the gateway's cap is a value to
	// compare against at run time, not a constant to fold.
	drainCap := 15 * time.Second
	if sessionDeleteRPCTimeout <= drainCap {
		t.Fatalf("sessionDeleteRPCTimeout = %v, want more than the gateway's %v work-admission drain: a delete that is still draining would be answered as a timeout", sessionDeleteRPCTimeout, drainCap)
	}
}

// A Clear is a command, not a read: a client that presses it and then
// disconnects must not turn the delete into a silent no-op. The call is detached
// from the request, the way /abort's commands are, so a cancelled request
// context leaves it running -- the session is deleted, and the answer is written
// to a response the client may no longer be there to read. Without the detach
// this handler would answer 504 and clear nothing.
func TestHandleSessionDeleteSurvivesAClientDisconnect(t *testing.T) {
	gw := &fakeGatewayClient{connected: true, deleteResult: ws.SessionDeleteResult{
		OK: true, Key: deleteTestKey, Deleted: true, Archived: []string{},
	}}
	s := newAbortTestServer(NewHub(), deleteConns(gw))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	s.handleSessionDelete(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/conv-1", nil).WithContext(ctx))

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200: the delete is detached from the request (body = %s)", rec.Code, rec.Body.String())
	}
	if len(gw.deletes) != 1 || gw.deletes[0] != deleteTestKey {
		t.Fatalf("deleted keys = %v, want one on %q: the delete must still reach the gateway", gw.deletes, deleteTestKey)
	}
}

// deleteIssuedGateway reports when the delete has been issued, so the settle
// test below can assert on what the handler does afterwards without racing the
// goroutine that runs it. Polling the fake's recorded keys for the same purpose
// would be a data race, and a bare sleep would let a handler that answers
// immediately pass as one that waited.
type deleteIssuedGateway struct {
	*fakeGatewayClient
	issued chan struct{}
}

func (g *deleteIssuedGateway) DeleteSession(ctx context.Context, key string) (ws.SessionDeleteResult, error) {
	select {
	case <-g.issued:
	default:
		close(g.issued)
	}
	return g.fakeGatewayClient.DeleteSession(ctx, key)
}

// A successful delete stops the gateway's run, but the terminal frame that
// releases this process's parked turn arrives asynchronously: the session's SSE
// stream can still be registered when the delete returns, and a client that
// starts the fresh conversation at that instant races Hub.Open into `409 another
// turn is already streaming`. The handler waits for the stream to release before
// it answers, which is what this pins: without the wait the 200 lands while the
// stream is still open, and the assertion below is that it has not.
func TestHandleSessionDeleteWaitsForTheSessionStreamToRelease(t *testing.T) {
	hub := NewHub()
	stream, err := hub.Open(deleteTestKey, httptest.NewRecorder(), httptest.NewRecorder())
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	gw := &deleteIssuedGateway{
		fakeGatewayClient: &fakeGatewayClient{connected: true, deleteResult: ws.SessionDeleteResult{
			OK: true, Key: deleteTestKey, Deleted: true, Archived: []string{},
		}},
		issued: make(chan struct{}),
	}
	s := newAbortTestServer(hub, deleteConns(gw))

	rec := httptest.NewRecorder()
	answered := make(chan struct{})
	go func() {
		defer close(answered)
		s.handleSessionDelete(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/conv-1", nil))
	}()

	// Once the delete has been issued, the handler is at or past it and the only
	// thing left ahead of its answer is the wait under test. The pause then gives
	// a handler without the wait -- which answers as soon as it is scheduled --
	// time to do exactly that, while one that waits parks until the stream closes
	// below.
	select {
	case <-gw.issued:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler never issued the delete")
	}
	select {
	case <-answered:
		t.Fatal("the handler answered with the session's stream still open: the client's very next send would race Hub.Open into a 409")
	case <-time.After(150 * time.Millisecond):
	}

	stream.Close()

	select {
	case <-answered:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler did not answer after the stream released")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (body = %s)", rec.Code, rec.Body.String())
	}
	// The property the wait exists for: with the 200 read, the follow-up send can
	// no longer 409.
	if _, err := hub.Open(deleteTestKey, httptest.NewRecorder(), httptest.NewRecorder()); err != nil {
		t.Fatalf("Open after the handler answered: %v", err)
	}
}

// The settle wait is bounded, and its expiry is not a failure of the delete: the
// delete succeeded, and the client is told that along with the fact that the
// session's turn has not released yet -- the one case in which a client must not
// act on the delete as if the session were free. Shortened the way
// abortSettleTimeout is in its own test: the production bound is seconds long
// while the stream below never closes.
func TestHandleSessionDeleteSettleTimeoutAnswers504(t *testing.T) {
	shortenSessionDeleteSettleTimeout(t, 100*time.Millisecond)

	hub := NewHub()
	streamRec := httptest.NewRecorder()
	stream, err := hub.Open(deleteTestKey, streamRec, streamRec)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer stream.Close()

	gw := &fakeGatewayClient{connected: true, deleteResult: ws.SessionDeleteResult{
		OK: true, Key: deleteTestKey, Deleted: true, Archived: []string{},
	}}
	s := newAbortTestServer(hub, deleteConns(gw))

	rec := httptest.NewRecorder()
	s.handleSessionDelete(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/conv-1", nil))

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("code = %d, want 504 (body = %s)", rec.Code, rec.Body.String())
	}
	assertErrorBody(t, rec.Body.Bytes(), "the conversation was deleted, but the session's turn did not release in time; retry (the delete is idempotent)")
	// The delete is not the thing that failed: it reached the gateway, and the
	// same bound expiry on the RPC itself would have answered the other 504.
	if len(gw.deletes) != 1 || gw.deletes[0] != deleteTestKey {
		t.Fatalf("deleted keys = %v, want one on %q: the session was deleted", gw.deletes, deleteTestKey)
	}
}

// shortenSessionDeleteSettleTimeout replaces the settle bound for one test. The
// 504 branch is otherwise reachable only by holding a stream open for the whole
// production budget.
func shortenSessionDeleteSettleTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := sessionDeleteSettleTimeout
	sessionDeleteSettleTimeout = d
	t.Cleanup(func() { sessionDeleteSettleTimeout = prev })
}

// assertErrorBody pins the one body shape every error uses: {"error": "..."}
// (api-conventions.md §5).
func assertErrorBody(t *testing.T, body []byte, want string) {
	t.Helper()
	var decoded struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if decoded.Error != want {
		t.Fatalf("error = %q, want %q", decoded.Error, want)
	}
}
