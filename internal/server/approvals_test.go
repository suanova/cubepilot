package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/suanova/cubepilot/internal/config"
	"github.com/suanova/cubepilot/internal/openclaw/ws"
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

func TestApprovalService_BeginPublishesApprovalPending(t *testing.T) {
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
	if !strings.Contains(body, "event: approval_pending") || !strings.Contains(body, `"callId":"appr-1"`) {
		t.Fatalf("expected approval_pending in stream, got %q", body)
	}
	if !strings.Contains(body, `"command":"kubectl delete pod foo"`) {
		t.Fatalf("expected command in approval_pending, got %q", body)
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
		gw       string // decision passed to the gateway resolver
	}{
		{"approve", true, "approve"},
		{"reject", false, "reject"},
		{"allow-always", true, "approve"}, // approve-once now; durable grant appended separately (issue #116)
	} {
		hub := NewHub()
		rec := httptest.NewRecorder()
		if _, err := hub.Open("conv-1", rec, rec); err != nil {
			t.Fatal(err)
		}
		st, err := store.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		res := &stubResolver{}
		svc := NewApprovalService(hub, st, t.Logf)
		svc.SetResolver(res)
		svc.Begin("alice", pendingApproval{ApprovalID: "appr-1", SessionKey: "conv-1", Command: "kubectl delete pod foo"})

		if _, err := svc.Resolve(context.Background(), "alice", "conv-1", tc.decision); err != nil {
			t.Fatalf("Resolve(%s): %v", tc.decision, err)
		}

		if len(res.calls) != 1 || res.calls[0] != "alice|appr-1|"+tc.gw {
			t.Errorf("resolver calls = %v, want alice|appr-1|%s", res.calls, tc.gw)
		}
		if _, ok := svc.Pending("alice", "conv-1"); ok {
			t.Error("pending must be cleared after resolve")
		}
		body := rec.Body.String()
		if !strings.Contains(body, "event: approval_resolved") || !strings.Contains(body, `"approved":`+map[bool]string{true: "true", false: "false"}[tc.approved]) {
			t.Errorf("expected approval_resolved approved=%v in stream, got %q", tc.approved, body)
		}
		audit, _ := st.ListAudit("alice", 0)
		if len(audit) == 0 || audit[0].Status != map[bool]string{true: "approved", false: "rejected"}[tc.approved] {
			t.Errorf("expected audit decision row, got %+v", audit)
		}
	}
}

func TestApprovalService_ResolveErrors(t *testing.T) {
	svc := NewApprovalService(NewHub(), nil, t.Logf)
	svc.Begin("alice", pendingApproval{ApprovalID: "appr-1", SessionKey: "conv-1"})

	if _, err := svc.Resolve(context.Background(), "alice", "conv-1", "maybe"); err == nil {
		t.Fatal("expected error for an unknown decision")
	}
	if _, err := svc.Resolve(context.Background(), "bob", "conv-1", "approve"); err == nil {
		t.Fatal("expected errNoPending for a non-owner")
	}
	if _, err := svc.Resolve(context.Background(), "alice", "conv-missing", "approve"); err == nil {
		t.Fatal("expected errNoPending for a missing session")
	}

	// nil resolver → errNoResolver, pending retained.
	if _, err := svc.Resolve(context.Background(), "alice", "conv-1", "approve"); err == nil {
		t.Fatal("expected errNoResolver")
	}
	if _, ok := svc.Pending("alice", "conv-1"); !ok {
		t.Fatal("pending must be retained when the resolver is unavailable")
	}

	// resolver error → pending retained for retry.
	res := &stubResolver{err: context.DeadlineExceeded}
	svc.SetResolver(res)
	if _, err := svc.Resolve(context.Background(), "alice", "conv-1", "approve"); err == nil {
		t.Fatal("expected resolver error to propagate")
	}
	if _, ok := svc.Pending("alice", "conv-1"); !ok {
		t.Fatal("pending must be retained after a failed resolve")
	}
}

