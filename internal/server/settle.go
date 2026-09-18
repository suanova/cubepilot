package server

import (
	"context"

	"github.com/suanova/cubepilot/internal/openclaw/ws"
	agentruntime "github.com/suanova/cubepilot/internal/runtime"
)

// settleApprovalResolved handles the gateway's exec.approval.resolved broadcast:
// an approval ended without the Portal deciding it -- it expired unanswered, or
// its run was aborted or lost gateway-side. The record is dropped and, when a
// view is attached, the card with it.
//
// It is the counterpart of settlePendingForSession for the case the platform
// never learns about any other way. That one runs because *we* stopped the turn
// and so know the run is gone; here the run ended somewhere this process cannot
// see, and the broadcast is the only account of it. Without this the ledger that
// reload recovery reads keeps the record indefinitely, so reopening the
// conversation paints a confirmation card for an approval the gateway has
// forgotten, and answering it fails against the gateway.
//
// Idempotent by construction, because the same broadcast also follows the
// Portal's own decision and the abort path's settle: the claim simply finds
// nothing, and no event is published for a record nobody held.
func (s *Server) settleApprovalResolved(user string, ev ws.ApprovalResolved) {
	if s.approvals == nil {
		return
	}
	p, ok := s.approvals.settleApproval(user, ev.ID)
	if !ok {
		return
	}
	// Approved is reported only for a decision that was actually taken, which is
	// what ResolvedBy records -- not what Decision says. Decision cannot carry
	// that on its own: the gateway's publication path fills an absent decision
	// with "deny" (`decision ?? "deny"`), so an approval that expired unanswered,
	// or one cancelled because its run's authority closed, arrives here looking
	// exactly like a denial. Reporting it as one would paint the user a red
	// "Rejected" for a decision they never made, which is the mislabelling the
	// client's absent-Approved branch exists to prevent: with no Approved it
	// renders the neutral "Stopped", which is what actually happened.
	var approved *bool
	if ev.ResolvedBy != "" {
		switch ev.Decision {
		case "allow-once", "allow-always":
			v := true
			approved = &v
		case "deny":
			v := false
			approved = &v
		}
	}
	s.hub.PublishTo(p.SessionKey, agentruntime.Event{
		Type:      agentruntime.EventApprovalResolved,
		SessionID: p.SessionKey,
		CallID:    p.ApprovalID,
		Approved:  approved,
	})
}

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
	// A nil approval service is a real state, not an impossible one: handleApproval
	// and handlePendingApproval both check for it, and a bare Server literal (the
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
				Type:      agentruntime.EventApprovalResolved,
				SessionID: p.SessionKey,
				CallID:    p.ApprovalID,
			})
		}
	}

	if s.gatewayConns == nil {
		return
	}
	list, err := s.gatewayConns.ListQuestions(ctx, user)
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
		// Portal never rendered -- a secret question the other paths drop -- is
		// still an open question of the dead run, not one left to answer. Closing
		// one that has no card is harmless: the client ignores a resolved event it
		// cannot match.
		if canonicalSessionKey(rec.SessionKey) != sessionKey || rec.Status != "pending" {
			continue
		}
		if err := s.gatewayConns.CancelQuestion(ctx, user, rec.ID); err != nil {
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
