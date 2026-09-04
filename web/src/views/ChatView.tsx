// Chat view -- session list + thread + composer, SSE streaming from /api/messages.
import { useCallback, useEffect, useRef, useState } from 'react'
import ReactMarkdown from 'react-markdown'
import remarkBreaks from 'remark-breaks'
import { api } from '@/api'
import { streamSSE } from '@/api/sse'
import { getCurrentUser } from '@/api/client'
import type { HistoryContentBlock, HistoryMessage, PendingConfirm, SessionInfo } from '@/api/types'
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

interface BubbleMsg {
  kind: 'user' | 'assistant'
  text?: string
  tools: ToolCallVM[]
  error?: string
  thinking: boolean // true while phase !== 'done'
  phase?: BubblePhase
  phaseAt?: number // Date.now() when the current phase started
  confirm?: BubbleConfirm // a pending/resolved write confirmation on this bubble
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
  const threadEl = useRef<HTMLDivElement | null>(null)
  const inputEl = useRef<HTMLTextAreaElement | null>(null)
  const bubblesRef = useRef<BubbleMsg[]>([])

  // Keep a mutable mirror of bubbles so SSE callbacks can mutate the latest
  // assistant bubble without stale-closure problems.
  bubblesRef.current = bubbles

  // Tick every second so running bubbles show live "waiting Ns" in pauses.
  const [, setNow] = useState(Date.now())
  useEffect(() => {
    const ticker = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(ticker)
  }, [])

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

  // After a reload mid-approval the platform still holds the pending write; this
  // restores its confirmation card from the pending endpoint (issue #20).
  async function recoverPending(id: string) {
    let p: PendingConfirm
    try {
      p = await api.pendingConfirm(id)
    } catch {
      return // no pending approval for this session
    }
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

  const switchSession = useCallback(async (id: string) => {
    setCurrentSessionId(id)
    await loadHistory(id)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  function newChat() {
    setCurrentSessionId(null)
    setBubbles([])
    setLoadingHistory(false)
    requestAnimationFrame(() => inputEl.current?.focus())
  }

  async function sendMessage() {
    const el = inputEl.current
    if (!el) return
    const text = el.value.trim()
    if (!text) return
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

    try {
      await streamSSE(
        '/api/messages',
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/json', 'X-CubePilot-User': user },
          body: JSON.stringify({ session_id: currentSessionId, content: text }),
        },
        (_evName, ev) => {
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
            setBubbles([...bubblesRef.current])
            requestAnimationFrame(scrollThread)
            return
          }
          if (ev.type === 'confirm_resolved') {
            if (bubble.confirm && (!ev.call_id || bubble.confirm.approvalId === ev.call_id)) {
              bubble.confirm.resolved = true
              bubble.confirm.approved = !!ev.approved
              bubble.confirm.busy = false
              setBubbles([...bubblesRef.current])
            }
            return
          }
          if (ev.type === 'message_delta') {
            setPhase(bubble, 'streaming')
            bubble.text = (bubble.text || '') + (ev.delta || '')
            return
          }
          if (ev.type === 'message_done') {
            setPhase(bubble, 'done')
            if (ev.error) bubble.error = ev.error
            return
          }
        },
      )
    } catch (e) {
      setPhase(bubble, 'done')
      bubble.error = String(e)
    } finally {
      // Push a new array reference so React re-renders with the mutated bubble.
      setBubbles([...bubblesRef.current])
      requestAnimationFrame(scrollThread)
    }
  }

  // decide sends the human's answer for a pending write confirmation
  // (issue #20). POSTing resolves the gateway approval; the SSE stream then
  // carries the resumed turn.
  async function decide(confirm: BubbleConfirm, decision: 'approve' | 'reject') {
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
      confirm.approved = decision === 'approve'
    } catch (e) {
      confirm.error = String(e)
    } finally {
      confirm.busy = false
      setBubbles([...bubblesRef.current])
      requestAnimationFrame(scrollThread)
    }
  }

  useEffect(() => {
    loadSessions()
  }, [])

  function statusLine(b: BubbleMsg): string {
    if (b.kind === 'user' || !b.phase) return ''
    if (b.kind === 'assistant' && b.confirm && !b.confirm.resolved) return 'Awaiting your approval...'
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
                    {b.phase === 'done' && !b.error && (
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
                          {b.confirm.resolved ? (
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
                          </div>
                        )}
                        {b.confirm.error && (
                          <div style={{ fontSize: 12.5, color: 'var(--danger)', marginTop: 6 }}>{b.confirm.error}</div>
                        )}
                      </div>
                    )}
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
            <button className="send-btn" aria-label="Send" onClick={sendMessage}>
              <SendIcon />
            </button>
          </div>
          <div className="composer-hint">
            Operate platform resources via natural language - type <span className="mono">@</span> to reference a resource - write operations ask for your approval before they run
          </div>
        </div>
      </div>
    </div>
  )
}