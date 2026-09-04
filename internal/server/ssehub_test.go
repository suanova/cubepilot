package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/suanova/cubepilot/internal/openclaw"
)

func TestSSEHub_OpenPublishAndClose(t *testing.T) {
	h := NewHub()
	rec := httptest.NewRecorder()
	// *httptest.ResponseRecorder implements http.Flusher.
	s, err := h.Open("conv-1", rec, rec)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !h.Active("conv-1") {
		t.Fatal("expected conv-1 to be active after Open")
	}

	// Turn event goes through Send.
	if err := s.Send(openclaw.Event{Type: openclaw.EventMessageStart, SessionID: "conv-1"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	// External (WS-originated) event goes through PublishTo.
	if !h.PublishTo("conv-1", openclaw.Event{Type: openclaw.EventConfirmPending, SessionID: "conv-1", CallID: "appr-1"}) {
		t.Fatal("PublishTo: expected true for an active stream")
	}

	body := rec.Body.String()
	if !strings.Contains(body, "event: message_start") || !strings.Contains(body, `"type":"message_start"`) {
		t.Errorf("missing message_start in stream body: %q", body)
	}
	if !strings.Contains(body, "event: confirm_pending") || !strings.Contains(body, `"call_id":"appr-1"`) {
		t.Errorf("missing confirm_pending in stream body: %q", body)
	}

	// Duplicate Open conflicts while the first stream is live.
	if _, err := h.Open("conv-1", httptest.NewRecorder(), rec); !IsStreamConflict(err) {
		t.Errorf("second Open: want IsStreamConflict, got %v", err)
	}

	// Close unregisters; further publishes and sends fail.
	s.Close()
	if h.Active("conv-1") {
		t.Fatal("expected conv-1 inactive after Close")
	}
	if h.PublishTo("conv-1", openclaw.Event{Type: openclaw.EventMessageDone}) {
		t.Fatal("PublishTo after Close: expected false")
	}
	if err := s.Send(openclaw.Event{Type: openclaw.EventMessageDone}); err == nil {
		t.Fatal("Send after Close: expected an error")
	}

	// Reopen works after close.
	if _, err := h.Open("conv-1", httptest.NewRecorder(), rec); err != nil {
		t.Errorf("reopen after close: %v", err)
	}
}

func TestSSEHub_PublishToNoStream(t *testing.T) {
	h := NewHub()
	if h.Active("conv-missing") {
		t.Fatal("expected not active for unknown session")
	}
	if h.PublishTo("conv-missing", openclaw.Event{Type: openclaw.EventConfirmPending}) {
		t.Fatal("PublishTo on unknown session: expected false")
	}
}
