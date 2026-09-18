package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/suanova/cubepilot/internal/openclaw/ws"
)

// sessionLifecycleChangedReason is the gateway's structured reason for an
// INVALID_REQUEST that means the session moved between the caller's read and
// the delete (SESSION_LIFECYCLE_CHANGED_ERROR_REASON in the gateway source,
// whose value is this string). It is a retry, not a failure.
const sessionLifecycleChangedReason = "session-changed"

// sessionDeleteRPCTimeout bounds the sessions.delete RPC, for the reason /abort
// and /turn bound theirs: this HTTP server sets no timeouts at all, so a
// half-open gateway connection would park the handler -- and the Portal's Clear
// button -- for as long as the socket lives. The call runs on a context
// detached from the request (see handleSessionDelete), so this bound is the only
// thing that can end it.
//
// The number is derived from what the gateway does inside this call, not picked.
// Before it deletes anything, the runtime drains the session's active work, and
// that drain has a cap of its own: 15s
// (SESSION_WORK_ADMISSION_DRAIN_TIMEOUT_MS in session-lifecycle-admission.ts).
// The deletion and the worktree cleanup happen after the drain, inside the same
// call. A bound at or below the drain therefore expires in the middle of a
// legitimately busy delete and reports a timeout for one that is still running
// and going to succeed -- and the gateway keeps going after we answer, because
// ws.Client.Call sends no cancellation when its context ends: it drops the
// pending entry and returns ctx.Err(), leaving the drain and the delete to
// finish on their own. Twice the drain leaves room for the deletion and the
// cleanup that follow it.
//
// Expiry is a timeout, answered as 504 (api-conventions.md §5), which is safe to
// retry: the delete is idempotent, so a client that retries after one that may
// have landed gets `deleted:false` and the same end state.
const sessionDeleteRPCTimeout = 30 * time.Second

// sessionDeleteSettleTimeout bounds the wait for this session's own SSE stream to
// release after a successful delete (see handleSessionDelete): the gateway stops
// the run, but the terminal frame that releases our parked turn arrives
// asynchronously, so this process can still hold the stream when the delete
// returns.
//
// A package-level var, in the style of abortSettleTimeout, so a test can shorten
// it: as a const the 504 branch below would be reachable only by holding a
// stream open for this whole budget.
var sessionDeleteSettleTimeout = 5 * time.Second

