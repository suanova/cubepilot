package server

import (
	"testing"

	"github.com/suanova/cubepilot/internal/openclaw"
)

const conv = "agent:main:conv-1"

func TestLiveProjector_ToolStream(t *testing.T) {
	p := newLiveProjector()
	var evs []openclaw.Event
	feed := func(payload string) {
		got, _ := p.feed(conv, "agent", []byte(payload))
		evs = append(evs, got...)
	}

	// Canonical stream="tool": start carries the tool call.
	feed(`{"sessionKey":"` + conv + `","stream":"tool","data":{"phase":"start","name":"exec","title":"exec kubectl get pods","args":{"command":"kubectl get pods -n default"},"toolCallId":"call_1"}}`)
	if len(evs) != 1 || evs[0].Type != openclaw.EventToolCall || evs[0].CallID != "call_1" {
		t.Fatalf("tool start = %+v", evs)
	}
	if evs[0].Arguments != `{"command":"kubectl get pods -n default"}` {
		t.Fatalf("arguments = %q, want the JSON args", evs[0].Arguments)
	}

	// update phases carry no SSE event.
	feed(`{"sessionKey":"` + conv + `","stream":"tool","data":{"phase":"update","partialResult":{},"toolCallId":"call_1"}}`)
	if len(evs) != 1 {
		t.Fatalf("update must not emit, got %d", len(evs))
	}

	// A production-order run with no command_output: the stream="tool" result is
	// deferred and emitted when the run goes terminal (chat final), never
	// pre-emptively.
	feed(`{"sessionKey":"` + conv + `","stream":"tool","data":{"phase":"result","result":{"output":"No resources found"},"toolCallId":"call_1"}}`)
	feed(`{"sessionKey":"` + conv + `","stream":"item","data":{"kind":"tool","phase":"end","name":"exec","title":"exec kubectl get pods","toolCallId":"call_1"}}`)
	if len(evs) != 1 {
		t.Fatalf("result must be deferred until terminal, got %d events", len(evs))
	}
	chatFinal := func(payload string) {
		got, _ := p.feed(conv, "chat", []byte(payload))
		evs = append(evs, got...)
	}
	chatFinal(`{"sessionKey":"` + conv + `","runId":"r1","state":"final"}`)
	if len(evs) != 2 || evs[1].Type != openclaw.EventToolResult || evs[1].Output != "No resources found" {
		t.Fatalf("after terminal = %+v, want one deferred tool_result", evs)
	}
	// A duplicate result event after finalize must not double-emit.
	if got, _ := p.feed(conv, "agent", []byte(`{"sessionKey":"`+conv+`","stream":"tool","data":{"phase":"result","result":{"output":"x"},"toolCallId":"call_1"}}`)); len(got) != 0 {
		t.Fatalf("duplicate result emitted: %+v", got)
	}
}

func TestLiveProjector_CommandOutputWinsInProductionOrder(t *testing.T) {
	p := newLiveProjector()
	var evs []openclaw.Event
	feed := func(payload string) {
		got, _ := p.feed(conv, "agent", []byte(payload))
		evs = append(evs, got...)
	}
	feed(`{"sessionKey":"` + conv + `","stream":"tool","data":{"phase":"start","name":"exec","title":"exec kubectl","toolCallId":"c2"}}`)
	// Production order: stream tool result, then item end, then the terminal
	// command_output -- the last must win as the real output.
	feed(`{"sessionKey":"` + conv + `","stream":"tool","data":{"phase":"result","result":{"output":"stale"},"toolCallId":"c2"}}`)
	feed(`{"sessionKey":"` + conv + `","stream":"item","data":{"kind":"tool","phase":"end","name":"exec","title":"exec kubectl","toolCallId":"c2"}}`)
	feed(`{"sessionKey":"` + conv + `","stream":"command_output","data":{"phase":"end","toolCallId":"c2","output":"real output","exitCode":0}}`)
	if len(evs) != 2 || evs[1].Type != openclaw.EventToolResult || evs[1].Output != "real output" {
		t.Fatalf("events = %+v, want command_output to win", evs)
	}
}

