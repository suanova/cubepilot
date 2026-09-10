package server

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/openclaw/ws"
	agentruntime "github.com/suanova/cubepilot/internal/runtime"
)

// attachSession is the canonical key the /stream route resolves "conv-1" to.
const attachSession = "agent:main:conv-1"

// attachTestServer builds a Server whose HITL connection for alice is already
// established, with no SSE stream open -- the state a browser is in after a
// reload that dropped the turn's stream (issue #167).
func attachTestServer(t *testing.T, gw *fakeHitlGateway) *Server {
	t.Helper()
	s := platformTestServer(t)
	s.hitl = newTestHitl(v1alpha1.ConfirmPolicyAllowlist, "rev-1", gw)
	if _, err := s.hitl.conn(context.Background(), "alice"); err != nil {
		t.Fatalf("establish the user's connection: %v", err)
	}
	return s
}

// attachResult carries AttachLiveTurn's two return values through a channel.
type attachResult struct {
	outcome agentruntime.TurnOutcome
	err     error
}

// waitFor polls cond until it holds. The fake guards its state, so polling it is
// a safe way to order this goroutine against an attach that is still starting.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestAttachLiveTurnObservesAParkedRun: the attach subscribes the session and
// forwards the parked run's frames to the sink, ignoring a foreign run of the
// same session.
func TestAttachLiveTurnObservesAParkedRun(t *testing.T) {
	gw := &fakeHitlGateway{}
	m := newTestHitl(v1alpha1.ConfirmPolicyAllowlist, "rev-1", gw)

	var mu sync.Mutex
	var got []agentruntime.Event
	done := make(chan attachResult, 1)
	go func() {
		outcome, err := m.AttachLiveTurn(context.Background(), "alice", "conv-1", "run-1", func(ev agentruntime.Event) error {
			mu.Lock()
			got = append(got, ev)
			mu.Unlock()
			return nil
		})
		done <- attachResult{outcome: outcome, err: err}
	}()

	waitFor(t, "the attach to subscribe", func() bool { return len(gw.subscribedSessions()) == 1 })
	onEvent := gw.eventSink()
	if onEvent == nil {
		t.Fatal("conn did not register an OnEvent router")
	}
	// A foreign run of the same session must not leak into the attached stream
	// (its terminal frame would end the observation of the parked run).
	onEvent("chat", []byte(`{"sessionKey":"conv-1","runId":"foreign","state":"final","deltaText":"nope"}`))
	onEvent("chat", []byte(`{"sessionKey":"conv-1","runId":"run-1","state":"delta","deltaText":"resumed"}`))
	onEvent("chat", []byte(`{"sessionKey":"conv-1","runId":"run-1","state":"final","deltaText":"resumed"}`))

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("AttachLiveTurn: %v", res.err)
		}
		if res.outcome.Stopped {
			t.Fatalf("outcome = %+v, want a plain completion", res.outcome)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("AttachLiveTurn did not return after the run's terminal frame")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0].Type != agentruntime.EventMessageDelta || got[0].Delta != "resumed" {
		t.Fatalf("events = %+v, want only the parked run's delta", got)
	}
	// The observation is released with the run: the session is unsubscribed so a
	// later turn starts from a clean subscription.
	if unsub := gw.unsubscribedSessions(); len(unsub) != 1 || unsub[0] != "conv-1" {
		t.Fatalf("unsubscribes = %v, want [conv-1]", unsub)
	}
}

// TestAttachLiveTurnReportsAStoppedRun: a run another tab stops is terminal but
// not a failure, so an observer must report the outcome rather than a plain
// completion -- otherwise the tab that answered would show the stopped turn as
// if it had finished (issue #166's outcome contract).
func TestAttachLiveTurnReportsAStoppedRun(t *testing.T) {
	gw := &fakeHitlGateway{}
	m := newTestHitl(v1alpha1.ConfirmPolicyAllowlist, "rev-1", gw)

	done := make(chan attachResult, 1)
	go func() {
		outcome, err := m.AttachLiveTurn(context.Background(), "alice", "conv-1", "run-1", func(agentruntime.Event) error { return nil })
		done <- attachResult{outcome: outcome, err: err}
	}()
	waitFor(t, "the attach to subscribe", func() bool { return len(gw.subscribedSessions()) == 1 })
	onEvent := gw.eventSink()
	if onEvent == nil {
		t.Fatal("conn did not register an OnEvent router")
	}
	// stopReason "rpc" is a request-initiated abort (chat.abort), not a failure.
	onEvent("chat", []byte(`{"sessionKey":"conv-1","runId":"run-1","state":"aborted","stopReason":"rpc"}`))

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("AttachLiveTurn: %v, want a stopped outcome with no error", res.err)
		}
		if !res.outcome.Stopped {
			t.Fatalf("outcome = %+v, want Stopped", res.outcome)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("AttachLiveTurn did not return after the abort frame")
	}
}

