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

	"github.com/google/uuid"

	"github.com/suanova/cubepilot/internal/audit"
	"github.com/suanova/cubepilot/internal/inspect"
	"github.com/suanova/cubepilot/internal/metrics"
	"github.com/suanova/cubepilot/internal/openclaw"
	"github.com/suanova/cubepilot/internal/store"
)

// userOf resolves the operator identity for a request (phase one has no auth;
// the Portal supplies it via header, falling back to the configured default).
func (s *Server) userOf(r *http.Request) string {
	if u := r.Header.Get("X-CubePilot-User"); u != "" {
		return u
	}
	return s.cfg.DefaultUser
}

func (s *Server) clientFor(user string) *openclaw.Client {
	return openclaw.New(s.mgr.BaseURL(user), s.cfg.GatewayToken)
}

// handleSessions lists the OpenClaw sessions for the current user.
func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	user := s.userOf(r)
	if err := s.mgr.Ensure(r.Context(), user); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": fmt.Sprintf("instance warming failed: %v", err)})
		return
	}
	sessions, err := s.clientFor(user).ListSessions(r.Context())
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
	history, err := s.clientFor(user).GetHistory(r.Context(), sessionKey, 200)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(history)
}

// handleLedger serves GET /api/sessions/{key}/ledger — the platform-side
// message ledger rows for a conversation (design §4.1: the platform is the
// source of truth for the session). This is the
// authoritative history for rendering and cross-runtime recovery; it does not
// require the agent instance to be alive.
func (s *Server) handleLedger(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET required"})
		return
	}
	sessionKey := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/sessions/"), "/ledger")
	sessionKey = strings.Trim(sessionKey, "/")
	if sessionKey == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing session key"})
		return
	}
	msgs, err := s.store.ListMessages(sessionKey, 0)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"conversationId": sessionKey, "messages": msgs})
}

