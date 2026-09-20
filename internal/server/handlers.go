package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/suanova/cubepilot/internal/audit"
	"github.com/suanova/cubepilot/internal/metrics"
	"github.com/suanova/cubepilot/internal/openclaw"
	"github.com/suanova/cubepilot/internal/openclaw/ws"
	agentruntime "github.com/suanova/cubepilot/internal/runtime"
)

// agentMainKey is the agent id an OpenAI-http run resolves to (openclaw/default
// -> "main"); used to canonicalize session keys (issue #20).
const agentMainKey = "main"

// canonicalSessionKey maps a platform session key to the form the gateway uses
// internally (agent:<agentId>:<segment>). Approval events carry the canonical
// key, so the SSE hub, ledger, x-openclaw-session-key, the echoed sessionId
// and /approval must all use the same canonical form or live confirmation cards
// never reach the initiating chat stream.
func canonicalSessionKey(key string) string {
	if strings.HasPrefix(key, "agent:") {
		return key
	}
	return "agent:" + agentMainKey + ":" + key
}

// hasSessionKey reports whether a canonicalised key names a conversation at all.
//
// It exists so the subresource handlers test one thing instead of spelling out
// what canonicalSessionKey("") happens to produce: that sentinel is a consequence
// of the canonical form, and a handler that hard-codes it keeps passing when the
// form changes -- with an empty key, which addresses no conversation.
func hasSessionKey(key string) bool {
	return key != "" && key != "agent:"+agentMainKey+":"
}

// userOf resolves the operator identity for a request (phase one has no auth;
// the Portal supplies it via header, falling back to the configured default).
func (s *Server) userOf(r *http.Request) string {
	if u := r.Header.Get("X-CubePilot-User"); u != "" {
		return u
	}
	return s.cfg.DefaultUser
}

// agentRuntimeFor composes the OpenClaw implementation's HTTP and WebSocket
// surfaces behind CubePilot's complete runtime-neutral contract. A future
// runtime replaces this construction without changing the handlers.
func (s *Server) agentRuntimeFor(user string) agentruntime.AgentRuntime {
	httpClient := openclaw.New(s.mgr.BaseURL(user), s.cfg.GatewayToken)
	live := &openClawLiveRunner{manager: s.gatewayConns, user: user}
	return agentruntime.Compose(live, httpClient, httpClient)
}

// sessionReaderFor returns the read-only runtime surface for session metadata
// and history. Model selection never affects these endpoints.
func (s *Server) sessionReaderFor(user string) agentruntime.SessionReader {
	return s.agentRuntimeFor(user)
}

// handleSessions lists the OpenClaw sessions for the current user.
func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET required"})
		return
	}
	user := s.userOf(r)
	if err := s.mgr.Ensure(r.Context(), user); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": fmt.Sprintf("instance warming failed: %v", err)})
		return
	}
	client := s.sessionReaderFor(user)
	sessions, err := client.ListSessions(r.Context())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

