# Chat narration (the agent's between-tool commentary) -- design

Date: 2026-09-18 · Status: approved for implementation · Scope: Portal chat streaming

## Context

The Portal chat stream shows two things: tool cards and the final answer. What
the agent says *between* tool calls -- "I'll read the skill first", "default has
four Pods, let me check the events next" -- never appears. The user asked for it
back:

> 流式输出只有卡片这种工具执行结果，没有 agent 的实时解说了，例如"发现了什么，我准备干什么"

It is not a rendering bug. Reproduced on the ycl cluster by driving a real turn
(`POST /api/v1/messages`, session `conv-narration-probe-1`, prompt: "先说明你打算
怎么查，再执行"). The SSE carried `message_start`, `agent_thinking`, two
`tool_call`/`tool_result` pairs, and one `message_delta` -- the final answer
only. The same session's transcript contains the missing narration as its own
assistant message:

```
[3] assistant TEXT: '打算这样查：\n\n1. **身份**：使用你自己的默认凭证…现在执行：'
[4] assistant TOOLCALL: exec {"command": "kubectl get pods -n default -o wide"}
```

The model produced it, the transcript recorded it, the stream dropped it.

## Verified facts (OpenClaw 2026.8.2, source-grounded)

**The gateway publishes assistant text on two lanes with different semantics.**

- The `chat` lane is a *display projection*: it deliberately drops
  `phase == "commentary"` assistant text and keeps only the final answer
  (`src/gateway/live-chat-projector.ts:161`,
  `shouldSuppressAssistantEventForLiveChat`). This is a product decision for
  chat surfaces -- one chat message is the answer, not the agent's musings.
- The `agent` lane publishes the same text as `stream: "assistant"` events
  (`src/gateway/server-chat.ts:1289-1299` for the uncoalesced path, `:1632` for
  the visible path), both reaching session-message subscribers through
  `broadcast("agent", …, {sessionKeys})`.

Both lanes are gated identically (`SESSION_SUBSCRIPTION_EVENTS`,
`server-broadcast.ts:112-121`; scope `[READ_SCOPE]`, `:43-45`), so a client that
receives `chat` frames receives `agent` frames too. The Portal's own tool cards
prove it receives `agent` frames.

**Commentary frames are block snapshots, not deltas.**

```js
// src/agents/embedded-agent-subscribe.handlers.messages.update.ts:258
buildAssistantStreamData({ text, replace: true, phase: "commentary", itemId })
```

`text` is the block's full text so far and `replace` is always true, so a
consumer never has to diff. `phase` is the gateway's own marker:
`type AssistantPhase = "commentary" | "final_answer"`
(`src/shared/chat-message-content.ts:24`). Our provider lane does stamp it --
`packages/ai/src/transports/openai-completions-stream.ts:564` tags the text
preceding a tool-call delta as commentary, and `:702` re-tags when the message
ends with `stopReason === "toolUse"`; `:696` *clears* the provisional tags when
it does not, which is what makes "followed by a tool call" the definition rather
than a guess.

**Two properties of our deployment that shape the contract.**

- `itemId` is absent on the completions lane: `emitAssistantCommentaryStreamData`
  passes it only for the Responses API
  (`src/agents/embedded-agent-subscribe.handlers.messages.stream.ts:201`). The
  Portal cannot use the gateway's block id; it must assign its own.
- Narration arrives as one block, not token by token. Our `qwen38-27b` turns
  carry `openclawDelivery.textPhaseRequiresTerminal: true` (confirmed on the
  probe transcript, on both the tool-calling and the final message), which makes
  `update.ts:283` withhold that message's deltas until the message ends. The
  block therefore lands immediately *before* the tool card it announces --
  live, but not streamed character by character. Narrowing this would mean
  changing the gateway's phase resolution; the Portal does not.

**This is a regression from #130.** Before the WS-only chat switch
(2026-09-08), the Portal read text from the OpenAI-compatible HTTP stream, which
writes every `stream: "assistant"` delta as content with no phase filter
(`src/gateway/openai-http.ts:1287`). That path carried the narration; the WS
live-chat lane that replaced it does not.

## Design

### 1. SSE contract: one new event

```jsonc
event: narration
data: {"type":"narration","sessionId":"agent:main:conv-x","blockId":"3",
       "text":"打算这样查：\n\n1. **身份**：…现在执行："}
```

- **Snapshot semantics.** `text` is the block's full text; a consumer replaces,
  never appends. The gateway publishes it that way, so the contract mirrors it
  and no consumer has to diff.
- **`blockId` is a per-turn ordinal assigned by the producer.** Same id means
  "still the same block, replace its text"; a new id means "new block". It is
  not the gateway's `itemId` (absent on our lane) -- see §2 for how it advances.
- **`text` is its own field, not `delta`.** Inside this contract `delta` means
  an increment (`message_delta`) and `text_replace` already abuses it for a
  snapshot; a third meaning would make the field unreadable.
- **The name is `narration`, not `commentary`.** This contract is
  runtime-neutral: another adapter maps whatever its backend calls this onto
  `narration`.

`message_delta` and `text_replace` are untouched and keep meaning *the final
answer*. That is the point of a separate event: the three places that reuse
`BubbleMsg.text` -- the stopped-turn evidence comparison, the answer panel, the
"Final result" label -- keep working without qualification.

### 2. Server projection (`internal/server/livetools.go`)

`liveProjector.feed` gains one case in its `agent` branch:

```
stream == "assistant" && data.phase == "commentary"
  -> narration{sessionId, blockId: <current block>, text: data.text}
```

Three rules, each with a reason:

- **Only `phase == "commentary"`.** Untagged assistant frames are not consumed:
  the gateway does not treat them as commentary either (its own
  `server-chat.agent-events.test.ts` asserts "Untagged text frame must not
  mirror"), and on the visible lane they carry the reply, which would duplicate
  the `chat` lane.
- **Stateless passthrough.** The lane is snapshot+replace by construction, so
  the projector forwards `data.text` without accumulating anything. Keeping a
  second copy would only add a way for the two to drift.
- **Whitespace-only snapshots are dropped** (`strings.TrimSpace(text) == ""`).
  The probe turn's first assistant text is literally `"\n\n"`; emitting it would
  put an empty grey paragraph in the bubble.

**How `blockId` advances.** The projector keeps the current block ordinal and
advances it every time it sees a tool call *start* -- including `ask_user`,
whose card is suppressed. The defining property of a commentary block is "an
assistant message that ended in a tool call", so a tool start is exactly the
boundary, and it stays exact for the one flow where event adjacency would fail:
a suppressed `ask_user` emits no card, and narration after the human answers
must not merge into the narration before the question.

### 3. Client model (`web/src/views/chat/model.ts`)

`BubbleMsg.tools: ToolCallVM[]` becomes an ordered list of **everything that
happened in the turn**, not just its content:

```ts
export type BubbleItem =
  | { kind: 'narration'; blockId: string; text: string }
  | { kind: 'tool'; tool: ToolCallVM }
  | { kind: 'approval'; confirm: BubbleConfirm }
  | { kind: 'question'; question: BubbleQuestion }
```

`BubbleMsg.text` still means *the reply* and is still rendered last, in the
answer panel. Ordering is safe because a run's final answer is terminal: any
later assistant text belongs to a new turn. Keeping `text` out of `items` is
what keeps the stopped-turn evidence, `headline`, and `superseded` untouched.

Approvals and questions belong in the list because they *happen* at a point in
the turn. Today they render at the end of the bubble, so a card from the second
tool call is drawn under the fifth tool card, and after the turn is over there
is no way to tell which execution a record was approving. Their existing
`BubbleConfirm` / `BubbleQuestion` records move into the item unchanged -- the
cards' rendering and state machines are untouched, only their position is.

A turn can carry several questions, and today they are a collection on the
bubble. As items they become one item each, in arrival order, which is what the
collection was approximating.

`pendingCards(bubbles)` -- what the composer dock draws -- walks the item lists
instead of the bubble's `confirm` / `questions` fields. Its semantics are
unchanged: pending only, across the whole thread.

Helpers that read the tool list (`attachToolResult`, `statusLine`'s
`Running N tool(s)` count, the answer panel's `tools.length > 0`) move to the
item list; nothing else about them changes.

### 4. Rendering (`web/src/views/chat/ChatThread.tsx`)

The item list renders in order, dispatching on `kind`: tool items render the
existing card, narration items render a new muted block --

```
[AI] ┌ tool read … Done
     └──────────────
     打算这样查：                    <- .narration: 13px, var(--muted), MdText
     1. **身份**：使用你自己的默认凭证…
     ┌ approval ─ 已批准 ─────────┐   <- settled record, at its own position
     └───────────────────────────┘
     ┌ tool exec kubectl get pods… Done
     └──────────────
     ╭─ Final result ─╮
```

Markdown rendering is shared with the reply (`MdText`), so headings, lists and
inline code in a narration block read the same way.

**A pending approval or question item renders nothing in the thread.** Its card
is in the composer dock, where it cannot scroll away (issue #204's decision,
unchanged). The item exists so the card has a position to settle into: when the
decision lands, the record appears where the turn actually paused instead of
jumping to the end of the bubble. While a card is pending the turn is parked on
it, so the item is the last one -- nothing is drawn below a hole.

### Known limitation: a settled approval record is live-only

The thread cannot rebuild an approval record, and this design does not change
that. The gateway keeps approval decisions in its own store, not in the session
transcript the thread replays, and the Portal's audit ledger writes every entry
as `Status: "executed"` regardless of the decision. After a reload:

- an **approved** write looks like any other executed tool call -- there is no
  trace that it was ever gated;
- a **denied** one is still visible, because the tool result the model received
  (`"Command did not run: approval was denied."`) is in the transcript and the
  history builder attaches it to the tool card.

So an approval item is correct within the view that watched it, and gone after a
reload. Making it durable means recording the decision somewhere that outlives
the view (the audit ledger is the natural home -- that is issue #180's root
cause, a separate change).

### 5. History replay (`web/src/views/chat/useChatThread.ts`)

The history builder gains the transcript's own rule, which is also the
gateway's: **an assistant message that contains a tool call contributes its text
as narration; a message without one contributes its text as the reply.**

- Narration is rebuilt in document order, so a reloaded turn looks like the
  turn did live -- one narration block per assistant message, interleaved with
  the tool cards it introduced.
- The reply **overwrites** `text` instead of the current
  `last.text = last.text + '\n' + c.text`, which is what fuses commentary and
  answer into one blob today.
- Whitespace-only text blocks are skipped, matching the server.
- The builder assigns its own `blockId` (the assistant message's ordinal). Live
  and replayed ids are separate spaces and never compared: each path only ever
  asks "is this the same block as the narration item I just wrote?", and each
  path answers within its own render.
- Approval and question items are **not** rebuilt from the transcript -- it does
  not carry them (see the known limitation). What changes is where the *pending*
  cards restored by the recovery path land: today each one appends a new bubble
  of its own (`useChatThread.ts:378-388`); now it becomes an item on the turn it
  belongs to, so answering it settles the record into place.

### 6. Tests

- **Go (`internal/server/livetools_test.go`)**: a commentary frame produces a
  `narration` event; a second snapshot of the same block keeps the `blockId` and
  replaces the text; a tool start between two commentary frames advances the
  `blockId`; a suppressed `ask_user` start advances it too; a whitespace-only
  snapshot emits nothing; a `phase == "final_answer"` or untagged assistant
  frame emits nothing.
- **Web**: `ChatView.test.tsx` -- a live turn rendering narration between two
  tool cards, in arrival order, with the reply still in the answer panel; a
  settled approval record drawn between the tool calls it sits between, not
  after them; `ChatThread.test.tsx` -- a narration item renders in the muted
  block and the tool cards stay expandable; history replay -- a transcript with
  commentary + answer rebuilds one narration block per assistant message and
  keeps the commentary out of `text`.

### 7. Documentation

`docs/cubepilot/cubepilot-design.md` (§4's event list) and
`docs/cubepilot/api.md`'s SSE section gain `narration`; the projector's
source-of-truth comment listing consumed gateway surfaces
(`internal/server/livetools.go:17-25`) gains the assistant/commentary line.

### Out of scope

- **The agent's reasoning** (`stream: "thinking"`). Not asked for, and it is a
  different lane with different semantics.
- **Character-by-character narration.** The gateway withholds it; see Verified
  facts.
- **Changing the gateway.** Its `chat` projection is correct for chat surfaces
  generally -- the Portal is an ops console that wants the process, so the
  Portal consumes the lane that carries it.