// handleSeed serves POST /api/sessions/{key}/seed — re-seeds a new runtime
// session from the platform ledger (design §4.1 runtime swap re-attach: the
// platform ledger replays recent messages as the new runtime's session
// context). The assistant service replays ledger
// rows as chat messages to the agent gateway, so a fresh runtime (or a rebuilt
// instance) can continue the conversation from the platform's source of truth.
func (s *Server) handleSeed(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST required"})
		return
	}
	user := s.userOf(r)
	sessionKey := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/sessions/"), "/seed")
	sessionKey = strings.Trim(sessionKey, "/")
	if sessionKey == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing session key"})
		return
	}
	msgs, err := s.store.ListMessages(sessionKey, 50)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if len(msgs) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"seeded": 0})
		return
	}
	if err := s.mgr.Ensure(r.Context(), user); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": fmt.Sprintf("instance warming failed: %v", err)})
		return
	}
	// Replay the ledger as an ordered chat history to the runtime session.
	var chat []openclaw.ChatMessage
	for _, m := range msgs {
		switch m.Role {
		case "user":
			chat = append(chat, openclaw.ChatMessage{Role: "user", Content: m.Content})
		case "assistant":
			chat = append(chat, openclaw.ChatMessage{Role: "assistant", Content: m.Content})
		}
	}
	if len(chat) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"seeded": 0})
		return
	}
	err = s.clientFor(user).StreamChat(r.Context(), openclaw.ChatParams{
		SessionKey: sessionKey,
		Messages:   chat,
	}, func(openclaw.Event) error { return nil })
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"seeded": len(chat)})
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
	user := s.userOf(r)

	// Session source of truth (design §4.1): the platform ledger is the source
	// of truth for
	// message history. Record the user message up front so the turn is durable
	// even if the runtime stream fails mid-way (marked incomplete on done).
	if s.store != nil {
		_, _ = s.store.AppendMessage(store.Message{
			ConversationID: sessionKey,
			User:           user,
			Role:           "user",
			Content:        body.Content,
		})
	}

	metrics.Inc("cubepilot_messages_total", "role=user", 1)
	metrics.Inc("cubepilot_sessions_total", "", 1)
	started := time.Now()
	firstToken := time.Time{}
	var streamErr error

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "streaming unsupported"})
		return
	}
	var doneEvent *openclaw.Event
	var sseMu sync.Mutex
	emit := func(ev openclaw.Event) error {
		// The gateway's chat-completions stream only carries final text; hold
		// message_done so we can first replay tool events extracted from the
		// session history (see extractToolEvents) before the turn ends.
		if ev.Type == openclaw.EventMessageDone {
			evCopy := ev
			doneEvent = &evCopy
			if ev.Error != "" {
				streamErr = errors.New(ev.Error)
			}
			s.ledgerEvent(user, sessionKey, ev)
			return nil
		}
		s.ledgerEvent(user, sessionKey, ev)
		if ev.Type == openclaw.EventMessageDelta && firstToken.IsZero() {
			firstToken = time.Now()
			metrics.ObserveFirstToken(firstToken.Sub(started).Milliseconds())
		}
		sseMu.Lock()
		writeSSE(w, ev)
		flusher.Flush()
		sseMu.Unlock()
		return nil
	}

	// Announce the session up front so the client can track the conversation.
	_ = emit(openclaw.Event{Type: openclaw.EventMessageStart, SessionID: sessionKey})
	_ = emit(openclaw.Event{Type: openclaw.EventAgentThinking, SessionID: sessionKey})

	// Ensure the instance is running; this may cold-start the Pod.
	if err := s.mgr.Ensure(r.Context(), user); err != nil {
		_ = emit(openclaw.Event{Type: openclaw.EventMessageDone, SessionID: sessionKey, Error: fmt.Sprintf("instance warming failed: %v", err)})
		return
	}

	messages := []openclaw.ChatMessage{{Role: "user", Content: body.Content}}
	if cfg, err := s.store.GetAgentConfig(); err == nil && strings.TrimSpace(cfg.SystemPrompt) != "" {
		messages = append([]openclaw.ChatMessage{{Role: "system", Content: cfg.SystemPrompt}}, messages...)
	}
	// While the gateway stream runs, poll the transcript so tool activity
	// (tool_call / tool_result) reaches the client live instead of only after
	// the turn ends. Seen-set is shared with the final drain below, so every
	// event is emitted exactly once even if the poll and the drain overlap.
	seen := map[string]bool{}
	toolCtx, cancelTools := context.WithCancel(r.Context())
	defer cancelTools()
	toolDone := make(chan struct{})
	go func() {
		defer close(toolDone)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-toolCtx.Done():
				return
			case <-ticker.C:
				s.streamToolEvents(toolCtx, user, sessionKey, seen, func(ev openclaw.Event) {
					s.ledgerEvent(user, sessionKey, ev)
					sseMu.Lock()
					writeSSE(w, ev)
					flusher.Flush()
					sseMu.Unlock()
				})
			}
		}
	}()

	if err := s.clientFor(user).StreamChat(r.Context(), openclaw.ChatParams{
		SessionKey: sessionKey,
		Messages:   messages,
	}, emit); err != nil {
		streamErr = err
	}
	// Stop the poller, then drain whatever tool events appeared after the last
	// poll (or that the poller never saw because the stream ended quickly).
	cancelTools()
	<-toolDone
	for _, ev := range s.extractToolEvents(r.Context(), user, sessionKey, seen) {
		s.ledgerEvent(user, sessionKey, ev)
		sseMu.Lock()
		writeSSE(w, ev)
		flusher.Flush()
		sseMu.Unlock()
	}
	if doneEvent != nil {
		sseMu.Lock()
		writeSSE(w, *doneEvent)
		flusher.Flush()
		sseMu.Unlock()
	}
	// Terminate the ledger turn: mark the assistant row done (incomplete when
	// the stream failed / instance warming failed).
	if s.store != nil {
		errMsg := ""
		if streamErr != nil {
			errMsg = streamErr.Error()
		}
		_ = s.store.TurnEnd(sessionKey, errMsg)
	}
	metrics.ObserveTurn(time.Since(started).Milliseconds())
	if streamErr != nil {
		metrics.Inc("cubepilot_turns_total", "status=failed", 1)
	} else {
		metrics.Inc("cubepilot_turns_total", "status=ok", 1)
	}
}

