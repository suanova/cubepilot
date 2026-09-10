package server

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/suanova/cubepilot/internal/openclaw/ws"
	agentruntime "github.com/suanova/cubepilot/internal/runtime"
)

// A stopped turn must not leave a card behind: after settling, the session has
// no pending confirmation, the record is gone from the recovery lookup, and a
// confirm_resolved event was published to any attached stream.
//
// Pending() on its own cannot pin this: it reports false for an approval whose
// byID entry is gone even when bySession still points at it, and bySession is
// the map reload recovery reads. The maps are asserted directly, for the
// settled session and for a second one that must keep its approval.
func TestSettlePendingForSessionClearsConfirm(t *testing.T) {
	h := NewHub()
	svc := NewApprovalService(h, nil, func(string, ...any) {})
	svc.Begin("admin", pendingApproval{
		ApprovalID: "ap-1",
		SessionKey: "conv-1",
		User:       "admin",
		Tool:       "exec",
		Command:    "kubectl delete pod x",
		Level:      "write",
	})
	svc.Begin("admin", pendingApproval{
		ApprovalID: "ap-2",
		SessionKey: "conv-2",
		User:       "admin",
		Tool:       "exec",
		Command:    "kubectl delete pod y",
		Level:      "write",
	})

	s := &Server{hub: h, approvals: svc}
	s.settlePendingForSession(context.Background(), "admin", "conv-1")

	if _, ok := svc.Pending("admin", "conv-1"); ok {
		t.Fatal("pending confirmation survived the settle")
	}
	if id, ok := svc.bySession["conv-1"]; ok {
		t.Fatalf("bySession still maps conv-1 to %q: a reload resurrects the card", id)
	}
	if _, ok := svc.byID["ap-1"]; ok {
		t.Error("byID still holds the settled approval")
	}
	// The settle is scoped to one session: the other session's approval, in both
	// maps, is not this settle's to clear.
	if id, ok := svc.bySession["conv-2"]; !ok || id != "ap-2" {
		t.Fatalf("bySession[conv-2] = %q (present %v), want ap-2", id, ok)
	}
	if p, ok := svc.byID["ap-2"]; !ok || p.SessionKey != "conv-2" {
		t.Fatalf("byID[ap-2] = %+v (present %v), want the untouched approval", p, ok)
	}
	if _, ok := svc.Pending("admin", "conv-2"); !ok {
		t.Error("the other session lost its pending confirmation")
	}
}

// A server built without an approval service must not panic. That is the shape
// the abort handler's fixture uses (&Server{hub: h, hitl: m}), and both
// handleConfirm and handlePendingConfirm already treat a nil service as a real
// state; the settle has to as well.
func TestSettlePendingForSessionWithoutApprovalService(t *testing.T) {
	gw := &fakeHitlGateway{}
	base, _ := questionTestServer(t, gw, questionTestSession)

	// The bare fixture: the live HITL manager, no approval service and no
	// question routes.
	s := &Server{hub: base.hub, hitl: base.hitl}
	s.settlePendingForSession(context.Background(), "alice", questionTestSession)

	if len(gw.questionCancels) != 0 {
		t.Fatalf("question cancels = %v, want none (nothing was asked)", gw.questionCancels)
	}
}

// Settling publishes confirm_resolved while the turn's stream is still open, so
// an attached view drops the card at once instead of waiting for a reload that
// would resurrect it.
func TestSettlePendingForSessionPublishesConfirmResolved(t *testing.T) {
	h := NewHub()
	svc := NewApprovalService(h, nil, tLogf)
	rec := httptest.NewRecorder()
	if _, err := h.Open("conv-1", rec, rec); err != nil {
		t.Fatalf("open stream: %v", err)
	}
	svc.Begin("admin", pendingApproval{
		ApprovalID: "ap-1",
		SessionKey: "conv-1",
		Tool:       "exec",
		Command:    "kubectl delete pod x",
	})

	s := &Server{hub: h, approvals: svc}
	s.settlePendingForSession(context.Background(), "admin", "conv-1")

	evs := sseEvents(t, rec.Body.String())
	if len(evs) == 0 {
		t.Fatal("no SSE events published")
	}
	last := evs[len(evs)-1]
	if last["type"] != agentruntime.EventConfirmResolved {
		t.Fatalf("last event = %v, want %s", last["type"], agentruntime.EventConfirmResolved)
	}
	if last["call_id"] != "ap-1" || last["session_id"] != "conv-1" {
		t.Fatalf("resolved event = %v, want call_id ap-1 on conv-1", last)
	}
	// A settle is not a decision: no allow/deny is implied.
	if _, ok := last["approved"]; ok {
		t.Errorf("settled confirmation carried approved=%v", last["approved"])
	}
}

// The question half of the settle cancels the session's open questions on the
// gateway, drops their routing entries, and publishes question_resolved; a
// question belonging to another session is untouched.
func TestSettlePendingForSessionClearsQuestions(t *testing.T) {
	mine := questionRecord("ask_1", questionTestSession)
	other := questionRecord("ask_other", "agent:main:other-session")
	gw := &fakeHitlGateway{pendingQuestions: []ws.QuestionRecord{mine, other}}
	s, rec := questionTestServer(t, gw, questionTestSession)
	s.qroutes.put("ask_1", questionTestSession)

	s.settlePendingForSession(context.Background(), "alice", questionTestSession)

	if len(gw.questionCancels) != 1 || gw.questionCancels[0] != "ask_1|alice" {
		t.Fatalf("question cancels = %v, want [ask_1|alice]", gw.questionCancels)
	}
	if _, ok := s.qroutes.take("ask_1"); ok {
		t.Error("question route survived the settle")
	}
	evs := sseEvents(t, rec.Body.String())
	if len(evs) != 1 {
		t.Fatalf("events = %v, want exactly one question_resolved", evs)
	}
	if evs[0]["type"] != agentruntime.EventQuestionResolved ||
		evs[0]["call_id"] != "ask_1" || evs[0]["message"] != "cancelled" {
		t.Fatalf("event = %v, want question_resolved/cancelled for ask_1", evs[0])
	}
}

