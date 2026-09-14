# Re-attach to a parked turn (issue #167) -- design

Date: 2026-09-10 · Status: approved for implementation · Scope: issue #167

## Context

Answering an `ask_user` question from a card that was **restored after a page load**
settles the card but the resumed turn's output never arrives; a second reload shows
the finished turn, so the answer did work. The write-confirmation recovery path has
the same gap.

The mechanism, reproduced on a local kind cluster:

- A turn's events are written only to the SSE stream the `POST /api/messages` request
  opened (`handleMessages`' `emitLive` -> `Stream.Send`). Question and confirmation
  events are the exception -- they are injected into the session's **active stream** by
  `hub.PublishTo`.
- When that request goes away (page navigation/reload, a closed or discarded tab, a
  dropped connection) Go cancels `r.Context()`, `RunLiveTurn` returns and
  `releaseLive` unsubscribes the session. The gateway run keeps going.
- Recovery (`GET .../question/pending`, `.../confirm/pending`) rebuilds the card from
  the gateway's own state, but nothing re-attaches a stream, so the question pushes and
  the whole continuation after the answer have no destination.
- `hub.PublishTo` returns `false` when the session has no open stream and **every
  caller ignores it**, so the question push is dropped without a log line.

Turn counters make the teardown visible (`cubepilot_turns_total`): a client
disconnection mid-stream records `status=failed`, and the response body ends without a
`message_done`. There is no server-side line saying which of these happened, which is
why this change also adds the missing diagnostics.

## Verified facts

- `hitlManager` keeps `live: sessionKey -> *liveTurn`; `routeLive` fans the gateway's
  session-message events to that turn's sink (`t.sink`). `registerLive` /
  `releaseLive` own the entry, and `releaseLive` also unsubscribes the session.
- The gateway broadcasts the run's terminal chat frame to every session subscriber, so
  an observer that subscribes mid-run still learns the run went terminal.
- `ws.QuestionRecord` carries `RunID`, which is the run the parked question belongs to.
  `pendingApproval` carries no run id (it is never needed: an approval parks the run
  inside the same session).
- `Hub.Open` allows one stream per session and returns a conflict otherwise; `Stream`
  serialises writes and runs the idle heartbeat that keeps a parked stream alive.
- The live projector treats a terminal `chat` frame (`final` / `aborted` / `error`) as
  the end of the run, and a fresh projector streams only what happens after it starts,
  which is exactly what an attach needs.

## Design

### 1. `GET /api/sessions/{key}/stream` -- re-attach to a parked turn

A new session sub-resource (routed in `handleSessionSubresource` beside
`/question/pending`) that opens an SSE stream observing a turn the caller did not
start. It is an *observation*, not a turn:

- **Gate: the session must have something parked.** A pending, unexpired, projectable
  question (`question.list`) or a pending approval (`ApprovalService.Pending`), else
  `404`. This is the state in which the Portal has a card to answer, and it is also the
  state in which the run is *guaranteed idle* -- so nothing produced between the
  disconnect and this attach is missed. It also bounds the stream's life.
- **`409` when the session already has an active stream.** One stream per session is
  the existing hub invariant; the tab holding it already receives every event, so the
  attaching tab stays quiet instead of competing for the same feed.
- `503` when HITL is not configured, matching the other session sub-resources.
- The handler opens a hub stream, subscribes the session's live message stream and
  registers a live turn for it (section 2). It sends `message_done` when the run goes
  terminal, when the client disconnects, or when the hard cap (1h) is reached, so a
  stream can never outlive the process's interest in it.
- Authentication and the `503`/`400` handling mirror `handleQuestion`; the route is
  added to the same switch, so it inherits the server's user header handling.

Ordering note: the hub stream is opened before the gateway subscription (the stream is
an in-process object, the subscription a round trip). Events produced in that gap would
be missed, and none can be: the run is parked on a human decision, and the answer can
only be submitted by a browser that has already seen the card.

### 2. `hitlManager.AttachLiveTurn`

```go
// AttachLiveTurn subscribes the session's live message stream and registers a live
// turn for it without sending a message: the caller observes a run that is already in
// flight and parked on a human decision. It returns when the run goes terminal, the
// context is cancelled, or deadline elapses.
func (m *hitlManager) AttachLiveTurn(ctx context.Context, user, sessionKey, runID string,
    sink func(agentruntime.Event) error) error
```

Mirrors `RunLiveTurn` minus the session prep and the send: `conn` -> `registerLive` ->
`setRunID(runID)` -> `SubscribeSessionMessages` -> wait on `t.done` / `ctx.Done()`,
with `defer releaseLive`. It deliberately does **not** call `agent.wait`: the terminal
chat frame is the authoritative end of an observed run, and the attach has no run it
started to wait on. `runID` seeds `liveTurn.acceptRun` so a foreign run of the same
session cannot leak into the stream; an empty value (approvals) accepts any run of the
session.

The attach counts as neither a message nor a turn: `cubepilot_messages_total`,
`sessions_total` and `turns_total` are untouched. Its emit path still calls
`recordToolCall`, so the resumed part of a turn reaches the audit ledger even though
the stream that started it is gone.

It reports the same `TurnOutcome` `RunLiveTurn` does (issue #166), and the handler
writes its terminal through `liveTurnDone`. That is what keeps a run *another tab*
stopped from reaching the browser as a plain completion: the observing tab settles the
card and reads "Stopped", because the outcome carries `stopped` instead of an error.

### 3. Projecting an attached stream

An observer that joins mid-run sees the tail of tool calls the originating stream
started -- `command_output` end frames and `stream="tool"` results for calls whose start
it never saw. `liveProjector` now emits nothing for such a call: its card belongs to the
stream that started it, and an unknown-named result would reach the browser as a phantom
entry it cannot match (the first version of the attach did exactly that, and the browser
dropped it silently). A call this projection did see start is unaffected, so the
originating turn's projection is unchanged.

### 4. Diagnostics

Two gaps made this bug hard to see, both closed here:

- **Turn end.** `handleMessages` logs one line per turn with the reason: normal
  completion, `context.Canceled`/`DeadlineExceeded` (the client went away), or the
  error returned by `RunLiveTurn`. Until now a turn that the browser abandoned simply
  vanished from the server's view.
- **Dropped pushes.** `relayQuestionRequested`, `relayQuestionResolved`,
  `ApprovalService.Begin` and `ApprovalService.Resolve` check `hub.PublishTo`'s return
  and log a line naming the question/approval and the session when there is no stream
  to carry the event.

### 5. Web

- `streamSSE` gains an optional `onHttpError(status, body)` callback. When supplied and
  the response is not ok, the helper delegates and returns instead of emitting a
  synthetic `message_done` error -- the attach path needs to treat `409` (another tab is
  already streaming) as "nothing to do here", not as a failed turn.
- The SSE event switch in `sendMessage` is extracted into `applyTurnEvent(bubble, ev)`
  inside `ChatView`, so the attach stream and a turn stream render through one path.
- `recoverPending` starts the attach when it restored a question card (or, absent
  questions, a confirmation card), targeting that bubble, and aborts the previous
  attach when the session changes (`AbortController`), so a parked session cannot leak
  streams as the user browses.
- Answering in an attached tab therefore renders the continuation in place. The stream
  it attaches is also the session's active stream, so the card settles through the same
  `question_resolved` push as before, and a second tab answering the same session
  renders into whichever stream is active.
- The without-a-stream banner (issue #166) is left alone while the run is parked: it
  says "Still running…", which is true of a parked run, and its Stop is the only control
  this view has for one. It is retired when the attached stream reports a *real*
  terminal -- this view then knows nothing is running, and a Stop offering to abort a
  finished turn would be the wrong control.

## Testing

- `server`: the attach route's gates -- no parked decision -> 404, active stream -> 409,
  no HITL -> 503, missing session key -> 400; the happy path against the fake gateway
  (subscribe recorded, a terminal chat frame produces `message_done`); a question
  resolved while attached delivers `question_resolved` plus the continuation.
- `server`: `AttachLiveTurn` registers/releases the live turn and routes a
  session-message frame to the sink; a foreign run id is rejected when seeded; a run
  another tab stopped comes back as `TurnOutcome{Stopped: true}`, not as an error.
- `server`: dropped-push logging -- a question relayed with no open stream logs the
  drop, and the existing tests that assert nothing is published keep passing.
- `server`: the projector emits nothing for a call whose start predates it (the tail
  an attach sees), while a call it saw start still reports normally.

## Out of scope

- Content generated between a disconnect and a later attach, i.e. observing a turn that
  is actively generating. Every card is only on screen while the run is parked, so this
  flow loses nothing. Widening the gate to "any run in flight" would need the in-flight
  signal #166 has since landed (`/turn`, from `chat.history`'s `inFlightRun`); it is not
  this change, and keeping the gate narrow is also what stops an idle observer from
  holding a session's single stream.
- Stopping or redirecting a turn, and any "is this session busy" indicator -- #166.
- Free-text answers and `isSecret` questions -- #161's scope.
- More than one stream per session. The attach takes the session's single stream, so a
  second tab is refused with `409` and shows the restored card without live output; the
  tab that holds the stream is the one that renders. Fanning a session out to several
  observers is the hub change #165 tracks.

## Known limitation

While an attach holds the session's stream, a new turn for that session is refused with
`409` -- the one-stream invariant, unchanged by this work. That is harmless for as long
as the parked run is real (the answer's continuation is being delivered), but a stale
pending question record -- a run that died with its question unresolved, which the
gateway only expires on its own deadline -- would keep a tab holding the stream for up
to the attach cap. The ways out are the ones the Portal already has: leave the session,
reload, or stop the turn from the banner.