// TestSessionStreamGates: only a parked turn can be attached, and only when no
// other stream holds the session.
func TestSessionStreamGates(t *testing.T) {
	t.Run("no parked turn", func(t *testing.T) {
		s := attachTestServer(t, &fakeHitlGateway{})
		rec := doReq(t, s.Handler(), http.MethodGet, "/api/sessions/conv-1/stream", "alice", nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("HITL not configured", func(t *testing.T) {
		s := platformTestServer(t)
		rec := doReq(t, s.Handler(), http.MethodGet, "/api/sessions/conv-1/stream", "alice", nil)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("missing session key", func(t *testing.T) {
		s := attachTestServer(t, &fakeHitlGateway{})
		rec := doReq(t, s.Handler(), http.MethodGet, "/api/sessions/agent:main:/stream", "alice", nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("another stream holds the session", func(t *testing.T) {
		gw := &fakeHitlGateway{pendingQuestions: []ws.QuestionRecord{questionRecord("ask_1", attachSession)}}
		s := attachTestServer(t, gw)
		held := httptest.NewRecorder()
		if _, err := s.hub.Open(attachSession, held, held); err != nil {
			t.Fatalf("open the turn's stream: %v", err)
		}
		rec := doReq(t, s.Handler(), http.MethodGet, "/api/sessions/conv-1/stream", "alice", nil)
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("POST is not the attach verb", func(t *testing.T) {
		s := attachTestServer(t, &fakeHitlGateway{})
		rec := doReq(t, s.Handler(), http.MethodPost, "/api/sessions/conv-1/stream", "alice", nil)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405: %s", rec.Code, rec.Body.String())
		}
	})
}

// TestSessionStreamCarriesTheResumedTurn: what the browser that restores a card
// and answers it needs -- the resolution and the run's continuation, on the
// stream it attached to.
func TestSessionStreamCarriesTheResumedTurn(t *testing.T) {
	parked := questionRecord("ask_1", attachSession)
	parked.RunID = "run-1"
	gw := &fakeHitlGateway{pendingQuestions: []ws.QuestionRecord{parked}}
	s := attachTestServer(t, gw)
	// The route a live relay or the recovery path records; question_resolved
	// carries no session key and is addressed through it.
	s.qroutes.put("ask_1", attachSession)

	rr := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := httptest.NewRequest(http.MethodGet, "/api/sessions/conv-1/stream", nil)
		req.Header.Set("X-CubePilot-User", "alice")
		s.Handler().ServeHTTP(rr, req)
	}()
	waitFor(t, "the attach to subscribe", func() bool { return len(gw.subscribedSessions()) == 1 })
	onEvent := gw.eventSink()
	if onEvent == nil {
		t.Fatal("conn did not register an OnEvent router")
	}

	// The answer lands: the gateway resolves the question and the run resumes.
	s.relayQuestionResolved(ws.QuestionResolved{ID: "ask_1", Status: "answered"})
	onEvent("chat", []byte(`{"sessionKey":"`+attachSession+`","runId":"run-1","state":"delta","deltaText":"replicas: 1"}`))
	onEvent("chat", []byte(`{"sessionKey":"`+attachSession+`","runId":"run-1","state":"final","deltaText":"replicas: 1"}`))

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the attach stream did not end with the run")
	}

	byType := map[string]map[string]any{}
	order := []string{}
	for _, ev := range sseEvents(t, rr.Body.String()) {
		typ, _ := ev["type"].(string)
		byType[typ] = ev
		order = append(order, typ)
	}
	if byType["question_resolved"] == nil || byType["question_resolved"]["message"] != "answered" {
		t.Errorf("events = %v, want the resolution on the attached stream", order)
	}
	if byType["message_delta"] == nil || byType["message_delta"]["delta"] != "replicas: 1" {
		t.Errorf("events = %v, want the resumed run's text", order)
	}
	if byType["message_done"] == nil {
		t.Errorf("events = %v, want a terminal message_done", order)
	}
	if err, ok := byType["message_done"]["error"]; ok && err != nil && err != "" {
		t.Errorf("message_done carried %v, want a clean end", err)
	}
}

// TestQuestionRelayLogsADroppedPush: a push with no stream to carry it used to
// vanish silently, which is what made issue #167 hard to see.
func TestQuestionRelayLogsADroppedPush(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	s := platformTestServer(t)
	s.relayQuestionRequested(questionRecord("ask_1", attachSession))

	out := buf.String()
	if !strings.Contains(out, "ask_1") || !strings.Contains(out, "push dropped") {
		t.Fatalf("log = %q, want the dropped push named", out)
	}
}
