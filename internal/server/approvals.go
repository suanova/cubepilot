package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/suanova/cubepilot/internal/openclaw"
	"github.com/suanova/cubepilot/internal/store"
)

// ApprovalService bridges gateway exec approvals to the Portal (issue #20):
// it records each pending approval surfaced by the gateway WebSocket client,
// injects a confirm_pending event into the session's open SSE stream, and
// resolves the Portal's approve/reject decision back over the WS client. The
// gateway-facing half is an ApprovalResolver (nil until the WS client is
// wired) so the service stays testable in isolation.
type ApprovalService struct {
	hub      *Hub
	store    *store.Store
	logf     func(format string, args ...any)
	resolver ApprovalResolver

	mu        sync.Mutex
	byID      map[string]pendingApproval // approval id -> pending
	bySession map[string]string          // session key -> approval id (one pending per session)
}

// ApprovalResolver resolves a pending approval on the gateway. decision is the
// canonical Portal value: "approve" or "reject".
type ApprovalResolver interface {
	ResolveApproval(ctx context.Context, user, approvalID, decision string) error
}

// pendingApproval is one write awaiting a human decision.
type pendingApproval struct {
	ApprovalID string
	SessionKey string
	User       string
	Tool       string
	Command    string
	Level      string
	Message    string
	CreatedAt  time.Time
}

// NewApprovalService returns an ApprovalService backed by the given hub/store.
func NewApprovalService(hub *Hub, st *store.Store, logf func(format string, args ...any)) *ApprovalService {
	return &ApprovalService{
		hub:       hub,
		store:     st,
		logf:      logf,
		byID:      map[string]pendingApproval{},
		bySession: map[string]string{},
	}
}

// SetResolver wires the gateway-facing approval channel (WS client).
func (s *ApprovalService) SetResolver(r ApprovalResolver) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resolver = r
}

// Begin records a gateway approval and surfaces confirm_pending on the session
// stream. Called by the gateway WS glue when exec.approval.requested arrives.
func (s *ApprovalService) Begin(user string, p pendingApproval) {
	if p.ApprovalID == "" || p.SessionKey == "" {
		s.logf("approvals: Begin skipped: missing approval/session id")
		return
	}
	s.mu.Lock()
	if _, dup := s.byID[p.ApprovalID]; dup {
		s.mu.Unlock()
		return
	}
	p.User = user
	p.Level = "write"
	if p.Tool == "" {
		p.Tool = "exec"
	}
	s.byID[p.ApprovalID] = p
	s.bySession[p.SessionKey] = p.ApprovalID
	s.mu.Unlock()

	s.hub.PublishTo(p.SessionKey, openclaw.Event{
		Type:      openclaw.EventConfirmPending,
		SessionID: p.SessionKey,
		CallID:    p.ApprovalID,
		Name:      p.Tool,
		Command:   p.Command,
		Level:     p.Level,
		Message:   p.Message,
	})
}

// Pending returns the active pending approval for a session, if the caller is
// its owner.
func (s *ApprovalService) Pending(user, sessionKey string) (pendingApproval, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.bySession[sessionKey]
	if !ok {
		return pendingApproval{}, false
	}
	p, ok := s.byID[id]
	if !ok || p.User != user {
		return pendingApproval{}, false
	}
	return p, true
}

