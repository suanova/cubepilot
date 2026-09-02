// Tasks view -- task list + reports + templates + create dialog (FR-M4).
// Tasks are either free-form (inline instruction, no template) or bound to a
// real TaskTemplate (templateRef + params) served by GET /api/tasktemplates.
import { useEffect, useRef, useState } from 'react'
import { api } from '@/api'
import { getCurrentUser } from '@/api/client'
import type { Report, Task, TaskTemplate } from '@/api/types'
import { downloadText, fmtDuration, fmtTime } from '@/utils/format'
import { cronDescription, lowercaseFirst } from '@/utils/cron'
import { showToast } from '@/stores/toast'

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

function templateDisplayName(t: TaskTemplate): string {
  return t.spec?.displayName || t.metadata.name
}

// renderInstruction interpolates {{param}} placeholders for the create-dialog
// preview (the server does the authoritative render on save).
function renderInstruction(instruction: string, params: Record<string, string>): string {
  let out = instruction
  for (const [k, v] of Object.entries(params)) {
    out = out.split('{{' + k + '}}').join(v)
  }
  return out
}

function defaultParams(t: TaskTemplate): Record<string, string> {
  const params: Record<string, string> = {}
  for (const p of t.spec?.paramsSchema || []) {
    if (p.default !== undefined) params[p.name] = p.default
  }
  return params
}

