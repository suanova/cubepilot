package runtime

import (
	"context"
	"encoding/json"
	"testing"
)

type fakeLiveRunner struct{ called bool }

func (f *fakeLiveRunner) RunLiveTurn(_ context.Context, _ string, _ LiveTurnParams, _ func(Event) error) error {
	f.called = true
	return nil
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

	if err := rt.RunLiveTurn(t.Context(), "session-1", LiveTurnParams{Message: "hello"}, func(Event) error { return nil }); err != nil {
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