// handleSessionDelete serves DELETE /api/v1/sessions/{key} -- a client with one
// fixed session key per user clearing its conversation.
//
// It deletes the session and its transcript, so the next turn under the same
// key starts a fresh conversation. Idempotence comes from the protocol: a key
// that does not exist is a success with deleted=false, not a 404, which is the
// answer a client that clears the same key every time needs.
//
// It does not abort first, and the caller does not have to: the gateway drains
// and stops active work as part of the delete. What the gateway will not do is
// delete a session that it cannot safely stop (UNAVAILABLE) or one that changed
// while the call was in flight (INVALID_REQUEST / session-changed); both are
// 409, and both mean "the turn is still going or just moved -- retry once it
// stops". A third refusal is the request itself being unservable -- a protected
// session, or one whose model selection is locked -- which is an
// INVALID_REQUEST carrying no session-changed reason and no retry that could
// help: 400 (see sessionDeleteErrorStatus).
//
// The answer is flat (api-conventions.md §4: the fields of one thing are not
// wrapped): deleted, archived, and worktreePreserved when a worktree survived.
// That last field is the honest caveat of this endpoint -- "cleared" does not
// promise the instance is indistinguishable from never having talked (see
// docs/cubepilot/api.md §4.7).
//
// A delete that landed is not answered yet: the 200 waits for this session's own
// SSE stream to release first, so a client's immediate follow-up send cannot race
// the hub into a 409 (see the wait below). That wait, and only that wait, follows
// the request context -- the delete ahead of it stays detached.
//
// The gateway call is detached from the request and bounded: pressing Clear and
// then navigating away still clears the session, and because the request's own
// cancellation no longer reaches the call, a deadline on it can only be that
// bound expiring -- answered as 504, never as a client that gave up.
func (s *Server) handleSessionDelete(w http.ResponseWriter, r *http.Request) {
	// The bare-key route has no other method: the path names a resource, and
	// DELETE is the only thing to do to it (api-conventions.md §6).
	if r.Method != http.MethodDelete {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "DELETE required"})
		return
	}
	user := s.userOf(r)
	// No suffix to trim on this route: everything after the prefix is the key,
	// so extra segments are absorbed into it exactly as the subresource routes
	// absorb them (see handleSessionSubresource).
	sessionKey := canonicalSessionKey(subresourceKey(r.URL.Path, ""))
	if sessionKey == "" || sessionKey == "agent:main:" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing session key"})
		return
	}
	if s.gatewayConns == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "session delete channel unavailable"})
		return
	}
	// Detached from the request, exactly as /abort's commands are
	// (context.WithoutCancel(r.Context()) under abortRPCDeadline, see handleAbort
	// in abort.go): a Clear is a command, and a client that presses it and then
	// disconnects must not turn the delete into a silent no-op -- the session
	// would stay, and the next turn would carry on a conversation the user
	// believes they cleared. Detaching is also why the bound below is the only
	// bound there is: the request's own cancellation no longer reaches the call.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), sessionDeleteRPCTimeout)
	defer cancel()
	res, err := s.gatewayConns.DeleteSession(ctx, user, sessionKey)
	if err != nil {
		s.writeSessionDeleteGatewayError(w, user, sessionKey, err)
		return
	}
	archived := res.Archived
	if archived == nil {
		// The gateway omits the field when nothing was archived. It is a list of
		// one thing's fields, so an empty list is `[]`: `null` would make every
		// client branch before iterating.
		archived = []string{}
	}
	body := map[string]any{"deleted": res.Deleted, "archived": archived}
	if res.WorktreePreserved != nil {
		body["worktreePreserved"] = res.WorktreePreserved
	}

	// The delete landed, but this process can still be holding the session's SSE
	// stream: the gateway stops the run, and the terminal frame that releases the
	// parked turn here arrives asynchronously, so a client that starts the fresh
	// conversation the moment it reads this 200 would race Hub.Open into `409
	// another turn is already streaming`. That is the race /abort's settle exists
	// to prevent, and it is why this answer waits for the local stream rather than
	// trusting the gateway's reply alone.
	//
	// The wait follows the request context, not the detached delete context above,
	// for /abort's reason: its answer is only meaningful to the caller still
	// holding the request open, and a client that gave up must not pin this handler
	// for the whole budget.
	//
	// /abort's wait also polls the gateway for "is a run still in flight". That
	// check is deliberately not repeated here: it earns its round trip there
	// because a run can outlive this process's knowledge of it, and it is the only
	// signal left on the reload-takeover path where no stream exists. This session
	// no longer exists -- the call above just deleted it -- so SessionBusy has
	// nothing to read, and the local stream is the only thing that can still hold a
	// follow-up send back.
	settleCtx, cancelSettle := context.WithTimeout(r.Context(), sessionDeleteSettleTimeout)
	defer cancelSettle()
	if err := s.hub.WaitIdle(settleCtx, sessionKey); errors.Is(err, context.Canceled) {
		// The caller is gone. The delete landed, so there is no failure to report
		// and nobody left to read one: answering the success it is -- to a
		// connection that is no longer there -- keeps a disconnect from being
		// turned into a reported failure.
	} else if err != nil {
		// The wait's own bound expired (context.DeadlineExceeded) with the stream
		// still registered. The delete is not in doubt -- it succeeded -- so the
		// client is told that, and told the retry is safe, rather than being left
		// with a 200 it must not yet act on.
		s.logf("session delete %s/%s: deleted, but the session's stream did not release: %v", user, sessionKey, err)
		writeJSON(w, http.StatusGatewayTimeout, map[string]any{
			"error": "the conversation was deleted, but the session's turn did not release in time; retry (the delete is idempotent)",
		})
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// writeSessionDeleteGatewayError maps a failed sessions.delete onto the Portal
// status, following the reason-extraction pattern of the question endpoint.
//
// A missing channel is not a gateway failure: the delete was never sent, and
// nothing about the round trip is in question.
//
// A call that ran out of time is not a failed round trip either: it is a timeout,
// and api-conventions.md §5 maps it to 504 -- the same two errors /abort's wait
// answers with StatusGatewayTimeout. After the handler's detach the request's
// cancellation no longer reaches the call, so a deadline can only be this
// handler's own bound expiring; 504 is left to that bound, while 502 stays for a
// gateway round trip that failed on its own. It is still this process's failure
// to complete the call, so it is logged exactly as the 502 is.
func (s *Server) writeSessionDeleteGatewayError(w http.ResponseWriter, user, sessionKey string, err error) {
	if errors.Is(err, errNoGatewayChannel) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "session delete channel unavailable"})
		return
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		// The log keeps the real error; the body must not. A deadline surfaces as
		// "context deadline exceeded", which is Go-internal text the Portal cannot
		// render, so the client is told what happened and that a retry is safe
		// instead -- the same shape as the 504 /abort's settle wait writes, with
		// the idempotence this endpoint has added: a retry that finds the session
		// already gone answers deleted=false, which is a success.
		s.logf("session delete %s/%s: %v", user, sessionKey, err)
		writeJSON(w, http.StatusGatewayTimeout, map[string]any{
			"error": "the session delete did not finish in time; retrying it is safe and idempotent",
		})
		return
	}
	status := sessionDeleteErrorStatus(ws.CodeOf(err), ws.ReasonOf(err))
	if status == http.StatusBadGateway {
		s.logf("session delete %s/%s: %v", user, sessionKey, err)
	}
	writeJSON(w, status, map[string]any{"error": err.Error()})
}

