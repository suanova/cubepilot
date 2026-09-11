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
// only thing that keeps a wedged connection from holding the handler open.
const abortSettleRPCTimeout = 3 * time.Second

// abortRunIDReadTimeout bounds the chat.history read that scopes an abort on the
// reload-takeover path. Short on purpose: it is a lookup in front of the command,
// and a lookup that does not answer only costs the session-scoped fallback.
const abortRunIDReadTimeout = 2 * time.Second

// abortReconcileTimeout bounds the post-failure read that decides whether a
// failed chat.abort nevertheless landed. Bounded for the same reason as every
// other gateway call here, and short because the handler still owes the caller a
// settle-and-wait afterwards.
const abortReconcileTimeout = 2 * time.Second

// handleAbort serves POST /api/sessions/{key}/abort -- the Portal's Stop.
//
// Order matters and is load-bearing:
//  1. abort the run, scoped to a run id read from the live turn, or from the
//     gateway when there is no live turn (reload takeover);
//  2. settle the session's pending HITL records IMMEDIATELY, while the SSE
//     stream is still open, or the resolved events have nowhere to go and the
//     UI keeps showing a card for a dead run;
//  3. wait for the session to be genuinely idle before returning, so the
//     client's follow-up send cannot race hub.Open into a 409.
//
// Both gateway steps (1 and 2) run on contexts detached from the request. A
// Stop is a command, not a read: it must not become a no-op because the client
// that issued it went away, and a client that disconnects between (1) and (2)
// would otherwise leave behind the very pending record (2) exists to clear,
// which resurfaces as a stale card on reload. Each detached step keeps its own
// bound, and the response is simply undeliverable once the client is gone.
//
// Step 1's failure is reconciled rather than trusted, and steps 2 and 3 run on
// its outcome: see the comment on the abort call below.
//
// Invariant: never settle a session whose run is still in flight. Step 2 is
// destructive -- it closes the records and drops the cards -- so running it
// against a live run deletes the only controls that run's parked confirmation or
// question had, for a turn the user asked to stop and which did not stop. Every
// path through step 1 therefore has to establish that the run is gone before
// step 2 runs: aborted=true is that proof, and both other outcomes (an RPC
// error, and a successful RPC that aborted nothing) reconcile against the
// gateway and settle only on a positive "not busy".
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
	} else {
		// No local turn to read one from: this is the reload-takeover path,
		// where the gateway is the only source of the id.
		runID = s.abortTargetRunID(r.Context(), user, sessionKey)
	}

	rpcCtx, cancelRPC := context.WithTimeout(context.WithoutCancel(r.Context()), abortRPCDeadline)
	defer cancelRPC()
	aborted, abortErr := s.hitl.Abort(rpcCtx, user, sessionKey, runID)
	switch {
	case abortErr != nil:
		// A failed RPC does not prove the stop did not happen. The frame is
		// written before the response is waited for, so an abort that errors on
		// its deadline can still have been delivered and honoured; treating that
		// as "nothing happened" would leave the session's cards pending for a run
		// that is already dead, and they would resurface on reload as cards
		// nothing can answer. Reconcile with a bounded read: only a gateway that
		// no longer has the run in flight proves the stop landed, in which case
		// the session is settled normally below.
		//
		// Anything else stays conservative. A run still in flight is a refusal --
		// the user asked to stop and it did not stop -- and an unanswered
		// reconciliation is unknown, and settling on an unknown result would
		// delete a live, answerable card for a run that never stopped, which is
		// worse than a stale one. Both keep the records pending and answer 502.
		if !s.abortLanded(r.Context(), user, sessionKey) {
			s.logf("abort %s/%s: %v", user, sessionKey, abortErr)
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": abortErr.Error()})
			return
		}
		s.logf("abort %s/%s: %v (the run is no longer in flight: the stop landed)", user, sessionKey, abortErr)
	case !aborted:
		// The RPC succeeded and stopped nothing: the gateway answered
		// {aborted:false, runIds:[]} because the run id matched no abortable run,
		// which a run that is session-abortable-only, or one promoted between the
		// in-flight read above and the RPC, both produce. "Nothing was aborted"
		// is not "the run is gone", so the same invariant applies as on the error
		// branch: reconcile, and settle only once the gateway says the session has
		// no run in flight. A run still in flight is a stop that did not happen --
		// answer it as a failure and leave the records pending, or the next card
		// the user needs is deleted for a run that is still going, and the 200
		// tells the Portal to send its follow-up into a busy session.
		//
		// The cost of reconciling before settling is one extra round trip, and the
		// stream may close inside it: the `*_resolved` publishes below then land on
		// a stream that is gone, so a card can linger on screen until a reload. A
		// stale card is strictly better than a card deleted for a live run.
		if !s.abortLanded(r.Context(), user, sessionKey) {
			s.logf("abort %s/%s: chat.abort stopped nothing and the session is not provably idle: not settling", user, sessionKey)
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"error": "the gateway did not stop a run for this session; it is still in flight",
			})
			return
		}
		s.logf("abort %s/%s: chat.abort stopped nothing, but no run is in flight: the session is idle", user, sessionKey)
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

