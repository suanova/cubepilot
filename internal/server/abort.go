package server

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// abortSettleTimeout bounds how long /abort waits for the session to go idle.
// Bounded on purpose: a stop that cannot settle must report that, not hang the
// UI forever.
//
// A package-level var, in the style of hitlPairRetryDelay, so a test can shorten
// it. As a const the 504 branch was reachable only through a request context
// that was already cancelled, which left a regression to an unbounded wait
// undetectable.
var abortSettleTimeout = 5 * time.Second

// abortRPCDeadline bounds a single chat.abort gateway RPC so a wedged WebSocket
// cannot hold the HTTP request open until the client gives up.
const abortRPCDeadline = 5 * time.Second

// abortSettleRPCTimeout bounds the settle step's gateway calls. The settle runs
// on a context detached from the request (see handleAbort), so this bound is the
// only thing that keeps a wedged connection from holding the handler open:
// ws.Client.Call blocks on the connection's write mutex before it ever selects
// on the context, so no per-call cancellation can rescue it.
const abortSettleRPCTimeout = 3 * time.Second

// handleAbort serves POST /api/sessions/{key}/abort -- the Portal's Stop.
//
// Order matters and is load-bearing:
//  1. abort the run, scoped to the live turn's run id when there is one;
//  2. settle the session's pending HITL records IMMEDIATELY, while the SSE
//     stream is still open, or the resolved events have nowhere to go and the
//     UI keeps showing a card for a dead run;
//  3. wait for the session to be genuinely idle before returning, so the
//     client's follow-up send cannot race hub.Open into a 409.
//
// Both gateway steps (1 and 2) run on contexts detached from the request. A
// Stop is a command, not a read: it must not become a no-op because the client
// that issued it went away. Detaching also removes an outright lie -- ws.Client.Call
// writes the request frame BEFORE it selects on the context, so cancelling the
// request mid-RPC leaves the abort delivered while the handler reports 502, and
// a client that disconnects between (1) and (2) would leave behind the very
// pending record (2) exists to clear, which resurfaces as a stale card on
// reload. Each detached step keeps its own bound, and the response is simply
// undeliverable once the client is gone.
func (s *Server) handleAbort(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST required"})
		return
	}
	user := s.userOf(r)
	sessionKey := canonicalSessionKey(subresourceKey(r.URL.Path, "/abort"))
	if sessionKey == "" || sessionKey == "agent:main:" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing session key"})
		return
	}
	if s.hitl == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "abort channel unavailable"})
		return
	}

	runID := ""
	if id, ok := s.hitl.LiveRunID(user, sessionKey); ok {
		runID = id
	}

	rpcCtx, cancelRPC := context.WithTimeout(context.WithoutCancel(r.Context()), abortRPCDeadline)
	defer cancelRPC()
	if err := s.hitl.Abort(rpcCtx, user, sessionKey, runID); err != nil {
		s.logf("abort %s/%s: %v", user, sessionKey, err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}

	// Before the wait: the stream is still attached and can deliver the
	// resolved events that drop the cards. Detached and short-bounded for the
	// same reason as the abort RPC above: it is post-abort cleanup, so a
	// disconnect must not skip it, and a wedged gateway must not hold the
	// request past abortSettleTimeout's whole budget.
	settleCtx, cancelSettle := context.WithTimeout(context.WithoutCancel(r.Context()), abortSettleRPCTimeout)
	s.settlePendingForSession(settleCtx, user, sessionKey)
	cancelSettle()

	// The wait stays on the request context: its answer is only meaningful to
	// the caller still holding the request open, and a client that gave up must
	// not pin the handler for the rest of the budget.
	ctx, cancel := context.WithTimeout(r.Context(), abortSettleTimeout)
	defer cancel()

	if err := s.waitSessionIdle(ctx, user, sessionKey); err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			writeJSON(w, http.StatusGatewayTimeout, map[string]any{
				"error": "the run did not settle in time; try again",
			})
			return
		}
		s.logf("abort %s/%s: wait idle: %v", user, sessionKey, err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleTurnStatus serves GET /api/sessions/{key}/turn -- whether the session
// still has a run in flight, so a Portal that reloaded (and therefore has no
// stream) can say so and offer Stop.
//
// The answer comes from the gateway, not the SSE hub: the hub only knows
// whether a browser is attached, which is false after a reload while the run is
// still going.
func (s *Server) handleTurnStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET required"})
		return
	}
	// Per-session liveness: never cache.
	w.Header().Set("Cache-Control", "no-store")

	user := s.userOf(r)
	sessionKey := canonicalSessionKey(subresourceKey(r.URL.Path, "/turn"))
	if sessionKey == "" || sessionKey == "agent:main:" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing session key"})
		return
	}
	if s.hitl == nil {
		writeJSON(w, http.StatusOK, map[string]any{"active": false})
		return
	}
	// Bound the read with the same abortRPCDeadline /abort's RPC uses, and for
	// the same reason: ws.Client.Call takes the connection's write mutex and
	// writes the frame before it ever selects on the context, and the HTTP
	// server sets no timeouts at all, so a half-open gateway connection would
	// park this handler forever -- one leaked blocked request per Portal
	// refresh. It stays on the request context, unlike /abort's detached
	// commands: a read is only meaningful to the caller still holding the
	// request. Expiry surfaces as the error below (502), never as (false, nil),
	// because "cannot determine" must not be rendered as "not busy".
	ctx, cancel := context.WithTimeout(r.Context(), abortRPCDeadline)
	defer cancel()
	busy, err := s.hitl.SessionBusy(ctx, user, sessionKey)
	if err != nil {
		// errNoGatewayChannel covers two states liveConn answers alike, and only
		// one of them may be read as idle. The connection is dialled lazily on
		// first use, so a process with *no channel* for the user -- never
		// dialled, or still handshaking, which the pairing retry can drag out
		// for up to 30s -- cannot be driving a turn for them: Stop could not
		// succeed either (it fails on the same missing channel), and claiming
		// "cannot determine" buys nothing while costing every conversation open
		// a false alarm plus a Stop button that provably cannot work.
		//
		// A channel that came up and has since gone down is the opposite case:
		// the break is in *observation*, not in the run -- the agent keeps
		// working through a gateway restart or a pod roll -- so a turn this
		// process started can still be in flight and the honest answer is that
		// it could not be determined. Answering idle there would hide a running
		// turn and offer no Stop, which is exactly what the check above this
		// handler exists to prevent; the caller gets an error it reports as
		// "could not check" with a Retry.
		if errors.Is(err, errNoGatewayChannel) && !s.hitl.gatewayConnected(user) {
			writeJSON(w, http.StatusOK, map[string]any{"active": false})
			return
		}
		s.logf("turn status %s/%s: %v", user, sessionKey, err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"active": busy})
}

