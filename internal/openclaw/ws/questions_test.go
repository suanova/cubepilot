package ws

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// questionGateway records the params of every method it is asked for and replies
// with canned question payloads.
type questionGateway struct {
	mu     sync.Mutex
	params map[string]json.RawMessage
}

func (g *questionGateway) record(method string, params json.RawMessage) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.params == nil {
		g.params = map[string]json.RawMessage{}
	}
	g.params[method] = params
}

func (g *questionGateway) got(t *testing.T, method string) map[string]any {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	var out map[string]any
	if err := json.Unmarshal(g.params[method], &out); err != nil {
		t.Fatalf("decode recorded %s params: %v", method, err)
	}
	return out
}

func newQuestionGateway(t *testing.T) (*httptest.Server, *questionGateway) {
	g := &questionGateway{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")
		ctx := context.Background()
		_ = conn.Write(ctx, websocket.MessageText, mustRaw(eventFrame{Type: "event", Event: challengeEvent, Payload: mustJSON(t, connectChallenge{Nonce: "n1"})}))
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var f requestFrame
			if err := json.Unmarshal(data, &f); err != nil {
				continue
			}
			if f.Method == "connect" {
				hello, _ := json.Marshal(map[string]any{"type": "hello-ok", "protocol": 4, "auth": map[string]any{"role": "operator", "scopes": []string{"operator.admin"}}})
				_ = conn.Write(ctx, websocket.MessageText, mustRaw(responseFrame{Type: "res", ID: f.ID, OK: true, Payload: hello}))
				continue
			}
			g.record(f.Method, f.Params)
			_ = conn.Write(ctx, websocket.MessageText, mustRaw(responseFrame{Type: "res", ID: f.ID, OK: true, Payload: questionReply(f.Method)}))
		}
	}))
	return ts, g
}

func questionReply(method string) json.RawMessage {
	switch method {
	case "question.get":
		return mustRaw(map[string]any{"question": map[string]any{
			"id": "ask_1", "sessionKey": "conv-1", "status": "pending",
			"questions": []map[string]any{{
				"questionId": "where", "header": "Target", "question": "Where should I write it?",
				"options": []map[string]any{{"label": "workspace"}, {"label": "home"}},
			}},
			"createdAtMs": 1, "expiresAtMs": 9999999999999,
		}})
	case "question.list":
		return mustRaw(map[string]any{"questions": []map[string]any{{
			"id": "ask_1", "sessionKey": "conv-1", "status": "pending",
			"questions":   []map[string]any{{"questionId": "where", "header": "Target", "question": "Where?", "options": []map[string]any{{"label": "workspace"}, {"label": "home"}}}},
			"createdAtMs": 1, "expiresAtMs": 9999999999999,
		}}})
	case "question.resolve":
		return mustRaw(map[string]any{"status": "answered", "answers": map[string]any{"answers": map[string][]string{"where": {"workspace"}}}})
	default:
		return json.RawMessage(`{"ok":true}`)
	}
}

