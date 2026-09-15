# Portal floating assistant widget (issue #30) -- design

Date: 2026-09-15 · Status: approved for implementation · Scope: issue #30

## Context

Issue #30 asks for a chat entry point that follows the user around: a
floating-window button at the bottom-right of every page that opens a
conversation with the user's agent, alongside the dedicated chat tab. The Portal
has the dedicated tab today -- the Chat view -- but no floating entry, so
reaching the assistant from the Scheduled Tasks or Agent Config page means
navigating away from what you were doing.

The issue also carries an acceptance criterion that shapes the whole design:

> Floating window opens from any page; floating window and dedicated tab
> **share the same session**

and the Phase 1 design doc states the same intent in its own words
(`docs/cubepilot/cubepilot-design.md`:14): `各页面右下角按钮弹浮窗对话（session
全局统一、不按模块区分）`.

Meanwhile the Chat view has grown a session list, a search box and "New
conversation" -- it is deliberately multi-session. The two facts coexist rather
than supersede each other (both landed within a week of each other in August
2026), so this design treats the criterion as a requirement to satisfy, not as
stale text: the widget and the Chat view point at **the same conversation**.

## What already exists

Grounded in the current tree, because most of this feature is assembly rather
than invention:

- **The thread machinery is one component.** `web/src/views/ChatView.tsx` is
  1914 lines owning the session list, the thread, SSE streaming, history
  loading, tool cards, write-confirmation cards, ask-user question cards, Stop,
  and post-reload recovery. Nothing is reusable as-is; see §3.
- **Sessions are gateway-side and the list is unfiltered.** `GET
  /api/v1/sessions` is a pass-through of the gateway's `sessions_list`
  (`internal/server/handlers.go`:84 →
  `internal/openclaw/client.go`:141). There is no server-side notion of a
  hidden session.
- **The client may choose the session key.** `handleMessages` takes
  `sessionId` from the request body and uses it, canonicalized, as the session
  key (`internal/server/handlers.go`:137-143). `canonicalSessionKey("conv-x")`
  is `agent:main:conv-x` (`internal/server/handlers.go`:29-34).
- **A fixed key works from the first message.** `RunLiveTurn` calls
  `gw.CreateSession(sessionKey)` before sending, which creates-or-adopts
  (`internal/server/live.go`:159-163). A key that has never been used is not an
  error.
- **One stream per session.** `hub.Open` returns 409 for a second concurrent
  turn on the same session (`internal/server/handlers.go`:154). This is the
  gate that makes two views on one conversation safe.
- **The two-view machinery is already written.** `runningElsewhere`,
  `checkTurnElsewhere` and `recoverPending` exist in ChatView
  (`web/src/views/ChatView.tsx`:483, :629, :678) precisely for "this
  conversation is being driven somewhere I cannot see".
- **Anything outside `<Routes>` survives navigation.** The router mounts views
  inside `<main className="content">` (`web/src/App.tsx`:204-214), so a sibling
  of that element is never unmounted by a route change.
- **There is no browser-side test runner.** `web/package.json` has no test
  script, and CI's `web` job runs only `npm ci` and `npm run build`
  (`.github/workflows/ci.yaml`:99-134) -- so types and bundling are guarded and
  behaviour is not. PR #168 recorded a hand-built component harness (243
  checks, mutation-checked) that was never committed, and named adding a runner
  as the highest-value follow-up on that branch. Issue #30's fourth acceptance
  criterion asks for frontend unit tests directly.

## Goals

- Summon the assistant from any page without leaving it.
- One conversation, reachable from both the widget and the Chat view.
- Full parity with the Chat view: tool calls, write-confirmation and question
  cards, Stop.
- An in-flight turn survives page navigation and panel collapse.

## Non-goals

- Reworking the Chat view's layout, or its multi-session model.
- Session management UI (rename, delete, archive) -- issue #30 puts this in
  Phase 2.
- Multimodal input.
- A per-module conversation. The widget is one conversation on every page.

## Design

### 1. Session model: one shared conversation

The widget binds to a fixed key, `conv-assistant` (canonical:
`agent:main:conv-assistant`). It is an ordinary session: it appears in the Chat
view's list like any other entry, and clicking that entry opens the same
conversation at full width.

This is what satisfies issue #30's "share the same session". The alternative --
a private session hidden from the list -- was rejected; see §Rejected
alternatives.

