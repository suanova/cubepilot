package server

import (
	"encoding/json"
	"strings"

	"github.com/suanova/cubepilot/internal/openclaw"
)

// Live chat projection (issue #130, WS-only chat). Chat turns are driven over
// the gateway-protocol WebSocket: a session's live message stream carries the
// run as one ordered event feed. liveProjector folds those frames into the chat
// SSE's tool_call / tool_result / message_delta events (and flags the run
// terminal) so the existing frontend renders the turn as it happens -- no
// transcript polling, no end-of-run drain.
//
// Content surfaces consumed (verified against OpenClaw v2026.8.2):
//   - agent stream="tool"  phase start/update/result   (the canonical tool
//     stream; a client that initiated chat.send with the tool-events capability
//     is registered as the run's tool recipient)
//   - agent stream="item" / "command_output"           (tool+command lifecycle
//     and streamed output; kept as a richer/back-compat source, deduped per
//     call id)
//   - chat  state delta/final/aborted/error            (visible assistant text;
//     aborted/error mark the run terminal)

const (
	// liveOutputCap bounds per-tool output accumulation so a runaway stream
	// cannot pin the API's memory for the duration of a turn.
	liveOutputCap = 1 << 20
)

// liveCall is the folding state of one in-flight tool call.
type liveCall struct {
	started   bool
	name      string // real tool name, preserved into the tool_result (audit/ledger)
	sawOutput bool   // output-carrying events seen for this call
	output    strings.Builder
	// pendingResult is the stream="tool" result candidate. It is deferred, not
	// emitted, because for exec-style tools the richer command_output arrives
	// afterwards and should win; non-exec tools finalize it at their item end.
	pendingResult string
	resultOut     bool // a tool_result has already been emitted for this call
}

// liveProjector folds the live session-message events of one chat turn into
// SSE events. One lives per active chat turn, so state is bounded by the turn.
type liveProjector struct {
	calls map[string]*liveCall
	texts map[string]bool // runId -> assistant text already emitted (final de-dup)
}

func newLiveProjector() *liveProjector {
	return &liveProjector{
		calls: map[string]*liveCall{},
		texts: map[string]bool{},
	}
}

// agentFrame is the subset of a gateway `agent` event payload we fold on.
type agentFrame struct {
	Stream string          `json:"stream"`
	Data   json.RawMessage `json:"data"`
}

// agentTool is the canonical tool stream event (stream="tool").
type agentTool struct {
	Phase       string          `json:"phase"`
	Name        string          `json:"name"`
	ToolCallID  string          `json:"toolCallId"`
	Title       string          `json:"title"`
	Args        json.RawMessage `json:"args"`
	Result      json.RawMessage `json:"result"`
	IsError     bool            `json:"isError"`
	ErrorString string          `json:"error"`
}

// agentItem / agentOutput cover the item / command_output streams.
type agentItem struct {
	Kind       string `json:"kind"`
	Phase      string `json:"phase"`
	Name       string `json:"name"`
	Meta       string `json:"meta"`
	Title      string `json:"title"`
	ToolCallID string `json:"toolCallId"`
}

type agentOutput struct {
	Phase      string `json:"phase"`
	ToolCallID string `json:"toolCallId"`
	Output     string `json:"output"`
	Summary    string `json:"summary"`
}

// chatDelta is the visible-text event the gateway broadcasts to session
// subscribers (server-chat.ts broadcastChatDelta): state "delta" carries the
// incremental deltaText; state "final"/"aborted"/"error" ends the run.
type chatDelta struct {
	RunID     string `json:"runId"`
	State     string `json:"state"`
	DeltaText string `json:"deltaText"`
	Replace   bool   `json:"replace"`
	Message   *struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
}

