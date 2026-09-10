package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/openclaw/ws"
)

const questionTestSession = "agent:main:conv-1"

// questionTestServer builds a server whose HITL manager owns gw for user alice,
// with a live SSE stream open for session (mimicking a chat turn parked on a
// question). user is the identity every request is made as.
func questionTestServer(t *testing.T, gw *fakeHitlGateway, session string) (*Server, *httptest.ResponseRecorder) {
	t.Helper()
	s := platformTestServer(t)
	s.hitl = newTestHitl(v1alpha1.ConfirmPolicyAllowlist, "rev-1", gw)
	// A registered connection is only usable once its handshake completed, so
	// the fixture marks the gateway connected rather than merely stored.
	gw.setConnected(true)
	s.hitl.conns["alice"] = &userHitlConn{user: "alice", gw: gw}
	rec := httptest.NewRecorder()
	if _, err := s.hub.Open(session, rec, rec); err != nil {
		t.Fatalf("open stream: %v", err)
	}
	return s, rec
}

// questionRecord builds a projectable pending record for session.
func questionRecord(id, session string) ws.QuestionRecord {
	return ws.QuestionRecord{
		ID: id, SessionKey: session, Status: "pending",
		ExpiresAtMs: time.Now().Add(840 * time.Second).UnixMilli(),
		Questions: []ws.Question{{
			QuestionID: "where", Header: "Target", Question: "Where should I write it?",
			Options: []ws.QuestionOption{{Label: "workspace"}, {Label: "home"}},
		}},
	}
}

// sseEvents parses the data: lines of an SSE body into their JSON objects.
func sseEvents(t *testing.T, body string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &ev); err != nil {
			t.Fatalf("decode SSE data %q: %v", line, err)
		}
		out = append(out, ev)
	}
	return out
}

func eventOfType(evs []map[string]any, typ string) map[string]any {
	for _, ev := range evs {
		if ev["type"] == typ {
			return ev
		}
	}
	return nil
}

func TestQuestionRelayProjectsPending(t *testing.T) {
	gw := &fakeHitlGateway{}
	s, rec := questionTestServer(t, gw, questionTestSession)

	s.relayQuestionRequested(questionRecord("ask_1", questionTestSession))

	ev := eventOfType(sseEvents(t, rec.Body.String()), "question_pending")
	if ev == nil {
		t.Fatalf("no question_pending in stream: %q", rec.Body.String())
	}
	if ev["call_id"] != "ask_1" || ev["session_id"] != questionTestSession {
		t.Errorf("event = %+v, want call_id ask_1 on its session", ev)
	}
	q, ok := ev["question"].(map[string]any)
	if !ok {
		t.Fatalf("question = %#v, want object", ev["question"])
	}
	items, ok := q["questions"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("questions = %#v, want one item", q["questions"])
	}
	item, _ := items[0].(map[string]any)
	if item["questionId"] != "where" || item["header"] != "Target" {
		t.Errorf("question item = %+v", item)
	}
	// The remaining time is what the browser counts down from; it must reflect
	// the gateway deadline without shipping an absolute timestamp.
	secs, ok := q["timeoutSeconds"].(float64)
	if !ok || secs <= 800 || secs > 840 {
		t.Errorf("timeoutSeconds = %#v, want ~840", q["timeoutSeconds"])
	}
}

// TestQuestionRelayProjectsRealAskUserRecord guards against a filter that
// rejects the very records the feature exists for. The ask_user tool's
// normalizer stamps isOther:true on every question it emits (that flag declares
// that free text is offered alongside the options -- see
// ask-user-tool-normalization.ts), so a record copy is not a representative
// fixture: this one carries the flags the tool actually sets.
func TestQuestionRelayProjectsRealAskUserRecord(t *testing.T) {
	req := questionRecord("ask_1", questionTestSession)
	req.Questions[0].IsOther = true

	gw := &fakeHitlGateway{}
	s, rec := questionTestServer(t, gw, questionTestSession)
	s.relayQuestionRequested(req)

	ev := eventOfType(sseEvents(t, rec.Body.String()), "question_pending")
	if ev == nil {
		t.Fatalf("a real ask_user record (isOther set) was not projected: %q", rec.Body.String())
	}
	if ev["call_id"] != "ask_1" {
		t.Errorf("event = %+v, want call_id ask_1", ev)
	}
}

