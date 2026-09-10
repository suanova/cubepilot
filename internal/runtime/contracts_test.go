package runtime

import (
	"context"
	"encoding/json"
	"testing"
)

type fakeLiveRunner struct{ called bool }

func (f *fakeLiveRunner) RunLiveTurn(_ context.Context, _ string, _ LiveTurnParams, _ func(Event) error) (TurnOutcome, error) {
	f.called = true
	return TurnOutcome{}, nil
}

type fakeOneShotRunner struct {
	model  string
	called bool
}

func (f *fakeOneShotRunner) StreamChat(_ context.Context, params ChatParams, _ func(Event) error) error {
	f.called = true
	f.model = params.Model
	return nil
}

type fakeSessionReader struct{ listed bool }

func (f *fakeSessionReader) ListSessions(context.Context) ([]Session, error) {
	f.listed = true
	return []Session{{SessionKey: "session-1"}}, nil
}
func (f *fakeSessionReader) GetHistory(context.Context, string, int) (json.RawMessage, error) {
	return json.RawMessage(`{"items":[]}`), nil
}

func TestComposeExposesCompleteAgentRuntime(t *testing.T) {
	live := &fakeLiveRunner{}
	oneShot := &fakeOneShotRunner{}
	sessions := &fakeSessionReader{}
	rt := Compose(live, oneShot, sessions)

	if _, err := rt.RunLiveTurn(t.Context(), "session-1", LiveTurnParams{Message: "hello"}, func(Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := rt.StreamChat(t.Context(), ChatParams{Model: "provider/model"}, func(Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	got, err := rt.ListSessions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !live.called || !oneShot.called || oneShot.model != "provider/model" || !sessions.listed || len(got) != 1 {
		t.Fatalf("composite did not delegate all surfaces: live=%v oneShot=%v model=%q sessions=%v got=%v", live.called, oneShot.called, oneShot.model, sessions.listed, got)
	}
}

// A stopped turn must be tellable apart from a completed one on the wire: the
// field is omitted when false so an unstopped turn is byte-identical to today.
func TestEventStoppedMarshals(t *testing.T) {
	plain, err := json.Marshal(Event{Type: EventMessageDone})
	if err != nil {
		t.Fatalf("marshal plain: %v", err)
	}
	if string(plain) != `{"type":"message_done"}` {
		t.Fatalf("plain message_done changed shape: %s", plain)
	}

	stopped, err := json.Marshal(Event{Type: EventMessageDone, Stopped: true})
	if err != nil {
		t.Fatalf("marshal stopped: %v", err)
	}
	if string(stopped) != `{"type":"message_done","stopped":true}` {
		t.Fatalf("stopped message_done = %s", stopped)
	}
}
