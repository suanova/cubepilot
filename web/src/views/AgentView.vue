<script setup lang="ts">
// Agent config view — model / system prompt / instance status / skills (FR-M2-005).
import { onMounted, ref } from 'vue'
import { api } from '@/api'
import type { AgentConfig, AgentStatus } from '@/api/types'
import { esc, fmtUptime } from '@/utils/format'
import { useToastStore } from '@/stores/toast'

const toast = useToastStore()

const cfg = ref<AgentConfig>({})
const status = ref<AgentStatus | null>(null)
const skills = ref<Array<{ name: string; enabled: boolean }>>([])

const SKILL_LABELS: Record<string, string> = {
  'kubectl-platform': '平台资源操作',
  'dev-environment': '开发环境',
  'inference-service': '推理服务',
  inspection: '智能巡检',
}

const MODELS = [
  'cuberouter/glm-5.1',
  'cuberouter/glm-5.2',
  'cuberouter/kimi-k3',
  'cuberouter/kimi-k2.7-code',
]

async function loadAgentView() {
  try {
    const [c, st] = await Promise.all([api.agentConfig(), api.agentStatus()])
    cfg.value = c
    status.value = st
    skills.value = c.skills || []
  } catch (e) {
    console.error('loadAgentView', e)
  }
}

function toggleSkill(name: string) {
  const sk = skills.value.find((s) => s.name === name)
  if (sk) sk.enabled = !sk.enabled
}

async function saveAgentConfig() {
  try {
    await api.saveAgentConfig({ model: cfg.value.model, systemPrompt: cfg.value.systemPrompt, skills: skills.value })
    toast.show('配置已保存 · 系统提示词即时生效')
  } catch (e) {
    toast.show('保存失败：' + e)
  }
}

onMounted(loadAgentView)
</script>