export default function TasksView() {
  const user = getCurrentUser()
  const [tasks, setTasks] = useState<Task[]>([])
  const [templates, setTemplates] = useState<TaskTemplate[]>([])
  const [templatesError, setTemplatesError] = useState('')
  const [selectedTaskId, setSelectedTaskId] = useState<string | null>(null)
  const [reports, setReports] = useState<Report[]>([])
  const [running, setRunning] = useState(false)
  const [tab, setTab] = useState<'list' | 'templates'>('list')
  const [dialogOpen, setDialogOpen] = useState(false)
  const [trigger, setTrigger] = useState<'Cron' | 'Manual'>('Cron')
  const [form, setForm] = useState({ name: '', prompt: '', cron: '0 2 * * *', templateRef: '', params: {} as Record<string, string> })
  const [selectedReport, setSelectedReport] = useState<Report | null>(null)
  const [reportIndex, setReportIndex] = useState(0)
  const pollTimeouts = useRef<number[]>([])

  function templateLabel(t: Task): string {
    if (!t.templateRef) return 'Free-form'
    const tpl = templates.find((x) => x.metadata.name === t.templateRef)
    return tpl ? templateDisplayName(tpl) : t.templateRef
  }
  function statusPill(t: Task): string {
    return t.enabled ? 'Enabled' : 'Disabled'
  }
  function triggerLabel(t: Task): string {
    return t.schedule && t.schedule.trim() ? 'Scheduled + Manual' : 'Manual only'
  }
  function scheduleCell(t: Task): string {
    if (!t.schedule || !t.schedule.trim()) return 'Manual only'
    const desc = cronDescription(t.schedule)
    // Stored schedules are server-validated; fall back to the raw value only if
    // the frontend ever fails to describe one.
    return desc.text ?? t.schedule
  }

  useEffect(() => {
    loadTasks()
    loadTemplates()
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

  async function loadTemplates() {
    try {
      setTemplates(await api.listTaskTemplates())
      setTemplatesError('')
    } catch (e) {
      console.error('loadTemplates', e)
      setTemplatesError(e instanceof Error ? e.message : String(e))
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

  function resetForm() {
    setForm({ name: '', prompt: '', cron: '0 2 * * *', templateRef: '', params: {} })
  }

  // openDialog defaults to free-form (no template); passing a template name
  // (from the Templates tab) pre-binds it.
  function openDialog(templateName?: string) {
    resetForm()
    setTrigger('Cron')
    setDialogOpen(true)
    if (templateName) onChangeTemplate(templateName)
  }

  function onChangeTemplate(templateName: string) {
    if (!templateName) {
      // Back to free-form: clear the binding but keep name/prompt/cron.
      setForm((f) => ({ ...f, templateRef: '', params: {} }))
      return
    }
    const tpl = templates.find((x) => x.metadata.name === templateName)
    if (!tpl) return
    const params = defaultParams(tpl)
    setForm((f) => ({
      ...f,
      templateRef: templateName,
      params,
      cron: tpl.spec?.defaultCron || f.cron || '0 2 * * *',
    }))
  }

  function setParam(name: string, value: string) {
    setForm((f) => ({ ...f, params: { ...f.params, [name]: value } }))
  }

  async function createTask() {
    const name = form.name.trim()
    if (!name) {
      showToast('Please enter a task name')
      return
    }
    const activeTemplate = templates.find((x) => x.metadata.name === form.templateRef)
    if (!activeTemplate && !form.prompt.trim()) {
      showToast('Please enter a task prompt')
      return
    }
    if (trigger === 'Cron') {
      const cron = cronDescription(form.cron)
      if (!form.cron.trim()) {
        // An empty schedule silently becomes Manual server-side; require an
        // explicit choice so the form matches the saved task.
        showToast('Enter a schedule, or switch the trigger to Manual')
        return
      }
      if (cron.error) {
        showToast(cron.error)
        return
      }
    }
    const schedule = trigger === 'Cron' ? form.cron.trim() : ''
    try {
      // Template-bound: send templateRef + params (the server renders the
      // instruction and defaults the schedule from the template). Free-form:
      // send the raw prompt, exactly as before.
      // Manual sends an explicit empty schedule so the server does NOT inherit
      // the template's defaultCron (only a fully omitted schedule does, which
      // the UI never sends); Cron sends the entered expression.
      const payload = activeTemplate
        ? { name, schedule, templateRef: activeTemplate.metadata.name, params: form.params }
        : { name, prompt: form.prompt.trim(), schedule }
      const task = await api.createTask(payload)
      setDialogOpen(false)
      resetForm()
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

  const activeTemplate = templates.find((x) => x.metadata.name === form.templateRef)
  const previewPrompt = activeTemplate
    ? renderInstruction(activeTemplate.spec?.instruction || '', form.params)
    : form.prompt

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
          Templates <span className="seg-count">{templates.length}</span>
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
                      <td>{templateLabel(t)}</td>
                      <td>{triggerLabel(t)}</td>
                      <td title={t.schedule && t.schedule.trim() ? `Cron: ${t.schedule.trim()}` : undefined}>{scheduleCell(t)}</td>
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
              <span className="card-hint">TaskTemplates are reusable "what to do" definitions - a task binds one by name (or is free-form)</span>
            </div>
          </div>
          <div style={{ overflowX: 'auto' }}>
            <table className="table">
              <thead>
                <tr>
                  <th>Template</th>
                  <th>Description</th>
                  <th>Default Cron</th>
                  <th>Actions</th>
                </tr>
              </thead>
              <tbody>
                {templatesError ? (
                  <tr>
                    <td colSpan={4} style={{ color: 'var(--danger)', padding: 16 }}>Failed to load templates: {templatesError}</td>
                  </tr>
                ) : !templates.length ? (
                  <tr>
                    <td colSpan={4} style={{ textAlign: 'center', color: 'var(--muted)', padding: 24 }}>
                      No task templates yet
                    </td>
                  </tr>
                ) : (
                  templates.map((tpl) => (
                    <tr key={tpl.metadata.name}>
                      <td>
                        <div style={{ display: 'flex', alignItems: 'center', gap: 7 }}>
                          <span className="lock-badge">
                            <LockIcon />
                            {templateDisplayName(tpl)}
                          </span>
                        </div>
                      </td>
                      <td style={{ fontSize: 12.5, color: 'var(--muted)' }}>
                        {tpl.spec?.description || (tpl.spec?.paramsSchema || []).map((p) => p.name).join(', ')}
                      </td>
                      <td className="mono">{tpl.spec?.defaultCron || '-'}</td>
                      <td>
                        <button className="btn sm ghost" onClick={() => openDialog(tpl.metadata.name)}>Create task from this</button>
                      </td>
                    </tr>
                  ))
                )}
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
                  value={form.templateRef}
                  onChange={(e) => onChangeTemplate(e.target.value)}
                >
                  <option value="">No template (free-form)</option>
                  {templates.map((tpl) => (
                    <option key={tpl.metadata.name} value={tpl.metadata.name}>
                      {templateDisplayName(tpl)}
                    </option>
                  ))}
                </select>
                {templatesError && (
                  <div style={{ marginTop: 4, fontSize: 12, color: 'var(--danger)' }}>Templates unavailable: {templatesError}</div>
                )}
              </div>
              {!activeTemplate && (
                <div>
                  <label className="label">Task Prompt (AI execution content)</label>
                  <textarea
                    className="input"
                    rows={5}
                    aria-label="Task prompt"
                    placeholder="e.g. Check node readiness and abnormal Pods; grade findings P0/P1/P2"
                    value={form.prompt}
                    onChange={(e) => setForm((f) => ({ ...f, prompt: e.target.value }))}
                  />
                </div>
              )}
              {activeTemplate && (
                <>
                  <div>
                    <label className="label">Parameters - {templateDisplayName(activeTemplate)}</label>
                    {(activeTemplate.spec?.paramsSchema || []).length === 0 ? (
                      <div style={{ fontSize: 12.5, color: 'var(--muted)' }}>
                        This template takes no parameters.
                      </div>
                    ) : (
                      (activeTemplate.spec?.paramsSchema || []).map((p) => (
                        <div key={p.name} style={{ marginBottom: 8 }}>
                          <label className="label" style={{ textTransform: 'capitalize' }}>{p.name}</label>
                          {p.enum && p.enum.length ? (
                            <select
                              className="input"
                              aria-label={p.name}
                              value={form.params[p.name] ?? p.default ?? ''}
                              onChange={(e) => setParam(p.name, e.target.value)}
                            >
                              {(p.enum || []).map((opt) => (
                                <option key={opt} value={opt}>{opt}</option>
                              ))}
                            </select>
                          ) : (
                            <input
                              className="input"
                              aria-label={p.name}
                              value={form.params[p.name] ?? p.default ?? ''}
                              onChange={(e) => setParam(p.name, e.target.value)}
                            />
                          )}
                        </div>
                      ))
                    )}
                  </div>
                  <div>
                    <label className="label">Prompt (rendered from template - not editable)</label>
                    <div className="run-log" style={{ whiteSpace: 'pre-wrap', maxHeight: 160, overflowY: 'auto' }}>
                      {previewPrompt || '(empty template instruction)'}
                    </div>
                  </div>
                </>
              )}
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
              {trigger === 'Cron' &&
                (() => {
                  const cron = cronDescription(form.cron)
                  return (
                    <div className="field" style={{ marginBottom: 0 }}>
                      <label className="label">Schedule (Cron, 5 fields)</label>
                      <input
                        className="input mono"
                        aria-label="Schedule"
                        value={form.cron}
                        onChange={(e) => setForm((f) => ({ ...f, cron: e.target.value }))}
                      />
                      {cron.error ? (
                        <div style={{ marginTop: 6, fontSize: 12, color: 'var(--danger)' }}>{cron.error}</div>
                      ) : cron.text ? (
                        <div style={{ marginTop: 6, fontSize: 12, color: 'var(--success)' }}>
                          Runs {lowercaseFirst(cron.text)}
                        </div>
                      ) : (
                        <div style={{ marginTop: 6, fontSize: 12, color: 'var(--muted)' }}>
                          No schedule set - enter one, or switch the trigger to Manual for a manual-only task
                        </div>
                      )}
                      {activeTemplate?.spec?.defaultCron && cron.text && (
                        <div style={{ marginTop: 2, fontSize: 11, color: 'var(--muted)' }}>
                          Template default: {activeTemplate.spec.defaultCron}
                        </div>
                      )}
                    </div>
                  )
                })()}
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
