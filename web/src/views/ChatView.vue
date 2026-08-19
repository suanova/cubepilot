<script setup lang="ts">
// Chat view — session list + thread + composer, SSE streaming from /api/messages.
import { computed, nextTick, onMounted, onUnmounted, ref } from 'vue'
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
  thinking: boolean // true while phase !== 'done' (kept for existing template)
  phase?: BubblePhase
  phaseAt?: number // Date.now() when the current phase started
}

const sessions = ref<SessionInfo[]>([])
const sessionSearch = ref('')
const currentSessionId = ref<string | null>(null)
const threadEl = ref<HTMLElement | null>(null)
const inputEl = ref<HTMLTextAreaElement | null>(null)
const bubbles = ref<BubbleMsg[]>([])
const loadingHistory = ref(false)
const chatTitle = computed(() => {
  if (!currentSessionId.value) return '新会话'
  const s = sessions.value.find((x) => x.sessionKey === currentSessionId.value)
  return s?.title || shortSession(currentSessionId.value)
})
const filteredSessions = computed(() => {
  const q = sessionSearch.value.trim().toLowerCase()
  if (!q) return sessions.value
  return sessions.value.filter((s) => (s.title || s.sessionKey).toLowerCase().includes(q))
})

// Tick every second so running bubbles show live "waiting Ns" in pauses.
const now = ref(Date.now())
const ticker = setInterval(() => {
  now.value = Date.now()
}, 1000)
onUnmounted(() => clearInterval(ticker))

function setPhase(b: BubbleMsg, phase: BubblePhase) {
  b.phase = phase
  b.phaseAt = Date.now()
  b.thinking = phase !== 'done'
}

function statusLine(b: BubbleMsg): string {
  if (b.kind === 'user' || !b.phase) return ''
  const secs = b.phaseAt ? Math.max(0, Math.round((now.value - b.phaseAt) / 1000)) : 0
  switch (b.phase) {
    case 'thinking':
      return `正在思考… ${secs}s`
    case 'tools': {
      const n = b.tools.filter((t) => !t.done).length
      return n > 0
        ? `正在执行 ${n} 个工具… ${secs}s`
        : `整理工具结果・思考中… ${secs}s`
    }
    case 'streaming':
      return `正在输出回复… ${secs}s`
    case 'done':
      return '回答完成'
  }
}

async function loadSessions() {
  try {
    sessions.value = await api.listSessions()
  } catch {
    /* keep whatever we have */
  }
}

async function switchSession(id: string) {
  currentSessionId.value = id
  await loadHistory(id)
}

