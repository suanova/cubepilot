package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/suanova/cubepilot/internal/openclaw/ws"
	agentruntime "github.com/suanova/cubepilot/internal/runtime"
)

// A stopped turn must not leave its cards behind. The approvals are captured
// before the abort (the abort cancels them gateway-side, so there is nothing left
// to read afterwards) and the settle publishes a neutral resolution for each, on
// the stream the browser is still holding -- so its cards drop at once instead of
// waiting for a reload that would find nothing to restore.
func TestSettlePendingForSessionPublishesCapturedApprovals(t *testing.T) {
	h := NewHub()
	svc := NewApprovalService(h, nil, tLogf)
	rec := openStream(t, h, "agent:main:conv-1")
	s := &Server{hub: h, approvals: svc}

	s.settlePendingForSession(context.Background(), "admin", "agent:main:conv-1", []pendingApproval{
		{ApprovalID: "ap-1", SessionKey: "agent:main:conv-1", Tool: "exec", Command: "kubectl delete pod x"},
		{ApprovalID: "ap-2", SessionKey: "agent:main:conv-1", Tool: "exec", Command: "kubectl delete pod y"},
	})

	evs := sseEvents(t, rec.Body.String())
	if len(evs) != 2 {
		t.Fatalf("events = %v, want one approval_resolved per captured approval", evs)
	}
	ids := map[string]bool{}
	for _, ev := range evs {
		if ev["type"] != agentruntime.EventApprovalResolved || ev["sessionId"] != "agent:main:conv-1" {
			t.Fatalf("event = %v, want approval_resolved on the session", ev)
		}
		// A settle is not a decision: no allow/deny is implied, and the client
		// renders the absent Approved as the neutral "stopped".
		if _, ok := ev["approved"]; ok {
			t.Errorf("settled approval carried approved=%v", ev["approved"])
		}
		ids[ev["callId"].(string)] = true
	}
	if !ids["ap-1"] || !ids["ap-2"] {
		t.Errorf("resolved ids = %v, want both captured approvals", ids)
	}
}

// A settle with nothing captured publishes nothing: an idle Stop (or one whose
// capture could not be read) must not drop a card for an approval nobody asked
// about.
func TestSettlePendingForSessionWithoutCapturedApprovals(t *testing.T) {
	h := NewHub()
	rec := openStream(t, h, "agent:main:conv-1")
	s := &Server{hub: h, approvals: NewApprovalService(h, nil, tLogf)}

	s.settlePendingForSession(context.Background(), "admin", "agent:main:conv-1", nil)

	if evs := sseEvents(t, rec.Body.String()); len(evs) != 0 {
		t.Fatalf("events = %v, want none", evs)
	}
}

// A server built without an approval service must not panic. That is the shape
// the abort handler's fixture uses (&Server{hub: h, gatewayConns: m}), and both
// handleApproval and handlePendingApproval already treat a nil service as a real
// state; the settle has to as well.
func TestSettlePendingForSessionWithoutApprovalService(t *testing.T) {
	gw := &fakeGatewayClient{}
	base, _ := questionTestServer(t, gw, questionTestSession)

	// The bare fixture: the live gateway manager, no approval service and no
	// question routes.
	s := &Server{hub: base.hub, gatewayConns: base.gatewayConns}
	s.settlePendingForSession(context.Background(), "alice", questionTestSession, nil)

	if len(gw.questionCancels) != 0 {
		t.Fatalf("question cancels = %v, want none (nothing was asked)", gw.questionCancels)
	}
}

// The question half of the settle cancels the session's open questions on the
// gateway, drops their routing entries, and publishes question_resolved; a
// question belonging to another session is untouched.
func TestSettlePendingForSessionClearsQuestions(t *testing.T) {
	mine := questionRecord("ask_1", questionTestSession)
	other := questionRecord("ask_other", "agent:main:other-session")
	gw := &fakeGatewayClient{pendingQuestions: []ws.QuestionRecord{mine, other}}
	s, rec := questionTestServer(t, gw, questionTestSession)
	s.qroutes.put("ask_1", questionTestSession)

	s.settlePendingForSession(context.Background(), "alice", questionTestSession, nil)

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
		evs[0]["callId"] != "ask_1" || evs[0]["message"] != "cancelled" {
		t.Fatalf("event = %v, want question_resolved/cancelled for ask_1", evs[0])
	}
}

