<script setup lang="ts">
// Tasks view — task list + reports + templates + create dialog (FR-M4).
import { onMounted, ref } from 'vue'
import { api } from '@/api'
import { getCurrentUser } from '@/api/client'
import type { Report, Task } from '@/api/types'
import { downloadText, fmtDuration, fmtTime } from '@/utils/format'
import { useToastStore } from '@/stores/toast'

const toast = useToastStore()
const user = getCurrentUser()

const tasks = ref<Task[]>([])
const selectedTaskId = ref<string | null>(null)
const reports = ref<Report[]>([])
const running = ref(false)
const tab = ref<'list' | 'templates'>('list')
const dialogOpen = ref(false)
const trigger = ref<'Cron' | 'Manual'>('Cron')

// create dialog form
const form = ref({ name: '', prompt: '', cron: '0 2 * * *', template: 'inspect' })

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

const templates = [
  { name: '集群健康巡检', type: '预置 · 不可删改', output: '结构化报告', skills: 'kubectl · logs · 平台组件', phase: '阶段一', tasks: 1 },
  { name: '推理服务自动验证', type: '预置 · 不可删改', output: '结构化报告', skills: 'InferenceService', phase: '阶段二', tasks: 1 },
  { name: 'GPU 资源利用率日报', type: '自定义', output: '自由文本', skills: 'GPUStack 指标', phase: '阶段二', tasks: 1 },
]

const selectedReport = ref<Report | null>(null)
const reportIndex = ref(0)

function taskKindLabel(t: Task): string {
  return (t.prompt || '').includes('巡检') ? '巡检 · 预置' : '自定义'
}
function statusPill(t: Task): string {
  return t.enabled ? '启用' : '停用'
}
function triggerLabel(t: Task): string {
  return t.schedule && t.schedule.trim() ? '定时 + 手动' : '仅手动'
}

async function loadTasks() {
  try {
    tasks.value = await api.listTasks()
    if (selectedTaskId.value) {
      const still = tasks.value.find((t) => t.id === selectedTaskId.value)
      if (still) await loadReports(selectedTaskId.value)
      else selectedTaskId.value = null
    }
  } catch (e) {
    toast.show('任务加载失败：' + e)
  }
}

async function selectTask(id: string) {
  selectedTaskId.value = id
  await loadReports(id)
}

async function toggleTask(t: Task) {
  await api.toggleTask(t.id)
  toast.show(t.enabled ? '任务已停用' : '任务已启用')
  await loadTasks()
}

async function deleteTask(t: Task) {
  if (!confirm(`删除任务「${t.name}」？（历史报告保留）`)) return
  await api.deleteTask(t.id)
  selectedTaskId.value = null
  toast.show('任务已删除')
  await loadTasks()
}

async function runSelected() {
  if (!selectedTaskId.value) {
    toast.show('请先在列表中选择一个任务')
    return
  }
  await runTaskId(selectedTaskId.value)
}

async function runTaskId(id: string) {
  if (running.value) return
  running.value = true
  try {
    const data = await api.runTask(id)
    toast.show(data.started ? '任务已触发，正在以创建者身份执行…' : '触发失败')
  } catch (e) {
    toast.show('触发失败：' + e)
  } finally {
    running.value = false
  }
  // Poll for the report as the async run completes.
  for (const ms of [5000, 15000, 30000, 60000]) {
    setTimeout(async () => {
      if (selectedTaskId.value) await loadReports(selectedTaskId.value)
    }, ms)
  }
}

async function loadReports(id: string) {
  try {
    reports.value = await api.taskReports(id)
  } catch {
    reports.value = []
  }
  reportIndex.value = Math.max(0, Math.min(reportIndex.value, reports.value.length - 1))
  selectedReport.value = reports.value[reportIndex.value] ?? null
}

function openDialog(tplName?: string) {
  dialogOpen.value = true
  if (tplName) {
    const entry = Object.entries(TASK_TEMPLATES).find(([, v]) => v.name === tplName)
    if (entry) applyTemplate(entry[0])
  } else {
    applyTemplate('inspect')
  }
}

function applyTemplate(key: string) {
  const tpl = TASK_TEMPLATES[key] || TASK_TEMPLATES.custom
  form.value.prompt = tpl.prompt
  form.value.cron = tpl.cron
}

