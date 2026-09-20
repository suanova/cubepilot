package server

import (
	"context"

	"github.com/suanova/cubepilot/internal/openclaw/ws"
	agentruntime "github.com/suanova/cubepilot/internal/runtime"
)

// relayApprovalResolved forwards an approval the gateway ended on its own -- a
// decision taken anywhere (the Portal's own included), an expiry, or a run
// aborted or lost gateway-side -- onto its session's stream.
//
// The broadcast carries the approval's request, so the session it belongs to
// arrives with it. That is what makes this a relay rather than bookkeeping:
// there is no record to look up, no owner to compare one against, and an
// approval this process never saw through (a restart, or a connection that was
// down when it was raised) still reaches the card that is waiting for it.
//
// Approved is reported only for a decision that was actually taken, which is
// what ResolvedBy records -- not what Decision says. Decision cannot carry that
// on its own: the gateway's publication path fills an absent decision with
// "deny" (`decision ?? "deny"`), so an approval that expired unanswered, or one
// cancelled because its run's authority closed, arrives here looking exactly
// like a denial. Reporting it as one would paint the user a red "Rejected" for a
// decision they never made, which is the mislabelling the client's
// absent-Approved branch exists to prevent: with no Approved it renders the
// neutral "Stopped", which is what actually happened.
func (s *Server) relayApprovalResolved(user string, ev ws.ApprovalResolved) {
	if ev.ID == "" {
		return
	}
	sessionKey := canonicalSessionKey(ev.Request.SessionKey)
	if sessionKey == "" || sessionKey == "agent:main:" {
		// The event cannot be addressed. The card, if one is on screen, stays
		// until the next pending read -- which asks the gateway, and so cannot
		// disagree with it.
		s.logf("approval %s: resolved (%s) for %s with no session key; not forwarded", ev.ID, ev.Decision, user)
		return
	}
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
	if !s.hub.PublishTo(sessionKey, agentruntime.Event{
		Type:      agentruntime.EventApprovalResolved,
		SessionID: sessionKey,
		CallID:    ev.ID,
		Approved:  approved,
	}) {
		s.logf("approval %s: no open stream for session %s; resolution not delivered", ev.ID, sessionKey)
	}
}

// settlePendingForSession closes out every human-in-the-loop record a stopped
// turn left behind. Aborting the run settles these gateway-side, but the browser
// is holding cards for them, and those cards are the only surface that could
// answer a record the gateway no longer has. Left alone, they stay on screen
// offering controls that cannot work.
//
// It must run while the turn's SSE stream is still open: the *_resolved events
// below are how an attached view drops the card immediately, and a later reload
// stops resurrecting it. Waiting for the question timeout, or relying on the
// gateway's own broadcast to arrive in time, are strictly worse.
//
// captured is the session's pending approvals as they were *before* the abort:
// the abort cancels the approvals bound to the run, so by the time the run is
// provably gone the gateway has none left to list. See handleAbort, which takes
// that read before it issues the stop.
func (s *Server) settlePendingForSession(ctx context.Context, user, sessionKey string, captured []pendingApproval) {
	// A nil approval service is a real state, not an impossible one: handleApproval
	// and handlePendingApproval both check for it, and a bare Server literal (the
	// abort handler's own fixture) leaves it unset. The question half below is
	// guarded the same way.
	if s.approvals != nil {
		for _, p := range s.approvals.SettleApprovals(captured) {
			// The record's own key addresses the stream, exactly as
			// ApprovalService.Resolve does -- a publish that follows the record
			// rather than the caller cannot drift from it. approved is nil: the
			// turn was stopped, so nobody decided this, and the client renders
			// that neutrally instead of as a rejection.
			s.approvals.publishResolved(p, nil)
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
