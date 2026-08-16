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
	"github.com/suanova/cubepilot/internal/openclaw"
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

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "streaming unsupported"})
		return
	}
	var doneEvent *openclaw.Event
	emit := func(ev openclaw.Event) error {
		// The gateway's chat-completions stream only carries final text; hold
		// message_done so we can first replay tool events extracted from the
		// session history (see extractToolEvents) before the turn ends.
		if ev.Type == openclaw.EventMessageDone {
			doneEvent = &ev
			return nil
		}
		writeSSE(w, ev)
		flusher.Flush()
		return nil
	}

	// Announce the session up front so the client can track the conversation.
	_ = emit(openclaw.Event{Type: openclaw.EventMessageStart, SessionID: sessionKey})
	_ = emit(openclaw.Event{Type: openclaw.EventAgentThinking, SessionID: sessionKey})

	// Ensure the instance is running; this may cold-start the Pod ("正在唤醒助手…").
	if err := s.mgr.Ensure(r.Context(), user); err != nil {
		_ = emit(openclaw.Event{Type: openclaw.EventMessageDone, SessionID: sessionKey, Error: fmt.Sprintf("instance warming failed: %v", err)})
		return
	}

	messages := []openclaw.ChatMessage{{Role: "user", Content: body.Content}}
	if cfg, err := s.store.GetAgentConfig(); err == nil && strings.TrimSpace(cfg.SystemPrompt) != "" {
		messages = append([]openclaw.ChatMessage{{Role: "system", Content: cfg.SystemPrompt}}, messages...)
	}
	_ = s.clientFor(user).StreamChat(r.Context(), openclaw.ChatParams{
		SessionKey: sessionKey,
		Messages:   messages,
	}, emit)

	for _, ev := range s.extractToolEvents(r.Context(), user, sessionKey) {
		writeSSE(w, ev)
		flusher.Flush()
	}
	if doneEvent != nil {
		writeSSE(w, *doneEvent)
		flusher.Flush()
	}
}

// extractToolEvents reconstructs tool_call / tool_result events for a just
// finished turn from the session transcript, and records them for audit (M5).
// The gateway's /v1/chat/completions stream does not expose tool calls, so the
// transcript is the authoritative source. Retries briefly to let the gateway
// flush the transcript file after the turn ends.
func (s *Server) extractToolEvents(ctx context.Context, user, sessionKey string) []openclaw.Event {
	var out []openclaw.Event
	for attempt := 0; attempt < 4; attempt++ {
		raw, err := s.clientFor(user).GetHistory(ctx, sessionKey, 50)
		if err == nil {
			out = parseHistoryTools(sessionKey, raw)
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
	for i := range out {
		if out[i].Type == openclaw.EventToolCall {
			s.recordToolCall(user, out[i])
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
	var out []openclaw.Event
	for _, it := range h.Items {
		for _, c := range it.Content {
			switch c.Type {
			case "toolCall":
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
}

// handleInspect runs a basic cluster inspection and returns the report as JSON.
// The run is also persisted as a report (taskID "inspect") with audit entries.
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
	s.extractToolEvents(r.Context(), user, sessionKey)
	report, _ := s.store.AddReport(storeReport("inspect", "手动巡检", "inspect", started, content, err))
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
