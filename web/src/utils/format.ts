// Shared formatting helpers (ported from the original single-file UI).

export function esc(s: unknown): string {
  return String(s).replace(/[&<>"]/g, (c) => {
    return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c] as string
  })
}

export function fmtTime(v?: string): string {
  if (!v) return '—'
  const d = new Date(v)
  if (isNaN(d.getTime())) return '—'
  const pad = (n: number) => (n < 10 ? '0' + n : '' + n)
  const now = new Date()
  const sameDay = d.toDateString() === now.toDateString()
  return (
    (sameDay ? '' : pad(d.getMonth() + 1) + '-' + pad(d.getDate()) + ' ') +
    pad(d.getHours()) +
    ':' +
    pad(d.getMinutes())
  )
}

export function fmtDuration(a?: string, b?: string): string {
  if (!a || !b) return '—'
  const ms = new Date(b).getTime() - new Date(a).getTime()
  if (isNaN(ms) || ms < 0) return '—'
  const s = Math.round(ms / 1000)
  if (s < 60) return s + 's'
  return Math.floor(s / 60) + 'm ' + (s % 60) + 's'
}

export function fmtUptime(totalSeconds?: number): string {
  if (totalSeconds == null) return '—'
  const m = Math.floor(totalSeconds / 60)
  return m + 'm ' + (totalSeconds % 60) + 's'
}

export function shortSession(s?: string): string {
  s = String(s || '').replace('agent:main:', '')
  if (s.length > 28) s = s.slice(0, 12) + '…' + s.slice(-12)
  return s
}

export function downloadText(name: string, text: string): void {
  const blob = new Blob([text], { type: 'text/plain;charset=utf-8' })
  const a = document.createElement('a')
  a.href = URL.createObjectURL(blob)
  a.download = name
  a.click()
  URL.revokeObjectURL(a.href)
}