func connectQuestionClient(t *testing.T, ts *httptest.Server, ctx context.Context) *Client {
	t.Helper()
	dev, err := GenerateDevice()
	if err != nil {
		t.Fatal(err)
	}
	c := NewClient(strings.Replace(ts.URL, "http", "ws", 1)+"/gateway", "token", dev)
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestResolveQuestionParams(t *testing.T) {
	ts, g := newQuestionGateway(t)
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cli := connectQuestionClient(t, ts, ctx)

	if err := cli.ResolveQuestion(ctx, "ask_1", map[string][]string{"where": {"workspace"}}, "alice"); err != nil {
		t.Fatalf("ResolveQuestion: %v", err)
	}
	got := g.got(t, "question.resolve")
	if got["id"] != "ask_1" {
		t.Errorf("id = %#v, want ask_1", got["id"])
	}
	if got["resolvedBy"] != "alice" {
		t.Errorf("resolvedBy = %#v, want alice", got["resolvedBy"])
	}
	if _, ok := got["cancel"]; ok {
		t.Errorf("cancel must be omitted when answering, got %#v", got["cancel"])
	}
	// The gateway schema nests the answer map twice: answers.answers[qid] = labels.
	answers, ok := got["answers"].(map[string]any)
	if !ok {
		t.Fatalf("answers = %#v, want object", got["answers"])
	}
	inner, ok := answers["answers"].(map[string]any)
	if !ok {
		t.Fatalf("answers.answers = %#v, want object", answers["answers"])
	}
	labels, ok := inner["where"].([]any)
	if !ok || len(labels) != 1 || labels[0] != "workspace" {
		t.Errorf("answers.answers.where = %#v, want [workspace]", inner["where"])
	}
}

func TestCancelQuestionParams(t *testing.T) {
	ts, g := newQuestionGateway(t)
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cli := connectQuestionClient(t, ts, ctx)

	if err := cli.CancelQuestion(ctx, "ask_2", "alice"); err != nil {
		t.Fatalf("CancelQuestion: %v", err)
	}
	got := g.got(t, "question.resolve")
	if got["id"] != "ask_2" {
		t.Errorf("id = %#v, want ask_2", got["id"])
	}
	if got["cancel"] != true {
		t.Errorf("cancel = %#v, want true", got["cancel"])
	}
	if _, ok := got["answers"]; ok {
		t.Errorf("answers must be omitted when cancelling, got %#v", got["answers"])
	}
}

// TestResolveQuestionRejectsEmptyAnswers keeps a malformed call off the wire:
// the answer variant of question.resolve requires at least one entry.
func TestResolveQuestionRejectsEmptyAnswers(t *testing.T) {
	ts, g := newQuestionGateway(t)
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cli := connectQuestionClient(t, ts, ctx)

	if err := cli.ResolveQuestion(ctx, "ask_3", nil, "alice"); err == nil {
		t.Fatal("ResolveQuestion with no answers must fail")
	}
	g.mu.Lock()
	_, sent := g.params["question.resolve"]
	g.mu.Unlock()
	if sent {
		t.Error("no question.resolve must be sent for an empty answer set")
	}
}

func TestGetAndListQuestions(t *testing.T) {
	ts, _ := newQuestionGateway(t)
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cli := connectQuestionClient(t, ts, ctx)

	rec, err := cli.GetQuestion(ctx, "ask_1")
	if err != nil {
		t.Fatalf("GetQuestion: %v", err)
	}
	if rec.ID != "ask_1" || rec.SessionKey != "conv-1" || rec.Status != "pending" {
		t.Errorf("record = %+v", rec)
	}
	if len(rec.Questions) != 1 || rec.Questions[0].QuestionID != "where" || len(rec.Questions[0].Options) != 2 {
		t.Errorf("questions = %+v", rec.Questions)
	}

	// question.list wraps the records: {questions:[...]}.
	list, err := cli.ListQuestions(ctx)
	if err != nil {
		t.Fatalf("ListQuestions: %v", err)
	}
	if len(list) != 1 || list[0].ID != "ask_1" || list[0].SessionKey != "conv-1" {
		t.Errorf("list = %+v", list)
	}
}

func TestQuestionEventsDispatch(t *testing.T) {
	ts, _ := newQuestionGateway(t)
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cli := connectQuestionClient(t, ts, ctx)

	reqCh := make(chan QuestionRecord, 1)
	resCh := make(chan QuestionResolved, 1)
	cli.OnQuestionRequested(func(r QuestionRecord) { reqCh <- r })
	cli.OnQuestionResolved(func(r QuestionResolved) { resCh <- r })
	cli.OnEvent(func(string, []byte) {
		t.Error("question events must not fall through to the live-stream observer")
	})

	cli.dispatchEvent(eventFrame{Type: "event", Event: questionRequested, Payload: mustRaw(map[string]any{
		"id": "ask_9", "sessionKey": "conv-7", "status": "pending",
		"questions": []map[string]any{{
			"questionId": "where", "header": "Target", "question": "Where?",
			"options": []map[string]any{{"label": "workspace"}, {"label": "home"}}, "multiSelect": true,
		}},
		"expiresAtMs": 9999999999999,
	})})
	cli.dispatchEvent(eventFrame{Type: "event", Event: questionResolved, Payload: mustRaw(map[string]any{"id": "ask_9", "status": "expired"})})

	select {
	case got := <-reqCh:
		if got.ID != "ask_9" || got.SessionKey != "conv-7" {
			t.Errorf("requested = %+v", got)
		}
		if len(got.Questions) != 1 || !got.Questions[0].MultiSelect {
			t.Errorf("questions = %+v", got.Questions)
		}
	case <-ctx.Done():
		t.Fatal("question.requested not dispatched")
	}
	select {
	case got := <-resCh:
		if got.ID != "ask_9" || got.Status != "expired" {
			t.Errorf("resolved = %+v", got)
		}
	case <-ctx.Done():
		t.Fatal("question.resolved not dispatched")
	}
}

// TestReasonOfFromGatewayErrorFrame pins the wire shape end to end: a failed
// call whose error frame carries details.reason surfaces through ReasonOf, so
// callers map it to their own status without parsing message text. The gateway
// sends these as {code, message, details:{reason}} (errorShape in
// packages/gateway-protocol).
func TestReasonOfFromGatewayErrorFrame(t *testing.T) {
	dev, err := GenerateDevice()
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")
		ctx := context.Background()
		_ = conn.Write(ctx, websocket.MessageText, mustRaw(eventFrame{Type: "event", Event: challengeEvent, Payload: mustJSON(t, connectChallenge{Nonce: "n1"})}))
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var f requestFrame
			if err := json.Unmarshal(data, &f); err != nil {
				continue
			}
			if f.Method == "connect" {
				hello, _ := json.Marshal(map[string]any{"type": "hello-ok", "protocol": 4, "auth": map[string]any{"role": "operator", "scopes": []string{"operator.admin"}}})
				_ = conn.Write(ctx, websocket.MessageText, mustRaw(responseFrame{Type: "res", ID: f.ID, OK: true, Payload: hello}))
				continue
			}
			_ = conn.Write(ctx, websocket.MessageText, mustRaw(responseFrame{
				Type: "res", ID: f.ID, OK: false,
				Error: &frameError{
					Code:    "INVALID_REQUEST",
					Message: "question 'ask_1' was not found",
					Details: json.RawMessage(`{"reason":"QUESTION_NOT_FOUND"}`),
				},
			}))
		}
	}))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cli := NewClient(strings.Replace(ts.URL, "http", "ws", 1)+"/gateway", "token", dev)
	if err := cli.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer cli.Close()

	if _, err := cli.GetQuestion(ctx, "ask_1"); err == nil {
		t.Fatal("GetQuestion on a missing question must fail")
	} else if got := ReasonOf(err); got != "QUESTION_NOT_FOUND" {
		t.Errorf("ReasonOf = %q, want QUESTION_NOT_FOUND (err = %v)", got, err)
	}
}

func TestFrameErrorReason(t *testing.T) {
	fe := &frameError{Code: "INVALID_REQUEST", Message: "question 'ask_1' was not found",
		Details: json.RawMessage(`{"reason":"QUESTION_NOT_FOUND"}`)}
	if got := fe.reason(); got != "QUESTION_NOT_FOUND" {
		t.Errorf("reason = %q, want QUESTION_NOT_FOUND", got)
	}
	if got := (&frameError{Code: "X"}).reason(); got != "" {
		t.Errorf("reason without details = %q, want empty", got)
	}
	if got := (&frameError{Details: json.RawMessage(`"nope"`)}).reason(); got != "" {
		t.Errorf("reason from non-object details = %q, want empty", got)
	}
}
