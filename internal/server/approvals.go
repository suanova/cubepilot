package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/openclaw/ws"
	agentruntime "github.com/suanova/cubepilot/internal/runtime"
	"github.com/suanova/cubepilot/internal/store"
)

// ApprovalService bridges gateway exec approvals to the Portal (issue #20 /
// #226): it relays each pending approval the gateway broadcasts onto the
// session's open SSE stream, reads a session's pending set from the gateway when
// a client asks for it, and writes the Portal's decision back addressed by
// approval id. The gateway-facing half is an ApprovalGateway (nil until the WS
// client is wired) so the service stays testable in isolation.
//
// The gateway is the single source of truth for what is pending, and nothing
// here keeps a copy of it. One session can hold several pending approvals at
// once, so a session-keyed slot holds only the newest of them -- and a decision
// looked up in such a copy settles whatever the copy pointed at rather than the
// approval the human clicked. A copy is also process-local, so a restart would
// lose approvals the gateway still holds, leaving cards on screen that no
// answer can settle.
type ApprovalService struct {
	hub   *Hub
	store *store.Store
	logf  func(format string, args ...any)

	mu      sync.Mutex
	gateway ApprovalGateway
	// inflight holds the decisions being written right now, one entry per approval
	// id. It is what makes two decisions on one id serialize rather than both
	// reaching the gateway (two tabs, or a double click), and what keeps a Stop
	// from publishing a neutral resolution over a decision already in flight. Each
	// entry is the decision's own reservation, closed when it ends so a waiter can
	// take it; it is bounded by the number of concurrent Resolve calls, so the map
	// is empty whenever no decision is being written.
	inflight map[string]*approvalDecision
}

// approvalDecision is one decision's reservation of an approval id. done is closed
// when the decision that holds it ends, whatever the outcome: a waiter takes the
// reservation and asks the gateway what is actually left.
type approvalDecision struct{ done chan struct{} }

// ApprovalGateway is the gateway-facing half of the approval channel: the
// pending approvals the user's connection can see, and the decision written back
// over it. Both run on the user's own device connection, which is what scopes
// them to that user -- the gateway filters the list to records that connection
// may see, so no platform-side owner check exists or is needed.
type ApprovalGateway interface {
	ListApprovals(ctx context.Context, user string) ([]ws.ApprovalRequested, error)
	ResolveApproval(ctx context.Context, user, approvalID, decision string) error
}

// pendingApproval is one write awaiting a human decision, projected from the
// gateway's own record. CreatedAtMs orders a session's cards; ExpiresAtMs is the
// gateway's deadline, which it enforces when it lists.
type pendingApproval struct {
	ApprovalID  string
	SessionKey  string
	Tool        string
	Command     string
	Level       string
	Message     string
	CreatedAtMs int64
	ExpiresAtMs int64
}

// projectApproval maps one gateway record onto the platform's projection. Tool
// and Level are fixed: only exec approvals are relayed here, and an exec
// approval is by definition a write.
func projectApproval(ev ws.ApprovalRequested) pendingApproval {
	return pendingApproval{
		ApprovalID:  ev.ID,
		SessionKey:  canonicalSessionKey(ev.Request.SessionKey),
		Tool:        "exec",
		Command:     ev.Request.Command,
		Level:       "write",
		Message:     ev.Request.WarningText,
		CreatedAtMs: ev.CreatedAtMs,
		ExpiresAtMs: ev.ExpiresAtMs,
	}
}

// NewApprovalService returns an ApprovalService backed by the given hub/store.
func NewApprovalService(hub *Hub, st *store.Store, logf func(format string, args ...any)) *ApprovalService {
	return &ApprovalService{
		hub:      hub,
		store:    st,
		logf:     logf,
		inflight: map[string]*approvalDecision{},
	}
}

// SetGateway wires the gateway-facing approval channel (WS client).
func (s *ApprovalService) SetGateway(g ApprovalGateway) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gateway = g
}

// list reads the pending approvals the user's gateway connection can see. There
// is deliberately no fallback to anything this process held: a fallback would
// answer from a copy the gateway may have moved on from, which is the state this
// service exists to remove.
func (s *ApprovalService) list(ctx context.Context, user string) ([]ws.ApprovalRequested, error) {
	s.mu.Lock()
	g := s.gateway
	s.mu.Unlock()
	if g == nil {
		return nil, errNoApprovalChannel
	}
	return g.ListApprovals(ctx, user)
}