// handleHistory returns the raw session history for /api/sessions/{key}/messages.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET required"})
		return
	}
	user := s.userOf(r)
	sessionKey := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, apiPrefix+"/sessions/"), "/messages")
	sessionKey = strings.Trim(sessionKey, "/")
	if sessionKey == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing session key"})
		return
	}
	if err := s.mgr.Ensure(r.Context(), user); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": fmt.Sprintf("instance warming failed: %v", err)})
		return
	}
	client := s.sessionReaderFor(user)
	history, err := client.GetHistory(r.Context(), sessionKey, 200)
	if err != nil {
		// A session that does not exist yet is answered with its own status, not
		// folded into the 502 below. A conversation is created by its first
		// message, so a caller that reads history before that is asking about an
		// unstarted conversation -- and a client that cannot tell that from an
		// unreachable runtime renders an empty thread for an outage, which looks
		// to the user like their history was erased. (issue #30)
		if errors.Is(err, agentruntime.ErrSessionNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such session"})
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(history)
}

// handleMessages streams one chat turn to the client as CubePilot SSE events.
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST required"})
		return
	}
	var body struct {
		SessionID string `json:"sessionId"`
		Content   string `json:"content"`
	}
	if !decodeJSONBody(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Content) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "content required"})
		return
	}

	sessionKey := body.SessionID
	if sessionKey == "" {
		sessionKey = "conv-" + uuid.NewString()
	}
	// Use the gateway's canonical form (agent:main:<segment>) for everything so
	// approval events (which carry it) route to the same stream (issue #20).
	sessionKey = canonicalSessionKey(sessionKey)
	user := s.userOf(r)

	metrics.Inc("cubepilot_messages_total", "role=user", 1)
	metrics.Inc("cubepilot_sessions_total", "", 1)
	started := time.Now()
	firstToken := time.Time{}
	var streamErr error

	// Open the per-session SSE stream BEFORE writing response headers so a
	// conflicting concurrent turn for the same session can be answered 409 as
	// JSON. All writes (turn events, WS-originated confirm_* events, heartbeats)
	// go through the single writer inside the Stream.
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "streaming unsupported"})
		return
	}
	stream, oerr := s.hub.Open(sessionKey, w, flusher)
	if oerr != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "another turn is already streaming for this session"})
		return
	}
	defer stream.Close()
	stream.Start()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	// Gateway WS-only chat (issue #130): the turn is driven and observed over
	// the per-user gateway device connection. sessions.send starts the run and
	// the subscribed session-message stream projects text / tools / done into
	// the SSE as one ordered feed -- no OpenAI-compat HTTP run, no transcript
	// polling, no end-of-run drain. emitLive is the single write path for both
	// the handler and the WS read goroutine (Stream.Send is concurrency-safe).
	emitLive := func(ev agentruntime.Event) error {
		s.recordToolCall(user, ev)
		if ev.Type == agentruntime.EventMessageDelta && firstToken.IsZero() {
			firstToken = time.Now()
			metrics.ObserveFirstToken(firstToken.Sub(started).Milliseconds())
		}
		return stream.Send(ev) // write error aborts the turn, like the old direct write
	}

	// Announce the session up front so the client can track the conversation.
	_ = emitLive(agentruntime.Event{Type: agentruntime.EventMessageStart, SessionID: sessionKey})
	_ = emitLive(agentruntime.Event{Type: agentruntime.EventAgentThinking, SessionID: sessionKey})

	// Ensure the instance is running; this may cold-start the Pod.
	if err := s.mgr.Ensure(r.Context(), user); err != nil {
		s.logf("chat %s: %s: turn ended: instance warming failed: %v", user, sessionKey, err)
		_ = stream.Send(agentruntime.Event{Type: agentruntime.EventMessageDone, SessionID: sessionKey, Error: fmt.Sprintf("instance warming failed: %v", err)})
		return
	}

	// Resolve the model before starting the runtime-neutral live turn. The
	// OpenClaw implementation drives it over WS and includes native confirmation
	// gating; another runtime can provide the same semantics using another
	// transport without changing this handler.
	var runErr error
	var selectedModel string
	if model, cerr := s.mgr.SelectedModelFor(r.Context(), user); cerr != nil {
		runErr = cerr
		s.logf("model resolution for %s: %v", user, cerr)
	} else {
		selectedModel = model
	}
	runtimeAdapter := s.agentRuntimeFor(user)

	// Drive the whole turn over the WebSocket: RunLiveTurn subscribes the
	// session, sends the message, and returns when the run is terminal (all
	// text/tool events were already streamed via emitLive as they arrived).
	if runErr != nil {
		streamErr = runErr
		_ = stream.Send(liveTurnDone(sessionKey, agentruntime.TurnOutcome{}, runErr))
	} else if outcome, err := runtimeAdapter.RunLiveTurn(r.Context(), sessionKey, agentruntime.LiveTurnParams{
		Message: body.Content,
		Model:   selectedModel,
	}, emitLive); err != nil {
		streamErr = err
		_ = stream.Send(liveTurnDone(sessionKey, agentruntime.TurnOutcome{}, err))
	} else {
		_ = stream.Send(liveTurnDone(sessionKey, outcome, nil))
	}
	metrics.ObserveTurn(time.Since(started).Milliseconds())
	if streamErr != nil {
		metrics.Inc("cubepilot_turns_total", "status=failed", 1)
	} else {
		metrics.Inc("cubepilot_turns_total", "status=ok", 1)
	}
	// Say how the turn ended. A turn whose browser went away is otherwise
	// invisible server-side: the run keeps going gateway-side while nothing
	// observes it, and losing the stream is what leaves a parked question or
	// confirmation with no way to deliver its continuation (issue #167).
	took := time.Since(started).Round(time.Millisecond)
	switch {
	case streamErr == nil:
		s.logf("chat %s: %s: turn done in %s", user, sessionKey, took)
	case r.Context().Err() != nil:
		s.logf("chat %s: %s: turn ended after %s: request context cancelled (%v)", user, sessionKey, took, r.Context().Err())
	default:
		s.logf("chat %s: %s: turn ended after %s: %v", user, sessionKey, took, streamErr)
	}
}