// waitSessionIdle waits for BOTH signals, because they cover different paths:
// the hub check is what stops the follow-up send from racing hub.Open into a
// 409, and the gateway check is the only one that means anything on the
// reload-takeover path, where no stream exists at all and a follow-up send
// could otherwise be steered into the dying run and swallowed.
//
// Both signals are re-read rather than latched: "idle" is an observation about
// an instant, and the session can become busy again right after it is seen (a
// second tab starting a turn on the same session). The 200 is therefore only
// written from a state that is still idle at the moment it is read.
func (s *Server) waitSessionIdle(ctx context.Context, user, sessionKey string) error {
	hubErr := make(chan error, 1)
	// armHubWait keeps exactly one WaitIdle in flight. WaitIdle is one-shot, so
	// every report (idle again after a new stream opened, or a re-check that
	// found the gateway still busy) has to re-arm it; a second concurrent
	// waiter would only add a goroutine nothing reads.
	armHubWait := func() { go func() { hubErr <- s.hub.WaitIdle(ctx, sessionKey) }() }
	armHubWait()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-hubErr:
			if err != nil {
				return err
			}
			// The hub reported idle. It must still be idle at the moment both
			// signals agree, not merely at the moment WaitIdle returned.
			busy, err := s.hitl.SessionBusy(ctx, user, sessionKey)
			if err != nil {
				return err
			}
			if !busy && !s.hub.Active(sessionKey) {
				return nil
			}
			armHubWait()
		default:
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
