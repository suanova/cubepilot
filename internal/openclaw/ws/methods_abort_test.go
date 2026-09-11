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

	if aborted, err := c.AbortChat(context.Background(), "session-a", ""); err != nil {
		t.Fatalf("AbortChat: %v", err)
	} else if !aborted {
		t.Fatal("AbortChat reported no abort for a payload that says aborted=true")
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
	if aborted, err := c.AbortChat(context.Background(), "session-a", "run-7"); err != nil {
		t.Fatalf("AbortChat: %v", err)
	} else if !aborted {
		t.Fatal("AbortChat reported no abort for a payload that says aborted=true")
	}
	var got map[string]any
	if err := json.Unmarshal((*calls)[0], &got); err != nil {
		t.Fatalf("decode params: %v", err)
	}
	if got["runId"] != "run-7" {
		t.Fatalf("runId = %v, want run-7", got["runId"])
	}
}

// TestAbortChatReportsAnAbortThatStoppedNothing: the gateway answers a
// *success* with {ok:true, aborted:false, runIds:[]} when the run id matched no
// abortable run. Discarding that flag is what lets a caller treat a stop that
// never happened as done, settle the session's HITL records for a live run, and
// answer its follow-up send with a 200 -- so it has to reach the caller.
func TestAbortChatReportsAnAbortThatStoppedNothing(t *testing.T) {
	c, _ := newTestClient(t, func(method string, _ json.RawMessage) (json.RawMessage, error) {
		if method != "chat.abort" {
			t.Fatalf("method = %q, want chat.abort", method)
		}
		return json.RawMessage(`{"ok":true,"aborted":false,"runIds":[]}`), nil
	})

	aborted, err := c.AbortChat(context.Background(), "session-a", "run-7")
	if err != nil {
		t.Fatalf("AbortChat: %v", err)
	}
	if aborted {
		t.Fatal("AbortChat reported a stop for a payload that says aborted=false")
	}
}

// The other direction, so the flag is not simply always false: runIds filled in
// with aborted true is the ordinary successful stop.
func TestAbortChatReportsAStop(t *testing.T) {
	c, _ := newTestClient(t, func(method string, _ json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"ok":true,"aborted":true,"runIds":["run-7"]}`), nil
	})

	aborted, err := c.AbortChat(context.Background(), "session-a", "run-7")
	if err != nil {
		t.Fatalf("AbortChat: %v", err)
	}
	if !aborted {
		t.Fatal("AbortChat reported no stop for a payload that says aborted=true")
	}
}

// A payload this client cannot read is a failure, not an aborted=false: "the
// gateway said something unreadable" and "the gateway said it stopped nothing"
// are different answers, and the caller treats the returned error
// conservatively (reconcile before settling) rather than as a stop.
func TestAbortChatRejectsAnUndecodablePayload(t *testing.T) {
	c, _ := newTestClient(t, func(method string, _ json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`not json`), nil
	})

	aborted, err := c.AbortChat(context.Background(), "session-a", "run-7")
	if err == nil {
		t.Fatal("AbortChat accepted an undecodable payload as a result")
	}
	if aborted {
		t.Fatal("AbortChat reported a stop alongside a decode failure")
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
//
// `active` is the second half of the answer and the reason the read is not just
// "the id or empty": a descriptor that carries no runId is a run in flight that
// cannot be named, and a caller that read it as idle would go on to abort
// session-wide (or settle a live session). Each case below pins both halves.
func TestSessionInFlightRunReadsRunID(t *testing.T) {
	for _, tc := range []struct {
		name       string
		body       string
		want       string
		wantActive bool
	}{
		{"absent", `{"kind":"delta","messages":[]}`, "", false},
		{"null", `{"kind":"delta","inFlightRun":null}`, "", false},
		{"present", `{"kind":"delta","inFlightRun":{"runId":"r1","startedAtMs":7}}`, "r1", true},
		// A descriptor without a run id is still a run: the busy read must see it
		// even though there is nothing here to scope an abort to. This is the
		// state that used to be indistinguishable from "absent".
		{"no runId", `{"kind":"delta","inFlightRun":{"status":"running"}}`, "", true},
		{"reset", `{"kind":"reset"}`, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestClient(t, func(method string, _ json.RawMessage) (json.RawMessage, error) {
				if method != "chat.history" {
					t.Fatalf("method = %q, want chat.history", method)
				}
				return json.RawMessage(tc.body), nil
			})
			got, active, err := c.SessionInFlightRun(context.Background(), "session-a")
			if err != nil {
				t.Fatalf("SessionInFlightRun: %v", err)
			}
			if got != tc.want || active != tc.wantActive {
				t.Fatalf("SessionInFlightRun = (%q, %v), want (%q, %v)", got, active, tc.want, tc.wantActive)
			}
		})
	}
}

// TestSessionInFlightRunPropagatesCallError: an unanswered read leaves the
// caller with no run id, and it must be able to tell that apart from a read that
// answered "nothing in flight" -- the first is a Stop that cannot be scoped
// (a failure), the second is the idempotent no-op case. Reporting the failure
// as active=false would collapse them the other way, so both halves are pinned.
func TestSessionInFlightRunPropagatesCallError(t *testing.T) {
	wantErr := errors.New("ws: not connected")
	c, _ := newTestClient(t, func(method string, _ json.RawMessage) (json.RawMessage, error) {
		return nil, wantErr
	})
	id, active, err := c.SessionInFlightRun(context.Background(), "session-a")
	if !errors.Is(err, wantErr) {
		t.Fatalf("SessionInFlightRun error = %v, want %v", err, wantErr)
	}
	if id != "" {
		t.Fatalf("SessionInFlightRun = %q alongside an error, want empty", id)
	}
	if active {
		t.Fatal("SessionInFlightRun reported a run in flight alongside an error: a failed read determines nothing")
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
