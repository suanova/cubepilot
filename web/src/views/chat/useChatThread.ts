// One conversation's lifecycle: its key, its bubbles, its SSE turn, its HITL
// cards.
//
// The hook is the conversation, not the page. `ChatView` renders it beside a
// session list and the floating widget renders it alone, and neither of them
// owns a line of what happens below -- which is the point: the SSE turn and
// the parked-card races are the parts it would be worst to have two of.
import { useCallback, useEffect, useRef, useState } from 'react'
import { api } from '@/api'
import { ApiError } from '@/api/client'
import { streamSSE } from '@/api/sse'
import { getCurrentUser } from '@/api/client'
import type { HistoryContentBlock, HistoryMessage, PendingApproval, QuestionItem } from '@/api/types'
import { showToast } from '@/stores/toast'
import {
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
  bannerUp: boolean
  runningElsewhere: boolean
  // The turn-status check itself failed. Kept apart from `runningElsewhere` so
  // the banner never claims a turn nobody confirmed.
  turnCheckFailed: boolean
  stoppingElsewhere: boolean
  allowAlwaysOk: boolean
  threadEl: React.RefObject<HTMLDivElement | null>
  inputEl: React.RefObject<HTMLTextAreaElement | null>
  autoGrow(): void
  switchSession(id: string): Promise<void>
  newChat(): void
  sendMessage(): Promise<void>
  stopTurn(): Promise<boolean>
  stopElsewhere(): Promise<void>
  retryTurnCheck(): void
  dismissTurnCheck(): void
  decide(confirm: BubbleConfirm, decision: 'approve' | 'reject' | 'allow-always'): Promise<void>
  pick(q: BubbleQuestion, item: QuestionItem, label: string): void
  submitQuestion(q: BubbleQuestion): Promise<void>
  dismissQuestion(q: BubbleQuestion): Promise<void>
}

const user = getCurrentUser()

