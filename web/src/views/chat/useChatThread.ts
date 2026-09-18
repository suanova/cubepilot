// One conversation's lifecycle: its key, its bubbles, its SSE turn, its HITL
// cards.
//
// The hook is the conversation, not the page. `ChatView` renders it beside a
// session list and the floating widget renders it alone, and neither of them
// owns a line of what happens below -- which is the point: the SSE turn and
// the parked-card races are the parts it would be worst to have two of.
import { useCallback, useEffect, useRef, useState } from 'react'
import { api } from '@/api'
import { ApiError, getCurrentUser } from '@/api/client'
import { streamSSE } from '@/api/sse'
import type { HistoryContentBlock, HistoryMessage, PendingApproval, QuestionItem, SSEEvent } from '@/api/types'
import { showToast } from '@/stores/toast'
import {
  answerFor,
  attachToolResult,
  newBubbleQuestion,
  setPhase,
  settleBubbleCards,
  toolArgsDisplay,
  type BubbleConfirm,
  type BubbleMsg,
  type BubbleQuestion,
  type StopEvidence,
} from './model'

export interface ChatThreadApi {
  sessionId: string | null
  bubbles: BubbleMsg[]
  loadingHistory: boolean
  streaming: boolean
  noStreamTurn: boolean
  runningElsewhere: boolean
  // The turn-status check itself failed. Kept apart from `runningElsewhere` so
  // the header never claims a turn nobody confirmed.
  turnCheckFailed: boolean
  stoppingElsewhere: boolean
  allowAlwaysOk: boolean
  threadEl: React.RefObject<HTMLDivElement | null>
  inputEl: React.RefObject<HTMLTextAreaElement | null>
  autoGrow(): void
  switchSession(id: string): Promise<void>
  newChat(): void
  refresh(): void
  sendMessage(): Promise<void>
  stopTurn(): Promise<boolean>
  stopElsewhere(): Promise<void>
  retryTurnCheck(): void
  dismissTurnCheck(): void
  decide(confirm: BubbleConfirm, decision: 'approve' | 'reject' | 'allow-always'): Promise<void>
  pick(q: BubbleQuestion, item: QuestionItem, label: string): void
  typeAnswer(q: BubbleQuestion, item: QuestionItem, text: string): void
  submitQuestion(q: BubbleQuestion): Promise<void>
  dismissQuestion(q: BubbleQuestion): Promise<void>
}

// How often a view holding no stream for the session's turn asks whether that
// turn is still running. Asking is the only way it can learn the turn ended, and
// the turning point is exactly when the reply it is missing becomes available in
// the history -- so the interval is the delay on the tail of an answer the user
// is already reading. Two seconds: quick enough that the rest of the reply lands
// while they are still looking at it, slow enough that a turn running for
// minutes does not become a request storm.
const noStreamTurnPollInterval = 2000

