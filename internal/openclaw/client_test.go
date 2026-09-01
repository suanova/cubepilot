package openclaw

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClient_ListSessions(t *testing.T) {
	// The gateway nests the sessions under result.details; a missing title
	// falls back to the session key.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/tools/invoke" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("Authorization = %q, want Bearer secret", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"details":{"count":2,"sessions":[
			{"key":"s1","title":"Chat A"},
			{"key":"s2"}
		]}}}`))
	}))
	defer srv.Close()

	sessions, err := New(srv.URL, "secret").ListSessions(t.Context())
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("got %d sessions, want 2", len(sessions))
	}
	if sessions[0].SessionKey != "s1" || sessions[0].Title != "Chat A" {
		t.Errorf("sessions[0] = %+v, want key s1 / title Chat A", sessions[0])
	}
	if sessions[1].SessionKey != "s2" || sessions[1].Title != "s2" {
		t.Errorf("sessions[1] = %+v, want title to fall back to the key", sessions[1])
	}
}

func TestClient_ListSessions_PropagatesGatewayError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":false,"error":{"type":"auth","message":"unauthorized"}}`))
	}))
	defer srv.Close()

	_, err := New(srv.URL, "bad").ListSessions(t.Context())
	if err == nil {
		t.Fatal("expected an error for a failed tools/invoke, got nil")
	}
	if !strings.Contains(err.Error(), "unauthorized") {
		t.Errorf("error = %q, want the gateway message", err.Error())
	}
}

func TestClient_GetHistory(t *testing.T) {
	raw := `{"messages":[{"role":"user","content":"hi"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/sessions/conv-abc/history" {
			t.Errorf("path = %s, want /sessions/conv-abc/history", r.URL.Path)
		}
		if got := r.URL.Query().Get("limit"); got != "50" {
			t.Errorf("limit = %q, want 50", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("Authorization = %q, want Bearer secret", got)
		}
		_, _ = w.Write([]byte(raw))
	}))
	defer srv.Close()

	got, err := New(srv.URL, "secret").GetHistory(t.Context(), "conv-abc", 50)
	if err != nil {
		t.Fatalf("GetHistory: %v", err)
	}
	if string(got) != raw {
		t.Errorf("history = %s, want %s", got, raw)
	}
	// The raw JSON is consumed as-is, so it must stay decodable.
	var dec map[string]any
	if err := json.Unmarshal(got, &dec); err != nil {
		t.Errorf("history is not valid JSON: %v", err)
	}
}

func TestClient_GetHistory_DefaultsLimit(t *testing.T) {
	var limit string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit = r.URL.Query().Get("limit")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	if _, err := New(srv.URL, "s").GetHistory(t.Context(), "conv-1", 0); err != nil {
		t.Fatalf("GetHistory: %v", err)
	}
	if limit != "100" {
		t.Errorf("default limit = %q, want 100", limit)
	}
}

func TestClient_SetModel_SendsOverrideHeader(t *testing.T) {
	var gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("x-openclaw-model")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := New(srv.URL, "secret")
	c.SetModel("gpt-4o")
	if err := c.StreamChat(t.Context(), ChatParams{Messages: []ChatMessage{{Role: "user", Content: "hi"}}}, func(Event) error { return nil }); err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	if gotHeader != "gpt-4o" {
		t.Errorf("x-openclaw-model = %q, want gpt-4o", gotHeader)
	}
}

func TestClient_NoModelOverride_OmitsHeader(t *testing.T) {
	var gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("x-openclaw-model")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	if err := New(srv.URL, "secret").StreamChat(t.Context(), ChatParams{Messages: []ChatMessage{{Role: "user", Content: "hi"}}}, func(Event) error { return nil }); err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	if gotHeader != "" {
		t.Errorf("x-openclaw-model = %q, want empty (no override set)", gotHeader)
	}
}
