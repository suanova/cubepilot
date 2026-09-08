// Package store persists CubePilot platform metadata (scheduled tasks, run
// reports,
// audit entries, agent config) as JSON files on the backend PVC -- the "tables"
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
	maxReports = 200
	maxAudit   = 1000
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

// SkillToggle is one skill switch on the Agent config page.
type SkillToggle struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

// AgentConfig is the persisted Agent config desired state (FR-M2-005 subset).
type AgentConfig struct {
	Model        string        `json:"model"`
	SystemPrompt string        `json:"systemPrompt"`
	Skills       []SkillToggle `json:"skills"`
}

// DefaultAgentConfig mirrors the baked-in skill catalog. The model is empty:
// no platform default LLM is assumed (issue #117) -- a model-less install must
// not present a DeepSeek default that is absent from the agent-for-cloud
// template. An operator-configured default supplied to New still overrides it.
func DefaultAgentConfig() AgentConfig {
	return AgentConfig{
		Model: "",
		Skills: []SkillToggle{
			{Name: "kubectl-platform", Enabled: true},
			{Name: "cluster-inspection", Enabled: true},
			{Name: "cubestack-platform", Enabled: true},
		},
	}
}

// defaultConfig returns the baked-in defaults with the operator-configured
// default model (seeds the portal model selector until the user picks a model).
func (s *Store) defaultConfig() AgentConfig {
	cfg := DefaultAgentConfig()
	if s.defaultModel != "" {
		cfg.Model = s.defaultModel
	}
	return cfg
}

// Store keeps each collection in one JSON file under dir.
type Store struct {
	dir          string
	defaultModel string // operator-configured default LLM (the builtin template default)
	mu           sync.Mutex
}

// New opens (creating if needed) a store rooted at dir. defaultModel is the
// operator-configured default LLM (the builtin AgentTemplate default): it seeds
// the Agent config so the portal's model selector always shows a valid template
// model instead of a stale hardcoded value.
func New(dir, defaultModel string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("store dir: %w", err)
	}
	return &Store{dir: dir, defaultModel: defaultModel}, nil
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

// GetAgentConfig returns the saved config merged over defaults. An explicitly
// saved empty model ("Runtime Default" on the portal) is preserved so the
// selector stays on Runtime Default and the instance keeps no override.
func (s *Store) GetAgentConfig() (AgentConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg := s.defaultConfig()
	if err := s.file("agent-config.json", &cfg, false); err != nil {
		return AgentConfig{}, err
	}
	return cfg, nil
}

// SaveAgentConfig persists the config.
func (s *Store) SaveAgentConfig(cfg AgentConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.save("agent-config.json", cfg)
}