// The record key is canonicalised before it is compared. The gateway reports
// the raw key (question.list is the same input handleQuestion and
// handlePendingQuestion canonicalise), while the abort handler passes the
// canonical one, so a raw-keyed record must still settle.
func TestSettlePendingForSessionMatchesRawRecordKey(t *testing.T) {
	raw := questionRecord("ask_raw", "conv-1") // canonicalises to questionTestSession
	gw := &fakeHitlGateway{pendingQuestions: []ws.QuestionRecord{raw}}
	s, rec := questionTestServer(t, gw, questionTestSession)
	s.qroutes.put("ask_raw", raw.SessionKey)

	s.settlePendingForSession(context.Background(), "alice", questionTestSession)

	if len(gw.questionCancels) != 1 || gw.questionCancels[0] != "ask_raw|alice" {
		t.Fatalf("question cancels = %v, want the raw-keyed record cancelled", gw.questionCancels)
	}
	evs := sseEvents(t, rec.Body.String())
	if len(evs) != 1 || evs[0]["call_id"] != "ask_raw" || evs[0]["session_id"] != questionTestSession {
		t.Fatalf("events = %v, want one question_resolved on the canonical session", evs)
	}
}

// A settle cancels the session's questions whether or not the Portal could
// render them. The run is dead, so a record the render filters drop -- expired,
// or a secret/free-text question this version has no input for -- is still an
// open question nobody can answer any more; closing it is what stops a card
// being left behind.
func TestSettlePendingForSessionCancelsUnrenderableQuestions(t *testing.T) {
	expired := questionRecord("ask_expired", questionTestSession)
	expired.ExpiresAtMs = time.Now().Add(-time.Minute).UnixMilli()
	freeText := questionRecord("ask_freeform", questionTestSession)
	freeText.Questions[0].Options = nil // nothing to render as buttons

	gw := &fakeHitlGateway{pendingQuestions: []ws.QuestionRecord{expired, freeText}}
	s, rec := questionTestServer(t, gw, questionTestSession)

	s.settlePendingForSession(context.Background(), "alice", questionTestSession)

	got := map[string]bool{}
	for _, c := range gw.questionCancels {
		got[c] = true
	}
	for _, want := range []string{"ask_expired|alice", "ask_freeform|alice"} {
		if !got[want] {
			t.Errorf("question %q not cancelled; cancels = %v", want, gw.questionCancels)
		}
	}
	evs := sseEvents(t, rec.Body.String())
	if len(evs) != 2 {
		t.Fatalf("events = %v, want one question_resolved per cancelled record", evs)
	}
}

// A question.list failure leaves the gateway's own records alone, but the
// approval half has already run by then: a question-side failure must not
// resurrect a confirmation card.
func TestSettlePendingForSessionListQuestionsError(t *testing.T) {
	gw := &fakeHitlGateway{listQuestionsErr: errors.New("question.list: boom")}
	s, rec := questionTestServer(t, gw, questionTestSession)
	s.approvals.Begin("alice", pendingApproval{
		ApprovalID: "ap-1",
		SessionKey: questionTestSession,
		Tool:       "exec",
	})
	s.qroutes.put("ask_1", questionTestSession)

	s.settlePendingForSession(context.Background(), "alice", questionTestSession)

	if _, ok := s.approvals.Pending("alice", questionTestSession); ok {
		t.Error("approval half did not settle before the question.list failure")
	}
	if len(gw.questionCancels) != 0 {
		t.Errorf("question cancels = %v, want none on a list failure", gw.questionCancels)
	}
	evs := sseEvents(t, rec.Body.String())
	if eventOfType(evs, agentruntime.EventConfirmResolved) == nil {
		t.Fatalf("events = %v, want the confirm_resolved", evs)
	}
	if ev := eventOfType(evs, agentruntime.EventQuestionResolved); ev != nil {
		t.Fatalf("question_resolved published despite the list failure: %v", ev)
	}
	// The question may still be open on the gateway, so its route is kept: the
	// gateway's own resolved broadcast still has to address it.
	if _, ok := s.qroutes.take("ask_1"); !ok {
		t.Error("a question.list failure dropped the route")
	}
}

// A failed cancel is not a settled question: the route must survive so a later
// retry can still address the card, and nothing is published -- the card is
// still the user's until the gateway says otherwise.
func TestSettlePendingForSessionCancelQuestionError(t *testing.T) {
	gw := &fakeHitlGateway{
		pendingQuestions:   []ws.QuestionRecord{questionRecord("ask_1", questionTestSession)},
		questionResolveErr: errors.New("question.cancel: boom"),
	}
	s, rec := questionTestServer(t, gw, questionTestSession)
	s.qroutes.put("ask_1", questionTestSession)

	s.settlePendingForSession(context.Background(), "alice", questionTestSession)

	if len(gw.questionCancels) != 1 || gw.questionCancels[0] != "ask_1|alice" {
		t.Fatalf("question cancels = %v, want the attempted cancel recorded", gw.questionCancels)
	}
	if _, ok := s.qroutes.take("ask_1"); !ok {
		t.Error("route dropped although the cancel failed; a retry could no longer address the card")
	}
	if evs := sseEvents(t, rec.Body.String()); len(evs) != 0 {
		t.Fatalf("events = %v, want none after a failed cancel", evs)
	}
}