func TestLiveProjector_ItemOnlyToolFinalizesAtTerminal(t *testing.T) {
	p := newLiveProjector()
	var evs []openclaw.Event
	feed := func(payload string) {
		got, _ := p.feed(conv, "agent", []byte(payload))
		evs = append(evs, got...)
	}
	feed(`{"sessionKey":"` + conv + `","stream":"item","data":{"kind":"tool","phase":"start","name":"read","title":"read file","meta":"read /tmp/x","toolCallId":"c3"}}`)
	feed(`{"sessionKey":"` + conv + `","stream":"item","data":{"kind":"tool","phase":"end","name":"read","title":"read file","toolCallId":"c3"}}`)
	if len(evs) != 1 {
		t.Fatalf("item-only tool must not emit before terminal, got %d", len(evs))
	}
	chatFinal := func(payload string) {
		got, _ := p.feed(conv, "chat", []byte(payload))
		evs = append(evs, got...)
	}
	chatFinal(`{"sessionKey":"` + conv + `","runId":"r1","state":"final"}`)
	if len(evs) != 2 || evs[1].Type != openclaw.EventToolResult || evs[1].CallID != "c3" || evs[1].Name != "read" {
		t.Fatalf("events = %+v, want tool_result at terminal", evs)
	}
}

func TestLiveProjector_ChatText(t *testing.T) {
	p := newLiveProjector()
	var evs []openclaw.Event
	feed := func(payload string) {
		got, _ := p.feed(conv, "chat", []byte(payload))
		evs = append(evs, got...)
	}

	feed(`{"sessionKey":"` + conv + `","runId":"r1","state":"delta","deltaText":"hello "}`)
	feed(`{"sessionKey":"` + conv + `","runId":"r1","state":"delta","deltaText":"world"}`)
	if len(evs) != 2 || evs[0].Type != openclaw.EventMessageDelta || evs[1].Delta != "world" {
		t.Fatalf("deltas = %+v", evs)
	}
	// final after deltas is a snapshot and must not duplicate.
	feed(`{"sessionKey":"` + conv + `","runId":"r1","state":"final","deltaText":"hello world"}`)
	if len(evs) != 2 {
		t.Fatalf("final duplicated text: %d events", len(evs))
	}

	// A reply that arrives only as final (no deltas) emits the text once.
	p2 := newLiveProjector()
	got, _ := p2.feed(conv, "chat", []byte(`{"sessionKey":"`+conv+`","runId":"r2","state":"final","deltaText":"short answer"}`))
	if len(got) != 1 || got[0].Type != openclaw.EventMessageDelta || got[0].Delta != "short answer" {
		t.Fatalf("final-only = %+v", got)
	}
}

func TestLiveProjector_TerminalByChatState(t *testing.T) {
	for _, state := range []string{"final", "aborted", "error"} {
		p := newLiveProjector()
		_, term := p.feed(conv, "chat", []byte(`{"sessionKey":"`+conv+`","state":"`+state+`"}`))
		if !term {
			t.Fatalf("chat state %q must be terminal", state)
		}
	}
	// Status and lifecycle are NOT terminal (lifecycle end precedes chat final).
	p := newLiveProjector()
	if _, term := p.feed(conv, "chat", []byte(`{"sessionKey":"`+conv+`","state":"status"}`)); term {
		t.Fatal("status must not be terminal")
	}
	if _, term := p.feed(conv, "agent", []byte(`{"sessionKey":"`+conv+`","stream":"lifecycle","data":{"phase":"end"}}`)); term {
		t.Fatal("lifecycle end must not terminate the turn by itself")
	}
}

