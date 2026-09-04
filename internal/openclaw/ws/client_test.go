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

// mockGateway implements just enough of the server side of the protocol
// (v2026.8.2) to exercise the client: challenge → connect → method responses,
// plus a pushed exec.approval.requested event.
type mockGateway struct {
	t          *testing.T
	conn       *websocket.Conn
	gotConnect json.RawMessage
}

func newMockGateway(t *testing.T) *httptest.Server {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		mg := &mockGateway{t: t, conn: conn}
		mg.serve()
	}))
	return ts
}

func (mg *mockGateway) write(v any) {
	b, _ := json.Marshal(v)
	if err := mg.conn.Write(context.Background(), websocket.MessageText, b); err != nil {
		mg.t.Logf("mock write: %v", err)
	}
}

func (mg *mockGateway) serve() {
	mg.write(eventFrame{Type: "event", Event: challengeEvent, Payload: mustJSON(mg.t, connectChallenge{Nonce: "nonce-1"})})

	// One connect request.
	var req requestFrame
	_, data, err := mg.conn.Read(context.Background())
	if err != nil {
		mg.t.Errorf("mock read connect: %v", err)
		return
	}
	if err := json.Unmarshal(data, &req); err != nil {
		mg.t.Errorf("mock decode connect: %v", err)
		return
	}
	if req.Method != "connect" {
		mg.t.Errorf("mock: first method = %q, want connect", req.Method)
		return
	}
	mg.gotConnect = req.Params
	hello, _ := json.Marshal(map[string]any{
		"type": "hello-ok", "protocol": 4,
		"auth": map[string]any{"role": "operator", "scopes": []string{"operator.admin", "operator.approvals"}, "deviceToken": "tok-1"},
	})
	mg.write(responseFrame{Type: "res", ID: req.ID, OK: true, Payload: hello})

	// Push an approval request so event dispatch is exercised.
	go func() {
		time.Sleep(50 * time.Millisecond)
		payload, _ := json.Marshal(map[string]any{
			"approvalKind": "exec",
			"id":           "appr-1",
			"request": map[string]any{
				"command":    "kubectl delete pod foo",
				"sessionKey": "conv-1",
				"agentId":    "main",
			},
			"createdAtMs": 1,
			"expiresAtMs": 9999999999,
		})
		mg.write(eventFrame{Type: "event", Event: execApprovalRequested, Payload: payload})
	}()

	for {
		_, data, err := mg.conn.Read(context.Background())
		if err != nil {
			return // client gone
		}
		var f requestFrame
		if err := json.Unmarshal(data, &f); err != nil {
			continue
		}
		mg.respondMethod(f)
	}
}

