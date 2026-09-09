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

	agentruntime "github.com/suanova/cubepilot/internal/runtime"
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

	s.hub.PublishTo(p.SessionKey, agentruntime.Event{
		Type:      agentruntime.EventConfirmPending,
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

// Resolve applies the Portal decision. decision is "approve", "reject" or
// "allow-always" (approve this once; issue #116 appends the durable grant
// separately). The approval is reserved under the lock before the gateway call
// so two concurrent decisions cannot both process the same pending approval; it
// is restored when the gateway call fails so the caller may retry.
func (s *ApprovalService) Resolve(ctx context.Context, user, sessionKey, decision string) (pendingApproval, error) {
	if decision != "approve" && decision != "reject" && decision != "allow-always" {
		return pendingApproval{}, fmt.Errorf("decision must be approve, reject or allow-always")
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
		// Re-add the reserved approval. Do not clobber the session mapping if a
		// newer Begin landed for the same session while the gateway call was in
		// flight -- that newer approval must stay the active one for the session.
		s.byID[p.ApprovalID] = p
		if cur, ok := s.bySession[p.SessionKey]; !ok || cur == p.ApprovalID {
			s.bySession[p.SessionKey] = p.ApprovalID
		}
		s.mu.Unlock()
	}
	if resolver == nil {
		restore()
		return pendingApproval{}, errNoResolver
	}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	approved := decision == "approve" || decision == "allow-always"
	gatewayDecision := "reject"
	if approved {
		gatewayDecision = "approve"
	}
	if err := resolver.ResolveApproval(ctx, user, p.ApprovalID, gatewayDecision); err != nil {
		restore()
		return pendingApproval{}, fmt.Errorf("resolve approval %s: %w", p.ApprovalID, err)
	}
	s.hub.PublishTo(p.SessionKey, agentruntime.Event{
		Type:      agentruntime.EventConfirmResolved,
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
	if body.Decision != "approve" && body.Decision != "reject" && body.Decision != "allow-always" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "decision must be approve, reject or allow-always"})
		return
	}
	p, err := s.approvals.Resolve(r.Context(), user, sessionKey, body.Decision)
	switch {
	case errors.Is(err, errNoPending):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no pending approval for this session"})
	case errors.Is(err, errNoResolver):
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "approval channel unavailable"})
	case err != nil:
		s.logf("confirm %s/%s: %v", user, sessionKey, err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
	default:
		approved := body.Decision != "reject"
		resp := map[string]any{"approved": approved, "decision": body.Decision, "approval_id": p.ApprovalID}
		if body.Decision == "allow-always" {
			// Durable grant (issue #116): approve-once happened above; now record
			// the command as an instance-owned allowlist entry so it auto-passes
			// from the next turn on (only under Allowlist policy).
			allowlisted := false
			if rule, ok := deriveAllowAlwaysRule(p.Command); ok {
				if ok, err := s.allowlistAlways(r.Context(), user, rule); err != nil {
					s.logf("confirm %s/%s: allow-always append: %v", user, sessionKey, err)
				} else {
					allowlisted = ok
				}
			}
			resp["allowlisted"] = allowlisted
		}
		writeJSON(w, http.StatusOK, resp)
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
