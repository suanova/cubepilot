// The conversation's data model and the pure helpers over it.
//
// Nothing here touches React or the network: `useChatThread` owns one
// conversation's lifecycle, and this is the vocabulary the hook and the
// renderer share. Keeping it separate is what lets the model be read without
// the state machine around it.
import type { QuestionItem } from '@/api/types'

export interface ToolCallVM {
  name: string
  cmd: string
  callID: string
  done: boolean
  // result is the tool's output; rendered directly under its command card so
  // each command reads together with what it produced.
  result?: string
}

// Turn lifecycle phases; drives the per-bubble status line so the user can
// always tell "still working" (think/tools/stream) from "finished" (done).
export type BubblePhase = 'thinking' | 'tools' | 'streaming' | 'done'

// A write operation paused for a human decision (issue #20 HITL).
export interface BubbleConfirm {
  sessionId: string
  approvalId: string
  command: string
  level: string
  message?: string
  resolved?: boolean // decision sent
  approved?: boolean
  busy?: boolean
  error?: string
}

// A question the agent is blocked on until a human answers (issue #161).
export interface BubbleQuestion {
  sessionId: string
  questionId: string // gateway question id the answer is submitted with
  items: QuestionItem[]
  // Local deadline derived from the event's remaining seconds. Working from a
  // remainder keeps the countdown immune to clock skew between this browser and
  // the API, and lets the shared 1s ticker drive it.
  deadline?: number
  picked: Record<string, string[]> // questionId -> selected option labels
  resolved?: boolean
  outcome?: string // answered | cancelled | expired
  busy?: boolean
  error?: string
}

// StopEvidence is what this view knows about a turn it stopped, and the only
// thing a later history render may use to recognise that turn's row.
//
// The transcript the server hands back carries no stopped field (the design's
// v1 decision), and a stopped turn that produced no text leaves no row at all:
// the gateway captures an aborted partial only when the run's text buffer has
// content. "The session's newest assistant bubble" is therefore not the stopped
// turn unless something says it is, and marking the wrong bubble paints a
// *completed* answer "Stopped". A stop has one of two ways to say so, depending
// on whether this view held a stream for the turn; see `stopEvidence`.
export type StopEvidence =
  // A stop over a stream this view held: the partial it watched the turn
  // produce. Only a bubble carrying exactly that text may be relabelled, so a
  // newer turn another tab started cannot be relabelled by this one.
  | { text: string }
  // A stop with no stream of this view's own: the transcript this
  // view had already rendered, as `transcriptShape`. See `applyStoppedTurn` for
  // what the reload has to look like.
  | { users: string[]; lastText: string }

export interface BubbleMsg {
  kind: 'user' | 'assistant'
  text?: string
  tools: ToolCallVM[]
  error?: string
  thinking: boolean // true while phase !== 'done'
  phase?: BubblePhase
  phaseAt?: number // Date.now() when the current phase started
  confirm?: BubbleConfirm // a pending/resolved write confirmation on this bubble
  // The user stopped this turn. Its partial text is not a finished answer, so
  // the bubble reads "Stopped" rather than the green Done check (issue #166).
  stopped?: boolean
  // The stream for this turn ended without a server terminal (sse.ts's
  // synthetic message_done): a transport failure, not a turn outcome, so this
  // is neither `stopped` nor `error`. The run may still be executing, which is
  // why the bubble says so rather than claiming Done or Failed, and why its
  // HITL cards are left live and answerable. Carries the reason the stream gave
  // up, for display.
  transportLost?: string
  // Commentary the gateway rewrote (`text_replace`, a snapshot that supersedes
  // what it streamed before). Kept rather than dropped: the rewrite is what the
  // reply reads now, and the text it replaced is what the user was reading when
  // the tool it introduces asked for their approval. Rendered as a disclosure
  // under the reply -- the reply is what they need, not the archaeology.
  superseded?: string[]
  // Every question this turn has asked, in order. The agent can ask several in
  // one turn (a blocked ask_user resumes, then another follows), so they are
  // kept as a collection rather than one slot that a later question replaces --
  // that would drop an answered card's record and, on the recovery path, the
  // other open questions.
  questions?: BubbleQuestion[]
}

// newBubbleQuestion builds the card state for one question event or recovery
// entry.
export function newBubbleQuestion(sessionId: string, questionId: string, items: QuestionItem[], timeoutSeconds?: number): BubbleQuestion {
  return {
    sessionId,
    questionId,
    items,
    deadline: timeoutSeconds ? Date.now() + timeoutSeconds * 1000 : undefined,
    picked: {},
  }
}

