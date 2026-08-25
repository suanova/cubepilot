// SSE streaming helper -- reads a fetch Response body chunk by chunk and
// parses `event:`/`data:` blocks (POST SSE cannot use EventSource).
//
// If the stream ends (or the connection dies) before a terminal message_done
// event arrives, a synthetic message_done carrying an error is emitted so the
// caller can always reset pending UI state instead of spinning forever.
import type { SSEEvent } from './types'

function emitDone(onEvent: (name: string, ev: SSEEvent) => void, error?: string) {
  onEvent('message_done', { type: 'message_done', session_id: '', error: error || '' })
}

export async function streamSSE(
  url: string,
  opts: RequestInit,
  onEvent: (name: string, ev: SSEEvent) => void,
): Promise<void> {
  const resp = await fetch(url, opts)
  if (!resp.ok) {
    const text = await resp.text().catch(() => '')
    emitDone(onEvent, `HTTP ${resp.status} ${text}`)
    return
  }
  const reader = resp.body?.getReader()
  if (!reader) {
    emitDone(onEvent, 'streaming not supported')
    return
  }
  const decoder = new TextDecoder()
  let buf = ''
  let sawDone = false
  let streamError = ''
  for (;;) {
    let r: ReadableStreamReadResult<Uint8Array>
    try {
      r = await reader.read()
    } catch (e) {
      streamError = String(e)
      break
    }
    if (r.done) break
    buf += decoder.decode(r.value, { stream: true })
    let idx: number
    while ((idx = buf.indexOf('\n\n')) >= 0) {
      const block = buf.slice(0, idx)
      buf = buf.slice(idx + 2)
      const dataLine = block.split('\n').find((l) => l.startsWith('data:'))
      if (!dataLine) continue
      try {
        const ev = JSON.parse(dataLine.slice(5).trim()) as SSEEvent
        if (ev.type === 'message_done') sawDone = true
        onEvent(ev.type || 'message', ev)
      } catch {
        /* malformed frame -- ignore */
      }
    }
  }
  // Terminal event missing (connection dropped / server died mid-stream):
  // synthesize one so the caller can reset its state.
  if (!sawDone) {
    emitDone(onEvent, streamError || 'connection closed before the turn finished')
  }
}