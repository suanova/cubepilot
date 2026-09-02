// Human-readable cron descriptions (cronstrue), shared by the task create
// dialog (live preview + validation) and the task list (Schedule column).
// The backend operator uses a hand-written 5-field parser, so this stays on
// 5 fields too; an empty schedule means a manual-only task.
import cronstrue from 'cronstrue'

export interface CronDescription {
  /** Human text when the expression parses; null when empty or invalid. */
  text: string | null
  /** Hint when the expression is non-empty but not parseable. */
  error: string | null
}

export function cronDescription(expr: string): CronDescription {
  const e = (expr ?? '').trim()
  if (!e) return { text: null, error: null } // empty schedule == manual-only
  const fields = e.split(/\s+/)
  if (fields.length !== 5) {
    return { text: null, error: 'Expected 5 cron fields (minute hour day month weekday), e.g. "0 2 * * *"' }
  }
  try {
    // verbose fills in the wildcard day/hour so "0 2 * * *" reads
    // "At 02:00 AM, every day" instead of the terse "At 02:00 AM".
    return { text: cronstrue.toString(e, { verbose: true }), error: null }
  } catch {
    return { text: null, error: 'Invalid cron expression' }
  }
}

// lowercaseFirst turns cronstrue's sentence case ("At 02:00 AM") into a
// clause ("at 02:00 AM") for embedding after a prefix like "Runs".
export function lowercaseFirst(s: string): string {
  return s.length > 0 ? s[0].toLowerCase() + s.slice(1) : s
}
