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
const abortSettleTimeout = 5 * time.Second

// abortRPCDeadline bounds a single chat.abort gateway RPC so a wedged WebSocket
// cannot hold the HTTP request open until the client gives up.
const abortRPCDeadline = 5 * time.Second

// handleAbort serves POST /api/sessions/{key}/abort -- the Portal's Stop.
//
// Order matters and is load-bearing:
//  1. abort the run, scoped to the live turn's run id when there is one;
//  2. settle the session's pending HITL records IMMEDIATELY, while the SSE
//     stream is still open, or the resolved events have nowhere to go and the
//     UI keeps showing a card for a dead run;
//  3. wait for the session to be genuinely idle before returning, so the
//     client's follow-up send cannot race hub.Open into a 409.
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

	rpcCtx, cancelRPC := context.WithTimeout(r.Context(), abortRPCDeadline)
	defer cancelRPC()
	if err := s.hitl.Abort(rpcCtx, user, sessionKey, runID); err != nil {
		s.logf("abort %s/%s: %v", user, sessionKey, err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}

	// Before the wait: the stream is still attached and can deliver the
	// resolved events that drop the cards.
	s.settlePendingForSession(r.Context(), user, sessionKey)

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

// waitSessionIdle waits for BOTH signals, because they cover different paths:
// the hub check is what stops the follow-up send from racing hub.Open into a
// 409, and the gateway check is the only one that means anything on the
// reload-takeover path, where no stream exists at all and a follow-up send
// could otherwise be steered into the dying run and swallowed.
func (s *Server) waitSessionIdle(ctx context.Context, user, sessionKey string) error {
	hubErr := make(chan error, 1)
	go func() { hubErr <- s.hub.WaitIdle(ctx, sessionKey) }()

	// hubIdle latches: hubErr is delivered once, and re-selecting on a drained
	// channel would block until ctx expires even though the hub is already idle.
	hubIdle := false
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if !hubIdle {
			select {
			case err := <-hubErr:
				if err != nil {
					return err
				}
				hubIdle = true
			default:
			}
		}
		if hubIdle {
			busy, err := s.hitl.SessionBusy(ctx, user, sessionKey)
			if err != nil {
				return err
			}
			if !busy {
				return nil
			}
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
