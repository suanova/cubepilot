// Market view -- browse/search platform skills and one-click install or
// uninstall (issue #24). The mutation lands in the caller's
// AgentInstance.spec.enabledSkills; the #22 supervisor syncs the workspace.
import { useEffect, useMemo, useState } from 'react'
import { api } from '@/api'
import type { PlatformObject } from '@/api/types'
import { enabledSkillsFromInstances } from '@/utils/skills'
import { showToast } from '@/stores/toast'

// specStr reads a string field from the CR spec (Record<string, unknown>).
function specStr(sk: PlatformObject, key: string): string {
  const v = sk.spec?.[key]
  return typeof v === 'string' ? v : ''
}

export default function MarketView() {
  const [skills, setSkills] = useState<PlatformObject[]>([])
  const [enabled, setEnabled] = useState<string[]>([])
  const [hasInstance, setHasInstance] = useState(false)
  const [query, setQuery] = useState('')
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')

  // Empty enabledSkills means "all enabled" per the resolver baseline.
  const isInstalled = (name: string) => enabled.length === 0 || enabled.includes(name)

  async function load() {
    try {
      const [skillList, instances] = await Promise.all([api.listSkills(), api.listInstances()])
      setSkills(skillList.filter((sk) => sk.status?.phase !== 'Unreachable'))
      setEnabled(enabledSkillsFromInstances(instances))
      setHasInstance(instances.length > 0)
      setError('')
    } catch (e) {
      console.error('loadMarket', e)
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    load()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const visible = useMemo(() => {
    const q = query.trim().toLowerCase()
    return skills.filter((sk) => {
      if (!q) return true
      return (
        (specStr(sk, 'displayName') || '').toLowerCase().includes(q) ||
        sk.metadata.name.toLowerCase().includes(q) ||
        (specStr(sk, 'description') || '').toLowerCase().includes(q)
      )
    })
  }, [skills, query])

  async function toggle(sk: PlatformObject) {
    const name = sk.metadata.name
    const installing = !isInstalled(name)
    try {
      const next = installing ? await api.installSkill(name) : await api.uninstallSkill(name)
      setEnabled(next)
      showToast(installing ? `Skill '${name}' installed` : `Skill '${name}' uninstalled`)
    } catch (e) {
      showToast((installing ? 'Install' : 'Uninstall') + ' failed: ' + (e instanceof Error ? e.message : String(e)))
    }
  }

  return (
    <div className="view active">
      <div className="view-head">
        <div>
          <div className="view-title">Market</div>
          <div className="view-desc">Browse and install platform skills - applied to your instance workspace (issue #24)</div>
        </div>
      </div>

      {!hasInstance && (
        <div className="card" style={{ marginBottom: 14 }}>
          <div className="card-pad">
            <div style={{ color: 'var(--danger)', fontSize: 13 }}>
              You have no instance yet - provision one on the Agent Config page before installing skills.
            </div>
          </div>
        </div>
      )}

      <div className="card">
        <div className="card-head">
          <span className="card-title">Skill Market</span>
          <span className="card-hint">Platform-visible skills - installed into your instance workspace on the next sync</span>
        </div>
        <div className="card-pad">
          <input
            className="input"
            placeholder="Search skills..."
            aria-label="Search skills"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
          />
        </div>
        <div className="card-pad" style={{ paddingTop: 4, paddingBottom: 10 }}>
          {error ? (
            <div style={{ color: 'var(--danger)', padding: '8px 0' }}>Failed to load skills: {error}</div>
          ) : loading ? (
            <div style={{ color: 'var(--muted)', padding: '8px 0' }}>Loading...</div>
          ) : visible.length ? (
            visible.map((sk) => {
              const installed = isInstalled(sk.metadata.name)
              const phase = typeof sk.status?.phase === 'string' ? sk.status.phase : ''
              return (
                <div key={sk.metadata.name} className="toggle">
                  <div className="toggle-info">
                    <div className="toggle-title">
                      {specStr(sk, 'displayName') || sk.metadata.name}{' '}
                      <span className="mono" style={{ color: 'var(--muted)', fontWeight: 500 }}>{sk.metadata.name}</span>
                    </div>
                    <div className="toggle-desc">{specStr(sk, 'description') || 'No description'}</div>
                  </div>
                  <span className={`pill ${phase === 'Available' ? 'success' : 'neutral'}`}>{phase || 'available'}</span>
                  <button
                    className={installed ? 'btn' : 'btn primary'}
                    disabled={!hasInstance}
                    onClick={() => toggle(sk)}
                  >
                    {installed ? 'Uninstall' : 'Install'}
                  </button>
                </div>
              )
            })
          ) : (
            <div style={{ color: 'var(--muted)', padding: '8px 0' }}>No skills match your search</div>
          )}
        </div>
      </div>
    </div>
  )
}