// Resolve applies the Portal decision. decision is "approve" or "reject". The
// approval is reserved under the lock before the gateway call so two
// concurrent decisions cannot both process the same pending approval; it is
// restored when the gateway call fails so the caller may retry.
func (s *ApprovalService) Resolve(user, sessionKey, decision string) (pendingApproval, error) {
	if decision != "approve" && decision != "reject" {
		return pendingApproval{}, fmt.Errorf("decision must be approve or reject")
	}

	s.mu.Lock()
	id, ok := s.bySession[sessionKey]
	if !ok {
		s.mu.Unlock()
		return pendingApproval{}, errNoPending
	}
	p, ok := s.byID[id]
	if !ok || p.User != user {
		s.mu.Unlock()
		return pendingApproval{}, errNoPending
	}
	// Reserve before the (slow) gateway round trip.
	delete(s.byID, id)
	delete(s.bySession, p.SessionKey)
	resolver := s.resolver
	s.mu.Unlock()

	restore := func() {
		s.mu.Lock()
		s.byID[p.ApprovalID] = p
		s.bySession[p.SessionKey] = p.ApprovalID
		s.mu.Unlock()
	}
	if resolver == nil {
		restore()
		return pendingApproval{}, errNoResolver
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := resolver.ResolveApproval(ctx, user, p.ApprovalID, decision); err != nil {
		restore()
		return pendingApproval{}, fmt.Errorf("resolve approval %s: %w", p.ApprovalID, err)
	}

	approved := decision == "approve"
	s.hub.PublishTo(p.SessionKey, openclaw.Event{
		Type:      openclaw.EventConfirmResolved,
		SessionID: p.SessionKey,
		CallID:    p.ApprovalID,
		Approved:  &approved,
	})
	s.recordDecision(user, p, approved)
	return p, nil
}

func (s *ApprovalService) recordDecision(user string, p pendingApproval, approved bool) {
	if s.store == nil {
		return
	}
	status := "rejected"
	if approved {
		status = "approved"
	}
	_ = s.store.AddAudit(store.AuditEntry{
		User:      user,
		SessionID: p.SessionKey,
		Tool:      p.Tool,
		Command:   p.Command,
		Level:     "L1", // a gated command is a write
		Status:    status,
		TS:        time.Now(),
	})
	_, _ = s.store.AppendMessage(store.Message{
		ConversationID: p.SessionKey,
		User:           user,
		Role:           "tool",
		EventType:      openclaw.EventConfirmResolved,
		ToolName:       p.Tool,
		CallID:         p.ApprovalID,
		Content:        p.Command,
		CreatedAt:      time.Now(),
	})
}

var (
	errNoPending  = errors.New("no pending approval for this session")
	errNoResolver = errors.New("approval channel unavailable")
)

// --- HTTP handlers -----------------------------------------------------------

// handleConfirm serves POST /api/sessions/{key}/confirm.
func (s *Server) handleConfirm(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST required"})
		return
	}
	user := s.userOf(r)
	sessionKey := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/sessions/"), "/confirm")
	sessionKey = strings.Trim(sessionKey, "/")
	if sessionKey == "" || s.approvals == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing session key"})
		return
	}
	var body struct {
		Decision string `json:"decision"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body"})
		return
	}
	if body.Decision != "approve" && body.Decision != "reject" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "decision must be approve or reject"})
		return
	}
	p, err := s.approvals.Resolve(user, sessionKey, body.Decision)
	switch {
	case errors.Is(err, errNoPending):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no pending approval for this session"})
	case errors.Is(err, errNoResolver):
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "approval channel unavailable"})
	case err != nil:
		s.logf("confirm %s/%s: %v", user, sessionKey, err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
	default:
		approved := body.Decision == "approve"
		writeJSON(w, http.StatusOK, map[string]any{"approved": approved, "decision": body.Decision, "approval_id": p.ApprovalID})
	}
}

// handlePendingConfirm serves GET /api/sessions/{key}/confirm/pending — used to
// restore a confirmation card after a Portal reload mid-approval.
func (s *Server) handlePendingConfirm(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET required"})
		return
	}
	user := s.userOf(r)
	sessionKey := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/sessions/"), "/confirm/pending")
	sessionKey = strings.Trim(sessionKey, "/")
	if sessionKey == "" || s.approvals == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing session key"})
		return
	}
	p, ok := s.approvals.Pending(user, sessionKey)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no pending approval"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id":  p.SessionKey,
		"approval_id": p.ApprovalID,
		"tool":        p.Tool,
		"command":     p.Command,
		"level":       p.Level,
		"message":     p.Message,
	})
}