<template>
  <div class="view active">
    <div class="view-head">
      <div>
        <div class="view-title">Agent 配置</div>
        <div class="view-desc">模型选择 · Skills 技能 · 系统提示词 · 确认规则 · 实例状态（FR-M2-005）</div>
      </div>
      <button class="btn primary" @click="saveAgentConfig">
        <svg class="icon" viewBox="0 0 24 24"><path d="M20 6L9 17l-5-5" /></svg>保存配置
      </button>
    </div>

    <div class="config-grid">
      <div style="display: flex; flex-direction: column; gap: 14px">
        <div class="card">
          <div class="card-head"><span class="card-title">模型与运行时</span><span class="card-hint">OpenAI 兼容接口 · 保存为偏好，实例重建时生效</span></div>
          <div class="card-pad">
            <div class="field">
              <label class="label">助手 LLM 模型</label>
              <select v-model="cfg.model" class="input" aria-label="选择模型">
                <option v-for="m in MODELS" :key="m" :value="m">{{ m }}</option>
              </select>
            </div>
            <div class="field" style="margin-bottom: 0">
              <label class="label">Agent 运行时</label>
              <select class="input" aria-label="选择运行时">
                <option selected>OpenClaw（阶段一）</option>
                <option disabled>Hermes（预留 · 阶段三）</option>
              </select>
            </div>
          </div>
        </div>
        <div class="card">
          <div class="card-head"><span class="card-title">系统提示词</span><span class="card-hint">保存后即时注入后续对话</span></div>
          <div class="card-pad">
            <textarea v-model="cfg.systemPrompt" class="input" rows="6" aria-label="系统提示词" placeholder="留空则使用 Agent 镜像内置人设（SOUL.md）"></textarea>
          </div>
        </div>
      </div>

      <div style="display: flex; flex-direction: column; gap: 14px">
        <div class="card">
          <div class="card-head">
            <span class="card-title">实例状态</span>
            <span class="pill" :class="status?.exists ? 'success' : 'neutral'">{{ status?.exists ? '运行中' : '已回收' }}</span>
          </div>
          <div class="card-pad">
            <div class="inst-grid">
              <div class="inst"><div class="k">实例 ID</div><div class="v"><span class="mono">{{ status?.id || '—' }}</span></div></div>
              <div class="inst"><div class="k">运行状态</div><div class="v">{{ status?.exists ? 'Running' : (status?.phase || '—') }}</div></div>
              <div class="inst"><div class="k">运行时长</div><div class="v">{{ status?.uptimeSeconds != null ? fmtUptime(status.uptimeSeconds) : '—' }}</div></div>
              <div class="inst"><div class="k">Agent 镜像</div><div class="v" style="font-size: 12px"><span class="mono">{{ status?.gatewayImage || '—' }}</span></div></div>
              <div class="inst"><div class="k">空闲回收</div><div class="v">{{ status?.idleTTLMinutes ? status.idleTTLMinutes + ' min' : '—' }}</div></div>
              <div class="inst"><div class="k">数据卷</div><div class="v" style="font-size: 12px"><span class="mono">data-{{ status?.user }}</span></div></div>
            </div>
          </div>
        </div>
        <div class="card">
          <div class="card-head"><span class="card-title">确认规则</span><span class="card-hint">阶段一读写直放 · 全部写入审计（M5）</span></div>
          <div class="card-pad">
            <div class="rule-row">
              <svg class="icon" style="width: 15px; height: 15px; color: var(--danger)" viewBox="0 0 24 24"><path d="M10.29 3.86 1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0zM12 9v4M12 17h.01" /></svg>
              <span class="mono">kubectl delete *</span><span class="pill accent">直放 · 记审计</span>
            </div>
            <div class="rule-row">
              <svg class="icon" style="width: 15px; height: 15px; color: var(--danger)" viewBox="0 0 24 24"><path d="M10.29 3.86 1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0zM12 9v4M12 17h.01" /></svg>
              <span class="mono">kubectl exec *</span><span class="pill accent">直放 · 记审计</span>
            </div>
            <div class="rule-row">
              <svg class="icon" style="width: 15px; height: 15px; color: var(--danger)" viewBox="0 0 24 24"><path d="M10.29 3.86 1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0zM12 9v4M12 17h.01" /></svg>
              <span class="mono">InferenceService delete</span><span class="pill accent">直放 · 记审计</span>
            </div>
          </div>
        </div>
      </div>
    </div>

    <div class="card" style="margin-top: 14px">
      <div class="card-head"><span class="card-title">Skills 技能</span><span class="card-hint">来自实例镜像内置能力目录 · 开关保存为偏好</span></div>
      <div class="card-pad" style="padding-top: 4px; padding-bottom: 10px">
        <div class="skill-group">系统技能 · 内置不可关闭</div>
        <div class="toggle">
          <div class="toggle-info">
            <div class="toggle-title">平台资源操作 <span class="mono" style="color: var(--muted); font-weight: 500">kubectl</span></div>
            <div class="toggle-desc">以用户身份直连 K8s API Server，读写平台资源（RBAC 强制）</div>
          </div>
          <span class="lock-badge">
            <svg class="icon" viewBox="0 0 24 24"><rect x="4" y="11" width="16" height="10" rx="2" /><path d="M8 11V7a4 4 0 0 1 8 0v4" /></svg>系统
          </span>
        </div>
        <div class="skill-group">平台能力 · 能力目录</div>
        <template v-if="skills.length">
          <div v-for="sk in skills" :key="sk.name" class="toggle">
            <div class="toggle-info">
              <div class="toggle-title">{{ SKILL_LABELS[sk.name] || sk.name }} <span class="mono" style="color: var(--muted); font-weight: 500">{{ esc(sk.name) }}</span></div>
              <div class="toggle-desc">来自实例镜像内置能力目录 · 开关保存为偏好</div>
            </div>
            <button
              class="switch"
              role="switch"
              :aria-checked="sk.enabled"
              :aria-label="SKILL_LABELS[sk.name] || sk.name"
              @click="toggleSkill(sk.name)"
            ></button>
          </div>
        </template>
        <div v-else style="color: var(--muted); padding: 8px 0">暂无登记能力</div>
      </div>
    </div>
  </div>
</template>
