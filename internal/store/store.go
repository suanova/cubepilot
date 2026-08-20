// Package store persists CubePilot platform metadata (scheduled tasks, run
// reports,
// audit entries, agent config) as JSON files on the backend PVC — the "tables"
// approach chosen over CRDs for phase one.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	maxReports  = 200
	maxAudit    = 1000
	maxMessages = 5000
)

// Report is one execution record of a task (or of /api/inspect).
type Report struct {
	ID         string    `json:"id"`
	TaskID     string    `json:"taskId"`
	TaskName   string    `json:"taskName"`
	Trigger    string    `json:"trigger"` // cron | manual | inspect
	Status     string    `json:"status"`  // success | failed
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`
	Content    string    `json:"content"`
	P0         int       `json:"p0"`
	P1         int       `json:"p1"`
	P2         int       `json:"p2"`
}

// AuditEntry records one tool invocation observed on the SSE stream (M5).
type AuditEntry struct {
	ID        string    `json:"id"`
	TS        time.Time `json:"ts"`
	User      string    `json:"user"`
	SessionID string    `json:"sessionId"`
	Tool      string    `json:"tool"`
	Command   string    `json:"command"`
	Level     string    `json:"level"`  // L0 readonly | L1 write
	Status    string    `json:"status"` // executed | failed
	Detail    string    `json:"detail,omitempty"`
}

// SkillToggle is one capability switch on the Agent config page.
type SkillToggle struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

// Message is one ledger row captured from the SSE stream (design doc §4.1:
// the platform is the source of truth for message history, captured via
// event-sourcing on the stream). Rows are written
// on the forwarding path as user messages and tool_call / tool_result /
// message_delta / message_done events flow through the assistant service.
type Message struct {
	ID             string          `json:"id"`
	ConversationID string          `json:"conversationId"`
	User           string          `json:"user"`
	Role           string          `json:"role"`                // user | assistant | tool | system
	EventType      string          `json:"eventType,omitempty"` // tool_call | tool_result | message_delta | message_done
	Content        string          `json:"content,omitempty"`
	ToolCalls      json.RawMessage `json:"toolCalls,omitempty"`
	ToolName       string          `json:"toolName,omitempty"`
	CallID         string          `json:"callId,omitempty"`
	Error          string          `json:"error,omitempty"`
	Incomplete     bool            `json:"incomplete,omitempty"` // interrupted/failed turn (design §4.1)
	CreatedAt      time.Time       `json:"createdAt"`
}

// TurnEnd marks a message_done: it flips the last assistant delta row (if any)
// from streaming to terminal and records whether the turn failed.
func (s *Store) TurnEnd(conversationID string, errMsg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var msgs []Message
	if err := s.file("messages.json", &msgs, false); err != nil {
		return err
	}
	// Mark the most recent assistant row for this conversation as terminal.
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].ConversationID == conversationID && msgs[i].Role == "assistant" {
			msgs[i].EventType = "message_done"
			msgs[i].Incomplete = errMsg != ""
			msgs[i].Error = errMsg
			break
		}
	}
	return s.save("messages.json", msgs)
}

// AppendMessage records one ledger row, capping the collection at maxMessages
// (oldest dropped). Returns the stored message with ID/timestamp filled in.
func (s *Store) AppendMessage(m Message) (Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var msgs []Message
	if err := s.file("messages.json", &msgs, false); err != nil {
		return Message{}, err
	}
	m.ID = shortID("m")
	m.CreatedAt = time.Now()
	msgs = append(msgs, m)
	if len(msgs) > maxMessages {
		msgs = msgs[len(msgs)-maxMessages:]
	}
	if err := s.save("messages.json", msgs); err != nil {
		return Message{}, err
	}
	return m, nil
}

// ListMessages returns ledger rows for a conversation, oldest-first (used for
// history rendering and cross-runtime re-seeding, design §4.1/§5.1).
func (s *Store) ListMessages(conversationID string, limit int) ([]Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var msgs []Message
	if err := s.file("messages.json", &msgs, false); err != nil {
		return nil, err
	}
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		if m.ConversationID == conversationID {
			out = append(out, m)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

// GCExpiredMessages prunes ledger rows older than the retention window
// (design §5.1 48~72h sliding window). Returns the number of rows removed.
func (s *Store) GCExpiredMessages(window time.Duration) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var msgs []Message
	if err := s.file("messages.json", &msgs, false); err != nil {
		return 0, err
	}
	cutoff := time.Now().Add(-window)
	kept := msgs[:0]
	for _, m := range msgs {
		if m.CreatedAt.After(cutoff) {
			kept = append(kept, m)
		}
	}
	removed := len(msgs) - len(kept)
	if removed > 0 {
		if err := s.save("messages.json", kept); err != nil {
			return 0, err
		}
	}
	return removed, nil
}

// AgentConfig is the persisted Agent config desired state (FR-M2-005 subset).
type AgentConfig struct {
	Model        string        `json:"model"`
	SystemPrompt string        `json:"systemPrompt"`
	Skills       []SkillToggle `json:"skills"`
}

// DefaultAgentConfig mirrors the baked-in capability catalog + gateway model.
func DefaultAgentConfig() AgentConfig {
	return AgentConfig{
		Model: "cuberouter/glm-5.1",
		Skills: []SkillToggle{
			{Name: "kubectl-platform", Enabled: true},
			{Name: "dev-environment", Enabled: true},
			{Name: "inference-service", Enabled: true},
			{Name: "inspection", Enabled: true},
		},
	}
}

// Store keeps each collection in one JSON file under dir.
type Store struct {
	dir string
	mu  sync.Mutex
}

// New opens (creating if needed) a store rooted at dir.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("store dir: %w", err)
	}
	return &Store{dir: dir}, nil
}

func shortID(prefix string) string {
	return fmt.Sprintf("%s-%s", prefix, uuid.NewString()[:8])
}

func (s *Store) file(name string, v any, create bool) error {
	path := filepath.Join(s.dir, name)
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if !create {
			return nil
		}
	case err != nil:
		return fmt.Errorf("read %s: %w", name, err)
	default:
		if err := json.Unmarshal(raw, v); err != nil {
			return fmt.Errorf("decode %s: %w", name, err)
		}
	}
	return nil
}

func (s *Store) save(name string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.dir, name+".tmp")
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	return os.Rename(tmp, filepath.Join(s.dir, name))
}

// ---- reports ----

// AddReport appends a report, capping the collection at maxReports.
func (s *Store) AddReport(r Report) (Report, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var reports []Report
	if err := s.file("reports.json", &reports, false); err != nil {
		return Report{}, err
	}
	r.ID = shortID("r")
	reports = append(reports, r)
	if len(reports) > maxReports {
		reports = reports[len(reports)-maxReports:]
	}
	if err := s.save("reports.json", reports); err != nil {
		return Report{}, err
	}
	return r, nil
}

// ---- audit ----

// AddAudit appends an audit entry, capping at maxAudit.
func (s *Store) AddAudit(e AuditEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var entries []AuditEntry
	if err := s.file("audit.json", &entries, false); err != nil {
		return err
	}
	e.ID = shortID("a")
	entries = append(entries, e)
	if len(entries) > maxAudit {
		entries = entries[len(entries)-maxAudit:]
	}
	return s.save("audit.json", entries)
}

// ListAudit returns audit entries newest-first.
func (s *Store) ListAudit(limit int) ([]AuditEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var entries []AuditEntry
	if err := s.file("audit.json", &entries, false); err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].TS.After(entries[j].TS) })
	if limit > 0 && len(entries) > limit {
		entries = entries[:limit]
	}
	return entries, nil
}

// ---- agent config ----

// GetAgentConfig returns the saved config merged over defaults.
func (s *Store) GetAgentConfig() (AgentConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg := DefaultAgentConfig()
	if err := s.file("agent-config.json", &cfg, false); err != nil {
		return AgentConfig{}, err
	}
	if cfg.Model == "" {
		cfg.Model = DefaultAgentConfig().Model
	}
	return cfg, nil
}

// SaveAgentConfig persists the config.
func (s *Store) SaveAgentConfig(cfg AgentConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.save("agent-config.json", cfg)
}
