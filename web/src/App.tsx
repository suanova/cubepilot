// App shell -- sidebar navigation + topbar + routed view.
import { useEffect, useState } from 'react'
import { Link, NavLink, Navigate, Route, Routes, useLocation } from 'react-router-dom'
import ChatView from '@/views/ChatView'
import TasksView from '@/views/TasksView'
import AuditView from '@/views/AuditView'
import AgentView from '@/views/AgentView'
import PublishView from '@/views/PublishView'
import { useToast } from '@/stores/toast'
import { api } from '@/api'
import { getCurrentUser } from '@/api/client'

const VIEW_TITLES: Record<string, string> = {
  chat: 'Chat',
  tasks: 'Scheduled Tasks',
  publish: 'Publisher',
  agent: 'Agent Config',
  audit: 'Audit',
}

function BucketIcon() {
  return (
    <svg className="brand-mark" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinejoin="round">
      <path d="M12 2.5 21 7v10l-9 4.5L3 17V7z" />
      <path d="M3.3 7 12 11.3 20.7 7M12 11.3v9.7" />
    </svg>
  )
}

function ChatIcon() {
  return (
    <svg className="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round">
      <path d="M21 11.5a8.38 8.38 0 0 1-.9 3.8 8.5 8.5 0 0 1-7.6 4.7 8.38 8.38 0 0 1-3.8-.9L3 21l1.9-5.7a8.38 8.38 0 0 1-.9-3.8 8.5 8.5 0 0 1 4.7-7.6 8.38 8.38 0 0 1 3.8-.9h.5a8.48 8.48 0 0 1 8 8v.5z" />
    </svg>
  )
}

function TasksIcon() {
  return (
    <svg className="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round">
      <circle cx="12" cy="12" r="9" />
      <path d="M12 7v5l3 3" />
    </svg>
  )
}

function AgentIcon() {
  return (
    <svg className="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round">
      <rect x="8" y="8" width="8" height="8" rx="1.5" />
      <path d="M8 5V3M12 5V3M16 5V3M8 21v-2M12 21v-2M16 21v-2M3 8h2M3 12h2M3 16h2M19 8h2M19 12h2M19 16h2" />
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

function CheckIcon() {
  return (
    <svg className="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round">
      <path d="M20 6L9 17l-5-5" />
    </svg>
  )
}

function SkillIcon() {
  return (
    <svg className="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round">
      <path d="M12 2 3 7l9 5 9-5-9-5zM3 12l9 5 9-5M3 17l9 5 9-5" />
    </svg>
  )
}

export default function App() {
  const { visible: toastVisible, message: toastMessage } = useToast()
  const { pathname } = useLocation()
  const user = getCurrentUser()
  const initials = user
    .split(/[._-]/)
    .map((p) => p[0]?.toUpperCase() ?? '')
    .slice(0, 2)
    .join('')

  // Derive the active view title from the path ("/xxx" -> "xxx").
  const segment = pathname.split('/')[1] || 'chat'
  const title = VIEW_TITLES[segment] ?? 'CubePilot'

  const navCls = ({ isActive }: { isActive: boolean }) => (isActive ? 'nav-item active' : 'nav-item')

  // Global agent-readiness nudge (issue #117): while the caller's agent is not
  // Ready or has no usable LLM (ModelConfigured=False), steer every view to
  // Agent Config. Hidden on /agent itself -- that is where the user fixes it.
  const [instHint, setInstHint] = useState<{ phase?: string; modelConfigured?: boolean } | null>(null)
  useEffect(() => {
    let stop = false
    const poll = async () => {
      try {
        const list = await api.listInstances()
        if (stop) return
        // listInstances is already scoped to the caller; only the caller's own
        // instance should drive this nudge.
        const own = list.find((i) => (i.spec as Record<string, unknown> | undefined)?.owner === user)
        if (!own) {
          setInstHint(null)
          return
        }
        const st = (own.status ?? {}) as { phase?: string; conditions?: Array<{ type?: string; status?: string }> }
        const cond = st.conditions?.find((c) => c.type === 'ModelConfigured')
        setInstHint({ phase: st.phase, modelConfigured: cond ? cond.status === 'True' : undefined })
      } catch {
        /* transient - keep the last hint */
      }
    }
    void poll()
    const ticker = setInterval(poll, 5000)
    return () => {
      stop = true
      clearInterval(ticker)
    }
  }, [user])

  const onAgentPage = segment === 'agent'
  // Every non-ready state gets a single "Go to Agent Config" call to action.
  const readinessNudge = (() => {
    if (onAgentPage || !instHint) return null
    if (instHint.modelConfigured === false) {
      return { text: 'Your agent has no LLM configured, so it cannot answer yet.' }
    }
    if (instHint.phase && instHint.phase !== 'Ready') {
      return { text: `Agent is not ready yet (${instHint.phase}); it will become ready shortly.` }
    }
    return null
  })()

  return (
    <div className="app">
      <aside className="sidebar">
        <div className="brand">
          <BucketIcon />
          <div className="brand-text">
            <span className="brand-name">CubeStack</span>
            <span className="brand-sub">CubePilot Intelligent Assistant</span>
          </div>
        </div>
        <nav className="nav">
          <NavLink to="/chat" className={navCls}>
            <ChatIcon />
            <span>Chat</span>
          </NavLink>
          <NavLink to="/tasks" className={navCls}>
            <TasksIcon />
            <span>Scheduled Tasks</span>
          </NavLink>
          {/* Audit entry temporarily hidden (M5, restore once real data exists) */}
          <NavLink to="/publish" className={navCls}>
            <SkillIcon />
            <span>Publisher</span>
          </NavLink>
          <NavLink to="/agent" className={navCls}>
            <AgentIcon />
            <span>Agent Config</span>
          </NavLink>
        </nav>
        <div className="sidebar-foot">
          <div className="user">
            <div className="avatar">{initials}</div>
            <div className="user-meta">
              <span className="name">{user}</span>
            </div>
          </div>
        </div>
      </aside>

      <div className="main">
        <header className="topbar">
          <div className="topbar-title">
            <strong>{title}</strong>
          </div>
          <div className="topbar-actions">
            <div className="search">
              <SearchIcon />
              <input placeholder="Search resources, logs, skills..." aria-label="Global search" />
            </div>
            <button className="avatar-btn" aria-label="Account">
              {initials}
            </button>
          </div>
        </header>
        {readinessNudge && (
          <div className="agent-nudge">
            <span>{readinessNudge.text}</span>
            <Link className="agent-nudge-link" to="/agent">
              Go to Agent Config -&gt;
            </Link>
          </div>
        )}
        <main className="content">
          <Routes>
            <Route path="/chat" element={<ChatView />} />
            <Route path="/tasks" element={<TasksView />} />
            <Route path="/audit" element={<AuditView />} />
            <Route path="/agent" element={<AgentView />} />
            <Route path="/publish" element={<PublishView />} />
            <Route path="/" element={<Navigate to="/chat" replace />} />
            <Route path="*" element={<Navigate to="/chat" replace />} />
          </Routes>
        </main>
      </div>

      <div className={`toast ${toastVisible ? 'show' : ''}`} role="status">
        <CheckIcon />
        {toastMessage}
      </div>
    </div>
  )
}