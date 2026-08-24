// Chat view — session list + thread + composer, SSE streaming from /api/messages.
import { useCallback, useEffect, useRef, useState } from 'react'
import { api } from '@/api'
import { streamSSE } from '@/api/sse'
import { getCurrentUser } from '@/api/client'
import type { HistoryMessage, SessionInfo } from '@/api/types'
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
}

// Turn lifecycle phases; drives the per-bubble status line so the user can
// always tell "still working" (think/tools/stream) from "finished" (done).
type BubblePhase = 'thinking' | 'tools' | 'streaming' | 'done'

interface BubbleMsg {
  kind: 'user' | 'assistant'
  text?: string
  tools: ToolCallVM[]
  toolResults: string[]
  error?: string
  thinking: boolean // true while phase !== 'done'
  phase?: BubblePhase
  phaseAt?: number // Date.now() when the current phase started
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
    if (!currentSessionId) return '新会话'
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
    } catch (e) {
      setBubbles([{ kind: 'assistant', text: '历史加载失败：' + String(e), tools: [], toolResults: [], thinking: false }])
    } finally {
      setLoadingHistory(false)
      requestAnimationFrame(scrollThread)
    }
  }

  function renderHistory(items: HistoryMessage[]) {
    const out: BubbleMsg[] = []
    let last: BubbleMsg | null = null
    for (const it of items) {
      if (it.role === 'user') {
        for (const c of it.content) {
          if (c.type === 'text' && c.text) {
            out.push({ kind: 'user', text: c.text, tools: [], toolResults: [], thinking: false })
          }
        }
        last = null
        continue
      }
      if (it.role === 'assistant') {
        const hasTool = it.content.some((c) => c.type === 'toolCall')
        const hasText = it.content.some((c) => c.type === 'text' && c.text)
        if (!hasTool && !hasText) continue
        if (!last || last.kind !== 'assistant') {
          last = { kind: 'assistant', tools: [], toolResults: [], thinking: false, phase: 'done' }
          out.push(last)
        }
        for (const c of it.content) {
          if (c.type === 'toolCall') {
            const args = typeof c.arguments === 'string' ? c.arguments : JSON.stringify(c.arguments || {})
            let cmd = ''
            try {
              const a = JSON.parse(args)
              cmd = a.command || a.cmd || ''
            } catch {
              cmd = args
            }
            last.tools.push({ name: c.name || 'exec', cmd, callID: c.id || '', done: true })
          } else if (c.type === 'text' && c.text) {
            last.text = (last.text ? last.text + '\n' : '') + c.text
          }
        }
        continue
      }
      if (it.role === 'toolResult') {
        for (const c of it.content) {
          if (c.type === 'text' && c.text) {
            if (!last || last.kind !== 'assistant') {
              last = { kind: 'assistant', tools: [], toolResults: [], thinking: false, phase: 'done' }
              out.push(last)
            }
            last.toolResults.push(c.text)
          }
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
      { kind: 'user' as const, text, tools: [], toolResults: [], thinking: false },
      { kind: 'assistant' as const, tools: [], toolResults: [], thinking: true },
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
            let cmd = ''
            try {
              const a = JSON.parse(ev.arguments)
              cmd = a.command || a.cmd || ''
            } catch {
              cmd = ev.arguments
            }
            bubble.tools.push({ name: ev.name, cmd, callID: ev.call_id || '', done: false })
            return
          }
          if (ev.type === 'tool_result') {
            setPhase(bubble, 'tools')
            bubble.toolResults.push(ev.output || '')
            // Mark the matching tool call completed (fall back to the oldest
            // unfinished one when the id doesn't line up).
            const t =
              bubble.tools.find((x) => x.callID && x.callID === ev.call_id) ||
              bubble.tools.find((x) => !x.done)
            if (t) t.done = true
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

  useEffect(() => {
    loadSessions()
  }, [])

  function statusLine(b: BubbleMsg): string {
    if (b.kind === 'user' || !b.phase) return ''
    const secs = b.phaseAt ? Math.max(0, Math.round((Date.now() - b.phaseAt) / 1000)) : 0
    switch (b.phase) {
      case 'thinking':
        return `正在思考… ${secs}s`
      case 'tools': {
        const n = b.tools.filter((t) => !t.done).length
        return n > 0 ? `正在执行 ${n} 个工具… ${secs}s` : `整理工具结果・思考中… ${secs}s`
      }
      case 'streaming':
        return `正在输出回复… ${secs}s`
      case 'done':
        return '回答完成'
    }
  }

  return (
    <div className="chat-body">
      <div className="session-panel">
        <div className="session-head">
          <div className="session-search">
            <SearchIcon />
            <input value={sessionSearch} onChange={(e) => setSessionSearch(e.target.value)} placeholder="搜索会话" aria-label="搜索会话" />
          </div>
          <button className="new-chat" aria-label="新建会话" onClick={newChat}>
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
              {loadingHistory ? '加载历史中…' : currentSessionId ? '已加载历史 · 继续对话即可' : '尚未开始'}
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
                            <span className="pill accent">执行中…</span>
                          ) : (
                            <span className="pill neutral">已完成</span>
                          )}
                        </div>
                        <div className="tool-body">
                          <span className="mono" style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-all' }}>{t.cmd}</span>
                        </div>
                      </div>
                    ))}
                    {b.toolResults.map((r, ri) => (
                      <div
                        key={'r' + ri}
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
                        {r}
                      </div>
                    ))}
                    {b.text && <div style={{ fontSize: 13.5, lineHeight: 1.7, whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>{b.text}</div>}
                    {b.error && <div style={{ fontSize: 13, color: 'var(--danger)', whiteSpace: 'pre-wrap' }}>⚠ {b.error}</div>}
                  </div>
                </div>
              ))
            ) : (
              <div className="thread-empty">
                <EmptyChatIcon />
                <div className="thread-empty-title">开始新的对话</div>
                <div className="thread-empty-desc">在下方输入需求，CubePilot 会调用平台能力帮你排查、部署或查询资源。</div>
              </div>
            )}
          </div>
        </div>

        <div className="composer">
          <div className="composer-inner">
            <textarea
              ref={inputEl}
              rows={1}
              placeholder="输入指令，例如「查看 GPU 利用率」或「创建一个开发环境」…"
              aria-label="输入消息"
              onInput={autoGrow}
              onKeyDown={(e) => {
                if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
                  e.preventDefault()
                  sendMessage()
                }
              }}
            />
            <button className="send-btn" aria-label="发送" onClick={sendMessage}>
              <SendIcon />
            </button>
          </div>
          <div className="composer-hint">
            支持自然语言操作平台资源 · 输入 <span className="mono">@</span> 引用资源 · 阶段一写操作直放
          </div>
        </div>
      </div>
    </div>
  )
}