// feed processes one live event frame for the turn's session and returns any
// SSE events it maps to, plus whether the event terminates the run. evName is
// the gateway event name ("agent" for run/tool events, "chat" for text/status).
func (p *liveProjector) feed(sessionKey, evName string, payload []byte) ([]openclaw.Event, bool) {
	switch evName {
	case "agent":
		var fr agentFrame
		if err := json.Unmarshal(payload, &fr); err != nil {
			return nil, false
		}
		switch fr.Stream {
		case "tool":
			return p.toolEvent(sessionKey, fr.Data), false
		case "item":
			var it agentItem
			if err := json.Unmarshal(fr.Data, &it); err != nil || it.ToolCallID == "" {
				return nil, false
			}
			call := p.call(it.ToolCallID)
			if it.Kind == "tool" && it.Phase == "start" {
				// The canonical stream="tool" start for the same call is emitted
				// first; never emit a duplicate card from the item stream.
				if call.started {
					return nil, false
				}
				call.started = true
				call.name = it.Name
				args := it.Meta
				if args == "" {
					args = it.Title
				}
				return []openclaw.Event{{
					Type:      openclaw.EventToolCall,
					SessionID: sessionKey,
					Name:      it.Name,
					CallID:    it.ToolCallID,
					Arguments: args,
				}}, false
			}
			// Tool end is NOT final here: for exec-style tools the terminal
			// command_output arrives after this item end and must win, and for
			// non-exec tools the deferred stream="tool" result is emitted when
			// the run goes terminal (see finalizeAll). Emitting at item end would
			// either preempt the richer command output or double cards.
		case "command_output":
			var o agentOutput
			if err := json.Unmarshal(fr.Data, &o); err != nil || o.ToolCallID == "" {
				return nil, false
			}
			call := p.call(o.ToolCallID)
			call.sawOutput = true
			switch {
			case o.Phase == "end" && o.Output != "":
				// The terminal command_output carries the complete output, not a
				// delta; replace accumulated deltas so it is not duplicated.
				call.output.Reset()
				call.output.WriteString(o.Output)
			case o.Output != "":
				call.append(o.Output)
			}
			if o.Phase == "end" && !call.resultOut {
				call.resultOut = true
				out := call.output.String()
				if out == "" {
					out = o.Summary
				}
				return []openclaw.Event{liveResult(sessionKey, o.ToolCallID, call.name, out)}, false
			}
		}
	case "chat":
		var d chatDelta
		if err := json.Unmarshal(payload, &d); err != nil {
			return nil, false
		}
		switch d.State {
		case "delta":
			if d.DeltaText == "" {
				return nil, false
			}
			p.texts[d.RunID] = true
			if d.Replace {
				// A snapshot that supersedes previously streamed text (e.g. a
				// commentary rewritten after the tool ran): the frontend must
				// replace the bubble text, not append.
				return []openclaw.Event{{
					Type:      openclaw.EventTextReplace,
					SessionID: sessionKey,
					Delta:     d.DeltaText,
				}}, false
			}
			return []openclaw.Event{{
				Type:      openclaw.EventMessageDelta,
				SessionID: sessionKey,
				Delta:     d.DeltaText,
			}}, false
		case "final":
			// final is a full snapshot of the run's text; only surface it when no
			// delta was streamed (short replies that arrive as one frame).
			var evs []openclaw.Event
			if !p.texts[d.RunID] {
				text := d.DeltaText
				if text == "" && d.Message != nil {
					var b strings.Builder
					for _, c := range d.Message.Content {
						if c.Type == "text" {
							b.WriteString(c.Text)
						}
					}
					text = b.String()
				}
				if text != "" {
					p.texts[d.RunID] = true
					evs = append(evs, openclaw.Event{
						Type:      openclaw.EventMessageDelta,
						SessionID: sessionKey,
						Delta:     text,
					})
				}
			}
			// final ends the run's content projection: settle any tool cards that
			// were never finalized by a command_output (non-exec tools whose
			// stream="tool" result was deferred).
			flush := p.finalizeAll(sessionKey)
			return append(flush, evs...), true
		case "aborted", "error":
			// Terminal failure; settle tool cards and let the caller map the
			// error text into message_done.
			return p.finalizeAll(sessionKey), true
		}
	}
	return nil, false
}

// finalizeAll emits a tool_result for every started call that never reached a
// command_output terminal (non-exec tools whose stream="tool" result was
// deferred). Called when the run goes terminal so no card stays "Running".
func (p *liveProjector) finalizeAll(sessionKey string) []openclaw.Event {
	var out []openclaw.Event
	for id, call := range p.calls {
		if !call.started || call.resultOut {
			continue
		}
		call.resultOut = true
		outText := call.pendingResult
		if outText == "" {
			outText = call.output.String()
		}
		out = append(out, liveResult(sessionKey, id, call.name, outText))
	}
	return out
}