func TestHandleApprovalAndPending(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := New(config.Config{DefaultUser: "alice"}, nil, st, nil, nil)
	res := &stubResolver{}
	srv.approvals.SetResolver(res)
	srv.approvals.Begin("alice", pendingApproval{
		ApprovalID: "appr-1", SessionKey: "conv-1", Command: "kubectl delete pod foo",
	})

	// pending GET: the pending approval is wrapped in an "approval" envelope.
	rec := doReq(t, srv.Handler(), http.MethodGet, "/api/v1/sessions/conv-1/approval/pending", "alice", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("pending GET status = %d, body %s", rec.Code, rec.Body.String())
	}
	var pend struct {
		Approval *struct {
			SessionID  string `json:"sessionId"`
			ApprovalID string `json:"approvalId"`
			Tool       string `json:"tool"`
			Command    string `json:"command"`
		} `json:"approval"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &pend); err != nil {
		t.Fatal(err)
	}
	if pend.Approval == nil {
		t.Fatalf("pending GET body = %s, want an \"approval\" envelope", rec.Body.String())
	}
	if pend.Approval.ApprovalID != "appr-1" || pend.Approval.SessionID != "conv-1" || pend.Approval.Command != "kubectl delete pod foo" {
		t.Errorf("pending approval = %+v, want appr-1 on conv-1", *pend.Approval)
	}

	// approve
	rec = doReq(t, srv.Handler(), http.MethodPost, "/api/v1/sessions/conv-1/approval", "alice", map[string]any{"decision": "approve"})
	if rec.Code != http.StatusOK {
		t.Fatalf("approval status = %d, body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"approved":true`) {
		t.Errorf("approval body = %s", rec.Body.String())
	}
	if len(res.calls) != 1 || res.calls[0] != "alice|appr-1|approve" {
		t.Errorf("resolver calls = %v", res.calls)
	}

	// second approval → 404 (already resolved)
	rec = doReq(t, srv.Handler(), http.MethodPost, "/api/v1/sessions/conv-1/approval", "alice", map[string]any{"decision": "reject"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("second approval status = %d", rec.Code)
	}

	// invalid decision → 400
	srv.approvals.Begin("alice", pendingApproval{ApprovalID: "appr-2", SessionKey: "conv-2"})
	rec = doReq(t, srv.Handler(), http.MethodPost, "/api/v1/sessions/conv-2/approval", "alice", map[string]any{"decision": "maybe"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid decision status = %d", rec.Code)
	}
}

// TestHandleConfirm_OwnerScoped proves a forged X-CubePilot-User cannot resolve
// another operator's pending approval (issue #20 code review).
func TestHandleConfirm_OwnerScoped(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := New(config.Config{DefaultUser: "alice"}, nil, st, nil, nil)
	res := &stubResolver{}
	srv.approvals.SetResolver(res)
	srv.approvals.Begin("alice", pendingApproval{ApprovalID: "appr-1", SessionKey: "conv-1", Command: "kubectl delete pod foo"})

	// bob (not the owner) cannot read or resolve it.
	rec := doReq(t, srv.Handler(), http.MethodGet, "/api/v1/sessions/conv-1/approval/pending", "bob", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("bob pending GET status = %d, want 404", rec.Code)
	}
	rec = doReq(t, srv.Handler(), http.MethodPost, "/api/v1/sessions/conv-1/approval", "bob", map[string]any{"decision": "reject"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("bob confirm status = %d, want 404", rec.Code)
	}
	if len(res.calls) != 0 {
		t.Fatalf("resolver must not be called for a non-owner, got %v", res.calls)
	}

	// The owner still can.
	rec = doReq(t, srv.Handler(), http.MethodPost, "/api/v1/sessions/conv-1/approval", "alice", map[string]any{"decision": "approve"})
	if rec.Code != http.StatusOK {
		t.Fatalf("alice confirm status = %d, want 200", rec.Code)
	}
}

// An approval the gateway ends by itself -- it expired unanswered, or its run
// was aborted or lost gateway-side -- has to leave the ledger. Nothing else
// removes it: Resolve only runs when the Portal decides, and the abort settle
// only runs when the platform stopped the run. Left behind, reload recovery
// paints a card for an approval the gateway has forgotten, and the answer to it
// fails. The gateway's exec.approval.resolved broadcast is the only signal that
// this happened.
func TestApprovalService_SettleApprovalResolvedByGateway(t *testing.T) {
	hub := NewHub()
	rec := httptest.NewRecorder()
	if _, err := hub.Open("conv-1", rec, rec); err != nil {
		t.Fatal(err)
	}
	svc := NewApprovalService(hub, nil, t.Logf)
	svc.Begin("alice", pendingApproval{ApprovalID: "appr-1", SessionKey: "conv-1", Command: "kubectl delete pod foo"})

	p, ok := svc.settleApproval("alice", "appr-1")
	if !ok || p.SessionKey != "conv-1" {
		t.Fatalf("settleApproval = %+v, %v; want the record claimed", p, ok)
	}
	if _, ok := svc.Pending("alice", "conv-1"); ok {
		t.Fatal("a gateway-resolved approval must not stay pending: reload would resurrect its card")
	}
	// Idempotent: the same broadcast also follows the Portal's own decision and
	// the abort settle, so a second delivery must be a harmless no-op rather than
	// a second claim.
	if _, ok := svc.settleApproval("alice", "appr-1"); ok {
		t.Fatal("second settleApproval claimed the record again")
	}
	if p, ok := svc.settleApproval("alice", ""); ok || p.ApprovalID != "" {
		t.Fatal("settleApproval with no id must claim nothing")
	}
}

// The broadcast carries no user, so the connection's user is what scopes it:
// another operator's record is not this caller's to clear.
func TestApprovalService_SettleApprovalIsOwnerScoped(t *testing.T) {
	svc := NewApprovalService(NewHub(), nil, t.Logf)
	svc.Begin("alice", pendingApproval{ApprovalID: "appr-1", SessionKey: "conv-1"})

	if _, ok := svc.settleApproval("bob", "appr-1"); ok {
		t.Fatal("bob settled alice's approval")
	}
	if _, ok := svc.Pending("alice", "conv-1"); !ok {
		t.Fatal("alice's approval must survive another user's settle")
	}
}

// The server half: the broadcast drops the ledger record and tells an attached
// view to drop the card, with Approved reported only for a decision the gateway
// actually recorded -- an expiry is not a rejection the user made.
func TestSettleApprovalResolved(t *testing.T) {
	for _, tc := range []struct {
		name         string
		decision     string
		resolvedBy   string
		wantApproved string // "" means the field must be absent
	}{
		// The cases the gateway produces for an approval it ended itself. Both
		// carry no resolver, and the second is why Decision alone cannot decide
		// what to show: the gateway fills an absent decision with "deny".
		{"expiry reports no decision", "", "", ""},
		{"cancelled run reports no decision", "deny", "", ""},
		{"allow-once", "allow-once", "device-1", `"approved":true`},
		{"deny", "deny", "device-1", `"approved":false`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub := NewHub()
			rec := httptest.NewRecorder()
			if _, err := hub.Open("conv-1", rec, rec); err != nil {
				t.Fatal(err)
			}
			srv := &Server{hub: hub, approvals: NewApprovalService(hub, nil, t.Logf)}
			srv.approvals.Begin("alice", pendingApproval{ApprovalID: "appr-1", SessionKey: "conv-1"})

			srv.settleApprovalResolved("alice", ws.ApprovalResolved{
				ID:         "appr-1",
				Decision:   tc.decision,
				ResolvedBy: tc.resolvedBy,
			})

			if _, ok := srv.approvals.Pending("alice", "conv-1"); ok {
				t.Fatal("record must be gone after the gateway resolves it")
			}
			body := rec.Body.String()
			if !strings.Contains(body, "event: approval_resolved") || !strings.Contains(body, `"callId":"appr-1"`) {
				t.Fatalf("expected approval_resolved for appr-1, got %q", body)
			}
			if tc.wantApproved == "" {
				if strings.Contains(body, `"approved":`) {
					t.Fatalf("an undecided resolution must not report a decision, got %q", body)
				}
				return
			}
			if !strings.Contains(body, tc.wantApproved) {
				t.Fatalf("expected %s in stream, got %q", tc.wantApproved, body)
			}
		})
	}
}