// The record key is canonicalised before it is compared. The gateway reports
// the raw key (question.list is the same input handleQuestion and
// handlePendingQuestion canonicalise), while the abort handler passes the
// canonical one, so a raw-keyed record must still settle.
func TestSettlePendingForSessionMatchesRawRecordKey(t *testing.T) {
	raw := questionRecord("ask_raw", "conv-1") // canonicalises to questionTestSession
	gw := &fakeGatewayClient{pendingQuestions: []ws.QuestionRecord{raw}}
	s, rec := questionTestServer(t, gw, questionTestSession)
	s.qroutes.put("ask_raw", raw.SessionKey)

	s.settlePendingForSession(context.Background(), "alice", questionTestSession, nil)

	if len(gw.questionCancels) != 1 || gw.questionCancels[0] != "ask_raw|alice" {
		t.Fatalf("question cancels = %v, want the raw-keyed record cancelled", gw.questionCancels)
	}
	evs := sseEvents(t, rec.Body.String())
	if len(evs) != 1 || evs[0]["callId"] != "ask_raw" || evs[0]["sessionId"] != questionTestSession {
		t.Fatalf("events = %v, want one question_resolved on the canonical session", evs)
	}
}

// A settle cancels the session's questions whether or not the Portal could
// render them. The run is dead, so a record the render filters drop -- expired,
// or a secret question this version has no input for -- is still an open
// question nobody can answer any more; closing it is what stops a card being
// left behind.
func TestSettlePendingForSessionCancelsUnrenderableQuestions(t *testing.T) {
	expired := questionRecord("ask_expired", questionTestSession)
	expired.ExpiresAtMs = time.Now().Add(-time.Minute).UnixMilli()
	secret := questionRecord("ask_secret", questionTestSession)
	secret.Questions[0].IsSecret = true // a variant the render filters drop

	gw := &fakeGatewayClient{pendingQuestions: []ws.QuestionRecord{expired, secret}}
	s, rec := questionTestServer(t, gw, questionTestSession)

	s.settlePendingForSession(context.Background(), "alice", questionTestSession, nil)

	got := map[string]bool{}
	for _, c := range gw.questionCancels {
		got[c] = true
	}
	for _, want := range []string{"ask_expired|alice", "ask_secret|alice"} {
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
// approval half has already run by then: a question-side failure must not leave
// a confirmation card behind for the stopped turn.
func TestSettlePendingForSessionListQuestionsError(t *testing.T) {
	gw := &fakeGatewayClient{listQuestionsErr: errors.New("question.list: boom")}
	s, rec := questionTestServer(t, gw, questionTestSession)
	s.qroutes.put("ask_1", questionTestSession)

	s.settlePendingForSession(context.Background(), "alice", questionTestSession, []pendingApproval{{
		ApprovalID: "ap-1",
		SessionKey: questionTestSession,
		Tool:       "exec",
	}})

	if len(gw.questionCancels) != 0 {
		t.Errorf("question cancels = %v, want none on a list failure", gw.questionCancels)
	}
	evs := sseEvents(t, rec.Body.String())
	resolved := eventOfType(evs, agentruntime.EventApprovalResolved)
	if resolved == nil {
		t.Fatalf("events = %v, want the approval_resolved", evs)
	}
	if resolved["callId"] != "ap-1" {
		t.Errorf("approval_resolved = %v, want ap-1", resolved)
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
	gw := &fakeGatewayClient{
		pendingQuestions:   []ws.QuestionRecord{questionRecord("ask_1", questionTestSession)},
		questionResolveErr: errors.New("question.cancel: boom"),
	}
	s, rec := questionTestServer(t, gw, questionTestSession)
	s.qroutes.put("ask_1", questionTestSession)

	s.settlePendingForSession(context.Background(), "alice", questionTestSession, nil)

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
