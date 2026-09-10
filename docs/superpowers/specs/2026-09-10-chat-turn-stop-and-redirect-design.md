# Stopping and redirecting an in-flight chat turn

Issue: #166. Status: design.

## Problem

Once a chat turn starts in the Portal there is no way to stop it or redirect it.

- The web chat has no streaming/busy state and no stop control: the send button
  (`web/src/views/ChatView.tsx:1107`) never disables and `streamSSE`'s `fetch`
  (`web/src/api/sse.ts:18`) carries no signal — there is no `AbortController`
  anywhere in `web/src`.
- One SSE stream per session. A second `POST /api/messages` for a session that is
  already streaming returns **409** `another turn is already streaming for this
  session` (`internal/server/handlers.go:150-153`, `internal/server/ssehub.go:40-45`).
  A mid-turn instruction is an error, not a queue and not a redirect.
- The run keeps going regardless. The turn executes gateway-side:
  `handleMessages` drives `RunLiveTurn` with `r.Context()`
  (`handlers.go:207`) and `hitl.go`'s deferred `releaseLive` unsubscribes when
  the request ends. A browser disconnect stops *observation* only — the agent
  keeps calling `kubectl` against the cluster.
- After a reload the user gets a frozen history snapshot (`loadHistory`,
  `ChatView.tsx:448`) with no indication that the agent is still running, and no
  way to stop it.

## Goals

1. A **Stop** control while a turn streams that actually cancels the gateway run.
2. Sending a new message mid-turn **stops the current turn and starts a new one**
   with the new instruction.
3. After a reload or navigation, the UI can tell that a turn is still running and
   offers Stop.

## Non-goals

- **Stream re-attach after a reload.** Takeover here means "know it is running +
  can stop", not a resumed stream. Resuming live deltas requires the turn to
  outlive the request that drives it (a per-session turn owner). The gateway-side
  catch-up primitive already exists: `chat.history` returns
  `{kind:"delta", messages, deltaCursor, inFlightRun}`.
- **Steering** (`queueMode: "steer"`) — injecting a correction into the running
  turn without restarting it.
- **Cancelling scheduled task runs.** `internal/runner` drives the gateway's
  HTTP compatibility endpoint and does not occupy the WS chat path.

## Gateway primitives

All three already exist upstream; cubepilot has not wired them up.

| User action | Gateway call | Semantics |
|---|---|---|
| Press Stop | `chat.abort {sessionKey}` | Terminates the active run. No new instruction. |
| Send while streaming | aborted, then a normal `chat.send` | Equivalent to `interrupt`: old run dies, new run starts with the new message. |
| Reload takeover | `chat.history {sessionKey}` → `inFlightRun` | Authoritative "is this session busy". |

`chat.abort` is defined at
`packages/gateway-protocol/src/schema/logs-chat.ts:255-260` as
`{ sessionKey, agentId?, runId?, preserveSideRuns? }`. Omitting `runId` is
session-scoped and does **not** cascade to child agents — which is exactly the
stop semantics wanted, and necessary because the browser never learns the run id.

`chat.history`'s delta result carries `inFlightRun`
(`logs-chat.ts:106`). `Hub.Active()` is **not** a usable substitute: it tracks
browser streams, not gateway runs, so it reports "idle" after a reload while the
run is still going.

### Why abort-then-send rather than `queueMode: "interrupt"`

OpenClaw's `chat.send` accepts `queueMode: "interrupt"`, which atomically
aborts the session's current admitted turn and starts a new one. Using it
directly would require `RunLiveTurn` to re-target to a run id it did not
generate when the aborted terminal arrives — precisely the correlation guard
that #130 added ("unrelated or pre-ACK runs rejected ... before any run of the
session can leak in", `internal/server/hitl.go:626-629`).

The two-step path reaches the same transcript (the gateway's own `interrupt`
also aborts and then starts a fresh turn) with `RunLiveTurn` untouched. The cost
is a window between the abort landing and the new send: while the session is
still busy the default disposition of an inbound prompt is to **steer** it into
the active run, and a steer onto a run that is being aborted can be silently
swallowed. The design closes that window by having `/abort` not return until the
session's stream has been unregistered (see below), so the follow-up send always
lands on a settled session.

Revisit this once turn ownership is decoupled from the originating HTTP request:
at that point `queueMode: "interrupt"` becomes a natural fit and only the
`/abort` implementation changes.

## Design

### Layer 1 — ws client

`internal/openclaw/ws/methods.go`, next to `CancelQuestion`:

```go
// AbortChat cancels the session's active run. No run id is passed: the Portal
// never learns it, and the session-scoped form does not cascade to children.
func (c *Client) AbortChat(ctx context.Context, sessionKey string) error
// → Call(ctx, "chat.abort", map[string]any{"sessionKey": sessionKey})
```

