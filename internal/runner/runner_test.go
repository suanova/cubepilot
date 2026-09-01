package runner

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeManager is a minimal manager double for exercising Runner.RunTask.
type fakeManager struct {
	baseURL      string
	model        string
	modelErr     error
	ensureCalled bool
}

// Ensure records that warming was requested for user.
func (f *fakeManager) Ensure(ctx context.Context, user string) error {
	f.ensureCalled = true
	return nil
}

// BaseURL returns the fake gateway URL.
func (f *fakeManager) BaseURL(user string) string { return f.baseURL }

// SelectedModelFor returns the configured model override, or the modelErr.
func (f *fakeManager) SelectedModelFor(ctx context.Context, user string) (string, error) {
	return f.model, f.modelErr
}

func TestRunTask_CollectsDeltasAndSendsModelOverride(t *testing.T) {
	// The gateway streams one content delta then finishes; the collected text
	// must be the concatenation of deltas, and the resolved model must travel
	// as the x-openclaw-model override header.
	sse := "" +
		"data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"Hello, \"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"world.\"}}]}\n\n" +
		"data: [DONE]\n\n"

	gotHeader := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader <- r.Header.Get("x-openclaw-model")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	mgr := &fakeManager{baseURL: srv.URL, model: "gpt-4o"}
	out, err := New(mgr, "secret").RunTask(t.Context(), "creator", "conv-1", "say hi")
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if out != "Hello, world." {
		t.Errorf("output = %q, want collected deltas", out)
	}
	if !mgr.ensureCalled {
		t.Error("Ensure was not called before the turn")
	}
	// The handler runs on the server's goroutine; hand its capture back over a
	// buffered channel so the read is synchronized (no data race under -race).
	if h := <-gotHeader; h != "gpt-4o" {
		t.Errorf("x-openclaw-model = %q, want gpt-4o", h)
	}
}

func TestRunTask_NoModelOverrideWithoutSelection(t *testing.T) {
	// With an empty SelectedModelFor the request must NOT carry the override
	// header (the runtime's configured model is used).
	gotHeader := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader <- r.Header.Get("x-openclaw-model")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	r := New(&fakeManager{baseURL: srv.URL}, "secret")
	if _, err := r.RunTask(t.Context(), "creator", "conv-1", "hi"); err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if h := <-gotHeader; h != "" {
		t.Errorf("x-openclaw-model = %q, want empty", h)
	}
}

func TestRunTask_SurfacesStreamedGatewayError(t *testing.T) {
	// A streamed error (agent run failed) must surface as a RunTask error
	// instead of finishing the turn with no content.
	sse := "" +
		"data: {\"error\":{\"message\":\"The agent run failed before producing a reply.\",\"type\":\"api_error\"}}\n\n" +
		"data: [DONE]\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	_, err := New(&fakeManager{baseURL: srv.URL}, "secret").RunTask(t.Context(), "creator", "conv-1", "hi")
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "The agent run failed before producing a reply.") {
		t.Errorf("error = %q, want the streamed message", err.Error())
	}
}

func TestRunTask_ModelResolutionFailureIsFailClosed(t *testing.T) {
	// An unavailable model selection must fail the turn before any gateway
	// call (design §3.2/§3.3: fail-closed on an unavailable selection).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("gateway must not be called when model resolution fails")
	}))
	defer srv.Close()

	mgr := &fakeManager{baseURL: srv.URL, modelErr: errors.New("model not in catalog")}
	if _, err := New(mgr, "secret").RunTask(t.Context(), "creator", "conv-1", "hi"); err == nil {
		t.Fatal("expected model resolution error, got nil")
	}
}
