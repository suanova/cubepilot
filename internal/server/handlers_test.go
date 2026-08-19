package server

import (
	"encoding/json"
	"testing"

	"github.com/suanova/cubepilot/internal/openclaw"
)

// buildHistoryItem builds one gateway transcript item.
func buildHistoryItem(role string, content []map[string]any) map[string]any {
	return map[string]any{"role": role, "content": content}
}

func toolCallContent(id, name string, args map[string]any) map[string]any {
	raw, _ := json.Marshal(args)
	return map[string]any{"type": "toolCall", "id": id, "name": name, "arguments": json.RawMessage(raw)}
}

func textContent(s string) map[string]any {
	return map[string]any{"type": "text", "text": s}
}

func TestParseHistoryToolsPairsCallsWithResults(t *testing.T) {
	// Mirrors the real gateway transcript shape: an assistant item carries N
	// toolCall blocks, and each toolResult item carries a single text block.
	history := map[string]any{"items": []any{
		buildHistoryItem("user", []map[string]any{textContent("检查环境")}),
		buildHistoryItem("assistant", []map[string]any{
			toolCallContent("call_1", "read", map[string]any{"path": "/skills/a"}),
			toolCallContent("call_2", "exec", map[string]any{"command": "kubectl get nodes"}),
		}),
		buildHistoryItem("toolResult", []map[string]any{textContent("SKILL content")}),
		buildHistoryItem("toolResult", []map[string]any{textContent("node list")}),
		buildHistoryItem("assistant", []map[string]any{toolCallContent("call_3", "exec", map[string]any{"command": "kubectl top"})}),
		buildHistoryItem("toolResult", []map[string]any{textContent("metrics error")}),
		buildHistoryItem("assistant", []map[string]any{textContent("## 报告")}),
	}}
	raw, _ := json.Marshal(history)

	evs := parseHistoryTools("sess-1", raw)

	// Expect: 3 tool_call + 3 tool_result, grouped in transcript order.
	if len(evs) != 6 {
		t.Fatalf("expected 6 events, got %d: %+v", len(evs), evs)
	}

	// tool_call(read) then tool_call(exec) — parallel batch of two.
	if evs[0].Type != openclaw.EventToolCall || evs[0].Name != "read" || evs[0].CallID != "call_1" {
		t.Errorf("evs[0] = %+v, want tool_call read/call_1", evs[0])
	}
	if evs[1].Type != openclaw.EventToolCall || evs[1].Name != "exec" || evs[1].CallID != "call_2" {
		t.Errorf("evs[1] = %+v, want tool_call exec/call_2", evs[1])
	}
	// tool_result pairs back to call_1 (real name read, not exec).
	if evs[2].Type != openclaw.EventToolResult || evs[2].Name != "read" || evs[2].CallID != "call_1" {
		t.Errorf("evs[2] = %+v, want tool_result read/call_1", evs[2])
	}
	if evs[2].Output != "SKILL content" {
		t.Errorf("evs[2].Output = %q, want SKILL content", evs[2].Output)
	}
	// Second result pairs back to call_2.
	if evs[3].Type != openclaw.EventToolResult || evs[3].Name != "exec" || evs[3].CallID != "call_2" || evs[3].Output != "node list" {
		t.Errorf("evs[3] = %+v, want tool_result exec/call_2 node list", evs[3])
	}
	// Third pair: exec / call_3 → metrics error.
	if evs[4].Type != openclaw.EventToolCall || evs[4].CallID != "call_3" {
		t.Errorf("evs[4] = %+v, want tool_call exec/call_3", evs[4])
	}
	if evs[5].Type != openclaw.EventToolResult || evs[5].CallID != "call_3" || evs[5].Output != "metrics error" {
		t.Errorf("evs[5] = %+v, want tool_result exec/call_3 metrics error", evs[5])
	}
}

func TestParseHistoryToolsWithoutResults(t *testing.T) {
	// A transcript with a toolCall but no toolResult yet (poll during the run).
	history := map[string]any{"items": []any{
		buildHistoryItem("assistant", []map[string]any{
			toolCallContent("call_1", "exec", map[string]any{"command": "kubectl get pods"}),
		}),
	}}
	raw, _ := json.Marshal(history)

	evs := parseHistoryTools("sess-1", raw)

	if len(evs) != 1 {
		t.Fatalf("expected 1 tool_call, got %d: %+v", len(evs), evs)
	}
	if evs[0].Type != openclaw.EventToolCall || evs[0].Name != "exec" {
		t.Errorf("evs[0] = %+v, want tool_call exec", evs[0])
	}
}

func TestParseHistoryToolsMalformed(t *testing.T) {
	if evs := parseHistoryTools("sess-1", []byte("not json")); evs != nil {
		t.Errorf("expected nil on malformed input, got %+v", evs)
	}
}
