package ws

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// newTestClient returns a Client whose Call is served by fn, recording the raw
// params of each call in order. It replaces the real transport so method
// builders can be exercised without a gateway.
func newTestClient(t *testing.T, fn func(method string, params json.RawMessage) (json.RawMessage, error)) (*Client, *[]json.RawMessage) {
	t.Helper()
	calls := &[]json.RawMessage{}
	c := &Client{}
	c.callFn = func(_ context.Context, method string, params any) (json.RawMessage, error) {
		raw, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		*calls = append(*calls, raw)
		return fn(method, raw)
	}
	return c, calls
}

// AbortChat must omit runId entirely when unknown: a present-but-empty runId
// would be rejected by the gateway's schema, and omitting it is what selects the
// session-scoped abort.
func TestAbortChatOmitsEmptyRunID(t *testing.T) {
	c, calls := newTestClient(t, func(method string, params json.RawMessage) (json.RawMessage, error) {
		if method != "chat.abort" {
			t.Fatalf("method = %q, want chat.abort", method)
		}
		return json.RawMessage(`{"aborted":true}`), nil
	})

	if err := c.AbortChat(context.Background(), "session-a", ""); err != nil {
		t.Fatalf("AbortChat: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal((*calls)[0], &got); err != nil {
		t.Fatalf("decode params: %v", err)
	}
	if got["sessionKey"] != "session-a" {
		t.Fatalf("sessionKey = %v", got["sessionKey"])
	}
	if _, present := got["runId"]; present {
		t.Fatalf("runId must be omitted when empty, got %v", got["runId"])
	}
}

func TestAbortChatSendsRunID(t *testing.T) {
	c, calls := newTestClient(t, func(method string, _ json.RawMessage) (json.RawMessage, error) {
		// Pin the method here too, not only in the empty-runId case: a
		// regression that dropped the run id from a *correctly named* call and
		// a regression that sent it under the wrong method are different bugs,
		// and each test should catch its own.
		if method != "chat.abort" {
			t.Fatalf("method = %q, want chat.abort", method)
		}
		return json.RawMessage(`{"aborted":true}`), nil
	})
	if err := c.AbortChat(context.Background(), "session-a", "run-7"); err != nil {
		t.Fatalf("AbortChat: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal((*calls)[0], &got); err != nil {
		t.Fatalf("decode params: %v", err)
	}
	if got["runId"] != "run-7" {
		t.Fatalf("runId = %v, want run-7", got["runId"])
	}
}

func TestSessionBusyReadsInFlightRun(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"absent", `{"kind":"delta","messages":[]}`, false},
		{"null", `{"kind":"delta","inFlightRun":null}`, false},
		{"present", `{"kind":"delta","inFlightRun":{"runId":"r1"}}`, true},
		{"reset", `{"kind":"reset"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestClient(t, func(method string, _ json.RawMessage) (json.RawMessage, error) {
				if method != "chat.history" {
					t.Fatalf("method = %q, want chat.history", method)
				}
				return json.RawMessage(tc.body), nil
			})
			got, err := c.SessionBusy(context.Background(), "session-a")
			if err != nil {
				t.Fatalf("SessionBusy: %v", err)
			}
			if got != tc.want {
				t.Fatalf("SessionBusy = %v, want %v", got, tc.want)
			}
		})
	}
}

// SessionInFlightRun is the id an abort on the reload-takeover path is scoped
// to, so it has to decode the run descriptor the gateway reports -- a payload
// this client's schema does not model, read for its runId alone.
func TestSessionInFlightRunReadsRunID(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"absent", `{"kind":"delta","messages":[]}`, ""},
		{"null", `{"kind":"delta","inFlightRun":null}`, ""},
		{"present", `{"kind":"delta","inFlightRun":{"runId":"r1","startedAtMs":7}}`, "r1"},
		// A descriptor without a run id is still a run: the busy read must see it
		// even though there is nothing here to scope an abort to.
		{"no runId", `{"kind":"delta","inFlightRun":{"status":"running"}}`, ""},
		{"reset", `{"kind":"reset"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestClient(t, func(method string, _ json.RawMessage) (json.RawMessage, error) {
				if method != "chat.history" {
					t.Fatalf("method = %q, want chat.history", method)
				}
				return json.RawMessage(tc.body), nil
			})
			got, err := c.SessionInFlightRun(context.Background(), "session-a")
			if err != nil {
				t.Fatalf("SessionInFlightRun: %v", err)
			}
			if got != tc.want {
				t.Fatalf("SessionInFlightRun = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSessionInFlightRunPropagatesCallError: an unanswered read leaves the
// caller with no run id, and it must be able to tell that apart from a read that
// answered "nothing in flight" -- one falls back to a session-scoped abort, the
// other is the idempotent no-op case.
func TestSessionInFlightRunPropagatesCallError(t *testing.T) {
	wantErr := errors.New("ws: not connected")
	c, _ := newTestClient(t, func(method string, _ json.RawMessage) (json.RawMessage, error) {
		return nil, wantErr
	})
	id, err := c.SessionInFlightRun(context.Background(), "session-a")
	if !errors.Is(err, wantErr) {
		t.Fatalf("SessionInFlightRun error = %v, want %v", err, wantErr)
	}
	if id != "" {
		t.Fatalf("SessionInFlightRun = %q alongside an error, want empty", id)
	}
}

// TestSessionBusyPropagatesCallError: a failed chat.history means the busy state
// cannot be determined. Returning (false, nil) would read as "not busy" and let
// a caller strand a run that is still going.
func TestSessionBusyPropagatesCallError(t *testing.T) {
	wantErr := errors.New("ws: not connected")
	c, _ := newTestClient(t, func(method string, _ json.RawMessage) (json.RawMessage, error) {
		if method != "chat.history" {
			t.Fatalf("method = %q, want chat.history", method)
		}
		return nil, wantErr
	})
	busy, err := c.SessionBusy(context.Background(), "session-a")
	if !errors.Is(err, wantErr) {
		t.Fatalf("SessionBusy error = %v, want %v", err, wantErr)
	}
	if busy {
		t.Fatal("SessionBusy = true alongside an error")
	}
}
