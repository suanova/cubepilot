package server

import (
	"context"
	"errors"
	"net/http"

	agentruntime "github.com/suanova/cubepilot/internal/runtime"
)

// Re-attach to a parked turn (issue #167). A turn's events are written only to
// the SSE stream that the POST /api/v1/messages request opened, so when that
// request goes away -- a page reload, a closed or discarded tab, a dropped
// connection -- nothing observes the run any more. The run itself keeps going:
// it is parked on a human decision, and the next page load restores its card
// from the gateway's own state. This route is what makes that restored card
// answerable in place: it observes the parked run and carries its continuation
// to the browser that answers.

// handleSessionStream serves GET /api/v1/sessions/{key}/stream, an SSE stream
// that observes a turn the caller did not start. It ends with a message_done
// when the run goes terminal, when the client disconnects, or at the manager's
// attach cap -- and, alone among the ways it can end, without one when the
// decision it attached to was answered before its subscription was ready: there
// is then no run for this stream to report on (see the revalidation below).
func (s *Server) handleSessionStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET required"})
		return
	}
	user := s.userOf(r)
	sessionKey := canonicalSessionKey(subresourceKey(r.URL.Path, "/stream"))
	if sessionKey == "" || sessionKey == "agent:main:" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing session key"})
		return
	}
	if s.gatewayConns == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "question channel unavailable"})
		return
	}
	// Only a parked decision is attachable. It is also the state in which the
	// attach is set up without racing the run's output: the run is waiting for a
	// human, so it is producing nothing while the subscription is established.
	runID, parked, err := s.parkedTurn(r.Context(), user, sessionKey)
	if err != nil {
		s.logf("attach %s: %s: %v", user, sessionKey, err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	if !parked {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no parked turn for this session"})
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "streaming unsupported"})
		return
	}
	// One stream per session: the tab that already holds one receives every event
	// (including the question and confirmation pushes), so an attaching tab stands
	// down rather than competing for the same feed.
	stream, oerr := s.hub.Open(sessionKey, w, flusher)
	if oerr != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "another stream is already open for this session"})
		return
	}
	defer stream.Close()
	stream.Start()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	emit := func(ev agentruntime.Event) error {
		// The stream that started this turn is gone, so its audit recording went
		// with it: record here too, or the resumed part of the turn (the tool calls
		// the answer triggered) never reaches the ledger.
		s.recordToolCall(user, ev)
		return stream.Send(ev)
	}
	outcome, attachErr := s.gatewayConns.AttachLiveTurn(r.Context(), user, sessionKey, runID, emit, func() error {
		// The parked check above says the run was waiting for a human; it cannot
		// promise the run is *still* waiting by the time this subscription is up.
		// The answer route is independent of both, so it can resolve the decision
		// in that window -- the web path opens the card before it opens the
		// stream, so a quick answer is enough. Everything the resumed run
		// produced since went to nobody, and no subscription can recover it:
		// what this stream must not do is keep waiting for a terminal frame that
		// has already been broadcast (issue #167).
		_, stillParked, err := s.parkedTurn(r.Context(), user, sessionKey)
		if err != nil {
			return err
		}
		if !stillParked {
			return errDecisionResolved
		}
		return nil
	})
	// A cancelled context means the browser went away; there is nobody to tell and
	// nothing to log beyond the attach path's own lines.
	if r.Context().Err() != nil {
		return
	}
	if errors.Is(attachErr, errDecisionResolved) {
		// Ending without a terminal is the honest report, and the shape the
		// browser already handles: its stream helper synthesizes a terminal for a
		// stream that ends without one, and the attach path reads a synthesized
		// terminal as exactly this -- a stream that observed no run, so nothing
		// for it to settle on the card (see attachTurn). A liveTurnDone here
		// would instead mark a turn this stream never saw as over, and paint the
		// race on the user as an error.
		s.logf("attach %s: %s: %v", user, sessionKey, errDecisionResolved)
		return
	}
	if attachErr != nil {
		s.logf("attach %s: %s: %v", user, sessionKey, attachErr)
	}
	// The single terminal event, chosen by the same rule a started turn uses:
	// a run another tab stopped is terminal but not a failure, so it must not
	// reach the browser as a plain completion (issue #166).
	_ = stream.Send(liveTurnDone(sessionKey, outcome, attachErr))
}

// errDecisionResolved reports that the decision an attach was gated on was
// answered before the attach got its subscription in place, so the resumed run's
// output has already gone out to nobody (issue #167). It is not a failure of the
// attach: it is the one outcome in which the caller is told, by the stream simply
// ending, that there was nothing here to observe.
var errDecisionResolved = errors.New("the parked decision was resolved before the stream was ready")

// parkedTurn reports the run a session is parked on -- the gate of the attach
// route, applied twice: once to decide whether there is anything to attach to,
// and once more once the subscription is up, to catch a decision answered in
// between. A question carries the run it belongs to, which the attach uses to
// scope what it observes; a write approval parks the run the same way but names
// no run of its own, so the session is the whole of that gate.
func (s *Server) parkedTurn(ctx context.Context, user, sessionKey string) (string, bool, error) {
	runID, parked, err := s.parkedQuestionRun(ctx, user, sessionKey)
	if err != nil || parked {
		return runID, parked, err
	}
	_, parked = s.approvals.Pending(user, sessionKey)
	return "", parked, nil
}

// parkedQuestionRun reports the run a session is parked on because of a question,
// with the same projectability, status and expiry gates the pending-questions
// route applies: only a question the Portal can render has a card to answer.
// A session with no live question channel simply has no parked question -- the
// same "nothing pending" answer handlePendingQuestion gives.
func (s *Server) parkedQuestionRun(ctx context.Context, user, sessionKey string) (string, bool, error) {
	list, err := s.gatewayConns.ListQuestions(ctx, user)
	if errors.Is(err, errNoQuestionChannel) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	for _, rec := range list {
		if canonicalSessionKey(rec.SessionKey) != sessionKey || rec.Status != "pending" ||
			questionExpired(rec) || unsupportedQuestionReason(rec) != "" {
			continue
		}
		return rec.RunID, true, nil
	}
	return "", false, nil
}