// RelayRequested surfaces a pending approval on its session's stream. Called by
// the gateway WS glue when exec.approval.requested arrives.
//
// Nothing is recorded. This event is how an attached view learns of the card,
// and the gateway's own pending list is how a view that was not attached (or
// that reloaded, or that never saw the event) recovers it -- the same division
// the question relay uses.
func (s *ApprovalService) RelayRequested(user string, ev ws.ApprovalRequested) {
	if ev.ID == "" || ev.Request.SessionKey == "" {
		s.logf("approvals: %s dropped: record carries no approval or session id", user)
		return
	}
	p := projectApproval(ev)
	// No open stream means no browser is watching this session: the card reaches
	// the Portal only through reload recovery (issue #167 logs the same drop for
	// questions).
	if !s.hub.PublishTo(p.SessionKey, agentruntime.Event{
		Type:        agentruntime.EventApprovalPending,
		SessionID:   p.SessionKey,
		CallID:      p.ApprovalID,
		Name:        p.Tool,
		Command:     p.Command,
		Level:       p.Level,
		Message:     p.Message,
		CreatedAtMs: p.CreatedAtMs,
		ExpiresAtMs: p.ExpiresAtMs,
	}) {
		s.logf("approval %s: no open stream for session %s; push dropped", p.ApprovalID, p.SessionKey)
	}
}

// Pending returns the approvals the gateway holds for a session, oldest first.
// It is the authoritative read: reload recovery and the attach gate both ask it
// rather than consulting any state this process kept.
//
// The order is fixed here rather than inherited. The gateway lists its map in
// insertion order, which happens to be oldest first; making that a contract of
// this endpoint is what keeps the cards a client draws from depending on the
// gateway's iteration order. Stable, so records stamped in the same millisecond
// keep the gateway's own order.
//
// There is no expiry filter to apply: exec.approval.list expires due records
// before it answers, so everything it returns is answerable. (The question path
// needs that filter because question.list does not.)
//
// The read is bounded by approvalGatewayTimeout. Callers pass a request context,
// and the API server sets no write timeout, so a gateway connection that is open
// but wedged would otherwise hold the request open for as long as it likes.
func (s *ApprovalService) Pending(ctx context.Context, user, sessionKey string) ([]pendingApproval, error) {
	ctx, cancel := context.WithTimeout(ctx, approvalGatewayTimeout)
	defer cancel()
	list, err := s.list(ctx, user)
	if err != nil {
		return nil, err
	}
	out := []pendingApproval{}
	for _, ev := range list {
		if canonicalSessionKey(ev.Request.SessionKey) != sessionKey {
			continue
		}
		out = append(out, projectApproval(ev))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAtMs < out[j].CreatedAtMs })
	return out, nil
}

// Resolve applies the Portal decision to the named approval. decision is
// "approve", "reject" or "allow-always" (approve this once; issue #116 appends
// the durable grant separately).
//
// The approval is named by id, and that is the point of the signature: a session
// can hold several pending approvals at once, so a decision addressed by session
// settles whichever record the platform looked up rather than the one the human
// clicked. The id is verified against the gateway before anything is written --
// still pending, and this session's -- which is also how the record's own command
// is recovered for the allow-always rule, and how a decision works after a
// restart, when this process holds nothing at all.
//
// The whole decision -- the read that verifies it and the write that settles it --
// runs under one bound. Both are gateway round trips on the caller's request
// context, and the API server sets no write timeout, so a connection that is open
// but wedged would otherwise park the request (and its goroutine) for good.
func (s *ApprovalService) Resolve(ctx context.Context, user, sessionKey, approvalID, decision string) (pendingApproval, error) {
	if decision != "approve" && decision != "reject" && decision != "allow-always" {
		return pendingApproval{}, fmt.Errorf("decision must be approve, reject or allow-always")
	}
	ctx, cancel := context.WithTimeout(ctx, approvalGatewayTimeout)
	defer cancel()
	// Reserve first: a second decision on one id -- a double click, two tabs --
	// waits here for the first to finish instead of racing it into the gateway.
	if err := s.reserve(ctx, approvalID); err != nil {
		return pendingApproval{}, err
	}
	defer s.release(approvalID)

	list, err := s.Pending(ctx, user, sessionKey)
	if err != nil {
		return pendingApproval{}, err
	}
	p, ok := findApproval(list, approvalID)
	if !ok {
		return pendingApproval{}, errNoPending
	}
	approved := decision == "approve" || decision == "allow-always"
	gatewayDecision := "reject"
	if approved {
		gatewayDecision = "approve"
	}
	if err := s.gateway.ResolveApproval(ctx, user, approvalID, gatewayDecision); err != nil {
		return pendingApproval{}, fmt.Errorf("resolve approval %s: %w", approvalID, err)
	}
	s.publishResolved(p, &approved)
	s.recordDecision(user, p, approved)
	return p, nil
}

