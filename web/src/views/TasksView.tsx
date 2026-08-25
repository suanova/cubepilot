// Tasks view -- task list + reports + templates + create dialog (FR-M4).
import { useEffect, useRef, useState } from 'react'
import { api } from '@/api'
import { getCurrentUser } from '@/api/client'
import type { Report, Task } from '@/api/types'
import { downloadText, fmtDuration, fmtTime } from '@/utils/format'
import { showToast } from '@/stores/toast'

const TASK_TEMPLATES: Record<string, { name: string; cron: string; prompt: string }> = {
  inspect: {
    name: 'Cluster Health Inspection',
    cron: '0 2 * * *',
    prompt:
      'Run a basic health inspection of the current Kubernetes cluster:\n1. Check node status (kubectl get nodes);\n2. Find abnormal Pods in all namespaces (not Running, e.g. CrashLoopBackOff / Pending / ImagePullBackOff / OOMKilled);\n3. Check the recent cluster events (kubectl get events -A).\nClassify findings by severity: P0 critical / P1 important / P2 minor, and output a structured inspection report in Simplified Chinese (with evidence for each item).',
  },
  'gpu-daily': {
    name: 'GPU Resource Utilization Daily Report',
    cron: '0 9 * * 1',
    prompt:
      'Summarize the cluster GPU resources:\n1. kubectl get nodes -o json to check each node\'s nvidia.com/gpu capacity and allocatable;\n2. List Pods requesting GPU and their node distribution;\n3. Output a GPU resource daily report (Simplified Chinese), flagging nodes with high utilization or idle, classified by P0/P1/P2.',
  },
  custom: { name: 'Custom Task', cron: '', prompt: '' },
}

const TEMPLATES = [
  { name: 'Cluster Health Inspection', type: 'Preset - cannot modify', output: 'Structured report', skills: 'kubectl - logs - platform components', phase: 'Phase One', tasks: 1 },
  { name: 'Inference Service Auto-Verification', type: 'Preset - cannot modify', output: 'Structured report', skills: 'InferenceService', phase: 'Phase Two', tasks: 1 },
  { name: 'GPU Resource Utilization Daily Report', type: 'Custom', output: 'Free text', skills: 'GPUStack metrics', phase: 'Phase Two', tasks: 1 },
]

function PlusIcon() {
  return <svg className="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round"><path d="M12 5v14M5 12h14" /></svg>
}
function CloseIcon() {
  return <svg className="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round"><path d="M18 6L6 18M6 6l12 12" /></svg>
}
function RunIcon() {
  return <svg className="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6"><path d="M6 4l14 8-14 8z" /></svg>
}
function ExportIcon() {
  return <svg className="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4M7 10l5 5 5-5M12 15V3" /></svg>
}
function LockIcon() {
  return <svg className="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round"><rect x="4" y="11" width="16" height="10" rx="2" /><path d="M8 11V7a4 4 0 0 1 8 0v4" /></svg>
}
function WarnIcon() {
  return <svg className="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round"><path d="M10.29 3.86 1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0zM12 9v4M12 17h.01" /></svg>
}