func TestLiveProjector_ChatStatus(t *testing.T) {
	p := newLiveProjector()

	got, term := p.feed(conv, "chat", []byte(`{"sessionKey":"`+conv+`","state":"status","phase":"preparing_workspace"}`))
	if term || len(got) != 1 || got[0].Type != openclaw.EventAgentStatus || got[0].Status != "preparing" {
		t.Fatalf("workspace status = %+v, terminal = %v", got, term)
	}
	// Multiple OpenClaw workspace phases collapse to one stable CubePilot status.
	got, _ = p.feed(conv, "chat", []byte(`{"sessionKey":"`+conv+`","state":"status","phase":"running_setup"}`))
	if len(got) != 0 {
		t.Fatalf("duplicate normalized status emitted: %+v", got)
	}
	got, _ = p.feed(conv, "chat", []byte(`{"sessionKey":"`+conv+`","state":"status","phase":"preparing_context"}`))
	if len(got) != 1 || got[0].Status != "building_context" {
		t.Fatalf("context status = %+v", got)
	}
	got, _ = p.feed(conv, "chat", []byte(`{"sessionKey":"`+conv+`","state":"status","phase":"future_phase"}`))
	if len(got) != 0 {
		t.Fatalf("unknown status must be ignored, got %+v", got)
	}
}

func TestLiveProjector_IgnoresUnrelatedAndMalformed(t *testing.T) {
	p := newLiveProjector()
	if got, term := p.feed(conv, "session.message", []byte(`{"role":"user"}`)); len(got) != 0 || term {
		t.Fatalf("session.message must be ignored")
	}
	if got, _ := p.feed(conv, "agent", []byte(`not json`)); len(got) != 0 {
		t.Fatalf("malformed payload must be ignored, got %+v", got)
	}
}

func TestLiveProjector_ToolStartDedupAndName(t *testing.T) {
	p := newLiveProjector()
	var evs []openclaw.Event
	feed := func(payload string) {
		got, _ := p.feed(conv, "agent", []byte(payload))
		evs = append(evs, got...)
	}
	// canonical stream="tool" start, then the tracked item start echo.
	feed(`{"sessionKey":"` + conv + `","stream":"tool","data":{"phase":"start","name":"read","title":"read /tmp/x","toolCallId":"c9"}}`)
	feed(`{"sessionKey":"` + conv + `","stream":"item","data":{"kind":"tool","phase":"start","name":"read","title":"read /tmp/x","toolCallId":"c9"}}`)
	if len(evs) != 1 {
		t.Fatalf("duplicate tool start emitted: %d events", len(evs))
	}
	if evs[0].Name != "read" {
		t.Fatalf("name = %q, want read", evs[0].Name)
	}
	// result is deferred to terminal and preserves the tool name, not exec.
	feed(`{"sessionKey":"` + conv + `","stream":"tool","data":{"phase":"result","result":{"content":[{"type":"text","text":"file contents"}]},"toolCallId":"c9"}}`)
	chatFinal := func(payload string) {
		got, _ := p.feed(conv, "chat", []byte(payload))
		evs = append(evs, got...)
	}
	chatFinal(`{"sessionKey":"` + conv + `","runId":"r1","state":"final"}`)
	if len(evs) != 2 || evs[1].Name != "read" || evs[1].Output != "file contents" {
		t.Fatalf("result = %+v, want name=read output='file contents'", evs)
	}
}

func TestLiveProjector_TextReplace(t *testing.T) {
	p := newLiveProjector()
	// A delta, then a replace snapshot that supersedes it.
	got, _ := p.feed(conv, "chat", []byte(`{"sessionKey":"`+conv+`","runId":"r1","state":"delta","deltaText":"Hello world"}`))
	if len(got) != 1 || got[0].Type != openclaw.EventMessageDelta {
		t.Fatalf("delta = %+v", got)
	}
	got, _ = p.feed(conv, "chat", []byte(`{"sessionKey":"`+conv+`","runId":"r1","state":"delta","deltaText":"Hi","replace":true}`))
	if len(got) != 1 || got[0].Type != openclaw.EventTextReplace || got[0].Delta != "Hi" {
		t.Fatalf("replace = %+v, want a text_replace event", got)
	}
}