// questionAnswered reports whether every question in the card has a selection.
export function questionAnswered(q: BubbleQuestion): boolean {
  return q.items.every((it) => (q.picked[it.questionId] || []).length > 0)
}

// attachToolResult pairs a tool's output with the tool call that produced it:
// exact callId when the stream carries one, otherwise the oldest call without
// a result yet (the gateway emits tool calls and results in the same order).
export function attachToolResult(tools: ToolCallVM[], callID: string, output: string) {
  const t =
    (callID && tools.find((x) => x.callID === callID)) ||
    tools.find((x) => !x.done) ||
    tools.find((x) => !x.result)
  if (!t) return
  t.result = t.result ? t.result + '\n' + output : output
  t.done = true
}

// settleBubbleCards closes the human-in-the-loop cards of a bubble whose turn
// has just ended.
//
// The abort settles these records server-side and publishes approval_resolved /
// question_resolved alongside the terminal, but that event is not reliable: the
// gateway broadcasts the aborted chat frame before it answers the chat.abort
// RPC, so the resolve races the stream's own close (Hub.PublishTo silently does
// nothing once the stream is gone) and for a question it is two gateway round
// trips behind. The record is deleted either way, so a card left live would
// offer Approve/Reject buttons that POST to something that no longer exists.
// `message_done` is the one event the stream guarantees, so the cards are
// settled from it as well. Doing it twice is harmless: it is idempotent.
//
// Only for a *confirmed* server terminal. A synthesized one
// (`message_done{synthetic:true}`) reports a transport failure, not the end of
// the turn: the run may still be parked on exactly these cards, and closing
// them would take away the only controls that can unblock it. The caller
// decides; this function must not be reached on that path.
export function settleBubbleCards(b: BubbleMsg) {
  if (b.confirm && !b.confirm.resolved) {
    b.confirm.resolved = true
    // `approved` stays undefined on purpose: nobody decided this, which renders
    // the neutral stopped pill rather than the red "Rejected" the user never
    // chose. A decision that did happen has already set the field.
    b.confirm.busy = false
  }
  for (const q of b.questions || []) {
    if (q.resolved) continue
    q.resolved = true
    // The same outcome the server's own settle publishes, so the card looks the
    // same whether its resolved event arrived or was lost.
    q.outcome = 'cancelled'
    q.busy = false
  }
}


export function setPhase(b: BubbleMsg, phase: BubblePhase) {
  b.phase = phase
  b.phaseAt = Date.now()
  b.thinking = phase !== 'done'
}

// toolArgsDisplay renders a tool call's headline for its card. Shell tools
// (exec) carry `arguments.command` / `arguments.cmd` -- show that verbatim.
// Other tools (read / write / search...) carry named arguments instead; surface
// them so the card is never an empty shell: a single argument shows its value,
// several show `key: value` pairs. Secret-looking keys are masked so argument
// summaries never leak credentials.
const secretKeyRe =
  /(password|passwd|token|secret|api[_-]?key|apikey|access[_-]?key|authorization|credential|private[_-]?key)/i
const secretMask = '••••••'

// redact replaces the value of any secret-looking key at any nesting depth, so
// serialized argument summaries never leak credentials embedded in nested
// records or arrays (e.g. { headers: { authorization: "Bearer …" } }).
export function redact(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(redact)
  if (value && typeof value === 'object') {
    const out: Record<string, unknown> = {}
    for (const [k, v] of Object.entries(value as Record<string, unknown>)) {
      out[k] = secretKeyRe.test(k) ? secretMask : redact(v)
    }
    return out
  }
  return value
}

export function summarizeArgs(args: unknown): string {
  if (args == null) return ''
  if (typeof args !== 'object') return String(args)
  const rec = args as Record<string, unknown>
  if (typeof rec.command === 'string') return rec.command
  if (typeof rec.cmd === 'string') return rec.cmd
  const pairs = Object.entries(rec).map(([k, v]) => ({
    k,
    text: typeof v === 'string' ? v : JSON.stringify(redact(v)),
    secret: secretKeyRe.test(k),
  }))
  if (pairs.length === 1) {
    const p = pairs[0]
    return p.secret ? `${p.k}: ${secretMask}` : p.text
  }
  return pairs.map((p) => `${p.k}: ${p.secret ? secretMask : p.text}`).join('  ')
}

