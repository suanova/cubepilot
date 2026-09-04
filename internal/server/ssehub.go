package server

import (
	"net/http"
	"sync"
	"time"

	"github.com/suanova/cubepilot/internal/openclaw"
)

// per-session SSE hub (issue #20 HITL). One active chat turn streams to the
// browser per session; events that originate on OTHER connections (the gateway
// WebSocket approval client) must be injected into that open stream. A Stream
// serialises every writeSSE (turn events, external confirm_* events, heartbeats)
// behind one mutex so the ResponseWriter is never written concurrently, and a
// heartbeat keeps the stream alive while an exec approval parks the turn.

const (
	// sseHeartbeatInterval is how often an idle stream emits a comment line so
	// proxies/browsers do not drop a stream parked on a pending approval.
	sseHeartbeatInterval = 15 * time.Second
	// sseHeartbeatIdle is the inactivity threshold after which a heartbeat fires.
	sseHeartbeatIdle = 15 * time.Second
)

// Hub tracks the active SSE stream per session key.
type Hub struct {
	mu      sync.Mutex
	active  map[string]*Stream
	nowFunc func() time.Time // overridable in tests
}

// NewHub returns an empty Hub.
func NewHub() *Hub {
	return &Hub{active: map[string]*Stream{}, nowFunc: time.Now}
}

// Open registers a new stream for the session and returns it. It fails when the
// session already has an active stream (one turn at a time per conversation).
func (h *Hub) Open(sessionKey string, w http.ResponseWriter, flusher http.Flusher) (*Stream, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.active[sessionKey]; ok {
		return nil, errStreamActive
	}
	s := &Stream{
		key:       sessionKey,
		hub:       h,
		w:         w,
		flusher:   flusher,
		closedCh:  make(chan struct{}),
		hbStop:    make(chan struct{}),
		lastWrite: h.nowFunc(),
	}
	h.active[sessionKey] = s
	return s, nil
}

// errStreamActive reports a concurrent turn for the same session.
var errStreamActive = &streamConflictError{}

type streamConflictError struct{}

func (*streamConflictError) Error() string { return "an active stream already exists for this session" }

// IsStreamConflict reports whether err is a duplicate-stream conflict.
func IsStreamConflict(err error) bool {
	_, ok := err.(*streamConflictError)
	return ok
}

// PublishTo injects an event into the session's active stream, if any. It is a
// best-effort, non-blocking on the caller (used from the WS approval goroutine);
// returns false when no stream is active or the stream has closed.
func (h *Hub) PublishTo(sessionKey string, ev openclaw.Event) bool {
	h.mu.Lock()
	s, ok := h.active[sessionKey]
	h.mu.Unlock()
	if !ok {
		return false
	}
	return s.inject(ev) == nil
}

// Active reports whether the session currently has an open stream.
func (h *Hub) Active(sessionKey string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.active[sessionKey]
	return ok
}

// Stream is one open SSE response for a session.
type Stream struct {
	key     string
	hub     *Hub
	w       http.ResponseWriter
	flusher http.Flusher

	mu        sync.Mutex
	lastWrite time.Time
	closed    bool
	closedCh  chan struct{}
	hbStop    chan struct{}
	hbOnce    sync.Once
}

// Start begins the idle heartbeat goroutine (idempotent).
func (s *Stream) Start() {
	s.hbOnce.Do(func() {
		go s.heartbeat()
	})
}

// Send writes one event to the browser under the stream lock and returns the
// write error (if any) so the caller aborts the turn exactly like today's emit.
func (s *Stream) Send(ev openclaw.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errStreamClosed
	}
	if err := writeSSE(s.w, ev); err != nil {
		return err
	}
	s.flusher.Flush()
	s.lastWrite = s.hub.nowFunc()
	return nil
}

// inject is the external-event path (approvals arriving on the WS connection).
func (s *Stream) inject(ev openclaw.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errStreamClosed
	}
	if err := writeSSE(s.w, ev); err != nil {
		return err
	}
	s.flusher.Flush()
	s.lastWrite = s.hub.nowFunc()
	return nil
}

// Close unregisters the stream and stops its heartbeat.
func (s *Stream) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	close(s.closedCh)
	s.mu.Unlock()
	s.hub.remove(s.key, s)
	close(s.hbStop)
}

func (h *Hub) remove(key string, s *Stream) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if cur, ok := h.active[key]; ok && cur == s {
		delete(h.active, key)
	}
}

// heartbeat emits SSE comment lines while the stream is idle so a pending
// approval does not let proxies drop the connection.
func (s *Stream) heartbeat() {
	t := time.NewTicker(sseHeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-s.hbStop:
			return
		case <-s.closedCh:
			return
		case now := <-t.C:
			s.mu.Lock()
			idle := now.Sub(s.lastWrite) >= sseHeartbeatIdle && !s.closed
			s.mu.Unlock()
			if !idle {
				continue
			}
			s.mu.Lock()
			if s.closed {
				s.mu.Unlock()
				return
			}
			// SSE comment line: ignored by the browser, keeps the stream alive.
			_, err := s.w.Write([]byte(": ping\n\n"))
			if err == nil {
				s.flusher.Flush()
				s.lastWrite = now
			}
			s.mu.Unlock()
		}
	}
}

var errStreamClosed = &streamClosedError{}

type streamClosedError struct{}

func (*streamClosedError) Error() string { return "stream closed" }
