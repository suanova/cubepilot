<script setup lang="ts">
// Audit view — tool invocation ledger with filters + CSV export (M5).
import { computed, onMounted, ref } from 'vue'
import { api } from '@/api'
import type { AuditEntry } from '@/api/types'
import { downloadText, esc, fmtTime, shortSession } from '@/utils/format'
import { useToastStore } from '@/stores/toast'

const toast = useToastStore()

const entries = ref<AuditEntry[]>([])
const fUser = ref('')
const fTool = ref('')
const fLevel = ref('')
const fStatus = ref('')
const search = ref('')

const users = computed(() => [...new Set(entries.value.map((e) => e.user))])
const tools = computed(() => [...new Set(entries.value.map((e) => e.tool))])

const filtered = computed(() => {
  const q = search.value.trim().toLowerCase()
  return entries.value.filter((e) => {
    if (fUser.value && e.user !== fUser.value) return false
    if (fTool.value && e.tool !== fTool.value) return false
    if (fLevel.value && e.level !== fLevel.value) return false
    if (fStatus.value && e.status !== fStatus.value) return false
    if (q) {
      const s = [e.user, e.sessionId, e.tool, e.command, e.level, e.status].join(' ').toLowerCase()
      if (!s.includes(q)) return false
    }
    return true
  })
})

async function loadAudit() {
  try {
    entries.value = await api.listAudit(400)
  } catch (e) {
    toast.show('审计加载失败：' + e)
  }
}

function exportAuditCSV() {
  if (!entries.value.length) {
    toast.show('暂无审计记录可导出')
    return
  }
  const rows = [['时间', '操作者', '会话', '工具', '命令', '级别', '状态']].concat(
    entries.value.map((e) => [
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

onMounted(loadAudit)
</script>

<template>
  <div class="view active">
    <div class="view-head">
      <div>
        <div class="view-title">审计</div>
        <div class="view-desc">工具调用实时记录（M5）· 按用户 / 工具 / 级别筛选 · L0 只读直放 / L1 写操作</div>
      </div>
      <button class="btn primary" @click="exportAuditCSV">
        <svg class="icon" viewBox="0 0 24 24"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4M7 10l5 5 5-5M12 15V3" /></svg>导出 CSV
      </button>
    </div>
    <div class="card">
      <div class="card-pad audit-filters">
        <select v-model="fUser" class="input" aria-label="操作者">
          <option value="">全部操作者</option>
          <option v-for="u in users" :key="u" :value="u">{{ u }}</option>
        </select>
        <select v-model="fTool" class="input" aria-label="工具">
          <option value="">全部工具</option>
          <option v-for="t in tools" :key="t" :value="t">{{ t }}</option>
        </select>
        <select v-model="fLevel" class="input" aria-label="级别">
          <option value="">全部级别</option>
          <option value="L0">L0 · 只读</option>
          <option value="L1">L1 · 写操作</option>
        </select>
        <select v-model="fStatus" class="input" aria-label="状态">
          <option value="">全部状态</option>
          <option value="executed">已执行</option>
          <option value="failed">失败</option>
        </select>
        <div class="search grow">
          <svg class="icon" style="width: 14px; height: 14px" viewBox="0 0 24 24"><circle cx="11" cy="11" r="8" /><path d="M21 21l-4.35-4.35" /></svg>
          <input v-model="search" placeholder="搜索会话 / 参数…" aria-label="搜索审计记录" />
        </div>
      </div>
      <div style="overflow-x: auto">
        <table class="table">
          <thead>
            <tr><th>时间</th><th>操作者</th><th>会话</th><th>工具</th><th>参数摘要</th><th>级别</th><th>状态</th><th>结果</th></tr>
          </thead>
          <tbody>
            <tr v-if="!filtered.length">
              <td colspan="8" style="text-align: center; color: var(--muted); padding: 24px">
                {{ entries.length ? '无匹配记录' : '暂无审计记录 · 发起一次对话/巡检后自动写入' }}
              </td>
            </tr>
            <tr v-for="e in filtered" :key="e.id">
              <td class="mono tnum">{{ fmtTime(e.ts) }}</td>
              <td>{{ esc(e.user) }}</td>
              <td class="mono">{{ shortSession(e.sessionId) }}</td>
              <td class="mono">{{ esc(e.tool) }}</td>
              <td class="mono" style="max-width: 320px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap">{{ esc(e.command || '') }}</td>
              <td><span class="pill" :class="e.level === 'L0' ? 'accent' : 'warn'">{{ esc(e.level) }}</span></td>
              <td><span class="pill" :class="e.status === 'executed' ? 'success' : 'danger'">{{ e.status === 'executed' ? '已执行' : '失败' }}</span></td>
              <td class="tnum" style="color: var(--muted)">{{ e.status === 'executed' ? 'OK' : 'fail' }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>
  </div>
</template>