export function toolArgsDisplay(args: unknown): string {
  if (typeof args !== 'string') return summarizeArgs(args)
  try {
    return summarizeArgs(JSON.parse(args))
  } catch {
    return args
  }
}

// MdText renders the agent's text as Markdown (headings / lists / emphasis /
// inline code / code blocks). remark-breaks keeps single line breaks as breaks,
// which chat text uses heavily. Raw HTML in the source is escaped by
// react-markdown by default.

export function statusLine(b: BubbleMsg): string {
  if (b.kind === 'user') return ''
  // The lost-connection headline is checked *before* the phase guard, and it
  // has to be: sse.ts synthesizes its terminal on paths that run before any
  // event arrives -- a non-2xx response, a rejected fetch, a body with no
  // reader -- so the bubble is left with no phase at all, and a guard that
  // returned '' on an unset phase would render the amber line empty and leave
  // only the raw reason underneath. The headline is the whole point of that
  // state, so it must not depend on a phase the failure never set.
  if (b.transportLost) return 'Lost connection — this turn may still be running'
  if (!b.phase) return ''
  // Stopped outranks the parked-state lines below: a turn that was stopped
  // cannot be waiting on a human, even when one of its cards has not been
  // settled by the stream yet (a transient the settled event closes).
  if (b.stopped) return 'Stopped'
  if (b.kind === 'assistant' && b.confirm && !b.confirm.resolved) return 'Awaiting your approval...'
  if (b.kind === 'assistant' && (b.questions || []).some((q) => !q.resolved)) return 'Awaiting your answer...'
  const secs = b.phaseAt ? Math.max(0, Math.round((Date.now() - b.phaseAt) / 1000)) : 0
  switch (b.phase) {
    case 'thinking':
      return `Thinking... ${secs}s`
    case 'tools': {
      const n = b.tools.filter((t) => !t.done).length
      return n > 0 ? `Running ${n} tool(s)... ${secs}s` : `Collating tool results / thinking... ${secs}s`
    }
    case 'streaming':
      return `Streaming reply... ${secs}s`
    case 'done':
      return 'Done'
  }
}

// TurnHeadline is the conversation's own state, as the header reports it. The
// tone drives the colour, so "still working" and "finished" are distinguishable
// without reading the text.
export interface TurnHeadline {
  text: string
  tone: 'running' | 'done' | 'stopped' | 'lost' | 'error'
}

// headline is what the header says about the conversation: the state of its
// newest turn.
//
// It carries the same line the bubble used to render, and that is the point of
// moving it: a turn's state is a fact about the turn, not a paragraph of it, and
// drawn inside the bubble it left the screen as soon as the turn produced a
// screenful of output -- exactly when the user starts wondering whether it is
// still going. Drawn in the header, it cannot scroll away.
export function headline(bubbles: BubbleMsg[]): TurnHeadline | null {
  const last = bubbles[bubbles.length - 1]
  // A turn starts when its prompt is sent: with the prompt newest, the turn it
  // belongs to has not begun and there is nothing to report about it.
  if (!last || last.kind !== 'assistant') return null
  const text = statusLine(last)
  if (!text) return null
  if (last.transportLost) return { text, tone: 'lost' }
  // An error does not get its message here: it can be a paragraph, and the
  // bubble it belongs to still carries it verbatim.
  if (last.error) return { text: 'Failed', tone: 'error' }
  if (last.stopped) return { text, tone: 'stopped' }
  if (last.phase === 'done') return { text, tone: 'done' }
  return { text, tone: 'running' }
}

// PendingCards is what the agent is blocked on, split by kind because the two
// are answered by different controls.
export interface PendingCards {
  confirm?: BubbleConfirm
  questions: BubbleQuestion[]
}

// pendingCards collects the cards still waiting for the user.
//
// They are collected across the whole thread rather than read off the newest
// bubble, because a parked write can belong to a turn several bubbles back (a
// redirect leaves the older turn's card live; a reload restores it as a bubble
// of its own), and it is still waiting for the user either way. The platform
// holds one pending approval per session, so the last one seen is the only one
// there is; questions are a set, and the agent can be parked on several.
export function pendingCards(bubbles: BubbleMsg[]): PendingCards {
  let confirm: BubbleConfirm | undefined
  const questions: BubbleQuestion[] = []
  for (const b of bubbles) {
    if (b.confirm && !b.confirm.resolved) confirm = b.confirm
    for (const q of b.questions || []) if (!q.resolved) questions.push(q)
  }
  return { confirm, questions }
}

