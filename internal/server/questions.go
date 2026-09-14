package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/suanova/cubepilot/internal/openclaw/ws"
	agentruntime "github.com/suanova/cubepilot/internal/runtime"
)

// Ask-user questions (issue #161). When the agent calls OpenClaw's ask_user
// tool the turn parks on the gateway until a human answers. The gateway
// broadcasts question.requested / question.resolved over the per-user device
// connection; this file relays them onto the parked turn's SSE stream as
// question_pending / question_resolved and resolves the Portal's answer back
// with question.resolve.
//
// Unlike approvals there is no pending registry: the gateway is the single
// source of truth for whether a question is still open, and every authoritative
// check (session binding, pending, expiry) is answered by question.get. The one
// piece of state kept here is a routing table, because the resolved broadcast
// carries no session key -- see questionRoutes.

// questionRoutes maps a gateway question id to the session whose SSE stream its
// events belong to. It exists because question.resolved carries only {id,
// status}: the gateway's resolved schema is a closed union with no sessionKey,
// so a stateless relay would have no way to address the event. Entries are
// written when a question is relayed or recovered, and dropped once it
// resolves; the table is bounded by the number of open questions.
//
// This is a routing table, not a copy of gateway state: it stores no answers
// and never decides whether a question may be answered.
type questionRoutes struct {
	mu sync.Mutex
	m  map[string]string
}

func newQuestionRoutes() *questionRoutes { return &questionRoutes{m: map[string]string{}} }

func (r *questionRoutes) put(id, sessionKey string) {
	if id == "" || sessionKey == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[id] = sessionKey
}

// take returns and forgets the session for a question id.
func (r *questionRoutes) take(id string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key, ok := r.m[id]
	if ok {
		delete(r.m, id)
	}
	return key, ok
}

// unsupportedQuestionReason returns "" when the Portal can render a question
// record as a choice card, or a short reason when it cannot. Callers log the
// reason, so a silently dropped question is diagnosable.
//
// The admin device connection receives every question.* event on the gateway,
// not only the ones ask_user produced, so a record from another producer must
// be filtered out rather than rendered as an ordinary choice prompt.
//
// Note what is NOT a reason to drop a record: isOther. It reads like "the human
// may answer something else", which sounds like an unsupported variant, but
// ask_user's own normalizer stamps isOther:true on every question it emits
// (ask-user-tool-normalization.ts) -- it is how the tool declares that free
// text is offered alongside the options, so treating it as unsupported rejects
// the entire feature.
func unsupportedQuestionReason(rec ws.QuestionRecord) string {
	if len(rec.Questions) == 0 {
		return "record carries no questions"
	}
	for _, q := range rec.Questions {
		switch {
		case q.IsSecret:
			return "secret question"
		case len(q.SecretStore) > 0:
			return "secret-store question"
		case len(q.Options) == 0:
			// Free text only: nothing to render as buttons, and this version
			// offers no text input.
			return "free-text-only question"
		}
	}
	return ""
}

// questionExpired reports whether the gateway's deadline has passed. The
// gateway expires lazily on read, so a record read from question.list can still
// claim "pending" past its deadline.
func questionExpired(rec ws.QuestionRecord) bool {
	return rec.ExpiresAtMs > 0 && time.Now().After(time.UnixMilli(rec.ExpiresAtMs))
}

// remainingSeconds is the time left before a question expires, computed when
// the event is produced. Sending the remainder rather than the gateway's
// absolute deadline keeps the browser's countdown independent of clock drift
// between the gateway pod and the user's machine.
func remainingSeconds(expiresAtMs int64) int {
	if expiresAtMs <= 0 {
		return 0
	}
	d := time.Until(time.UnixMilli(expiresAtMs))
	if d < 0 {
		return 0
	}
	return int(d / time.Second)
}

