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
`{ sessionKey, agentId?, runId?, preserveSideRuns? }`. Passing `runId` scopes the
abort to that run; omitting it is session-scoped and does **not** cascade to
child agents.

**Pass `runId` whenever one is known.** The browser never learns the run id, but
the server does: `hitlManager` holds it on the session's live turn
(`liveTurn.runID`, `hitl.go:123-135`). A session-scoped abort is not race-free —
if the current run settles and another is promoted before the RPC is processed
(a queued follow-up draining, or a message from a second tab), the session-scoped
abort terminates the **newer** run. Session-scoped is the fallback only for the
reload-takeover path, where no live turn exists to read a run id from; there,
serialize against send or accept the narrow race knowingly, but do not describe
it as settled.

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
// AbortChat cancels a run. runID scopes the abort to that run and is what the
// live-turn path passes; the empty string aborts the session's active run and
// is the fallback only when no run id is known (reload takeover).
func (c *Client) AbortChat(ctx context.Context, sessionKey, runID string) error
// → Call(ctx, "chat.abort", {sessionKey, ...(runID ? {runId: runID} : {})})
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

`/turn` reports per-session liveness, so it must not be cached: send
`Cache-Control: no-store` on the response. The existing JSON routes do not set
cache headers, so this is not a new class of problem — but the right header on
the new route costs nothing.

**Authorization.** Both routes inherit the phase-one trust model: the caller is
identified by the `X-CubePilot-User` header with no authentication, exactly like
`/api/messages` and `/confirm` today, so they add no new trust boundary — a
caller who can already send a message as any user does not gain anything by also
being able to stop one. Binding streams and routes to a real identity is tracked
separately and is out of scope here.

**`/abort` must wait for the stream to be gone before returning.** Otherwise the
client's follow-up `POST /api/messages` races `hub.Open` and still gets a 409,
which is the whole failure being fixed. Add to `Hub`:

```go
// WaitIdle blocks until the session has no active stream, or ctx expires.
func (h *Hub) WaitIdle(ctx context.Context, sessionKey string) error
```

It must **not** simply wait on the active stream's `closedCh`. `Stream.Close`
(`ssehub.go:146-158`) closes that channel *before* unregistering:

```go
s.closed = true
close(s.closedCh)      // waiter wakes here
s.mu.Unlock()
s.hub.remove(s.key, s) // ...but the hub still lists it until here
```

