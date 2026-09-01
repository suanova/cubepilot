// Skills view -- publish a skill directory to the market and list what is
// published (issue #23). The directory is packed to a gzip tar client-side
// (web/src/utils/pack.ts) and uploaded via the user-facing publish endpoint.
import { useEffect, useRef, useState } from 'react'
import type { ChangeEvent } from 'react'
import { api } from '@/api'
import type { PlatformObject } from '@/api/types'
import { packSkillDir } from '@/utils/pack'
import { showToast } from '@/stores/toast'

// specStr reads a string field from the CR spec (Record<string, unknown>).
function specStr(sk: PlatformObject, key: string): string {
  const v = sk.spec?.[key]
  return typeof v === 'string' ? v : ''
}

export default function SkillsView() {
  const [skills, setSkills] = useState<PlatformObject[]>([])
  const [name, setName] = useState('')
  const [displayName, setDisplayName] = useState('')
  const [description, setDescription] = useState('')
  const [dirFiles, setDirFiles] = useState<File[]>([])
  const [dirName, setDirName] = useState('')
  const [publishing, setPublishing] = useState(false)
  const dirInput = useRef<HTMLInputElement>(null)

  async function loadSkills() {
    try {
      setSkills(await api.listSkills())
    } catch (e) {
      console.error('loadSkills', e)
    }
  }

  useEffect(() => {
    loadSkills()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // A webkitdirectory picker reports every file with a folder-relative path;
  // the leading segment is the chosen directory's name (used as the slug).
  function onPickDir(e: ChangeEvent<HTMLInputElement>) {
    const files = Array.from(e.target.files ?? [])
    setDirFiles(files)
    const first = files.find((f) => f.webkitRelativePath)
    const folder = first ? first.webkitRelativePath.split('/')[0] : ''
    setDirName(folder)
    if (folder) setName(folder)
  }

  async function publish() {
    if (publishing) return
    const slug = name.trim()
    const title = displayName.trim()
    if (!slug || !title) {
      showToast('Name and display name are required')
      return
    }
    if (dirFiles.length === 0) {
      showToast('Pick a skill directory first')
      return
    }
    setPublishing(true)
    try {
      const tar = await packSkillDir(dirFiles)
      const sk = await api.publishSkill(slug, { displayName: title, description: description.trim() || undefined }, tar)
      showToast(`Skill '${sk.metadata.name}' published`)
      setName('')
      setDisplayName('')
      setDescription('')
      setDirFiles([])
      setDirName('')
      if (dirInput.current) dirInput.current.value = ''
      await loadSkills()
    } catch (e) {
      showToast('Publish failed: ' + (e instanceof Error ? e.message : String(e)))
    } finally {
      setPublishing(false)
    }
  }

  return (
    <div className="view active">
      <div className="view-head">
        <div>
          <div className="view-title">Skills</div>
          <div className="view-desc">Publish a skill directory to the market - installed into instances by the supervisor (issue #23)</div>
        </div>
      </div>

      <div className="config-grid">
        <div style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
          <div className="card">
            <div className="card-head">
              <span className="card-title">Publish a Skill</span>
              <span className="card-hint">Pick the skill directory - packed to a gzip tar in the browser</span>
            </div>
            <div className="card-pad">
              <div className="field">
                <label className="label">Skill name (slug)</label>
                <input
                  className="input"
                  placeholder="e.g. harbor-scan (prefilled from the folder name)"
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                />
              </div>
              <div className="field">
                <label className="label">Display name</label>
                <input
                  className="input"
                  placeholder="e.g. Harbor Image Scan"
                  value={displayName}
                  onChange={(e) => setDisplayName(e.target.value)}
                />
              </div>
              <div className="field">
                <label className="label">Description</label>
                <textarea
                  className="input"
                  rows={3}
                  placeholder="One line about what this skill does"
                  value={description}
                  onChange={(e) => setDescription(e.target.value)}
                />
              </div>
              <div className="field" style={{ marginBottom: 0 }}>
                <label className="label">Skill directory</label>
                <input
                  ref={dirInput}
                  type="file"
                  className="input"
                  {...({ webkitdirectory: '' } as Record<string, string>)}
                  onChange={onPickDir}
                />
                {dirName ? (
                  <div style={{ marginTop: 4, fontSize: 12, color: 'var(--muted)' }}>
                    {dirName}/ - {dirFiles.length} file{dirFiles.length === 1 ? '' : 's'}
                  </div>
                ) : null}
              </div>
              <button className="btn primary" style={{ width: '100%', marginTop: 12 }} disabled={publishing} onClick={publish}>
                {publishing ? 'Publishing...' : 'Publish to Market'}
              </button>
            </div>
          </div>
        </div>

        <div style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
          <div className="card">
            <div className="card-head">
              <span className="card-title">Published Skills</span>
              <span className="card-hint">Platform-visible skills in the market - installed per instance (issue #24)</span>
            </div>
            <div className="card-pad" style={{ paddingTop: 4, paddingBottom: 10 }}>
              {skills.length ? (
                skills.map((sk) => {
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
                      <span className="pill neutral">{specStr(sk, 'visibility') || 'Platform'}</span>
                      <span className={`pill ${phase === 'Available' ? 'success' : 'neutral'}`}>{phase || 'unknown'}</span>
                    </div>
                  )
                })
              ) : (
                <div style={{ color: 'var(--muted)', padding: '8px 0' }}>No published skills</div>
              )}
            </div>
          </div>
        </div>
      </div>
    </div>
  )
}
