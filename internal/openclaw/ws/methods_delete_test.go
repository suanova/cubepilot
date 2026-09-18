package ws

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// sessions.delete sends deleteTranscript explicitly. The gateway defaults it to
// true, so omitting it would work today and would silently become a different
// (transcript-preserving) call the day that default changes -- on the one call
// here whose whole point is destroying data.
func TestDeleteSessionSendsKeyAndDeleteTranscript(t *testing.T) {
	c, calls := newTestClient(t, func(method string, _ json.RawMessage) (json.RawMessage, error) {
		if method != "sessions.delete" {
			t.Fatalf("method = %q, want sessions.delete", method)
		}
		return json.RawMessage(`{"ok":true,"key":"session-a","deleted":true,"archived":[]}`), nil
	})

	res, err := c.DeleteSession(context.Background(), "session-a")
	if err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if !res.Deleted {
		t.Fatal("deleted = false for a payload that says deleted=true")
	}
	if len(*calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(*calls))
	}
	var got map[string]any
	if err := json.Unmarshal((*calls)[0], &got); err != nil {
		t.Fatalf("decode params: %v", err)
	}
	if got["key"] != "session-a" {
		t.Fatalf("key = %v, want session-a", got["key"])
	}
	if got["deleteTranscript"] != true {
		t.Fatalf("deleteTranscript = %v, want an explicit true", got["deleteTranscript"])
	}
}

// The result's other two fields are the honest half of the answer: what the
// runtime kept behind. Decoding them away would let the endpoint promise a
// clean instance it cannot deliver, which is the caveat the API documents.
func TestDeleteSessionDecodesWhatSurvived(t *testing.T) {
	c, _ := newTestClient(t, func(method string, _ json.RawMessage) (json.RawMessage, error) {
		if method != "sessions.delete" {
			t.Fatalf("method = %q, want sessions.delete", method)
		}
		return json.RawMessage(`{"ok":true,"key":"session-a","deleted":true,` +
			`"archived":["sessions/session-a.jsonl"],` +
			`"worktreePreserved":{"id":"wt-1","branch":"session-a","path":"/work/wt-1","reason":"uncommitted changes"}}`), nil
	})

	res, err := c.DeleteSession(context.Background(), "session-a")
	if err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if len(res.Archived) != 1 || res.Archived[0] != "sessions/session-a.jsonl" {
		t.Fatalf("archived = %v, want the one archived item", res.Archived)
	}
	if res.WorktreePreserved == nil {
		t.Fatal("worktreePreserved = nil, want the preserved worktree")
	}
	if *res.WorktreePreserved != (PreservedSessionWorktree{
		ID: "wt-1", Branch: "session-a", Path: "/work/wt-1", Reason: "uncommitted changes",
	}) {
		t.Fatalf("worktreePreserved = %+v", *res.WorktreePreserved)
	}
}

// A key that is not there is a success, not a failure: this is where the
// endpoint's idempotence comes from, and a client that clears the same fixed key
// every time depends on it. A nil WorktreePreserved is the ordinary case.
func TestDeleteSessionReportsANothingToDelete(t *testing.T) {
	c, _ := newTestClient(t, func(method string, _ json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"ok":true,"key":"session-a","deleted":false,"archived":[]}`), nil
	})

	res, err := c.DeleteSession(context.Background(), "session-a")
	if err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if res.Deleted {
		t.Fatal("deleted = true for a payload that says deleted=false")
	}
	if res.WorktreePreserved != nil {
		t.Fatalf("worktreePreserved = %+v, want absent", *res.WorktreePreserved)
	}
}

// The method and the key are both in the error, so a failed delete is
// diagnosable from the message alone -- and the failure still unwraps to the
// caller's error for errors.Is / CodeOf / ReasonOf.
func TestDeleteSessionWrapsCallError(t *testing.T) {
	wantErr := errors.New("ws: not connected")
	c, _ := newTestClient(t, func(method string, _ json.RawMessage) (json.RawMessage, error) {
		return nil, wantErr
	})

	res, err := c.DeleteSession(context.Background(), "session-a")
	if !errors.Is(err, wantErr) {
		t.Fatalf("DeleteSession error = %v, want it to wrap %v", err, wantErr)
	}
	if !strings.Contains(err.Error(), `sessions.delete "session-a"`) {
		t.Fatalf("error = %q, want it to name the method and the key", err)
	}
	if res.Deleted {
		t.Fatal("a failed delete reported deleted=true")
	}
}

// A payload this client cannot read is a failure, never a deletion: "the
// gateway said something unreadable" and "the session is gone" are different
// answers, and only one of them lets a client show an empty conversation.
func TestDeleteSessionRejectsAnUndecodablePayload(t *testing.T) {
	c, _ := newTestClient(t, func(method string, _ json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`not json`), nil
	})

	if _, err := c.DeleteSession(context.Background(), "session-a"); err == nil {
		t.Fatal("DeleteSession accepted an undecodable payload as a result")
	} else if !strings.Contains(err.Error(), "decode sessions.delete") {
		t.Fatalf("error = %q, want a decode failure", err)
	}
}

// The two refusals the endpoint has to tell apart arrive as different wire
// fields: a still-active session is an UNAVAILABLE with a retryable detail and
// no reason at all, and one that changed under the call is an INVALID_REQUEST
// whose reason is the only thing that identifies it. The frames below are the
// gateway's own shapes (errorShape in packages/gateway-protocol); pinning both
// through the real decoder is what lets the HTTP layer classify them without
// parsing message text.
func TestDeleteSessionSurfacesCodeAndReason(t *testing.T) {
	cases := []struct {
		name       string
		code       string
		message    string
		details    string
		wantCode   string
		wantReason string
	}{
		{
			name:     "still active",
			code:     "UNAVAILABLE",
			message:  "Session session-a is still active; try again.",
			details:  `{"retryable":true}`,
			wantCode: "UNAVAILABLE",
		},
		{
			name:       "lifecycle changed",
			code:       "INVALID_REQUEST",
			message:    "Session session-a changed before deletion. Retry.",
			details:    `{"reason":"session-changed"}`,
			wantCode:   "INVALID_REQUEST",
			wantReason: "session-changed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestClient(t, func(method string, _ json.RawMessage) (json.RawMessage, error) {
				return nil, frameErrorOf(responseFrame{
					Type: "res", ID: "r1", OK: false,
					Error: &frameError{Code: tc.code, Message: tc.message, Details: json.RawMessage(tc.details)},
				})
			})

			if _, err := c.DeleteSession(context.Background(), "session-a"); err == nil {
				t.Fatal("DeleteSession accepted a failure frame as a result")
			} else {
				if got := CodeOf(err); got != tc.wantCode {
					t.Errorf("CodeOf = %q, want %q (err = %v)", got, tc.wantCode, err)
				}
				if got := ReasonOf(err); got != tc.wantReason {
					t.Errorf("ReasonOf = %q, want %q (err = %v)", got, tc.wantReason, err)
				}
			}
		})
	}
}
