// Chat view -- the session list, with the conversation beside it.
//
// The conversation is not here: it is `useChatThread` and `<ChatThread>`, which
// the floating widget renders too. What is left is the half that is only ever
// this page -- which conversations exist, and which one is on screen.
import { useCallback, useEffect, useState } from 'react'
import { api } from '@/api'
import type { SessionInfo } from '@/api/types'
import { invalidateSessions, useSessionsVersion } from '@/stores/sessions'
import { shortSession } from '@/utils/format'
import { ChatThread } from './chat/ChatThread'
import { useChatThread } from './chat/useChatThread'
import { ChatBubbleIcon, NewChatIcon, SearchIcon } from './chat/icons'

export default function ChatView() {
  const [sessions, setSessions] = useState<SessionInfo[]>([])
  const [sessionSearch, setSessionSearch] = useState('')

  const loadSessions = useCallback(async () => {
    try {
      setSessions(await api.listSessions())
    } catch {
      /* keep whatever we have */
    }
  }, [])

  // A conversation can start in either entry -- this one or the widget -- and
  // the key is minted mid-turn by the server, so whoever started it says so
  // rather than the sidebar guessing. See stores/sessions.ts.
  const sessionsVersion = useSessionsVersion()
  const thread = useChatThread({ onSessionStarted: invalidateSessions })

  // Derived from the list, so it stays here rather than in the hook: the thread
  // knows its key, not what the key is called.
  const chatTitle = (() => {
    if (!thread.sessionId) return 'New conversation'
    const s = sessions.find((x) => x.sessionKey === thread.sessionId)
    return s?.title || shortSession(thread.sessionId)
  })()

  const filteredSessions = (() => {
    const q = sessionSearch.trim().toLowerCase()
    if (!q) return sessions
    return sessions.filter((s) => (s.title || s.sessionKey).toLowerCase().includes(q))
  })()

  useEffect(() => {
    loadSessions()
  }, [loadSessions, sessionsVersion])

  return (
    <div className="chat-body">
      <div className="session-panel">
        <div className="session-head">
          <div className="session-search">
            <SearchIcon />
            <input value={sessionSearch} onChange={(e) => setSessionSearch(e.target.value)} placeholder="Search conversations" aria-label="Search conversations" />
          </div>
          <button className="new-chat" aria-label="New conversation" onClick={thread.newChat}>
            <NewChatIcon />
          </button>
        </div>
        <div className="session-list">
          {filteredSessions.map((s) => (
            <div
              key={s.sessionKey}
              className={`session-item ${thread.sessionId === s.sessionKey ? 'active' : ''}`}
              onClick={() => thread.switchSession(s.sessionKey)}
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

      <ChatThread thread={thread} title={chatTitle} />
    </div>
  )
}
