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

// The same code with a different reason is not the retryable case, and must not
// be answered as one: a malformed request reported as "conflict" would tell the
// client to retry something that can never succeed.
func TestHandleSessionDeleteOtherInvalidRequestIsNotAConflict(t *testing.T) {
	gw := &fakeGatewayClient{connected: true, deleteErr: &ws.RPCError{
		Code:    "INVALID_REQUEST",
		Message: "expectedSessionId does not match",
		Reason:  "SESSION_MISMATCH",
	}}
	s := newAbortTestServer(NewHub(), deleteConns(gw))

	rec := httptest.NewRecorder()
	s.handleSessionDelete(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/conv-1", nil))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code = %d, want 502 (body = %s)", rec.Code, rec.Body.String())
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
		name   string
		method string
		path   string
		hitl   bool
		want   int
	}{
		{name: "method", method: http.MethodGet, path: "/api/v1/sessions/conv-1", hitl: true, want: http.StatusMethodNotAllowed},
		{name: "empty key", method: http.MethodDelete, path: "/api/v1/sessions/", hitl: true, want: http.StatusBadRequest},
		{name: "main key", method: http.MethodDelete, path: "/api/v1/sessions/agent:main:", hitl: true, want: http.StatusBadRequest},
		{name: "no hitl", method: http.MethodDelete, path: "/api/v1/sessions/conv-1", hitl: false, want: http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var m *gatewayConns
			if tc.hitl {
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
// have no way to start over. This drives the real Handler chain. With no HITL
// channel the delete answers 503 -- the truthful answer for "there is nothing to
// send this over" -- which keeps the assertion on routing rather than on the
// gateway.
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