// TestQuestionRelayDropsUnsupportedVariant: the admin connection sees every
// question.* event on the gateway, so a record from another producer (a secret
// prompt) must not be rendered as an ordinary choice card.
func TestQuestionRelayDropsUnsupportedVariant(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*ws.QuestionRecord)
	}{
		{"isSecret", func(r *ws.QuestionRecord) { r.Questions[0].IsSecret = true }},
		{"secretStore", func(r *ws.QuestionRecord) { r.Questions[0].SecretStore = json.RawMessage(`{"name":"TOKEN"}`) }},
		// A free-text-only question (no options) has nothing to render as
		// buttons, and this version offers no text input.
		{"no options", func(r *ws.QuestionRecord) { r.Questions[0].Options = nil }},
		{"no questions", func(r *ws.QuestionRecord) { r.Questions = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gw := &fakeHitlGateway{}
			s, rec := questionTestServer(t, gw, questionTestSession)
			req := questionRecord("ask_1", questionTestSession)
			tc.mutate(&req)
			s.relayQuestionRequested(req)
			if ev := eventOfType(sseEvents(t, rec.Body.String()), "question_pending"); ev != nil {
				t.Fatalf("unsupported record was projected: %+v", ev)
			}
		})
	}
}

func TestQuestionRelayDropsRecordWithoutSession(t *testing.T) {
	gw := &fakeHitlGateway{}
	s, rec := questionTestServer(t, gw, questionTestSession)
	s.relayQuestionRequested(questionRecord("ask_1", ""))
	if ev := eventOfType(sseEvents(t, rec.Body.String()), "question_pending"); ev != nil {
		t.Fatalf("record without a session key was projected: %+v", ev)
	}
}

// TestQuestionResolvedRoutesByRecordedSession: question.resolved carries only
// {id, status}, so it is addressed through the route recorded when the question
// was relayed.
func TestQuestionResolvedRoutesByRecordedSession(t *testing.T) {
	gw := &fakeHitlGateway{}
	s, rec := questionTestServer(t, gw, questionTestSession)

	s.relayQuestionRequested(questionRecord("ask_1", questionTestSession))
	s.relayQuestionResolved(ws.QuestionResolved{ID: "ask_1", Status: "expired"})

	ev := eventOfType(sseEvents(t, rec.Body.String()), "question_resolved")
	if ev == nil {
		t.Fatalf("no question_resolved in stream: %q", rec.Body.String())
	}
	if ev["call_id"] != "ask_1" || ev["session_id"] != questionTestSession || ev["message"] != "expired" {
		t.Errorf("event = %+v", ev)
	}
}

// TestQuestionResolvedUnknownIDIsDropped covers a resolution for a question
// this process never relayed or recovered: it must not be broadcast blindly.
func TestQuestionResolvedUnknownIDIsDropped(t *testing.T) {
	gw := &fakeHitlGateway{}
	s, rec := questionTestServer(t, gw, questionTestSession)
	before := rec.Body.Len()
	s.relayQuestionResolved(ws.QuestionResolved{ID: "ask-unknown", Status: "cancelled"})
	if rec.Body.Len() != before {
		t.Errorf("unknown resolved event was forwarded: %q", rec.Body.String())
	}
}

