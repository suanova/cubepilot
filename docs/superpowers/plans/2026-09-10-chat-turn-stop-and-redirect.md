# Stop and Redirect an In-Flight Chat Turn — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a Portal user stop a running chat turn, redirect it by sending a new message mid-turn, and see that a turn is still running after a reload — and stop it from there.

**Architecture:** The gateway already exposes the primitives (`chat.abort`, `chat.history`'s `inFlightRun`); CubePilot only has to wire them up. Redirect is `abort` then a normal send (not `queueMode:"interrupt"`), which keeps `RunLiveTurn`'s run-correlation guard untouched. The turn's outcome travels back from the runtime over a new typed `TurnOutcome` because the terminal `message_done` is emitted by the HTTP handler, not the runtime.

**Tech Stack:** Go 1.x (net/http, controller-runtime client), gateway WebSocket protocol v4, React 19 + TypeScript + Vite.

Spec: `docs/superpowers/specs/2026-09-10-chat-turn-stop-and-redirect-design.md`

## Global Constraints

- All code, comments, identifiers, and commit messages in English.
- No GitHub issue/PR numbers, milestone labels, or roadmap phase names in user-visible UI copy. Source comments may reference them.
- Commits use `-s` (DCO `Signed-off-by`) and append `Assisted-by: Claude Code`.
- `message_done` remains the **only** terminal SSE event. `stopped` and `error` are mutually exclusive; `stopped: true` always means an empty `error`.
- A stopped turn **keeps** its partial text. Stopping is not a rollback.
- Do not change `RunLiveTurn`'s run-id correlation (`liveTurn.acceptRun`). Task 1 changes only its return type.
- Go checks: `go build ./...`, `go vet ./...`, `go test ./...`. Web: `npm run build` in `web/`.

---

### Task 1: Typed turn outcome across the runtime boundary

The terminal `message_done` is emitted by `handleMessages`, and the only thing crossing the runtime boundary today is an `error`. Without this task, a stopped turn is indistinguishable from a completed one.

**Files:**
- Modify: `internal/runtime/contracts.go:106-110`, `internal/runtime/contracts.go:54-70`
- Modify: `internal/server/hitl.go:65-75`, `internal/server/hitl.go:585`, `internal/server/hitl.go:120-156`, `internal/server/hitl.go:690-712`
- Modify: `internal/server/handlers.go:204-216`
- Test: `internal/runtime/contracts_test.go` (create if absent)

**Interfaces:**
- Produces: `agentruntime.TurnOutcome{Stopped bool}`; `LiveTurnRunner.RunLiveTurn(...) (TurnOutcome, error)`; `agentruntime.Event.Stopped bool` (`json:"stopped,omitempty"`); `liveTurn.finishWith(err error, stopped bool)`; `liveTurn.outcome() TurnOutcome`.
- Consumes: nothing from earlier tasks.

- [ ] **Step 1: Write the failing test**

Create `internal/runtime/contracts_test.go`:

```go
package runtime

import (
	"encoding/json"
	"testing"
)

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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/runtime/ -run TestEventStoppedMarshals -v`
Expected: FAIL — `unknown field Stopped in struct literal`.

- [ ] **Step 3: Add the field and the outcome type**

In `internal/runtime/contracts.go`, add to `Event` after `Error`:

```go
	// Stopped reports that the turn ended because the user stopped it, as
	// opposed to failing or completing. Only ever set on message_done, and
	// never together with Error.
	Stopped bool `json:"stopped,omitempty"`
```

Add above `LiveTurnRunner`:

```go
// TurnOutcome reports how a live turn ended when it did not fail. The terminal
// message_done is emitted by the HTTP handler, so a runtime that merely returns
// a nil error cannot express "stopped by request" -- that needs its own channel.
type TurnOutcome struct {
	// Stopped is true when the turn was cancelled by request (the chat.abort
	// RPC or the gateway's /stop command), not by a failure and not by
	// completing normally.
	Stopped bool
}
```

Change the interface:

```go
type LiveTurnRunner interface {
	RunLiveTurn(ctx context.Context, sessionKey string, params LiveTurnParams, emit func(Event) error) (TurnOutcome, error)
}
```

- [ ] **Step 4: Run the contract test**

Run: `go test ./internal/runtime/ -run TestEventStoppedMarshals -v`
Expected: PASS.

- [ ] **Step 5: Record the outcome on liveTurn**

In `internal/server/hitl.go`, add two fields to `liveTurn` (next to `doneErr`):

```go
	done    chan struct{}
	once    sync.Once
	doneErr error
	// doneStopped is written inside once.Do and read only through outcome(),
	// which is mutex-guarded so the agent.wait tail path cannot race it.
	resMu       sync.Mutex
	doneStopped bool
```

Replace `finish` (currently `internal/server/hitl.go:152-157`) with:

```go
// finish marks the turn terminal as neither stopped nor failed (idempotent).
func (t *liveTurn) finish(err error) {
	t.finishWith(err, false)
}

// finishWith marks the turn terminal with its outcome (idempotent). stopped
// records that the terminal frame was a request-initiated abort.
func (t *liveTurn) finishWith(err error, stopped bool) {
	t.once.Do(func() {
		t.resMu.Lock()
		t.doneErr = err
		t.doneStopped = stopped
		t.resMu.Unlock()
		close(t.done)
	})
}

// outcome reports how the turn ended. Safe to call after terminal and from the
// agent.wait tail path where t.done did not close.
func (t *liveTurn) outcome() agentruntime.TurnOutcome {
	t.resMu.Lock()
	defer t.resMu.Unlock()
	return agentruntime.TurnOutcome{Stopped: t.doneStopped}
}
```

- [ ] **Step 6: Produce the outcome from the terminal frame**

In `routeLive` (`internal/server/hitl.go`, the `if terminal {` block near line 700), replace:

```go
	if terminal {
		// A chat "error"/"aborted" frame is a terminal failure; surface its text
		// so the caller emits message_done with an error instead of a plain done.
		var err error
		if evName == "chat" {
			err = chatTerminalErr(payload)
		}
		t.finish(err)
	}
```

with:

```go
	if terminal {
		// A chat "error"/"aborted" frame is terminal. A request-initiated abort
		// is not a failure, so it is reported as an outcome rather than an error.
		var (
			err     error
			stopped bool
		)
		if evName == "chat" {
			err, stopped = chatTerminalOutcome(payload)
		}
		t.finishWith(err, stopped)
	}
```

Replace `chatTerminalErr` (`internal/server/hitl.go:717-736`) with `chatTerminalOutcome`:

```go
// chatTerminalOutcome classifies a terminal chat frame (state "error"/"aborted").
// OpenClaw carries the diagnostic under errorMessage (and sometimes error) and
// the cancel origin under stopReason. A request-initiated stop -- stopReason
// "rpc" for the chat.abort RPC, "stop" for the /stop command path -- is an
// outcome, not a failure: it returns a nil error and stopped=true, so the
// caller emits message_done{stopped:true} instead of message_done{error}.
// Any other abort (timeout, restart, auth-revoked) stays an error.
func chatTerminalOutcome(payload []byte) (error, bool) {
	var st struct {
		State        string `json:"state"`
		Error        string `json:"error"`
		ErrorMessage string `json:"errorMessage"`
		StopReason   string `json:"stopReason"`
	}
	if json.Unmarshal(payload, &st) != nil || (st.State != "error" && st.State != "aborted") {
		return nil, false
	}
	if st.StopReason == "rpc" || st.StopReason == "stop" {
		return nil, true
	}
	msg := st.ErrorMessage
	if msg == "" {
		msg = st.Error
	}
	if msg == "" {
		msg = "agent run " + st.State
	}
	return fmt.Errorf("%s", msg), false
}
```

- [ ] **Step 7: Update the two RunLiveTurn implementations**

`openClawLiveRunner.RunLiveTurn` (`internal/server/hitl.go:65`):

```go
func (r *openClawLiveRunner) RunLiveTurn(ctx context.Context, sessionKey string, params agentruntime.LiveTurnParams, emit func(agentruntime.Event) error) (agentruntime.TurnOutcome, error) {
	if r.manager == nil {
		return agentruntime.TurnOutcome{}, fmt.Errorf("live chat unavailable: runtime live channel is not configured")
	}
	guarded, err := r.manager.PreTurn(ctx, r.user)
	if err != nil {
		return agentruntime.TurnOutcome{}, fmt.Errorf("confirmation gating failed: %w", err)
	}
	return r.manager.RunLiveTurn(ctx, r.user, sessionKey, params.Message, params.Model, guarded, emit)
}
```

`hitlManager.RunLiveTurn` (`internal/server/hitl.go:585`): change the signature to return `(agentruntime.TurnOutcome, error)` and rewrite its returns:

- every `return err` before `registerLive` becomes `return agentruntime.TurnOutcome{}, err`;
- `return t.doneErr` becomes `return t.outcome(), t.doneErr`;
- `return ctx.Err()` becomes `return agentruntime.TurnOutcome{}, ctx.Err()`;
- the fallback `return t.doneErr` after `wsRunTail` becomes `return t.outcome(), t.doneErr`.

- [ ] **Step 8: Emit the terminal from the handler**

In `internal/server/handlers.go`, replace the `runtimeAdapter.RunLiveTurn(...)` branch (around lines 204-216) with:

```go
	} else if outcome, err := runtimeAdapter.RunLiveTurn(r.Context(), sessionKey, agentruntime.LiveTurnParams{
		Message: body.Content,
		Model:   selectedModel,
	}, emitLive); err != nil {
		streamErr = err
		_ = stream.Send(agentruntime.Event{Type: agentruntime.EventMessageDone, SessionID: sessionKey, Error: err.Error()})
	} else if outcome.Stopped {
		// The user stopped the turn: terminal, but not a failure, and not a
		// normal completion -- the browser must not present the partial text as
		// a finished answer.
		_ = stream.Send(agentruntime.Event{Type: agentruntime.EventMessageDone, SessionID: sessionKey, Stopped: true})
	} else {
		_ = stream.Send(agentruntime.Event{Type: agentruntime.EventMessageDone, SessionID: sessionKey})
	}
```

- [ ] **Step 9: Fix the test fakes**

Run: `go build ./... && go vet ./...`

Every fake implementing `LiveTurnRunner` needs the new return type. Fix each compile error by returning `agentruntime.TurnOutcome{}, err` in place of `err`. Find them with:

```bash
grep -rn "RunLiveTurn" --include='*_test.go' internal/
```

- [ ] **Step 10: Run the suite**

Run: `go test ./...`
Expected: PASS.

- [ ] **Step 11: Commit**

```bash
git add internal/runtime/contracts.go internal/runtime/contracts_test.go internal/server/hitl.go internal/server/handlers.go
git add -A
git commit -s -m "feat(runtime): carry a typed turn outcome so a stop is not a failure (issue #166)

Assisted-by: Claude Code"
```

---

### Task 2: The abort endpoint's gateway wiring (ws + manager)

**Files:**
- Modify: `internal/openclaw/ws/methods.go` (add after `CancelQuestion`)

**Interfaces:**
- Produces: `(*ws.Client).AbortChat(ctx, sessionKey, runID string) error`; `(*ws.Client).SessionBusy(ctx, sessionKey string) (bool, error)`; `hitlGateway` gains both; `(*hitlManager).Abort(ctx, user, sessionKey, runID string) error`; `(*hitlManager).SessionBusy(ctx, user, sessionKey string) (bool, error)`; `(*hitlManager).LiveRunID(sessionKey string) (string, bool)`.
- Consumes: Task 1's `hitlGateway` interface (unchanged by Task 1).

- [ ] **Step 1: Write the failing ws test**

Create `internal/openclaw/ws/methods_abort_test.go`:

```go
package ws

import (
	"context"
	"encoding/json"
	"testing"
)

// AbortChat must omit runId entirely when unknown: a present-but-empty runId
// would be rejected by the gateway's schema, and omitting it is what selects the
// session-scoped abort.
func TestAbortChatOmitsEmptyRunID(t *testing.T) {
	c, calls := newTestClient(t, func(method string, params json.RawMessage) (json.RawMessage, error) {
		if method != "chat.abort" {
			t.Fatalf("method = %q, want chat.abort", method)
		}
		return json.RawMessage(`{"aborted":true}`), nil
	})

	if err := c.AbortChat(context.Background(), "session-a", ""); err != nil {
		t.Fatalf("AbortChat: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal((*calls)[0], &got); err != nil {
		t.Fatalf("decode params: %v", err)
	}
	if got["sessionKey"] != "session-a" {
		t.Fatalf("sessionKey = %v", got["sessionKey"])
	}
	if _, present := got["runId"]; present {
		t.Fatalf("runId must be omitted when empty, got %v", got["runId"])
	}
}

func TestAbortChatSendsRunID(t *testing.T) {
	c, calls := newTestClient(t, func(string, json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"aborted":true}`), nil
	})
	if err := c.AbortChat(context.Background(), "session-a", "run-7"); err != nil {
		t.Fatalf("AbortChat: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal((*calls)[0], &got); err != nil {
		t.Fatalf("decode params: %v", err)
	}
	if got["runId"] != "run-7" {
		t.Fatalf("runId = %v, want run-7", got["runId"])
	}
}

