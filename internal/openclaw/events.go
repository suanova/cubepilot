package openclaw

import "encoding/json"

// Event types of the CubePilot streaming contract (design doc FR-M1-003).
// Phase one emits the six non-confirm events; confirm_* is phase two (HITL).
const (
	EventMessageStart    = "message_start"
	EventAgentThinking   = "agent_thinking"
	EventToolCall        = "tool_call"
	EventToolResult      = "tool_result"
	EventMessageDelta    = "message_delta"
	EventMessageDone     = "message_done"
	EventConfirmPending  = "confirm_pending" // phase two
	EventConfirmResolved = "confirm_resolved"
)

// Event is a single CubePilot SSE event payload.
type Event struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id,omitempty"`
	// Tool call / result fields.
	Name      string `json:"name,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Output    string `json:"output,omitempty"`
	// Confirm fields (confirm_pending / confirm_resolved, issue #20). call_id
	// carries the gateway approval id; Name carries the tool ("exec").
	Command string `json:"command,omitempty"`
	Level   string `json:"level,omitempty"` // "read" | "write"
	Message string `json:"message,omitempty"`
	// Approved is *bool so confirm_resolved can carry an explicit false (reject)
	// while every other event omits the key entirely.
	Approved *bool `json:"approved,omitempty"`
	// Streaming text fields.
	Delta string `json:"delta,omitempty"`
	// Terminal error, carried on message_done.
	Error string `json:"error,omitempty"`
}

// Marshal returns the SSE "data:" payload for an event.
func (e Event) Marshal() []byte {
	b, _ := json.Marshal(e)
	return b
}

// ChatChunk is the OpenAI-compatible streamed chunk from OpenClaw's
// /v1/chat/completions gateway endpoint (runs the full agent loop server-side).
type ChatChunk struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Choices []chatChoice `json:"choices"`
	// Error carries a streamed OpenAI-compatible error (e.g. the agent run
	// failed mid-turn). The gateway emits data: {"error": {...}} before [DONE];
	// without it the client would silently finish with no content.
	Error *chatError `json:"error"`
}

type chatError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

type chatChoice struct {
	Index int `json:"index"`
	Delta struct {
		Role      string     `json:"role"`
		Content   string     `json:"content"`
		ToolCalls []toolCall `json:"tool_calls"`
	} `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

type toolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// streamMapper folds a sequence of OpenAI chunks into CubePilot events.
// Tool-call arguments stream as fragments, so they are accumulated and flushed
// once the turn moves on to content (or finishes with a tool-calls stop reason).
// message_start / agent_thinking are emitted by the HTTP handler (which also
// signals the instance cold-start "warming" phase), not by the mapper.
type streamMapper struct {
	sessionID string

	flushed map[int]bool
	toolAcc map[int]*toolCallAcc
	done    bool
}

type toolCallAcc struct {
	id   string
	name string
	args []byte
}

// newStreamMapper returns a mapper for one chat turn.
func newStreamMapper(sessionID string) *streamMapper {
	return &streamMapper{
		sessionID: sessionID,
		flushed:   map[int]bool{},
		toolAcc:   map[int]*toolCallAcc{},
	}
}

// mapChunk returns the events produced by a single incoming chunk (may be empty).
func (m *streamMapper) mapChunk(ch ChatChunk) []Event {
	var out []Event
	for _, c := range ch.Choices {
		// Accumulate tool-call fragments.
		for _, tc := range c.Delta.ToolCalls {
			acc, ok := m.toolAcc[tc.Index]
			if !ok {
				acc = &toolCallAcc{id: tc.ID}
				m.toolAcc[tc.Index] = acc
			}
			if tc.ID != "" {
				acc.id = tc.ID
			}
			if tc.Function.Name != "" {
				acc.name = tc.Function.Name
			}
			acc.args = append(acc.args, tc.Function.Arguments...)
		}

		// A content delta means the tool-call phase is over: flush pending tool calls.
		if c.Delta.Content != "" {
			out = append(out, m.flushToolCalls()...)
			out = append(out, Event{Type: EventMessageDelta, SessionID: m.sessionID, Delta: c.Delta.Content})
		}

		// The model may stop with a tool-calls reason; flush accumulated calls too.
		if c.FinishReason != nil && *c.FinishReason == "tool_calls" {
			out = append(out, m.flushToolCalls()...)
		}
	}
	return out
}

func (m *streamMapper) flushToolCalls() []Event {
	var out []Event
	for idx, acc := range m.toolAcc {
		if m.flushed[idx] {
			continue
		}
		m.flushed[idx] = true
		out = append(out, Event{
			Type:      EventToolCall,
			SessionID: m.sessionID,
			Name:      acc.name,
			CallID:    acc.id,
			Arguments: string(acc.args),
		})
	}
	return out
}

// finish returns the terminal event for a completed (or failed) turn.
func (m *streamMapper) finish(errMsg string) Event {
	if m.done {
		return Event{}
	}
	m.done = true
	return Event{Type: EventMessageDone, SessionID: m.sessionID, Error: errMsg}
}