// toolEvent maps one agent stream="tool" frame. start begins the card,
// update streams nothing (progress), result emits the tool_result once.
func (p *liveProjector) toolEvent(sessionKey string, data json.RawMessage) []openclaw.Event {
	var t agentTool
	if err := json.Unmarshal(data, &t); err != nil || t.ToolCallID == "" {
		return nil
	}
	call := p.call(t.ToolCallID)
	switch t.Phase {
	case "start":
		// The item stream echoes a start for the same call later; emit once.
		if call.started {
			return nil
		}
		call.started = true
		call.sawOutput = false
		call.name = t.Name
		args := ""
		if len(t.Args) > 0 && string(t.Args) != "null" {
			args = string(t.Args) // keep JSON so the frontend renders arguments
		} else if t.Title != "" {
			args = t.Title
		}
		return []openclaw.Event{{
			Type:      openclaw.EventToolCall,
			SessionID: sessionKey,
			Name:      t.Name,
			CallID:    t.ToolCallID,
			Arguments: args,
		}}
	case "update":
		// partialResult progress -- no SSE equivalent yet; ignore.
		return nil
	case "result":
		if call.resultOut {
			return nil
		}
		// Only exec/bash-style tools defer to their later command_output (the
		// richer, complete output). Every other tool's stream result IS the
		// complete result, so emit it now -- deferring until run terminal would
		// leave read/search cards Running for the rest of the model turn.
		if isCommandTool(call.name) {
			call.pendingResult = toolResultText(t.Result)
			if t.IsError && call.pendingResult == "" {
				call.pendingResult = t.ErrorString
			}
			return nil
		}
		call.resultOut = true
		out := toolResultText(t.Result)
		if t.IsError && out == "" {
			out = t.ErrorString
		}
		return []openclaw.Event{liveResult(sessionKey, t.ToolCallID, call.name, out)}
	}
	return nil
}

// isCommandTool reports whether a tool streams its output through a separate
// command_output event (exec/bash family), whose terminal frame is the complete
// output. read/search/... carry their full result in stream="tool" phase=result.
func isCommandTool(name string) bool {
	if name == "" {
		return false
	}
	return name == "exec" || name == "bash" || strings.HasSuffix(name, "-exec")
}

// toolResultText renders a stream="tool" result payload as display text.
func toolResultText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	// Direct string or a single plain-string JSON value.
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
	}
	// Prefer common text-carrying fields.
	var rec map[string]json.RawMessage
	if json.Unmarshal(raw, &rec) == nil {
		for _, k := range []string{"text", "output", "stdout", "result"} {
			if v, ok := rec[k]; ok && len(v) > 0 && string(v) != "null" && string(v) != `""` {
				if v[0] == '"' {
					var s string
					if json.Unmarshal(v, &s) == nil && s != "" {
						return s
					}
				} else {
					return string(v)
				}
			}
		}
		// OpenAI-style content blocks: content: [{type:"text", text: ...}]
		if content, ok := rec["content"]; ok {
			var blocks []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(content, &blocks) == nil {
				var b strings.Builder
				for _, blk := range blocks {
					if blk.Type == "text" {
						b.WriteString(blk.Text)
					}
				}
				if b.Len() > 0 {
					return b.String()
				}
			}
		}
		// exec aggregates its full output under details.aggregated.
		var details map[string]json.RawMessage
		if rawDetails, ok := rec["details"]; ok && json.Unmarshal(rawDetails, &details) == nil {
			if v, ok := details["aggregated"]; ok && len(v) > 0 && string(v) != "null" {
				var s string
				if v[0] == '"' && json.Unmarshal(v, &s) == nil && s != "" {
					return s
				}
			}
		}
	}
	// Fall back to compact JSON.
	if b, err := json.Marshal(raw); err == nil {
		return string(b)
	}
	return string(raw)
}

func (p *liveProjector) call(id string) *liveCall {
	c, ok := p.calls[id]
	if !ok {
		c = &liveCall{}
		p.calls[id] = c
	}
	return c
}

func (c *liveCall) append(s string) {
	if c.output.Len() >= liveOutputCap {
		return
	}
	if c.output.Len()+len(s) > liveOutputCap {
		s = s[:liveOutputCap-c.output.Len()]
	}
	c.output.WriteString(s)
}

func liveResult(sessionKey, callID, name, output string) openclaw.Event {
	return openclaw.Event{
		Type:      openclaw.EventToolResult,
		SessionID: sessionKey,
		Name:      name,
		CallID:    callID,
		Output:    output,
	}
}