// questionPrompt projects a question record onto the runtime-neutral prompt
// sent to the browser.
func questionPrompt(rec ws.QuestionRecord) *agentruntime.QuestionPrompt {
	items := make([]agentruntime.QuestionItem, 0, len(rec.Questions))
	for _, q := range rec.Questions {
		opts := make([]agentruntime.QuestionOption, 0, len(q.Options))
		for _, o := range q.Options {
			opts = append(opts, agentruntime.QuestionOption{Label: o.Label, Description: o.Description})
		}
		items = append(items, agentruntime.QuestionItem{
			QuestionID:  q.QuestionID,
			Header:      q.Header,
			Question:    q.Question,
			Options:     opts,
			MultiSelect: q.MultiSelect,
		})
	}
	return &agentruntime.QuestionPrompt{
		Questions:      items,
		TimeoutSeconds: remainingSeconds(rec.ExpiresAtMs),
	}
}

// relayQuestionRequested forwards a pending question onto its session's SSE
// stream and remembers the route for its resolution.
func (s *Server) relayQuestionRequested(rec ws.QuestionRecord) {
	if rec.SessionKey == "" {
		s.logf("question %s: dropped: record carries no session key", rec.ID)
		return
	}
	if reason := unsupportedQuestionReason(rec); reason != "" {
		s.logf("question %s: not projected: %s (session %s)", rec.ID, reason, rec.SessionKey)
		return
	}
	s.qroutes.put(rec.ID, rec.SessionKey)
	// No open stream means no browser is watching this session: the question
	// reaches the Portal only through reload recovery. Say so -- a silently
	// dropped push is what made issue #167 hard to see.
	if !s.hub.PublishTo(rec.SessionKey, agentruntime.Event{
		Type:      agentruntime.EventQuestionPending,
		SessionID: rec.SessionKey,
		CallID:    rec.ID,
		Question:  questionPrompt(rec),
	}) {
		s.logf("question %s: no open stream for session %s; push dropped", rec.ID, rec.SessionKey)
	}
}

// relayQuestionResolved settles a question card. The broadcast carries no
// session key, so the route recorded when the question was relayed or recovered
// is what addresses it; an unknown id is logged and dropped (reachable only
// when a card survived the process that relayed it).
func (s *Server) relayQuestionResolved(res ws.QuestionResolved) {
	sessionKey, ok := s.qroutes.take(res.ID)
	if !ok {
		s.logf("question %s: resolved (%s) with no known session; not forwarded", res.ID, res.Status)
		return
	}
	if !s.hub.PublishTo(sessionKey, agentruntime.Event{
		Type:      agentruntime.EventQuestionResolved,
		SessionID: sessionKey,
		CallID:    res.ID,
		Message:   res.Status, // answered | cancelled | expired
	}) {
		s.logf("question %s: no open stream for session %s; resolution not delivered", res.ID, sessionKey)
	}
}

// questionEntry is one pending question served to the Portal for reload
// recovery. It mirrors the question_pending SSE payload so the browser renders
// a restored card with the same code path.
type questionEntry struct {
	ID             string                      `json:"id"`
	Questions      []agentruntime.QuestionItem `json:"questions"`
	TimeoutSeconds int                         `json:"timeoutSeconds,omitempty"`
}

