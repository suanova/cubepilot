package framework

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/suanova/cubepilot/internal/openclaw"
)

// do performs an HTTP request with optional headers against the given URL.
func (f *Framework) do(ctx context.Context, method, url string, body io.Reader, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return http.DefaultClient.Do(req)
}

// GetRaw performs a GET and returns the raw body and status code.
func (f *Framework) GetRaw(ctx context.Context, url string) ([]byte, int, error) {
	resp, err := f.do(ctx, http.MethodGet, url, nil, nil)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return b, resp.StatusCode, nil
}

// GetJSON performs a GET and decodes the JSON response. An empty body is
// reported as an error (use GetRaw for plain-text / empty responses).
func (f *Framework) GetJSON(ctx context.Context, url string, headers map[string]string) (map[string]any, int, error) {
	resp, err := f.do(ctx, http.MethodGet, url, nil, headers)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, resp.StatusCode, err
	}
	return out, resp.StatusCode, nil
}

// SSEEvent is one parsed Server-Sent Event from the chat stream.
type SSEEvent struct {
	Event string          // the event: type
	Data  json.RawMessage // the data: payload (JSON)
}

// ChatSSE posts a chat message to the portal's /api/messages and reads the SSE
// reply stream until message_done (or the stream ends / context deadline). It
// replicates the assertions the old scripts/e2e.sh chat phase made with curl.
//
// HITL (issue #20): when the stream carries a confirm_pending (a write paused
// for a human), the stream only resumes after a decision, so the reader
// auto-resolves it via POST /api/sessions/{key}/confirm with the given decision
// ("approve" by default; pass another decision to exercise the reject path).
func (f *Framework) ChatSSE(ctx context.Context, user, sessionID, content string) ([]SSEEvent, error) {
	return f.ChatSSEWithDecision(ctx, user, sessionID, content, "approve")
}

// ChatSSEWithDecision is ChatSSE with a configurable approval decision.
func (f *Framework) ChatSSEWithDecision(ctx context.Context, user, sessionID, content, decision string) ([]SSEEvent, error) {
	body, err := json.Marshal(map[string]string{"session_id": sessionID, "content": content})
	if err != nil {
		return nil, err
	}
	resp, err := f.do(ctx, http.MethodPost, f.PortalBase+"/api/messages",
		bytes.NewReader(body), map[string]string{
			"Content-Type":     "application/json",
			"X-CubePilot-User": user,
		})
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("chat POST returned %d", resp.StatusCode)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var events []SSEEvent
	var cur SSEEvent
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if cur.Event != "" {
				events = append(events, cur)
				if cur.Event == openclaw.EventMessageDone {
					return events, nil
				}
				if cur.Event == openclaw.EventConfirmPending {
					if err := f.resolveConfirm(ctx, user, cur.Data, decision); err != nil {
						return events, err
					}
				}
				cur = SSEEvent{}
			}
		case strings.HasPrefix(line, "event: "):
			cur.Event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			payload := strings.TrimPrefix(line, "data: ")
			if len(cur.Data) == 0 {
				cur.Data = json.RawMessage(payload)
			} else {
				cur.Data = append(cur.Data, '\n')
				cur.Data = append(cur.Data, payload...)
			}
		}
	}
	return events, scanner.Err()
}

// resolveConfirm posts the human decision for a confirm_pending event so the
// paused gateway run resumes and the SSE stream reaches message_done.
func (f *Framework) resolveConfirm(ctx context.Context, user string, data json.RawMessage, decision string) error {
	var pending struct {
		SessionID string `json:"session_id"`
		CallID    string `json:"call_id"`
	}
	if err := json.Unmarshal(data, &pending); err != nil {
		return fmt.Errorf("decode confirm_pending: %w", err)
	}
	if pending.SessionID == "" {
		return fmt.Errorf("confirm_pending carried no session_id")
	}
	reqBody, err := json.Marshal(map[string]string{"decision": decision})
	if err != nil {
		return err
	}
	resp, err := f.do(ctx, http.MethodPost,
		f.PortalBase+"/api/sessions/"+url.PathEscape(pending.SessionID)+"/confirm",
		bytes.NewReader(reqBody), map[string]string{
			"Content-Type":     "application/json",
			"X-CubePilot-User": user,
		})
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("confirm (%s) returned %d: %s", decision, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}
