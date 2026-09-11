// Package runtime defines CubePilot's transport-neutral agent adapter contract.
// Implementations choose their own protocols; callers depend only on the
// interaction semantics exposed here.
package runtime

import (
	"context"
	"encoding/json"
)

const (
	EventMessageStart    = "message_start"
	EventAgentThinking   = "agent_thinking"
	EventToolCall        = "tool_call"
	EventToolResult      = "tool_result"
	EventMessageDelta    = "message_delta"
	EventTextReplace     = "text_replace"
	EventMessageDone     = "message_done"
	EventConfirmPending  = "confirm_pending"
	EventConfirmResolved = "confirm_resolved"
	// EventQuestionPending surfaces an ask_user question the agent is blocked
	// on; EventQuestionResolved settles its card. message carries the terminal
	// status (answered / cancelled / expired).
	EventQuestionPending  = "question_pending"
	EventQuestionResolved = "question_resolved"
)

// QuestionOption is one selectable answer of a question.
type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// QuestionItem is one question the human must answer. QuestionID is the key the
// answer is submitted under.
type QuestionItem struct {
	QuestionID  string           `json:"questionId"`
	Header      string           `json:"header"`
	Question    string           `json:"question"`
	Options     []QuestionOption `json:"options"`
	MultiSelect bool             `json:"multiSelect,omitempty"`
}

// QuestionPrompt is the runtime-neutral projection of a pending question.
// TimeoutSeconds is the time remaining when the event was produced (not an
// absolute deadline), so a countdown does not depend on the reader's clock
// agreeing with the producer's.
type QuestionPrompt struct {
	Questions      []QuestionItem `json:"questions"`
	TimeoutSeconds int            `json:"timeoutSeconds,omitempty"`
}

// Event is one runtime-neutral event in CubePilot's streaming contract.
type Event struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id,omitempty"`
	Name      string `json:"name,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Output    string `json:"output,omitempty"`
	Command   string `json:"command,omitempty"`
	Level     string `json:"level,omitempty"`
	Message   string `json:"message,omitempty"`
	Approved  *bool  `json:"approved,omitempty"`
	Delta     string `json:"delta,omitempty"`
	Error     string `json:"error,omitempty"`
	// Stopped reports that the turn ended because the user stopped it, as
	// opposed to failing or completing. Only ever set on message_done, and
	// never together with Error.
	Stopped bool `json:"stopped,omitempty"`
	// Question carries the pending question prompt on question_pending, with
	// CallID holding the gateway question id used to answer it.
	Question *QuestionPrompt `json:"question,omitempty"`
}

// Marshal returns the SSE data payload for an event.
func (e Event) Marshal() []byte {
	b, _ := json.Marshal(e)
	return b
}

// ChatMessage is one input message for a one-shot turn.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatParams carries the inputs for a one-shot turn. Model is an optional
// resolved per-turn backend model override; an empty value means the runtime
// default.
type ChatParams struct {
	Model      string
	SessionKey string
	Messages   []ChatMessage
}

// Session is the runtime-neutral session list projection.
type Session struct {
	SessionKey string `json:"sessionKey"`
	Title      string `json:"title"`
}

// LiveTurnParams carries one interactive turn. Model is an optional resolved
// per-turn override; an empty value means the runtime default.
type LiveTurnParams struct {
	Message string
	Model   string
}

// TurnOutcome reports how a live turn ended when it did not fail. The terminal
// message_done is emitted by the HTTP handler, so a runtime that merely returns
// a nil error cannot express "stopped by request" -- that needs its own channel.
type TurnOutcome struct {
	// Stopped is true when the turn was cancelled by request (the chat.abort
	// RPC or the gateway's /stop command), not by a failure and not by
	// completing normally.
	Stopped bool
}

// LiveTurnRunner drives interactive chat, including live tool events and any
// runtime-native approval gate required before the turn starts.
type LiveTurnRunner interface {
	RunLiveTurn(ctx context.Context, sessionKey string, params LiveTurnParams, emit func(Event) error) (TurnOutcome, error)
}

// OneShotRunner drives non-interactive turns such as scheduled tasks and
// synchronous inspections.
type OneShotRunner interface {
	StreamChat(ctx context.Context, params ChatParams, emit func(Event) error) error
}

// SessionReader exposes read-only session metadata and history.
type SessionReader interface {
	ListSessions(ctx context.Context) ([]Session, error)
	GetHistory(ctx context.Context, sessionKey string, limit int) (json.RawMessage, error)
}

// AgentRuntime is the complete adapter a runtime implementation exposes to
// CubePilot. A new runtime implements this interface without adopting
// OpenClaw's HTTP or WebSocket protocols.
type AgentRuntime interface {
	LiveTurnRunner
	OneShotRunner
	SessionReader
}

// Composite combines independently implemented live, one-shot, and session
// surfaces into one AgentRuntime. It lets adapters use different transports
// internally without leaking that split to callers.
type Composite struct {
	LiveTurnRunner
	OneShotRunner
	SessionReader
}

var _ AgentRuntime = (*Composite)(nil)

// Compose returns one complete runtime adapter from its semantic surfaces.
func Compose(live LiveTurnRunner, oneShot OneShotRunner, sessions SessionReader) AgentRuntime {
	return &Composite{
		LiveTurnRunner: live,
		OneShotRunner:  oneShot,
		SessionReader:  sessions,
	}
}
