package server

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/suanova/cubepilot/internal/openclaw/ws"
	agentruntime "github.com/suanova/cubepilot/internal/runtime"
)

// A stopped turn must not leave a card behind: after settling, the session has
// no pending confirmation, the record is gone from the recovery lookup, and a
// confirm_resolved event was published to any attached stream.
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

	s := &Server{hub: h, approvals: svc}
	s.settlePendingForSession(context.Background(), "admin", "conv-1")

	if _, ok := svc.Pending("admin", "conv-1"); ok {
		t.Fatal("pending confirmation survived the settle")
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
