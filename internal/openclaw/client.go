// Package openclaw implements the HTTP side of CubePilot's OpenClaw adapter.
// Interactive Portal chat is intentionally driven by internal/openclaw/ws so
// text, tools, approvals, and completion share one ordered live stream. This
// client is reserved for read-only session APIs and one-shot background turns,
// where request/response HTTP semantics are the simpler and more stable fit.
package openclaw

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	agentruntime "github.com/suanova/cubepilot/internal/runtime"
)

var (
	_ agentruntime.OneShotRunner = (*Client)(nil)
	_ agentruntime.SessionReader = (*Client)(nil)
)

// Client is an authenticated HTTP client for one OpenClaw gateway.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New returns a Client for the given gateway base URL (e.g. http://agent-zhang.wei.cubepilot.svc:18789)
// and bearer token.
func New(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: 0}, // streaming responses are long-lived
	}
}

// ChatMessage is a single chat turn message.
type ChatMessage = agentruntime.ChatMessage

// ChatParams carries the inputs for one agent turn.
type ChatParams = agentruntime.ChatParams

// StreamChat POSTs a one-shot/background turn and invokes emit for each mapped
// CubePilot event as the OpenAI-compatible SSE stream is decoded. It always
// emits a terminal message_done event (even on error). Portal chat does not use
// this method; it runs over the gateway WebSocket protocol.
func (c *Client) StreamChat(ctx context.Context, p ChatParams, emit func(Event) error) error {
	// The request body carries the OpenClaw agent target. The runtime-neutral
	// Model field is the backend model override and is sent separately below.
	body, err := json.Marshal(map[string]any{
		"model":    "openclaw/default",
		"stream":   true,
		"messages": p.Messages,
	})
	if err != nil {
		return fmt.Errorf("marshal chat request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return err
	}
	c.applyAuth(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if p.SessionKey != "" {
		req.Header.Set("x-openclaw-session-key", p.SessionKey)
	}
	// Backend model override: the body keeps the agent target; x-openclaw-model
	// switches the backend provider/model for this turn (shared-secret callers
	// may use it directly, hot override without a restart).
	if p.Model != "" {
		req.Header.Set("x-openclaw-model", p.Model)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return c.fail(emit, fmt.Errorf("gateway request failed: %w", err))
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return c.fail(emit, fmt.Errorf("gateway returned %d: %s", resp.StatusCode, strings.TrimSpace(string(msg))))
	}

	m := newStreamMapper(p.SessionKey)
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var chunk ChatChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue // skip malformed keepalive/usage lines
		}
		// The gateway streams agent-run failures as an error line before [DONE]
		// (e.g. "The agent run failed before producing a reply."). Surface it
		// instead of finishing the turn with no content.
		if chunk.Error != nil && strings.TrimSpace(chunk.Error.Message) != "" {
			return c.fail(emit, fmt.Errorf("%s", strings.TrimSpace(chunk.Error.Message)))
		}
		for _, ev := range m.mapChunk(chunk) {
			if err := emit(ev); err != nil {
				return err
			}
		}
	}
	if err := sc.Err(); err != nil {
		return c.fail(emit, fmt.Errorf("reading stream: %w", err))
	}

	return emit(m.finish(""))
}

func (c *Client) fail(emit func(Event) error, err error) error {
	_ = emit(Event{Type: EventMessageDone, Error: err.Error()})
	return err
}

// Session is a minimal projection of an OpenClaw session from sessions_list.
type Session = agentruntime.Session

// ListSessions returns the sessions visible to the gateway via /tools/invoke.
// The gateway payload nests the list under result.details (same JSON also
// appears in result.content[0].text); field is "key" there, mapped to
// SessionKey here.
func (c *Client) ListSessions(ctx context.Context) ([]Session, error) {
	resp, err := c.toolsInvoke(ctx, "sessions_list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var out struct {
		Details struct {
			Count    int `json:"count"`
			Sessions []struct {
				Key   string `json:"key"`
				Title string `json:"title"`
			} `json:"sessions"`
		} `json:"details"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		return nil, fmt.Errorf("decode sessions_list result: %w", err)
	}
	sessions := make([]Session, 0, len(out.Details.Sessions))
	for _, s := range out.Details.Sessions {
		title := s.Title
		if title == "" {
			title = s.Key
		}
		sessions = append(sessions, Session{SessionKey: s.Key, Title: title})
	}
	return sessions, nil
}

// GetHistory returns the raw session history JSON for a session key.
func (c *Client) GetHistory(ctx context.Context, sessionKey string, limit int) (json.RawMessage, error) {
	if limit <= 0 {
		limit = 100
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/sessions/%s/history?limit=%d", c.baseURL, sessionKey, limit), nil)
	if err != nil {
		return nil, err
	}
	c.applyAuth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("history request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("history returned %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return io.ReadAll(resp.Body)
}

func (c *Client) toolsInvoke(ctx context.Context, tool string, args any) (json.RawMessage, error) {
	body, err := json.Marshal(map[string]any{"tool": tool, "args": args})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/tools/invoke", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	c.applyAuth(req)
	req.Header.Set("Content-Type", "application/json")

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tools/invoke %s: %w", tool, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tools/invoke %s returned %d: %s", tool, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode tools/invoke %s: %w", tool, err)
	}
	if !out.OK {
		return nil, fmt.Errorf("tools/invoke %s failed: %v", tool, out.Error)
	}
	return out.Result, nil
}

func (c *Client) applyAuth(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}
