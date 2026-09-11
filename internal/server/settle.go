package server

import (
	"context"

	agentruntime "github.com/suanova/cubepilot/internal/runtime"
)

// settlePendingForSession closes out every human-in-the-loop record a stopped
// turn left behind. Aborting the run settles these gateway-side, but the
// platform keeps its own bookkeeping -- the pending confirmation and the
// question routing entries -- and that bookkeeping is exactly what powers
// reload recovery. Left alone, it resurfaces a card for a run that no longer
// exists and errors when the user clicks it.
//
// It must run while the turn's SSE stream is still open: the *_resolved events
// below are how an attached view drops the card immediately, and a later reload
// stops resurrecting it. Deleting the records silently, or waiting for the
// question timeout, are strictly worse.
func (s *Server) settlePendingForSession(ctx context.Context, user, sessionKey string) {
	// A nil approval service is a real state, not an impossible one: handleConfirm
	// and handlePendingConfirm both check for it, and a bare Server literal (the
	// abort handler's own fixture) leaves it unset. The question half below is
	// guarded the same way.
	if s.approvals != nil {
		// One atomic claim, not a Pending-then-settle pair: a Resolve that
		// reserves the approval between the two would leave nothing for the
		// settle to find, and the failed resolve would then restore a card for a
		// session whose turn was stopped (see ApprovalService.settleSession).
		if p, ok := s.approvals.settleSession(user, sessionKey); ok {
			// The record's own key addresses the stream, exactly as
			// ApprovalService.Resolve does -- the claim only ever returns a
			// record this session owned, but a publish that follows the record
			// rather than the caller cannot drift from it.
			s.hub.PublishTo(p.SessionKey, agentruntime.Event{
				Type:      agentruntime.EventConfirmResolved,
				SessionID: p.SessionKey,
				CallID:    p.ApprovalID,
			})
		}
	}

	if s.hitl == nil {
		return
	}
	list, err := s.hitl.ListQuestions(ctx, user)
	if err != nil {
		s.logf("settle %s/%s: list questions: %v", user, sessionKey, err)
		return
	}
	for _, rec := range list {
		// Only the session binding and the gateway's own pending status gate the
		// cancel; the render filters the question endpoints apply
		// (unsupportedQuestionReason, questionExpired) deliberately do not. Those
		// filters decide what the Portal may paint or restore, and a settle paints
		// nothing -- it closes the records of a run that is now dead. A card the
		// browser already holds can still be on screen (a countdown that ran out
		// leaves it locked until a question_resolved arrives), and a record this
		// Portal never rendered -- a secret or free-text question the other paths
		// drop -- is still an open question of the dead run, not one left to
		// answer. Closing one that has no card is harmless: the client ignores a
		// resolved event it cannot match.
		if canonicalSessionKey(rec.SessionKey) != sessionKey || rec.Status != "pending" {
			continue
		}
		if err := s.hitl.CancelQuestion(ctx, user, rec.ID); err != nil {
			s.logf("settle %s/%s: cancel question %s: %v", user, sessionKey, rec.ID, err)
			continue
		}
		s.qroutes.take(rec.ID)
		s.hub.PublishTo(sessionKey, agentruntime.Event{
			Type:      agentruntime.EventQuestionResolved,
			SessionID: sessionKey,
			CallID:    rec.ID,
			Message:   "cancelled",
		})
	}
}
