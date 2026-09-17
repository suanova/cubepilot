package server

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// captureLog redirects the stdlib logger for the duration of fn and returns
// what was written. s.logf goes through log.Printf.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&buf)
	defer func() {
		log.SetOutput(prev)
		log.SetFlags(prevFlags)
	}()
	fn()
	return buf.String()
}

func TestLogRequestsRecordsMethodPathAndStatus(t *testing.T) {
	s := &Server{}
	h := s.logRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("hello"))
	}))

	got := captureLog(t, func() {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil))
	})

	for _, want := range []string{"GET", "/api/v1/sessions", "418", "5B"} {
		if !strings.Contains(got, want) {
			t.Errorf("access line %q is missing %q", got, want)
		}
	}
}

func TestLogRequestsSkipsProbePaths(t *testing.T) {
	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		if !isProbePath(path) {
			t.Errorf("isProbePath(%q) = false, want true", path)
		}
	}

	s := &Server{}
	h := s.logRequests(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	got := captureLog(t, func() {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))
	})
	if got != "" {
		t.Errorf("probe path was logged: %q", got)
	}
}

// TestLogRequestsSkipsInternalAPIPaths guards the supervisor-to-api surface:
// the poll loop hits these routes every 10s, and without the skip that is
// thousands of access-log lines a day per agent Pod.
func TestLogRequestsSkipsInternalAPIPaths(t *testing.T) {
	for _, path := range []string{"/internal/agents/admin/config", "/internal/gateway/config/admin"} {
		if !isInternalAPIPath(path) {
			t.Errorf("isInternalAPIPath(%q) = false, want true", path)
		}
	}

	s := &Server{}
	h := s.logRequests(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	for _, path := range []string{"/internal/agents/admin/config", "/internal/gateway/config/admin"} {
		got := captureLog(t, func() {
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
		})
		if got != "" {
			t.Errorf("internal api path %q was logged: %q", path, got)
		}
	}
}

// SSE depends on this: handlers.go:153 asserts w.(http.Flusher), so a wrapper
// that swallows it breaks every streaming response.
func TestLogRequestsPreservesFlusher(t *testing.T) {
	s := &Server{}
	var got bool
	h := s.logRequests(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, got = w.(http.Flusher)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/sessions/x/messages", nil))
	if !got {
		t.Fatal("the wrapped ResponseWriter no longer satisfies http.Flusher: SSE would break")
	}
}
