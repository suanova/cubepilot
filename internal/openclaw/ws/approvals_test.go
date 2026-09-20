package ws

import (
	"context"
	"encoding/json"
	"testing"
)

// approvalEntry is one exec.approval.list element in the gateway's shape:
// {approvalKind, id, request, createdAtMs, expiresAtMs}.
func approvalEntry(id, sessionKey, command string, createdAtMs, expiresAtMs int64) map[string]any {
	return map[string]any{
		"approvalKind": "exec",
		"id":           id,
		"request": map[string]any{
			"command":    command,
			"sessionKey": sessionKey,
			"agentId":    "main",
			"security":   "full",
			"ask":        "on-miss",
		},
		"createdAtMs": createdAtMs,
		"expiresAtMs": expiresAtMs,
	}
}

// TestListApprovalsDecodesPendingRecords pins the exec.approval.list contract:
// the method takes no params, the payload is a bare array (not an object
// wrapping one), and each element carries the record's request -- sessionKey and
// command included -- plus its creation and expiry stamps. Those three fields
// are the whole reason this read replaces exec.approval.get: get answers with
// display text and no session key, so it can neither bind an approval to a
// conversation nor supply the command an "always allow" rule is derived from.
func TestListApprovalsDecodesPendingRecords(t *testing.T) {
	c, calls := newTestClient(t, func(method string, params json.RawMessage) (json.RawMessage, error) {
		if method != "exec.approval.list" {
			t.Fatalf("method = %q, want exec.approval.list", method)
		}
		return mustRaw([]map[string]any{
			approvalEntry("appr-1", "agent:main:conv-1", "kubectl delete pod foo", 1000, 9999999999999),
			approvalEntry("appr-2", "agent:main:conv-1", "kubectl apply -f bar.yaml", 1001, 9999999999999),
		}), nil
	})

	list, err := c.ListApprovals(context.Background())
	if err != nil {
		t.Fatalf("ListApprovals: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ListApprovals returned %d records, want 2", len(list))
	}
	first := list[0]
	if first.ID != "appr-1" || first.Kind != "exec" {
		t.Errorf("record = %+v, want id appr-1 kind exec", first)
	}
	if first.Request.SessionKey != "agent:main:conv-1" {
		t.Errorf("sessionKey = %q, want agent:main:conv-1", first.Request.SessionKey)
	}
	if first.Request.Command != "kubectl delete pod foo" {
		t.Errorf("command = %q", first.Request.Command)
	}
	if first.CreatedAtMs != 1000 || first.ExpiresAtMs != 9999999999999 {
		t.Errorf("stamps = (%d, %d)", first.CreatedAtMs, first.ExpiresAtMs)
	}
	if second := list[1]; second.ID != "appr-2" {
		t.Errorf("second record = %+v, want id appr-2", second)
	}
	// The method takes no arguments; an empty object is what the client sends.
	if len(*calls) != 1 || string((*calls)[0]) != "{}" {
		t.Errorf("recorded params = %v, want one empty object", *calls)
	}
}

// TestListApprovalsEmpty pins that an empty pending set is an empty slice, not a
// decode error: "nothing is pending" is the ordinary answer for a session that is
// not parked.
func TestListApprovalsEmpty(t *testing.T) {
	c, _ := newTestClient(t, func(method string, params json.RawMessage) (json.RawMessage, error) {
		return mustRaw([]map[string]any{}), nil
	})

	list, err := c.ListApprovals(context.Background())
	if err != nil {
		t.Fatalf("ListApprovals: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("ListApprovals returned %d records, want 0", len(list))
	}
}

// TestApprovalResolvedCarriesRequest pins the one field the relay depends on:
// the resolved broadcast echoes the whole request, so the session an approval
// belonged to arrives with the resolution. Without it the platform would need an
// id -> session table of the kind the question path keeps -- and a restart would
// lose it.
func TestApprovalResolvedCarriesRequest(t *testing.T) {
	c := &Client{}
	resCh := make(chan ApprovalResolved, 1)
	c.OnApprovalResolved(func(r ApprovalResolved) { resCh <- r })
	c.OnEvent(func(string, []byte) {
		t.Error("approval events must not fall through to the live-stream observer")
	})

	c.dispatchEvent(eventFrame{Type: "event", Event: execApprovalResolved, Payload: mustRaw(map[string]any{
		"id": "appr-1", "decision": "allow-once", "resolvedBy": "zhujian", "ts": 42,
		"request": map[string]any{"command": "kubectl delete pod foo", "sessionKey": "agent:main:conv-1"},
	})})

	select {
	case got := <-resCh:
		if got.ID != "appr-1" || got.Decision != "allow-once" || got.ResolvedBy != "zhujian" {
			t.Errorf("resolved = %+v", got)
		}
		if got.Request.SessionKey != "agent:main:conv-1" {
			t.Errorf("request.sessionKey = %q, want agent:main:conv-1", got.Request.SessionKey)
		}
		if got.Request.Command != "kubectl delete pod foo" {
			t.Errorf("request.command = %q", got.Request.Command)
		}
	default:
		t.Fatal("exec.approval.resolved not dispatched")
	}
}