// sessionDeleteErrorStatus maps a gateway sessions.delete failure onto the
// Portal status. Code and reason are two wire fields, so both are consulted:
// the retryable "still busy" answer is an UNAVAILABLE that carries no reason at
// all, while the "changed under us" answer is an INVALID_REQUEST whose reason
// is the only thing that distinguishes it from the others.
//
// Both retryable outcomes are 409: neither is the caller's fault and neither
// needs the client to abort first -- the gateway stops active work itself -- so
// the client retries once the turn is over.
//
// The remaining INVALID_REQUESTs are the opposite: the gateway refusing this
// request itself, where no retry can help. 400 is what api-conventions.md §5
// keeps for a request problem, and it is the answer that tells the client the
// truth -- a 502 here would report the backend link as failed and invite a retry
// that can never succeed.
func sessionDeleteErrorStatus(code, reason string) int {
	switch {
	case code == "UNAVAILABLE":
		return http.StatusConflict
	case code == "INVALID_REQUEST" && reason == sessionLifecycleChangedReason:
		return http.StatusConflict
	case code == "INVALID_REQUEST":
		// The request itself, refused: a protected session, or one whose model
		// selection is locked. The first is reachable from the bare-key route --
		// DELETE /api/v1/sessions/main canonicalises to agent:main:main, which
		// OpenClaw protects -- and both are permanent for as long as the client
		// keeps asking for the same session. The gateway's message says which
		// refusal it was, so the body carries the reason and no field or second
		// lookup is needed.
		return http.StatusBadRequest
	case code == "FORBIDDEN":
		// The platform's own paired device was not granted operator.admin. That
		// is a deployment problem, not something the caller did or can fix, so it
		// is reported as this process's failure to reach the gateway rather than
		// as a conflict the client should retry.
		return http.StatusBadGateway
	default:
		return http.StatusBadGateway
	}
}
