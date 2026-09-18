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

## What the deployment actually sends (measured)

The first version of this design was read out of the gateway source and got the
lane wrong. The frames below are from a real turn against the deployed gateway
(OpenClaw 2026.8.2), logged at the API's own gateway boundary while the turn
ran, with a prompt that makes the agent state its plan before running it.

**The narration is an `item` of kind `preamble`:**

```
ev=agent stream="item" phase="update" kind="preamble"
   progressText="我先说明查询方式，然后执行。 **查…"
```

One per step, arriving before the tool card that step introduces. This is the
surface the Control UI reads (`ui/src/pages/chat/tool-stream-preamble.ts`), and
the gateway builds it in `src/agents/embedded-agent-subscribe.reply-delivery.ts`
by *rewriting* an assistant event classified as `phase === "commentary"` -- so
commentary never leaves the runtime as assistant text at all.

**The assistant lane carries the answer alone**: one frame per turn, with no
`phase` field (`keys=[text,delta]`), and the `chat` lane's deltas carry the same
answer. The source-level reading -- "commentary is an assistant event with
`phase == "commentary"` that the chat lane drops" -- describes the gateway's
internals correctly, but what a client receives for a step is the preamble item.

**Two properties of this lane shape the contract:**

- `progressText` is the step **folded onto one line** by the gateway
  (`data.text.replace(/\s+/g, " ").trim()`), so the live narration is a
  single-line summary of the step rather than its Markdown layout.
- The live item carries **no `itemId`** in this gateway version (measured:
  `itemId=""`), which is a known gateway bug, fixed later in
  `fb598bdc7d3`. The projection therefore numbers blocks itself.

**The durable row is marked, and holds the step in full.** In the transcript a
step is its own assistant row:

```json
{"role":"assistant","content":[{"type":"text","text":"\n\n先说明查询方案：…"}],
 "openclawStreamFallback":{"itemId":"commentary-0","source":"segment",
                           "replacementText":"\n\n先说明查询方案：…"}}
```

That marker is how the Control UI reconciles a replayed row with the live item
(`ui/src/pages/chat/stream-reconciliation.ts`), and it is the only thing that
tells a step from the answer in history: both are assistant text, and neither
carries a tool call.

**Why the previous reading was wrong**, recorded so it is not repeated: it looked
for `phase == "commentary"` on the `agent` lane and concluded the narration was
never published. It was published all along, as an item whose `kind` the probe
did not log. Reading a lane from the source is not the same as watching it.

**On the OpenAI-compatible path this replaced (#130)**, the commentary was
reachable through its own fallback (`src/gateway/openai-http.ts:1106`, `:1394`),
so the WS lane that replaced it needs the preamble item read explicitly to keep
the same content in front of a reader.

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
stream == "item" && data.kind == "preamble"
  -> narration{sessionId, blockId: <current block>, text: data.progressText}
```

Four rules, each with a reason:

- **Read before the tool items.** A preamble carries no `toolCallId`, and the
  item branch drops frames without one; the narration has to be taken first.
- **Only `kind == "preamble"`.** Assistant frames on this lane are the answer
  (the `chat` lane carries the same text, so projecting them would print it
  twice), and `kind == "tool"` / `"command"` items are cards, not narration.
- **Stateless passthrough.** `progressText` is the step's whole text, already
  folded onto one line by the gateway, so nothing is accumulated here. A step
  that folds to nothing is dropped -- it is not a paragraph, and drawing it
  would put an empty line between two cards.
- **`blockId` advances on every tool call that starts**, including `ask_user`,
  whose card is suppressed. A step is what a tool call ends, so a tool start is
  exactly the boundary -- and it stays exact for the one flow where adjacency
  would fail: no card is emitted for a suppressed `ask_user`, and narration on
  either side of a human's answer must not merge into one paragraph.

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

- **A step is recognised by the gateway's own marker**, not by shape:
  `openclawStreamFallback.itemId` is set on the rows the gateway published as
  commentary (the same field the Control UI reconciles by), and its
  `replacementText` holds the step in full -- which is what a reload reads, since
  the live lane only ever carried it folded onto one line. A row with text and a
  tool call counts as a step too, which is the gateway's live rule for
  commentary; the answer is the assistant row that is neither.
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