// findApproval returns the named approval from the session's pending set. The id
// is looked up inside that set rather than beside it, so the session binding is
// the same rule everywhere: an id that belongs to another conversation is simply
// not in this one's set, and is reported as not found -- the answer says nothing
// about approvals this caller was never shown.
func findApproval(list []pendingApproval, approvalID string) (pendingApproval, bool) {
	for _, p := range list {
		if p.ApprovalID == approvalID {
			return p, true
		}
	}
	return pendingApproval{}, false
}

// reserve claims an approval for one decision, waiting for a decision already in
// flight for the same id to finish first.
//
// Waiting rather than refusing is what makes a second decision safe. Refusing it
// with a conflict -- before the first has reached the gateway -- tells the second
// caller the approval is settled when nothing has been decided yet, and a client
// that closes the card on that answer (the web client does: 409 is "settled
// underneath the click") has lost the only control it had if the first decision
// then fails. Waiting answers with an outcome instead: the loop takes the
// reservation once the other decision is done, and the read that follows finds
// either a settled approval -- reported as not found, which is the truth -- or one
// still pending, which this call then settles itself. A failed decision is
// therefore retried by whichever caller is still holding the card.
//
// The wait is the caller's own bound, which Resolve has already applied, so a
// gateway that wedges cannot hold a waiter past its deadline.
func (s *ApprovalService) reserve(ctx context.Context, approvalID string) error {
	for {
		s.mu.Lock()
		held, busy := s.inflight[approvalID]
		if !busy {
			s.inflight[approvalID] = &approvalDecision{done: make(chan struct{})}
			s.mu.Unlock()
			return nil
		}
		done := held.done
		s.mu.Unlock()
		select {
		case <-done:
			// The reservation is gone: loop and take it ourselves.
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// release ends a reservation and wakes whatever was waiting on it. Every exit path
// of a decision calls it exactly once, so the entry is the decision's own and no
// other holder can remove it: inflight is empty again once the decision is over.
func (s *ApprovalService) release(approvalID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	held, ok := s.inflight[approvalID]
	if !ok {
		return
	}
	delete(s.inflight, approvalID)
	close(held.done)
}

// deciding reports whether a decision for this approval is being written now.
func (s *ApprovalService) deciding(approvalID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, busy := s.inflight[approvalID]
	return busy
}

// publishResolved drops the card on the session's stream. approved is nil for a
// resolution nobody decided -- a stopped turn -- which the client renders
// neutrally rather than as a rejection the human never made.
func (s *ApprovalService) publishResolved(p pendingApproval, approved *bool) {
	if !s.hub.PublishTo(p.SessionKey, agentruntime.Event{
		Type:      agentruntime.EventApprovalResolved,
		SessionID: p.SessionKey,
		CallID:    p.ApprovalID,
		Approved:  approved,
	}) {
		s.logf("approval %s: no open stream for session %s; resolution not delivered", p.ApprovalID, p.SessionKey)
	}
}

// recordDecision writes the human's decision to the audit ledger. The approval
// id is part of the entry: without it a ledger with two approvals and one
// decision cannot say which of them was answered, and the question "what did
// this decision apply to" has no answer that does not come from the gateway's
// logs.
func (s *ApprovalService) recordDecision(user string, p pendingApproval, approved bool) {
	if s.store == nil {
		return
	}
	status := "rejected"
	if approved {
		status = "approved"
	}
	_ = s.store.AddAudit(store.AuditEntry{
		User:       user,
		SessionID:  p.SessionKey,
		ApprovalID: p.ApprovalID,
		Tool:       p.Tool,
		Command:    p.Command,
		Level:      "L1", // a gated command is a write
		Status:     status,
		TS:         time.Now(),
	})
}

// approvalGatewayTimeout bounds one approval round trip -- a pending read, or a
// decision. It covers the whole operation the caller asked for, not one hop of
// it: a decision reads the gateway before it writes to it, and the read is what
// verifies that the approval is still the one the human clicked.
const approvalGatewayTimeout = 15 * time.Second

var (
	errNoPending = errors.New("no pending approval for this session")
	// errNoApprovalChannel reports that the user has no live gateway connection,
	// so the approval set cannot be read and no decision can be written. Reads
	// and decisions deliberately use the user's existing connection rather than
	// dialing one: opening a connection can trigger a device pairing, which a
	// status read must never do as a side effect.
	errNoApprovalChannel = errors.New("approval channel unavailable")
)

// --- HTTP handlers -----------------------------------------------------------

// handleApproval serves POST /api/sessions/{key}/approval -- the human's decision
// for one pending write, named by its approval id.
func (s *Server) handleApproval(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST required"})
		return
	}
	user := s.userOf(r)
	sessionKey := canonicalSessionKey(subresourceKey(r.URL.Path, "/approval"))
	if !hasSessionKey(sessionKey) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing session key"})
		return
	}
	if s.approvals == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": errNoApprovalChannel.Error()})
		return
	}
	var body struct {
		ApprovalID string `json:"approvalId"`
		Decision   string `json:"decision"`
	}
	if !decodeJSONBody(w, r, &body) {
		return
	}
	// The id is required, not defaulted. A session can hold several pending
	// approvals, so a decision that does not name one has no correct meaning --
	// and inventing "the newest" is exactly how a click on one card settles
	// another.
	if body.ApprovalID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "approvalId required"})
		return
	}
	if body.Decision != "approve" && body.Decision != "reject" && body.Decision != "allow-always" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "decision must be approve, reject or allow-always"})
		return
	}
	p, err := s.approvals.Resolve(r.Context(), user, sessionKey, body.ApprovalID, body.Decision)
	switch {
	case errors.Is(err, errNoPending):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such pending approval for this session"})
	case errors.Is(err, errNoApprovalChannel):
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "approval channel unavailable"})
	case err != nil:
		s.writeGatewayError(w, user, "approval "+body.ApprovalID, err, approvalErrorStatus)
	default:
		approved := body.Decision != "reject"
		resp := map[string]any{"approved": approved, "decision": body.Decision, "approvalId": p.ApprovalID}
		if body.Decision == "allow-always" {
			// Durable grant (issue #116): approve-once happened above; now record
			// the command in the user's grants ConfigMap (grants.Store, via
			// allowlistAlways) so it auto-passes from the next turn on (only
			// under Allowlist policy). The instance spec is not touched. The
			// command comes from the gateway's own record of this approval, which
			// is also how it is known at all after a restart.
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

// approvalErrorStatus maps a gateway approval error reason onto the Portal
// status. APPROVAL_NOT_FOUND is the answer for an approval that expired or was
// cleared (the gateway expires due records and clears a dead run's), and
// APPROVAL_ALREADY_RESOLVED for one another client settled first; both mean the
// card is gone rather than that the request failed.
func approvalErrorStatus(reason string) int {
	switch reason {
	case "APPROVAL_NOT_FOUND":
		return http.StatusNotFound
	case "APPROVAL_ALREADY_RESOLVED":
		return http.StatusConflict
	default:
		return http.StatusBadGateway
	}
}

// approvalEntry is one pending approval served to the Portal for reload
// recovery. It mirrors the approval_pending SSE payload, so a restored card is
// built by the same code path as a live one -- plus the stamps a client needs to
// order several cards and to tell how long the gateway will hold them.
type approvalEntry struct {
	SessionID   string `json:"sessionId"`
	ApprovalID  string `json:"approvalId"`
	Tool        string `json:"tool"`
	Command     string `json:"command"`
	Level       string `json:"level"`
	Message     string `json:"message,omitempty"`
	CreatedAtMs int64  `json:"createdAtMs,omitempty"`
	ExpiresAtMs int64  `json:"expiresAtMs,omitempty"`
}

// handlePendingApproval serves GET /api/sessions/{key}/approval/pending — used to
// restore confirmation cards after a Portal reload mid-approval. It answers the
// session's whole pending set, because a session can hold several approvals at
// once and the page that reloads must come back with all of them.
//
// 404 means "nothing pending", the same convention the sibling question endpoint
// uses, so that two subresources of one session do not give an empty read two
// different meanings.
func (s *Server) handlePendingApproval(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET required"})
		return
	}
	user := s.userOf(r)
	sessionKey := canonicalSessionKey(subresourceKey(r.URL.Path, "/approval/pending"))
	if !hasSessionKey(sessionKey) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing session key"})
		return
	}
	noPending := func() {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no pending approval"})
	}
	if s.approvals == nil {
		noPending()
		return
	}
	list, err := s.approvals.Pending(r.Context(), user, sessionKey)
	switch {
	case errors.Is(err, errNoApprovalChannel):
		// No channel means no open approval, the same answer the question
		// endpoint gives: an approval only ever exists alongside the live turn
		// that is parked on it, and that turn has the channel.
		noPending()
		return
	case err != nil:
		// A gateway that could not answer is not a session with nothing pending:
		// saying so would leave the reloaded page holding no card for a write
		// that is still parked.
		s.logf("pending approval %s/%s: %v", user, sessionKey, err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	out := make([]approvalEntry, 0, len(list))
	for _, p := range list {
		out = append(out, approvalEntry{
			SessionID:   p.SessionKey,
			ApprovalID:  p.ApprovalID,
			Tool:        p.Tool,
			Command:     p.Command,
			Level:       p.Level,
			Message:     p.Message,
			CreatedAtMs: p.CreatedAtMs,
			ExpiresAtMs: p.ExpiresAtMs,
		})
	}
	if len(out) == 0 {
		noPending()
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"approvals": out})
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
		return false, fmt.Errorf("gateway %s: cannot resolve confirm policy: %w", user, err)
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
		return false, fmt.Errorf("gateway %s: unknown approval policy %q", user, pol)
	}
	gw, err := m.conn(ctx, user)
	if err != nil {
		return false, fmt.Errorf("gateway %s: cannot gate %s turn (approval channel unavailable): %w", user, pol, err)
	}
	// Apply the effective exec policy when the resolved-config revision changed;
	// only a successful apply advances revPol so a transient failure is retried
	// next turn.
	m.mu.Lock()
	appliedRev := m.revPol[user]
	m.mu.Unlock()
	if rev != "" && rev != appliedRev {
		if err := m.applyPolicy(ctx, user, gw, pol, allow); err != nil {
			return false, fmt.Errorf("gateway %s: cannot apply %s exec policy: %w", user, pol, err)
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
		m.sayf("gateway %s: channel probe: %v", user, err)
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
		m.sayf("gateway %s: exec.approvals.get: %v", user, err)
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
		m.sayf("gateway %s: exec.approvals.set: %v", user, err)
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

// ResolveApproval implements ApprovalGateway: the Portal decision is applied to
// the user's gateway connection, addressed by approval id.
func (m *gatewayConns) ResolveApproval(ctx context.Context, user, approvalID, decision string) error {
	gw, ok := m.liveConn(user)
	if !ok {
		return errNoApprovalChannel
	}
	gwDecision := "deny"
	if decision == "approve" {
		gwDecision = "allow-once"
	}
	return gw.ResolveApproval(ctx, approvalID, gwDecision)
}

// ListApprovals implements ApprovalGateway: the pending approvals the user's
// gateway connection can see.
func (m *gatewayConns) ListApprovals(ctx context.Context, user string) ([]ws.ApprovalRequested, error) {
	gw, ok := m.liveConn(user)
	if !ok {
		return nil, errNoApprovalChannel
	}
	return gw.ListApprovals(ctx)
}
