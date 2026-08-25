// Audit view -- tool invocation ledger with filters + CSV export (M5).
import { useEffect, useMemo, useState } from 'react'
import { api } from '@/api'
import type { AuditEntry } from '@/api/types'
import { downloadText, esc, fmtTime, shortSession } from '@/utils/format'
import { showToast } from '@/stores/toast'

function ExportIcon() {
  return (
    <svg className="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round">
      <path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4M7 10l5 5 5-5M12 15V3" />
    </svg>
  )
}

export default function AuditView() {
  const [entries, setEntries] = useState<AuditEntry[]>([])
  const [fUser, setFUser] = useState('')
  const [fTool, setFTool] = useState('')
  const [fLevel, setFLevel] = useState('')
  const [fStatus, setFStatus] = useState('')
  const [search, setSearch] = useState('')

  const users = useMemo(() => [...new Set(entries.map((e) => e.user))], [entries])
  const tools = useMemo(() => [...new Set(entries.map((e) => e.tool))], [entries])

  const filtered = useMemo(() => {
    const q = search.trim().toLowerCase()
    return entries.filter((e) => {
      if (fUser && e.user !== fUser) return false
      if (fTool && e.tool !== fTool) return false
      if (fLevel && e.level !== fLevel) return false
      if (fStatus && e.status !== fStatus) return false
      if (q) {
        const s = [e.user, e.sessionId, e.tool, e.command, e.level, e.status].join(' ').toLowerCase()
        if (!s.includes(q)) return false
      }
      return true
    })
  }, [entries, search, fUser, fTool, fLevel, fStatus])

  async function loadAudit() {
    try {
      setEntries(await api.listAudit(400))
    } catch (e) {
      showToast('Audit load failed: ' + e)
    }
  }

  function exportAuditCSV() {
    if (!entries.length) {
      showToast('No audit records to export')
      return
    }
    const rows = [['Time', 'Operator', 'Session', 'Tool', 'Command', 'Level', 'Status']].concat(
      entries.map((e) => [
        new Date(e.ts).toISOString(),
        e.user,
        e.sessionId,
        e.tool,
        e.command,
        e.level,
        e.status,
      ]),
    )
    const csv = rows
      .map((r) => r.map((c) => '"' + String(c || '').replace(/"/g, '""') + '"').join(','))
      .join('\n')
    downloadText('cubepilot-audit.csv', '\ufeff' + csv)
  }

  useEffect(() => {
    loadAudit()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  return (
    <div className="view active">
      <div className="view-head">
        <div>
          <div className="view-title">Audit</div>
          <div className="view-desc">Real-time tool invocation ledger (M5) - filter by user / tool / level - L0 read-only pass-through / L1 write operations</div>
        </div>
        <button className="btn primary" onClick={exportAuditCSV}>
          <ExportIcon />
          Export CSV
        </button>
      </div>
      <div className="card">
        <div className="card-pad audit-filters">
          <select className="input" aria-label="Operator" value={fUser} onChange={(e) => setFUser(e.target.value)}>
            <option value="">All operators</option>
            {users.map((u) => (
              <option key={u} value={u}>
                {u}
              </option>
            ))}
          </select>
          <select className="input" aria-label="Tool" value={fTool} onChange={(e) => setFTool(e.target.value)}>
            <option value="">All tools</option>
            {tools.map((t) => (
              <option key={t} value={t}>
                {t}
              </option>
            ))}
          </select>
          <select className="input" aria-label="Level" value={fLevel} onChange={(e) => setFLevel(e.target.value)}>
            <option value="">All levels</option>
            <option value="L0">L0 - Read-only</option>
            <option value="L1">L1 - Write operation</option>
          </select>
          <select className="input" aria-label="Status" value={fStatus} onChange={(e) => setFStatus(e.target.value)}>
            <option value="">All statuses</option>
            <option value="executed">Executed</option>
            <option value="failed">Failed</option>
          </select>
          <div className="search grow">
            <svg className="icon" style={{ width: 14, height: 14 }} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round">
              <circle cx="11" cy="11" r="8" />
              <path d="M21 21l-4.35-4.35" />
            </svg>
            <input value={search} onChange={(e) => setSearch(e.target.value)} placeholder="Search session / args..." aria-label="Search audit records" />
          </div>
        </div>
        <div style={{ overflowX: 'auto' }}>
          <table className="table">
            <thead>
              <tr>
                <th>Time</th>
                <th>Operator</th>
                <th>Session</th>
                <th>Tool</th>
                <th>Args Summary</th>
                <th>Level</th>
                <th>Status</th>
                <th>Result</th>
              </tr>
            </thead>
            <tbody>
              {!filtered.length && (
                <tr>
                  <td colSpan={8} style={{ textAlign: 'center', color: 'var(--muted)', padding: 24 }}>
                    {entries.length ? 'No matching records' : 'No audit records yet - they are written automatically after a chat or inspection run'}
                  </td>
                </tr>
              )}
              {filtered.map((e) => (
                <tr key={e.id}>
                  <td className="mono tnum">{fmtTime(e.ts)}</td>
                  <td>{esc(e.user)}</td>
                  <td className="mono">{shortSession(e.sessionId)}</td>
                  <td className="mono">{esc(e.tool)}</td>
                  <td className="mono" style={{ maxWidth: 320, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                    {esc(e.command || '')}
                  </td>
                  <td>
                    <span className={`pill ${e.level === 'L0' ? 'accent' : 'warn'}`}>{esc(e.level)}</span>
                  </td>
                  <td>
                    <span className={`pill ${e.status === 'executed' ? 'success' : 'danger'}`}>
                      {e.status === 'executed' ? 'Executed' : 'Failed'}
                    </span>
                  </td>
                  <td className="tnum" style={{ color: 'var(--muted)' }}>
                    {e.status === 'executed' ? 'OK' : 'fail'}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </div>
    </div>
  )
}