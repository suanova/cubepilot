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

// SendJSON performs a request with a JSON body and decodes the JSON response
// (nil for an empty body). A non-2xx status is returned to the caller rather
// than raised: these specs assert on the code.
func (f *Framework) SendJSON(ctx context.Context, method, url string, body any, headers map[string]string) (map[string]any, int, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		reader = bytes.NewReader(b)
	}
	h := map[string]string{"Content-Type": "application/json"}
	for k, v := range headers {
		h[k] = v
	}
	resp, err := f.do(ctx, method, url, reader, h)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if len(raw) == 0 {
		return nil, resp.StatusCode, nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("decode %q: %w", string(raw), err)
	}
	return out, resp.StatusCode, nil
}

// PostRaw performs a POST with the Portal's identity header and returns the raw
// body and status code. body may be nil for endpoints that take no payload.
func (f *Framework) PostRaw(ctx context.Context, url, user string, body []byte) ([]byte, int, error) {
	var r io.Reader
	headers := map[string]string{"X-CubePilot-User": user}
	if body != nil {
		r = bytes.NewReader(body)
		headers["Content-Type"] = "application/json"
	}
	resp, err := f.do(ctx, http.MethodPost, url, r, headers)
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

// AbortSession posts POST /api/sessions/{key}/abort -- the Portal's Stop, the
// same call the UI makes before it sends a redirecting message.
//
// The endpoint deliberately does not answer until the session has settled, so a
// 200 here is the promise the follow-up send relies on: it means the session is
// idle, and the next POST /api/messages for it cannot be refused as a
// concurrent turn.
func (f *Framework) AbortSession(ctx context.Context, user, sessionKey string) ([]byte, int, error) {
	return f.PostRaw(ctx, f.PortalBase+"/api/sessions/"+url.PathEscape(sessionKey)+"/abort", user, nil)
}

// SessionTurnActive reads GET /api/sessions/{key}/turn -- whether the session
// still has a run in flight -- and returns the active flag with the status code.
//
// The API answers 502 when it cannot determine the answer (a gateway read that
// failed on a live connection, or one that timed out) rather than reporting "not
// busy", so callers must check the status: an unreadable status is not an idle
// session. A user with no gateway connection at all is a different case and
// answers an idle 200, because with no channel nothing can be running that this
// process could stop.
func (f *Framework) SessionTurnActive(ctx context.Context, user, sessionKey string) (bool, int, error) {
	data, code, err := f.GetJSON(ctx,
		f.PortalBase+"/api/sessions/"+url.PathEscape(sessionKey)+"/turn",
		map[string]string{"X-CubePilot-User": user})
	if err != nil {
		return false, code, err
	}
	active, _ := data["active"].(bool)
	return active, code, nil
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