func TestSessionBusyReadsInFlightRun(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"absent", `{"kind":"delta","messages":[]}`, false},
		{"null", `{"kind":"delta","inFlightRun":null}`, false},
		{"present", `{"kind":"delta","inFlightRun":{"runId":"r1"}}`, true},
		{"reset", `{"kind":"reset"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestClient(t, func(method string, _ json.RawMessage) (json.RawMessage, error) {
				if method != "chat.history" {
					t.Fatalf("method = %q, want chat.history", method)
				}
				return json.RawMessage(tc.body), nil
			})
			got, err := c.SessionBusy(context.Background(), "session-a")
			if err != nil {
				t.Fatalf("SessionBusy: %v", err)
			}
			if got != tc.want {
				t.Fatalf("SessionBusy = %v, want %v", got, tc.want)
			}
		})
	}
}
```

Add the helper to the same file (there is no existing `newTestClient`; check `internal/openclaw/ws/*_test.go` for the established fake — if one exists, reuse it instead of adding this):

```go
// newTestClient returns a Client whose Call is served by fn, recording the raw
// params of each call in order.
func newTestClient(t *testing.T, fn func(method string, params json.RawMessage) (json.RawMessage, error)) (*Client, *[]json.RawMessage) {
	t.Helper()
	calls := &[]json.RawMessage{}
	c := &Client{}
	c.callFn = func(_ context.Context, method string, params any) (json.RawMessage, error) {
		raw, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		*calls = append(*calls, raw)
		return fn(method, raw)
	}
	return c, calls
}
```

`Client.callFn` does not exist yet — add it in Step 3.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/openclaw/ws/ -run 'TestAbortChat|TestSessionBusy' -v`
Expected: FAIL — `c.AbortChat undefined`.

- [ ] **Step 3: Implement**

If `Client` has no injectable call hook, add one. Read `internal/openclaw/ws/client.go` to find how `Call` is defined, then add:

```go
// Client:
	// callFn overrides Call in tests; nil means use the real transport.
	callFn func(ctx context.Context, method string, params any) (json.RawMessage, error)
```

and at the top of `Call`:

```go
	if c.callFn != nil {
		return c.callFn(ctx, method, params)
	}
```

(If a test hook already exists under another name, use it and drop this step.)

Append to `internal/openclaw/ws/methods.go`:

```go
// chatAbortParams is the chat.abort request body. runId is omitted entirely when
// empty: the gateway's schema rejects an empty string, and omitting it is what
// selects the session-scoped abort.
type chatAbortParams struct {
	SessionKey string `json:"sessionKey"`
	RunID      string `json:"runId,omitempty"`
}

// AbortChat cancels a run (chat.abort). runID scopes the abort to that run and
// is what the live-turn path passes; an empty runID aborts the session's active
// run and is the fallback only when no run id is known (reload takeover).
func (c *Client) AbortChat(ctx context.Context, sessionKey, runID string) error {
	if sessionKey == "" {
		return fmt.Errorf("chat.abort: empty session key")
	}
	if _, err := c.Call(ctx, "chat.abort", chatAbortParams{SessionKey: sessionKey, RunID: runID}); err != nil {
		return fmt.Errorf("chat.abort %q: %w", sessionKey, err)
	}
	return nil
}

type chatHistoryParams struct {
	SessionKey string `json:"sessionKey"`
}

// chatHistoryResult is the subset of chat.history's delta result this client
// reads. The delta payload also carries messages and a deltaCursor that a future
// stream re-attach would use; nothing here consumes them, and the gateway's
// "reset" shape simply has no inFlightRun.
type chatHistoryResult struct {
	InFlightRun json.RawMessage `json:"inFlightRun"`
}

// SessionBusy reports whether the gateway has an in-flight run for the session.
// It is the authoritative "is this session busy" signal: the SSE hub only knows
// whether a browser is attached, which is false after a reload while the run is
// still going.
func (c *Client) SessionBusy(ctx context.Context, sessionKey string) (bool, error) {
	raw, err := c.Call(ctx, "chat.history", chatHistoryParams{SessionKey: sessionKey})
	if err != nil {
		return false, fmt.Errorf("chat.history %q: %w", sessionKey, err)
	}
	var out chatHistoryResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return false, fmt.Errorf("decode chat.history: %w", err)
	}
	trimmed := strings.TrimSpace(string(out.InFlightRun))
	return trimmed != "" && trimmed != "null", nil
}
```

Add `"strings"` to the imports if absent.

- [ ] **Step 4: Run the ws tests**

Run: `go test ./internal/openclaw/ws/ -v`
Expected: PASS.

- [ ] **Step 5: Extend the HITL gateway interface and manager**

In `internal/server/hitl.go`, add to `hitlGateway`:

```go
	AbortChat(ctx context.Context, sessionKey, runID string) error
	SessionBusy(ctx context.Context, sessionKey string) (bool, error)