func TestHandleQuestionAnswers(t *testing.T) {
	gw := &fakeHitlGateway{questionRecords: map[string]ws.QuestionRecord{
		"ask_1": questionRecord("ask_1", questionTestSession),
	}}
	s, _ := questionTestServer(t, gw, questionTestSession)

	rec := doReq(t, s.Handler(), http.MethodPost, "/api/sessions/conv-1/question", "alice",
		map[string]any{"id": "ask_1", "answers": map[string][]string{"where": {"workspace"}}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(gw.questionResolves) != 1 || gw.questionResolves[0] != "ask_1|alice|where=workspace" {
		t.Errorf("resolves = %v, want ask_1 answered by alice as where=workspace", gw.questionResolves)
	}
}

func TestHandleQuestionCancel(t *testing.T) {
	gw := &fakeHitlGateway{questionRecords: map[string]ws.QuestionRecord{
		"ask_1": questionRecord("ask_1", questionTestSession),
	}}
	s, _ := questionTestServer(t, gw, questionTestSession)

	rec := doReq(t, s.Handler(), http.MethodPost, "/api/sessions/conv-1/question", "alice",
		map[string]any{"id": "ask_1", "cancel": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(gw.questionCancels) != 1 || gw.questionCancels[0] != "ask_1|alice" {
		t.Errorf("cancels = %v, want ask_1 cancelled by alice", gw.questionCancels)
	}
	if len(gw.questionResolves) != 0 {
		t.Errorf("a cancel must not answer: %v", gw.questionResolves)
	}
}

// TestHandleQuestionRejectsForeignSession is the guard that stops a stale card
// for one session from resolving a question belonging to another.
func TestHandleQuestionRejectsForeignSession(t *testing.T) {
	gw := &fakeHitlGateway{questionRecords: map[string]ws.QuestionRecord{
		"ask_1": questionRecord("ask_1", "agent:main:other-session"),
	}}
	s, _ := questionTestServer(t, gw, questionTestSession)

	rec := doReq(t, s.Handler(), http.MethodPost, "/api/sessions/conv-1/question", "alice",
		map[string]any{"id": "ask_1", "answers": map[string][]string{"where": {"workspace"}}})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	if len(gw.questionResolves) != 0 {
		t.Errorf("a foreign-session question was resolved: %v", gw.questionResolves)
	}
}

func TestHandleQuestionRejectsNotPendingOrExpired(t *testing.T) {
	expired := questionRecord("ask_1", questionTestSession)
	expired.ExpiresAtMs = time.Now().Add(-time.Minute).UnixMilli()
	answered := questionRecord("ask_2", questionTestSession)
	answered.Status = "answered"

	gw := &fakeHitlGateway{questionRecords: map[string]ws.QuestionRecord{
		"ask_1": expired, "ask_2": answered,
	}}
	s, _ := questionTestServer(t, gw, questionTestSession)

	for id, scenario := range map[string]string{"ask_1": "expired", "ask_2": "already answered"} {
		rec := doReq(t, s.Handler(), http.MethodPost, "/api/sessions/conv-1/question", "alice",
			map[string]any{"id": id, "answers": map[string][]string{"where": {"workspace"}}})
		if rec.Code != http.StatusConflict {
			t.Errorf("%s (%s): status = %d, want 409: %s", id, scenario, rec.Code, rec.Body.String())
		}
	}
	if len(gw.questionResolves) != 0 {
		t.Errorf("a closed question was resolved: %v", gw.questionResolves)
	}
}

func TestHandleQuestionBadRequests(t *testing.T) {
	gw := &fakeHitlGateway{questionRecords: map[string]ws.QuestionRecord{
		"ask_1": questionRecord("ask_1", questionTestSession),
	}}
	s, _ := questionTestServer(t, gw, questionTestSession)

	cases := map[string]map[string]any{
		"missing id":            {"answers": map[string][]string{"where": {"workspace"}}},
		"answers and cancel":    {"id": "ask_1", "cancel": true, "answers": map[string][]string{"where": {"workspace"}}},
		"neither answers":       {"id": "ask_1"},
		"answers with no entry": {"id": "ask_1", "answers": map[string][]string{}},
	}
	for name, body := range cases {
		rec := doReq(t, s.Handler(), http.MethodPost, "/api/sessions/conv-1/question", "alice", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400: %s", name, rec.Code, rec.Body.String())
		}
	}
}

// TestHandleQuestionWithoutChannel: answering and reading use the user's
// existing connection rather than dialing a new one, so a user with no live
// channel is told the channel is unavailable.
func TestHandleQuestionWithoutChannel(t *testing.T) {
	gw := &fakeHitlGateway{questionRecords: map[string]ws.QuestionRecord{
		"ask_1": questionRecord("ask_1", questionTestSession),
	}}
	s, _ := questionTestServer(t, gw, questionTestSession)
	delete(s.hitl.conns, "alice")

	rec := doReq(t, s.Handler(), http.MethodPost, "/api/sessions/conv-1/question", "alice",
		map[string]any{"id": "ask_1", "answers": map[string][]string{"where": {"workspace"}}})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body.String())
	}
}

// TestHandleQuestionDuringPairingReportsUnavailable: a connection is stored
// before its handshake completes, so one that is still pairing must read as an
// unavailable channel rather than failing later inside the RPC as a gateway
// error.
func TestHandleQuestionDuringPairingReportsUnavailable(t *testing.T) {
	gw := &fakeHitlGateway{questionRecords: map[string]ws.QuestionRecord{
		"ask_1": questionRecord("ask_1", questionTestSession),
	}}
	s, _ := questionTestServer(t, gw, questionTestSession)
	gw.setConnected(false) // registered, handshake still in flight

	rec := doReq(t, s.Handler(), http.MethodPost, "/api/sessions/conv-1/question", "alice",
		map[string]any{"id": "ask_1", "answers": map[string][]string{"where": {"workspace"}}})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body.String())
	}
	pending := doReq(t, s.Handler(), http.MethodGet, "/api/sessions/conv-1/question/pending", "alice", nil)
	if pending.Code != http.StatusNotFound {
		t.Fatalf("pending status = %d, want 404: %s", pending.Code, pending.Body.String())
	}
}

