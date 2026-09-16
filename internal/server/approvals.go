package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/openclaw/ws"
	agentruntime "github.com/suanova/cubepilot/internal/runtime"
	"github.com/suanova/cubepilot/internal/store"
)

// ApprovalService bridges gateway exec approvals to the Portal (issue #20):
// it records each pending approval surfaced by the gateway WebSocket client,
// injects a approval_pending event into the session's open SSE stream, and
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
	// inflight holds the approvals a Resolve has taken out of the maps above
	// for the duration of its gateway round trip. It exists so a settle that
	// lands in that window can still find the record: the reservation is what
	// makes the card absent from Pending, and without this the settle would find
	// nothing, the failed resolve would restore the record, and a reload would
	// resurrect a card for a session whose turn was stopped.
	//
	// It is bounded by the number of concurrent Resolve calls (at most one entry
	// each, removed on every exit path), so it is empty whenever no decision is
	// in flight.
	inflight map[string]*approvalReservation
}

// approvalReservation is one approval a Resolve has reserved while it talks to
// the gateway. settled records that the session was settled in that window: the
// gateway reply may still be honoured, but the card must not come back, so the
// restore path drops it instead of re-adding it.
type approvalReservation struct {
	pending pendingApproval
	settled bool
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
		inflight:  map[string]*approvalReservation{},
	}
}

// SetResolver wires the gateway-facing approval channel (WS client).
func (s *ApprovalService) SetResolver(r ApprovalResolver) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resolver = r
}

// Begin records a gateway approval and surfaces approval_pending on the session
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
		Type:      agentruntime.EventApprovalPending,
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
	// Reserve before the (slow) gateway round trip. The reservation stays
	// visible to a settle through inflight: the record is gone from the two maps
	// Pending reads, and a settle that lands in this window has to be able to
	// find it anyway.
	delete(s.byID, id)
	delete(s.bySession, p.SessionKey)
	res := &approvalReservation{pending: p}
	s.inflight[id] = res
	resolver := s.resolver
	s.mu.Unlock()

	// release ends the reservation. Every exit path calls exactly one of
	// release / restore, so inflight is empty again once the decision is over --
	// there is nothing to expire and nothing to sweep.
	release := func() {
		s.mu.Lock()
		delete(s.inflight, p.ApprovalID)
		s.mu.Unlock()
	}
	restore := func() {
		s.mu.Lock()
		delete(s.inflight, p.ApprovalID)
		// A settle that ran while this decision was in flight owns the outcome:
		// the session's turn is gone, so re-adding the record would put a card
		// back on screen (and back into reload recovery) for a run that was
		// stopped. The marker is consumed here -- it only ever covers an
		// in-flight reservation, and this is the one moment it can be honoured.
		if res.settled {
			s.mu.Unlock()
			return
		}
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
	release()
	s.hub.PublishTo(p.SessionKey, agentruntime.Event{
		Type:      agentruntime.EventApprovalResolved,
		SessionID: p.SessionKey,
		CallID:    p.ApprovalID,
		Approved:  &approved,
	})
	s.recordDecision(user, p, approved)
	return p, nil
}

// settleSession claims and forgets the session's pending approval without
// deciding it. It is the abort path: the run is gone, so there is nothing to
// allow or deny, and no decision is recorded -- a stopped turn is neither an
// approval nor a rejection that a later audit could attribute to the human. The
// session mapping is dropped only when it still points at this approval, so a
// newer Begin for the same session (which Resolve protects the same way) keeps
// its claim.
//
// Lookup and removal share one lock acquisition by design. As two (Pending, then
// settle) a Resolve can reserve the approval in between -- it deletes from both
// maps before its gateway round trip -- and the settle then finds nothing to
// claim, leaving the failed resolve's restore to put the card back for a session
// whose turn was stopped. Under one lock the reservation is found instead, and
// the outcome it returns is what restore honours.
//
// The reservation scan runs on every call, including the one that also claims a
// record from the maps: a claim from the maps says nothing about the reservations
// in flight for the same session, and one that is left unmarked is restored by
// its own failed resolve.
//
// A record is claimed only for its owner; another user's pending approval for
// the same session key is not this caller's to clear.
func (s *ApprovalService) settleSession(user, sessionKey string) (pendingApproval, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Mark every in-flight reservation for this session settled, on every call --
	// not only when the maps held nothing. Both states can hold for one session at
	// once: a Resolve reserves approval A, the gateway raises approval B, and
	// Begin puts B into the maps. Claiming B and returning would leave A's
	// reservation unmarked, so A's failed gateway call would restore it -- and,
	// with bySession just deleted, re-point the session at A -- putting a card
	// back on screen for a turn the user stopped. Marking all of them (rather
	// than the first) is the conservative choice: each belongs to a session whose
	// turn is gone, and the marker is only ever honoured by that reservation's
	// own restore.
	var reserved pendingApproval
	haveReserved := false
	for _, res := range s.inflight {
		if res.pending.SessionKey == sessionKey && res.pending.User == user {
			res.settled = true
			if !haveReserved {
				reserved, haveReserved = res.pending, true
			}
		}
	}

	// Whatever the maps still hold for this session is claimed and returned.
	if id, ok := s.bySession[sessionKey]; ok {
		if p, ok := s.byID[id]; ok && p.User == user {
			if cur, ok := s.bySession[p.SessionKey]; ok && cur == p.ApprovalID {
				delete(s.bySession, p.SessionKey)
			}
			delete(s.byID, p.ApprovalID)
			return p, true
		}
	}
	// Nothing in the maps, but a decision for this session may be mid-flight: the
	// record is out of them because Resolve reserved it. Reporting it lets the
	// caller publish the resolved event that drops the card.
	if haveReserved {
		return reserved, true
	}
	return pendingApproval{}, false
}

