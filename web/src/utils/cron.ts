// Human-readable cron descriptions + validation, shared by the task create
// dialog (live preview + validation) and the task list (Schedule column).
//
// The authoritative parser is the operator's hand-written 5-field grammar in
// internal/schedule/cron.go: per field only "*", "*/n", plain numbers and
// comma-separated lists of those (no ranges a-b, no day/month names, no
// 6-field seconds). cronstrue is used ONLY to describe an already-valid
// expression -- validation here mirrors the backend grammar so the dialog
// never shows an expression the server would later reject.
import cronstrue from 'cronstrue'

export interface CronDescription {
  /** Human text when the expression parses; null when empty or invalid. */
  text: string | null
  /** Hint when the expression is non-empty but not parseable. */
  error: string | null
}

// Field bounds in standard 5-field order: minute hour day-of-month month
// day-of-week. Day-of-week accepts 0 and 7 (both Sunday), as the backend does.
const FIELD_BOUNDS: Array<[number, number]> = [
  [0, 59],
  [0, 23],
  [1, 31],
  [1, 12],
  [0, 7],
]

// tokenOk reports whether one comma-separated token matches the backend
// grammar for a field: "*", "*/n" (n >= 1), or a plain number within bounds.
// Values must be plain digits (like Go strconv.Atoi) -- "1e1" is not a number.
function tokenOk(token: string, min: number, max: number): boolean {
  if (token === '*') return true
  const digits = /^\d+$/
  if (token.startsWith('*/')) {
    const step = token.slice(2)
    return digits.test(step) && Number(step) >= 1
  }
  if (!digits.test(token)) return false
  const n = Number(token)
  return n >= min && n <= max
}

// fieldOk mirrors internal/schedule.parseField: a comma-separated list whose
// entries are each one of the allowed token shapes.
function fieldOk(spec: string, min: number, max: number): boolean {
  if (!spec) return false
  const parts = spec.split(',')
  return parts.every((p) => {
    const t = p.trim()
    return t !== '' && tokenOk(t, min, max)
  })
}

// isValidCron reports whether expr is accepted by the backend 5-field grammar.
export function isValidCron(expr: string): boolean {
  const fields = expr.trim().split(/\s+/)
  if (fields.length !== FIELD_BOUNDS.length) return false
  return FIELD_BOUNDS.every(([min, max], i) => fieldOk(fields[i], min, max))
}

export function cronDescription(expr: string): CronDescription {
  const e = (expr ?? '').trim()
  if (!e) return { text: null, error: null } // empty schedule == manual-only
  if (!isValidCron(e)) {
    return {
      text: null,
      error: 'Invalid cron (5 fields: minute hour day month weekday; numbers, */step, comma lists, e.g. "0 2 * * *")',
    }
  }
  try {
    // verbose fills in wildcard day/hour so "0 2 * * *" reads
    // "At 02:00 AM, every day" instead of the terse "At 02:00 AM".
    return { text: cronstrue.toString(e, { verbose: true }), error: null }
  } catch {
    // Backend-valid expressions are always cronstrue-describable in practice;
    // degrade to the raw expression rather than misreporting it as invalid.
    return { text: e, error: null }
  }
}

// lowercaseFirst turns cronstrue's sentence case ("At 02:00 AM") into a
// clause ("at 02:00 AM") for embedding after a prefix like "Runs".
export function lowercaseFirst(s: string): string {
  return s.length > 0 ? s[0].toLowerCase() + s.slice(1) : s
}
