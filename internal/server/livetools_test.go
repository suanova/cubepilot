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

func TestLiveProjector_IgnoresUnrelatedAndMalformed(t *testing.T) {
	p := newLiveProjector()
	if got, term := p.feed(conv, "session.message", []byte(`{"role":"user"}`)); len(got) != 0 || term {
		t.Fatalf("session.message must be ignored")
	}
	if got, _ := p.feed(conv, "agent", []byte(`not json`)); len(got) != 0 {
		t.Fatalf("malformed payload must be ignored, got %+v", got)
	}
}

// TestLiveProjector_IgnoresTailsOfCallsStartedBeforeAttach: an observer that
// joins a run already in flight (issue #167 re-attach) sees results for calls
// whose start it never saw. Those have no card in this projection, and emitting
// them would put a phantom entry in front of the browser.
func TestLiveProjector_IgnoresTailsOfCallsStartedBeforeAttach(t *testing.T) {
	p := newLiveProjector()
	feed := func(payload string) []openclaw.Event {
		got, _ := p.feed(conv, "agent", []byte(payload))
		return got
	}
	if got := feed(`{"sessionKey":"` + conv + `","stream":"tool","data":{"phase":"result","result":{"text":"done"},"toolCallId":"call_before"}}`); len(got) != 0 {
		t.Fatalf("result for an unseen call must not emit, got %+v", got)
	}
	if got := feed(`{"sessionKey":"` + conv + `","stream":"command_output","data":{"phase":"end","output":"already ran","toolCallId":"exec_before"}}`); len(got) != 0 {
		t.Fatalf("command output for an unseen call must not emit, got %+v", got)
	}
	// A call this projection did see start still reports normally.
	feed(`{"sessionKey":"` + conv + `","stream":"tool","data":{"phase":"start","name":"read","toolCallId":"call_after"}}`)
	if got := feed(`{"sessionKey":"` + conv + `","stream":"tool","data":{"phase":"result","result":{"text":"seen"},"toolCallId":"call_after"}}`); len(got) != 1 || got[0].CallID != "call_after" {
		t.Fatalf("result for a seen call = %+v, want it emitted", got)
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

// preamble builds the item frame the gateway publishes the agent's between-tool
// narration as: an item of kind "preamble" whose progressText is the whole step,
// folded onto one line by the gateway. This is the surface the Control UI reads,
// and the one the deployed gateway was observed to send (the assistant stream
// carries the answer alone).
func preamble(text string) string {
	return `{"sessionKey":"` + conv + `","stream":"item","data":{"kind":"preamble","title":"Preamble","phase":"update","progressText":"` + text + `"}}`
}

func TestLiveProjector_Narration(t *testing.T) {
	p := newLiveProjector()
	got, _ := p.feed(conv, "agent", []byte(preamble("先看看 default 有哪些 Pod。")))
	if len(got) != 1 || got[0].Type != openclaw.EventNarration {
		t.Fatalf("preamble = %+v, want one narration event", got)
	}
	if got[0].Text != "先看看 default 有哪些 Pod。" {
		t.Fatalf("text = %q, want the preamble text", got[0].Text)
	}
	if got[0].BlockID == "" {
		t.Fatal("block id is empty; a block that cannot be identified cannot be replaced")
	}
}

func TestLiveProjector_NarrationReplacesWithinOneBlock(t *testing.T) {
	p := newLiveProjector()
	first, _ := p.feed(conv, "agent", []byte(preamble("先看看")))
	// The gateway re-emits a step as it grows, so the same block arrives again.
	second, _ := p.feed(conv, "agent", []byte(preamble("先看看 default 有哪些 Pod。")))
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("expected one event per update, got %d then %d", len(first), len(second))
	}
	if first[0].BlockID != second[0].BlockID {
		t.Fatalf("block id changed within one block: %q -> %q", first[0].BlockID, second[0].BlockID)
	}
	if second[0].Text != "先看看 default 有哪些 Pod。" {
		t.Fatalf("text = %q, want the newer text", second[0].Text)
	}
}

func TestLiveProjector_NarrationAdvancesOnToolStart(t *testing.T) {
	p := newLiveProjector()
	before, _ := p.feed(conv, "agent", []byte(preamble("先查 Pod")))
	p.feed(conv, "agent", []byte(`{"sessionKey":"`+conv+`","stream":"tool","data":{"phase":"start","name":"exec","toolCallId":"call_1"}}`))
	after, _ := p.feed(conv, "agent", []byte(preamble("再查事件")))
	if len(before) != 1 || len(after) != 1 {
		t.Fatalf("expected one narration each side of the tool, got %d then %d", len(before), len(after))
	}
	if before[0].BlockID == after[0].BlockID {
		t.Fatalf("both narrations share block id %q; the tool call between them is a block boundary", before[0].BlockID)
	}
}

func TestLiveProjector_NarrationAdvancesOnSuppressedToolStart(t *testing.T) {
	p := newLiveProjector()
	before, _ := p.feed(conv, "agent", []byte(preamble("我需要确认一下")))
	// ask_user's card is suppressed, so nothing is emitted for it -- and a block
	// id that advanced only on emitted events would merge the two narrations into
	// one paragraph across the question.
	if got, _ := p.feed(conv, "agent", []byte(`{"sessionKey":"`+conv+`","stream":"tool","data":{"phase":"start","name":"ask_user","toolCallId":"call_q"}}`)); len(got) != 0 {
		t.Fatalf("ask_user start = %+v, want no card", got)
	}
	after, _ := p.feed(conv, "agent", []byte(preamble("好，那我继续")))
	if len(before) != 1 || len(after) != 1 {
		t.Fatalf("expected one narration each side of the question, got %d then %d", len(before), len(after))
	}
	if before[0].BlockID == after[0].BlockID {
		t.Fatalf("both narrations share block id %q; the question between them is a block boundary", before[0].BlockID)
	}
}

func TestLiveProjector_NarrationPrefersTheGatewayItemID(t *testing.T) {
	p := newLiveProjector()
	// A lane that names its items (the Responses API) gives a block identity the
	// gateway already reconciled against, and two commentary items before one
	// tool call must not collapse into a single block.
	first, _ := p.feed(conv, "agent", []byte(`{"sessionKey":"`+conv+`","stream":"item","data":{"kind":"preamble","phase":"update","itemId":"commentary-0","progressText":"先看 a"}}`))
	second, _ := p.feed(conv, "agent", []byte(`{"sessionKey":"`+conv+`","stream":"item","data":{"kind":"preamble","phase":"update","itemId":"commentary-1","progressText":"再看 b"}}`))
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("expected one event per item, got %d then %d", len(first), len(second))
	}
	if first[0].BlockID != "commentary-0" || second[0].BlockID != "commentary-1" {
		t.Fatalf("block ids = %q, %q; want the gateway's own ids", first[0].BlockID, second[0].BlockID)
	}
}