The key is created lazily: it does not exist until the widget sends its first
message, so a user who never opens the widget sees no extra entry in their
list. Until then a history read for it answers 404, which the widget renders as
an empty conversation (§5).

Two views on one conversation is safe because of the existing 409 gate: only
one turn can be in flight per session, and both views already need to handle
"a turn is running that I am not streaming" -- the mechanism ChatView uses for
a reload (`runningElsewhere`). The widget reuses it rather than inventing a
second one.

### 2. Placement and lifetime

`<AssistantWidget />` mounts in `App.tsx` as a sibling of `<div
className="main">`, outside `<Routes>`. It therefore lives for the whole app
session: a route change does not unmount it, which is how "keep talking after
the page changes" is achieved without any persistence layer.

Panel open/closed is local state. Collapsing the panel **hides** the thread
with CSS; it does not unmount it. That matters: an in-flight turn keeps
streaming while the panel is closed, and reopening shows the reply that
arrived. Unmounting instead would tear the stream down and leave the turn
running invisibly server-side -- the failure mode `streamSSE`'s abort contract
warns about (`web/src/api/sse.ts`:14-21).

### 3. Component structure

The widget and the Chat view share the conversation machinery, so it is
extracted first. Splitting it is the bulk of this work; everything after it is
assembly.

| Unit | Responsibility |
| --- | --- |
| `useChatThread(sessionKey)` | All conversation state and lifecycle: bubbles, the SSE turn, history loading, send, Stop, pending confirmation/question recovery, the "running elsewhere" check. |
| `<ChatThread>` | Presentational shell: header, thread, composer. Consumes the hook. |
| `ChatView` | Keeps the session list and search; renders `<ChatThread>` for the selected session. |
| `<AssistantWidget>` | The floating button and panel; renders `<ChatThread>` bound to the fixed key. |

Two callers, one implementation. The alternative -- a second, smaller chat
implementation for the widget -- would duplicate the SSE turn lifecycle and the
HITL cards, which is exactly where divergence is most expensive: a fix to a
parked-card race would have to be made twice, and the second time would be
found by a user.

The hook takes `string | null`, because the two callers differ here and only
here: ChatView's "new conversation" starts with no key and learns it from the
first `message_start` (`web/src/views/ChatView.tsx`:927), while the widget has
its key from the first render.

Extraction is behaviour-preserving and lands as its own change, before the
widget, so that a regression in the refactor cannot be confused with a
regression in the widget.

### 4. Widget behaviour

**Closed.** A round button, fixed to the bottom-right, above the toast
(`web/src/App.tsx`:217-220). It renders on every route.

**Opening.** On the closed → open transition the widget re-fetches the
session's history, unless it is itself streaming a turn. This is what makes a
message sent from the Chat view appear in the widget: the widget is not the
only writer to this conversation, so it cannot assume its local state is
current.

**Running elsewhere.** If the session has a turn in flight that the widget is
not streaming, it shows the same affordance ChatView shows -- an indication
that the turn is running, with Stop. Reused, not reimplemented.

**Composing.** The composer is the Chat view's, unchanged: send, Stop while
streaming, and the same card interactions (approve / reject / allow-always,
question options, dismiss).

**Sizing.** ~400px wide, ~560px tall, capped to the viewport on small screens.
Long content -- a YAML block, a diff, a tool result -- stays cramped here, and
that is the accepted trade: the widget is for asking, and the Chat view is
where long content is read. §1 is what makes that trade safe, because the same
conversation is one click away at full width.

### 5. Backend: an unknown session's history is a 404

The gateway answers `404 {"ok":false,"error":{"type":"not_found","message":
"Session not found: <key>"}}` for the history of a session that does not exist
(`openclaw/src/gateway/sessions-history-http.ts`:202). CubePilot's client
flattens any non-200 into an error, and `handleHistory` returns it as **502**.

So "this conversation has not started yet" currently arrives at the browser as
"the gateway is broken" -- indistinguishable from a real outage, which is the
one thing the widget must not confuse, because rendering an empty thread for a
genuine gateway failure would look to the user like their history was erased
(`internal/server/handlers.go`:110-114).

Fix: `handleHistory` maps the gateway's 404 to our own 404 with a typed error
body. The frontend already carries the status (`ApiError.status`,
`web/src/api/client.ts`:6-13), so the widget treats 404 as "no conversation
yet" and every other failure as an error it must show.

This is a small change with independent value -- it corrects the error
semantics for any consumer of the route, not just the widget.

## Edge cases