// abortTargetRunID reads the run the gateway has in flight for the session, so
// an abort issued after a reload is scoped to that run rather than to whatever
// the session happens to be running when the RPC is processed.
//
// The local live turn is the cheap source and the first one consulted by the
// caller; it is gone on this path, because releaseLive removes it when the
// request driving the turn ends and the run itself can outlive that (a browser
// disconnect stops observation, not the run). A session-scoped abort is not
// race-free -- a run promoted between this read and the RPC is the one it would
// kill -- so the id is worth this lookup.
//
// Best-effort by design: an unanswered read leaves the caller with the
// session-scoped abort, which is the last resort and still better than refusing
// to stop a run the user asked to stop.
func (s *Server) abortTargetRunID(ctx context.Context, user, sessionKey string) string {
	ctx, cancel := context.WithTimeout(ctx, abortRunIDReadTimeout)
	defer cancel()
	id, err := s.hitl.InFlightRunID(ctx, user, sessionKey)
	if err != nil {
		s.logf("abort %s/%s: in-flight run lookup: %v", user, sessionKey, err)
		return ""
	}
	return id
}

// abortLanded reports whether a reconciliation read proves the gateway no longer
// has the session's run in flight, i.e. that an abort RPC which failed was
// nevertheless delivered and honoured.
//
// It fails closed in both directions a read can fail: an RPC error leaves the
// result unknown (the run may or may not have stopped), so it reports false and
// the caller keeps the records pending. Only a positive "not busy" answer counts.
func (s *Server) abortLanded(ctx context.Context, user, sessionKey string) bool {
	ctx, cancel := context.WithTimeout(ctx, abortReconcileTimeout)
	defer cancel()
	busy, err := s.hitl.SessionBusy(ctx, user, sessionKey)
	if err != nil {
		s.logf("abort %s/%s: reconcile: %v", user, sessionKey, err)
		return false
	}
	return !busy
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
	// the same reason: the HTTP server sets no timeouts at all, so a half-open
	// gateway connection would park this handler forever -- one leaked blocked
	// request per Portal refresh. It stays on the request context, unlike
	// /abort's detached commands: a read is only meaningful to the caller still
	// holding the request. Expiry surfaces as the error below (502), never as
	// (false, nil), because "cannot determine" must not be rendered as
	// "not busy".
	ctx, cancel := context.WithTimeout(r.Context(), abortRPCDeadline)
	defer cancel()
	busy, err := s.hitl.SessionBusyEstablished(ctx, user, sessionKey)
	if err != nil {
		// The read establishes the channel before it asks, so it is not answered
		// from local state at all. That matters after a restart or a rollout:
		// the replacement process has no entry for the user while a run the
		// *previous* process started can still be executing gateway-side, and
		// reading the missing entry as idle would hide that turn and remove its
		// Stop. A user who has simply never chatted dials straight through (the
		// dial is bounded by channelProbeTimeout) and gets a true answer, so the
		// only outcome left here is a channel that genuinely could not be
		// established -- "could not check" with a Retry, which is now accurate
		// rather than the old false alarm on every fresh conversation.
		s.logf("turn status %s/%s (a channel was once established: %v): %v", user, sessionKey, s.hitl.gatewayConnected(user), err)
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
