// Agent config view — model / system prompt / instance status / skills (FR-M2-005).
import { useEffect, useMemo, useState } from 'react'
import { api } from '@/api'
import type { AgentConfig, AgentStatus } from '@/api/types'
import { esc, fmtUptime } from '@/utils/format'
import { showToast } from '@/stores/toast'

const SKILL_LABELS: Record<string, string> = {
  'kubectl-platform': '平台资源操作',
  'dev-environment': '开发环境',
  'inference-service': '推理服务',
  inspection: '智能巡检',
}

interface ModelRow {
  name: string
  displayName: string
  phase: string
  provider: string
  endpoint: string
}

function CheckIcon() {
  return <svg className="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round"><path d="M20 6L9 17l-5-5" /></svg>
}
function CloseIcon() {
  return <svg className="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round"><path d="M18 6L6 18M6 6l12 12" /></svg>
}
function WarnIcon() {
  return <svg className="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round"><path d="M10.29 3.86 1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0zM12 9v4M12 17h.01" /></svg>
}
function LockIcon() {
  return <svg className="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round"><rect x="4" y="11" width="16" height="10" rx="2" /><path d="M8 11V7a4 4 0 0 1 8 0v4" /></svg>
}

export default function AgentView() {
  const [cfg, setCfg] = useState<AgentConfig>({})
  const [status, setStatus] = useState<AgentStatus | null>(null)
  const [skills, setSkills] = useState<Array<{ name: string; enabled: boolean }>>([])
  const [provisioning, setProvisioning] = useState(false)
  const [models, setModels] = useState<ModelRow[]>([])

  // Add-model dialog state
  const [modelDialogOpen, setModelDialogOpen] = useState(false)
  const [modelForm, setModelForm] = useState({
    displayName: '',
    provider: 'External',
    endpoint: '',
    credentialRef: '',
    modelId: '',
  })
  const [modelSaving, setModelSaving] = useState(false)

  const availableModels = useMemo(() => models.filter((m) => m.phase !== 'Unreachable'), [models])

  async function loadModels() {
    try {
      const list = await api.listModels()
      const mapped = list.map((m) => ({
        name: m.metadata?.name ?? '',
        displayName: String(m.spec?.displayName ?? m.metadata?.name ?? ''),
        phase: String(m.status?.phase ?? ''),
        provider: String(m.spec?.provider ?? ''),
        endpoint: String(m.spec?.endpoint ?? ''),
      }))
      // Keep a stale selection visible (it may belong to an Unreachable model).
      if (cfg.model && !mapped.some((m) => m.name === cfg.model)) {
        mapped.push({ name: cfg.model, displayName: cfg.model, phase: 'Unreachable', provider: '', endpoint: '' })
      }
      setModels(mapped)
    } catch (e) {
      console.error('loadModels', e)
    }
  }

  async function loadAgentView() {
    try {
      const [c, st] = await Promise.all([api.agentConfig(), api.agentStatus()])
      setCfg(c)
      setStatus(st)
      setSkills(c.skills || [])
      await loadModels()
    } catch (e) {
      console.error('loadAgentView', e)
    }
  }

  useEffect(() => {
    loadAgentView()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  function openModelDialog() {
    setModelForm({ displayName: '', provider: 'External', endpoint: '', credentialRef: '', modelId: '' })
    setModelDialogOpen(true)
  }

  async function createModel() {
    if (!modelForm.displayName.trim()) {
      showToast('模型名称必填')
      return
    }
    setModelSaving(true)
    try {
      await api.createModel(modelForm)
      showToast('模型已添加 · 控制器探测后将变为可用')
      setModelDialogOpen(false)
      await loadModels()
    } catch (e) {
      showToast('添加失败：' + e)
    } finally {
      setModelSaving(false)
    }
  }

  async function provisionInstance() {
    if (provisioning) return
    setProvisioning(true)
    try {
      const inst = await api.createInstance({ agentRef: 'agent-for-cloud', selectedModel: cfg.model || undefined })
      showToast(inst.metadata?.name ? '实例已创建 · 控制器正在拉起 Pod' : '实例已创建 · 控制器正在拉起 Pod')
      await loadAgentView()
    } catch (e) {
      showToast('开通失败：' + e)
    } finally {
      setProvisioning(false)
    }
  }

  function toggleSkill(name: string) {
    setSkills((prev) => prev.map((s) => (s.name === name ? { ...s, enabled: !s.enabled } : s)))
  }

  async function saveAgentConfig() {
    try {
      await api.saveAgentConfig({ model: cfg.model, systemPrompt: cfg.systemPrompt, skills })
      showToast('配置已保存 · 系统提示词即时生效')
    } catch (e) {
      showToast('保存失败：' + e)
    }
  }

  return (
    <div className="view active">
      <div className="view-head">
        <div>
          <div className="view-title">Agent 配置</div>
          <div className="view-desc">模型选择 · Skills 技能 · 系统提示词 · 确认规则 · 实例状态（FR-M2-005）</div>
        </div>
        <button className="btn primary" onClick={saveAgentConfig}>
          <CheckIcon />
          保存配置
        </button>
      </div>

      <div className="config-grid">
        <div style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
          <div className="card">
            <div className="card-head">
              <span className="card-title">模型与运行时</span>
              <span className="card-hint">OpenAI 兼容接口 · 保存为偏好，实例重建时生效</span>
            </div>
            <div className="card-pad">
              <div className="field">
                <label className="label">助手 LLM 模型</label>
                <select
                  className="input"
                  aria-label="选择模型"
                  value={cfg.model || ''}
                  onChange={(e) => setCfg((c) => ({ ...c, model: e.target.value }))}
                >
                  {availableModels.map((m) => (
                    <option key={m.name} value={m.name}>
                      {m.displayName || m.name}
                      {m.phase === 'Unreachable' ? '（不可用）' : ''}
                    </option>
                  ))}
                </select>
                <button className="btn" style={{ marginTop: 8, width: '100%' }} onClick={openModelDialog}>
                  ＋ 添加模型
                </button>
              </div>
              <div className="field" style={{ marginBottom: 0 }}>
                <label className="label">Agent 运行时</label>
                <select className="input" aria-label="选择运行时">
                  <option>OpenClaw（阶段一）</option>
                  <option disabled>Hermes（预留 · 阶段三）</option>
                </select>
              </div>
            </div>
          </div>
          <div className="card">
            <div className="card-head">
              <span className="card-title">系统提示词</span>
              <span className="card-hint">保存后即时注入后续对话</span>
            </div>
            <div className="card-pad">
              <textarea
                className="input"
                rows={6}
                aria-label="系统提示词"
                placeholder="留空则使用 Agent 镜像内置人设（SOUL.md）"
                value={cfg.systemPrompt || ''}
                onChange={(e) => setCfg((c) => ({ ...c, systemPrompt: e.target.value }))}
              />
            </div>
          </div>
        </div>

        <div style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
          <div className="card">
            <div className="card-head">
              <span className="card-title">实例状态</span>
              <span className={`pill ${status?.exists ? 'success' : 'neutral'}`}>{status?.exists ? '运行中' : '已回收'}</span>
            </div>
            <div className="card-pad">
              <div className="inst-grid">
                <div className="inst">
                  <div className="k">实例 ID</div>
                  <div className="v">
                    <span className="mono">{status?.id || '—'}</span>
                  </div>
                </div>
                <div className="inst">
                  <div className="k">运行状态</div>
                  <div className="v">{status?.exists ? 'Running' : status?.phase || '—'}</div>
                </div>
                <div className="inst">
                  <div className="k">运行时长</div>
                  <div className="v">{status?.uptimeSeconds != null ? fmtUptime(status.uptimeSeconds) : '—'}</div>
                </div>
                <div className="inst">
                  <div className="k">Agent 镜像</div>
                  <div className="v" style={{ fontSize: 12 }}>
                    <span className="mono">{status?.gatewayImage || '—'}</span>
                  </div>
                </div>
                <div className="inst">
                  <div className="k">空闲回收</div>
                  <div className="v">{status?.idleTTLMinutes ? status.idleTTLMinutes + ' min' : '—'}</div>
                </div>
                <div className="inst">
                  <div className="k">数据卷</div>
                  <div className="v" style={{ fontSize: 12 }}>
                    <span className="mono">data-{status?.user}</span>
                  </div>
                </div>
              </div>
              {!status?.exists && (
                <button
                  className="btn primary"
                  style={{ marginTop: 12, width: '100%' }}
                  disabled={provisioning}
                  onClick={provisionInstance}
                >
                  {provisioning ? '开通中…' : '开通我的实例'}
                </button>
              )}
            </div>
          </div>
          <div className="card">
            <div className="card-head">
              <span className="card-title">确认规则</span>
              <span className="card-hint">阶段一读写直放 · 全部写入审计（M5）</span>
            </div>
            <div className="card-pad">
              <div className="rule-row">
                <WarnIcon />
                <span className="mono">kubectl delete *</span>
                <span className="pill accent">直放 · 记审计</span>
              </div>
              <div className="rule-row">
                <WarnIcon />
                <span className="mono">kubectl exec *</span>
                <span className="pill accent">直放 · 记审计</span>
              </div>
              <div className="rule-row">
                <WarnIcon />
                <span className="mono">InferenceService delete</span>
                <span className="pill accent">直放 · 记审计</span>
              </div>
            </div>
          </div>
        </div>
      </div>

      <div className="card" style={{ marginTop: 14 }}>
        <div className="card-head">
          <span className="card-title">Skills 技能</span>
          <span className="card-hint">来自实例镜像内置能力目录 · 开关保存为偏好</span>
        </div>
        <div className="card-pad" style={{ paddingTop: 4, paddingBottom: 10 }}>
          <div className="skill-group">系统技能 · 内置不可关闭</div>
          <div className="toggle">
            <div className="toggle-info">
              <div className="toggle-title">
                平台资源操作 <span className="mono" style={{ color: 'var(--muted)', fontWeight: 500 }}>kubectl</span>
              </div>
              <div className="toggle-desc">以用户身份直连 K8s API Server，读写平台资源（RBAC 强制）</div>
            </div>
            <span className="lock-badge">
              <LockIcon />
              系统
            </span>
          </div>
          <div className="skill-group">平台能力 · 能力目录</div>
          {skills.length ? (
            skills.map((sk) => (
              <div key={sk.name} className="toggle">
                <div className="toggle-info">
                  <div className="toggle-title">
                    {SKILL_LABELS[sk.name] || sk.name}{' '}
                    <span className="mono" style={{ color: 'var(--muted)', fontWeight: 500 }}>{esc(sk.name)}</span>
                  </div>
                  <div className="toggle-desc">来自实例镜像内置能力目录 · 开关保存为偏好</div>
                </div>
                <button
                  className="switch"
                  role="switch"
                  aria-checked={sk.enabled}
                  aria-label={SKILL_LABELS[sk.name] || sk.name}
                  onClick={() => toggleSkill(sk.name)}
                />
              </div>
            ))
          ) : (
            <div style={{ color: 'var(--muted)', padding: '8px 0' }}>暂无登记能力</div>
          )}
        </div>
      </div>

      {modelDialogOpen && (
        <div
          className="modal-overlay open"
          role="dialog"
          aria-modal="true"
          onMouseDown={(e) => {
            if (e.target === e.currentTarget) setModelDialogOpen(false)
          }}
        >
          <div className="modal">
            <div className="modal-head">
              <span className="modal-title">添加模型</span>
              <button className="modal-close" aria-label="关闭" onClick={() => setModelDialogOpen(false)}>
                <CloseIcon />
              </button>
            </div>
            <div className="modal-body">
              <div>
                <label className="label">模型名称（显示名）</label>
                <input
                  className="input"
                  placeholder="例如：DeepSeek V3 内部部署"
                  aria-label="模型名称"
                  value={modelForm.displayName}
                  onChange={(e) => setModelForm((f) => ({ ...f, displayName: e.target.value }))}
                />
              </div>
              <div>
                <label className="label">提供方式</label>
                <div className="radio-row" role="radiogroup" aria-label="提供方式">
                  <button
                    type="button"
                    className={`radio ${modelForm.provider === 'Platform' ? 'active' : ''}`}
                    role="radio"
                    aria-checked={modelForm.provider === 'Platform'}
                    onClick={() => setModelForm((f) => ({ ...f, provider: 'Platform' }))}
                  >
                    平台部署（内置/手动）
                  </button>
                  <button
                    type="button"
                    className={`radio ${modelForm.provider === 'External' ? 'active' : ''}`}
                    role="radio"
                    aria-checked={modelForm.provider === 'External'}
                    onClick={() => setModelForm((f) => ({ ...f, provider: 'External' }))}
                  >
                    外部兼容端点
                  </button>
                </div>
              </div>
              <div>
                <label className="label">
                  端点（OpenAI 兼容 Base URL）
                  {modelForm.provider === 'External' && <span style={{ color: 'var(--danger)' }}> · 必填</span>}
                </label>
                <input
                  className="input mono"
                  placeholder="https://inference.example.com/v1"
                  aria-label="端点"
                  value={modelForm.endpoint}
                  onChange={(e) => setModelForm((f) => ({ ...f, endpoint: e.target.value }))}
                />
              </div>
              {modelForm.provider === 'External' && (
                <div>
                  <label className="label">凭据 Secret（credentialRef · 平台管理）</label>
                  <input
                    className="input mono"
                    placeholder="model-credential"
                    aria-label="凭据引用"
                    value={modelForm.credentialRef}
                    onChange={(e) => setModelForm((f) => ({ ...f, credentialRef: e.target.value }))}
                  />
                </div>
              )}
              <div>
                <label className="label">后端模型 ID（可选 · 留空 = 运行时默认）</label>
                <input
                  className="input mono"
                  placeholder="deepseek/deepseek-v4-flash"
                  aria-label="后端模型 ID"
                  value={modelForm.modelId}
                  onChange={(e) => setModelForm((f) => ({ ...f, modelId: e.target.value }))}
                />
              </div>
              <div className="notice">
                <WarnIcon />
                <span>
                  添加后控制器会探测端点连通性，Available 后才能被实例选用；external 需先在集群中创建凭据 Secret（key:{' '}
                  <span className="mono">apiKey</span>）。
                </span>
              </div>
            </div>
            <div className="modal-foot">
              <button className="btn" onClick={() => setModelDialogOpen(false)}>
                取消
              </button>
              <button className="btn primary" disabled={modelSaving} onClick={createModel}>
                {modelSaving ? '提交中…' : '添加模型'}
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  )
}