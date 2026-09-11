// Agent config view -- model / system prompt / instance status / skills (FR-M2-005).
import { useEffect, useState } from 'react'
import { api } from '@/api'
import type { AgentConfig, AgentConfirmView, AgentStatus, AllowlistRule, PlatformObject } from '@/api/types'
import { esc, fmtUptime } from '@/utils/format'
import { enabledSkillsFromInstances, skillSpecStr } from '@/utils/skills'
import { showToast } from '@/stores/toast'

interface TemplateModel {
  name: string
  endpoint: string
  credentialRef?: { name: string }
}

function CheckIcon() {
  return <svg className="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round"><path d="M20 6L9 17l-5-5" /></svg>
}
function WarnIcon() {
  return <svg className="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round"><path d="M10.29 3.86 1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0zM12 9v4M12 17h.01" /></svg>
}

export default function AgentView() {
  const [cfg, setCfg] = useState<AgentConfig>({ exists: false, model: '', systemPrompt: '' })
  const [status, setStatus] = useState<AgentStatus | null>(null)
  const [skills, setSkills] = useState<Array<{ sk: PlatformObject; enabled: boolean }>>([])
  const [hasInstance, setHasInstance] = useState(false)
  const [provisioning, setProvisioning] = useState(false)
  const [templateModels, setTemplateModels] = useState<TemplateModel[]>([])
  const [defaultModel, setDefaultModel] = useState('')
  const [llmForm, setLLMForm] = useState({ name: '', endpoint: '', apiKey: '', public: false })
  const [llmBusy, setLLMBusy] = useState(false)
  // The model the card form is currently editing; null means the form adds a
  // new one. The form is shared, so only one of the two is ever in flight.
  const [editingModel, setEditingModel] = useState<string | null>(null)

  // Confirmation posture (issue #116): confirmPolicy override + owned allowlist.
  const [confirm, setConfirm] = useState<AgentConfirmView | null>(null)
  const [policySel, setPolicySel] = useState('') // '' = follow the template default
  const [ruleForm, setRuleForm] = useState<{ pattern: string; argPattern: string }>({ pattern: '', argPattern: '' })
  const [confirmBusy, setConfirmBusy] = useState(false)

  async function loadTemplate() {
    try {
      const list = await api.listAgentTemplates()
      const tmpl = list[0]
      if (!tmpl) return
      const models: TemplateModel[] = ((tmpl.spec?.models || []) as Array<Record<string, string | { name: string }>>).map((m) => ({
        name: String(m.name ?? ''),
        endpoint: String(m.endpoint ?? ''),
        credentialRef: (m.credentialRef as { name: string } | undefined) ?? undefined,
      }))
      setTemplateModels(models)
      setDefaultModel(String(tmpl.spec?.defaultModel ?? ''))
    } catch (e) {
      console.error('loadTemplate', e)
    }
  }

  // The toggle list comes from the same Skill catalog the Publisher page lists
  // (issue #152): every visible skill, including kubectl-platform, is an
  // ordinary toggle -- there is no front-end notion of a locked system skill.
  // The on/off state reflects the real enabledSkills (AgentInstance CR); an
  // empty set is the resolver's "all enabled" baseline. Without an instance
  // there is no workspace to install into, so the toggles are all off instead.
  async function loadSkills() {
    try {
      const [skillList, instances] = await Promise.all([api.listSkills(), api.listInstances()])
      const has = instances.length > 0
      setHasInstance(has)
      const enabled = has ? enabledSkillsFromInstances(instances) : []
      const on = !has ? new Set<string>() : enabled.length === 0 ? new Set(skillList.map((s) => s.metadata.name)) : new Set(enabled)
      // Unreachable skills are hidden: install is refused for them, so a
      // toggle would never be usable.
      const toggleable = skillList.filter((sk) => sk.status?.phase !== 'Unreachable')
      setSkills(toggleable.map((sk) => ({ sk, enabled: on.has(sk.metadata.name) })))
    } catch (e) {
      console.error('loadSkills', e)
    }
  }

  async function loadAgentView() {
    try {
      const [c, st] = await Promise.all([api.agentConfig(), api.agentStatus()])
      setCfg(c)
      setStatus(st)
      await loadTemplate()
      await loadSkills()
      await loadConfirm()
    } catch (e) {
      console.error('loadAgentView', e)
    }
  }

  // Confirmation posture (issue #116) --------------------------------
  // The API may omit allowlist/allowlistOwned (null); normalize to [] so no
  // render/handler path calls .some/.length on null (issue #123).
  const withConfirmDefaults = (v: AgentConfirmView): AgentConfirmView => ({
    ...v,
    allowlist: v.allowlist ?? [],
    allowlistOwned: v.allowlistOwned ?? [],
  })

  async function loadConfirm() {
    try {
      const v = await api.agentConfirm()
      setConfirm(withConfirmDefaults(v))
      setPolicySel(v.override || '')
    } catch (e) {
      console.error('loadConfirm', e)
    }
  }

  const ruleKey = (r: AllowlistRule) => r.pattern + '|' + (r.argPattern || '')

  // Human label for an allowlist rule. Only the server knows whether a rule is
  // one of the platform builtin read-only rules (it sets `label`); the UI must
  // not guess -- a user/template rule with pattern "kubectl" can allow a write.
  // Rules without a label render their raw pattern.
  function allowlistLabel(r: AllowlistRule): string {
    return r.label || r.pattern || '(empty)'
  }

  // Every change PUTs the full desired owned state; the server treats an empty
  // list as inheriting the template default live.
  async function persistConfirm(owned: AllowlistRule[] | null, policy?: string) {
    if (!confirm || confirmBusy) return
    setConfirmBusy(true)
    try {
      const pol = policy !== undefined ? policy : policySel
      const v = await api.saveAgentConfirm({ confirmPolicy: pol, allowlist: owned ?? [] })
      setConfirm(withConfirmDefaults(v))
      setPolicySel(v.override || '')
    } catch (e) {
      showToast('Save confirmation config failed: ' + (e instanceof Error ? e.message : String(e)))
    } finally {
      setConfirmBusy(false)
    }
  }

  function changePolicy(value: string) {
    setPolicySel(value)
    const owned = confirm && confirm.allowlistOwned.length ? confirm.allowlistOwned : null
    void persistConfirm(owned, value)
  }

  function addRule() {
    const pattern = ruleForm.pattern.trim()
    if (!pattern) {
      showToast('Command pattern is required')
      return
    }
    const entry: AllowlistRule = { pattern, argPattern: ruleForm.argPattern.trim() || undefined }
    const base = confirm && confirm.allowlistOwned.length ? confirm.allowlistOwned : (confirm ? confirm.allowlist : [])
    if (base.some((r) => ruleKey(r) === ruleKey(entry))) {
      showToast('That command is already on the allowlist')
      return
    }
    void persistConfirm([...base, entry])
    setRuleForm({ pattern: '', argPattern: '' })
  }

  function removeRule(key: string) {
    if (!confirm) return
    // Removing from an inheriting list materializes it first (owned = effective
    // minus the rule), so the removal really sticks.
    const base = confirm.allowlistOwned.length ? confirm.allowlistOwned : confirm.allowlist
    void persistConfirm(base.filter((r) => ruleKey(r) !== key))
  }

  function resetConfirm() {
    setPolicySel('')
    void persistConfirm([], '')
  }

  useEffect(() => {
    loadAgentView()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  async function provisionInstance() {
    if (provisioning) return
    setProvisioning(true)
    try {
      const inst = await api.createInstance({ templateRef: 'cubepilot', selectedModel: cfg.model || undefined, userInstructions: cfg.systemPrompt || undefined })
      showToast(inst.metadata?.name ? 'Instance created - the controller is starting the Pod' : 'Instance created - the controller is starting the Pod')
      await loadAgentView()
    } catch (e) {
      showToast('Provisioning failed: ' + e)
    } finally {
      setProvisioning(false)
    }
  }

  // Toggle writes the real enabledSkills (install/uninstall); the supervisor
  // picks it up on its next sync. Errors surface via toast.
  async function toggleSkill(name: string) {
    const cur = skills.find((s) => s.sk.metadata.name === name)
    if (!cur) return
    if (!hasInstance) {
      showToast('Provision your instance on the Agent Config page first')
      return
    }
    try {
      if (cur.enabled) {
        await api.uninstallSkill(name)
      } else {
        await api.installSkill(name)
      }
      await loadSkills()
    } catch (e) {
      showToast('Skill update failed: ' + (e instanceof Error ? e.message : String(e)))
    }
  }

  async function saveAgentConfig() {
    // The selections live on the instance CR, so provisioning must come first.
    if (!hasInstance) {
      showToast('Provision your instance on the Agent Config page first')
      return
    }
    try {
      // Skills are persisted separately by the toggles above (enabledSkills);
      // here it is model (selectedModel) + system prompt (userInstructions),
      // both of which the resolver reads on the next turn.
      const v = await api.saveAgentConfig({ model: cfg.model, systemPrompt: cfg.systemPrompt })
      setCfg(v)
      showToast('Config saved - model and system prompt take effect on the next turn')
    } catch (e) {
      showToast('Save failed: ' + (e instanceof Error ? e.message : String(e)))
    }
  }

  function resetLLMForm() {
    setLLMForm({ name: '', endpoint: '', apiKey: '', public: false })
    setEditingModel(null)
  }

  function startEditLLM(m: TemplateModel) {
    // The stored key is never sent to the browser, so the field starts blank --
    // and a blank key on edit means "keep the current credential".
    setLLMForm({ name: m.name, endpoint: m.endpoint, apiKey: '', public: !m.credentialRef })
    setEditingModel(m.name)
  }

  async function removeLLM(name: string) {
    if (llmBusy) return
    // window.confirm: the `confirm` state in this view is the confirmation
    // posture, not the browser dialog.
    if (!window.confirm(`Remove model "${name}"? Its credential is deleted too.`)) return
    setLLMBusy(true)
    try {
      const res = await api.deleteLLM(name)
      showToast(res.warning || `Model "${name}" removed`)
      if (editingModel === name) resetLLMForm()
      await loadTemplate()
    } catch (e) {
      // A model an instance still selects is refused with the instances named.
      showToast('Remove failed: ' + (e instanceof Error ? e.message : String(e)))
    } finally {
      setLLMBusy(false)
    }
  }

  // Saves the card form: an add when no edit is in flight, otherwise a PUT.
  // Only an add needs a name and a credential decision -- an edit keeps the
  // stored name and, with a blank key, the stored credential.
  async function submitLLM() {
    if (llmBusy) return
    if (!llmForm.endpoint.trim()) {
      showToast('Endpoint is required')
      return
    }
    if (!editingModel && !llmForm.name.trim()) {
      showToast('Name is required')
      return
    }
    if (!editingModel && !llmForm.apiKey && !llmForm.public) {
      showToast('Enter the apiKey, or mark the endpoint public')
      return
    }
    setLLMBusy(true)
    try {
      if (editingModel) {
        const res = await api.updateLLM(editingModel, {
          endpoint: llmForm.endpoint,
          apiKey: llmForm.apiKey || undefined,
          public: llmForm.public,
        })
        showToast(res.warning || 'LLM updated - the operator will re-render the gateway')
      } else {
        await api.addLLM({
          name: llmForm.name,
          endpoint: llmForm.endpoint,
          apiKey: llmForm.apiKey || undefined,
          public: llmForm.public,
        })
        showToast('LLM added - the operator will wire it into the gateway')
      }
      resetLLMForm()
      await loadTemplate()
    } catch (e) {
      showToast((editingModel ? 'Update' : 'Add') + ' LLM failed: ' + (e instanceof Error ? e.message : String(e)))
    } finally {
      setLLMBusy(false)
    }
  }

  return (
    <div className="view active">
      <div className="view-head">
        <div>
          <div className="view-title">Agent Config</div>
          <div className="view-desc">Model selection - Skills - System prompt - Confirm rules - Instance status</div>
        </div>
        <button className="btn primary" onClick={saveAgentConfig} disabled={!hasInstance} title={hasInstance ? undefined : 'Provision your instance first'}>
          <CheckIcon />
          Save Config
        </button>
      </div>

      <div className="config-grid">
        <div style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
          <div className="card">
            <div className="card-head">
              <span className="card-title">Model & Runtime</span>
              <span className="card-hint">Models are inlined in the AgentTemplate - select from the template's models list</span>
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
                  <option value="" disabled>-- Select a model --</option>
                  {templateModels.map((m) => (
                    <option key={m.name} value={m.name}>
                      {m.name}
                    </option>
                  ))}
                </select>
                {templateModels.length === 0 && (
                  <div style={{ marginTop: 4, fontSize: 12, color: 'var(--danger)' }}>
                    No models yet — add one in the LLM Config card below
                  </div>
                )}
                {defaultModel && (
                  <div style={{ marginTop: 4, fontSize: 12, color: 'var(--muted)' }}>
                    Template default: {defaultModel}
                  </div>
                )}
              </div>
              <div className="field" style={{ marginBottom: 0 }}>
                <label className="label">Agent Runtime</label>
                <select className="input" aria-label="Select runtime">
                  <option>OpenClaw</option>
                </select>
              </div>
            </div>
          </div>
          <div className="card" style={{ order: 2 }}>
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
          <div className="card" style={{ order: 1 }}>
            <div className="card-head">
              <span className="card-title">LLM Config</span>
              <span className="card-hint">Add an OpenAI-compatible model to the platform catalog, or edit one already in it</span>
            </div>
            <div className="card-pad">
              <div style={{ display: 'flex', flexDirection: 'column', gap: 8, marginBottom: 12 }}>
                {templateModels.length === 0 && <div className="muted" style={{ fontSize: 13 }}>No models yet.</div>}
                {templateModels.map((m) => (
                  <div key={m.name} style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', gap: 8, fontSize: 13 }}>
                    <span className="mono">{m.name}</span>
                    <span style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
                      <span className="pill neutral">{m.credentialRef ? 'keyed' : 'public'}</span>
                      <button className="btn sm ghost" disabled={llmBusy} onClick={() => startEditLLM(m)}>Edit</button>
                      <button className="btn sm ghost" disabled={llmBusy} onClick={() => removeLLM(m.name)}>Remove</button>
                    </span>
                  </div>
                ))}
              </div>
              {/* One form serves both paths: the name is read-only while editing
                  because it is the model's identity downstream (provider key,
                  model id, credential name) -- renaming is Remove + Add. */}
              {editingModel ? (
                <div className="field" style={{ marginBottom: 8 }}>
                  <label className="label">Editing {editingModel}</label>
                </div>
              ) : (
                <input
                  className="input"
                  placeholder="Model name (sent to the endpoint)"
                  value={llmForm.name}
                  onChange={(e) => setLLMForm((f) => ({ ...f, name: e.target.value }))}
                />
              )}
              <input
                className="input"
                placeholder="Endpoint (API root, e.g. https://api.example.com/v1)"
                value={llmForm.endpoint}
                onChange={(e) => setLLMForm((f) => ({ ...f, endpoint: e.target.value }))}
              />
              <div style={{ margin: '-6px 0 8px', fontSize: 12, color: 'var(--muted)' }}>
                Use the API root -- do not include /chat/completions.
              </div>
              <input
                className="input"
                type="password"
                disabled={llmForm.public}
                placeholder={editingModel ? 'apiKey (leave empty to keep the current one)' : 'apiKey'}
                value={llmForm.apiKey}
                onChange={(e) => setLLMForm((f) => ({ ...f, apiKey: e.target.value }))}
              />
              <label style={{ display: 'flex', alignItems: 'center', gap: 8, fontSize: 13, marginBottom: 10 }}>
                <input
                  type="checkbox"
                  checked={llmForm.public}
                  onChange={(e) => setLLMForm((f) => ({ ...f, public: e.target.checked, apiKey: e.target.checked ? '' : f.apiKey }))}
                />
                Public endpoint -- requires no API key
              </label>
              <button className="btn primary" style={{ width: '100%' }} disabled={llmBusy} onClick={submitLLM}>
                {llmBusy ? 'Saving...' : editingModel ? 'Save Changes' : 'Add LLM'}
              </button>
              {editingModel && (
                <button className="btn" style={{ width: '100%', marginTop: 6 }} disabled={llmBusy} onClick={resetLLMForm}>
                  Cancel
                </button>
              )}
            </div>
          </div>
        </div>

        <div style={{ display: 'flex', flexDirection: 'column', gap: 14, order: -1 }}>
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
        </div>
      </div>

      <div className="card" style={{ marginTop: 14 }}>
        <div className="card-head">
          <span className="card-title">Confirmations</span>
              <span className="card-hint">Operations off the allowlist pause for your approval in chat; safe commands auto-pass</span>
            </div>
            <div className="card-pad">
              <div className="field">
                <label className="label">Confirmation policy</label>
                <select
                  className="input"
                  aria-label="Confirmation policy"
                  value={policySel}
                  disabled={!confirm?.exists || confirmBusy}
                  onChange={(e) => changePolicy(e.target.value)}
                >
                  <option value="">Follow template default ({confirm?.templatePolicy || 'Allowlist'})</option>
                  <option value="Allowlist">Allowlist — safe commands auto, everything else asks</option>
                  <option value="AlwaysAsk">AlwaysAsk — every operation asks</option>
                  <option value="None">None — pass through, audited</option>
                </select>
                {confirm && confirm.exists && (
                  <div style={{ marginTop: 4, fontSize: 12, color: 'var(--muted)' }}>
                    Effective: <span className="pill neutral">{confirm.confirmPolicy || 'None'}</span>
                    {' '}{confirm.override ? 'you override' : 'inherited from template'}
                  </div>
                )}
                {confirm && confirm.exists && (confirm.confirmPolicy === 'Allowlist' || confirm.confirmPolicy === 'AlwaysAsk') && confirm.channel && confirm.channel !== 'up' && (
                  <div style={{ marginTop: 6, fontSize: 12, color: confirm.channel === 'pairing' ? 'var(--warn)' : 'var(--danger)' }}>
                    {confirm.channel === 'unconfigured'
                      ? 'Approval channel is not configured — gated policies cannot be enforced.'
                      : confirm.channel === 'pairing'
                        ? 'Approval channel is pairing (first-time device setup) — it enables automatically in a moment.'
                        : 'Approval channel is unreachable — Allowlist / AlwaysAsk turns are refused until it recovers.'}
                  </div>
                )}
                {confirm && !confirm.exists && (
                  <div style={{ marginTop: 4, fontSize: 12, color: 'var(--muted)' }}>
                    Provision your instance first to configure confirmations.
                  </div>
                )}
              </div>

              {confirm?.confirmPolicy === 'Allowlist' ? (
                <>
                  <div className="field">
                    <label className="label">Allowlist — safe commands that auto-pass</label>
                    <div style={{ display: 'flex', flexDirection: 'column', gap: 6, marginBottom: 8 }}>
                      {(confirm.allowlist || []).map((r) => {
                        const isOwned = confirm.allowlistOwned.some((o) => ruleKey(o) === ruleKey(r))
                        return (
                          <div key={ruleKey(r)} className="rule-row" style={{ display: 'block', padding: '8px 12px', marginBottom: 6 }}>
                            <div style={{ display: 'flex', alignItems: 'center', gap: 9 }}>
                              <WarnIcon />
                              <span className="mono" style={{ flex: 1, minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={allowlistLabel(r)}>
                                {allowlistLabel(r)}
                              </span>
                              <span className={`pill ${isOwned ? 'accent' : 'neutral'}`}>{isOwned ? 'Yours' : 'Platform'}</span>
                              <button className="btn" style={{ padding: '2px 8px', flex: 'none' }} disabled={confirmBusy} onClick={() => removeRule(ruleKey(r))}>
                                Remove
                              </button>
                            </div>
                            {r.argPattern ? (
                              <div className="mono" title={r.argPattern} style={{ marginTop: 4, fontSize: 11, color: 'var(--muted)', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                                argPattern: {r.argPattern}
                              </div>
                            ) : null}
                          </div>
                        )
                      })}
                      {confirm.allowlist.length === 0 && (
                        <div style={{ color: 'var(--muted)', fontSize: 13 }}>Empty allowlist — every command asks.</div>
                      )}
                    </div>
                  </div>
                  <div style={{ display: 'flex', gap: 6, marginBottom: 8 }}>
                    <input className="input" style={{ flex: 1, minWidth: 0 }} placeholder="command, e.g. helm" value={ruleForm.pattern}
                      onChange={(e) => setRuleForm((f) => ({ ...f, pattern: e.target.value }))} />
                    <input className="input" style={{ flex: 1, minWidth: 0 }} placeholder="argPattern (optional)" value={ruleForm.argPattern}
                      onChange={(e) => setRuleForm((f) => ({ ...f, argPattern: e.target.value }))} />
                    <button className="btn" disabled={confirmBusy} onClick={addRule}>Add</button>
                  </div>
                  <button className="btn" disabled={confirmBusy} onClick={resetConfirm}>Reset to template default</button>
                </>
              ) : (
                <div style={{ marginTop: 8, fontSize: 13, color: 'var(--muted)' }}>
                  {confirm?.confirmPolicy === 'AlwaysAsk'
                    ? 'AlwaysAsk asks on every operation — the allowlist is not applied.'
                    : 'None passes everything through (audited) — no allowlist is applied.'}
                </div>
              )}
            </div>
          </div>

      <div className="card" style={{ marginTop: 14 }}>
        <div className="card-head">
          <span className="card-title">Skills</span>
          <span className="card-hint">Platform skills installed into your instance workspace - toggles write enabledSkills (synced on the next supervisor poll)</span>
        </div>
        <div className="card-pad" style={{ paddingTop: 4, paddingBottom: 10 }}>
          <div className="skill-group">Platform Skills</div>
          {skills.length ? (
            skills.map((s) => (
              <div key={s.sk.metadata.name} className="toggle">
                <div className="toggle-info">
                  <div className="toggle-title">
                    {skillSpecStr(s.sk, 'displayName') || s.sk.metadata.name}{' '}
                    <span className="mono" style={{ color: 'var(--muted)', fontWeight: 500 }}>{esc(s.sk.metadata.name)}</span>
                  </div>
                  <div className="toggle-desc">{skillSpecStr(s.sk, 'description') || 'No description'}</div>
                </div>
                <button
                  className="switch"
                  role="switch"
                  aria-checked={s.enabled}
                  aria-label={skillSpecStr(s.sk, 'displayName') || s.sk.metadata.name}
                  onClick={() => toggleSkill(s.sk.metadata.name)}
                />
              </div>
            ))
          ) : (
            <div style={{ color: 'var(--muted)', padding: '8px 0' }}>No registered skills</div>
          )}
        </div>
      </div>
    </div>
  )
}