// settleApproval forgets one approval the gateway resolved on its own, keyed by
// approval id rather than by session.
//
// settleSession covers the case where the *platform* ended the run (Stop), and
// Resolve covers the case where the Portal decided. Neither covers the gateway
// ending an approval by itself: an approval that expires unanswered, or whose
// run is aborted gateway-side or dies with an agent runtime restart, is gone on
// the gateway while this service's maps still hold it. Nothing else ever removes
// such a record, so it stays in Pending -- and Pending is exactly what powers
// reload recovery, so the record comes back as a confirmation card on the next
// open of that conversation and fails when the user answers it, against an
// approval the gateway no longer recognises. The gateway broadcasts
// exec.approval.resolved for these, which is the only signal that they happened.
//
// It is not the same event as the Portal's own resolve: that path deletes the
// record itself and then publishes, and its gateway call also produces a
// broadcast, so this can arrive for a record that is already gone. Every branch
// is therefore idempotent, and the reservation scan mirrors settleSession --
// a Resolve in flight for this id must not restore the record afterwards, or the
// broadcast that says the gateway resolved it would be undone by our own
// bookkeeping.
//
// Only a record the caller's user owns is returned, matching settleSession: the
// gateway broadcast carries no user, so the caller passes the connection's user
// and another user's record is left alone.
func (s *ApprovalService) settleApproval(user, approvalID string) (pendingApproval, bool) {
	if approvalID == "" {
		return pendingApproval{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if res, ok := s.inflight[approvalID]; ok && res.pending.User == user {
		res.settled = true
	}

	p, ok := s.byID[approvalID]
	if !ok || p.User != user {
		return pendingApproval{}, false
	}
	if cur, ok := s.bySession[p.SessionKey]; ok && cur == p.ApprovalID {
		delete(s.bySession, p.SessionKey)
	}
	delete(s.byID, p.ApprovalID)
	return p, true
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

// handleApproval serves POST /api/sessions/{key}/approval.
func (s *Server) handleApproval(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST required"})
		return
	}
	user := s.userOf(r)
	sessionKey := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, apiPrefix+"/sessions/"), "/approval")
	sessionKey = strings.Trim(sessionKey, "/")
	if sessionKey == "" || s.approvals == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing session key"})
		return
	}
	var body struct {
		Decision string `json:"decision"`
	}
	if !decodeJSONBody(w, r, &body) {
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
		resp := map[string]any{"approved": approved, "decision": body.Decision, "approvalId": p.ApprovalID}
		if body.Decision == "allow-always" {
			// Durable grant (issue #116): approve-once happened above; now record
			// the command in the user's grants ConfigMap (grants.Store, via
			// allowlistAlways) so it auto-passes from the next turn on (only
			// under Allowlist policy). The instance spec is not touched.
			allowlisted := false
			if rule, ok := deriveAllowAlwaysRule(p.Command); ok {
				if ok, err := s.allowlistAlways(r.Context(), user, p.Command, rule); err != nil {
					s.logf("confirm %s/%s: allow-always grant: %v", user, sessionKey, err)
				} else {
					allowlisted = ok
				}
			}
			resp["allowlisted"] = allowlisted
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// handlePendingApproval serves GET /api/sessions/{key}/approval/pending — used to
// restore a confirmation card after a Portal reload mid-approval.
func (s *Server) handlePendingApproval(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET required"})
		return
	}
	user := s.userOf(r)
	sessionKey := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, apiPrefix+"/sessions/"), "/approval/pending")
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
	writeJSON(w, http.StatusOK, map[string]any{"approval": map[string]any{
		"sessionId":  p.SessionKey,
		"approvalId": p.ApprovalID,
		"tool":       p.Tool,
		"command":    p.Command,
		"level":      p.Level,
		"message":    p.Message,
	}})
}

