package store

import (
	"encoding/json"
	"testing"
	"time"
)

func TestAppendAndListMessages(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Append a user message and an assistant delta.
	um, err := s.AppendMessage(Message{ConversationID: "conv-1", User: "u1", Role: "user", Content: "hello"})
	if err != nil {
		t.Fatalf("AppendMessage user: %v", err)
	}
	if um.ID == "" || um.CreatedAt.IsZero() {
		t.Fatalf("message missing id/timestamp: %+v", um)
	}
	am, err := s.AppendMessage(Message{ConversationID: "conv-1", User: "u1", Role: "assistant", EventType: "message_delta", Content: "hello!"})
	if err != nil {
		t.Fatalf("AppendMessage assistant: %v", err)
	}
	// Another conversation must not leak in.
	if _, err := s.AppendMessage(Message{ConversationID: "conv-2", User: "u1", Role: "user", Content: "other"}); err != nil {
		t.Fatalf("AppendMessage conv-2: %v", err)
	}

	msgs, err := s.ListMessages("conv-1", 0)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages for conv-1, got %d", len(msgs))
	}
	if msgs[0].Role != "user" || msgs[1].Role != "assistant" {
		t.Fatalf("order wrong: %+v", msgs)
	}
	if msgs[1].EventType != "message_delta" || msgs[1].Content != "hello!" {
		t.Fatalf("assistant row wrong: %+v", msgs[1])
	}
	_ = am
}

func TestTurnEndMarksAssistantRow(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir, "")

	_, _ = s.AppendMessage(Message{ConversationID: "c", Role: "user", Content: "q"})
	_, _ = s.AppendMessage(Message{ConversationID: "c", Role: "assistant", EventType: "message_delta", Content: "a"})

	if err := s.TurnEnd("c", ""); err != nil {
		t.Fatalf("TurnEnd: %v", err)
	}
	msgs, _ := s.ListMessages("c", 0)
	if msgs[1].EventType != "message_done" {
		t.Fatalf("expected terminal event_type message_done, got %q", msgs[1].EventType)
	}
	if msgs[1].Incomplete {
		t.Fatalf("expected complete turn")
	}

	// A failed turn flips Incomplete.
	_, _ = s.AppendMessage(Message{ConversationID: "c", Role: "user", Content: "q2"})
	_, _ = s.AppendMessage(Message{ConversationID: "c", Role: "assistant", EventType: "message_delta", Content: "partial"})
	if err := s.TurnEnd("c", "gateway timeout"); err != nil {
		t.Fatalf("TurnEnd err: %v", err)
	}
	msgs, _ = s.ListMessages("c", 0)
	last := msgs[len(msgs)-1]
	if !last.Incomplete || last.Error != "gateway timeout" {
		t.Fatalf("expected incomplete+error, got %+v", last)
	}
}

func TestGCExpiredMessages(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir, "")

	_, _ = s.AppendMessage(Message{ConversationID: "c", Role: "user", Content: "fresh"})
	// Simulate an old row by writing directly with a stale timestamp.
	s.mu.Lock()
	var msgs []Message
	_ = s.file("messages.json", &msgs, false)
	msgs = append(msgs, Message{ID: "m-old", ConversationID: "c", Role: "user", Content: "old", CreatedAt: time.Now().Add(-100 * time.Hour)})
	_ = s.save("messages.json", msgs)
	s.mu.Unlock()

	removed, err := s.GCExpiredMessages(72 * time.Hour)
	if err != nil {
		t.Fatalf("GCExpiredMessages: %v", err)
	}
	if removed != 1 {
		t.Fatalf("want 1 removed, got %d", removed)
	}
	msgs, _ = s.ListMessages("c", 0)
	if len(msgs) != 1 || msgs[0].Content != "fresh" {
		t.Fatalf("fresh row should remain, got %+v", msgs)
	}
}

func TestMessageToolCallsJSON(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir, "")
	// ToolCalls stores the raw arguments JSON (the same payload that flows on
	// the SSE stream), not a doubly-encoded string.
	_, _ = s.AppendMessage(Message{ConversationID: "c", Role: "tool", EventType: "tool_call", ToolName: "exec", CallID: "call-1", ToolCalls: json.RawMessage(`{"command":"kubectl get nodes"}`)})

	msgs, _ := s.ListMessages("c", 0)
	if len(msgs) != 1 || msgs[0].ToolName != "exec" || msgs[0].CallID != "call-1" {
		t.Fatalf("tool row wrong: %+v", msgs)
	}
	var got map[string]string
	if err := json.Unmarshal(msgs[0].ToolCalls, &got); err != nil {
		t.Fatalf("toolCalls should be valid JSON, got %s: %v", msgs[0].ToolCalls, err)
	}
	if got["command"] != "kubectl get nodes" {
		t.Fatalf("command field wrong: %+v", got)
	}
}