export default function TasksView() {
  const user = getCurrentUser()
  const [tasks, setTasks] = useState<Task[]>([])
  const [selectedTaskId, setSelectedTaskId] = useState<string | null>(null)
  const [reports, setReports] = useState<Report[]>([])
  const [running, setRunning] = useState(false)
  const [tab, setTab] = useState<'list' | 'templates'>('list')
  const [dialogOpen, setDialogOpen] = useState(false)
  const [trigger, setTrigger] = useState<'Cron' | 'Manual'>('Cron')
  const [form, setForm] = useState({ name: '', prompt: '', cron: '0 2 * * *', template: 'inspect' })
  const [selectedReport, setSelectedReport] = useState<Report | null>(null)
  const [reportIndex, setReportIndex] = useState(0)
  const pollTimeouts = useRef<number[]>([])

  function taskKindLabel(t: Task): string {
    return (t.prompt || '').includes('inspection') ? 'Inspection - preset' : 'Custom'
  }
  function statusPill(t: Task): string {
    return t.enabled ? 'Enabled' : 'Disabled'
  }
  function triggerLabel(t: Task): string {
    return t.schedule && t.schedule.trim() ? 'Scheduled + Manual' : 'Manual only'
  }

  useEffect(() => {
    loadTasks()
    return () => {
      pollTimeouts.current.forEach((id) => clearTimeout(id))
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  async function loadTasks() {
    try {
      const list = await api.listTasks()
      setTasks(list)
      if (selectedTaskId) {
        const still = list.find((t) => t.id === selectedTaskId)
        if (still) await loadReports(selectedTaskId)
        else setSelectedTaskId(null)
      }
    } catch (e) {
      showToast('Task load failed: ' + e)
    }
  }

  async function selectTask(id: string) {
    setSelectedTaskId(id)
    await loadReports(id)
  }

  async function toggleTask(t: Task) {
    await api.toggleTask(t.id)
    showToast(t.enabled ? 'Task disabled' : 'Task enabled')
    await loadTasks()
  }

  async function deleteTask(t: Task) {
    if (!confirm(`Delete task "${t.name}"? (historical reports are kept)`)) return
    await api.deleteTask(t.id)
    setSelectedTaskId(null)
    showToast('Task deleted')
    await loadTasks()
  }

  async function runSelected() {
    if (!selectedTaskId) {
      showToast('Please select a task from the list first')
      return
    }
    await runTaskId(selectedTaskId)
  }

  async function runTaskId(id: string) {
    if (running) return
    setRunning(true)
    try {
      const data = await api.runTask(id)
      showToast(data.started ? 'Task triggered - running as the creator...' : 'Trigger failed')
    } catch (e) {
      showToast('Trigger failed: ' + e)
    } finally {
      setRunning(false)
    }
    // Poll for the report as the async run completes.
    for (const ms of [5000, 15000, 30000, 60000]) {
      const t = setTimeout(async () => {
        if (selectedTaskId) await loadReports(selectedTaskId)
      }, ms)
      pollTimeouts.current.push(t)
    }
  }

  async function loadReports(id: string) {
    try {
      const list = await api.taskReports(id)
      setReports(list)
      setReportIndex((prev) => Math.max(0, Math.min(prev, list.length - 1)))
      setSelectedReport(list[Math.min(reportIndex, list.length - 1)] ?? null)
    } catch {
      setReports([])
      setSelectedReport(null)
    }
  }

  function openDialog(tplName?: string) {
    setDialogOpen(true)
    if (tplName) {
      const entry = Object.entries(TASK_TEMPLATES).find(([, v]) => v.name === tplName)
      if (entry) applyTemplate(entry[0])
    } else {
      applyTemplate('inspect')
    }
  }

  function applyTemplate(key: string) {
    const tpl = TASK_TEMPLATES[key] || TASK_TEMPLATES.custom
    setForm((f) => ({ ...f, prompt: tpl.prompt, cron: tpl.cron, template: key }))
  }

  async function createTask() {
    const name = form.name.trim()
    const prompt = form.prompt.trim()
    if (!name) {
      showToast('Please enter a task name')
      return
    }
    if (!prompt) {
      showToast('Please enter a task prompt')
      return
    }
    const schedule = trigger === 'Cron' ? form.cron.trim() : ''
    try {
      const task = await api.createTask({ name, prompt, schedule })
      setDialogOpen(false)
      setForm((f) => ({ ...f, name: '' }))
      showToast(`Task "${name}" created - it will run directly as you`)
      await loadTasks()
      setSelectedTaskId(task.id)
      await loadReports(task.id)
    } catch (e) {
      showToast('Create failed: ' + e)
    }
  }

  function exportReport() {
    const r = selectedReport
    if (!r) {
      showToast('No report to export')
      return
    }
    downloadText('report-' + r.id + '.md', `# ${r.taskName}\n\nTime: ${fmtTime(r.startedAt)}\nStatus: ${r.status}\n\n${r.content || ''}`)
  }

  return (
    <div className="view active">
      <div className="view-head">
        <div>
          <div className="view-title">Scheduled Tasks</div>
          <div className="view-desc">Scheduled AI tasks - preset + custom templates - run as the creator - reports queryable (FR-M4)</div>
        </div>
        <button className="btn primary" onClick={() => openDialog()}>
          <PlusIcon />
          New Task
        </button>
      </div>

      <div className="seg">
        <button className={`seg-item ${tab === 'list' ? 'active' : ''}`} onClick={() => setTab('list')}>
          Task List
        </button>
        <button className={`seg-item ${tab === 'templates' ? 'active' : ''}`} onClick={() => setTab('templates')}>
          Templates <span className="seg-count">{TEMPLATES.length}</span>
        </button>
      </div>

      {tab === 'list' && (
        <>
          <div className="card" style={{ marginBottom: 16 }}>
            <div className="card-head">
              <span className="card-title">Task List</span>
              <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
                <span className="card-hint">{tasks.length} task(s) - report type depends on the template</span>
                <button className="btn sm" disabled={running} onClick={runSelected}>
                  {running ? <span className="spin" style={{ borderTopColor: '#fff' }} /> : <RunIcon />}
                  {running ? 'Running...' : 'Run Now'}
                </button>
              </div>
            </div>
            <div style={{ overflowX: 'auto' }}>
              <table className="table">
                <thead>
                  <tr>
                    <th style={{ width: 34 }} />
                    <th>Task</th>
                    <th>Template</th>
                    <th>Trigger</th>
                    <th>Schedule</th>
                    <th>Status</th>
                    <th>Last Run</th>
                    <th>Next Run</th>
                    <th>Creator</th>
                    <th />
                  </tr>
                </thead>
                <tbody>
                  {!tasks.length && (
                    <tr>
                      <td colSpan={10} style={{ textAlign: 'center', color: 'var(--muted)', padding: 24 }}>
                        No tasks yet - click "New Task" at the top right to create one
                      </td>
                    </tr>
                  )}
                  {tasks.map((t) => (
                    <tr
                      key={t.id}
                      className={`task-row ${selectedTaskId === t.id ? 'selected' : ''}`}
                      onClick={() => selectTask(t.id)}
                    >
                      <td>
                        <span className="task-radio" />
                      </td>
                      <td style={{ fontWeight: 600 }}>{t.name}</td>
                      <td>{taskKindLabel(t)}</td>
                      <td>{triggerLabel(t)}</td>
                      <td>{t.schedule && t.schedule.trim() ? t.schedule : 'Manual only'}</td>
                      <td>
                        <span className={`pill ${t.enabled ? 'success' : 'neutral'}`}>{statusPill(t)}</span>
                      </td>
                      <td className="tnum">{t.lastRunAt ? fmtTime(t.lastRunAt) : '-'}</td>
                      <td className="tnum">{t.nextRunAt ? fmtTime(t.nextRunAt) : '-'}</td>
                      <td>{t.creator || '-'}</td>
                      <td style={{ whiteSpace: 'nowrap' }}>
                        <button className="btn sm ghost" onClick={(e) => { e.stopPropagation(); runTaskId(t.id) }}>Run</button>
                        <button className="btn sm ghost" onClick={(e) => { e.stopPropagation(); toggleTask(t) }}>{t.enabled ? 'Disable' : 'Enable'}</button>
                        <button className="btn sm ghost" onClick={(e) => { e.stopPropagation(); deleteTask(t) }}>Delete</button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </div>

          {selectedTaskId && (
            <div className="view-head" style={{ marginBottom: 12 }}>
              <div>
                <div className="view-title" style={{ fontSize: 15 }}>Task Report</div>
                <div className="view-desc">
                  {selectedReport
                    ? selectedReport.taskName + ' - ' + (selectedReport.trigger === 'Cron' ? 'Scheduled run' : 'Manual run') + ' - report is the Agent real output'
                    : 'Select a task to show its execution records'}
                </div>
              </div>
              <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
                <select
                  className="input"
                  style={{ width: 'auto' }}
                  aria-label="Select report run"
                  value={reportIndex}
                  onChange={(e) => {
                    const i = Number(e.target.value)
                    setReportIndex(i)
                    setSelectedReport(reports[i] ?? null)
                  }}
                >
                  {reports.map((r, i) => (
                    <option key={r.id} value={i}>
                      {fmtTime(r.startedAt)} - {r.trigger === 'Cron' ? 'Scheduled' : r.trigger === 'Manual' ? 'Manual' : 'Inspection'}
                      {r.status === 'failed' ? ' - failed' : ''}
                    </option>
                  ))}
                </select>
                <button className="btn" onClick={exportReport}>
                  <ExportIcon />
                  Export Report
                </button>
              </div>
            </div>
          )}

          {selectedReport && (
            <div id="report-inspect">
              <div className="stat-grid">
                <div className="stat">
                  <div className="stat-top">
                    <span className="stat-label">Last Run</span>
                  </div>
                  <div className="stat-value" style={{ fontSize: 18 }}>{fmtTime(selectedReport.startedAt)}</div>
                  <div className="stat-sub">
                    Duration {fmtDuration(selectedReport.startedAt, selectedReport.finishedAt)} - {selectedReport.status === 'failed' ? 'Failed' : 'Completed'}
                  </div>
                </div>
                <div className="stat">
                  <div className="stat-top">
                    <span className="stat-label">Severity Counts</span>
                    <span className="pill warn">{selectedReport.p0 + selectedReport.p1 + selectedReport.p2} items</span>
                  </div>
                  <div className="sev-row">
                    <span className="sev p0"><b>{selectedReport.p0}</b> P0 Critical</span>
                    <span className="sev p1"><b>{selectedReport.p1}</b> P1 Important</span>
                    <span className="sev p2"><b>{selectedReport.p2}</b> P2 Minor</span>
                  </div>
                  <div className="stat-sub">Severity counts from the selected run report</div>
                </div>
                <div className="stat">
                  <div className="stat-top">
                    <span className="stat-label">Run Count</span>
                  </div>
                  <div className="stat-value">{reports.length}</div>
                  <div className="stat-sub">Total runs of the current task</div>
                </div>
                <div className="stat">
                  <div className="stat-top">
                    <span className="stat-label">Run Status</span>
                  </div>
                  <div
                    className="stat-value"
                    style={{ fontSize: 18, color: selectedReport.status === 'failed' ? 'var(--danger)' : 'var(--success)' }}
                  >
                    {selectedReport.status === 'failed' ? 'Failed' : 'Successful'}
                  </div>
                  <div className="stat-sub">Trigger: {selectedReport.trigger === 'Cron' ? 'Scheduled' : 'Manual'}</div>
                </div>
              </div>

              <div className="card">
                <div className="card-head">
                  <span className="card-title">Report Content</span>
                  <span className="card-hint">{fmtTime(selectedReport.startedAt)} run - {selectedReport.status === 'failed' ? 'Failed' : 'Successful'}</span>
                </div>
                <div style={{ padding: '16px 18px 18px' }}>
                  <div className="run-log" style={{ whiteSpace: 'pre-wrap' }}>{selectedReport.content || '(empty report)'}</div>
                </div>
              </div>
            </div>
          )}
        </>
      )}

      {tab === 'templates' && (
        <div className="card">
          <div className="card-head">
            <span className="card-title">Template Management</span>
            <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
              <span className="card-hint">Templates are reusable task definitions - preset templates are built-in and cannot be modified - custom templates open in phase two</span>
              <button className="btn sm" onClick={() => showToast('Custom template creation opens in phase two')}>New Template</button>
            </div>
          </div>
          <div style={{ overflowX: 'auto' }}>
            <table className="table">
              <thead>
                <tr>
                  <th>Template</th>
                  <th>Type</th>
                  <th>Output Type</th>
                  <th>Bound Skills</th>
                  <th>Phase</th>
                  <th>Linked Tasks</th>
                  <th>Actions</th>
                </tr>
              </thead>
              <tbody>
                {TEMPLATES.map((tpl) => (
                  <tr key={tpl.name}>
                    <td>
                      <div style={{ display: 'flex', alignItems: 'center', gap: 7 }}>
                        <span style={{ fontWeight: 600 }}>{tpl.name}</span>
                      </div>
                    </td>
                    <td>
                      {tpl.type.startsWith('Preset') ? (
                        <span className="lock-badge">
                          <LockIcon />
                          {tpl.type}
                        </span>
                      ) : (
                        <span className="pill neutral">{tpl.type}</span>
                      )}
                    </td>
                    <td>{tpl.output}</td>
                    <td className="mono">{tpl.skills}</td>
                    <td>
                      <span className={`pill ${tpl.phase === 'Phase One' ? 'accent' : 'warn'}`}>{tpl.phase}</span>
                    </td>
                    <td className="tnum">{tpl.tasks}</td>
                    <td>
                      <button className="btn sm ghost" onClick={() => openDialog(tpl.name)}>Create task from this</button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}

      {dialogOpen && (
        <div
          className="modal-overlay open"
          role="dialog"
          aria-modal="true"
          onMouseDown={(e) => {
            if (e.target === e.currentTarget) setDialogOpen(false)
          }}
        >
          <div className="modal">
            <div className="modal-head">
              <span className="modal-title">New Scheduled Task</span>
              <button className="modal-close" aria-label="Close" onClick={() => setDialogOpen(false)}>
                <CloseIcon />
              </button>
            </div>
            <div className="modal-body">
              <div className="field" style={{ marginBottom: 0 }}>
                <label className="label">Task Name</label>
                <input
                  className="input"
                  placeholder="e.g. production namespace inspection"
                  aria-label="Task name"
                  value={form.name}
                  onChange={(e) => setForm((f) => ({ ...f, name: e.target.value }))}
                />
              </div>
              <div>
                <label className="label">Task Template</label>
                <select
                  className="input"
                  aria-label="Task template"
                  value={form.template}
                  onChange={(e) => applyTemplate(e.target.value)}
                >
                  <option value="inspect">Cluster Health Inspection (preset)</option>
                  <option value="gpu-daily">GPU Resource Utilization Daily Report (custom)</option>
                  <option value="custom">Custom</option>
                </select>
              </div>
              <div>
                <label className="label">Task Prompt (AI execution content, editable)</label>
                <textarea
                  className="input"
                  rows={5}
                  aria-label="Task prompt"
                  value={form.prompt}
                  onChange={(e) => setForm((f) => ({ ...f, prompt: e.target.value }))}
                />
              </div>
              <div>
                <label className="label">Trigger</label>
                <div className="radio-row" role="radiogroup" aria-label="Trigger">
                  <button
                    type="button"
                    className={`radio ${trigger === 'Cron' ? 'active' : ''}`}
                    role="radio"
                    aria-checked={trigger === 'Cron'}
                    onClick={() => setTrigger('Cron')}
                  >
                    Scheduled
                  </button>
                  <button
                    type="button"
                    className={`radio ${trigger === 'Manual' ? 'active' : ''}`}
                    role="radio"
                    aria-checked={trigger === 'Manual'}
                    onClick={() => setTrigger('Manual')}
                  >
                    Manual
                  </button>
                </div>
              </div>
              {trigger === 'Cron' && (
                <div className="field" style={{ marginBottom: 0 }}>
                  <label className="label">Schedule (Cron) - empty = manual only</label>
                  <input
                    className="input mono"
                    aria-label="Schedule"
                    value={form.cron}
                    onChange={(e) => setForm((f) => ({ ...f, cron: e.target.value }))}
                  />
                </div>
              )}
              <div className="notice">
                <WarnIcon />
                <span>
                  This task will run directly as <b>you ({user})</b>; phase one allows read/write pass-through with <b>no second confirmation</b>; items without permission are rejected and flagged during execution.
                </span>
              </div>
            </div>
            <div className="modal-foot">
              <button className="btn" onClick={() => setDialogOpen(false)}>Cancel</button>
              <button className="btn primary" onClick={createTask}>Create Task</button>
            </div>
          </div>
        </div>
      )}
    </div>
  )
}