// handleQuestion serves POST /api/sessions/{key}/question -- the human's answer
// (or dismissal) for a pending question on this session.
func (s *Server) handleQuestion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST required"})
		return
	}
	user := s.userOf(r)
	sessionKey := canonicalSessionKey(subresourceKey(r.URL.Path, "/question"))
	if sessionKey == "" || sessionKey == "agent:main:" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing session key"})
		return
	}
	if s.hitl == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "question channel unavailable"})
		return
	}
	var body struct {
		ID      string              `json:"id"`
		Answers map[string][]string `json:"answers"`
		Cancel  bool                `json:"cancel"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body"})
		return
	}
	if body.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id required"})
		return
	}
	if body.Cancel == (len(body.Answers) > 0) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "send either answers or cancel"})
		return
	}
	ctx := r.Context()
	// Bind the id to this session before acting on it. A stale card (or any
	// other client holding an id) must not answer a question belonging to a
	// different session, and only a question the gateway still considers open
	// may be answered.
	rec, err := s.hitl.GetQuestion(ctx, user, body.ID)
	switch {
	case errors.Is(err, errNoQuestionChannel):
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "question channel unavailable"})
		return
	case err != nil:
		s.writeQuestionGatewayError(w, user, "question "+body.ID, err)
		return
	}
	if canonicalSessionKey(rec.SessionKey) != sessionKey {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such pending question for this session"})
		return
	}
	if rec.Status != "pending" || questionExpired(*rec) {
		// The gateway may have expired it between the card being painted and the
		// click; the Portal re-syncs the card from this status.
		writeJSON(w, http.StatusConflict, map[string]any{"error": "question is no longer pending"})
		return
	}
	if body.Cancel {
		err = s.hitl.CancelQuestion(ctx, user, body.ID)
	} else {
		err = s.hitl.ResolveQuestion(ctx, user, body.ID, body.Answers)
	}
	if err != nil {
		s.writeQuestionGatewayError(w, user, "question "+body.ID, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"question_id": body.ID, "cancelled": body.Cancel})
}

// handlePendingQuestion serves GET /api/sessions/{key}/question/pending --
// used to restore a question card after a Portal reload mid-question. It
// answers 404 when the session has no open question, which is also what a
// session with no live channel reports: an open question only ever exists
// alongside the live turn that is parked on it.
func (s *Server) handlePendingQuestion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET required"})
		return
	}
	user := s.userOf(r)
	sessionKey := canonicalSessionKey(subresourceKey(r.URL.Path, "/question/pending"))
	if sessionKey == "" || sessionKey == "agent:main:" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing session key"})
		return
	}
	noPending := func() {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no pending question"})
	}
	if s.hitl == nil {
		noPending()
		return
	}
	list, err := s.hitl.ListQuestions(r.Context(), user)
	switch {
	case errors.Is(err, errNoQuestionChannel):
		noPending()
		return
	case err != nil:
		s.logf("pending question %s/%s: %v", user, sessionKey, err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	// question.list gateway order is oldest first; keep it so a session with
	// several open questions restores its cards in the order they were asked.
	out := []questionEntry{}
	for _, rec := range list {
		if canonicalSessionKey(rec.SessionKey) != sessionKey || rec.Status != "pending" ||
			questionExpired(rec) || unsupportedQuestionReason(rec) != "" {
			continue
		}
		// Recovery is also how this process learns the id -> session route for a
		// question it did not itself relay (e.g. restored after a restart), so a
		// later question.resolved can still address its card.
		s.qroutes.put(rec.ID, rec.SessionKey)
		out = append(out, questionEntry{
			ID:             rec.ID,
			Questions:      questionPrompt(rec).Questions,
			TimeoutSeconds: remainingSeconds(rec.ExpiresAtMs),
		})
	}
	if len(out) == 0 {
		noPending()
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"questions": out})
}

// writeQuestionGatewayError maps a gateway failure onto the Portal status. The
// structured reason is what distinguishes a question that expired between paint
// and click (409) from an answer the gateway rejected (400) or a transport
// failure (502).
func (s *Server) writeQuestionGatewayError(w http.ResponseWriter, user, what string, err error) {
	status := questionErrorStatus(ws.ReasonOf(err))
	if status == http.StatusBadGateway {
		s.logf("%s %s: %v", what, user, err)
	}
	writeJSON(w, status, map[string]any{"error": err.Error()})
}

// questionErrorStatus maps a gateway question error reason onto the Portal
// status. The structured reason is what separates a question that expired
// between the card being painted and the click from an answer the gateway
// rejected or a plain transport failure.
func questionErrorStatus(reason string) int {
	switch reason {
	case "QUESTION_NOT_FOUND":
		return http.StatusNotFound
	case "QUESTION_ALREADY_TERMINAL":
		return http.StatusConflict
	case "QUESTION_INVALID_ANSWER":
		return http.StatusBadRequest
	default:
		return http.StatusBadGateway
	}
}

// subresourceKey extracts the session key from a subresource path
// (/api/sessions/{key}<suffix>).
func subresourceKey(path, suffix string) string {
	key := strings.TrimSuffix(strings.TrimPrefix(path, "/api/sessions/"), suffix)
	return strings.Trim(key, "/")
}

// errNoQuestionChannel reports that the user has no live gateway connection, so
// there is nothing to ask about or answer. Answering and listing deliberately
// use the user's existing connection rather than dialing a new one: opening a
// connection can trigger a device pairing, which a read-only status request
// must never do as a side effect.
var errNoQuestionChannel = errors.New("question channel unavailable")
