package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/suanova/cubepilot/internal/audit"
	"github.com/suanova/cubepilot/internal/inspect"
	"github.com/suanova/cubepilot/internal/metrics"
	"github.com/suanova/cubepilot/internal/openclaw"
	agentruntime "github.com/suanova/cubepilot/internal/runtime"
)

// agentMainKey is the agent id an OpenAI-http run resolves to (openclaw/default
// -> "main"); used to canonicalize session keys (issue #20).
const agentMainKey = "main"

// canonicalSessionKey maps a platform session key to the form the gateway uses
// internally (agent:<agentId>:<segment>). Approval events carry the canonical
// key, so the SSE hub, ledger, x-openclaw-session-key, the echoed session_id
// and /confirm must all use the same canonical form or live confirmation cards
// never reach the initiating chat stream.
func canonicalSessionKey(key string) string {
	if strings.HasPrefix(key, "agent:") {
		return key
	}
	return "agent:" + agentMainKey + ":" + key
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
	live := &openClawLiveRunner{manager: s.hitl, user: user}
	return agentruntime.Compose(live, httpClient, httpClient)
}

// sessionReaderFor returns the read-only runtime surface for session metadata
// and history. Model selection never affects these endpoints.
func (s *Server) sessionReaderFor(user string) agentruntime.SessionReader {
	return s.agentRuntimeFor(user)
}

// oneShotRunnerFor returns the HTTP adapter for non-interactive turns, with the
// selected model applied as a per-request override. Interactive Portal chat
// must use RunLiveTurn and the gateway WebSocket protocol instead.
func (s *Server) oneShotRunnerFor(ctx context.Context, user string) (agentruntime.OneShotRunner, error) {
	rt := s.agentRuntimeFor(user)
	if model, err := s.mgr.SelectedModelFor(ctx, user); err != nil {
		return rt, err
	} else if model != "" {
		rt.SetModel(model)
	}
	return rt, nil
}

// handleSessions lists the OpenClaw sessions for the current user.
func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
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
	user := s.userOf(r)
	sessionKey := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/sessions/"), "/messages")
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
		SessionID string `json:"session_id"`
		Content   string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Content) == "" {
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
		_ = stream.Send(agentruntime.Event{Type: agentruntime.EventMessageDone, SessionID: sessionKey, Error: runErr.Error()})
	} else if err := runtimeAdapter.RunLiveTurn(r.Context(), sessionKey, agentruntime.LiveTurnParams{
		Message: body.Content,
		Model:   selectedModel,
	}, emitLive); err != nil {
		streamErr = err
		_ = stream.Send(agentruntime.Event{Type: agentruntime.EventMessageDone, SessionID: sessionKey, Error: err.Error()})
	} else {
		_ = stream.Send(agentruntime.Event{Type: agentruntime.EventMessageDone, SessionID: sessionKey})
	}
	metrics.ObserveTurn(time.Since(started).Milliseconds())
	if streamErr != nil {
		metrics.Inc("cubepilot_turns_total", "status=failed", 1)
	} else {
		metrics.Inc("cubepilot_turns_total", "status=ok", 1)
	}
}

// extractToolEvents reconstructs tool_call / tool_result events for a just
// finished turn from the session transcript, and records them for audit (M5).
// The gateway's /v1/chat/completions stream does not expose tool calls, so the
// transcript is the authoritative source. Retries briefly to let the gateway
// flush the transcript file after the turn ends.
func (s *Server) extractToolEvents(ctx context.Context, user, sessionKey string, seen map[string]bool) []agentruntime.Event {
	var out []agentruntime.Event
	for attempt := 0; attempt < 4; attempt++ {
		client := s.sessionReaderFor(user)
		raw, err := client.GetHistory(ctx, sessionKey, 50)
		if err == nil {
			for _, ev := range parseHistoryTools(sessionKey, raw) {
				key := ev.Type + ":" + ev.CallID
				if seen[key] {
					continue
				}
				seen[key] = true
				out = append(out, ev)
			}
			if len(out) > 0 {
				break
			}
		}
		select {
		case <-time.After(500 * time.Millisecond):
		case <-ctx.Done():
			return nil
		}
	}
	return out
}

type historyItem struct {
	Role    string `json:"role"`
	Content []struct {
		Type      string          `json:"type"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Text      string          `json:"text"`
	} `json:"content"`
}

func parseHistoryTools(sessionKey string, raw []byte) []agentruntime.Event {
	var h struct {
		Items []historyItem `json:"items"`
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		return nil
	}
	// Pair tool calls with their results by linear order: an assistant item
	// carries N toolCall content blocks, and the following toolResult item
	// carries N text blocks (the gateway transcript shape). The queue maps
	// each result text back to the call that produced it, so events carry the
	// real tool name and a stable callID (used for dedup on the SSE stream).
	type pendingCall struct {
		id, name string
		args     json.RawMessage
	}
	var out []agentruntime.Event
	var pending []pendingCall
	for _, it := range h.Items {
		for _, c := range it.Content {
			switch c.Type {
			case "toolCall":
				pending = append(pending, pendingCall{id: c.ID, name: c.Name, args: c.Arguments})
				out = append(out, agentruntime.Event{
					Type:      agentruntime.EventToolCall,
					SessionID: sessionKey,
					Name:      c.Name,
					CallID:    c.ID,
					Arguments: string(c.Arguments),
				})
			case "toolResult":
				out = append(out, agentruntime.Event{
					Type:      agentruntime.EventToolResult,
					SessionID: sessionKey,
					Name:      "exec",
					Output:    c.Text,
				})
			case "text":
				// toolResult items carry their output as plain text blocks;
				// pair each with the oldest unmatched tool call.
				if it.Role == "toolResult" && len(pending) > 0 {
					pc := pending[0]
					pending = pending[1:]
					out = append(out, agentruntime.Event{
						Type:      agentruntime.EventToolResult,
						SessionID: sessionKey,
						Name:      pc.name,
						CallID:    pc.id,
						Output:    c.Text,
					})
				}
			}
		}
	}
	return out
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

// handleInspect runs a basic cluster inspection and returns the report as JSON.
// The run's tool calls are still recorded to the caller's audit ledger; the
// report body itself is only returned inline (scheduled-task reports are TaskRun
// CRs). Run the inspection with the creator's identity and the creator's
// permissions (design §5.4 authorization contract): read-only is enforced by the
// inspection template's behavior plus RBAC as the backstop; a dedicated
// read-only instance is no longer used.
func (s *Server) handleInspect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST required"})
		return
	}
	user := s.userOf(r)
	if err := s.mgr.Ensure(r.Context(), user); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": fmt.Sprintf("instance warming failed: %v", err)})
		return
	}
	sessionKey := "inspect-" + uuid.NewString()[:8]
	client, cerr := s.oneShotRunnerFor(r.Context(), user)
	if cerr != nil {
		// Fail-closed: an inspection must run with the user's selected model,
		// never silently with a different one.
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": cerr.Error()})
		return
	}
	content, err := inspect.Run(r.Context(), client, sessionKey, func(ev agentruntime.Event) {
		s.recordToolCall(user, ev)
	})
	// Tool calls are not on the stream; replay the transcript for audit.
	for _, ev := range s.extractToolEvents(r.Context(), user, sessionKey, map[string]bool{}) {
		if ev.Type == agentruntime.EventToolCall {
			s.recordToolCall(user, ev)
		}
	}
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"report": content})
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

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
