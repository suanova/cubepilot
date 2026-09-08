// Package store persists the API's audit ledger as JSON files on the backend
// PVC. Reports, the platform message ledger and the global agent config have
// all moved out: run/task state lives in CRDs and the runtime, and agent config
// is per-user on the AgentInstance CR -- the only metadata the API itself must
// own is the audit trail, which must survive instance restarts.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const maxAudit = 1000

// AuditEntry records one tool invocation observed on the SSE stream (M5).
type AuditEntry struct {
	ID        string    `json:"id"`
	TS        time.Time `json:"ts"`
	User      string    `json:"user"`
	SessionID string    `json:"sessionId"`
	Tool      string    `json:"tool"`
	Command   string    `json:"command"`
	Level     string    `json:"level"`  // L0 readonly | L1 write
	Status    string    `json:"status"` // executed | approved | rejected | failed
	Detail    string    `json:"detail,omitempty"`
}

// Store keeps one audit JSON file per user under dir.
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

func shortID() string {
	return fmt.Sprintf("a-%s", uuid.NewString()[:8])
}

// file loads (or lazily creates) one JSON file into v.
func (s *Store) file(name string, v any) error {
	path := filepath.Join(s.dir, name)
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
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

// auditFile maps a user to their audit file name. Users come from the portal
// identity header (arbitrary strings), so only filesystem-safe characters are
// kept; collisions after sanitizing are tolerated because the entry's User
// field is authoritative on read.
func auditFile(user string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, user)
	if safe == "" {
		safe = "default"
	}
	return "audit-" + safe + ".json"
}

// ---- audit ----

// AddAudit appends an audit entry to its user's ledger, capping the collection
// at maxAudit. The user is read from the entry (never trusted from the file
// name), so each user's ledger is isolated on disk.
func (s *Store) AddAudit(e AuditEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := auditFile(e.User)
	var entries []AuditEntry
	if err := s.file(name, &entries); err != nil {
		return err
	}
	e.ID = shortID()
	entries = append(entries, e)
	if len(entries) > maxAudit {
		entries = entries[len(entries)-maxAudit:]
	}
	return s.save(name, entries)
}

// ListAudit returns a user's audit entries newest-first.
func (s *Store) ListAudit(user string, limit int) ([]AuditEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var entries []AuditEntry
	if err := s.file(auditFile(user), &entries); err != nil {
		return nil, err
	}
	// The file name is only a partition hint; trust the entry's User on read so
	// a sanitized-name collision can never leak another user's entries.
	owned := entries[:0]
	for _, e := range entries {
		if e.User == user {
			owned = append(owned, e)
		}
	}
	sort.Slice(owned, func(i, j int) bool { return owned[i].TS.After(owned[j].TS) })
	if limit > 0 && len(owned) > limit {
		owned = owned[:limit]
	}
	return owned, nil
}
