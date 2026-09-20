package ws

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// approvalGateway records the params of every method it is asked for and replies
// with canned approval payloads.
type approvalGateway struct {
	mu     sync.Mutex
	params map[string]json.RawMessage
	// list is the payload exec.approval.list answers with. A nil value answers
	// the default two-entry list; an explicit empty slice answers [].
	list []map[string]any
}

func (g *approvalGateway) record(method string, params json.RawMessage) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.params == nil {
		g.params = map[string]json.RawMessage{}
	}
	g.params[method] = params
}

func (g *approvalGateway) got(t *testing.T, method string) map[string]any {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	var out map[string]any
	if err := json.Unmarshal(g.params[method], &out); err != nil {
		t.Fatalf("decode recorded %s params: %v", method, err)
	}
	return out
}

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

func newApprovalGateway(t *testing.T) (*httptest.Server, *approvalGateway) {
	g := &approvalGateway{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")
		ctx := context.Background()
		_ = conn.Write(ctx, websocket.MessageText, mustRaw(eventFrame{Type: "event", Event: challengeEvent, Payload: mustJSON(t, connectChallenge{Nonce: "n1"})}))
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var f requestFrame
			if err := json.Unmarshal(data, &f); err != nil {
				continue
			}
			if f.Method == "connect" {
				hello, _ := json.Marshal(map[string]any{"type": "hello-ok", "protocol": 4, "auth": map[string]any{"role": "operator", "scopes": []string{"operator.admin"}}})
				_ = conn.Write(ctx, websocket.MessageText, mustRaw(responseFrame{Type: "res", ID: f.ID, OK: true, Payload: hello}))
				continue
			}
			g.record(f.Method, f.Params)
			payload := json.RawMessage(`{"ok":true}`)
			if f.Method == "exec.approval.list" {
				list := g.list
				if list == nil {
					list = []map[string]any{
						approvalEntry("appr-1", "agent:main:conv-1", "kubectl delete pod foo", 1000, 9999999999999),
						approvalEntry("appr-2", "agent:main:conv-1", "kubectl apply -f bar.yaml", 1001, 9999999999999),
					}
				}
				payload = mustJSON(t, list)
			}
			_ = conn.Write(ctx, websocket.MessageText, mustRaw(responseFrame{Type: "res", ID: f.ID, OK: true, Payload: payload}))
		}
	}))
	return ts, g
}

func connectApprovalClient(t *testing.T, ts *httptest.Server, ctx context.Context) *Client {
	t.Helper()
	dev, err := GenerateDevice()
	if err != nil {
		t.Fatal(err)
	}
	c := NewClient(strings.Replace(ts.URL, "http", "ws", 1)+"/gateway", "token", dev)
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

// TestListApprovalsDecodesPendingRecords pins the exec.approval.list contract:
// the method takes no params, the payload is a bare array (not an object
// wrapping one), and each element carries the record's request -- sessionKey and
// command included -- plus its creation and expiry stamps. Those three fields
// are the whole reason this read replaces exec.approval.get: get answers with
// display text and no session key, so it can neither bind an approval to a
// conversation nor supply the command an "always allow" rule is derived from.
func TestListApprovalsDecodesPendingRecords(t *testing.T) {
	ts, g := newApprovalGateway(t)
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cli := connectApprovalClient(t, ts, ctx)

	list, err := cli.ListApprovals(ctx)
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
	if params := g.got(t, "exec.approval.list"); len(params) != 0 {
		t.Errorf("exec.approval.list params = %v, want none", params)
	}
}

// TestListApprovalsEmpty pins that an empty pending set is an empty slice, not a
// decode error: "nothing is pending" is the ordinary answer for a session that is
// not parked.
func TestListApprovalsEmpty(t *testing.T) {
	ts, g := newApprovalGateway(t)
	g.list = []map[string]any{}
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cli := connectApprovalClient(t, ts, ctx)

	list, err := cli.ListApprovals(ctx)
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
	ts, _ := newApprovalGateway(t)
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cli := connectApprovalClient(t, ts, ctx)
	resCh := make(chan ApprovalResolved, 1)
	cli.OnApprovalResolved(func(r ApprovalResolved) { resCh <- r })
	cli.OnEvent(func(string, []byte) {
		t.Error("approval events must not fall through to the live-stream observer")
	})

	cli.dispatchEvent(eventFrame{Type: "event", Event: execApprovalResolved, Payload: mustRaw(map[string]any{
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
	case <-ctx.Done():
		t.Fatal("exec.approval.resolved not dispatched")
	}
}