// PreTurn is called at the start of an interactive turn. For Allowlist and
// AlwaysAsk it ensures the approval connection and applies the effective exec
// policy once per config revision. It returns whether the session must be
// guarded; RunLiveTurn reconciles that state atomically with the model.
//
// Both gated policies fail closed (issue #127): approvalPolicy is the single
// authority for whether a turn is guarded, so if the policy cannot be resolved
// or the approval channel/policy/guard cannot be applied, PreTurn returns an
// error and the caller must not start the turn. A policy that says "ask" must
// never silently run a turn that cannot ask -- that is the silent downgrade
// issue #127 exists to remove. Only a resolved None/empty policy passes through
// (an unresolvable config is treated as gated-unknown and fails closed, not as
// None).
func (m *gatewayConns) PreTurn(ctx context.Context, user string) (bool, error) {
	pol, allow, rev, err := m.resolved(ctx, user)
	if err != nil {
		return false, fmt.Errorf("hitl %s: cannot resolve confirm policy: %w", user, err)
	}
	switch pol {
	case v1alpha1.ApprovalPolicyAllowlist, v1alpha1.ApprovalPolicyAlwaysAsk:
		// Gated: the turn may not run unless the approval channel is up.
	case "", v1alpha1.ApprovalPolicyNone:
		return false, nil // nothing to gate
	default:
		// Fail closed. Reading an unrecognised policy as "nothing asks" would
		// silently run a write that was meant to be gated. The CRD schema
		// constrains the enum, so this is reachable only for a value stored
		// before the schema was applied -- which is exactly when a wrong guess
		// is most dangerous.
		return false, fmt.Errorf("hitl %s: unknown approval policy %q", user, pol)
	}
	gw, err := m.conn(ctx, user)
	if err != nil {
		return false, fmt.Errorf("hitl %s: cannot gate %s turn (approval channel unavailable): %w", user, pol, err)
	}
	// Apply the effective exec policy when the resolved-config revision changed;
	// only a successful apply advances revPol so a transient failure is retried
	// next turn.
	m.mu.Lock()
	appliedRev := m.revPol[user]
	m.mu.Unlock()
	if rev != "" && rev != appliedRev {
		if err := m.applyPolicy(ctx, user, gw, pol, allow); err != nil {
			return false, fmt.Errorf("hitl %s: cannot apply %s exec policy: %w", user, pol, err)
		}
		m.mu.Lock()
		m.revPol[user] = rev
		m.mu.Unlock()
	}
	return true, nil
}

// channelState reports whether the per-user approval channel can currently
// carry a gated turn (issue #127). An established connection answers "up" from
// cache; otherwise one bounded connect attempt is made and immediately dropped.
// A NOT_PAIRED rejection means the supervisor has not yet approved this user's
// derived device (first use) and reports "pairing" -- the rejected connect
// seeds the pending device the supervisor auto-approves on its next poll, so
// the state self-heals. Surfaced on the confirm view so a policy edit is never
// a silent no-op: with a "down"/"unconfigured" channel a gated turn fails
// closed rather than running ungated.
func (m *gatewayConns) channelState(ctx context.Context, user string) string {
	m.mu.Lock()
	if c, ok := m.conns[user]; ok && c.gw != nil && c.gw.Connected() {
		m.mu.Unlock()
		return approvalChannelUp
	}
	m.mu.Unlock()

	dev := m.deviceFor(user)
	gw := m.newClient(m.wsURLOf(user), dev)
	defer gw.Close()
	aCtx, cancel := context.WithTimeout(ctx, channelProbeTimeout)
	defer cancel()
	if err := gw.Connect(aCtx); err != nil {
		if strings.Contains(err.Error(), "NOT_PAIRED") {
			return approvalChannelPairing
		}
		m.sayf("hitl %s: channel probe: %v", user, err)
		return approvalChannelDown
	}
	return approvalChannelUp
}