- **Two tabs.** Each tab's widget addresses the same key. The second to send
  gets the 409 and shows "running elsewhere", which is the behaviour the Chat
  view already has when open in two tabs.
- **The Chat view has the conversation selected while the widget is open.**
  Both render it; only one can have a turn in flight, and the other reports it
  as running elsewhere. No merge is attempted.
- **Widget closed mid-turn, then a reload.** The turn is still running
  server-side and the widget holds no stream, so on mount it sees the session
  is busy and offers Stop -- the same path ChatView takes after a reload.
- **The gateway 404s on mount, before the first message.** Rendered as an
  empty conversation, not an error (§5).
- **The agent is not ready.** The existing readiness nudge
  (`web/src/App.tsx`:129-140) already steers every view to Agent Config; the
  widget inherits it and needs no separate handling.

## Rejected alternatives

### A private session, hidden from the Chat view's list

The widget gets its own session and `loadSessions` filters it out. Attractive
because the widget would then be the only writer, so it needs no re-fetch on
open and no "running elsewhere" state.

Rejected because it fails issue #30's "share the same session" and loses the
experience behind it: a conversation that starts in the widget would be
stranded there. The widget is a small window, and the moment a conversation
produces something worth keeping -- a CRD to apply, a diagnosis to reread --
the user wants it at full width. The saving is smaller than it looks: the
machinery for two views on one session already exists and is exercised.

### Collapse to a single global session

Take `session 全局统一` literally and remove the Chat view's session list
entirely, so the whole product has one conversation.

Rejected because the multi-session Chat view is deliberate and older than this
issue: it shipped in the Vue Portal (`6c2517b`, 2026-08-19) and survived the
React migration. A single unbounded conversation also degrades in practice --
unrelated work accumulates in one context, and there is no way back to "the
conversation where I changed that CRD".

### Reuse ChatView wholesale behind a `variant` prop

Give `ChatView` a `mode: 'page' | 'widget'` prop and hide the session panel
with CSS in widget mode. A far smaller diff than §3.

Rejected because the prop would be permanent: the widget needs a fixed session
key, a different header and a different empty state, so `variant` would grow
into a second layout inside a component that is already 1914 lines. It also
leaves the conversation logic untestable in isolation, which is where the
value is.

## Testing

Issue #30 asks for frontend unit tests covering session state and event
rendering, and this work refactors the one part of the frontend with no
regression net. Both point the same way, so testing lands **first**, as its own
change, before any refactor:

- Add `vitest` + `jsdom` + `@testing-library/react` and a `test` script, and
  run it from CI's existing `web` job (`.github/workflows/ci.yaml`:99-134). No
  product change in this step.
- Cover the main line of a turn with the gateway stubbed at `fetch`: send →
  incremental text → tool card → terminal, plus a write-confirmation card
  approved and rejected, plus Stop.
- After §3, the same suite runs against the extracted hook, and the refactor's
  claim of "no behaviour change" is checked rather than asserted.

Backend: `handleHistory`'s 404 mapping gets a table test alongside the existing
handler tests.

## Sequencing

One PR, four commits in dependency order:

1. `test(web)`: introduce the runner and the first turn-level tests. No product
   change.
2. `refactor(web)`: extract `useChatThread` and `<ChatThread>`; ChatView keeps
   its session list and gains nothing else. Behaviour-preserving.
3. `fix(api)`: map the gateway's 404 on session history to a typed 404.
4. `feat(web)`: the widget itself, on top of 1-3.

The ordering is load-bearing and belongs in the commits, not in separate PRs:
without 1 a bug in 2 is invisible, and without 2 the widget would duplicate the
machinery 2 exists to share. No piece has to reach `main` before the next can
be written, so none of them is a PR of its own.

## Out of scope

- Session management (rename, delete, archive).
- Notifications or an unread badge on the collapsed button.
- Persisting panel open/closed state across reloads.
- Any change to how the Chat view lists or titles sessions beyond the new
  conversation appearing in it.

## Open questions

- **Widget session title.** `sessions_list` supplies a title and falls back to
  the raw key (`internal/openclaw/client.go`:155-158), so the entry may read as
  `agent:main:conv-assistant` until the gateway titles it. If that reads badly
  in the list, the fix is a display-side label in the Chat view, not a rename
  of the key.
- **Mobile.** The Portal is a desktop console today. The widget is designed for
  a desktop viewport; a phone-width layout is not addressed here.