```

Add to `hitlManager`, next to `ReleaseLive`-adjacent accessors:

```go
// LiveRunID returns the run id of the session's active live turn, if any. The
// browser never learns the run id; the server does, and passing it scopes an
// abort so it cannot kill a run promoted after this one settles.
func (m *hitlManager) LiveRunID(sessionKey string) (string, bool) {
	m.liveMu.Lock()
	t := m.live[sessionKey]
	m.liveMu.Unlock()
	if t == nil {
		return "", false
	}
	t.runMu.Lock()
	defer t.runMu.Unlock()
	return t.runID, t.runID != ""
}

// Abort cancels the session's active run over the user's gateway connection.
func (m *hitlManager) Abort(ctx context.Context, user, sessionKey, runID string) error {
	m.liveMu.Lock()
	gw, ok := m.conns[user]
	m.liveMu.Unlock()
	if !ok || gw == nil {
		return fmt.Errorf("abort %q: no live gateway channel", sessionKey)
	}
	return gw.gw.AbortChat(ctx, sessionKey, runID)
}
```

(`m.conns` is guarded by `m.mu`, not `m.liveMu` — use `m.mu` here; check `liveConn` at `internal/server/hitl.go:477` and copy its access pattern exactly instead of the sketch above.)

```go
// SessionBusy reports whether the gateway still has an in-flight run for the
// session. Unlike the SSE hub this survives a browser disconnect, so it is the
// only signal that means anything on the reload-takeover path.
func (m *hitlManager) SessionBusy(ctx context.Context, user, sessionKey string) (bool, error) {
	m.mu.Lock()
	conn, ok := m.conns[user]
	m.mu.Unlock()
	if !ok || conn == nil {
		return false, fmt.Errorf("session busy %q: no live gateway channel", sessionKey)
	}
	return conn.gw.SessionBusy(ctx, sessionKey)
}
```

- [ ] **Step 6: Fix the fakes and run**

Run: `go build ./... && go test ./internal/server/ ./internal/openclaw/...`

Add the two methods to every fake `hitlGateway` (grep for `hitlGateway` in `*_test.go`); the fake should record the last `(sessionKey, runID)` and return a configurable busy value.

- [ ] **Step 7: Commit**

```bash
git add -A
git commit -s -m "feat(ws): wire chat.abort and the session-busy signal (issue #166)

