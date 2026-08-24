// Tasks view — task list + reports + templates + create dialog (FR-M4).
import { useEffect, useRef, useState } from 'react'
import { api } from '@/api'
import { getCurrentUser } from '@/api/client'
import type { Report, Task } from '@/api/types'
import { downloadText, fmtDuration, fmtTime } from '@/utils/format'
import { showToast } from '@/stores/toast'

const TASK_TEMPLATES: Record<string, { name: string; cron: string; prompt: string }> = {
  inspect: {
    name: '集群健康巡检',
    cron: '0 2 * * *',
    prompt:
      '请对当前 Kubernetes 集群执行一次基础巡检：\n1. 查看节点状态（kubectl get nodes）；\n2. 查找所有命名空间中状态异常的 Pod（非 Running，如 CrashLoopBackOff / Pending / ImagePullBackOff / OOMKilled）；\n3. 查看最近的集群事件（kubectl get events -A）。\n将发现的异常按严重程度分级：P0 紧急 / P1 重要 / P2 一般，并用简体中文输出一份结构化巡检报告（含每项的证据）。',
  },
  'gpu-daily': {
    name: 'GPU 资源利用率日报',
    cron: '0 9 * * 1',
    prompt:
      '请汇总集群 GPU 资源情况：\n1. kubectl get nodes -o json 查看各节点 nvidia.com/gpu capacity 与 allocatable；\n2. 列出请求 GPU 的 Pod 及其节点分布；\n3. 输出一份 GPU 资源日报（简体中文），标注利用率偏高/闲置的节点，按 P0/P1/P2 分级。',
  },
  custom: { name: '自定义任务', cron: '', prompt: '' },
}

