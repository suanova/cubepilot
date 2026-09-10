package server

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// Close signals closedCh BEFORE it unregisters, so a waiter that trusts the
// channel alone returns while the hub still lists the stream -- and the client's
// very next POST /api/messages then races hub.Open into a 409.
//
// The interleaving is constructed directly (closedCh closed, still registered)
// rather than by holding h.mu from the test: WaitIdle takes that same lock, so
// holding it would park the waiter in Lock() and never exercise the channel
// path at all -- the test would pass for the wrong reason.
//
// This is a deliberate, deterministic unit check of the membership re-check, and
// that is ALL it is: it fabricates the close(closedCh)-before-remove window by
// hand and never calls Close, so it does not cover the real close path. Do not
// mistake it for that coverage -- TestHubWaitIdleUnblocksOnRealClose below is.
func TestHubWaitIdleRechecksMembership(t *testing.T) {
	h := NewHub()
	w := httptest.NewRecorder()
	s, err := h.Open("conv-1", w, w)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// Exactly the window inside Stream.Close between close(closedCh) and
	// hub.remove: the channel is closed, the stream is still listed.
	s.mu.Lock()
	s.closed = true
	close(s.closedCh)
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := h.WaitIdle(ctx, "conv-1"); err == nil {
		t.Fatal("WaitIdle returned nil while the stream was still registered")
	}

	h.remove("conv-1", s)

	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	if err := h.WaitIdle(ctx2, "conv-1"); err != nil {
		t.Fatalf("WaitIdle after removal: %v", err)
	}
}

func TestHubWaitIdleReturnsImmediatelyWhenIdle(t *testing.T) {
	h := NewHub()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := h.WaitIdle(ctx, "conv-missing"); err != nil {
		t.Fatalf("WaitIdle on an idle session: %v", err)
	}
}

// TestHubWaitIdleUnblocksOnRealClose drives the REAL Stream.Close while a
// waiter is parked, which is the production shape: the abort endpoint parks in
// WaitIdle, the SSE handler's deferred s.Close() fires, and the caller must see
// success -- and above all the client's very next send must not 409.
//
// TestHubWaitIdleRechecksMembership above fabricates the window by hand; this is
// the coverage of the actual close path. An implementation that returns a
// sentinel from the closedCh wake instead of re-checking membership passes the
// existing tests but fails here: the stream is genuinely gone, so a wake that
// reports failure is wrong.
func TestHubWaitIdleUnblocksOnRealClose(t *testing.T) {
	h := NewHub()
	rec := httptest.NewRecorder()
	s, err := h.Open("conv-close", rec, rec)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// Buffered so the waiter never blocks handing its result back. The
	// result channel plus the generous timeout below is what keeps this
	// non-flaky: Close happens outside WaitIdle's own wait, so the assertion
	// races nothing.
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		done <- h.WaitIdle(ctx, "conv-close")
	}()

	// Let the waiter reach the parked state under test. A longer pause can only
	// make the test slower, never fail it spuriously: were the waiter still
	// unscheduled, it would return nil on the first membership check anyway.
	time.Sleep(50 * time.Millisecond)

	s.Close()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WaitIdle after a real Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WaitIdle did not return after Stream.Close")
	}

	// The property the abort endpoint actually promises its caller: Stop having
	// returned implies the follow-up send will not race hub.Open into a 409.
	if _, err := h.Open("conv-close", httptest.NewRecorder(), rec); err != nil {
		t.Fatalf("Open after WaitIdle returned: %v", err)
	}
}
