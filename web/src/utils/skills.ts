// Shared skill helpers for the market page and the Agent Config skills toggles.
import type { PlatformObject } from '@/api/types'

// enabledSkillsFromInstances extracts the caller's AgentInstance enabledSkills.
// Empty (or a missing instance) is the resolver's "all enabled" baseline.
export function enabledSkillsFromInstances(instances: PlatformObject[]): string[] {
  const es = instances[0]?.spec?.enabledSkills
  return Array.isArray(es) ? (es as string[]) : []
}