Plus a reader for the busy signal, also in `internal/openclaw/ws/methods.go`:

```go
// SessionBusy reports whether the gateway has an in-flight run for the session.
func (c *Client) SessionBusy(ctx context.Context, sessionKey string) (bool, error)
// → Call(ctx, "chat.history", {sessionKey}); report result.inFlightRun != nil
```

`chat.history`'s delta has a 200-entry / byte budget and can return
`{kind:"reset"}`. This change reads only `inFlightRun`, so neither branch
matters today; a future stream re-attach will have to handle `reset`.

`hitlGateway` (`internal/server/hitl.go:31-52`) gains both methods, and the test
fake gains them too.

### Layer 2 — server

Two routes under `handleSessionSubresource`
(`internal/server/server.go:193-208`), modelled on `handleQuestion`
(`internal/server/questions.go:191`): bind the canonical session key, validate,
map gateway errors to statuses.

```
POST /api/sessions/{key}/abort   → {ok: true}
GET  /api/sessions/{key}/turn    → {active: bool}
```

**`/abort` must wait for the stream to be gone before returning.** Otherwise the
client's follow-up `POST /api/messages` races `hub.Open` and still gets a 409,
which is the whole failure being fixed. Add to `Hub`:

```go
// WaitIdle blocks until the session has no active stream, or ctx expires.
func (h *Hub) WaitIdle(ctx context.Context, sessionKey string) error
```

backed by the active `Stream`'s existing `closedCh`. `/abort` then:

1. `AbortChat(ctx, sessionKey)` — idempotent; "nothing was running" is success,
   not an error.
2. `hub.WaitIdle(ctx, sessionKey)` with a 5s deadline. On timeout return **504**
   rather than a success: the follow-up send would otherwise land on a still-busy
   session and could be steered into the dying run and swallowed. The UI keeps the
   turn and the Stop button so the user can retry, instead of failing silently.
3. Settle the session's local pending HITL records (below).

If no stream is active (the reload-takeover path), step 2 returns immediately.

**Terminal state.** On abort the gateway emits chat `state:"aborted"`
(`internal/server/livetools.go:245-248` already treats it as terminal), but
`chatTerminalErr` (`hitl.go:719-736`) maps it to a non-nil error, so a
user-initiated stop currently renders as a *failed* turn. Change it to
distinguish the gateway's `stopReason`:

- `stopReason` is `"rpc"` (the `chat.abort` RPC) or `"stop"` (the `/stop`
  command path) → the turn was stopped **by request**: not an error.
- anything else (`"timeout"`, `"restart"`, `"auth-revoked"`, …) → still an
  error, as today.

Carry that on the existing terminal event rather than adding a new one.

**Terminal contract.** `message_done` is the *only* terminal event and carries
the outcome:

```
message_done { }                 ← completed
message_done { stopped: true }   ← stopped by request
message_done { error: "..." }    ← failed
```

Invariant: `stopped` and `error` are mutually exclusive, and `stopped: true`
always means an empty `error`.

Why not a separate `message_aborted` event type:

- `message_done` is already an outcome-carrying terminal — it has carried `error`
  since the beginning, so `stopped` is a third outcome of an existing concept,
  not a new concept. Splitting one discriminated event into two types would be
  less consistent with what is already there.
- A single terminal is a safety property, not just tidiness. `sse.ts`'s `sawDone`
  keys on `ev.type === 'message_done'` and synthesizes an error terminal when it
  is missing; with two terminal types, "did I miss the terminal?" becomes a
  two-way question and any consumer that handles only one of them leaves the UI
  spinning forever.
- The gateway models the same fact as one chat event discriminated by `state`
  (`status` / `delta` / `final` / `aborted` / `error`), so the projection layer
  stays closest to its source by discriminating rather than multiplying types.
- It extends cleanly if more outcomes appear (a timeout, a budget stop).

`agentruntime.Event` (`internal/runtime/contracts.go:54-70`) gains a `stopped`
field (omitempty).

Text already streamed is kept: the bubble keeps its partial content and is
marked stopped. Stopping is not a rollback.

**Stopped turns must survive a reload.** The `stopped` flag alone only reaches a
client that was attached to the stream. A user who reloads has no stream — they
see history (and `/turn`), so a truncated answer would render as if it were a
complete one. The gateway persists an aborted partial's assistant message into
the transcript with `openclawAbort: { aborted: true, origin, runId }`
(`src/gateway/server-methods/chat-transcript-inject.ts:137-143`), which is the
signal history can render from. **Verify during implementation** that this field
is present in the `/sessions/{key}/history` payload cubepilot reads — it is
written into the transcript but the gateway itself never reads it back, so
whether the history endpoint surfaces raw message bodies is unconfirmed. If it
does not surface, the fallback is to mark the turn stopped only for a live
stream and treat the reload marker as a follow-up; do not render a partial as a
completed answer in the meantime.

