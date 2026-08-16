package openclaw

import (
	"net/http"
	"net/http/httptest"
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
		"data: {\"choices\":[{\"delta\":{\"content\":\"共发现 2 个异常 Pod。\"}}]}\n\n" +
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
		Messages:   []ChatMessage{{Role: "user", Content: "查一下异常 Pod"}},
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

func eventTypes(evs []Event) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Type)
	}
	return out
}