so a waiter can wake, return "idle", and the client's very next `POST
/api/messages` still sees the old stream and gets a 409 — the exact failure
`WaitIdle` exists to prevent. `WaitIdle` must re-check membership under the hub
mutex after waking, looping until the session is genuinely absent (or ctx
expires). Reordering `Close` so the unregistration precedes the close would also
work, but the recheck is robust either way and does not perturb existing
teardown.

`/abort` then, in this order:

1. Resolve the session's live turn and take its `runID` if there is one, then
   `AbortChat(ctx, sessionKey, runID)` under a **bounded context** (the same
   per-RPC deadline style the manager already uses, e.g. 5s). Without a bound a
   wedged WS `Call` holds the HTTP request open until the client gives up.
   Idempotent: "nothing was running" is success, not an error.
2. **Settle the session's local pending HITL records immediately** (below) — not
   after the wait. They must be resolved while the stream is still open, or the
   `confirm_resolved` / `question_resolved` events have nowhere to go and the
   attached UI keeps showing a card for a run that is already dead.
3. Wait for the session to actually be idle before returning, bounded at 5s,
   satisfied by **both** of:
   - the gateway reporting not-busy (`SessionBusy` false), and
   - `hub.WaitIdle` for the session's SSE stream, when one exists.

   Both are needed. The hub check is what stops the follow-up `POST /api/messages`
   from racing `hub.Open` into a 409; the gateway check is the only one that means
   anything on the reload-takeover path, where there is no stream at all and a
   follow-up send could otherwise be steered into the dying run and swallowed.
   On timeout return **504** rather than success, so the UI keeps the turn and the
   Stop button and the user can retry instead of failing silently. Step 2 has
   already run on this path, so a *wait* timeout leaves no dead cards behind.

   That scoping matters, because the other failure path is deliberately the
   opposite. If the abort RPC itself fails, the handler answers 502 **before**
   step 2 and leaves the session's records pending. That is not an oversight:
   the RPC failing means the run's fate is unknown, and a run that is still
   alive still has a live, answerable card. Settling on an unknown result would
   delete a card for a run that never stopped — a worse failure than a stale one,
   and the reverse of what step 2 is for. The records are cleared on the next
   successful abort, and a card whose record is already gone answers 404, which
   the client renders as gone rather than as an error.

**Terminal state.** On abort the gateway emits chat `state:"aborted"`
(`internal/server/livetools.go:245-248` already treats it as terminal), but
`chatTerminalErr` (`hitl.go:719-736`) maps it to a non-nil error, so a
user-initiated stop currently renders as a *failed* turn. Change it to
distinguish the gateway's `stopReason`:

- `stopReason` is `"rpc"` (the `chat.abort` RPC) or `"stop"` (the `/stop`
  command path) → the turn was stopped **by request**: not an error.
- anything else (`"timeout"`, `"restart"`, `"auth-revoked"`, …) → still an
  error, as today.

**Propagation: the outcome needs its own channel.** The terminal event is emitted
by the *handler*, not by the runtime adapter, and today the only thing crossing
that boundary is an `error` (`handlers.go:207-215`):

```go
} else if err := runtimeAdapter.RunLiveTurn(...); err != nil {
    _ = stream.Send(Event{Type: EventMessageDone, Error: err.Error()})
} else {
    _ = stream.Send(Event{Type: EventMessageDone})
}
```

So teaching `chatTerminalErr` to return `nil` for a stop would produce
`message_done{}` — indistinguishable from normal completion. A `stopped` field on
`agentruntime.Event` is necessary but not sufficient; it also has to be emitted,
and the manager does not emit the terminal.

Change `LiveTurnRunner.RunLiveTurn` to return a typed outcome alongside the
error:

```go
type TurnOutcome struct {
    Stopped bool
}
type LiveTurnRunner interface {
    RunLiveTurn(ctx context.Context, sessionKey string, params LiveTurnParams,
        emit func(Event) error) (TurnOutcome, error)
}
```

and let the handler discriminate:

```go
outcome, err := runtimeAdapter.RunLiveTurn(...)
switch {
case err != nil:   // message_done{error}
case outcome.Stopped: // message_done{stopped:true}
default:           // message_done{}
}
```

The ripple is contained: one interface, one implementer
(`openClawLiveRunner`, `hitl.go:65`) — `Compose` passes a single `LiveTurnRunner`
— one call site, and test fakes. Do **not** have the manager emit the terminal
through the sink instead: the handler owns the terminal, and letting both write
it is how the single-terminal invariant gets broken.

Carry the outcome on the existing terminal event rather than adding a new one.

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
client attached to the stream. A user who reloads has no stream — they see
history (and `/turn`) — so the stopped state has to be recoverable from history
or it is simply lost.

The gateway does persist an aborted partial into the transcript with
`openclawAbort: { aborted: true, origin, runId }`
(`src/gateway/server-methods/chat-transcript-inject.ts:137-143`). Its semantics
are narrower than "every stopped turn is marked", and the design must state them
rather than assume them:

- A partial that is still buffered and not yet committed **is** persisted and
  marked.
- A reply the run **already committed** is deliberately left **unmarked** — the
  gateway suppresses the late re-persist as a duplicate
  (`chat.abort-persistence.test.ts:488`, *"does not duplicate a committed reply
  when a late abort re-persists the buffered text"*, asserting `openclawAbort`
  is undefined; `chat.abort-live-proof.test.ts:272` asserts the same against a
  live gateway). That row is the run's complete reply, so leaving it unmarked is
  correct — it is not a defect to work around.

Two questions remain open and belong to the implementation-time spike:

1. Does `/sessions/{key}/history`, which cubepilot reads, surface
   `openclawAbort` at all? It is written into the transcript but nothing in the
   gateway reads it back, so the history endpoint's projection of raw message
   bodies is unconfirmed.
2. Can a **mid-turn** committed row be truncated — commentary committed when a
   tool ran, then superseded (cubepilot's `text_replace` exists for exactly that
   rewrite)? If so, that row would be both committed and truncated, and would
   fall in the unmarked case. That is the only scenario where a marked partial
   is insufficient.

A **CubePilot-owned durable marker** (a per-session record written when `/abort`
succeeds and consulted when rendering history) is **explicitly out of scope** —
it was considered and declined. If either open question comes back badly, the
correct response is to drop the reload marker, not to grow this feature.

**Decision (v1): the reload marker is dropped.** The spike above was not run
before v1, so this is recorded as an accepted limitation rather than left to
look like an oversight:

- A turn stopped while the tab is attached is marked correctly, because the
  `stopped` flag arrives on the stream.
- A **plain reload** afterwards cannot recover it. The persisted partial comes
  back as an ordinary assistant message, so a stopped turn reads as a completed
  one. This is the accepted gap, and closing it needs either the spike plus
  `openclawAbort` being surfaced, or the declined CubePilot-owned marker.
- The one case that is both reachable and worth fixing without either is
  **"the user just stopped it themselves"**: the client already knows it issued
  the stop, so it marks that turn locally on the refresh it triggers, rather than
  asking the server to tell it something the server cannot.

The spike remains the gate for revisiting this. What the design still forbids,
and the local marker upholds, is presenting a *known*-stopped turn as finished.

**HITL cleanup.** If the agent is parked on a `confirm_pending` or
`question_pending`, the abort settles it gateway-side, but cubepilot's
`ApprovalService.bySession` (`internal/server/approvals.go`) and `questionRoutes`
(`internal/server/questions.go`) keep the record. Without cleanup, a reload
resurfaces a card that errors when acted on. On a successful abort — as step 2,
while the stream is still open — mark the session's unresolved records
settled/expired and publish the corresponding `confirm_resolved` /
`question_resolved` event so any attached stream agrees. Doing this after the
idle wait is too late: the stream is gone by then.

Settling *and publishing* is deliberate, against the two cheaper alternatives:
leaving the records to expire keeps a dead card on screen for the question
timeout (~900s), and deleting them silently leaves an attached view showing a
live-looking card until it happens to refetch. This is not new machinery — it is
the same operation the `ask_user` card's own Dismiss already performs, applied to
every pending record for the session at once.

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

Adding a signal is not enough on its own, because today an abort is
indistinguishable from a transport failure:

- `reader.read()` throwing lands in the same catch that sets `streamError`
  (`sse.ts:33-39`), and the missing-terminal fallback then synthesizes
  `message_done{error}` (`sse.ts:60-62`). An *intentional* cancel would surface
  as a user-visible failure. The signal must be checked on that path and the
  synthetic error suppressed — an aborted stream is a clean, silent stop, not
  an error.
- `ChatView`'s event callback and its `catch` both mutate the captured bubble
  and call `setBubbles([...bubblesRef.current])` (`ChatView.tsx:722-737`) with no
  check that the bubble still belongs to the active session. In a session switch
  the old stream's late events — or its abort — would mutate the newly selected
  session's view. The callback must drop events for a session that is no longer
  current (capture the session at send time and compare), and the `catch` must
  not set `bubble.error` when the stream was intentionally cancelled.

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
- **server**: `/abort` calls `AbortChat` with the live turn's `runID`, and with
  no run id when there is no live turn; the abort RPC carries a deadline; it
  waits for gateway-idle as well as `WaitIdle`, including on the takeover path
  where no stream exists; it returns 504 on timeout; it settles pending
  confirm/question records *before* the idle wait, so the resolution events are
  emitted while the stream is still open; a `state:"aborted"` frame with
  `stopReason:"rpc"` produces `message_done{stopped:true}` with an empty error,
  while `"timeout"` still produces an error; `/turn` reflects `inFlightRun` and
  sets `Cache-Control: no-store`.
- **handler outcome plumbing**: a runtime returning `TurnOutcome{Stopped:true}`
  with a nil error produces `message_done{stopped:true}` (not `{}`), and a
  stopped outcome never sets `error`. This is the regression test for the gap
  where a nil error means "completed".
- **hub**: `WaitIdle` does not return while the session is still listed in
  `h.active` — a stream closing and unregistering concurrently must not let a
  waiter return early. Test the interleaving explicitly (close, then observe),
  not just the already-idle case.
- **web**: switching the composer to Stop while streaming; Stop renders a
  stopped (not failed) bubble and preserves partial text; send-while-streaming
  awaits the abort before posting; mount-time `/turn` shows the running banner
  and Stop; a stopped turn loaded from history renders as stopped, not as a
  completed answer; an intentional `AbortSignal` cancel produces **no** error
  bubble; an event arriving for a session that is no longer current does not
  mutate the active session's bubbles.
- **e2e**: with a live agent, start a turn, press Stop, assert the turn ends
  promptly and the session is idle; then send a second message mid-turn and
  assert the first turn is stopped and the second runs. Also reload after a Stop
  and assert the partial is marked stopped.
- **verification spike (before implementation)**: against a live gateway, settle
  the two open questions in "Stopped turns must survive a reload" — (1) does
  `/sessions/{key}/history` surface `openclawAbort` at all, and (2) can a
  mid-turn committed row be truncated (probe by aborting during a turn that has
  already had commentary committed and superseded).

  This is a **gate on the reload-marker part of the feature**, not a background
  note. If either answer comes back badly, the reload marker is **dropped** —
  the CubePilot-owned durable marker is out of scope (see above). The rest of the
  feature (Stop, redirect, live `stopped`) does not depend on it and proceeds
  either way.

## Rollout

No migration — both routes are additive and `message_done{stopped}` is an
optional field on the wire.

It is **not** behaviorally backward-compatible, though, and the rollout must not
pretend otherwise: a client that does not understand `stopped` renders a stopped
turn as a *completed* one, which is precisely the "truncated reply shown as
finished" failure this design exists to prevent. The web bundle and the API ship
from the same Helm chart, so they roll together; the requirement is simply that
they are not deployed separately, and that a stale cached web bundle is treated
as a bug rather than tolerated.