func TestLiveProjector_NarrationHidesChannelDirectives(t *testing.T) {
	p := newLiveProjector()
	// A line that addresses a channel rather than a reader, and the token that
	// means "say nothing here", are not narration a Portal should show.
	got, _ := p.feed(conv, "agent", []byte(preamble("[[reply_to_current]] 先看 default 的 Pod")))
	if len(got) != 1 {
		t.Fatalf("events = %+v, want one narration", got)
	}
	if got[0].Text != "先看 default 的 Pod" {
		t.Fatalf("text = %q, want the directive stripped", got[0].Text)
	}
	if got, _ := p.feed(conv, "agent", []byte(preamble("NO_REPLY"))); len(got) != 0 {
		t.Fatalf("silent-reply token = %+v, want nothing drawn", got)
	}
}

func TestLiveProjector_NarrationDropsBlankText(t *testing.T) {
	p := newLiveProjector()
	// A step that narrated nothing must not draw an empty line between two cards.
	got, _ := p.feed(conv, "agent", []byte(preamble("   ")))
	if len(got) != 0 {
		t.Fatalf("blank preamble = %+v, want no narration", got)
	}
}

func TestLiveProjector_NarrationIgnoresOtherFrames(t *testing.T) {
	p := newLiveProjector()
	cases := map[string]string{
		// The answer rides the assistant stream, and the chat stream carries it
		// too; projecting it here as well would print it twice.
		"assistant answer": `{"sessionKey":"` + conv + `","stream":"assistant","data":{"text":"结论是这样的","delta":"结论是这样的"}}`,
		// Tool and command items are cards, not narration.
		"tool item":    `{"sessionKey":"` + conv + `","stream":"item","data":{"kind":"tool","phase":"start","name":"exec","toolCallId":"c9"}}`,
		"command item": `{"sessionKey":"` + conv + `","stream":"item","data":{"kind":"command","phase":"update","toolCallId":"c9"}}`,
	}
	for name, payload := range cases {
		got, _ := p.feed(conv, "agent", []byte(payload))
		for _, ev := range got {
			if ev.Type == openclaw.EventNarration {
				t.Fatalf("%s produced a narration: %+v", name, ev)
			}
		}
	}
}
