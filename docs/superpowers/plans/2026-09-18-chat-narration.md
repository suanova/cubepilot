# Chat narration -- implementation plan

Spec: `docs/superpowers/specs/2026-09-18-chat-narration-design.md` · Issue #216

## Steps

1. **Server: the event** (`internal/runtime/contracts.go`)
   Add `EventNarration = "narration"` and the `Text` / `BlockID` fields to
   `Event`. Failing test first in `internal/server/livetools_test.go`: a
   commentary frame must produce a `narration` event.

2. **Server: the projection** (`internal/server/livetools.go`)
   `feed`'s `agent` branch gains `stream == "assistant" && data.phase ==
   "commentary"`. The projector keeps the current narration block ordinal and
   advances it on every tool start, suppressed ones included. Drop
   whitespace-only snapshots. Cover: repeat snapshot keeps the block, tool start
   advances it, `ask_user` start advances it, blank snapshot emits nothing,
   `final_answer` and unphased frames emit nothing.

3. **Web: the event type** (`web/src/api/types.ts`)
   `SSENarration` in the `SSEEvent` union.

4. **Web: the model** (`web/src/views/chat/model.ts`)
   `BubbleItem` (`narration` / `tool` / `approval` / `question`), `BubbleMsg`
   loses `tools`, `confirm`, `questions`. `attachToolResult` and `statusLine`
   move to the item list; `pendingCards` walks it. Failing tests first in
   `model`'s existing test home.

5. **Web: live** (`web/src/views/chat/useChatThread.ts`)
   `applyTurnEvent` pushes narration (replace by `blockId`, else new item), a
   tool item, an approval item, a question item. The recovery path
   (`:356-420`) adds its restored cards as items on an existing turn rather than
   as a new bubble.

6. **Web: history** (`web/src/views/chat/useChatThread.ts`)
   The builder splits assistant messages by whether they carry a tool call:
   narration vs reply, `text` overwritten rather than concatenated, blank
   blocks skipped.

7. **Web: rendering** (`web/src/views/chat/ChatThread.tsx`, `styles/main.css`)
   Render `items` in order; narration as the muted `.narration` block; a pending
   approval/question item renders nothing (the dock owns it); a settled one
   renders its existing card.

8. **Docs** (`docs/cubepilot/cubepilot-design.md` §4 event list,
   `docs/cubepilot/api.md` SSE section, `internal/server/livetools.go`'s
   consumed-surfaces comment).

9. **Checks**
   `go test ./...`, `golangci-lint run`, `cd web && npm test`, `npm run build`.
   Then re-run the probe turn against the deployed gateway to confirm the frames
   carry `phase: "commentary"` and the narration arrives before its tool card --
   the one link only the real gateway can confirm. That needs an image build and
   deploy, so it is a manual step, not part of CI.

## Commit order

One PR (issue #216). Commits: server projection, web model + render, docs.

## Out of scope

- Making approval records survive a reload (issue #180's root cause).
- Showing the agent's reasoning (`stream: "thinking"`), plan, patch or media.
- Changing the gateway's `chat` projection.
