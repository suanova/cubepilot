// Agent config view -- model / system prompt / instance status / skills (FR-M2-005).
import { useEffect, useMemo, useState } from 'react'
import { api } from '@/api'
import type { AgentConfig, AgentStatus } from '@/api/types'
import { esc, fmtUptime } from '@/utils/format'
import { showToast } from '@/stores/toast'

const SKILL_LABELS: Record<string, string> = {
  'kubectl-platform': 'Platform Resource Operations',
  'dev-environment': 'Development Environment',
  'inference-service': 'Inference Service',
  inspection: 'Smart Inspection',
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
      showToast('Model name is required')
      return
    }
    setModelSaving(true)
    try {
      await api.createModel(modelForm)
      showToast('Model added - it becomes usable after the controller probes it')
      setModelDialogOpen(false)
      await loadModels()
    } catch (e) {
      showToast('Add failed: ' + e)
    } finally {
      setModelSaving(false)
    }
  }

  async function provisionInstance() {
    if (provisioning) return
    setProvisioning(true)
    try {
      const inst = await api.createInstance({ agentRef: 'agent-for-cloud', selectedModel: cfg.model || undefined })
      showToast(inst.metadata?.name ? 'Instance created - the controller is starting the Pod' : 'Instance created - the controller is starting the Pod')
      await loadAgentView()
    } catch (e) {
      showToast('Provisioning failed: ' + e)
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
      showToast('Config saved - system prompt takes effect immediately')
    } catch (e) {
      showToast('Save failed: ' + e)
    }
  }

  return (
    <div className="view active">
      <div className="view-head">
        <div>
          <div className="view-title">Agent Config</div>
          <div className="view-desc">Model selection - Skills - System prompt - Confirm rules - Instance status (FR-M2-005)</div>
        </div>
        <button className="btn primary" onClick={saveAgentConfig}>
          <CheckIcon />
          Save Config
        </button>
      </div>

      <div className="config-grid">
        <div style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
          <div className="card">
            <div className="card-head">
              <span className="card-title">Model & Runtime</span>
              <span className="card-hint">OpenAI-compatible endpoint - saved as preference, applied on instance rebuild</span>
            </div>
            <div className="card-pad">
              <div className="field">
                <label className="label">Assistant LLM Model</label>
                <select
                  className="input"
                  aria-label="Select model"
                  value={cfg.model || ''}
                  onChange={(e) => setCfg((c) => ({ ...c, model: e.target.value }))}
                >
                  {availableModels.map((m) => (
                    <option key={m.name} value={m.name}>
                      {m.displayName || m.name}
                      {m.phase === 'Unreachable' ? ' (unavailable)' : ''}
                    </option>
                  ))}
                </select>
                <button className="btn" style={{ marginTop: 8, width: '100%' }} onClick={openModelDialog}>
                  + Add Model
                </button>
              </div>
              <div className="field" style={{ marginBottom: 0 }}>
                <label className="label">Agent Runtime</label>
                <select className="input" aria-label="Select runtime">
                  <option>OpenClaw (Phase One)</option>
                  <option disabled>Hermes (reserved - Phase Three)</option>
                </select>
              </div>
            </div>
          </div>
          <div className="card">
            <div className="card-head">
              <span className="card-title">System Prompt</span>
              <span className="card-hint">Injected into subsequent conversations immediately after saving</span>
            </div>
            <div className="card-pad">
              <textarea
                className="input"
                rows={6}
                aria-label="System prompt"
                placeholder="Leave empty to use the persona built into the Agent image (SOUL.md)"
                value={cfg.systemPrompt || ''}
                onChange={(e) => setCfg((c) => ({ ...c, systemPrompt: e.target.value }))}
              />
            </div>
          </div>
        </div>

        <div style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
          <div className="card">
            <div className="card-head">
              <span className="card-title">Instance Status</span>
              <span className={`pill ${status?.exists ? 'success' : 'neutral'}`}>{status?.exists ? 'Running' : 'Reclaimed'}</span>
            </div>
            <div className="card-pad">
              <div className="inst-grid">
                <div className="inst">
                  <div className="k">Instance ID</div>
                  <div className="v">
                    <span className="mono">{status?.id || '-'}</span>
                  </div>
                </div>
                <div className="inst">
                  <div className="k">Run Status</div>
                  <div className="v">{status?.exists ? 'Running' : status?.phase || '-'}</div>
                </div>
                <div className="inst">
                  <div className="k">Uptime</div>
                  <div className="v">{status?.uptimeSeconds != null ? fmtUptime(status.uptimeSeconds) : '-'}</div>
                </div>
                <div className="inst">
                  <div className="k">Agent Image</div>
                  <div className="v" style={{ fontSize: 12 }}>
                    <span className="mono">{status?.gatewayImage || '-'}</span>
                  </div>
                </div>
                <div className="inst">
                  <div className="k">Idle Reclaim</div>
                  <div className="v">{status?.idleTTLMinutes ? status.idleTTLMinutes + ' min' : '-'}</div>
                </div>
                <div className="inst">
                  <div className="k">Data Volume</div>
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
                  {provisioning ? 'Provisioning...' : 'Provision My Instance'}
                </button>
              )}
            </div>
          </div>
          <div className="card">
            <div className="card-head">
              <span className="card-title">Confirm Rules</span>
              <span className="card-hint">Phase one: read/write pass through directly - all writes audited (M5)</span>
            </div>
            <div className="card-pad">
              <div className="rule-row">
                <WarnIcon />
                <span className="mono">kubectl delete *</span>
                <span className="pill accent">Pass-through - audited</span>
              </div>
              <div className="rule-row">
                <WarnIcon />
                <span className="mono">kubectl exec *</span>
                <span className="pill accent">Pass-through - audited</span>
              </div>
              <div className="rule-row">
                <WarnIcon />
                <span className="mono">InferenceService delete</span>
                <span className="pill accent">Pass-through - audited</span>
              </div>
            </div>
          </div>
        </div>
      </div>

      <div className="card" style={{ marginTop: 14 }}>
        <div className="card-head">
          <span className="card-title">Skills</span>
          <span className="card-hint">From the capability catalog built into the instance image - toggles saved as preferences</span>
        </div>
        <div className="card-pad" style={{ paddingTop: 4, paddingBottom: 10 }}>
          <div className="skill-group">System Skills - built-in, cannot be disabled</div>
          <div className="toggle">
            <div className="toggle-info">
              <div className="toggle-title">
                Platform Resource Operations <span className="mono" style={{ color: 'var(--muted)', fontWeight: 500 }}>kubectl</span>
              </div>
              <div className="toggle-desc">Connects to the K8s API Server as the user to read/write platform resources (RBAC enforced)</div>
            </div>
            <span className="lock-badge">
              <LockIcon />
              System
            </span>
          </div>
          <div className="skill-group">Platform Capabilities - Capability Catalog</div>
          {skills.length ? (
            skills.map((sk) => (
              <div key={sk.name} className="toggle">
                <div className="toggle-info">
                  <div className="toggle-title">
                    {SKILL_LABELS[sk.name] || sk.name}{' '}
                    <span className="mono" style={{ color: 'var(--muted)', fontWeight: 500 }}>{esc(sk.name)}</span>
                  </div>
                  <div className="toggle-desc">From the capability catalog built into the instance image - toggles saved as preferences</div>
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
            <div style={{ color: 'var(--muted)', padding: '8px 0' }}>No registered capabilities</div>
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
              <span className="modal-title">Add Model</span>
              <button className="modal-close" aria-label="Close" onClick={() => setModelDialogOpen(false)}>
                <CloseIcon />
              </button>
            </div>
            <div className="modal-body">
              <div>
                <label className="label">Model Name (Display Name)</label>
                <input
                  className="input"
                  placeholder="e.g. DeepSeek V3 internal deployment"
                  aria-label="Model name"
                  value={modelForm.displayName}
                  onChange={(e) => setModelForm((f) => ({ ...f, displayName: e.target.value }))}
                />
              </div>
              <div>
                <label className="label">Provisioning</label>
                <div className="radio-row" role="radiogroup" aria-label="Provisioning">
                  <button
                    type="button"
                    className={`radio ${modelForm.provider === 'Platform' ? 'active' : ''}`}
                    role="radio"
                    aria-checked={modelForm.provider === 'Platform'}
                    onClick={() => setModelForm((f) => ({ ...f, provider: 'Platform' }))}
                  >
                    Platform Deployed (built-in/manual)
                  </button>
                  <button
                    type="button"
                    className={`radio ${modelForm.provider === 'External' ? 'active' : ''}`}
                    role="radio"
                    aria-checked={modelForm.provider === 'External'}
                    onClick={() => setModelForm((f) => ({ ...f, provider: 'External' }))}
                  >
                    External Compatible Endpoint
                  </button>
                </div>
              </div>
              <div>
                <label className="label">
                  Endpoint (OpenAI-compatible Base URL)
                  {modelForm.provider === 'External' && <span style={{ color: 'var(--danger)' }}> - required</span>}
                </label>
                <input
                  className="input mono"
                  placeholder="https://inference.example.com/v1"
                  aria-label="Endpoint"
                  value={modelForm.endpoint}
                  onChange={(e) => setModelForm((f) => ({ ...f, endpoint: e.target.value }))}
                />
              </div>
              {modelForm.provider === 'External' && (
                <div>
                  <label className="label">Credential Secret (credentialRef - platform managed)</label>
                  <input
                    className="input mono"
                    placeholder="model-credential"
                    aria-label="Credential reference"
                    value={modelForm.credentialRef}
                    onChange={(e) => setModelForm((f) => ({ ...f, credentialRef: e.target.value }))}
                  />
                </div>
              )}
              <div>
                <label className="label">Backend Model ID (optional - empty = runtime default)</label>
                <input
                  className="input mono"
                  placeholder="deepseek/deepseek-v4-flash"
                  aria-label="Backend model ID"
                  value={modelForm.modelId}
                  onChange={(e) => setModelForm((f) => ({ ...f, modelId: e.target.value }))}
                />
              </div>
              <div className="notice">
                <WarnIcon />
                <span>
                  After adding, the controller probes endpoint connectivity; the model becomes selectable only once Available. For external models, first create a credential Secret in the cluster (key:{' '}
                  <span className="mono">apiKey</span>).
                </span>
              </div>
            </div>
            <div className="modal-foot">
              <button className="btn" onClick={() => setModelDialogOpen(false)}>
                Cancel
              </button>
              <button className="btn primary" disabled={modelSaving} onClick={createModel}>
                {modelSaving ? 'Submitting...' : 'Add Model'}
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  )
}