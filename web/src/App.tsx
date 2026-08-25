// App shell -- sidebar navigation + topbar + routed view.
import { NavLink, Navigate, Route, Routes, useLocation } from 'react-router-dom'
import ChatView from '@/views/ChatView'
import TasksView from '@/views/TasksView'
import AuditView from '@/views/AuditView'
import AgentView from '@/views/AgentView'
import { useToast } from '@/stores/toast'
import { getCurrentUser } from '@/api/client'

const VIEW_TITLES: Record<string, string> = {
  chat: 'Chat',
  tasks: 'Scheduled Tasks',
  audit: 'Audit',
  agent: 'Agent Config',
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
              <span className="scope">suanova-dev / cubepilot</span>
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
              <input placeholder="Search resources, logs, capabilities..." aria-label="Global search" />
            </div>
            <button className="avatar-btn" aria-label="Account">
              {initials}
            </button>
          </div>
        </header>
        <main className="content">
          <Routes>
            <Route path="/chat" element={<ChatView />} />
            <Route path="/tasks" element={<TasksView />} />
            <Route path="/audit" element={<AuditView />} />
            <Route path="/agent" element={<AgentView />} />
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