func TestHandlePendingQuestion(t *testing.T) {
	expired := questionRecord("ask_old", questionTestSession)
	expired.ExpiresAtMs = time.Now().Add(-time.Second).UnixMilli()
	secret := questionRecord("ask_secret", questionTestSession)
	secret.Questions[0].IsSecret = true
	gw := &fakeHitlGateway{pendingQuestions: []ws.QuestionRecord{
		questionRecord("ask_1", questionTestSession),
		expired,
		secret,
		questionRecord("ask_other", "agent:main:other-session"),
		questionRecord("ask_2", questionTestSession),
	}}
	s, _ := questionTestServer(t, gw, questionTestSession)

	rec := doReq(t, s.Handler(), http.MethodGet, "/api/sessions/conv-1/question/pending", "alice", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Questions []questionEntry `json:"questions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Only this session's open, projectable questions, in gateway order.
	if len(resp.Questions) != 2 || resp.Questions[0].ID != "ask_1" || resp.Questions[1].ID != "ask_2" {
		t.Fatalf("questions = %+v, want ask_1 then ask_2", resp.Questions)
	}
	if resp.Questions[0].TimeoutSeconds <= 800 {
		t.Errorf("timeoutSeconds = %d, want the remaining time", resp.Questions[0].TimeoutSeconds)
	}
	// Recovery also teaches the route table, so a later resolution can be
	// addressed even for a question this process never relayed.
	if key, ok := s.qroutes.take("ask_2"); !ok || key != questionTestSession {
		t.Errorf("recovered question not routed: key=%q ok=%v", key, ok)
	}
}

// TestHandlePendingQuestionNoPending: a session with nothing open reports 404,
// which is what the browser uses to decide whether to render a card.
func TestHandlePendingQuestionNoPending(t *testing.T) {
	gw := &fakeHitlGateway{pendingQuestions: []ws.QuestionRecord{
		questionRecord("ask_1", "agent:main:other-session"),
	}}
	s, _ := questionTestServer(t, gw, questionTestSession)
	rec := doReq(t, s.Handler(), http.MethodGet, "/api/sessions/conv-1/question/pending", "alice", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleQuestionRoutePrecedence(t *testing.T) {
	gw := &fakeHitlGateway{pendingQuestions: nil}
	s, _ := questionTestServer(t, gw, questionTestSession)
	// /question/pending must not be swallowed by the /question route.
	rec := doReq(t, s.Handler(), http.MethodGet, "/api/sessions/conv-1/question/pending", "alice", nil)
	if rec.Code == http.StatusMethodNotAllowed {
		t.Fatalf("GET .../question/pending was routed to the answer handler: %s", rec.Body.String())
	}
}

func TestQuestionErrorStatus(t *testing.T) {
	for reason, want := range map[string]int{
		"QUESTION_NOT_FOUND":        http.StatusNotFound,
		"QUESTION_ALREADY_TERMINAL": http.StatusConflict,
		"QUESTION_INVALID_ANSWER":   http.StatusBadRequest,
		"":                          http.StatusBadGateway,
		"SOMETHING_ELSE_ENTIRELY":   http.StatusBadGateway,
	} {
		if got := questionErrorStatus(reason); got != want {
			t.Errorf("questionErrorStatus(%q) = %d, want %d", reason, got, want)
		}
	}
}

// TestAskUserToolCardIsSuppressed: ask_user renders only as the interactive
// question card, never as a generic tool card that would spin for the whole
// human wait.
func TestAskUserToolCardIsSuppressed(t *testing.T) {
	p := newLiveProjector()
	const session = "agent:main:conv-1"

	events, _ := p.feed(session, "agent", mustJSONRaw(t, map[string]any{
		"stream": "tool",
		"data": map[string]any{
			"phase": "start", "name": "ask_user", "toolCallId": "call-q",
			"args": map[string]any{"questions": []any{}},
		},
	}))
	if len(events) != 0 {
		t.Fatalf("ask_user start produced %+v, want no card", events)
	}
	// Its result must stay silent too (no result event at the run's end either).
	events, _ = p.feed(session, "agent", mustJSONRaw(t, map[string]any{
		"stream": "tool",
		"data": map[string]any{
			"phase": "result", "name": "ask_user", "toolCallId": "call-q",
			"result": map[string]any{"text": "answered"},
		},
	}))
	if len(events) != 0 {
		t.Fatalf("ask_user result produced %+v, want nothing", events)
	}
	if flush := p.finalizeAll(session); len(flush) != 0 {
		t.Fatalf("finalize produced %+v for a suppressed call", flush)
	}

	// Other tools still project normally.
	events, _ = p.feed(session, "agent", mustJSONRaw(t, map[string]any{
		"stream": "tool",
		"data": map[string]any{
			"phase": "start", "name": "read", "toolCallId": "call-r",
			"args": map[string]any{"path": "/tmp/x"},
		},
	}))
	if len(events) != 1 || events[0].Type != "tool_call" || events[0].Name != "read" {
		t.Fatalf("read start = %+v, want one tool_call", events)
	}
}

func mustJSONRaw(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