func (mg *mockGateway) respondMethod(f requestFrame) {
	switch f.Method {
	case "exec.approvals.get":
		payload, _ := json.Marshal(map[string]any{
			"exists": false,
			"hash":   "h0",
			"file":   map[string]any{"version": 1},
		})
		mg.write(responseFrame{Type: "res", ID: f.ID, OK: true, Payload: payload})
	case "device.pair.list":
		payload, _ := json.Marshal(map[string]any{
			"pending": []map[string]any{{
				"requestId": "req-1", "deviceId": "dev-abc", "publicKey": "pk",
				"role": "operator", "scopes": []string{"operator.admin"},
			}},
			"paired": []any{},
		})
		mg.write(responseFrame{Type: "res", ID: f.ID, OK: true, Payload: payload})
	default:
		mg.write(responseFrame{Type: "res", ID: f.ID, OK: true, Payload: json.RawMessage(`{"ok":true}`)})
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestClientConnectAndCall(t *testing.T) {
	dev, err := GenerateDevice()
	if err != nil {
		t.Fatal(err)
	}
	ts := newMockGateway(t)
	defer ts.Close()

	url := strings.Replace(ts.URL, "http", "ws", 1) + "/gateway"
	cli := NewClient(url, "token", dev)

	var (
		mu   sync.Mutex
		got  *ApprovalRequested
		evCh = make(chan struct{}, 1)
	)
	cli.OnApprovalRequested(func(a ApprovalRequested) {
		mu.Lock()
		got = &a
		mu.Unlock()
		select {
		case evCh <- struct{}{}:
		default:
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := cli.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer cli.Close()

	snap, err := cli.GetApprovalsPolicy(ctx)
	if err != nil {
		t.Fatalf("GetApprovalsPolicy: %v", err)
	}
	if snap.Exists || snap.Hash != "h0" {
		t.Errorf("snapshot = %+v", snap)
	}

	if err := cli.EnsureSessionGuarded(ctx, "conv-1"); err != nil {
		t.Fatalf("EnsureSessionGuarded: %v", err)
	}
	if err := cli.ResolveApproval(ctx, "appr-1", "allow-once"); err != nil {
		t.Fatalf("ResolveApproval: %v", err)
	}

	select {
	case <-evCh:
	case <-ctx.Done():
		t.Fatal("approval request event not received")
	}
	mu.Lock()
	defer mu.Unlock()
	if got == nil || got.ID != "appr-1" || got.Request.Command != "kubectl delete pod foo" || got.Request.SessionKey != "conv-1" {
		t.Errorf("approval event = %+v", got)
	}
}

func TestClientConnectNotPairedFails(t *testing.T) {
	dev, err := GenerateDevice()
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		payload, _ := json.Marshal(connectChallenge{Nonce: "n1"})
		_ = conn.Write(context.Background(), websocket.MessageText, mustRaw(eventFrame{Type: "event", Event: challengeEvent, Payload: payload}))
		_, data, err := conn.Read(context.Background())
		if err != nil {
			return
		}
		var f requestFrame
		_ = json.Unmarshal(data, &f)
		_ = conn.Write(context.Background(), websocket.MessageText, mustRaw(responseFrame{
			Type: "res", ID: f.ID, OK: false,
			Error: &frameError{Code: "NOT_PAIRED", Message: "device is not approved yet"},
		}))
		_ = conn.Close(websocket.StatusPolicyViolation, "pairing required")
	}))
	defer ts.Close()

	cli := NewClient(strings.Replace(ts.URL, "http", "ws", 1)+"/gateway", "token", dev)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err = cli.Connect(ctx)
	if err == nil || !strings.Contains(err.Error(), "NOT_PAIRED") {
		t.Fatalf("Connect err = %v, want NOT_PAIRED", err)
	}
}

// TestClientDeviceLessConnect verifies a nil-Device connection is sent without
// a device proof (loopback bootstrap path used by the supervisor to approve
// device pairings).
func TestClientDeviceLessConnect(t *testing.T) {
	var gotConnect connectParams
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		_ = conn.Write(context.Background(), websocket.MessageText, mustRaw(eventFrame{Type: "event", Event: challengeEvent, Payload: mustJSON(t, connectChallenge{Nonce: "n1"})}))
		_, data, err := conn.Read(context.Background())
		if err != nil {
			return
		}
		var f requestFrame
		if err := json.Unmarshal(data, &f); err != nil {
			return
		}
		if err := json.Unmarshal(f.Params, &gotConnect); err != nil {
			return
		}
		hello, _ := json.Marshal(map[string]any{"type": "hello-ok", "protocol": 4, "auth": map[string]any{"role": "operator", "scopes": []string{"operator.admin"}}})
		_ = conn.Write(context.Background(), websocket.MessageText, mustRaw(responseFrame{Type: "res", ID: f.ID, OK: true, Payload: hello}))
		for {
			if _, _, err := conn.Read(context.Background()); err != nil {
				return // client gone
			}
		}
	}))
	defer ts.Close()

	cli := NewClient(strings.Replace(ts.URL, "http", "ws", 1)+"/gateway", "token", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := cli.Connect(ctx); err != nil {
		t.Fatalf("device-less connect: %v", err)
	}
	defer cli.Close()
	if gotConnect.Device != nil {
		t.Errorf("device-less connect must not send a device proof, got %+v", gotConnect.Device)
	}
	if gotConnect.Auth == nil || gotConnect.Auth.Token != "token" {
		t.Errorf("device-less connect must carry the shared token, got %+v", gotConnect.Auth)
	}
	if gotConnect.Scopes == nil || len(gotConnect.Scopes) == 0 {
		t.Errorf("device-less connect must declare scopes, got %v", gotConnect.Scopes)
	}
}

func TestClientDevicePairList(t *testing.T) {
	dev, err := GenerateDevice()
	if err != nil {
		t.Fatal(err)
	}
	ts := newMockGateway(t)
	defer ts.Close()
	cli := NewClient(strings.Replace(ts.URL, "http", "ws", 1)+"/gateway", "token", dev)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := cli.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	lst, err := cli.DevicePairList(ctx)
	if err != nil {
		t.Fatalf("DevicePairList: %v", err)
	}
	if len(lst.Pending) != 1 || lst.Pending[0].RequestID != "req-1" || lst.Pending[0].DeviceID != "dev-abc" {
		t.Errorf("pending = %+v", lst.Pending)
	}
	if err := cli.DevicePairApprove(ctx, "req-1"); err != nil {
		t.Fatalf("DevicePairApprove: %v", err)
	}
}

func mustRaw(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