const TEMPLATES = [
  { name: '集群健康巡检', type: '预置 · 不可删改', output: '结构化报告', skills: 'kubectl · logs · 平台组件', phase: '阶段一', tasks: 1 },
  { name: '推理服务自动验证', type: '预置 · 不可删改', output: '结构化报告', skills: 'InferenceService', phase: '阶段二', tasks: 1 },
  { name: 'GPU 资源利用率日报', type: '自定义', output: '自由文本', skills: 'GPUStack 指标', phase: '阶段二', tasks: 1 },
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
    return (t.prompt || '').includes('巡检') ? '巡检 · 预置' : '自定义'
  }
  function statusPill(t: Task): string {
    return t.enabled ? '启用' : '停用'
  }
  function triggerLabel(t: Task): string {
    return t.schedule && t.schedule.trim() ? '定时 + 手动' : '仅手动'
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
      showToast('任务加载失败：' + e)
    }
  }

  async function selectTask(id: string) {
    setSelectedTaskId(id)
    await loadReports(id)
  }

  async function toggleTask(t: Task) {
    await api.toggleTask(t.id)
    showToast(t.enabled ? '任务已停用' : '任务已启用')
    await loadTasks()
  }

  async function deleteTask(t: Task) {
    if (!confirm(`删除任务「${t.name}」？（历史报告保留）`)) return
    await api.deleteTask(t.id)
    setSelectedTaskId(null)
    showToast('任务已删除')
    await loadTasks()
  }

  async function runSelected() {
    if (!selectedTaskId) {
      showToast('请先在列表中选择一个任务')
      return
    }
    await runTaskId(selectedTaskId)
  }

  async function runTaskId(id: string) {
    if (running) return
    setRunning(true)
    try {
      const data = await api.runTask(id)
      showToast(data.started ? '任务已触发，正在以创建者身份执行…' : '触发失败')
    } catch (e) {
      showToast('触发失败：' + e)
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
      showToast('请填写任务名称')
      return
    }
    if (!prompt) {
      showToast('请填写任务提示词')
      return
    }
    const schedule = trigger === 'Cron' ? form.cron.trim() : ''
    try {
      const task = await api.createTask({ name, prompt, schedule })
      setDialogOpen(false)
      setForm((f) => ({ ...f, name: '' }))
      showToast(`任务「${name}」已创建 · 将以你的身份直接执行`)
      await loadTasks()
      setSelectedTaskId(task.id)
      await loadReports(task.id)
    } catch (e) {
      showToast('创建失败：' + e)
    }
  }

  function exportReport() {
    const r = selectedReport
    if (!r) {
      showToast('暂无报告可导出')
      return
    }
    downloadText('report-' + r.id + '.md', `# ${r.taskName}\n\n时间：${fmtTime(r.startedAt)}\n状态：${r.status}\n\n${r.content || ''}`)
  }

  return (
    <div className="view active">
      <div className="view-head">
        <div>
          <div className="view-title">定时任务</div>
          <div className="view-desc">定时 AI 任务 · 预置 + 自定义模板 · 以创建者身份执行 · 报告可查询（FR-M4）</div>
        </div>
        <button className="btn primary" onClick={() => openDialog()}>
          <PlusIcon />
          新建任务
        </button>
      </div>

      <div className="seg">
        <button className={`seg-item ${tab === 'list' ? 'active' : ''}`} onClick={() => setTab('list')}>
          任务列表
        </button>
        <button className={`seg-item ${tab === 'templates' ? 'active' : ''}`} onClick={() => setTab('templates')}>
          模板 <span className="seg-count">{TEMPLATES.length}</span>
        </button>
      </div>

      {tab === 'list' && (
        <>
          <div className="card" style={{ marginBottom: 16 }}>
            <div className="card-head">
              <span className="card-title">任务列表</span>
              <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
                <span className="card-hint">{tasks.length} 个任务 · 报告类型随模板而定</span>
                <button className="btn sm" disabled={running} onClick={runSelected}>
                  {running ? <span className="spin" style={{ borderTopColor: '#fff' }} /> : <RunIcon />}
                  {running ? '执行中…' : '立即执行'}
                </button>
              </div>
            </div>
            <div style={{ overflowX: 'auto' }}>
              <table className="table">
                <thead>
                  <tr>
                    <th style={{ width: 34 }} />
                    <th>任务</th>
                    <th>模板</th>
                    <th>触发方式</th>
                    <th>调度</th>
                    <th>状态</th>
                    <th>上次执行</th>
                    <th>下次执行</th>
                    <th>创建者</th>
                    <th />
                  </tr>
                </thead>
                <tbody>
                  {!tasks.length && (
                    <tr>
                      <td colSpan={10} style={{ textAlign: 'center', color: 'var(--muted)', padding: 24 }}>
                        暂无任务 · 点击右上角「新建任务」创建
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
                      <td>{t.schedule && t.schedule.trim() ? t.schedule : '仅手动'}</td>
                      <td>
                        <span className={`pill ${t.enabled ? 'success' : 'neutral'}`}>{statusPill(t)}</span>
                      </td>
                      <td className="tnum">{t.lastRunAt ? fmtTime(t.lastRunAt) : '—'}</td>
                      <td className="tnum">{t.nextRunAt ? fmtTime(t.nextRunAt) : '—'}</td>
                      <td>{t.creator || '—'}</td>
                      <td style={{ whiteSpace: 'nowrap' }}>
                        <button className="btn sm ghost" onClick={(e) => { e.stopPropagation(); runTaskId(t.id) }}>运行</button>
                        <button className="btn sm ghost" onClick={(e) => { e.stopPropagation(); toggleTask(t) }}>{t.enabled ? '停用' : '启用'}</button>
                        <button className="btn sm ghost" onClick={(e) => { e.stopPropagation(); deleteTask(t) }}>删除</button>
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
                <div className="view-title" style={{ fontSize: 15 }}>任务报告</div>
                <div className="view-desc">
                  {selectedReport
                    ? selectedReport.taskName + ' · ' + (selectedReport.trigger === 'Cron' ? '定时执行' : '手动执行') + ' · 报告为 Agent 真实输出'
                    : '选择任务后展示其执行记录'}
                </div>
              </div>
              <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
                <select
                  className="input"
                  style={{ width: 'auto' }}
                  aria-label="选择报告运行"
                  value={reportIndex}
                  onChange={(e) => {
                    const i = Number(e.target.value)
                    setReportIndex(i)
                    setSelectedReport(reports[i] ?? null)
                  }}
                >
                  {reports.map((r, i) => (
                    <option key={r.id} value={i}>
                      {fmtTime(r.startedAt)} · {r.trigger === 'Cron' ? '定时' : r.trigger === 'Manual' ? '手动' : '巡检'}
                      {r.status === 'failed' ? ' · 失败' : ''}
                    </option>
                  ))}
                </select>
                <button className="btn" onClick={exportReport}>
                  <ExportIcon />
                  导出报告
                </button>
              </div>
            </div>
          )}

          {selectedReport && (
            <div id="report-inspect">
              <div className="stat-grid">
                <div className="stat">
                  <div className="stat-top">
                    <span className="stat-label">最近一次执行</span>
                  </div>
                  <div className="stat-value" style={{ fontSize: 18 }}>{fmtTime(selectedReport.startedAt)}</div>
                  <div className="stat-sub">
                    耗时 {fmtDuration(selectedReport.startedAt, selectedReport.finishedAt)} · {selectedReport.status === 'failed' ? '执行失败' : '已完成'}
                  </div>
                </div>
                <div className="stat">
                  <div className="stat-top">
                    <span className="stat-label">异常分级计数</span>
                    <span className="pill warn">{selectedReport.p0 + selectedReport.p1 + selectedReport.p2} 项</span>
                  </div>
                  <div className="sev-row">
                    <span className="sev p0"><b>{selectedReport.p0}</b> P0 紧急</span>
                    <span className="sev p1"><b>{selectedReport.p1}</b> P1 重要</span>
                    <span className="sev p2"><b>{selectedReport.p2}</b> P2 一般</span>
                  </div>
                  <div className="stat-sub">来自所选运行报告中的分级计数</div>
                </div>
                <div className="stat">
                  <div className="stat-top">
                    <span className="stat-label">执行次数</span>
                  </div>
                  <div className="stat-value">{reports.length}</div>
                  <div className="stat-sub">当前任务累计运行次数</div>
                </div>
                <div className="stat">
                  <div className="stat-top">
                    <span className="stat-label">运行状态</span>
                  </div>
                  <div
                    className="stat-value"
                    style={{ fontSize: 18, color: selectedReport.status === 'failed' ? 'var(--danger)' : 'var(--success)' }}
                  >
                    {selectedReport.status === 'failed' ? '失败' : '成功'}
                  </div>
                  <div className="stat-sub">触发方式：{selectedReport.trigger === 'Cron' ? '定时' : '手动'}</div>
                </div>
              </div>

              <div className="card">
                <div className="card-head">
                  <span className="card-title">报告内容</span>
                  <span className="card-hint">{fmtTime(selectedReport.startedAt)} 运行 · {selectedReport.status === 'failed' ? '失败' : '成功'}</span>
                </div>
                <div style={{ padding: '16px 18px 18px' }}>
                  <div className="run-log" style={{ whiteSpace: 'pre-wrap' }}>{selectedReport.content || '（空报告）'}</div>
                </div>
              </div>
            </div>
          )}
        </>
      )}

      {tab === 'templates' && (
        <div className="card">
          <div className="card-head">
            <span className="card-title">模板管理</span>
            <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
              <span className="card-hint">模板是任务的可复用定义 · 预置模板系统内置不可删改 · 自定义模板阶段二开放</span>
              <button className="btn sm" onClick={() => showToast('自定义模板创建将于阶段二开放')}>新建模板</button>
            </div>
          </div>
          <div style={{ overflowX: 'auto' }}>
            <table className="table">
              <thead>
                <tr>
                  <th>模板</th>
                  <th>类型</th>
                  <th>输出类型</th>
                  <th>绑定 Skills</th>
                  <th>阶段</th>
                  <th>关联任务</th>
                  <th>操作</th>
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
                      {tpl.type.startsWith('预置') ? (
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
                      <span className={`pill ${tpl.phase === '阶段一' ? 'accent' : 'warn'}`}>{tpl.phase}</span>
                    </td>
                    <td className="tnum">{tpl.tasks}</td>
                    <td>
                      <button className="btn sm ghost" onClick={() => openDialog(tpl.name)}>基于此创建任务</button>
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
              <span className="modal-title">新建定时任务</span>
              <button className="modal-close" aria-label="关闭" onClick={() => setDialogOpen(false)}>
                <CloseIcon />
              </button>
            </div>
            <div className="modal-body">
              <div className="field" style={{ marginBottom: 0 }}>
                <label className="label">任务名称</label>
                <input
                  className="input"
                  placeholder="例如：生产命名空间巡检"
                  aria-label="任务名称"
                  value={form.name}
                  onChange={(e) => setForm((f) => ({ ...f, name: e.target.value }))}
                />
              </div>
              <div>
                <label className="label">任务模板</label>
                <select
                  className="input"
                  aria-label="任务模板"
                  value={form.template}
                  onChange={(e) => applyTemplate(e.target.value)}
                >
                  <option value="inspect">集群健康巡检（预置）</option>
                  <option value="gpu-daily">GPU 资源利用率日报（自定义）</option>
                  <option value="custom">自定义</option>
                </select>
              </div>
              <div>
                <label className="label">任务提示词（AI 执行内容，可编辑）</label>
                <textarea
                  className="input"
                  rows={5}
                  aria-label="任务提示词"
                  value={form.prompt}
                  onChange={(e) => setForm((f) => ({ ...f, prompt: e.target.value }))}
                />
              </div>
              <div>
                <label className="label">触发方式</label>
                <div className="radio-row" role="radiogroup" aria-label="触发方式">
                  <button
                    type="button"
                    className={`radio ${trigger === 'Cron' ? 'active' : ''}`}
                    role="radio"
                    aria-checked={trigger === 'Cron'}
                    onClick={() => setTrigger('Cron')}
                  >
                    定时
                  </button>
                  <button
                    type="button"
                    className={`radio ${trigger === 'Manual' ? 'active' : ''}`}
                    role="radio"
                    aria-checked={trigger === 'Manual'}
                    onClick={() => setTrigger('Manual')}
                  >
                    手动
                  </button>
                </div>
              </div>
              {trigger === 'Cron' && (
                <div className="field" style={{ marginBottom: 0 }}>
                  <label className="label">调度时间（Cron）· 留空 = 仅手动</label>
                  <input
                    className="input mono"
                    aria-label="调度时间"
                    value={form.cron}
                    onChange={(e) => setForm((f) => ({ ...f, cron: e.target.value }))}
                  />
                </div>
              )}
              <div className="notice">
                <WarnIcon />
                <span>
                  该任务将以 <b>你（{user}）</b> 的身份直接执行，阶段一读写直放、<b>不再二次确认</b>；无权限项执行时将被拒绝并标注。
                </span>
              </div>
            </div>
            <div className="modal-foot">
              <button className="btn" onClick={() => setDialogOpen(false)}>取消</button>
              <button className="btn primary" onClick={createTask}>创建任务</button>
            </div>
          </div>
        </div>
      )}
    </div>
  )
}