async function createTask() {
  const name = form.value.name.trim()
  const prompt = form.value.prompt.trim()
  if (!name) {
    toast.show('请填写任务名称')
    return
  }
  if (!prompt) {
    toast.show('请填写任务提示词')
    return
  }
  const schedule = trigger.value === 'Cron' ? form.value.cron.trim() : ''
  try {
    const task = await api.createTask({ name, prompt, schedule })
    dialogOpen.value = false
    form.value.name = ''
    toast.show(`任务「${name}」已创建 · 将以你的身份直接执行`)
    await loadTasks()
    selectedTaskId.value = task.id
    await loadReports(task.id)
  } catch (e) {
    toast.show('创建失败：' + e)
  }
}

function exportReport() {
  const r = selectedReport.value
  if (!r) {
    toast.show('暂无报告可导出')
    return
  }
  downloadText('report-' + r.id + '.md', `# ${r.taskName}\n\n时间：${fmtTime(r.startedAt)}\n状态：${r.status}\n\n${r.content || ''}`)
}

onMounted(loadTasks)
</script>

<template>
  <div class="view active">
    <div class="view-head">
      <div>
        <div class="view-title">定时任务</div>
        <div class="view-desc">定时 AI 任务 · 预置 + 自定义模板 · 以创建者身份执行 · 报告可查询（FR-M4）</div>
      </div>
      <button class="btn primary" @click="openDialog()">
        <svg class="icon" viewBox="0 0 24 24"><path d="M12 5v14M5 12h14" /></svg>新建任务
      </button>
    </div>

    <div class="seg">
      <button class="seg-item" :class="{ active: tab === 'list' }" @click="tab = 'list'">任务列表</button>
      <button class="seg-item" :class="{ active: tab === 'templates' }" @click="tab = 'templates'">
        模板 <span class="seg-count">{{ templates.length }}</span>
      </button>
    </div>

    <!-- 任务列表 -->
    <div v-show="tab === 'list'">
      <div class="card" style="margin-bottom: 16px">
        <div class="card-head">
          <span class="card-title">任务列表</span>
          <div style="display: flex; align-items: center; gap: 12px">
            <span class="card-hint">{{ tasks.length }} 个任务 · 报告类型随模板而定</span>
            <button class="btn sm" :disabled="running" @click="runSelected">
              <span v-if="running" class="spin" style="border-top-color: #fff"></span>
              <svg v-else class="icon" viewBox="0 0 24 24"><path d="M6 4l14 8-14 8z" /></svg>
              {{ running ? '执行中…' : '立即执行' }}
            </button>
          </div>
        </div>
        <div style="overflow-x: auto">
          <table class="table">
            <thead>
              <tr>
                <th style="width: 34px"></th>
                <th>任务</th><th>模板</th><th>触发方式</th><th>调度</th><th>状态</th><th>上次执行</th><th>下次执行</th><th>创建者</th><th></th>
              </tr>
            </thead>
            <tbody>
              <tr v-if="!tasks.length">
                <td colspan="10" style="text-align: center; color: var(--muted); padding: 24px">暂无任务 · 点击右上角「新建任务」创建</td>
              </tr>
              <tr
                v-for="t in tasks"
                :key="t.id"
                class="task-row"
                :class="{ selected: selectedTaskId === t.id }"
                @click="selectTask(t.id)"
              >
                <td><span class="task-radio"></span></td>
                <td style="font-weight: 600">{{ t.name }}</td>
                <td>{{ taskKindLabel(t) }}</td>
                <td>{{ triggerLabel(t) }}</td>
                <td>{{ t.schedule && t.schedule.trim() ? t.schedule : '仅手动' }}</td>
                <td><span class="pill" :class="t.enabled ? 'success' : 'neutral'">{{ statusPill(t) }}</span></td>
                <td class="tnum">{{ t.lastRunAt ? fmtTime(t.lastRunAt) : '—' }}</td>
                <td class="tnum">{{ t.nextRunAt ? fmtTime(t.nextRunAt) : '—' }}</td>
                <td>{{ t.creator || '—' }}</td>
                <td style="white-space: nowrap">
                  <button class="btn sm ghost" @click.stop="runTaskId(t.id)">运行</button>
                  <button class="btn sm ghost" @click.stop="toggleTask(t)">{{ t.enabled ? '停用' : '启用' }}</button>
                  <button class="btn sm ghost" @click.stop="deleteTask(t)">删除</button>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>

      <div v-if="selectedTaskId" class="view-head" style="margin-bottom: 12px">
        <div>
          <div class="view-title" style="font-size: 15px">任务报告</div>
          <div class="view-desc">{{ selectedReport ? selectedReport.taskName + ' · ' + (selectedReport.trigger === 'Cron' ? '定时执行' : '手动执行') + ' · 报告为 Agent 真实输出' : '选择任务后展示其执行记录' }}</div>
        </div>
        <div style="display: flex; gap: 8px; align-items: center">
          <select v-model.number="reportIndex" class="input" style="width: auto" aria-label="选择报告运行" @change="selectedReport = reports[reportIndex] ?? null">
            <option v-for="(r, i) in reports" :key="r.id" :value="i">{{ fmtTime(r.startedAt) }} · {{ r.trigger === 'Cron' ? '定时' : (r.trigger === 'Manual' ? '手动' : '巡检') }}{{ r.status === 'failed' ? ' · 失败' : '' }}</option>
          </select>
          <button class="btn" @click="exportReport">
            <svg class="icon" viewBox="0 0 24 24"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4M7 10l5 5 5-5M12 15V3" /></svg>导出报告
          </button>
        </div>
      </div>

      <div v-if="selectedReport" id="report-inspect">
        <div class="stat-grid">
          <div class="stat">
            <div class="stat-top"><span class="stat-label">最近一次执行</span></div>
            <div class="stat-value" style="font-size: 18px">{{ fmtTime(selectedReport.startedAt) }}</div>
            <div class="stat-sub">耗时 {{ fmtDuration(selectedReport.startedAt, selectedReport.finishedAt) }} · {{ selectedReport.status === 'failed' ? '执行失败' : '已完成' }}</div>
          </div>
          <div class="stat">
            <div class="stat-top"><span class="stat-label">异常分级计数</span><span class="pill warn">{{ selectedReport.p0 + selectedReport.p1 + selectedReport.p2 }} 项</span></div>
            <div class="sev-row">
              <span class="sev p0"><b>{{ selectedReport.p0 }}</b> P0 紧急</span>
              <span class="sev p1"><b>{{ selectedReport.p1 }}</b> P1 重要</span>
              <span class="sev p2"><b>{{ selectedReport.p2 }}</b> P2 一般</span>
            </div>
            <div class="stat-sub">来自所选运行报告中的分级计数</div>
          </div>
          <div class="stat">
            <div class="stat-top"><span class="stat-label">执行次数</span></div>
            <div class="stat-value">{{ reports.length }}</div>
            <div class="stat-sub">当前任务累计运行次数</div>
          </div>
          <div class="stat">
            <div class="stat-top"><span class="stat-label">运行状态</span></div>
            <div class="stat-value" style="font-size: 18px" :style="{ color: selectedReport.status === 'failed' ? 'var(--danger)' : 'var(--success)' }">{{ selectedReport.status === 'failed' ? '失败' : '成功' }}</div>
            <div class="stat-sub">触发方式：{{ selectedReport.trigger === 'Cron' ? '定时' : '手动' }}</div>
          </div>
        </div>

        <div class="card">
          <div class="card-head">
            <span class="card-title">报告内容</span>
            <span class="card-hint">{{ fmtTime(selectedReport.startedAt) }} 运行 · {{ selectedReport.status === 'failed' ? '失败' : '成功' }}</span>
          </div>
          <div style="padding: 16px 18px 18px">
            <div class="run-log" style="white-space: pre-wrap">{{ selectedReport.content || '（空报告）' }}</div>
          </div>
        </div>
      </div>
    </div>

    <!-- 模板 -->
    <div v-show="tab === 'templates'">
      <div class="card">
        <div class="card-head">
          <span class="card-title">模板管理</span>
          <div style="display: flex; align-items: center; gap: 12px">
            <span class="card-hint">模板是任务的可复用定义 · 预置模板系统内置不可删改 · 自定义模板阶段二开放</span>
            <button class="btn sm" @click="toast.show('自定义模板创建将于阶段二开放')">新建模板</button>
          </div>
        </div>
        <div style="overflow-x: auto">
          <table class="table">
            <thead><tr><th>模板</th><th>类型</th><th>输出类型</th><th>绑定 Skills</th><th>阶段</th><th>关联任务</th><th>操作</th></tr></thead>
            <tbody>
              <tr v-for="tpl in templates" :key="tpl.name">
                <td><div style="display: flex; align-items: center; gap: 7px"><span style="font-weight: 600">{{ tpl.name }}</span></div></td>
                <td>
                  <span v-if="tpl.type.startsWith('预置')" class="lock-badge">
                    <svg class="icon" viewBox="0 0 24 24"><rect x="4" y="11" width="16" height="10" rx="2" /><path d="M8 11V7a4 4 0 0 1 8 0v4" /></svg>{{ tpl.type }}
                  </span>
                  <span v-else class="pill neutral">{{ tpl.type }}</span>
                </td>
                <td>{{ tpl.output }}</td>
                <td class="mono">{{ tpl.skills }}</td>
                <td><span class="pill" :class="tpl.phase === '阶段一' ? 'accent' : 'warn'">{{ tpl.phase }}</span></td>
                <td class="tnum">{{ tpl.tasks }}</td>
                <td><button class="btn sm ghost" @click="openDialog(tpl.name)">基于此创建任务</button></td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>
    </div>

    <!-- 新建任务弹窗 -->
    <div v-if="dialogOpen" class="modal-overlay open" role="dialog" aria-modal="true" @mousedown.self="dialogOpen = false">
      <div class="modal">
        <div class="modal-head">
          <span class="modal-title">新建定时任务</span>
          <button class="modal-close" aria-label="关闭" @click="dialogOpen = false">
            <svg class="icon" viewBox="0 0 24 24"><path d="M18 6L6 18M6 6l12 12" /></svg>
          </button>
        </div>
        <div class="modal-body">
          <div class="field" style="margin-bottom: 0">
            <label class="label">任务名称</label>
            <input v-model="form.name" class="input" placeholder="例如：生产命名空间巡检" aria-label="任务名称" />
          </div>
          <div>
            <label class="label">任务模板</label>
            <select v-model="form.template" class="input" aria-label="任务模板" @change="applyTemplate(form.template)">
              <option value="inspect" selected>集群健康巡检（预置）</option>
              <option value="gpu-daily">GPU 资源利用率日报（自定义）</option>
              <option value="custom">自定义</option>
            </select>
          </div>
          <div>
            <label class="label">任务提示词（AI 执行内容，可编辑）</label>
            <textarea v-model="form.prompt" class="input" rows="5" aria-label="任务提示词"></textarea>
          </div>
          <div>
            <label class="label">触发方式</label>
            <div class="radio-row" role="radiogroup" aria-label="触发方式">
              <button type="button" class="radio" :class="{ active: trigger === 'Cron' }" role="radio" :aria-checked="trigger === 'Cron'" @click="trigger = 'Cron'">定时</button>
              <button type="button" class="radio" :class="{ active: trigger === 'Manual' }" role="radio" :aria-checked="trigger === 'Manual'" @click="trigger = 'Manual'">手动</button>
            </div>
          </div>
          <div v-if="trigger === 'Cron'" class="field" style="margin-bottom: 0">
            <label class="label">调度时间（Cron）· 留空 = 仅手动</label>
            <input v-model="form.cron" class="input mono" aria-label="调度时间" />
          </div>
          <div class="notice">
            <svg class="icon" viewBox="0 0 24 24"><path d="M10.29 3.86 1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0zM12 9v4M12 17h.01" /></svg>
            <span>该任务将以 <b>你（{{ user }}）</b> 的身份直接执行，阶段一读写直放、<b>不再二次确认</b>；无权限项执行时将被拒绝并标注。</span>
          </div>
        </div>
        <div class="modal-foot">
          <button class="btn" @click="dialogOpen = false">取消</button>
          <button class="btn primary" @click="createTask">创建任务</button>
        </div>
      </div>
    </div>
  </div>
</template>