export function useChatThread({
  initialSessionKey,
  onSessionStarted,
}: {
  // The conversation to open with. Omitted by the Chat view, which starts on a
  // new conversation and learns the key the server mints from `message_start`;
  // supplied by the widget, which is bound to the same one every time it opens.
  initialSessionKey?: string
  onSessionStarted: () => void
}): ChatThreadApi {

  const [currentSessionId, setCurrentSessionId] = useState<string | null>(initialSessionKey ?? null)
  const [bubbles, setBubbles] = useState<BubbleMsg[]>([])
  const [loadingHistory, setLoadingHistory] = useState(false)
  const [streaming, setStreaming] = useState(false)
  // A turn running without a stream of this view's own: it was started in
  // another tab, or this view was reloaded while the agent kept working. It can
  // still be stopped; its output arrives on the next history refresh, because
  // stream re-attach is out of scope.
  const [runningElsewhere, setRunningElsewhere] = useState(false)
  // The turn-status check itself failed. It is kept apart from
  // `runningElsewhere` so the header never claims a turn nobody confirmed, and
  // it is still shown: the API answers 502 when it cannot determine, and
  // "cannot tell" is not "idle" -- hiding Stop here would strand exactly the
  // user whose turn is running.
  const [turnCheckFailed, setTurnCheckFailed] = useState(false)
  // The session whose composer Stop is waiting on `/abort`, or null. The
  // server answers only once the turn has settled -- seconds -- so the header
  // reads "Stopping…" and the composer's Send is disabled for the whole window.
  // It is also what refuses a send in that window, because Enter reaches
  // `sendMessage` past the disabled button.
  //
  // A session rather than a bool: `stoppingRef` already says an abort is in
  // flight for *some* turn, but a stop the user has navigated away from must
  // neither label nor block the session they moved to.
  const [stoppingSession, setStoppingSession] = useState<string | null>(null)
  // Only Allowlist policy honors a durable "always allow" grant; under
  // AlwaysAsk everything asks and under None nothing does (issue #116).
  const [allowAlwaysOk, setAllowAlwaysOk] = useState(false)
  const threadEl = useRef<HTMLDivElement | null>(null)
  const inputEl = useRef<HTMLTextAreaElement | null>(null)
  const bubblesRef = useRef<BubbleMsg[]>([])
  // activeSessionRef tracks the session the user is currently looking at, so a
  // slow history/pending request that resolves after a switch is discarded
  // instead of painting another session's confirmations onto this one.
  const activeSessionRef = useRef<string | null>(null)
  // Monotonic stream generation: a stream opened for an earlier session (or
  // before a session switch) must not write into the current view.
  //
  // The session id cannot be the guard. A brand-new chat has no id at send
  // time -- the server mints one and reports it in message_start -- so guarding
  // on the id would invalidate a just-started stream as soon as its own first
  // event arrived, and nothing would ever render.
  const streamGenRef = useRef(0)
  const abortRef = useRef<AbortController | null>(null)
  // The re-attach stream this view holds, if any (issue #167): one at a time,
  // and retired by dropStream like the turn stream it observes.
  const attachRef = useRef<AbortController | null>(null)
  // One submission at a time. The guard matters for a redirect: the textarea
  // keeps the typed text for the whole abort round trip, so without it a second
  // Enter in that window passes `if (!text) return` and `if (streaming)` both
  // and starts a second send -- two POSTs for one redirect, the loser refused by
  // the server's one-stream-per-session guard, and the winner superseded before
  // its answer arrives. The same window opens when the settling old stream
  // clears `streaming` mid-redirect and the composer offers Send again.
  const sendingRef = useRef(false)
  // One abort per turn. The Stop control stays live for the whole redirect
  // window (the composer still renders Stop while the send waits on the abort),
  // and a click there would issue a second POST /abort for a run that is
  // already settling -- a redundant request at best, and a spurious error toast
  // if the gateway rejects the second one. Held across the request and released
  // on every exit path: a missed release would leave Stop permanently inert.
  const stoppingRef = useRef(false)
  // Sessions whose newest turn this tab stopped, and what this tab knows that
  // lets a history render recognise that turn's row (see StopEvidence). A stop
  // this view issued is the one stopped state the server cannot be asked about
  // later -- the design's v1 decision records that a plain reload loses it -- so
  // it is remembered here and applied whenever that session's history is
  // rendered, which is the refresh the stop itself triggers. It is dropped as
  // soon as this view sends a new message for the session, because the stopped
  // turn is not the newest one any more then. Never persisted: a real reload has
  // no local memory left, and the design declined a server-side marker.
  const stoppedTurnsRef = useRef<Map<string, StopEvidence>>(new Map())

  // Keep a mutable mirror of bubbles so SSE callbacks can mutate the latest
  // assistant bubble without stale-closure problems.
  bubblesRef.current = bubbles

  // Tick every second so running bubbles show live "waiting Ns" in pauses.
  const [, setNow] = useState(Date.now())
  useEffect(() => {
    const ticker = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(ticker)
  }, [])

  // Open the conversation a caller named. The Chat view never names one (it
  // starts on a new conversation), so this is the widget's path: it is bound to
  // one conversation, and it has to be showing that conversation's history --
  // and know whether a turn is still running in it -- before the user says
  // anything. Same capture-then-re-check as `switchSession`: the answer is only
  // applied while this view still holds the generation it asked under.
  useEffect(() => {
    if (!initialSessionKey) return
    const gen = streamGenRef.current
    activeSessionRef.current = initialSessionKey
    void loadHistory(initialSessionKey)
    if (streamGenRef.current !== gen) return
    void checkTurnElsewhere(initialSessionKey, gen)
    // Runs once, for the key the caller opened with: re-running it on a later
    // render would reload history over a turn the hook is already streaming.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // A turn this view holds no stream for is only ever observed by asking, and
  // its output arrives on the next history refresh -- this is what performs that
  // refresh. Without it nothing here learns the turn ended: the reply stays as
  // truncated as the history snapshot the view loaded, and the header keeps
  // claiming a turn that is already over, until the user switches sessions or
  // reloads. A stream that died mid-turn lands in exactly this state, which is
  // what makes that half-answer permanent.
  //
  // The poll ends with the state it is keyed on: the refresh below calls
  // clearTurnElsewhere, and a session switch or a new chat bumps the generation
  // the in-flight answer is checked against.
  useEffect(() => {
    if (!runningElsewhere) return
    const id = currentSessionId
    if (!id) return
    const gen = streamGenRef.current
    let cancelled = false
    const timer = setInterval(() => {
      void (async () => {
        let active: boolean
        try {
          ;({ active } = await api.sessionTurn(id))
        } catch {
          // "Cannot tell" is not "finished", so keep asking -- the same reading
          // the header's own check refuses to collapse into "idle".
          return
        }
        if (cancelled || streamGenRef.current !== gen) return
        if (active) return
        clearTurnElsewhere()
        // keepVisible: the conversation on screen is being completed, not
        // replaced, and blanking it to refill with the same thread plus a tail
        // is a flash the user did not ask for.
        void loadHistory(id, true)
      })()
    }, noStreamTurnPollInterval)
    return () => {
      cancelled = true
      clearInterval(timer)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [runningElsewhere, currentSessionId])

  // A turn this view holds no stream for -- confirmed, or uncheckable -- is the
  // session's live state: the composer's Stop is the direct way to end it, and a
  // send while it is up is a redirect, which `sendMessage` runs through its own
  // stop-then-send sequence.
  const noStreamTurn = (runningElsewhere || turnCheckFailed) && !streaming
  // ...and that Stop is waiting on the server for the session on screen. The
  // turn is then still running, so a send must not go out yet: it would be
  // POSTed against a session whose turn is still settling, and retiring this
  // state would take away that turn's only Stop control. Both the controls and
  // the refusal in `sendMessage` read this.
  const stoppingElsewhere = noStreamTurn && stoppingSession === currentSessionId


  function scrollThread() {
    const el = threadEl.current
    if (el) el.scrollTop = el.scrollHeight
  }

  function autoGrow() {
    const el = inputEl.current
    if (!el) return
    el.style.height = 'auto'
    el.style.height = Math.min(el.scrollHeight, 120) + 'px'
  }


  // keepVisible holds the current thread on screen until the reload replaces
  // it. A switch must not (the old session's messages under a new session's
  // header would be a lie), but a refresh of the session already on screen must
  // -- blanking and refilling is a visible flash on every reopen, and the
  // content is about to be the same conversation.
  async function loadHistory(id: string, keepVisible = false, recover: { attach?: boolean } = {}) {
    // The generation this read belongs to, checked again once it answers. The
    // check cannot be left to the caller: the response is applied here, and a
    // session switch during the request has to win -- otherwise a slower answer
    // for the session the user left paints its thread under the header of the
    // one they moved to. The attach's catch-up made that reachable with no user
    // action at all, but the race is the same one a switch already had.
    const gen = streamGenRef.current
    setLoadingHistory(true)
    if (!keepVisible) setBubbles([])
    try {
      const items = await api.sessionHistory(id)
      if (streamGenRef.current !== gen) return
      renderHistory(items, id)
      void recoverPending(id, recover.attach ?? true)
    } catch (e) {
      if (streamGenRef.current !== gen) return
      // A 404 is "this conversation has not started", which is not a failure:
      // every conversation is in that state until its first message reaches the
      // server. Painting it as one would make a brand-new conversation look
      // like an erased one -- and the distinction only works if a genuinely
      // unreachable runtime keeps looking like the failure it is.
      if (e instanceof ApiError && e.status === 404) {
        setBubbles([])
      } else {
        setBubbles([{ kind: 'assistant', text: 'History load failed: ' + String(e), items: [], thinking: false }])
      }
    } finally {
      // Only while this read is still the view's: a superseding load owns the
      // flag, and its own finally is the one that clears it.
      if (streamGenRef.current === gen) {
        setLoadingHistory(false)
        requestAnimationFrame(scrollThread)
      }
    }
  }

  // checkTurnElsewhere asks the server whether the session still has a turn in
  // flight -- the only signal that survives a reload, since this view has no
  // stream to consult.
  //
  // Best-effort in what a failure costs (nothing: history has already loaded
  // and is never blocked by this), never in how one is read. An error here
  // means the server could not determine the answer, so it is reported as
  // exactly that rather than collapsed into "not running".
  //
  // `gen` is the generation the caller captured *before* its own await, and the
  // answer is applied only while the view still holds it -- the same
  // capture-then-re-check the redirect continuation uses. A session switch or a
  // new chat in the window runs dropStream(), and an answer for the session the
  // user left must not paint a status onto the view they moved to.
  //
  // `onIdle` is the caller's follow-up for a turn the server reports as over --
  // the attach's catch-up is the one caller that needs it. It runs only for an
  // answer this view still holds the generation for, so it can never be the
  // session the user left.
  async function checkTurnElsewhere(id: string, gen: number, onIdle?: () => void) {
    try {
      const { active } = await api.sessionTurn(id)
      if (streamGenRef.current !== gen) return
      setRunningElsewhere(!!active)
      setTurnCheckFailed(false)
      if (!active) onIdle?.()
    } catch {
      // Never folded into "not running": the API answers 502 exactly when it
      // could not determine whether the turn is still going.
      if (streamGenRef.current !== gen) return
      setRunningElsewhere(false)
      setTurnCheckFailed(true)
    }
  }

  // retryTurnCheck re-asks after a failed check. Without it a "could not check"
  // status would have no way back to a definite answer short of leaving the
  // session, which is a poor trade for one cheap GET. It is withdrawn while the
  // retry is in flight and returns if the check fails again.
  function retryTurnCheck() {
    if (!currentSessionId) return
    setTurnCheckFailed(false)
    void checkTurnElsewhere(currentSessionId, streamGenRef.current)
  }

  // dismissTurnCheck withdraws the "could not check" status on request. Retry is
  // the way back to an answer, but it is not a way *out*: for a channel this
  // process cannot use, every retry fails the same way, and a reload fails the
  // check again, so the status would sit in the header of a conversation that is
  // otherwise perfectly usable, with no control that removes it. Dismissing claims
  // nothing -- the next reload, session switch or Retry asks again, and a send is
  // unaffected -- it only stops the alarm from being permanent.
  function dismissTurnCheck() {
    clearTurnElsewhere()
  }

  // syncAllowAlways refreshes whether a durable "Always allow" is meaningful
  // for the effective confirmation policy (Allowlist only). Called when a
  // confirmation card appears.
  function syncAllowAlways() {
    api
      .agentApproval()
      .then((v) => setAllowAlwaysOk(!!v.exists && v.approvalPolicy === 'Allowlist'))
      .catch(() => setAllowAlwaysOk(false))
  }

  // After a reload mid-approval the platform still holds the pending write; this
  // restores its confirmation card from the pending endpoint (issue #20). The
  // result is discarded if the user switched sessions while it was in flight.
  //
  // `attach` is what it does with a restored card: draw it and open the stream its
  // answer's output comes back on (the default), or draw it and open nothing. A
  // reload that follows an attach which already failed passes false -- the card is
  // still the user's only control, but re-attaching from there is the one thing
  // that can make a catch-up feed itself: the reload redraws the card, the attach
  // is refused again for the same reason, and the follow-up asks for another
  // reload. Nothing here can observe that run any better on the second try.
  async function recoverPending(id: string, attach = true) {
    // The card a restored question or approval is drawn on: it is also what the
    // re-attach stream renders the parked turn's continuation into (issue #167),
    // so answering in this tab shows the output here.
    let attachBubble: BubbleMsg | null = null
    // Questions first: a parked ask_user turn is the more recent state, and a
    // session can hold several open questions (one card per record).
    try {
      const pending = await api.pendingQuestions(id)
      if (activeSessionRef.current !== id) return // stale: a different session is now active
      if (pending.length > 0) {
        const { bubble, existed } = parkedBubble()
        for (const p of pending) {
          bubble.items.push({ kind: 'question', question: newBubbleQuestion(id, p.id, p.questions, p.timeoutSeconds) })
        }
        setBubbles(existed ? [...bubblesRef.current] : [...bubblesRef.current, bubble])
        requestAnimationFrame(scrollThread)
        attachBubble = bubble
      }
    } catch (e) {
      // 404 is the ordinary "nothing pending for this session" answer. Anything
      // else (a gateway failure, a dropped channel) means we could not tell: a
      // parked turn would then look like an idle one, with no card and no way
      // to send another message, so say so instead of staying silent.
      if (!(e instanceof ApiError && e.status === 404) && activeSessionRef.current === id) {
        setBubbles((prev) => [
          ...prev,
          {
            kind: 'assistant' as const,
            items: [],
            thinking: false,
            phase: 'done' as const,
            error: `Could not check for a pending question: ${String(e)}`,
          },
        ])
      }
    }
    let p: PendingApproval
    try {
      p = await api.pendingApproval(id)
    } catch {
      // No pending approval for this session; a restored question still needs the
      // stream its answer's output comes back on. The card was appended before
      // this lookup went out, so this path can land after a switch, with
      // dropStream already run -- attaching then would open an invisible stream
      // for the session the user left, holding its one stream against every
      // legitimate client until the next switch or the server's cap. Same guard
      // as the success path below.
      if (attach && attachBubble && activeSessionRef.current === id) void attachTurn(id, attachBubble)
      return
    }
    if (activeSessionRef.current !== id) return // stale: a different session is now active
    const { bubble: confirmBubble, existed } = parkedBubble()
    confirmBubble.items.push({
      kind: 'approval',
      confirm: {
        sessionId: p.sessionId,
        approvalId: p.approvalId,
        command: p.command,
        level: p.level,
        message: p.message,
      },
    })
    setBubbles(existed ? [...bubblesRef.current] : [...bubblesRef.current, confirmBubble])
    syncAllowAlways()
    requestAnimationFrame(scrollThread)
    // An approval parks the run just like a question, so the same stream carries
    // its continuation. The question card wins when both exist: it sits above the
    // approval, so the resumed output reads as its answer.
    if (attach) void attachTurn(id, attachBubble || confirmBubble)
  }

  // parkedBubble is the bubble a restored card belongs to: the turn the parked
  // run is on. A parked run is the newest thing in its session -- the session is
  // blocked on it, so nothing was produced after it -- which makes the last
  // assistant bubble the parked turn, and the card lands where the question or
  // the write actually happened instead of in a bubble of its own at the end.
  // A transcript with no assistant turn yet (nothing of the parked run has
  // settled, which is what a run parked on its first tool call looks like) gets
  // a bubble of its own to carry the card.
  function parkedBubble(): { bubble: BubbleMsg; existed: boolean } {
    const list = bubblesRef.current
    const last = list[list.length - 1]
    if (last && last.kind === 'assistant') return { bubble: last, existed: true }
    return { bubble: { kind: 'assistant', items: [], thinking: false, phase: 'done' }, existed: false }
  }

  // The gateway serves a history message's content either as a plain string
  // (user role) or as an array of text/toolCall blocks (assistant / toolResult).
  // Normalize to blocks so a single render path handles both shapes (issue
  // #104: a user prompt carried as a string used to be iterated character by
  // character and silently dropped, leaving only the agent side visible).
  const blocks = (c: HistoryMessage['content']): HistoryContentBlock[] =>
    typeof c === 'string' ? [{ type: 'text', text: c }] : c

  // markStoppedTurn records that this tab stopped the session's newest turn,
  // together with the evidence that identifies that turn's row. It is what lets
  // renderHistory show that turn as stopped on the refresh the stop triggers:
  // the server has no field to ask for, and the stopped flag on the wire only
  // ever reaches a client still attached to the stream. `null` evidence is
  // knowledge too -- a turn stopped before it produced any text leaves no row
  // behind -- and drops the record rather than keeping the previous one, so no
  // stale evidence can relabel some other turn's answer.
  function markStoppedTurn(session: string | null, evidence: StopEvidence | null) {
    if (!session) return
    if (!evidence) {
      stoppedTurnsRef.current.delete(session)
      return
    }
    stoppedTurnsRef.current.set(session, evidence)
  }

  // transcriptShape is the part of a rendered list that identifies its turns:
  // the prompts in order, and the newest assistant text. A list that carries a
  // prompt this shape does not have belongs to a newer turn, and a list whose
  // newest assistant text has not moved carries nothing the stop persisted.
  function transcriptShape(list: BubbleMsg[]): { users: string[]; lastText: string } {
    const users: string[] = []
    let lastText = ''
    for (const b of list) {
      if (b.kind === 'user') users.push(b.text || '')
      else if (b.text) lastText = b.text
    }
    return { users, lastText }
  }

  // stopEvidence captures what this view knows about the turn it is about to
  // stop, before the round trip. A stream of this view's own identifies the turn
  // outright: its bubble is the newest assistant bubble and the text it has
  // produced is the partial the abort will persist -- and a turn stopped before
  // it produced any text persists no row at all (the gateway captures an aborted
  // partial only when the run's text buffer has content), so there is nothing to
  // mark and no evidence to record. With no stream of this view's own, the only
  // thing this view can recognise the row by later is the transcript it has
  // already rendered.
  function stopEvidence(): StopEvidence | null {
    const list = bubblesRef.current
    if (!streaming) return transcriptShape(list)
    const last = list[list.length - 1]
    const text = last && last.kind === 'assistant' ? last.text : undefined
    return text ? { text } : null
  }

  // applyStoppedTurn marks a freshly rendered bubble list as stopped, when this
  // tab stopped a turn and the list is consistent with what that stop left
  // behind. It is applied to the history a reload or a session switch renders --
  // the two places a stopped turn would otherwise come back as a finished
  // answer. A turn that ends while a stream is attached needs none of this:
  // message_done{stopped} carries the marker itself.
  //
  // Nothing is ever marked on the strength of position alone: see StopEvidence
  // for why "the newest assistant bubble" is a different turn's answer whenever
  // the stopped turn persisted no text.
  function applyStoppedTurn(list: BubbleMsg[], session: string | null): BubbleMsg[] {
    const evidence = session ? stoppedTurnsRef.current.get(session) : undefined
    if (!evidence) return list
    if ('text' in evidence) {
      // The stopped turn's own row, by the exact partial this view watched it
      // produce. Searched from the end so the newest match wins when the same
      // partial was produced twice, and a row that does not carry that text is
      // never touched: another turn's answer -- a newer one any tab started
      // included -- cannot be relabelled by this.
      for (let i = list.length - 1; i >= 0; i--) {
        if (list[i].kind === 'assistant' && list[i].text === evidence.text) {
          list[i].stopped = true
          break
        }
      }
      return list
    }
    // With no stream of this view's own there is no partial to recognise, so the
    // marker may only fall on the newest bubble, and only when the reload is the
    // transcript this view had rendered *plus* assistant content it had not
    // seen. A prompt the shape does not have means a newer turn -- possibly
    // another tab's -- owns that bubble; an unchanged newest text means this
    // stop left no row here, i.e. it stopped a turn that had produced nothing.
    // Either way the bubble on screen belongs to another turn, and relabelling
    // it is the bug this evidence exists to prevent.
    const last = list[list.length - 1]
    if (!last || last.kind !== 'assistant') return list
    const shape = transcriptShape(list)
    if (shape.users.length !== evidence.users.length || shape.users.some((u, i) => u !== evidence.users[i])) return list
    if (shape.lastText === evidence.lastText) return list
    last.stopped = true
    return list
  }

  function renderHistory(items: HistoryMessage[], session: string) {
    const out: BubbleMsg[] = []
    let last: BubbleMsg | null = null
    // The narration block ids this rebuild hands out. They are only ever
    // compared with each other, within this render -- the live path numbers its
    // own -- so a counter is enough to keep "same block" meaning the same thing
    // here.
    let narrationBlocks = 0
    const openAssistant = () => {
      if (last && last.kind === 'assistant') return
      last = { kind: 'assistant', items: [], thinking: false, phase: 'done' }
      out.push(last)
    }
    for (const it of items) {
      const content = blocks(it.content)
      if (it.role === 'user') {
        for (const c of content) {
          if (c.type === 'text' && c.text) {
            out.push({ kind: 'user', text: c.text, items: [], thinking: false })
          }
        }
        last = null
        continue
      }
      if (it.role === 'assistant') {
        const hasTool = content.some((c) => c.type === 'toolCall')
        const hasText = content.some((c) => c.type === 'text' && c.text)
        if (!hasTool && !hasText) continue
        openAssistant()
        // A step, not the answer -- and the gateway says which is which: it
        // splits a mixed row before serving history, giving the commentary its
        // own row marked `openclawStreamFallback`, and leaving the rest as the
        // tool calls plus whatever the answer was. So an unmarked row's text is
        // the answer, never a step: a row like `[toolCall, {text: "Done."}]` is
        // a tool call and a finished answer, and reading it as narration both
        // invents a step and eats the answer.
        const fallback = it.openclawStreamFallback
        const marked = typeof fallback?.itemId === 'string' && fallback.itemId !== ''
        if (marked) {
          const stepText = fallback?.replacementText || content
            .map((c) => (c.type === 'text' && c.text ? c.text : ''))
            .join('')
          if (stepText.trim()) {
            last!.items.push({ kind: 'narration', blockId: `h${++narrationBlocks}`, text: stepText })
          }
        }
        for (const c of content) {
          if (c.type === 'toolCall') {
            last!.items.push({
              kind: 'tool',
              tool: { name: c.name || 'exec', cmd: toolArgsDisplay(c.arguments), callID: c.id || '', done: true },
            })
          } else if (c.type === 'text' && c.text && !marked) {
            // The reply, and only ever one per turn: the newest one wins.
            last!.text = c.text
          }
        }
        continue
      }
      // toolResult: pair the output with the tool call that produced it, so the
      // result renders directly under its command card.
      if (it.role === 'toolResult') {
        openAssistant()
        for (const c of content) {
          if (c.type === 'text' && c.text) attachToolResult(last!.items, '', c.text)
        }
      }
    }
    const next = applyStoppedTurn(out, session)
    // Mirrored into the ref before the state update lands, because the recovery
    // that runs straight after a reload reads this view's bubbles to find the
    // turn a restored card belongs to -- and it is not a render, so it would
    // otherwise read the list this reload just replaced.
    bubblesRef.current = next
    setBubbles(next)
  }

  // dropStream retires the live stream: every event still in flight for it is
  // superseded (its generation no longer matches) and its fetch is cancelled.
  // The streaming flag is cleared here rather than left to the retiring
  // stream's own `finally`, which sees itself as stale and refuses to touch it.
  //
  // The no-stream turn status is retired with it -- both describe a turn this
  // view is leaving behind -- and the generation bump discards a turn-status
  // check still in flight for it, whose snapshot could otherwise re-arm a
  // status that has just been retired.
  function dropStream() {
    streamGenRef.current++
    abortRef.current?.abort()
    // The re-attach stream belongs to the same turn this view is leaving behind:
    // a visit to the session restores its card and attaches again (issue #167).
    attachRef.current?.abort()
    setStreaming(false)
    clearTurnElsewhere()
  }

  // clearTurnElsewhere retires the no-stream turn state, both parts at once
  // so they can never disagree. The paths that leave a session's turn behind --
  // a switch, a new chat, a stop -- all go through dropStream; a send clears it
  // alongside its own generation bump; and the check's own answer is the third.
  function clearTurnElsewhere() {
    setRunningElsewhere(false)
    setTurnCheckFailed(false)
  }

  const switchSession = useCallback(async (id: string) => {
    dropStream()
    // Captured with the session, not after the history load: by then another
    // switch (into a session whose history loaded faster) has already taken the
    // view, and a check issued then would be asking about a session this view
    // no longer shows.
    const gen = streamGenRef.current
    activeSessionRef.current = id
    setCurrentSessionId(id)
    await loadHistory(id)
    if (streamGenRef.current !== gen) return
    void checkTurnElsewhere(id, gen)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // refresh re-reads the conversation from the server. The widget calls it when
  // it is reopened: it shares its conversation with the Chat view, so anything
  // sent from there is not something this view can know about on its own, and a
  // panel that reopened onto a stale thread would be quietly wrong.
  //
  // Skipped while this view is streaming -- the live thread is ahead of what
  // the server would return -- and skipped with no session, which is a
  // conversation that has not started and therefore has nothing to re-read.
  function refresh() {
    if (streaming) return
    const id = currentSessionId
    if (!id) return
    activeSessionRef.current = id
    void loadHistory(id, true)
    // A turn may have started elsewhere while the panel was shut; asking is the
    // only way to find out, and the composer's Stop is then the only control that
    // can end it.
    void checkTurnElsewhere(id, streamGenRef.current)
  }

  function newChat() {
    dropStream()
    activeSessionRef.current = null
    setCurrentSessionId(null)
    setBubbles([])
    setLoadingHistory(false)
    requestAnimationFrame(() => inputEl.current?.focus())
  }

  // stopTurn asks the server to stop the session's running turn. It resolves
  // true once the turn has settled and false when the stop did not take -- the
  // caller must then treat the turn as still running and say so. An ApiError
  // here is an expected outcome, not a crash: 504 means the turn did not settle
  // in time and 502 that the gateway channel was unavailable, so it is surfaced
  // as a toast and the stop control stays available rather than the page being
  // painted as broken.
  async function stopTurn(): Promise<boolean> {
    // No session id yet: the request is still in flight and the server has not
    // reported the id it minted, so there is no turn we can address. This is a
    // refusal, not a success -- a POST with an empty `sessionId` does not
    // conflict with the running turn, the server mints a *new* session for it
    // and the conversation forks, leaving the original turn running invisibly.
    // So the caller keeps the text and the Stop control, exactly as for a stop
    // the server refused.
    if (!currentSessionId) return false
    // An abort is already in flight for this turn: join it by refusing, rather
    // than starting a second request whose outcome nothing is waiting on. The
    // caller treats a refusal as "the turn is still running", which is exactly
    // what is true while the first abort settles.
    if (stoppingRef.current) return false
    const session = currentSessionId
    // Captured before the round trip, and about the session the abort is issued
    // for, not whatever the view shows when it answers.
    const evidence = stopEvidence()
    stoppingRef.current = true
    try {
      await api.abortSession(session)
    } catch (e) {
      // Refused: the turn is still running, so nothing was stopped and any
      // record of an *earlier* stop of this session no longer describes its
      // newest turn. Dropping it here is what keeps a later history render from
      // stamping this turn's row with a stop that never happened.
      markStoppedTurn(session, null)
      showToast(String(e))
      return false
    } finally {
      stoppingRef.current = false
    }
    markStoppedTurn(session, evidence)
    // The stream closes with message_done{stopped:true}; nothing more to do
    // here -- do NOT abort the local fetch, the server's terminal is cleaner.
    return true
  }

  // stopElsewhere stops a turn this view has no stream for. `/abort` does not
  // answer until the turn has settled, so the history reload that follows it
  // already contains the stopped turn's partial output.
  //
  // It shares `stoppingRef` with the streaming Stop: both issue the same
  // gateway abort. The composer's button is still on screen for the whole round
  // trip -- it is only withdrawn once the server has answered -- and it is
  // disabled in the meantime, but `stoppingRef` is what actually refuses a
  // click that arrives before that re-render: a second POST for a turn that is
  // already settling is redundant at best. A stop the server refuses is an
  // expected outcome -- 504 means it did not settle in time, 502 that the
  // channel was unavailable -- so it is surfaced as a toast and the status
  // stays, because the turn is then still running.
  async function stopElsewhere() {
    const session = currentSessionId
    if (!session) return
    if (stoppingRef.current) return
    // The round trip is long -- the server answers only once the turn has
    // settled -- so the user can switch away mid-stop. The generation, captured
    // before the await and re-checked after it, is what stops this from
    // reloading the stopped session's history into the view they moved to.
    const gen = streamGenRef.current
    // With no stream of this view's own, the transcript it has rendered is the
    // only thing that can identify the stopped turn's row once the reload lands.
    const evidence = stopEvidence()
    stoppingRef.current = true
    setStoppingSession(session)
    try {
      await api.abortSession(session)
    } catch (e) {
      // Refused: the turn is still running. The record of an earlier stop of
      // this session must go with it -- the evidence would otherwise outlive
      // the turn it described and mark a later one's row on the next history
      // render (see stopTurn for the same clearing).
      markStoppedTurn(session, null)
      showToast(String(e))
      return
    } finally {
      stoppingRef.current = false
      setStoppingSession(null)
    }
    // The stop landed, whatever the view does next: the session's newest turn is
    // stopped, and only this tab knows it (see stoppedTurnsRef).
    markStoppedTurn(session, evidence)
    if (streamGenRef.current !== gen) return
    // dropStream, not clearTurnElsewhere: the state is done, and the
    // generation bump discards a turn-status check still in flight for it,
    // whose pre-stop snapshot would otherwise re-arm the status the stop just
    // retired.
    dropStream()
    await loadHistory(session)
  }

  // attachTurn observes a turn this view did not start (issue #167). After a
  // reload the question or approval card comes back from the pending endpoint,
  // but the run parked on it is only observable over a stream: the request that
  // carried the turn died with the page, so without this the answer settles the
  // card and then shows nothing until the next reload.
  //
  // The stream the server gives back is the session's one stream, so this is
  // also the reason a tab that merely attached is the tab that renders what the
  // answer produced.
  async function attachTurn(id: string, bubble: BubbleMsg) {
    attachRef.current?.abort()
    const ctl = new AbortController()
    attachRef.current = ctl
    const gen = streamGenRef.current
    // Whether the server's own terminal reached this stream, which is the one
    // thing that separates a stream that observed the run from one that ended
    // without observing it. The helper's *synthesized* terminal says nothing of
    // the sort -- it is a statement about the transport -- and the attach never
    // lets it reach the bubble (see below).
    let observedTerminal = false
    try {
      await streamSSE(
        `/api/v1/sessions/${encodeURIComponent(id)}/stream`,
        // streamSSE fetches directly, so the identity every other request gets
        // from apiFetch has to be passed here explicitly, as the turn stream
        // does. Without it the server resolves the default user -- and a card
        // restored under a selected one is then attached on a gateway that
        // holds nothing for that session, which answers 404.
        { headers: { 'X-CubePilot-User': getCurrentUser() } },
        (_evName, ev) => {
          // An aborted stream is this view walking away, not a failed turn.
          if (ctl.signal.aborted) return
          // A synthesized terminal is the stream helper's own, emitted when the
          // request failed before any response arrived, or when the stream ended
          // without the server's terminal (see streamSSE). It has nothing to say
          // about this bubble: the attach never carried a turn, so there is no
          // turn of this card's to end -- passing it on would mark a card
          // restored from the pending endpoint as `transportLost` over a
          // connection that never opened.
          if (ev.type === 'message_done' && ev.synthetic) return
          if (ev.type === 'message_done') observedTerminal = true
          applyTurnEvent(bubble, ev)
          // An observed turn that really ended answers the header's question:
          // nothing is running any more, so its Stop must not sit there offering
          // to abort a turn that is already over.
          if (ev.type === 'message_done') clearTurnElsewhere()
        },
        ctl.signal,
        // A refusal is not a stream that ended, and none of it may reach the
        // card: the helper's synthesized terminal would stamp a card restored
        // from the pending endpoint as a lost transport. What a refusal means
        // for the turn is the follow-up below, which asks.
        () => {},
      )
    } catch {
      /* the request never started; the card stays as it is */
    }
    if (observedTerminal || ctl.signal.aborted) return
    // Nothing here ever observed the run. That covers a stream that ended without
    // the server's terminal, and every refusal -- the 404 of a card answered
    // before the request arrived, the 409 of another tab holding the session's
    // stream, a failure that left no stream at all. None of them says the run is
    // over, so ask: a run still going is the no-stream turn status's job, and
    // that status polls for the moment its output lands (see the effect above),
    // while a run already done has output in the transcript that only a reload
    // brings back. The reload draws a still-pending card but attaches nothing --
    // an attach just failed here, and re-attaching from a reload is what would
    // let this catch-up feed itself (see recoverPending).
    void checkTurnElsewhere(id, gen, () => void loadHistory(id, true, { attach: false }))
  }

  // applyTurnEvent folds one SSE event into the bubble it belongs to. It is the
  // one render path for a turn's output, shared by the stream this tab started
  // (/api/v1/messages) and one it re-attached to (/api/v1/sessions/{key}/stream,
  // issue #167), so a turn this tab merely observes renders like one it drove.
  //
  // Two things are deliberately not here, because they belong to the stream
  // rather than to the bubble: `message_start` (which session the stream turned
  // out to be for) and the follow-up a *synthetic* terminal needs (asking the
  // server whether a run was left behind). Both callers intercept those first.
  function applyTurnEvent(bubble: BubbleMsg, ev: SSEEvent) {
    if (ev.type === 'agent_thinking') {
      setPhase(bubble, 'thinking')
      return
    }
    if (ev.type === 'narration') {
      // The agent's between-tool narration (issue #216). The text is the block's
      // whole snapshot, not an increment, so the same blockId replaces what this
      // view holds and a new one starts a new block. The block belongs where it
      // arrived: between the cards it introduces. A blank snapshot is not a
      // paragraph, but nothing else about the text is this view's to change --
      // trimming it would rewrite what the gateway chose to say.
      const text = ev.text || ''
      if (!text.trim()) return
      setPhase(bubble, 'thinking')
      const previous = bubble.items[bubble.items.length - 1]
      if (previous && previous.kind === 'narration' && previous.blockId === ev.blockId) {
        previous.text = text
      } else {
        bubble.items.push({ kind: 'narration', blockId: ev.blockId || '', text })
      }
      return
    }
    if (ev.type === 'tool_call') {
      setPhase(bubble, 'tools')
      bubble.items.push({
        kind: 'tool',
        tool: { name: ev.name, cmd: toolArgsDisplay(ev.arguments), callID: ev.callId || '', done: false },
      })
      return
    }
    if (ev.type === 'tool_result') {
      setPhase(bubble, 'tools')
      // Attach unconditionally (an empty output is still a result): the
      // call must be marked done even when the tool returned nothing.
      attachToolResult(bubble.items, ev.callId || '', ev.output || '')
      return
    }
    if (ev.type === 'approval_pending') {
      // A write is parked awaiting the human (issue #20). Show the card
      // immediately rather than waiting for the next 1s ticker render. The card
      // itself is drawn in the composer dock while it is pending; the item holds
      // its place in the turn, so the record of the decision lands beside the
      // call it gated.
      setPhase(bubble, 'tools')
      bubble.items.push({
        kind: 'approval',
        confirm: {
          sessionId: ev.sessionId || currentSessionId || '',
          approvalId: ev.callId || '',
          command: ev.command || '',
          level: ev.level || 'write',
          message: ev.message,
        },
      })
      syncAllowAlways()
      setBubbles([...bubblesRef.current])
      requestAnimationFrame(scrollThread)
      return
    }
    if (ev.type === 'approval_resolved') {
      const item = bubble.items.find(
        (i) => i.kind === 'approval' && (!ev.callId || i.confirm.approvalId === ev.callId),
      )
      if (item && item.kind === 'approval') {
        item.confirm.resolved = true
        // `approved` is a *bool on the wire: absent means nobody decided
        // this -- the turn was stopped while the write was parked -- and
        // that is not the same as the explicit false of a rejection.
        // Copying it only when present leaves the card reading "Stopped"
        // instead of painting the user a red "Rejected" they never chose.
        if (ev.approved !== undefined) item.confirm.approved = ev.approved
        item.confirm.busy = false
        setBubbles([...bubblesRef.current])
      }
      return
    }
    if (ev.type === 'question_pending') {
      // The agent's ask_user tool is parked on a human answer (issue
      // #161). Show the question card immediately; its tool card is
      // suppressed server-side so this is the only surface for it.
      if (ev.question && ev.question.questions?.length) {
        setPhase(bubble, 'tools')
        bubble.items.push({
          kind: 'question',
          question: newBubbleQuestion(
            ev.sessionId || currentSessionId || '',
            ev.callId || '',
            ev.question.questions,
            ev.question.timeoutSeconds,
          ),
        })
        setBubbles([...bubblesRef.current])
        requestAnimationFrame(scrollThread)
      }
      return
    }
    if (ev.type === 'question_resolved') {
      // Settle only the matching card: another question of this turn may
      // still be open.
      const item = bubble.items.find(
        (i) => i.kind === 'question' && (!ev.callId || i.question.questionId === ev.callId),
      )
      if (item && item.kind === 'question') {
        item.question.resolved = true
        item.question.outcome = ev.message || 'answered'
        item.question.busy = false
        setBubbles([...bubblesRef.current])
      }
      return
    }
    if (ev.type === 'message_delta') {
      setPhase(bubble, 'streaming')
      bubble.text = (bubble.text || '') + (ev.delta || '')
      return
    }
    if (ev.type === 'text_replace') {
      // Snapshot superseding earlier text (e.g. commentary rewritten after
      // a tool ran): replace, never append (issue #130). The text it
      // supersedes is kept rather than dropped -- it is what the user was
      // reading when the rewrite landed, and a rewrite is not a reason to
      // take it away from them (issue #204). Only a rewrite that would
      // change nothing is discarded, so a repeated snapshot cannot pile up
      // copies of the same text.
      setPhase(bubble, 'streaming')
      const next = ev.delta || ''
      if (bubble.text && bubble.text !== next) {
        bubble.superseded = [...(bubble.superseded || []), bubble.text]
      }
      bubble.text = next
      return
    }
    if (ev.type === 'message_done') {
      // A synthesized terminal is a transport failure, not a turn
      // outcome: the stream died (or never opened) before the server's
      // own terminal arrived. The gateway run may still be executing, and
      // its approval or question may still be live -- so nothing here may
      // treat the turn as over. No phase freeze (`done` would render
      // in-flight tools as "Stopped" and the bubble as finished), no
      // `stopped`, no `error`, and above all no settleBubbleCards:
      // settling the cards is what removes the only controls that can
      // unblock that run. The partial text stays on the bubble and the
      // cards stay live and answerable.
      if (ev.synthetic) {
        bubble.transportLost = ev.error || 'the stream ended before the turn finished'
        setBubbles([...bubblesRef.current])
        // Nothing else may follow: no phase freeze (`done` would render
        // in-flight tools as "Stopped" and the bubble as finished), no
        // `stopped`, no `error`, and above all no settleBubbleCards --
        // settling the cards is what removes the only controls that can
        // unblock a run that may still be executing. The caller then asks
        // the server whether a run was left behind rather than asserting
        // one (see the streams' own callbacks).
        return
      }
      // Past this point the terminal is the server's own, so the turn
      // really is over.
      setPhase(bubble, 'done')
      // A stopped turn is neither a failure nor a normal completion, so
      // it sets `stopped` and leaves `error` empty.
      if (ev.stopped) bubble.stopped = true
      else if (ev.error) bubble.error = ev.error
      // The turn is over, so any card it left parked is dead -- the server
      // settled those records. Its own *_resolved events are not reliable
      // here (they race the stream's close), so the terminal settles them
      // too; see settleBubbleCards.
      settleBubbleCards(bubble)
      return
    }
  }

  async function sendMessage() {
    const el = inputEl.current
    if (!el) return
    if (sendingRef.current) return
    const text = el.value.trim()
    if (!text) return
    // A composer Stop is in flight for the session on screen, so its turn is
    // still running server-side and its outcome is not known yet. Refusing here
    // keeps this send from racing it: the stop-then-send branch below would find
    // the stop already in flight and abandon the send anyway, retiring the
    // status and, with it, the running turn's only Stop control. The composer's
    // Send is disabled and the header reads "Stopping…" for the same
    // window, so this is not a click swallowed in silence; and it is
    // deliberately not a queue -- the text stays in the box, and Enter again
    // once the stop answers sends it.
    if (stoppingElsewhere) return
    // `streaming` and `noStreamTurn` are the same situation from the send's
    // point of view: a turn is running for the session on screen and this
    // send is the user redirecting it. A send in this state has to take the
    // stop-then-send route too, not just the streaming one. Left on the plain
    // path it gets one of two wrong outcomes, neither of them a refusal the
    // pre-reload stream is still registered server-side, so the POST is refused
    // with a raw 409; or that stream has closed while the run has not, and the
    // gateway's default queueMode steers the text into the running turn -- no
    // stop, no new turn, and the message swallowed, which is the steering
    // behaviour the design declares a non-goal. `/abort` answers only once the
    // session has settled, so the send that follows it can do neither. (A
    // turnCheckFailed state takes the same route: the check failed, so a turn
    // may well be running, and an abort is a no-op when none is.)
    if (streaming || noStreamTurn) {
      // Redirect: stop the running turn first. The server only answers once the
      // turn has settled, so the send below cannot hit the 409 guard. What
      // happens when the stop does not take is decided below, per state: a
      // confirmed turn keeps the text and the Stop button, the un-checkable one
      // sends anyway.
      //
      // The guard is held for the whole stop-then-send sequence -- including the
      // state's history reload -- and released on every exit path: the
      // abandoned redirect below, the throw out of the stop, and the send that
      // follows. It has to span the reload too. The stop settling to the
      // textarea being cleared is where the composer looks most idle: nothing
      // has visibly happened, the text is still in the box and Send is offered
      // again, so pressing Enter once more is the natural reaction. Without the
      // guard that second submission re-enters this whole sequence and issues a
      // *second* `POST /abort` for the session, which is not harmless: by then
      // the redirect's own send has started a new run for the session, that run
      // is the one the server's in-flight read finds, and a stop scoped to it is
      // a stop of the turn the user just asked for.
      //
      // The generation is captured *before* the await and re-checked *after*
      // it, not just before the send. Everything below -- the bubbles, the POST
      // body, the new stream -- is built from this render closure's
      // `currentSessionId`, and the abort round trip is long (up to the
      // server's settle timeout). A session switch or a new chat in that window
      // runs dropStream(), and the continuation resumes as if it were still
      // current, because it takes a fresh generation of its own: it would POST
      // to the session the user left (silently giving it a turn that only shows
      // up on reload) while painting that turn's bubbles into the view they
      // switched to. The re-check is what makes the switch win; a check only
      // before the send cannot see a switch that has not happened yet.
      const genAtSend = streamGenRef.current
      // The Stop control is on screen with nothing to show for the wait, so the
      // round trip is made visible the same way the header's status is: the
      // composer's Send is disabled and the header reads "Stopping…" for the
      // whole sequence, reload included. Held here rather
      // than left to `stopTurn`/`stopElsewhere`, which release it as soon as the
      // abort answers -- before the reload that is the rest of the wait.
      const holdVisibleStop = noStreamTurn && !!currentSessionId
      if (holdVisibleStop) setStoppingSession(currentSessionId)
      sendingRef.current = true
      let stopped = false
      try {
        stopped = await stopTurn()
        if (streamGenRef.current !== genAtSend) return
        if (!stopped) {
          // The stop did not take. For a turn the view or the server has
          // *confirmed* -- a stream of this view's own, or `runningElsewhere` --
          // the send is abandoned, and deliberately: the Stop control is on
          // screen, it is the control that ends that turn, and it is worth
          // another try. Putting the message in flight against a session that is
          // still running is the 409-or-silent-steer outcome the stop-then-send
          // route exists to prevent.
          //
          // The "could not check" state is the exception, and it is the state
          // that would otherwise be a dead end. Nothing there confirmed a turn,
          // and the stop is refused for the same reason the check failed -- a
          // gateway channel this process cannot use -- so an abort from here
          // provably cannot work either. The send is then the only request left
          // that re-dials the channel (it is the turn path that calls `conn`,
          // see the API's PreTurn), and refusing it leaves no in-page recovery
          // at all: the user retries, reloads, and lands on the same status,
          // because the next /turn check fails the same way. So it falls through
          // to the ordinary send below.
          //
          // What that trades away, exactly: if the channel was merely down for
          // the *API* while the gateway run was still alive, the abandoned stop
          // means the follow-up send can be steered into that run and swallowed
          // -- the case the stop-then-send route was built for. It is taken
          // knowingly, and only here: a confirmed turn never falls through, and
          // the alternative is a state whose only exit is "New chat".
          if (!turnCheckFailed) return
        } else if (noStreamTurn) {
          // That turn had no stream in this view, so nothing in it ever
          // carried that turn's stopped marker: the only record of what happened
          // is the history the abort has just persisted. Re-render it -- with
          // `stoppedTurnsRef` marking its own row -- before the new turn's
          // bubbles are appended below, or the turn the user just stopped comes
          // back looking like a finished answer.
          if (!currentSessionId) return
          await loadHistory(currentSessionId)
          if (streamGenRef.current !== genAtSend) return
        }
      } finally {
        sendingRef.current = false
        if (holdVisibleStop) setStoppingSession(null)
      }
    }
    // The newest turn of this session is about to be the one this send starts,
    // so the local stopped marker no longer describes it: a later history render
    // must not mark the new turn stopped.
    if (currentSessionId) stoppedTurnsRef.current.delete(currentSessionId)
    // This view is about to drive its own turn: the no-stream turn status
    // describes the turn being left behind, and would otherwise reappear when
    // the new stream ends.
    clearTurnElsewhere()
    // A functional update, not a spread of bubblesRef.current: the history
    // reload above may have set new bubbles that React has not re-rendered yet,
    // and the ref would still hold the pre-reload list -- dropping exactly the
    // stopped turn that reload exists for.
    const bubble: BubbleMsg = { kind: 'assistant', items: [], thinking: true }
    setBubbles((prev) => [...prev, { kind: 'user' as const, text, items: [], thinking: false }, bubble])
    el.value = ''
    el.style.height = 'auto'
    requestAnimationFrame(scrollThread)

    const gen = ++streamGenRef.current
    const stale = () => streamGenRef.current !== gen
    // The session this stream turned out to be for. A brand-new chat has no id
    // at send time -- the server mints one and reports it in message_start --
    // so the send-time `currentSessionId` cannot name it. Needed by the
    // synthesized terminal below, which carries no sessionId of its own.
    let turnSession = currentSessionId
    const controller = new AbortController()
    abortRef.current = controller
    setStreaming(true)

    try {
      await streamSSE(
        '/api/v1/messages',
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/json', 'X-CubePilot-User': getCurrentUser() },
          body: JSON.stringify({ sessionId: currentSessionId, content: text }),
        },
        (_evName, ev) => {
          if (stale()) {
            // A superseded stream paints nothing into the current view -- with
            // one exception. A redirect leaves the stopped turn's bubble on
            // screen, and the events that close that turn out address that
            // bubble, not a turn the view has moved on from. They are:
            //   - its own `message_done`, else the bubble spins "Running..."
            //     forever;
            //   - the `approval_resolved` / `question_resolved` the abort
            //     publishes alongside it, else its write card keeps live
            //     Approve/Reject buttons that POST to a record the settle
            //     already deleted (and its question card keeps offering an
            //     answer, to the same effect).
            // Those are state transitions on a bubble the user is still looking
            // at, and they happen to be the only way it can leave the "live"
            // rendering. A *synthesized* terminal is the exception to the
            // exception: it is a transport failure, not the superseded turn
            // ending, so it closes nothing (see the handler below) and the
            // session is not re-checked -- this view has left that session, and
            // dropStream already retired its status. Turn *output* is different
            // and stays dropped: deltas, tool calls and fresh pending cards
            // carry content from the superseded turn's own conversation, which
            // is exactly what the user redirected away from. The session-switch
            // path aborts the fetch instead, so an aborted stream emits nothing
            // at all and cannot reach here.
            const settlesSupersededTurn =
              ev.type === 'message_done' || ev.type === 'approval_resolved' || ev.type === 'question_resolved'
            if (!settlesSupersededTurn) return
          }
          if (ev.type === 'message_start') {
            if (ev.sessionId) {
              turnSession = ev.sessionId
              setCurrentSessionId(ev.sessionId)
              onSessionStarted()
            }
            return
          }
          applyTurnEvent(bubble, ev)
          // A synthesized terminal says the transport died, not that the turn
          // did: the run may still be executing server-side with no stream of
          // this view's own, which is what the header describes. Ask the server
          // rather than assert it.
          if (ev.type === 'message_done' && ev.synthetic && !stale() && turnSession) {
            void checkTurnElsewhere(turnSession, streamGenRef.current)
          }
        },
        controller.signal,
      )
    } catch (e) {
      // An intentional abort (unmount, session switch) is a clean stop, not a
      // failure: the stream helper has already returned silently for it, and
      // nothing here should paint the user an error they did not cause.
      if (!stale() && !controller.signal.aborted) {
        setPhase(bubble, 'done')
        bubble.error = String(e)
      }
    } finally {
      // A superseded stream touches nothing: the flags and the bubble list now
      // belong to its successor, or to the session that retired it (dropStream
      // clears the flag precisely because this branch will refuse to).
      if (!stale()) {
        setStreaming(false)
        // Push a new array reference so React re-renders with the mutated
        // bubble.
        setBubbles([...bubblesRef.current])
        requestAnimationFrame(scrollThread)
      }
    }
  }

  // decide sends the human's answer for a pending write confirmation
  // (issue #20 / #116). "allow-always" approves this once and records the
  // command as a learned grant for the user, so it auto-passes from then on;
  // the instance allowlist is not touched. POSTing resolves the gateway
  // approval; the SSE stream then carries the resumed turn.
  async function decide(confirm: BubbleConfirm, decision: 'approve' | 'reject' | 'allow-always') {
    const session = confirm.sessionId || currentSessionId
    if (!session) {
      confirm.error = 'no session'
      return
    }
    confirm.busy = true
    confirm.error = ''
    setBubbles([...bubblesRef.current])
    try {
      const res = await api.postApproval(session, decision)
      confirm.resolved = true
      confirm.approved = decision !== 'reject'
      // The approval itself went through, but recording the durable grant did
      // not, so this command will ask again. Reporting the plain success the
      // user is relying on would be a lie (issue #185).
      if (decision === 'allow-always' && res.allowlisted === false) {
        showToast('Approved, but the command was not added to your allowlist -- it will ask again.')
      }
    } catch (e) {
      // 404 is the card having been settled underneath the click: the turn was
      // stopped and the settle deleted the record, or it expired. The click lost
      // that race, so the card is closed -- neutrally, with no decision recorded
      // -- rather than left offering buttons that cannot work or reporting a
      // failure the user did not cause.
      if (e instanceof ApiError && e.status === 404) {
        confirm.resolved = true
        confirm.approved = undefined
      } else {
        confirm.error = String(e)
      }
    } finally {
      confirm.busy = false
      setBubbles([...bubblesRef.current])
      requestAnimationFrame(scrollThread)
    }
  }

  // pick toggles an option of a question. A multiSelect question keeps every
  // choice; a single-select one replaces it, matching how the gateway reads the
  // answer back (one label per question).
  function pick(q: BubbleQuestion, item: QuestionItem, label: string) {
    const cur = q.picked[item.questionId] || []
    if (item.multiSelect) {
      q.picked[item.questionId] = cur.includes(label) ? cur.filter((l) => l !== label) : [...cur, label]
    } else {
      q.picked[item.questionId] = cur.length === 1 && cur[0] === label ? [] : [label]
    }
    // Choosing an option is the whole answer to that question, so it drops the
    // text typed as the other form of it.
    delete q.free[item.questionId]
    q.error = ''
    setBubbles([...bubblesRef.current])
  }

  // typeAnswer records the human's own answer for a question. It replaces the
  // selection rather than joining it: the card treats them as alternatives. The
  // gateway rejects more than one value on a single-select question and allows
  // mixing on a multiSelect one, but this card applies the same rule to both.
  // Text that is empty once trimmed is no answer at all, so the selection stays.
  function typeAnswer(q: BubbleQuestion, item: QuestionItem, text: string) {
    q.free[item.questionId] = text
    if (text.trim()) delete q.picked[item.questionId]
    q.error = ''
    setBubbles([...bubblesRef.current])
  }

  // submitQuestion sends the human's answer. The gateway's resolve response is
  // authoritative: if it reports the question is already gone (expired between
  // the card being painted and the click) the card is re-synced from the
  // pending endpoint rather than left claiming to be answerable.
  async function submitQuestion(q: BubbleQuestion) {
    const session = q.sessionId || currentSessionId
    if (!session) {
      q.error = 'no session'
      return
    }
    q.busy = true
    q.error = ''
    setBubbles([...bubblesRef.current])
    try {
      // The answer is whatever the card settled on per question: the labels the
      // human picked, or the text they typed instead. The server relays it
      // unchanged -- it does not know the gateway's answer semantics.
      const answers: Record<string, string[]> = {}
      for (const it of q.items) answers[it.questionId] = answerFor(q, it)
      await api.postQuestion(session, q.questionId, answers)
      q.resolved = true
      q.outcome = 'answered'
    } catch (e) {
      await resyncQuestion(q, session, e)
    } finally {
      q.busy = false
      setBubbles([...bubblesRef.current])
      requestAnimationFrame(scrollThread)
    }
  }

  // dismissQuestion cancels the question so the agent continues its turn
  // instead of waiting out its own (long) timeout.
  async function dismissQuestion(q: BubbleQuestion) {
    const session = q.sessionId || currentSessionId
    if (!session) {
      q.error = 'no session'
      return
    }
    q.busy = true
    q.error = ''
    setBubbles([...bubblesRef.current])
    try {
      await api.postQuestionCancel(session, q.questionId)
      q.resolved = true
      q.outcome = 'cancelled'
    } catch (e) {
      await resyncQuestion(q, session, e)
    } finally {
      q.busy = false
      setBubbles([...bubblesRef.current])
      requestAnimationFrame(scrollThread)
    }
  }

  // resyncQuestion reconciles a card whose answer the server refused. A 404/409
  // means the gateway no longer holds the question open, so the card is settled
  // from the pending list instead of guessing at its state.
  async function resyncQuestion(q: BubbleQuestion, session: string, err: unknown) {
    const stale = err instanceof ApiError && (err.status === 404 || err.status === 409)
    if (!stale) {
      q.error = String(err)
      return
    }
    try {
      const pending = await api.pendingQuestions(session)
      const live = pending.find((p) => p.id === q.questionId)
      if (!live) {
        q.resolved = true
        q.outcome = 'expired'
        return
      }
      q.deadline = live.timeoutSeconds ? Date.now() + live.timeoutSeconds * 1000 : undefined
      q.error = 'That answer was not accepted; the question is still open.'
    } catch (e2) {
      // Only a confirmed "gone" settles the card. A transient failure would
      // otherwise hide the controls while the agent is still parked, leaving
      // the user unable to answer until a reload.
      if (e2 instanceof ApiError && e2.status === 404) {
        q.resolved = true
        q.outcome = 'expired'
        return
      }
      q.error = 'Could not refresh the question. Try again.'
    }
  }


  // Unmount is the one teardown the stream cannot outlive: cancel the fetch so
  // a reply cannot keep arriving into a view that is gone. Session switches and
  // a new chat go through dropStream instead -- and so does this, rather than a
  // bare abort: a redirect waiting on its abort when the view goes away resumes
  // on the other side of that await, and it is the generation bump, not the
  // cancelled fetch, that stops it POSTing into a view that no longer exists.
  useEffect(() => {
    return () => {
      dropStream()
    }
  }, [])


  return {
    sessionId: currentSessionId,
    bubbles,
    loadingHistory,
    streaming,
    noStreamTurn,
    runningElsewhere,
    turnCheckFailed,
    stoppingElsewhere,
    allowAlwaysOk,
    threadEl,
    inputEl,
    autoGrow,
    switchSession,
    newChat,
    refresh,
    sendMessage,
    stopTurn,
    stopElsewhere,
    retryTurnCheck,
    dismissTurnCheck,
    decide,
    pick,
    typeAnswer,
    submitQuestion,
    dismissQuestion,
  }
}