**HITL cleanup.** If the agent is parked on a `confirm_pending` or
`question_pending`, the abort settles it gateway-side, but cubepilot's
`ApprovalService.bySession` (`internal/server/approvals.go`) and `questionRoutes`
(`internal/server/questions.go`) keep the record. Without cleanup, a reload
resurfaces a card that errors when acted on. On a successful abort, mark the
session's unresolved records settled/expired and publish the corresponding
`confirm_resolved` / `question_resolved` event so any attached stream agrees.

### Layer 3 — web

`web/src/views/ChatView.tsx`:

- Add a **`streaming`** state — it does not exist today; the only signal is the
  non-interactive status line (`ChatView.tsx:935-946`).
- While streaming, the send button swaps to a **Stop** button (square icon,
  reusing `.send-btn` styling and the `aria-label` convention). The composer
  stays **usable** so the user can type the redirect.
- **Stop**: `POST /api/sessions/{key}/abort`. Do **not** abort the local fetch —
  let the server close the stream cleanly and deliver
  `message_done{stopped:true}`, so the bubble stops spinning, keeps its partial
  text, and is marked stopped rather than failed.
- **Send while streaming**: `await abort()` then the normal `POST /api/messages`.
  The second send opens a fresh stream, which `/abort` has already made safe.
- Adding a draft after Stop is not auto-sent; the session is idle and the next
  send is an ordinary turn.

`web/src/api/sse.ts`: thread an optional `AbortSignal` into `RequestInit`, used
**only** for unmount / session-switch cleanup — never for Stop.

`web/src/api/types.ts`: `MessageDone` gains `stopped?: boolean`.

**Reload takeover**: after `loadHistory` on mount, query
`GET /api/sessions/{key}/turn`; when `active`, show "agent is still running" plus
a Stop button. There is no live stream in this state, so after Stop call
`loadHistory()` to pick up the persisted partial and its stopped marker.

**Rendering a stopped turn from history**: a persisted aborted partial must be
marked as stopped rather than shown as a finished answer (see "Stopped turns must
survive a reload" above for the source of that signal and its open verification
question). This applies both to the post-Stop `loadHistory()` refresh and to a
plain reload.

## Edge cases

- **Abort races natural completion.** `chat.abort` on a settled session is a
  no-op; `/abort` still returns success and the follow-up send starts normally.
- **Stop pressed with nothing running.** Same: success, no-op.
- **Agent parked on confirm/question.** Abort cancels the run; the local records
  are settled so no dead card survives a reload.
- **Second tab.** A session still allows only one SSE stream, so a second tab's
  `POST /api/messages` still 409s. With this change that tab can at least see
  "still running" via `/turn` and stop the run.
- **Client disappears mid-abort.** `/abort`'s wait is bounded and the hub
  cleanup is driven by the handler's `defer`, so a vanished client cannot wedge
  the session.
- **Gateway abort fails.** Surface a 502 with the gateway error; the UI keeps the
  turn streaming and re-enables the Stop button.
- **Abort succeeds but the stream does not close in time.** Surface a 504 (see
  above); the UI keeps the turn and the Stop button so the user can retry rather
  than have the follow-up send silently swallowed.

## Testing

- **ws**: `AbortChat` sends `chat.abort` with the session key and no run id;
  `SessionBusy` decodes `inFlightRun` present/absent and the `reset` branch.
- **server**: `/abort` calls `AbortChat` and then `WaitIdle`; returns success
  when idle; settles pending confirm/question records for the session; a
  `state:"aborted"` frame with `stopReason:"rpc"` produces
  `message_done{stopped:true}` with an empty error, while `"timeout"` still
  produces an error; `/turn` reflects `inFlightRun`.
- **web**: switching the composer to Stop while streaming; Stop renders a
  stopped (not failed) bubble and preserves partial text; send-while-streaming
  awaits the abort before posting; mount-time `/turn` shows the running banner
  and Stop; a stopped turn loaded from history renders as stopped, not as a
  completed answer.
- **e2e**: with a live agent, start a turn, press Stop, assert the turn ends
  promptly and the session is idle; then send a second message mid-turn and
  assert the first turn is stopped and the second runs. Also reload after a Stop
  and assert the partial is marked stopped.
- **verification spike (before implementation)**: confirm against a live gateway
  whether `openclawAbort` appears in the `/sessions/{key}/history` payload. This
  decides whether the reload marker is in scope or a follow-up.

## Rollout

No migration. Both new routes are additive; `message_done{stopped}` is an
optional field, so an older client ignores it and simply sees a completed turn.