Assisted-by: Claude Code"
```

---

### Task 3: `Hub.WaitIdle` that cannot return early

`Stream.Close` closes `closedCh` **before** unregistering from `h.active`, so a waiter woken by `closedCh` can return while `Hub.Open` still sees the old stream and answers 409 — the exact failure this exists to prevent.

**Files:**
- Modify: `internal/server/ssehub.go` (add after `Active`, around line 90)
- Test: `internal/server/ssehub_test.go`

**Interfaces:**
- Produces: `(*Hub).WaitIdle(ctx context.Context, sessionKey string) error`.
- Consumes: nothing from earlier tasks.

- [ ] **Step 1: Write the failing test**

Append to `internal/server/ssehub_test.go`:

```go
// Close signals closedCh before it unregisters, so a waiter that trusts the
// channel alone can return while the hub still lists the stream. WaitIdle must
// re-check membership and only report idle once the session is really gone.
func TestHubWaitIdleWaitsForUnregistration(t *testing.T) {
	h := NewHub()
	w := httptest.NewRecorder()
	s, err := h.Open("conv-1", w, w)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// Hold the stream between its closedCh close and its hub removal by taking
	// the hub lock, then closing from another goroutine.
	h.mu.Lock()
	go s.Close()

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		done <- h.WaitIdle(ctx, "conv-1")
	}()

	select {
	case err := <-done:
		t.Fatalf("WaitIdle returned %v while the session was still registered", err)
	case <-time.After(50 * time.Millisecond):
		// Still blocked: correct.
	}

	h.mu.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WaitIdle after unregistration: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitIdle did not return after unregistration")
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/server/ -run TestHubWaitIdle -v`
Expected: FAIL — `h.WaitIdle undefined`.

- [ ] **Step 3: Implement**

In `internal/server/ssehub.go`:

```go
// WaitIdle blocks until the session has no active stream, or ctx expires.
//
// It deliberately does NOT trust the active stream's closedCh alone: Close
// closes that channel before it unregisters, so a waiter woken by it could
// return while Open still sees the old stream and answers 409 -- the exact
// conflict this exists to prevent. Membership is re-checked under the hub lock
// after every wake.
func (h *Hub) WaitIdle(ctx context.Context, sessionKey string) error {
	for {
		h.mu.Lock()
		s, ok := h.active[sessionKey]
		h.mu.Unlock()
		if !ok {
			return nil
		}
		select {
		case <-s.closedCh:
			// Woken, but unregistration may not have happened yet: loop.
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/server/ -run TestHubWaitIdle -race -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/server/ssehub.go internal/server/ssehub_test.go
git commit -s -m "fix(sse): WaitIdle must re-check hub membership after Close wakes it (issue #166)

Assisted-by: Claude Code"
```

---

### Task 4: Settle a session's pending HITL records

**Files:**
- Create: `internal/server/settle.go`
- Test: `internal/server/settle_test.go`

**Interfaces:**
- Produces: `(*Server).settlePendingForSession(ctx context.Context, user, sessionKey string)`.
- Consumes: Task 1's `Event.Stopped` (unrelated); `ApprovalService.Pending` (`internal/server/approvals.go:104`); `hitlManager.ListQuestions` / `CancelQuestion`.

- [ ] **Step 1: Write the failing test**

Create `internal/server/settle_test.go`:

```go
// A stopped turn must not leave a card behind: after settling, the session has
// no pending confirmation, the record is gone from the recovery lookup, and a
// confirm_resolved event was published to any attached stream.
func TestSettlePendingForSessionClearsConfirm(t *testing.T) {
	h := NewHub()
	svc := NewApprovalService(h, nil, func(string, ...any) {})
	svc.Begin("admin", pendingApproval{
		ApprovalID: "ap-1",
		SessionKey: "conv-1",
		User:       "admin",
		Tool:       "exec",
		Command:    "kubectl delete pod x",
		Level:      "write",
	})

	s := &Server{hub: h, approvals: svc}
	s.settlePendingForSession(context.Background(), "admin", "conv-1")

	if _, ok := svc.Pending("admin", "conv-1"); ok {
		t.Fatal("pending confirmation survived the settle")
	}
}
```

`pendingApproval` is `internal/server/approvals.go:41-50`; `Begin` is at line 72 and stores into `bySession`/`byID`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/server/ -run TestSettlePendingForSession -v`
Expected: FAIL — `s.settlePendingForSession undefined`.

- [ ] **Step 3: Implement**

Create `internal/server/settle.go`:

```go
package server

import (
	"context"

	agentruntime "github.com/suanova/cubepilot/internal/runtime"
)

// settlePendingForSession closes out every human-in-the-loop record a stopped
// turn left behind. Aborting the run settles these gateway-side, but the
// platform keeps its own bookkeeping -- the pending confirmation and the
// question routing entries -- and that bookkeeping is exactly what powers
// reload recovery. Left alone, it resurfaces a card for a run that no longer
// exists and errors when the user clicks it.
//
// It must run while the turn's SSE stream is still open: the *_resolved events
// below are how an attached view drops the card immediately, and a later reload
// stops resurrecting it. Deleting the records silently, or waiting for the
// question timeout, are strictly worse.
func (s *Server) settlePendingForSession(ctx context.Context, user, sessionKey string) {
	if p, ok := s.approvals.Pending(user, sessionKey); ok {
		s.approvals.settle(p)
		s.hub.PublishTo(sessionKey, agentruntime.Event{
			Type:      agentruntime.EventConfirmResolved,
			SessionID: sessionKey,
			CallID:    p.ApprovalID,
		})
	}

	if s.hitl == nil {
		return
	}
	list, err := s.hitl.ListQuestions(ctx, user)
	if err != nil {
		s.logf("settle %s/%s: list questions: %v", user, sessionKey, err)
		return
	}
	for _, rec := range list {
		if canonicalSessionKey(rec.SessionKey) != sessionKey || rec.Status != "pending" {
			continue
		}
		if err := s.hitl.CancelQuestion(ctx, user, rec.ID); err != nil {
			s.logf("settle %s/%s: cancel question %s: %v", user, sessionKey, rec.ID, err)
			continue
		}
		s.qroutes.take(rec.ID)
		s.hub.PublishTo(sessionKey, agentruntime.Event{
			Type:      agentruntime.EventQuestionResolved,
			SessionID: sessionKey,
			CallID:    rec.ID,
			Message:   "cancelled",
		})
	}
}
```

Add to `ApprovalService` in `internal/server/approvals.go` (mirror `Resolve`'s bookkeeping at lines 141-152):

```go
// settle forgets a pending approval without deciding it. It is the abort path:
// the run is gone, so there is nothing to allow or deny.
func (s *ApprovalService) settle(p pendingApproval) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.bySession[p.SessionKey]; ok && cur == p.ApprovalID {
		delete(s.bySession, p.SessionKey)
	}
	delete(s.byID, p.ApprovalID)
}
```

Match the real mutex and map names in `approvals.go` — read it before writing.

- [ ] **Step 4: Run the test**

Run: `go test ./internal/server/ -run TestSettlePendingForSession -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/server/settle.go internal/server/settle_test.go internal/server/approvals.go
git commit -s -m "feat(server): settle a session's pending HITL records on abort (issue #166)

Assisted-by: Claude Code"
```

---

### Task 5: `POST /api/sessions/{key}/abort`

**Files:**
- Create: `internal/server/abort.go`
- Modify: `internal/server/server.go:193-208` (add the case)
- Test: `internal/server/abort_test.go`

**Interfaces:**
- Consumes: Task 2's `hitlManager.Abort` / `LiveRunID` / `SessionBusy`; Task 3's `Hub.WaitIdle`; Task 4's `settlePendingForSession`.
- Produces: `(*Server).handleAbort(w, r)`.

- [ ] **Step 1: Write the failing test**

Create `internal/server/abort_test.go`:

```go
// Stop must not return until the session is genuinely idle, or the client's
// next POST /api/messages races hub.Open and 409s. It must also settle pending
// HITL records while the stream is still open, and report a timeout as 504
// rather than pretending the session is free.
func TestHandleAbortWaitsForIdle(t *testing.T) {
	h := NewHub()
	w := httptest.NewRecorder()
	stream, err := h.Open("conv-1", w, w)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	gw := &fakeAbortGateway{}
	m := &hitlManager{
		newClient: func(string, *ws.Device) hitlGateway { return gw },
		conns:     map[string]*userHitlConn{"admin": {user: "admin", gw: gw}},
	}
	// A live turn is what supplies the run id; without one the handler falls
	// back to the session-scoped abort.
	live := m.registerLive("admin", "conv-1", func(agentruntime.Event) error { return nil })
	live.setRunID("run-9")
	gw.busy = false

	s := &Server{hub: h, hitl: m}

	done := make(chan int, 1)
	rec := httptest.NewRecorder()
	go func() {
		s.handleAbort(rec, httptest.NewRequest(http.MethodPost, "/api/sessions/conv-1/abort", nil))
		done <- rec.Code
	}()

	select {
	case code := <-done:
		t.Fatalf("handleAbort returned %d while the stream was open", code)
	case <-time.After(50 * time.Millisecond):
	}

	stream.Close()
	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Fatalf("code = %d, want 200", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handleAbort did not return after the stream closed")
	}

	if gw.lastAbortSession != "conv-1" {
		t.Fatalf("abort session = %q", gw.lastAbortSession)
	}
	if gw.lastAbortRunID != "run-9" {
		t.Fatalf("abort runID = %q, want the live turn's run id", gw.lastAbortRunID)
	}
}

// fakeAbortGateway is the hitlGateway slice /abort exercises.
type fakeAbortGateway struct {
	hitlGateway // embed for the methods this test never calls
	busy            bool
	lastAbortSession string
	lastAbortRunID   string
}

func (f *fakeAbortGateway) AbortChat(_ context.Context, sessionKey, runID string) error {
	f.lastAbortSession, f.lastAbortRunID = sessionKey, runID
	return nil
}

func (f *fakeAbortGateway) SessionBusy(context.Context, string) (bool, error) {
	return f.busy, nil
}
```

`hitlManager.registerLive` (`internal/server/hitl.go:547`) and `liveTurn.setRunID` (`:131`) are unexported but the test is in the same package. `hitlManager.conns` is `map[string]*userHitlConn` guarded by `m.mu`; `newClient` is only used by `Connect`, which this test does not call — the embedded `hitlGateway` keeps the fake compiling without stubbing every method.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/server/ -run TestHandleAbort -v`
Expected: FAIL — `s.handleAbort undefined`.

- [ ] **Step 3: Implement**

Create `internal/server/abort.go`:

```go
package server

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// abortSettleTimeout bounds how long /abort waits for the session to go idle.
// Bounded on purpose: a stop that cannot settle must report that, not hang the
// UI forever.
const abortSettleTimeout = 5 * time.Second

// abortRPCDeadline bounds a single chat.abort gateway RPC so a wedged WebSocket
// cannot hold the HTTP request open until the client gives up.
const abortRPCDeadline = 5 * time.Second

// handleAbort serves POST /api/sessions/{key}/abort -- the Portal's Stop.
//
// Order matters and is load-bearing:
//  1. abort the run, scoped to the live turn's run id when there is one;
//  2. settle the session's pending HITL records IMMEDIATELY, while the SSE
//     stream is still open, or the resolved events have nowhere to go and the
//     UI keeps showing a card for a dead run;
//  3. wait for the session to be genuinely idle before returning, so the
//     client's follow-up send cannot race hub.Open into a 409.
func (s *Server) handleAbort(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST required"})
		return
	}
	user := s.userOf(r)
	sessionKey := canonicalSessionKey(subresourceKey(r.URL.Path, "/abort"))
	if sessionKey == "" || sessionKey == "agent:main:" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing session key"})
		return
	}
	if s.hitl == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "abort channel unavailable"})
		return
	}

	runID := ""
	if id, ok := s.hitl.LiveRunID(sessionKey); ok {
		runID = id
	}

	rpcCtx, cancelRPC := context.WithTimeout(r.Context(), abortRPCDeadline)
	defer cancelRPC()
	if err := s.hitl.Abort(rpcCtx, user, sessionKey, runID); err != nil {
		s.logf("abort %s/%s: %v", user, sessionKey, err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}

	// Before the wait: the stream is still attached and can deliver the
	// resolved events that drop the cards.
	s.settlePendingForSession(r.Context(), user, sessionKey)

	ctx, cancel := context.WithTimeout(r.Context(), abortSettleTimeout)
	defer cancel()

	if err := s.waitSessionIdle(ctx, user, sessionKey); err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			writeJSON(w, http.StatusGatewayTimeout, map[string]any{
				"error": "the run did not settle in time; try again",
			})
			return
		}
		s.logf("abort %s/%s: wait idle: %v", user, sessionKey, err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// waitSessionIdle waits for BOTH signals, because they cover different paths:
// the hub check is what stops the follow-up send from racing hub.Open into a
// 409, and the gateway check is the only one that means anything on the
// reload-takeover path, where no stream exists at all and a follow-up send
// could otherwise be steered into the dying run and swallowed.
func (s *Server) waitSessionIdle(ctx context.Context, user, sessionKey string) error {
	hubCtx, cancelHub := context.WithCancel(ctx)
	defer cancelHub()
	hubErr := make(chan error, 1)
	go func() { hubErr <- s.hub.WaitIdle(hubCtx, sessionKey) }()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		busy, err := s.hitl.SessionBusy(ctx, user, sessionKey)
		if err != nil {
			return err
		}
		if !busy {
			return <-hubErr
		}
		select {
		case err := <-hubErr:
			if err != nil {
				return err
			}
		case <-ticker.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
```

Wire the route in `internal/server/server.go` inside `handleSessionSubresource`:

```go
	case strings.HasSuffix(r.URL.Path, "/abort"):
		s.handleAbort(w, r)
```

- [ ] **Step 4: Run the test**

Run: `go test ./internal/server/ -run TestHandleAbort -race -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/server/abort.go internal/server/abort_test.go internal/server/server.go
git commit -s -m "feat(api): POST /api/sessions/{key}/abort (issue #166)

Assisted-by: Claude Code"
```

---

### Task 6: `GET /api/sessions/{key}/turn`

**Files:**
- Modify: `internal/server/abort.go` (add the handler beside `handleAbort`)
- Modify: `internal/server/server.go` (add the route case)
- Test: `internal/server/abort_test.go`

**Interfaces:**
- Consumes: Task 2's `hitlManager.SessionBusy`.
- Produces: `(*Server).handleTurnStatus(w, r)` answering `{"active": bool}`.

- [ ] **Step 1: Write the failing test**

Append to `internal/server/abort_test.go`:

```go
// The reload-takeover path has no stream, so /turn must answer from the gateway
// and must not be cached: it is per-session liveness, not a shareable resource.
func TestHandleTurnStatus(t *testing.T) {
	fake := newFakeHitl(t)
	fake.busy = true
	s := &Server{hitl: fake}

	rec := httptest.NewRecorder()
	s.handleTurnStatus(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/conv-1/turn", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	var body struct {
		Active bool `json:"active"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Active {
		t.Fatal("active = false, want true")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/server/ -run TestHandleTurnStatus -v`
Expected: FAIL — `s.handleTurnStatus undefined`.

- [ ] **Step 3: Implement**

Add to `internal/server/abort.go`:

```go
// handleTurnStatus serves GET /api/sessions/{key}/turn -- whether the session
// still has a run in flight, so a Portal that reloaded (and therefore has no
// stream) can say so and offer Stop.
//
// The answer comes from the gateway, not the SSE hub: the hub only knows
// whether a browser is attached, which is false after a reload while the run is
// still going.
func (s *Server) handleTurnStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET required"})
		return
	}
	// Per-session liveness: never cache.
	w.Header().Set("Cache-Control", "no-store")

	user := s.userOf(r)
	sessionKey := canonicalSessionKey(subresourceKey(r.URL.Path, "/turn"))
	if sessionKey == "" || sessionKey == "agent:main:" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing session key"})
		return
	}
	if s.hitl == nil {
		writeJSON(w, http.StatusOK, map[string]any{"active": false})
		return
	}
	busy, err := s.hitl.SessionBusy(r.Context(), user, sessionKey)
	if err != nil {
		s.logf("turn status %s/%s: %v", user, sessionKey, err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"active": busy})
}
```

Route:

```go
	case strings.HasSuffix(r.URL.Path, "/turn"):
		s.handleTurnStatus(w, r)
```

- [ ] **Step 4: Run the test**

Run: `go test ./internal/server/ -run TestHandleTurnStatus -v`
Expected: PASS.

- [ ] **Step 5: Full check and commit**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS.

```bash
git add -A
git commit -s -m "feat(api): GET /api/sessions/{key}/turn (issue #166)

Assisted-by: Claude Code"
```

---

### Task 7: Web — the stopped/message_done contract and abort-safe streaming

**Files:**
- Modify: `web/src/api/types.ts:163-167`
- Modify: `web/src/api/sse.ts`
- Modify: `web/src/api/index.ts`

**Interfaces:**
- Produces: `SSEMessageDone.stopped?: boolean`; `streamSSE(url, opts, onEvent, signal?)`; `api.abortSession(sessionKey, user)`; `api.sessionTurn(sessionKey, user)`.

- [ ] **Step 1: Extend the event type**

In `web/src/api/types.ts`:

```ts
export interface SSEMessageDone {
  type: 'message_done'
  session_id: string
  error?: string
  // The user stopped this turn (chat.abort or /stop). Mutually exclusive with
  // error: a stopped turn is neither a failure nor a normal completion, and its
  // partial text must not be presented as a finished answer.
  stopped?: boolean
}
```

- [ ] **Step 2: Thread an abort signal without inventing an error**

In `web/src/api/sse.ts`, change the signature to accept an optional signal and make an intentional cancel silent:

```ts
export async function streamSSE(
  url: string,
  opts: RequestInit,
  onEvent: (name: string, ev: SSEEvent) => void,
  signal?: AbortSignal,
): Promise<void> {
  const resp = await fetch(url, { ...opts, signal })
  // ...
```

In the read-failure branch, distinguish an intentional abort from a real one:

```ts
    try {
      r = await reader.read()
    } catch (e) {
      // An intentional cancel (unmount, session switch) is a clean stop, not a
      // transport failure: do not synthesize an error the user would see.
      if (signal?.aborted) return
      streamError = String(e)
      break
    }
```

Also guard the missing-terminal fallback at the end of the function:

```ts
  if (!sawDone && !signal?.aborted) {
    emitDone(onEvent, streamError)
  }
```

- [ ] **Step 3: Add the API methods**

In `web/src/api/index.ts`, beside the existing session calls. `apiFetch` already
attaches `X-CubePilot-User` from `getCurrentUser()`, so neither call takes a user
argument:

```ts
  // Stops the session's running turn. The server does not answer until the turn
  // has settled, so a send issued after this resolves cannot be rejected as a
  // concurrent turn.
  abortSession: (sessionKey: string) =>
    apiFetch<{ ok: boolean }>(
      `/api/sessions/${encodeURIComponent(sessionKey)}/abort`,
      { method: 'POST' },
    ),

  // Whether the session still has a turn in flight -- used after a reload, when
  // this tab has no stream to tell it.
  sessionTurn: (sessionKey: string) =>
    apiFetch<{ active: boolean }>(
      `/api/sessions/${encodeURIComponent(sessionKey)}/turn`,
    ),
```

- [ ] **Step 4: Build**

Run: `cd web && npm run build`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/src/api/types.ts web/src/api/sse.ts web/src/api/index.ts
git commit -s -m "feat(web): message_done.stopped, abort-safe SSE, abort/turn API (issue #166)

Assisted-by: Claude Code"
```

---

### Task 8: Web — streaming state, Stop button, send-while-streaming

**Files:**
- Modify: `web/src/views/ChatView.tsx` (state near line 393; `sendMessage` at 608-740; composer at 1092-1110; status line at 935-946)

Add one import — `import { showToast } from '@/stores/toast'` — and note that
`user` is already a module-level const (`ChatView.tsx:12`), so nothing here takes
a user argument.

**Interfaces:**
- Consumes: Task 7's `api.abortSession`, `streamSSE(..., signal)`, `SSEMessageDone.stopped`.

- [ ] **Step 1: Add the streaming state and a stale-session guard**

Near the other state declarations in `ChatView`:

```tsx
  const [streaming, setStreaming] = useState(false)
  // The session this component is currently showing. A stream started for an
  // earlier session must not write into the new one's bubbles.
  const activeSessionRef = useRef<string>('')
  const abortRef = useRef<AbortController | null>(null)
```

Set `activeSessionRef.current = id` wherever `currentSessionId` changes (same places `loadHistory` is called). In `sendMessage`, capture `const sentFor = currentSessionId` before the await and drop events when it no longer matches:

```tsx
  const stale = () => activeSessionRef.current !== sentFor
```

Wrap the `onEvent` callback body: at the top, `if (stale()) return`. In the `catch`, only set the error when the abort was not intentional:

```tsx
    } catch (e) {
      if (!aborted) {
        setPhase(bubble, 'done')
        bubble.error = String(e)
      }
    }
```

- [ ] **Step 2: Render the stopped state**

In the `message_done` branch of the event callback:

```tsx
          if (ev.type === 'message_done') {
            setPhase(bubble, 'done')
            if (ev.stopped) bubble.stopped = true
            else if (ev.error) bubble.error = ev.error
            return
          }
```

Add `stopped?: boolean` to the `BubbleMsg` interface. In `statusLine()` (around line 868), return `'Stopped'` when the newest bubble is stopped, and render a distinct muted marker on the bubble (reuse the existing `.tool-status` class).

- [ ] **Step 3: Swap the send button for Stop while streaming**

In the composer:

```tsx
        {streaming ? (
          <button className="send-btn" aria-label="Stop" onClick={stopTurn}>
            <StopIcon />
          </button>
        ) : (
          <button className="send-btn" aria-label="Send" onClick={sendMessage}>
            <SendIcon />
          </button>
        )}
```

Add a `StopIcon` beside `SendIcon` (a filled square, `width`/`height` 12-14, `currentColor`). The textarea stays enabled — the user must be able to type the redirect.

- [ ] **Step 4: Implement `stopTurn` and the send-while-streaming path**

```tsx
  async function stopTurn() {
    if (!currentSessionId) return
    try {
      await api.abortSession(currentSessionId)
    } catch (e) {
      // The turn is still running: keep the Stop button and say so.
      showToast(String(e))
      return
    }
    // The stream closes with message_done{stopped:true}; nothing more to do
    // here -- do NOT abort the local fetch, the server's terminal is cleaner.
  }
```

In `sendMessage`, before opening a new stream:

```tsx
    if (streaming) {
      // Redirect: stop the running turn first. The server only answers once the
      // turn has settled, so the send below cannot hit the 409 guard.
      await stopTurn()
    }
```

Set `setStreaming(true)` before `streamSSE` and `setStreaming(false)` in the `finally`.

- [ ] **Step 5: Abort the stream on unmount only**

```tsx
  useEffect(() => {
    return () => {
      abortRef.current?.abort()
    }
  }, [])
```

Set `abortRef.current = new AbortController()` before each `streamSSE` call and pass `abortRef.current.signal` as the fourth argument. Abort it when switching sessions (in the same place `loadHistory` is triggered).

- [ ] **Step 6: Build and check**

Run: `cd web && npm run build`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add web/src/views/ChatView.tsx
git commit -s -m "feat(web): stop button and mid-turn redirect in Portal chat (issue #166)

Assisted-by: Claude Code"
```

---

### Task 9: Web — reload takeover

**Files:**
- Modify: `web/src/views/ChatView.tsx` (the mount effect around line 411)

**Interfaces:**
- Consumes: Task 7's `api.sessionTurn`; Task 8's `streaming` / `stopTurn`.

- [ ] **Step 1: Query the turn status after history loads**

In the mount effect, after `loadHistory(id)` resolves and `activeSessionRef.current = id` is set:

```tsx
      try {
        const { active } = await api.sessionTurn(id)
        if (active && activeSessionRef.current === id) setRunningElsewhere(true)
      } catch {
        // Best-effort: a failure here must not block the chat from loading.
      }
```

Add the state:

```tsx
  // A turn started in another tab, or before a reload, that this tab has no
  // stream for. It can still be stopped; its output arrives on the next history
  // refresh, because stream re-attach is out of scope.
  const [runningElsewhere, setRunningElsewhere] = useState(false)
```

- [ ] **Step 2: Render the banner and Stop**

Above the composer, when `runningElsewhere && !streaming`:

```tsx
        {runningElsewhere && !streaming && (
          <div className="turn-banner">
            <span className="spin" />
            <span>Still running…</span>
            <button className="btn sm" onClick={stopElsewhere}>
              Stop
            </button>
          </div>
        )}
```

```tsx
  async function stopElsewhere() {
    if (!currentSessionId) return
    try {
      await api.abortSession(currentSessionId)
      setRunningElsewhere(false)
      await loadHistory(currentSessionId)
    } catch (e) {
      showToast(String(e))
    }
  }
```

- [ ] **Step 3: Build**

Run: `cd web && npm run build`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add web/src/views/ChatView.tsx
git commit -s -m "feat(web): offer Stop for a turn running without a local stream (issue #166)

Assisted-by: Claude Code"
```

---

### Task 10: Verification spike and e2e

**Files:**
- Modify: `test/e2e/` (follow the existing chat e2e from the ask_user work)

**Interfaces:** none.

- [ ] **Step 1: Run the reload-marker spike**

Against a live gateway (kind cluster, real apiKey), abort a turn while it is still streaming and dump the transcript:

```bash
kubectl -n cubepilot exec deploy/cubepilot -- \
  sh -c 'cat /data/agents/*/sessions/*.jsonl' | tail -5 | jq -c 'select(.role=="assistant") | {text: .content, openclawAbort}'
```

Then fetch what the Portal reads:

```bash
curl -s -H "X-CubePilot-User: admin" localhost:8080/api/sessions/<key>/messages | jq .
```

Record the answers to both open questions from the spec (does `openclawAbort` appear in the history payload; can a mid-turn committed row be truncated). **If either comes back badly, stop and report — the reload marker is dropped, not worked around with a new CubePilot-owned store.**

- [ ] **Step 2: e2e — Stop ends the turn**

Add to the chat e2e: start a turn, POST `/api/sessions/{key}/abort`, then assert a follow-up `POST /api/messages` does **not** return 409 and the session reports `active: false`.

- [ ] **Step 3: e2e — redirect mid-turn**

Start a turn, then POST a second message while `streaming`; assert the first turn's stream ends with `message_done{stopped}` and the second turn runs.

- [ ] **Step 4: Run the full stack**

Run: `go test ./... && (cd web && npm run build)`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add -A
git commit -s -m "test(e2e): stop and mid-turn redirect (issue #166)

Assisted-by: Claude Code"
```

---

## Self-Review Notes

- **Spec coverage:** Stop (Tasks 2, 5, 8), redirect (Tasks 5, 8), reload takeover (Tasks 2, 6, 9), terminal contract + propagation (Tasks 1, 8), `WaitIdle` (Task 3), HITL settle (Tasks 4, 5), `/turn` caching (Task 6), abort-safe signal (Tasks 7, 8), rollout note (implemented by the single-chart deploy; no task needed), verification spike (Task 10).
- **Deliberately absent:** stream re-attach, steer, scheduled-task cancel, and any CubePilot-owned durable stop marker (declined).
- **Known-uncertain points to read before coding:** the exact field names in `approvals.go`, the existing `ws` test fake, and the real `apiFetch` signature. Each is called out in its task; do not invent them.
