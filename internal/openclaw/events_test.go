package openclaw

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStreamChat_MapsOpenAISSEToCubePilotEvents(t *testing.T) {
	// A canned OpenAI-compatible stream: assistant role -> tool call (exec) ->
	// content delta -> [DONE]. This is the shape OpenClaw's gateway emits when the
	// agent chooses to call a tool and then produces its final answer.
	sse := "" +
		"data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"exec\",\"arguments\":\"kubectl get\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\" pods -A\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"Found 2 abnormal Pods.\"}}]}\n\n" +
		"data: [DONE]\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("x-openclaw-session-key") != "conv-abc" {
			t.Errorf("session key header not forwarded")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	c := New(srv.URL, "secret")
	var got []Event
	err := c.StreamChat(t.Context(), ChatParams{
		SessionKey: "conv-abc",
		Messages:   []ChatMessage{{Role: "user", Content: "check abnormal pods"}},
	}, func(e Event) error {
		got = append(got, e)
		return nil
	})
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}

	types := eventTypes(got)
	want := []string{
		EventToolCall,
		EventMessageDelta,
		EventMessageDone,
	}
	if len(types) != len(want) {
		t.Fatalf("event sequence = %v, want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("event[%d] = %q, want %q (full: %v)", i, types[i], want[i], types)
		}
	}

	// The tool call must carry the accumulated arguments across fragments.
	var toolCall *Event
	for i := range got {
		if got[i].Type == EventToolCall {
			toolCall = &got[i]
		}
	}
	if toolCall == nil {
		t.Fatal("missing tool_call event")
	}
	if toolCall.Name != "exec" {
		t.Errorf("tool name = %q, want exec", toolCall.Name)
	}
	if toolCall.Arguments != "kubectl get pods -A" {
		t.Errorf("tool arguments = %q, want accumulated fragments", toolCall.Arguments)
	}
}

func TestStreamChat_SurfacesStreamedError(t *testing.T) {
	// The gateway streams an agent-run failure as an OpenAI-compatible error
	// line before [DONE]. The client must surface it instead of finishing the
	// turn with no content (which showed only "done" in the portal).
	sse := "" +
		"data: {\"error\":{\"message\":\"The agent run failed before producing a reply.\",\"type\":\"api_error\"}}\n\n" +
		"data: [DONE]\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	c := New(srv.URL, "secret")
	var got []Event
	err := c.StreamChat(t.Context(), ChatParams{
		SessionKey: "conv-abc",
		Messages:   []ChatMessage{{Role: "user", Content: "hi"}},
	}, func(e Event) error {
		got = append(got, e)
		return nil
	})
	if err == nil {
		t.Fatal("expected an error for a streamed agent failure, got nil")
	}
	if !strings.Contains(err.Error(), "The agent run failed before producing a reply.") {
		t.Errorf("error = %q, want the streamed message", err.Error())
	}
	// The terminal event must carry the error so the portal can render it.
	if len(got) == 0 || got[len(got)-1].Type != EventMessageDone || got[len(got)-1].Error == "" {
		t.Errorf("expected message_done carrying the error, got %+v", got)
	}
}

func eventTypes(evs []Event) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Type)
	}
	return out
}

// TestConfirmEventMarshal pins the confirm_pending / confirm_resolved SSE JSON
// to the documented contract (docs/cubepilot/api.md lines 436-437): call_id is
// the gateway approval id, tool is the exec tool, command/level/message carry
// the write, approved the decision.
func TestConfirmEventMarshal(t *testing.T) {
	pending := Event{
		Type:      EventConfirmPending,
		SessionID: "conv-abc",
		CallID:    "appr-1",
		Name:      "exec",
		Command:   "kubectl delete pod foo",
		Level:     "write",
		Message:   "approve or reject the delete",
	}
	got := string(pending.Marshal())
	for _, want := range []string{
		`"type":"confirm_pending"`,
		`"session_id":"conv-abc"`,
		`"call_id":"appr-1"`,
		`"name":"exec"`,
		`"command":"kubectl delete pod foo"`,
		`"level":"write"`,
		`"message":"approve or reject the delete"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("pending payload missing %s: %s", want, got)
		}
	}

	resolved := Event{
		Type:      EventConfirmResolved,
		SessionID: "conv-abc",
		CallID:    "appr-1",
		Approved:  boolPtr(true),
	}
	got = string(resolved.Marshal())
	for _, want := range []string{
		`"type":"confirm_resolved"`,
		`"session_id":"conv-abc"`,
		`"call_id":"appr-1"`,
		`"approved":true`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("resolved payload missing %s: %s", want, got)
		}
	}

	// A reject must serialize an explicit approved:false (not be omitted).
	rejected := Event{
		Type:      EventConfirmResolved,
		SessionID: "conv-abc",
		CallID:    "appr-2",
		Approved:  boolPtr(false),
	}
	if got := string(rejected.Marshal()); !strings.Contains(got, `"approved":false`) {
		t.Errorf("rejected payload must carry approved:false, got %s", got)
	}
	// Non-confirm events must not carry the approved key.
	delta := Event{Type: EventMessageDelta, SessionID: "conv-abc", Delta: "hi"}
	if got := string(delta.Marshal()); strings.Contains(got, `"approved"`) {
		t.Errorf("message_delta must not carry approved, got %s", got)
	}
}

func boolPtr(v bool) *bool { return &v }