// ledgerEvent writes one message-ledger row per SSE event flowing through the
// forwarding path (design §4.1 event capture on the stream, event-sourcing).
// Tool events are
// recorded for audit as well; user-facing deltas are coalesced into the
// assistant row (latest delta row is the terminal text).
func (s *Server) ledgerEvent(user, sessionKey string, ev openclaw.Event) {
	if s.store == nil {
		return
	}
	switch ev.Type {
	case openclaw.EventToolCall:
		s.recordToolCall(user, ev)
		args, _ := json.Marshal(ev.Arguments)
		_, _ = s.store.AppendMessage(store.Message{
			ConversationID: sessionKey,
			User:           user,
			Role:           "tool",
			EventType:      ev.Type,
			ToolName:       ev.Name,
			CallID:         ev.CallID,
			ToolCalls:      args,
		})
	case openclaw.EventToolResult:
		_, _ = s.store.AppendMessage(store.Message{
			ConversationID: sessionKey,
			User:           user,
			Role:           "tool",
			EventType:      ev.Type,
			ToolName:       ev.Name,
			Content:        ev.Output,
		})
	case openclaw.EventMessageDelta:
		_, _ = s.store.AppendMessage(store.Message{
			ConversationID: sessionKey,
			User:           user,
			Role:           "assistant",
			EventType:      ev.Type,
			Content:        ev.Delta,
		})
	case openclaw.EventMessageDone:
		// TurnEnd marks the assistant row terminal; nothing else to append.
	}
}

// streamToolEvents polls the gateway session transcript once and pushes any
// tool_call / tool_result events not yet seen. The gateway's chat-completions
// stream only carries text deltas (tool execution happens inside its agent
// loop), so the transcript is the only live source of tool activity. Dedup by
// type+callID keeps repeated polls idempotent on the SSE stream and in the
// audit log.
func (s *Server) streamToolEvents(ctx context.Context, user, sessionKey string, seen map[string]bool, push func(openclaw.Event)) {
	raw, err := s.clientFor(user).GetHistory(ctx, sessionKey, 50)
	if err != nil {
		return
	}
	for _, ev := range parseHistoryTools(sessionKey, raw) {
		key := ev.Type + ":" + ev.CallID
		if seen[key] {
			continue
		}
		seen[key] = true
		push(ev)
	}
}

// extractToolEvents reconstructs tool_call / tool_result events for a just
// finished turn from the session transcript, and records them for audit (M5).
// The gateway's /v1/chat/completions stream does not expose tool calls, so the
// transcript is the authoritative source. Retries briefly to let the gateway
// flush the transcript file after the turn ends.
func (s *Server) extractToolEvents(ctx context.Context, user, sessionKey string, seen map[string]bool) []openclaw.Event {
	var out []openclaw.Event
	for attempt := 0; attempt < 4; attempt++ {
		raw, err := s.clientFor(user).GetHistory(ctx, sessionKey, 50)
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

func parseHistoryTools(sessionKey string, raw []byte) []openclaw.Event {
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
	var out []openclaw.Event
	var pending []pendingCall
	for _, it := range h.Items {
		for _, c := range it.Content {
			switch c.Type {
			case "toolCall":
				pending = append(pending, pendingCall{id: c.ID, name: c.Name, args: c.Arguments})
				out = append(out, openclaw.Event{
					Type:      openclaw.EventToolCall,
					SessionID: sessionKey,
					Name:      c.Name,
					CallID:    c.ID,
					Arguments: string(c.Arguments),
				})
			case "toolResult":
				out = append(out, openclaw.Event{
					Type:      openclaw.EventToolResult,
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
					out = append(out, openclaw.Event{
						Type:      openclaw.EventToolResult,
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
func (s *Server) recordToolCall(user string, ev openclaw.Event) {
	if ev.Type != openclaw.EventToolCall || s.store == nil {
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
// The run is also persisted as a report (taskID "inspect") with audit entries.
// Run the inspection with the creator's identity and the creator's
// permissions (design §5.4 / FR-M4 authorization contract): read-only is
// enforced by the inspection template's behavior plus RBAC as the backstop;
// a dedicated read-only instance is no longer used.
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
	started := time.Now()
	content, err := inspect.Run(r.Context(), s.clientFor(user), sessionKey, func(ev openclaw.Event) {
		s.recordToolCall(user, ev)
	})
	// Tool calls are not on the stream; replay the transcript for audit.
	for _, ev := range s.extractToolEvents(r.Context(), user, sessionKey, map[string]bool{}) {
		if ev.Type == openclaw.EventToolCall {
			s.recordToolCall(user, ev)
		}
	}
	report, _ := s.store.AddReport(storeReport("inspect", "manual inspection", "inspect", started, content, err))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "report": report})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"report": content, "reportId": report.ID})
}

func writeSSE(w http.ResponseWriter, ev openclaw.Event) {
	_, _ = fmt.Fprintf(w, "event: %s\n", ev.Type)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", ev.Marshal())
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
