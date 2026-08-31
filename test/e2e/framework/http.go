package framework

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
func (f *Framework) ChatSSE(ctx context.Context, user, sessionID, content string) ([]SSEEvent, error) {
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
