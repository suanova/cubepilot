package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/suanova/cubepilot/internal/config"
	"github.com/suanova/cubepilot/internal/openclaw"
	"github.com/suanova/cubepilot/internal/store"
)

type stubResolver struct {
	calls []string // "user|approvalID|decision"
	err   error
}

func (r *stubResolver) ResolveApproval(_ context.Context, user, approvalID, decision string) error {
	r.calls = append(r.calls, user+"|"+approvalID+"|"+decision)
	return r.err
}

func TestApprovalService_BeginPublishesConfirmPending(t *testing.T) {
	hub := NewHub()
	rec := httptest.NewRecorder()
	if _, err := hub.Open("conv-1", rec, rec); err != nil {
		t.Fatal(err)
	}
	svc := NewApprovalService(hub, nil, t.Logf)

	svc.Begin("alice", pendingApproval{
		ApprovalID: "appr-1",
		SessionKey: "conv-1",
		Tool:       "exec",
		Command:    "kubectl delete pod foo",
		Message:    "approve?",
	})

	body := rec.Body.String()
	if !strings.Contains(body, "event: confirm_pending") || !strings.Contains(body, `"call_id":"appr-1"`) {
		t.Fatalf("expected confirm_pending in stream, got %q", body)
	}
	if !strings.Contains(body, `"command":"kubectl delete pod foo"`) {
		t.Fatalf("expected command in confirm_pending, got %q", body)
	}

	p, ok := svc.Pending("alice", "conv-1")
	if !ok || p.ApprovalID != "appr-1" {
		t.Fatalf("Pending = %+v, %v; want appr-1", p, ok)
	}
	if _, ok := svc.Pending("bob", "conv-1"); ok {
		t.Fatal("Pending must be owner-scoped")
	}
}

func TestApprovalService_ResolveApproveAndReject(t *testing.T) {
	for _, tc := range []struct {
		decision string
		approved bool
	}{
		{"approve", true},
		{"reject", false},
	} {
		hub := NewHub()
		rec := httptest.NewRecorder()
		if _, err := hub.Open("conv-1", rec, rec); err != nil {
			t.Fatal(err)
		}
		st, err := store.New(t.TempDir(), "test-model")
		if err != nil {
			t.Fatal(err)
		}
		res := &stubResolver{}
		svc := NewApprovalService(hub, st, t.Logf)
		svc.SetResolver(res)
		svc.Begin("alice", pendingApproval{ApprovalID: "appr-1", SessionKey: "conv-1", Command: "kubectl delete pod foo"})

		if _, err := svc.Resolve("alice", "conv-1", tc.decision); err != nil {
			t.Fatalf("Resolve(%s): %v", tc.decision, err)
		}

		if len(res.calls) != 1 || res.calls[0] != "alice|appr-1|"+tc.decision {
			t.Errorf("resolver calls = %v, want alice|appr-1|%s", res.calls, tc.decision)
		}
		if _, ok := svc.Pending("alice", "conv-1"); ok {
			t.Error("pending must be cleared after resolve")
		}
		body := rec.Body.String()
		if !strings.Contains(body, "event: confirm_resolved") || !strings.Contains(body, `"approved":`+map[bool]string{true: "true", false: "false"}[tc.approved]) {
			t.Errorf("expected confirm_resolved approved=%v in stream, got %q", tc.approved, body)
		}
		msgs, _ := st.ListMessages("conv-1", 0)
		found := false
		for _, m := range msgs {
			if m.EventType == openclaw.EventConfirmResolved && m.CallID == "appr-1" {
				found = true
			}
		}
		if !found {
			t.Errorf("expected confirm_resolved ledger row, got %+v", msgs)
		}
		audit, _ := st.ListAudit(0)
		if len(audit) == 0 || audit[0].Status != map[bool]string{true: "approved", false: "rejected"}[tc.approved] {
			t.Errorf("expected audit decision row, got %+v", audit)
		}
	}
}

func TestApprovalService_ResolveErrors(t *testing.T) {
	svc := NewApprovalService(NewHub(), nil, t.Logf)
	svc.Begin("alice", pendingApproval{ApprovalID: "appr-1", SessionKey: "conv-1"})

	if _, err := svc.Resolve("alice", "conv-1", "maybe"); err == nil {
		t.Fatal("expected error for an unknown decision")
	}
	if _, err := svc.Resolve("bob", "conv-1", "approve"); err == nil {
		t.Fatal("expected errNoPending for a non-owner")
	}
	if _, err := svc.Resolve("alice", "conv-missing", "approve"); err == nil {
		t.Fatal("expected errNoPending for a missing session")
	}

	// nil resolver → errNoResolver, pending retained.
	if _, err := svc.Resolve("alice", "conv-1", "approve"); err == nil {
		t.Fatal("expected errNoResolver")
	}
	if _, ok := svc.Pending("alice", "conv-1"); !ok {
		t.Fatal("pending must be retained when the resolver is unavailable")
	}

	// resolver error → pending retained for retry.
	res := &stubResolver{err: context.DeadlineExceeded}
	svc.SetResolver(res)
	if _, err := svc.Resolve("alice", "conv-1", "approve"); err == nil {
		t.Fatal("expected resolver error to propagate")
	}
	if _, ok := svc.Pending("alice", "conv-1"); !ok {
		t.Fatal("pending must be retained after a failed resolve")
	}
}

func TestHandleConfirmAndPending(t *testing.T) {
	st, err := store.New(t.TempDir(), "test-model")
	if err != nil {
		t.Fatal(err)
	}
	srv := New(config.Config{DefaultUser: "alice"}, nil, st, nil, nil)
	res := &stubResolver{}
	srv.approvals.SetResolver(res)
	srv.approvals.Begin("alice", pendingApproval{
		ApprovalID: "appr-1", SessionKey: "conv-1", Command: "kubectl delete pod foo",
	})

	// pending GET
	rec := doReq(t, srv.Handler(), http.MethodGet, "/api/sessions/conv-1/confirm/pending", "alice", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("pending GET status = %d, body %s", rec.Code, rec.Body.String())
	}
	var pend map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &pend); err != nil {
		t.Fatal(err)
	}
	if pend["approval_id"] != "appr-1" {
		t.Errorf("pending approval_id = %v", pend["approval_id"])
	}

	// confirm approve
	rec = doReq(t, srv.Handler(), http.MethodPost, "/api/sessions/conv-1/confirm", "alice", map[string]any{"decision": "approve"})
	if rec.Code != http.StatusOK {
		t.Fatalf("confirm status = %d, body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"approved":true`) {
		t.Errorf("confirm body = %s", rec.Body.String())
	}
	if len(res.calls) != 1 || res.calls[0] != "alice|appr-1|approve" {
		t.Errorf("resolver calls = %v", res.calls)
	}

	// second confirm → 404 (already resolved)
	rec = doReq(t, srv.Handler(), http.MethodPost, "/api/sessions/conv-1/confirm", "alice", map[string]any{"decision": "reject"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("second confirm status = %d", rec.Code)
	}

	// invalid decision → 400
	srv.approvals.Begin("alice", pendingApproval{ApprovalID: "appr-2", SessionKey: "conv-2"})
	rec = doReq(t, srv.Handler(), http.MethodPost, "/api/sessions/conv-2/confirm", "alice", map[string]any{"decision": "maybe"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid decision status = %d", rec.Code)
	}
}