async function loadHistory(id: string) {
  loadingHistory.value = true
  bubbles.value = []
  try {
    const items = await api.sessionHistory(id)
    renderHistory(items)
  } catch (e) {
    bubbles.value = [{ kind: 'assistant', text: '历史加载失败：' + String(e), tools: [], toolResults: [], thinking: false }]
  } finally {
    loadingHistory.value = false
    await nextTick()
    scrollThread()
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
  bubbles.value = out
}

function newChat() {
  currentSessionId.value = null
  bubbles.value = []
  loadingHistory.value = false
  nextTick(() => inputEl.value?.focus())
}

function scrollThread() {
  const el = threadEl.value
  if (el) el.scrollTop = el.scrollHeight
}

function autoGrow() {
  const el = inputEl.value
  if (!el) return
  el.style.height = 'auto'
  el.style.height = Math.min(el.scrollHeight, 120) + 'px'
}

async function sendMessage() {
  const el = inputEl.value
  if (!el) return
  const text = el.value.trim()
  if (!text) return
  bubbles.value.push({ kind: 'user', text, tools: [], toolResults: [], thinking: false })
  el.value = ''
  el.style.height = 'auto'
  // IMPORTANT: read the bubble back through the reactive proxy. Mutating a
  // plain-object closure reference (created before push) would bypass Vue's
  // reactivity and never trigger a re-render — the UI would stay stuck on
  // the initial "thinking" state even though the SSE events arrive.
  bubbles.value.push({ kind: 'assistant', tools: [], toolResults: [], thinking: true })
  const bubble: BubbleMsg = bubbles.value[bubbles.value.length - 1]
  await nextTick()
  scrollThread()

  try {
    await streamSSE(
      '/api/messages',
      {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'X-CubePilot-User': user },
        body: JSON.stringify({ session_id: currentSessionId.value, content: text }),
      },
      (_evName, ev) => {
        if (ev.type === 'message_start') {
          if (ev.session_id) currentSessionId.value = ev.session_id
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
          const t = bubble.tools.find((x) => x.callID && x.callID === ev.call_id)
            || bubble.tools.find((x) => !x.done)
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
    await nextTick()
    scrollThread()
  }
}

onMounted(() => {
  loadSessions()
})
</script>

<template>
  <div class="chat-body">
    <div class="session-panel">
      <div class="session-head">
        <div class="session-search">
          <svg class="icon" style="width: 14px; height: 14px" viewBox="0 0 24 24"><circle cx="11" cy="11" r="8" /><path d="M21 21l-4.35-4.35" /></svg>
          <input v-model="sessionSearch" placeholder="搜索会话" aria-label="搜索会话" />
        </div>
        <button class="new-chat" aria-label="新建会话" @click="newChat">
          <svg class="icon" viewBox="0 0 24 24"><path d="M12 5v14M5 12h14" /></svg>
        </button>
      </div>
      <div class="session-list">
        <div
          v-for="s in filteredSessions"
          :key="s.sessionKey"
          class="session-item"
          :class="{ active: currentSessionId === s.sessionKey }"
          @click="switchSession(s.sessionKey)"
        >
          <svg class="s-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M21 11.5a8.38 8.38 0 0 1-.9 3.8 8.5 8.5 0 0 1-7.6 4.7 8.38 8.38 0 0 1-3.8-.9L3 21l1.9-5.7a8.38 8.38 0 0 1-.9-3.8 8.5 8.5 0 0 1 4.7-7.6 8.38 8.38 0 0 1 3.8-.9h.5a8.48 8.48 0 0 1 8 8v.5z" /></svg>
          <div class="s-main">
            <div class="s-title">{{ s.title || shortSession(s.sessionKey) }}</div>
            <div class="s-meta"><span class="mono" style="font-size: 10px">{{ shortSession(s.sessionKey) }}</span></div>
          </div>
        </div>
      </div>
    </div>

    <div class="chat-main">
      <div class="chat-head">
        <div class="chat-head-main">
          <div class="chat-head-title">{{ chatTitle }}</div>
          <div class="chat-head-meta">{{ loadingHistory ? '加载历史中…' : (currentSessionId ? '已加载历史 · 继续对话即可' : '尚未开始') }}</div>
        </div>
      </div>

      <div ref="threadEl" class="thread">
        <div class="thread-inner">
          <template v-if="bubbles.length">
            <div v-for="(b, i) in bubbles" :key="i" class="msg" :class="b.kind">
              <div class="avatar">{{ b.kind === 'user' ? userInitials : 'AI' }}</div>
              <div class="bubble">
                <!-- Status line: thinking / executing tools / streaming / done -->
                <div v-if="b.phase && b.phase !== 'done'" class="tool-status">
                  <span class="spin"></span>{{ statusLine(b) }}
                </div>
                <div v-if="b.phase === 'done' && !b.error" class="tool-status done-mark">
                  <svg class="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><path d="M20 6L9 17l-5-5"/></svg>
                  {{ statusLine(b) }}
                </div>
                <div
                  v-for="(t, ti) in b.tools"
                  :key="'t' + ti"
                  class="tool-card"
                  :class="{ 'tool-running': !t.done && b.phase !== 'done' }"
                >
                  <div class="tool-head">
                    <svg class="icon" viewBox="0 0 24 24"><path d="M4 17l6-6-6-6M12 19h8" /></svg>
                    <span class="tool-cmd">{{ t.name }}</span>
                    <span v-if="!t.done && b.phase !== 'done'" class="pill accent">执行中…</span>
                    <span v-else class="pill neutral">已完成</span>
                  </div>
                  <div class="tool-body"><span class="mono" style="white-space: pre-wrap; word-break: break-all">{{ t.cmd }}</span></div>
                </div>
                <div
                  v-for="(r, ri) in b.toolResults"
                  :key="'r' + ri"
                  style="margin-top: 8px; background: rgba(0,0,0,.04); border: 1px solid var(--border); border-radius: 6px; padding: 8px 10px; max-height: 220px; overflow: auto; font-size: 12px; line-height: 1.6; white-space: pre-wrap; word-break: break-all"
                  class="mono"
                >{{ r }}</div>
                <div v-if="b.text" style="font-size: 13.5px; line-height: 1.7; white-space: pre-wrap; word-break: break-word">{{ b.text }}</div>
                <div v-if="b.error" style="font-size: 13px; color: var(--danger); white-space: pre-wrap">⚠ {{ b.error }}</div>
              </div>
            </div>
          </template>
          <div v-else class="thread-empty">
            <svg class="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round"><path d="M21 11.5a8.38 8.38 0 0 1-.9 3.8 8.5 8.5 0 0 1-7.6 4.7 8.38 8.38 0 0 1-3.8-.9L3 21l1.9-5.7a8.38 8.38 0 0 1-.9-3.8 8.5 8.5 0 0 1 4.7-7.6 8.38 8.38 0 0 1 3.8-.9h.5a8.48 8.48 0 0 1 8 8v.5z" /></svg>
            <div class="thread-empty-title">开始新的对话</div>
            <div class="thread-empty-desc">在下方输入需求，CubePilot 会调用平台能力帮你排查、部署或查询资源。</div>
          </div>
        </div>
      </div>

      <div class="composer">
        <div class="composer-inner">
          <textarea
            ref="inputEl"
            rows="1"
            placeholder="输入指令，例如「查看 GPU 利用率」或「创建一个开发环境」…"
            aria-label="输入消息"
            @input="autoGrow"
            @keydown.enter.exact.prevent="sendMessage"
          ></textarea>
          <button class="send-btn" aria-label="发送" @click="sendMessage">
            <svg class="icon" viewBox="0 0 24 24"><path d="M22 2 11 13M22 2l-7 20-4-9-9-4z" /></svg>
          </button>
        </div>
        <div class="composer-hint">支持自然语言操作平台资源 · 输入 <span class="mono">@</span> 引用资源 · 阶段一写操作直放</div>
      </div>
    </div>
  </div>
</template>
