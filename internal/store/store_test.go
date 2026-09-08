package store

import (
	"testing"
	"time"
)

func mustAdd(t *testing.T, s *Store, user, command string, ts time.Time) {
	t.Helper()
	if err := s.AddAudit(AuditEntry{User: user, Command: command, TS: ts}); err != nil {
		t.Fatalf("AddAudit: %v", err)
	}
}

// TestAuditIsPerUser verifies each user's ledger is isolated: entries land on
// per-user files and ListAudit(user) never returns another user's entries.
func TestAuditIsPerUser(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mustAdd(t, s, "li.ming", "kubectl get pods", time.Now())
	mustAdd(t, s, "li.ming", "kubectl delete pod x", time.Now().Add(-time.Second))
	mustAdd(t, s, "zhang.wei", "helm list", time.Now())

	own, err := s.ListAudit("li.ming", 0)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if len(own) != 2 {
		t.Fatalf("li.ming entries = %d, want 2: %+v", len(own), own)
	}
	other, err := s.ListAudit("zhang.wei", 0)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if len(other) != 1 || other[0].User != "zhang.wei" {
		t.Fatalf("zhang.wei entries = %+v, want only his own", other)
	}
}

// TestAuditNewestFirst verifies ListAudit sorts newest first and honours limit.
func TestAuditNewestFirst(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mustAdd(t, s, "u", "old", time.Unix(0, 0))
	mustAdd(t, s, "u", "mid", time.Unix(5, 0))
	mustAdd(t, s, "u", "new", time.Unix(10, 0))

	got, err := s.ListAudit("u", 2)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if len(got) != 2 || got[0].Command != "new" || got[1].Command != "mid" {
		t.Fatalf("ListAudit = %+v, want [new mid]", got)
	}
}

// TestAuditCapsPerUser verifies the per-user ledger is trimmed to maxAudit and
// one user's volume cannot evict another's.
func TestAuditCapsPerUser(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i := 0; i < maxAudit+50; i++ {
		mustAdd(t, s, "alice", "cmd", time.Unix(int64(i), 0))
	}
	for i := 0; i < 5; i++ {
		mustAdd(t, s, "bob", "cmd", time.Unix(int64(i), 0))
	}
	alice, err := s.ListAudit("alice", 0)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if len(alice) != maxAudit {
		t.Fatalf("alice capped at %d, got %d", maxAudit, len(alice))
	}
	bob, err := s.ListAudit("bob", 0)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if len(bob) != 5 {
		t.Fatalf("bob entries = %d, want 5 (untouched by alice's volume)", len(bob))
	}
}
