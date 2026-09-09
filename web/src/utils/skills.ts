// Shared skill helpers for the market page and the Agent Config skills toggles.
import type { PlatformObject } from '@/api/types'

// enabledSkillsFromInstances extracts the caller's AgentInstance enabledSkills.
// Empty (or a missing instance) is the resolver's "all enabled" baseline.
export function enabledSkillsFromInstances(instances: PlatformObject[]): string[] {
  const es = instances[0]?.spec?.enabledSkills
  return Array.isArray(es) ? (es as string[]) : []
}

// skillSpecStr reads a string field from a Skill CR spec (displayName,
// description, visibility). The market page and the Agent Config skills toggles
// both render from the Skill CRs, so the spec reader lives here rather than
// being duplicated per view.
export function skillSpecStr(sk: PlatformObject, key: string): string {
  const v = sk.spec?.[key]
  return typeof v === 'string' ? v : ''
}
