// Chat view -- session list + thread + composer, SSE streaming from /api/messages.
import { useCallback, useEffect, useRef, useState } from 'react'
import ReactMarkdown from 'react-markdown'
import remarkBreaks from 'remark-breaks'
import { api } from '@/api'
import { ApiError } from '@/api/client'
import { streamSSE } from '@/api/sse'
import { getCurrentUser } from '@/api/client'
import type { HistoryContentBlock, HistoryMessage, PendingConfirm, QuestionItem, SessionInfo } from '@/api/types'
import { showToast } from '@/stores/toast'
import { shortSession } from '@/utils/format'

const user = getCurrentUser()
const userInitials = user
  .split(/[._-]/)
  .map((p) => p[0]?.toUpperCase() ?? '')
  .slice(0, 2)
  .join('')

interface ToolCallVM {
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
type BubblePhase = 'thinking' | 'tools' | 'streaming' | 'done'

// A write operation paused for a human decision (issue #20 HITL).
interface BubbleConfirm {
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
interface BubbleQuestion {
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

interface BubbleMsg {
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
  // Every question this turn has asked, in order. The agent can ask several in
  // one turn (a blocked ask_user resumes, then another follows), so they are
  // kept as a collection rather than one slot that a later question replaces --
  // that would drop an answered card's record and, on the recovery path, the
  // other open questions.
  questions?: BubbleQuestion[]
}

// newBubbleQuestion builds the card state for one question event or recovery
// entry.
function newBubbleQuestion(sessionId: string, questionId: string, items: QuestionItem[], timeoutSeconds?: number): BubbleQuestion {
  return {
    sessionId,
    questionId,
    items,
    deadline: timeoutSeconds ? Date.now() + timeoutSeconds * 1000 : undefined,
    picked: {},
  }
}

// questionAnswered reports whether every question in the card has a selection.
function questionAnswered(q: BubbleQuestion): boolean {
  return q.items.every((it) => (q.picked[it.questionId] || []).length > 0)
}

// attachToolResult pairs a tool's output with the tool call that produced it:
// exact call_id when the stream carries one, otherwise the oldest call without
// a result yet (the gateway emits tool calls and results in the same order).
function attachToolResult(tools: ToolCallVM[], callID: string, output: string) {
  const t =
    (callID && tools.find((x) => x.callID === callID)) ||
    tools.find((x) => !x.done) ||
    tools.find((x) => !x.result)
  if (!t) return
  t.result = t.result ? t.result + '\n' + output : output
  t.done = true
}

function ChatBubbleIcon() {
  return (
    <svg className="s-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round">
      <path d="M21 11.5a8.38 8.38 0 0 1-.9 3.8 8.5 8.5 0 0 1-7.6 4.7 8.38 8.38 0 0 1-3.8-.9L3 21l1.9-5.7a8.38 8.38 0 0 1-.9-3.8 8.5 8.5 0 0 1 4.7-7.6 8.38 8.38 0 0 1 3.8-.9h.5a8.48 8.48 0 0 1 8 8v.5z" />
    </svg>
  )
}

// QuestionCard renders an ask_user question (issue #161): one block per
// question with its options as selectable buttons, plus Submit and Dismiss.
// The countdown is presentation only -- the gateway's resolve response is what
// settles the card, and the server relays its expiry as question_resolved.
function QuestionCard({
  question,
  onPick,
  onSubmit,
  onDismiss,
}: {
  question: BubbleQuestion
  onPick: (q: BubbleQuestion, item: QuestionItem, label: string) => void
  onSubmit: (q: BubbleQuestion) => void
  onDismiss: (q: BubbleQuestion) => void
}) {
  const remaining = question.deadline ? Math.max(0, Math.round((question.deadline - Date.now()) / 1000)) : undefined
  const settled = question.resolved
  // The local countdown has run out but the gateway has not settled the
  // question yet: it is about to (or already has). Stop offering the controls
  // rather than let the user click into a 409, but do not claim "Expired"
  // ourselves -- that is the gateway's call, and only it can say so.
  const expiring = !settled && remaining === 0
  const locked = settled || expiring
  const outcomeLabel: Record<string, string> = {
    answered: 'Answered',
    cancelled: 'Dismissed',
    expired: 'Expired',
  }
  return (
    <div className="tool-card" style={{ borderColor: 'rgba(59,130,246,.45)' }}>
      <div className="tool-head">
        <ToolIcon />
        <span className="tool-cmd">Question from the agent</span>
        {settled ? (
          <span
            style={{
              fontSize: 12,
              borderRadius: 999,
              padding: '2px 10px',
              background: question.outcome === 'answered' ? 'rgba(34,197,94,.15)' : 'rgba(0,0,0,.07)',
              color: question.outcome === 'answered' ? '#15803d' : 'rgba(0,0,0,.55)',
            }}
          >
            {outcomeLabel[question.outcome || ''] || 'Closed'}
          </span>
        ) : (
          <span
            style={{
              fontSize: 12,
              borderRadius: 999,
              padding: '2px 10px',
              background: 'rgba(59,130,246,.15)',
              color: '#1d4ed8',
            }}
          >
            {expiring ? 'Expiring…' : `Awaiting your answer${remaining !== undefined ? ` · ${remaining}s` : ''}`}
          </span>
        )}
      </div>
      {question.items.map((item) => {
        const picked = question.picked[item.questionId] || []
        return (
          <div key={item.questionId} style={{ padding: '10px 12px', borderTop: '1px solid var(--border)' }}>
            <div style={{ display: 'flex', alignItems: 'center', gap: 6, marginBottom: 4 }}>
              {item.header && (
                <span style={{ fontSize: 11, letterSpacing: '.04em', textTransform: 'uppercase', color: 'var(--muted, rgba(0,0,0,.55))' }}>
                  {item.header}
                </span>
              )}
              {item.multiSelect && (
                <span style={{ fontSize: 11, color: 'var(--muted, rgba(0,0,0,.55))' }}>select one or more</span>
              )}
            </div>
            <div style={{ fontSize: 13.5, lineHeight: 1.6, marginBottom: 8 }}>{item.question}</div>
            {/* role=group + aria-label give the options an accessible group and
                name; the selected state itself is exposed by aria-pressed on
                each button rather than by colour alone. */}
            <div role="group" aria-label={item.question} style={{ display: 'flex', flexWrap: 'wrap', gap: 8 }}>
              {item.options.map((o) => {
                const active = picked.includes(o.label)
                return (
                  <button
                    key={o.label}
                    onClick={() => onPick(question, item, o.label)}
                    disabled={locked || !!question.busy}
                    aria-pressed={active}
                    title={o.description}
                    style={{
                      background: active ? 'var(--accent, #3b82f6)' : 'none',
                      border: `1px solid ${active ? 'var(--accent, #3b82f6)' : 'var(--border)'}`,
                      color: active ? '#fff' : 'inherit',
                      borderRadius: 6,
                      padding: '6px 14px',
                      cursor: settled ? 'default' : 'pointer',
                      fontSize: 13,
                      textAlign: 'left',
                    }}
                  >
                    {o.label}
                    {o.description && (
                      <span style={{ display: 'block', fontSize: 11.5, opacity: 0.8, marginTop: 2 }}>{o.description}</span>
                    )}
                  </button>
                )
              })}
            </div>
          </div>
        )
      })}
      {!settled && (
        <div style={{ display: 'flex', gap: 8, padding: '0 12px 12px' }}>
          <button
            onClick={() => onDismiss(question)}
            disabled={locked || !!question.busy}
            title="Dismiss the question and let the agent continue without an answer"
            style={{
              background: 'none',
              border: '1px solid var(--border)',
              borderRadius: 6,
              padding: '6px 14px',
              cursor: locked ? 'default' : 'pointer',
              opacity: locked ? 0.5 : 1,
              fontSize: 13,
            }}
          >
            Dismiss
          </button>
          <button
            onClick={() => onSubmit(question)}
            disabled={locked || !!question.busy || !questionAnswered(question)}
            style={{
              background: 'var(--accent, #3b82f6)',
              border: 'none',
              color: '#fff',
              borderRadius: 6,
              padding: '6px 14px',
              cursor: !locked && questionAnswered(question) ? 'pointer' : 'default',
              opacity: locked || !questionAnswered(question) ? 0.5 : 1,
              fontSize: 13,
            }}
          >
            {question.busy ? 'Sending…' : 'Submit'}
          </button>
        </div>
      )}
      {question.error && (
        <div style={{ fontSize: 12.5, color: 'var(--danger)', padding: '0 12px 12px' }}>{question.error}</div>
      )}
    </div>
  )
}

function SearchIcon() {
  return (
    <svg className="icon" style={{ width: 14, height: 14 }} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round">
      <circle cx="11" cy="11" r="8" />
      <path d="M21 21l-4.35-4.35" />
    </svg>
  )
}

function NewChatIcon() {
  return (
    <svg className="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round">
      <path d="M12 5v14M5 12h14" />
    </svg>
  )
}

function SendIcon() {
  return (
    <svg className="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round">
      <path d="M22 2 11 13M22 2l-7 20-4-9-9-4z" />
    </svg>
  )
}

// A filled square, unlike the outline icons it sits beside. The shared `.icon`
// class paints outlines (`fill:none`), and a CSS rule beats any presentation
// attribute, so the fill is set inline where it wins.
function StopIcon() {
  return (
    <svg
      className="icon"
      viewBox="0 0 24 24"
      aria-hidden="true"
      style={{ width: 13, height: 13, fill: 'currentColor', stroke: 'none' }}
    >
      <rect x="5" y="5" width="14" height="14" rx="2.5" />
    </svg>
  )
}

function DoneCheckIcon() {
  return (
    <svg className="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round">
      <path d="M20 6L9 17l-5-5" />
    </svg>
  )
}

function EmptyChatIcon() {
  return (
    <svg className="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.4" strokeLinecap="round" strokeLinejoin="round">
      <path d="M21 11.5a8.38 8.38 0 0 1-.9 3.8 8.5 8.5 0 0 1-7.6 4.7 8.38 8.38 0 0 1-3.8-.9L3 21l1.9-5.7a8.38 8.38 0 0 1-.9-3.8 8.5 8.5 0 0 1 4.7-7.6 8.38 8.38 0 0 1 3.8-.9h.5a8.48 8.48 0 0 1 8 8v.5z" />
    </svg>
  )
}

function ToolIcon() {
  return (
    <svg className="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round">
      <path d="M4 17l6-6-6-6M12 19h8" />
    </svg>
  )
}

function setPhase(b: BubbleMsg, phase: BubblePhase) {
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
function redact(value: unknown): unknown {
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

function summarizeArgs(args: unknown): string {
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

function toolArgsDisplay(args: unknown): string {
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
function MdText({ text }: { text: string }) {
  return (
    <div className="md">
      <ReactMarkdown remarkPlugins={[remarkBreaks]}>{text}</ReactMarkdown>
    </div>
  )
}

export default function ChatView() {
  const [sessions, setSessions] = useState<SessionInfo[]>([])
  const [sessionSearch, setSessionSearch] = useState('')
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
  // for, so the banner's Stop -- not this composer's Send -- owns it.
  const bannerUp = (runningElsewhere || turnCheckFailed) && !streaming
  // ...and that Stop is waiting on the server for the session on screen. The
  // turn is then still running, so a send must not go out yet: it would be
  // POSTed against a session whose turn is still settling (the redirect path
  // that serializes stop-then-send is not available here, there being no stream
  // to redirect), and retiring the banner would take away that turn's only Stop
  // control. Both the controls and the refusal in `sendMessage` read this.
  const stoppingElsewhere = bannerUp && stoppingSession === currentSessionId

  const chatTitle = (() => {
    if (!currentSessionId) return 'New conversation'
    const s = sessions.find((x) => x.sessionKey === currentSessionId)
    return s?.title || shortSession(currentSessionId)
  })()

  const filteredSessions = (() => {
    const q = sessionSearch.trim().toLowerCase()
    if (!q) return sessions
    return sessions.filter((s) => (s.title || s.sessionKey).toLowerCase().includes(q))
  })()

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

  async function loadSessions() {
    try {
      setSessions(await api.listSessions())
    } catch {
      /* keep whatever we have */
    }
  }

  async function loadHistory(id: string) {
    setLoadingHistory(true)
    setBubbles([])
    try {
      const items = await api.sessionHistory(id)
      renderHistory(items)
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

  // syncAllowAlways refreshes whether a durable "Always allow" is meaningful
  // for the effective confirmation policy (Allowlist only). Called when a
  // confirmation card appears.
  function syncAllowAlways() {
    api
      .agentConfirm()
      .then((v) => setAllowAlwaysOk(!!v.exists && v.confirmPolicy === 'Allowlist'))
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
    let p: PendingConfirm
    try {
      p = await api.pendingConfirm(id)
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
          sessionId: p.session_id,
          approvalId: p.approval_id,
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

  function renderHistory(items: HistoryMessage[]) {
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
    setBubbles(out)
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
    // refusal, not a success -- a POST with an empty `session_id` does not
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
    stoppingRef.current = true
    try {
      await api.abortSession(currentSessionId)
    } catch (e) {
      showToast(String(e))
      return false
    } finally {
      stoppingRef.current = false
    }
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
    stoppingRef.current = true
    setStoppingSession(session)
    try {
      await api.abortSession(session)
    } catch (e) {
      showToast(String(e))
      return
    } finally {
      stoppingRef.current = false
      setStoppingSession(null)
    }
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
    // still running server-side. Refusing here is what keeps a send from racing
    // that stop: the redirect branch below is not taken -- this view holds no
    // stream to stop -- so the send would take the plain path and be POSTed
    // against a turn that has not settled, retiring the banner and with it the
    // running turn's only Stop control. The composer's Send is disabled and the
    // banner's Stop reads "Stopping…" for the same window, so this is not a
    // click swallowed in silence; and it is deliberately not a queue -- the
    // text stays in the box, and Enter again once the stop answers sends it.
    if (stoppingElsewhere) return
    if (streaming) {
      // Redirect: stop the running turn first. The server only answers once the
      // turn has settled, so the send below cannot hit the 409 guard. If the
      // stop did not take, the send is abandoned -- issuing it would be refused
      // as a concurrent turn and would leave the running turn with no Stop
      // control, so the user keeps their text and the Stop button instead.
      //
      // The guard is held for the whole stop-then-send sequence and released on
      // both exits: the abandoned redirect below, and the send that follows. The
      // release cannot be observed by a second submission: from it to the
      // textarea being cleared there is no await, so both happen in one turn.
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
      sendingRef.current = true
      let stopped = false
      try {
        stopped = await stopTurn()
      } finally {
        sendingRef.current = false
      }
      if (streamGenRef.current !== genAtSend) return
      if (!stopped) return
    }
    // This view is about to drive its own turn: the without-a-stream banner
    // describes the turn being left behind, and would otherwise reappear when
    // the new stream ends.
    clearTurnElsewhere()
    const nextBubbles = [
      ...bubblesRef.current,
      { kind: 'user' as const, text, tools: [], thinking: false },
      { kind: 'assistant' as const, tools: [], thinking: true },
    ]
    setBubbles(nextBubbles)
    el.value = ''
    el.style.height = 'auto'
    const bubble: BubbleMsg = nextBubbles[nextBubbles.length - 1]
    requestAnimationFrame(scrollThread)

    const gen = ++streamGenRef.current
    const stale = () => streamGenRef.current !== gen
    const controller = new AbortController()
    abortRef.current = controller
    setStreaming(true)

    try {
      await streamSSE(
        '/api/messages',
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/json', 'X-CubePilot-User': user },
          body: JSON.stringify({ session_id: currentSessionId, content: text }),
        },
        (_evName, ev) => {
          if (stale()) {
            // A superseded stream paints nothing into the current view -- with
            // one exception. A redirect leaves the stopped turn's bubble on
            // screen, and the events that close that turn out address that
            // bubble, not a turn the view has moved on from. They are:
            //   - its own `message_done`, else the bubble spins "Running..."
            //     forever;
            //   - the `confirm_resolved` / `question_resolved` the abort
            //     publishes alongside it, else its write card keeps live
            //     Approve/Reject buttons that POST to a record the settle
            //     already deleted (and its question card keeps offering an
            //     answer, to the same effect).
            // Those are state transitions on a bubble the user is still looking
            // at, and they happen to be the only way it can leave the "live"
            // rendering. Turn *output* is different and stays dropped: deltas,
            // tool calls and fresh pending cards carry content from the superseded
            // turn's own conversation, which is exactly what the user redirected
            // away from. The session-switch path aborts the fetch instead, so an
            // aborted stream emits nothing at all and cannot reach here.
            const settlesSupersededTurn =
              ev.type === 'message_done' || ev.type === 'confirm_resolved' || ev.type === 'question_resolved'
            if (!settlesSupersededTurn) return
          }
          if (ev.type === 'message_start') {
            if (ev.session_id) {
              setCurrentSessionId(ev.session_id)
              loadSessions()
            }
            return
          }
          if (ev.type === 'agent_thinking') {
            setPhase(bubble, 'thinking')
            return
          }
          if (ev.type === 'tool_call') {
            setPhase(bubble, 'tools')
            bubble.tools.push({ name: ev.name, cmd: toolArgsDisplay(ev.arguments), callID: ev.call_id || '', done: false })
            return
          }
          if (ev.type === 'tool_result') {
            setPhase(bubble, 'tools')
            // Attach unconditionally (an empty output is still a result): the
            // call must be marked done even when the tool returned nothing.
            attachToolResult(bubble.tools, ev.call_id || '', ev.output || '')
            return
          }
          if (ev.type === 'confirm_pending') {
            // A write is parked awaiting the human (issue #20). Show the card
            // immediately rather than waiting for the next 1s ticker render.
            setPhase(bubble, 'tools')
            bubble.confirm = {
              sessionId: ev.session_id || currentSessionId || '',
              approvalId: ev.call_id || '',
              command: ev.command || '',
              level: ev.level || 'write',
              message: ev.message,
            }
            syncAllowAlways()
            setBubbles([...bubblesRef.current])
            requestAnimationFrame(scrollThread)
            return
          }
          if (ev.type === 'confirm_resolved') {
            if (bubble.confirm && (!ev.call_id || bubble.confirm.approvalId === ev.call_id)) {
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
                  ev.session_id || currentSessionId || '',
                  ev.call_id || '',
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
            const q = (bubble.questions || []).find((x) => !ev.call_id || x.questionId === ev.call_id)
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
            setPhase(bubble, 'done')
            // A stopped turn is neither a failure nor a normal completion, so
            // it sets `stopped` and leaves `error` empty.
            if (ev.stopped) bubble.stopped = true
            else if (ev.error) bubble.error = ev.error
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
  // command on the instance allowlist so it auto-passes from then on. POSTing
  // resolves the gateway approval; the SSE stream then carries the resumed turn.
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
      await api.postConfirm(session, decision)
      confirm.resolved = true
      confirm.approved = decision !== 'reject'
    } catch (e) {
      confirm.error = String(e)
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

  useEffect(() => {
    loadSessions()
  }, [])

  // Unmount is the one teardown the stream cannot outlive: cancel the fetch so
  // a reply cannot keep arriving into a view that is gone. Session switches and
  // a new chat go through dropStream instead.
  useEffect(() => {
    return () => {
      abortRef.current?.abort()
    }
  }, [])

  function statusLine(b: BubbleMsg): string {
    if (b.kind === 'user' || !b.phase) return ''
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

  return (
    <div className="chat-body">
      <div className="session-panel">
        <div className="session-head">
          <div className="session-search">
            <SearchIcon />
            <input value={sessionSearch} onChange={(e) => setSessionSearch(e.target.value)} placeholder="Search conversations" aria-label="Search conversations" />
          </div>
          <button className="new-chat" aria-label="New conversation" onClick={newChat}>
            <NewChatIcon />
          </button>
        </div>
        <div className="session-list">
          {filteredSessions.map((s) => (
            <div
              key={s.sessionKey}
              className={`session-item ${currentSessionId === s.sessionKey ? 'active' : ''}`}
              onClick={() => switchSession(s.sessionKey)}
            >
              <ChatBubbleIcon />
              <div className="s-main">
                <div className="s-title">{s.title || shortSession(s.sessionKey)}</div>
                <div className="s-meta">
                  <span className="mono" style={{ fontSize: 10 }}>{shortSession(s.sessionKey)}</span>
                </div>
              </div>
            </div>
          ))}
        </div>
      </div>

      <div className="chat-main">
        <div className="chat-head">
          <div className="chat-head-main">
            <div className="chat-head-title">{chatTitle}</div>
            <div className="chat-head-meta">
              {loadingHistory ? 'Loading history...' : currentSessionId ? 'History loaded - continue the conversation' : 'Not started yet'}
            </div>
          </div>
        </div>

        <div ref={threadEl} className="thread">
          <div className="thread-inner">
            {bubbles.length ? (
              bubbles.map((b, i) => (
                <div key={i} className={`msg ${b.kind}`}>
                  <div className="avatar">{b.kind === 'user' ? userInitials : 'AI'}</div>
                  <div className="bubble">
                    {b.phase && b.phase !== 'done' && (
                      <div className="tool-status">
                        <span className="spin" />
                        {statusLine(b)}
                      </div>
                    )}
                    {b.phase === 'done' && b.stopped && (
                      // A stopped turn gets the muted marker, not the green Done
                      // check: it neither finished nor failed.
                      <div className="tool-status">
                        <StopIcon />
                        {statusLine(b)}
                      </div>
                    )}
                    {b.phase === 'done' && !b.stopped && !b.error && (
                      <div className="tool-status done-mark">
                        <DoneCheckIcon />
                        {statusLine(b)}
                      </div>
                    )}
                    {b.tools.map((t, ti) => (
                      <div key={'t' + ti} className={`tool-card ${!t.done && b.phase !== 'done' ? 'tool-running' : ''}`}>
                        <div className="tool-head">
                          <ToolIcon />
                          <span className="tool-cmd">{t.name}</span>
                          {!t.done && b.phase !== 'done' ? (
                            <span className="pill accent">Running...</span>
                          ) : !t.done ? (
                            // The turn was stopped while this tool was still in
                            // flight: `message_done{stopped}` freezes the phase
                            // to done, so the neutral "Done" pill would claim
                            // completion for work that was interrupted.
                            <span className="pill neutral">Stopped</span>
                          ) : (
                            <span className="pill neutral">Done</span>
                          )}
                        </div>
                        {t.cmd && (
                          <div className="tool-body">
                            <span className="mono" style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-all' }}>{t.cmd}</span>
                          </div>
                        )}
                        {t.result && (
                          <div
                            className="mono"
                            style={{
                              marginTop: 8,
                              background: 'rgba(0,0,0,.04)',
                              border: '1px solid var(--border)',
                              borderRadius: 6,
                              padding: '8px 10px',
                              maxHeight: 220,
                              overflow: 'auto',
                              fontSize: 12,
                              lineHeight: 1.6,
                              whiteSpace: 'pre-wrap',
                              wordBreak: 'break-all',
                            }}
                          >
                            {t.result}
                          </div>
                        )}
                      </div>
                    ))}
                    {/* A write awaiting (or resolved by) a human decision
                        (issue #20 HITL). */}
                    {b.confirm && (
                      <div className="tool-card" style={{ borderColor: 'rgba(245,158,11,.45)' }}>
                        <div className="tool-head">
                          <ToolIcon />
                          <span className="tool-cmd">Write confirmation</span>
                          {b.confirm.resolved && b.confirm.approved === undefined ? (
                            // Settled without a decision: the turn was stopped
                            // while this write was parked. Neutral, not a
                            // rejection the user never made.
                            <span
                              style={{
                                fontSize: 12,
                                borderRadius: 999,
                                padding: '2px 10px',
                                background: 'rgba(0,0,0,.07)',
                                color: 'rgba(0,0,0,.55)',
                              }}
                            >
                              Stopped
                            </span>
                          ) : b.confirm.resolved ? (
                            <span
                              style={{
                                fontSize: 12,
                                borderRadius: 999,
                                padding: '2px 10px',
                                background: b.confirm.approved ? 'rgba(34,197,94,.15)' : 'rgba(239,68,68,.15)',
                                color: b.confirm.approved ? '#15803d' : '#b91c1c',
                              }}
                            >
                              {b.confirm.approved ? 'Approved' : 'Rejected'}
                            </span>
                          ) : (
                            <span
                              style={{
                                fontSize: 12,
                                borderRadius: 999,
                                padding: '2px 10px',
                                background: 'rgba(245,158,11,.15)',
                                color: '#b45309',
                              }}
                            >
                              Awaiting your decision
                            </span>
                          )}
                        </div>
                        {b.confirm.command && (
                          <div className="tool-body">
                            <span className="mono" style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-all' }}>{b.confirm.command}</span>
                          </div>
                        )}
                        {b.confirm.message && (
                          <div style={{ fontSize: 12.5, color: 'var(--muted, rgba(0,0,0,.55))', marginTop: 6, lineHeight: 1.5 }}>{b.confirm.message}</div>
                        )}
                        {!b.confirm.resolved && (
                          <div style={{ display: 'flex', gap: 8, marginTop: 10 }}>
                            <button
                              onClick={() => decide(b.confirm!, 'reject')}
                              disabled={!!b.confirm.busy}
                              style={{ background: 'none', border: '1px solid var(--danger)', color: 'var(--danger)', borderRadius: 6, padding: '6px 14px', cursor: 'pointer', fontSize: 13 }}
                            >
                              Reject
                            </button>
                            <button
                              onClick={() => decide(b.confirm!, 'approve')}
                              disabled={!!b.confirm.busy}
                              style={{ background: 'var(--accent, #3b82f6)', border: 'none', color: '#fff', borderRadius: 6, padding: '6px 14px', cursor: 'pointer', fontSize: 13 }}
                            >
                              {b.confirm.busy ? 'Sending…' : 'Approve'}
                            </button>
                            {allowAlwaysOk && (
                              <button
                                onClick={() => decide(b.confirm!, 'allow-always')}
                                disabled={!!b.confirm.busy}
                                title="Approve and add this command to your allowlist so it no longer asks"
                                style={{ background: 'none', border: '1px solid var(--accent, #3b82f6)', color: 'var(--accent, #3b82f6)', borderRadius: 6, padding: '6px 14px', cursor: 'pointer', fontSize: 13 }}
                              >
                                Always allow
                              </button>
                            )}
                          </div>
                        )}
                        {b.confirm.error && (
                          <div style={{ fontSize: 12.5, color: 'var(--danger)', marginTop: 6 }}>{b.confirm.error}</div>
                        )}
                      </div>
                    )}
                    {/* Questions the agent is blocked on until answered, one
                        card each (issue #161). */}
                    {(b.questions || []).map((q) => (
                      <QuestionCard key={q.questionId} question={q} onPick={pick} onSubmit={submitQuestion} onDismiss={dismissQuestion} />
                    ))}
                    {/* When an assistant reply ran tools, its closing text is the
                        takeaway: render it as a highlighted panel so it stands out
                        from the tool log. Assistant text renders as Markdown; user
                        messages stay plain text. */}
                    {b.kind === 'assistant' && b.tools.length > 0 && b.text ? (
                      <div className="answer-panel">
                        <span className="answer-label">最终结果</span>
                        <MdText text={b.text} />
                      </div>
                    ) : b.kind === 'assistant' && b.text ? (
                      <MdText text={b.text} />
                    ) : b.text ? (
                      <div style={{ fontSize: 13.5, lineHeight: 1.7, whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>{b.text}</div>
                    ) : null}
                    {b.error && <div style={{ fontSize: 13, color: 'var(--danger)', whiteSpace: 'pre-wrap' }}>{b.error}</div>}
                  </div>
                </div>
              ))
            ) : (
              <div className="thread-empty">
                <EmptyChatIcon />
                <div className="thread-empty-title">Start a new conversation</div>
                <div className="thread-empty-desc">Type your request below; CubePilot will use platform skills to troubleshoot, deploy or query resources for you.</div>
              </div>
            )}
          </div>
        </div>

        <div className="composer">
          {/* A turn with no stream attached to this view (another tab, or a
              reload). It sits above the input, and is hidden while this view
              streams its own turn: the composer's Stop is the control for that
              one. */}
          {bannerUp && (
            <div className="turn-banner">
              {runningElsewhere && <span className="spin" />}
              <span>
                {runningElsewhere
                  ? 'Still running…'
                  : 'Could not check whether this chat is still running.'}
              </span>
              {turnCheckFailed && (
                <button className="btn sm ghost" onClick={retryTurnCheck}>
                  Retry
                </button>
              )}
              {/* Still on screen while the abort is in flight, unlike the
                  composer's glyph button which has no label to change. */}
              <button className="btn sm" onClick={stopElsewhere} disabled={stoppingElsewhere}>
                {stoppingElsewhere ? 'Stopping…' : 'Stop'}
              </button>
            </div>
          )}
          <div className="composer-inner">
            <textarea
              ref={inputEl}
              rows={1}
              placeholder="Type a command, e.g. `check GPU utilization` or `create a development environment`..."
              aria-label="Message input"
              onInput={autoGrow}
              onKeyDown={(e) => {
                if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
                  e.preventDefault()
                  sendMessage()
                }
              }}
            />
            {/* While a turn is running the send button becomes Stop. The
                textarea stays enabled so the user can type the redirect they
                want to send next. */}
            {streaming ? (
              <button className="send-btn" aria-label="Stop" onClick={stopTurn}>
                <StopIcon />
              </button>
            ) : (
              // Disabled while a banner Stop is in flight, so the refusal in
              // `sendMessage` is something the user can see. Enter in the
              // textarea still reaches it -- the button is a shortcut, not the
              // only path -- which is why the refusal lives there too.
              <button className="send-btn" aria-label="Send" onClick={sendMessage} disabled={stoppingElsewhere}>
                <SendIcon />
              </button>
            )}
          </div>
          <div className="composer-hint">
            Operate platform resources via natural language - type <span className="mono">@</span> to reference a resource - write operations ask for your approval before they run
          </div>
        </div>
      </div>
    </div>
  )
}