export function useChatThread({ onSessionStarted }: { onSessionStarted: () => void }): ChatThreadApi {

  const [currentSessionId, setCurrentSessionId] = useState<string | null>(null)
  const [bubbles, setBubbles] = useState<BubbleMsg[]>([])
  const [loadingHistory, setLoadingHistory] = useState(false)
  const [streaming, setStreaming] = useState(false)
  // A turn running without a stream of this view's own: it was started in
  // another tab, or this view was reloaded while the agent kept working. It can
  // still be stopped; its output arrives on the next history refresh, because
  // stream re-attach is out of scope.
  const [runningElsewhere, setRunningElsewhere] = useState(false)
  // The turn-status check itself failed. It is kept apart from
  // `runningElsewhere` so the banner never claims a turn nobody confirmed, and
  // it is still shown: the API answers 502 when it cannot determine, and
  // "cannot tell" is not "idle" -- hiding Stop here would strand exactly the
  // user whose turn is running.
  const [turnCheckFailed, setTurnCheckFailed] = useState(false)
  // The session whose banner Stop is waiting on `/abort`, or null. The server
  // answers only once the turn has settled -- seconds -- so the banner's Stop
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

  // The without-a-stream banner is on screen: a turn this view holds no stream
  // for, so the banner's Stop is the direct way to end it -- and a send while it
  // is up is a redirect, which `sendMessage` runs through its own
  // stop-then-send sequence.
  const bannerUp = (runningElsewhere || turnCheckFailed) && !streaming
  // ...and that Stop is waiting on the server for the session on screen. The
  // turn is then still running, so a send must not go out yet: it would be
  // POSTed against a session whose turn is still settling, and retiring the
  // banner would take away that turn's only Stop control. Both the controls and
  // the refusal in `sendMessage` read this.
  const stoppingElsewhere = bannerUp && stoppingSession === currentSessionId


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


  async function loadHistory(id: string) {
    setLoadingHistory(true)
    setBubbles([])
    try {
      const items = await api.sessionHistory(id)
      renderHistory(items, id)
      void recoverPending(id)
    } catch (e) {
      setBubbles([{ kind: 'assistant', text: 'History load failed: ' + String(e), tools: [], thinking: false }])
    } finally {
      setLoadingHistory(false)
      requestAnimationFrame(scrollThread)
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
  // user left must not paint a banner onto the view they moved to.
  async function checkTurnElsewhere(id: string, gen: number) {
    try {
      const { active } = await api.sessionTurn(id)
      if (streamGenRef.current !== gen) return
      setRunningElsewhere(!!active)
      setTurnCheckFailed(false)
    } catch {
      // Never folded into "not running": the API answers 502 exactly when it
      // could not determine whether the turn is still going.
      if (streamGenRef.current !== gen) return
      setRunningElsewhere(false)
      setTurnCheckFailed(true)
    }
  }

  // retryTurnCheck re-asks after a failed check. Without it an "unknown" banner
  // would have no way back to a definite answer short of leaving the session,
  // which is a poor trade for one cheap GET. The banner is withdrawn while the
  // retry is in flight and returns if the check fails again.
  function retryTurnCheck() {
    if (!currentSessionId) return
    setTurnCheckFailed(false)
    void checkTurnElsewhere(currentSessionId, streamGenRef.current)
  }

  // dismissTurnCheck withdraws the "could not check" banner on request. Retry is
  // the way back to an answer, but it is not a way *out*: for a channel this
  // process cannot use, every retry fails the same way, and a reload fails the
  // check again, so the banner would sit over a conversation that is otherwise
  // perfectly usable with no control that removes it. Dismissing claims nothing
  // -- the next reload, session switch or Retry asks again, and a send is
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
  async function recoverPending(id: string) {
    // Questions first: a parked ask_user turn is the more recent state, and a
    // session can hold several open questions (one card per record).
    try {
      const pending = await api.pendingQuestions(id)
      if (activeSessionRef.current !== id) return // stale: a different session is now active
      if (pending.length > 0) {
        setBubbles((prev) => [
          ...prev,
          {
            kind: 'assistant' as const,
            tools: [],
            thinking: false,
            phase: 'done' as const,
            questions: pending.map((p) => newBubbleQuestion(id, p.id, p.questions, p.timeoutSeconds)),
          },
        ])
        requestAnimationFrame(scrollThread)
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
            tools: [],
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
      return // no pending approval for this session
    }
    if (activeSessionRef.current !== id) return // stale: a different session is now active
    setBubbles((prev) => [
      ...prev,
      {
        kind: 'assistant' as const,
        tools: [],
        thinking: false,
        phase: 'done' as const,
        confirm: {
          sessionId: p.sessionId,
          approvalId: p.approvalId,
          command: p.command,
          level: p.level,
          message: p.message,
        },
      },
    ])
    syncAllowAlways()
    requestAnimationFrame(scrollThread)
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
  // mark and no evidence to record. With no stream -- the banner -- the only
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
    const openAssistant = () => {
      if (last && last.kind === 'assistant') return
      last = { kind: 'assistant', tools: [], thinking: false, phase: 'done' }
      out.push(last)
    }
    for (const it of items) {
      const content = blocks(it.content)
      if (it.role === 'user') {
        for (const c of content) {
          if (c.type === 'text' && c.text) {
            out.push({ kind: 'user', text: c.text, tools: [], thinking: false })
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
        for (const c of content) {
          if (c.type === 'toolCall') {
            last!.tools.push({ name: c.name || 'exec', cmd: toolArgsDisplay(c.arguments), callID: c.id || '', done: true })
          } else if (c.type === 'text' && c.text) {
            last!.text = (last!.text ? last!.text + '\n' : '') + c.text
          }
        }
        continue
      }
      // toolResult: pair the output with the tool call that produced it, so the
      // result renders directly under its command card.
      if (it.role === 'toolResult') {
        openAssistant()
        for (const c of content) {
          if (c.type === 'text' && c.text) attachToolResult(last!.tools, '', c.text)
        }
      }
    }
    setBubbles(applyStoppedTurn(out, session))
  }

  // dropStream retires the live stream: every event still in flight for it is
  // superseded (its generation no longer matches) and its fetch is cancelled.
  // The streaming flag is cleared here rather than left to the retiring
  // stream's own `finally`, which sees itself as stale and refuses to touch it.
  //
  // The without-a-stream banner is retired with it -- both describe a turn this
  // view is leaving behind -- and the generation bump discards a turn-status
  // check still in flight for it, whose snapshot could otherwise re-arm a
  // banner that has just been retired.
  function dropStream() {
    streamGenRef.current++
    abortRef.current?.abort()
    setStreaming(false)
    clearTurnElsewhere()
  }

  // clearTurnElsewhere retires the without-a-stream banner, both states at once
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
  // gateway abort. The banner's button is still on screen for the whole round
  // trip -- it is only withdrawn once the server has answered -- and it is
  // disabled in the meantime, but `stoppingRef` is what actually refuses a
  // click that arrives before that re-render: a second POST for a turn that is
  // already settling is redundant at best. A stop the server refuses is an
  // expected outcome -- 504 means it did not settle in time, 502 that the
  // channel was unavailable -- so it is surfaced as a toast and the banner
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
    // dropStream, not clearTurnElsewhere: the banner is done, and the
    // generation bump discards a turn-status check still in flight for it,
    // whose pre-stop snapshot would otherwise re-arm the banner the stop just
    // retired.
    dropStream()
    await loadHistory(session)
  }

  async function sendMessage() {
    const el = inputEl.current
    if (!el) return
    if (sendingRef.current) return
    const text = el.value.trim()
    if (!text) return
    // A banner Stop is in flight for the session on screen, so its turn is
    // still running server-side and its outcome is not known yet. Refusing here
    // keeps this send from racing it: the stop-then-send branch below would find
    // the stop already in flight and abandon the send anyway, retiring the
    // banner and, with it, the running turn's only Stop control. The composer's
    // Send is disabled and the banner's Stop reads "Stopping…" for the same
    // window, so this is not a click swallowed in silence; and it is
    // deliberately not a queue -- the text stays in the box, and Enter again
    // once the stop answers sends it.
    if (stoppingElsewhere) return
    // `streaming` and `bannerUp` are the same situation from the send's point of
    // view: a turn is running for the session on screen and this send is the
    // user redirecting it. A banner send has to take the stop-then-send route
    // too, not just the streaming one. Left on the plain path it gets one of two
    // wrong outcomes, neither of them a refusal the user could act on: the
    // pre-reload stream is still registered server-side, so the POST is refused
    // with a raw 409; or that stream has closed while the run has not, and the
    // gateway's default queueMode steers the text into the running turn -- no
    // stop, no new turn, and the message swallowed, which is the steering
    // behaviour the design declares a non-goal. `/abort` answers only once the
    // session has settled, so the send that follows it can do neither. (A
    // turnCheckFailed banner takes the same route: the check failed, so a turn
    // may well be running, and an abort is a no-op when none is.)
    if (streaming || bannerUp) {
      // Redirect: stop the running turn first. The server only answers once the
      // turn has settled, so the send below cannot hit the 409 guard. What
      // happens when the stop does not take is decided below, per banner: a
      // confirmed turn keeps the text and the Stop button, the un-checkable one
      // sends anyway.
      //
      // The guard is held for the whole stop-then-send sequence -- including the
      // banner's history reload -- and released on every exit path: the
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
      // The banner's own Stop control is on screen with nothing to show for the
      // wait, so the round trip is made visible the same way the banner's Stop
      // makes it visible: the composer's Send is disabled and the banner reads
      // "Stopping…" for the whole sequence, reload included. Held here rather
      // than left to `stopTurn`/`stopElsewhere`, which release it as soon as the
      // abort answers -- before the reload that is the rest of the wait.
      const holdVisibleStop = bannerUp && !!currentSessionId
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
          // The "could not check" banner is the exception, and it is the state
          // that would otherwise be a dead end. Nothing there confirmed a turn,
          // and the stop is refused for the same reason the check failed -- a
          // gateway channel this process cannot use -- so the banner's Stop
          // provably cannot work either. The send is then the only request left
          // that re-dials the channel (it is the turn path that calls `conn`,
          // see the API's PreTurn), and refusing it leaves no in-page recovery
          // at all: the user retries, reloads, and lands on the same banner,
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
        } else if (bannerUp) {
          // The banner's turn had no stream in this view, so nothing in it ever
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
    // This view is about to drive its own turn: the without-a-stream banner
    // describes the turn being left behind, and would otherwise reappear when
    // the new stream ends.
    clearTurnElsewhere()
    // A functional update, not a spread of bubblesRef.current: the history
    // reload above may have set new bubbles that React has not re-rendered yet,
    // and the ref would still hold the pre-reload list -- dropping exactly the
    // stopped turn that reload exists for.
    const bubble: BubbleMsg = { kind: 'assistant', tools: [], thinking: true }
    setBubbles((prev) => [...prev, { kind: 'user' as const, text, tools: [], thinking: false }, bubble])
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
          headers: { 'Content-Type': 'application/json', 'X-CubePilot-User': user },
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
            // dropStream already retired its banner. Turn *output* is different
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
          if (ev.type === 'agent_thinking') {
            setPhase(bubble, 'thinking')
            return
          }
          if (ev.type === 'tool_call') {
            setPhase(bubble, 'tools')
            bubble.tools.push({ name: ev.name, cmd: toolArgsDisplay(ev.arguments), callID: ev.callId || '', done: false })
            return
          }
          if (ev.type === 'tool_result') {
            setPhase(bubble, 'tools')
            // Attach unconditionally (an empty output is still a result): the
            // call must be marked done even when the tool returned nothing.
            attachToolResult(bubble.tools, ev.callId || '', ev.output || '')
            return
          }
          if (ev.type === 'approval_pending') {
            // A write is parked awaiting the human (issue #20). Show the card
            // immediately rather than waiting for the next 1s ticker render.
            setPhase(bubble, 'tools')
            bubble.confirm = {
              sessionId: ev.sessionId || currentSessionId || '',
              approvalId: ev.callId || '',
              command: ev.command || '',
              level: ev.level || 'write',
              message: ev.message,
            }
            syncAllowAlways()
            setBubbles([...bubblesRef.current])
            requestAnimationFrame(scrollThread)
            return
          }
          if (ev.type === 'approval_resolved') {
            if (bubble.confirm && (!ev.callId || bubble.confirm.approvalId === ev.callId)) {
              bubble.confirm.resolved = true
              // `approved` is a *bool on the wire: absent means nobody decided
              // this -- the turn was stopped while the write was parked -- and
              // that is not the same as the explicit false of a rejection.
              // Copying it only when present leaves the card reading "Stopped"
              // instead of painting the user a red "Rejected" they never chose.
              if (ev.approved !== undefined) bubble.confirm.approved = ev.approved
              bubble.confirm.busy = false
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
              bubble.questions = [
                ...(bubble.questions || []),
                newBubbleQuestion(
                  ev.sessionId || currentSessionId || '',
                  ev.callId || '',
                  ev.question.questions,
                  ev.question.timeoutSeconds,
                ),
              ]
              setBubbles([...bubblesRef.current])
              requestAnimationFrame(scrollThread)
            }
            return
          }
          if (ev.type === 'question_resolved') {
            // Settle only the matching card: another question of this turn may
            // still be open.
            const q = (bubble.questions || []).find((x) => !ev.callId || x.questionId === ev.callId)
            if (q) {
              q.resolved = true
              q.outcome = ev.message || 'answered'
              q.busy = false
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
            // a tool ran): replace, never append (issue #130).
            setPhase(bubble, 'streaming')
            bubble.text = ev.delta || ''
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
              // The turn may still be running with no stream of this view's
              // own, which is exactly the state the banner describes -- and the
              // banner's Stop is then the only control that can end it. Ask the
              // server rather than assert it: a run that really did settle
              // answers `active: false` and raises no banner. The check is
              // issued only for the current stream (a superseded one has left
              // its session behind, and dropStream already retired that
              // banner).
              if (!stale() && turnSession) void checkTurnElsewhere(turnSession, streamGenRef.current)
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
      await api.postQuestion(session, q.questionId, q.picked)
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
    bannerUp,
    runningElsewhere,
    turnCheckFailed,
    stoppingElsewhere,
    allowAlwaysOk,
    threadEl,
    inputEl,
    autoGrow,
    switchSession,
    newChat,
    sendMessage,
    stopTurn,
    stopElsewhere,
    retryTurnCheck,
    dismissTurnCheck,
    decide,
    pick,
    submitQuestion,
    dismissQuestion,
  }
}