// liveTurnDone builds the single terminal SSE event for a finished live turn.
// message_done is the only terminal event, and stopped and error are mutually
// exclusive: a turn the user stopped is terminal but not a failure, so it
// carries stopped=true and no error; a turn that failed carries its diagnostic
// and no stopped flag. Keeping the choice in one place is what makes "a stop is
// not a completion" verifiable without driving the whole HTTP handler.
func liveTurnDone(sessionKey string, outcome agentruntime.TurnOutcome, err error) agentruntime.Event {
	ev := agentruntime.Event{Type: agentruntime.EventMessageDone, SessionID: sessionKey}
	switch {
	case err != nil:
		ev.Error = err.Error()
	case outcome.Stopped:
		ev.Stopped = true
	}
	return ev
}

// recordToolCall writes an M5 audit entry for each observed tool_call event.
func (s *Server) recordToolCall(user string, ev agentruntime.Event) {
	if ev.Type != agentruntime.EventToolCall || s.store == nil {
		return
	}
	entry := audit.Entry(user, ev.SessionID, ev.Name, ev.Arguments)
	entry.TS = time.Now()
	if err := s.store.AddAudit(entry); err != nil {
		// audit must never break the chat stream
		_ = err
	}
	metrics.Inc("cubepilot_tool_calls_total", "level="+entry.Level, 1)
}

func writeSSE(w http.ResponseWriter, ev agentruntime.Event) error {
	if _, err := fmt.Fprintf(w, "event: %s\n", ev.Type); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", ev.Marshal()); err != nil {
		return err
	}
	return nil
}

// decodeJSONBody reads a JSON request body into v, rejecting unknown fields.
//
// Every handler that accepts a body goes through here. encoding/json otherwise
// ignores keys it does not know, so a misspelled field -- or a payload in a
// shape this API used to use -- decodes to a zero value and the handler acts on
// it. For a PUT that overwrites stored state that is a silent wipe: the caller
// gets 200 and their configuration is gone.
//
// Strictness is affordable here precisely because v1 is unreleased and carries
// no forward-compatibility obligation: there is no client that legitimately
// sends fields this API does not know. It returns false after writing the 400,
// so callers read:
//
//	var body x
//	if !decodeJSONBody(w, r, &body) {
//		return
//	}
func decodeJSONBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad JSON body"})
		return false
	}
	return true
}

// writeNotFound answers an unresolvable path with the same JSON error shape as
// every other response. http.NotFound would emit Go's plain-text
// "404 page not found", which a client that parses {"error": ...} cannot read,
// forcing it to handle two error formats for one status code.
func writeNotFound(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusNotFound, map[string]any{"error": msg})
}

// writeGatewayError maps a failed gateway call onto the Portal status, through
// the caller's own reason table.
//
// The structured reason is what separates an outcome from a failure: an approval
// or question that expired, was answered elsewhere, or was rejected as
// unanswerable is something the client acts on (close the card, re-sync it),
// while a transport failure is a 502 it may retry. Only the table differs between
// the two HITL surfaces, so only the table is passed in -- the mapping, the
// log-only-on-502 rule and the response shape are the same call and live here.
func (s *Server) writeGatewayError(w http.ResponseWriter, user, what string, err error, statusFor func(reason string) int) {
	status := statusFor(ws.ReasonOf(err))
	if status == http.StatusBadGateway {
		s.logf("%s %s: %v", what, user, err)
	}
	writeJSON(w, status, map[string]any{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
