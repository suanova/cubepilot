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
	if p, ok := s.approvals.Pending(user, sessionKey); ok {
		s.approvals.settle(p)
		s.hub.PublishTo(sessionKey, agentruntime.Event{
			Type:      agentruntime.EventConfirmResolved,
			SessionID: sessionKey,
			CallID:    p.ApprovalID,
		})
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
