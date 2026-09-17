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

// Log injection: URL.Path is percent-decoded, so a request for /%0aFAKE_RECORD
// arrives carrying a real newline. Logging it raw splits one request into two
// records, and the attacker chooses the second one -- enough to forge a line in
// an audit trail. EscapedPath keeps every request on exactly one line.
func TestLogRequestsKeepsOneRecordPerRequest(t *testing.T) {
	s := &Server{}
	h := s.logRequests(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeNotFound(w, "no such endpoint")
	}))

	got := captureLog(t, func() {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/%0aFAKE_RECORD", nil))
	})

	if trimmed := strings.TrimRight(got, "\n"); strings.Contains(trimmed, "\n") {
		t.Errorf("a decoded path injected a second log line: %q", got)
	}
	if !strings.Contains(got, "%0aFAKE_RECORD") {
		t.Errorf("the path should still be visible, escaped: %q", got)
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
	// Wrapped so the access line for this request does not leak into the test
	// output of every run.
	captureLog(t, func() {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/sessions/x/messages", nil))
	})
	if !got {
		t.Fatal("the wrapped ResponseWriter no longer satisfies http.Flusher: SSE would break")
	}
}

// The logged status must be the status the client actually received. net/http
// ignores a second WriteHeader and treats 1xx as provisional, so recording the
// latest value would both report a status that was never sent and let a
// provisional 103 become the final answer.
func TestLogRequestsRecordsTheEffectiveStatus(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler func(w http.ResponseWriter)
		want    string
	}{
		{
			name: "a repeated WriteHeader logs the first final status",
			handler: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusOK)
				w.WriteHeader(http.StatusInternalServerError)
			},
			want: "200",
		},
		{
			name: "Early Hints are provisional, not the final status",
			handler: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusEarlyHints)
				w.WriteHeader(http.StatusTeapot)
			},
			want: "418",
		},
		{
			name: "a body written without WriteHeader is a 200",
			handler: func(w http.ResponseWriter) {
				_, _ = w.Write([]byte("hi"))
			},
			want: "200",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{}
			h := s.logRequests(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				tc.handler(w)
			}))
			got := captureLog(t, func() {
				h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil))
			})
			if !strings.Contains(got, " "+tc.want+" ") {
				t.Errorf("access line %q does not report status %s", got, tc.want)
			}
		})
	}
}