// applyPolicyAttempts is how many get-modify-set rounds applyPolicy runs before
// giving up, and applyPolicyRetryDelay the pause between them.
const (
	applyPolicyAttempts   = 3
	applyPolicyRetryDelay = 50 * time.Millisecond
)

// applyPolicy writes the effective exec-approvals policy into agents."main" of
// the gateway (get -> set, CAS). The allowlist is rewritten wholesale from the
// resolved config (issue #116): the bookkeeping is that resolved list, so a
// removed entry really disappears -- for the entries a removal can express. An
// instance's own rule or a revoked grant drops out; a platform builtin cannot be
// removed at all (issue #185 accepted that loss), so it comes back on the next
// push. AlwaysAsk runs a guarded,
// on-miss session with an empty allowlist -- every command misses and therefore
// asks -- which is the strictest posture and needs no unverified ask:always
// semantics. It reports failure so the caller can defer advancing the
// applied-revision watermark, and retries a lost CAS before reporting one
// (issue #185).
func (m *gatewayConns) applyPolicy(ctx context.Context, user string, gw gatewayClient, pol v1alpha1.ApprovalPolicy, allow []v1alpha1.AllowlistRule) error {
	// Bounded retry (issue #185): the set is a compare-and-set against the hash
	// the get returned, so a write landing in between fails it. The caller
	// treats an error here as fatal to the turn, so a lost race must not be
	// surfaced. Both halves are repeated, not just the set: the retry needs the
	// hash the winner left, and re-reading it is also why no error is classified
	// as a conflict -- the gateway reports a CAS failure as a plain JSON-RPC
	// error with no typed discriminator to test for. The last error still
	// surfaces, so a gateway that is down or consistently rejecting is not
	// retried into a hang.
	var lastErr error
	for attempt := 0; attempt < applyPolicyAttempts; attempt++ {
		lastErr = m.applyPolicyOnce(ctx, user, gw, pol, allow)
		if lastErr == nil {
			return nil
		}
		if attempt < applyPolicyAttempts-1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(applyPolicyRetryDelay):
			}
		}
	}
	return lastErr
}

// applyPolicyOnce is one get-modify-set round of applyPolicy.
func (m *gatewayConns) applyPolicyOnce(ctx context.Context, user string, gw gatewayClient, pol v1alpha1.ApprovalPolicy, allow []v1alpha1.AllowlistRule) error {
	snap, err := gw.GetApprovalsPolicy(ctx)
	if err != nil {
		m.sayf("hitl %s: exec.approvals.get: %v", user, err)
		return err
	}
	file := snap.File
	if file.Agents == nil {
		file.Agents = map[string]ws.ApprovalAgentPolicy{}
	}
	agent := file.Agents["main"]
	switch pol {
	case v1alpha1.ApprovalPolicyAlwaysAsk:
		// Clear any allowlist a prior Allowlist mode wrote: on-miss with an
		// empty allowlist asks on everything.
		agent.Allowlist = nil
	default: // ApprovalPolicyAllowlist
		agent.Allowlist = toWSEntries(allow)
	}
	file.Agents["main"] = agent
	base := ""
	if snap.Exists {
		base = snap.Hash
	}
	if _, err := gw.SetApprovalsPolicy(ctx, file, base); err != nil {
		m.sayf("hitl %s: exec.approvals.set: %v", user, err)
		return err
	}
	return nil
}

// toWSEntries converts the resolved (public-API) allowlist rules into the
// gateway's exec-approvals entry shape.
func toWSEntries(rules []v1alpha1.AllowlistRule) []ws.AllowlistEntry {
	out := make([]ws.AllowlistEntry, 0, len(rules))
	for _, r := range rules {
		if r.Pattern == "" {
			continue
		}
		out = append(out, ws.AllowlistEntry{Pattern: r.Pattern, ArgPattern: r.ArgPattern})
	}
	return out
}

// ResolveApproval implements ApprovalResolver: the Portal decision is applied
// to the user's gateway connection.
func (m *gatewayConns) ResolveApproval(ctx context.Context, user, approvalID, decision string) error {
	gw, ok := m.liveConn(user)
	if !ok {
		return fmt.Errorf("no approval connection for %s", user)
	}
	gwDecision := "deny"
	if decision == "approve" {
		gwDecision = "allow-once"
	}
	return gw.ResolveApproval(ctx, approvalID, gwDecision)
}
