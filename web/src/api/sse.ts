// SSE streaming helper — reads a fetch Response body chunk by chunk and
// parses `event:`/`data:` blocks (POST SSE cannot use EventSource).
import type { SSEEvent } from './types'

function parseSSEBlock(raw: string, onEvent: (name: string, ev: SSEEvent) => void): void {
  let ev = 'message'
  let data = ''
  for (const line of raw.split('\n')) {
    if (line.startsWith('event:')) ev = line.slice(6).trim()
    else if (line.startsWith('data:')) data += line.slice(5).trim()
  }
  if (!data) return
  try {
    onEvent(ev, JSON.parse(data) as SSEEvent)
  } catch {
    /* malformed frame — ignore */
  }
}

export async function streamSSE(
  url: string,
  opts: RequestInit,
  onEvent: (name: string, ev: SSEEvent) => void,
): Promise<void> {
  const resp = await fetch(url, opts)
  if (!resp.ok) {
    const text = await resp.text().catch(() => '')
    onEvent('message_done', {
      type: 'message_done',
      session_id: '',
      error: `HTTP ${resp.status} ${text}`,
    })
    return
  }
  const reader = resp.body?.getReader()
  if (!reader) return
  const decoder = new TextDecoder()
  let buf = ''
  for (;;) {
    const r = await reader.read()
    if (r.done) break
    buf += decoder.decode(r.value, { stream: true })
    let idx: number
    while ((idx = buf.indexOf('\n\n')) >= 0) {
      parseSSEBlock(buf.slice(0, idx), onEvent)
      buf = buf.slice(idx + 2)
    }
  }
}
