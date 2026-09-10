// SSE streaming helper -- reads a fetch Response body chunk by chunk and
// parses `event:`/`data:` blocks (POST SSE cannot use EventSource).
//
// If the stream ends (or the connection dies) before a terminal message_done
// event arrives, a synthetic message_done carrying an error is emitted so the
// caller can always reset pending UI state instead of spinning forever. It is
// marked `synthetic: true` so a caller can tell it from the server's own
// terminal: this is a transport failure, NOT the end of the turn. The gateway
// run may still be executing server-side, with its approval or question still
// live and answerable, so a caller must not read a synthesized terminal as
// "the turn is over" (see ChatView's message_done handler). An older server
// never sets the marker, and absence keeps meaning a real terminal.
//
// Abort contract: when the optional signal fires, this helper returns silently
// and emits NO terminal event -- not an error, not a done. Callers that abort
// are tearing the stream down themselves (unmount, session switch) and resolve
// their own state, so an event would be noise at best. A caller that wants to
// stop a turn while keeping the UI live must therefore NOT merely abort this
// signal: it has to go through the session abort API so the server closes the
// turn with `message_done { stopped: true }`. Aborting the signal alone would
// emit nothing, and a UI waiting on a terminal event would spin forever.
import type { SSEEvent } from './types'

// emitDone is this helper's own terminal -- never a server one -- so it always
// carries `synthetic: true`. That marker is the only thing that distinguishes
// it: the empty session_id is incidental, and callers must not infer from it.
function emitDone(onEvent: (name: string, ev: SSEEvent) => void, error?: string) {
  onEvent('message_done', { type: 'message_done', session_id: '', error: error || '', synthetic: true })
}

// True when `e` is the error a cancelled fetch / errored stream rejects with.
// Matched by name rather than `instanceof DOMException`, which fails when the
// rejection crosses a realm (worker, iframe) even though the contract -- a
// DOMException named "AbortError" -- is the same.
function isAbortError(e: unknown): boolean {
  return typeof e === 'object' && e !== null && (e as { name?: unknown }).name === 'AbortError'
}

export async function streamSSE(
  url: string,
  opts: RequestInit,
  onEvent: (name: string, ev: SSEEvent) => void,
  signal?: AbortSignal,
  // onHttpError takes over a non-OK response instead of the synthetic
  // message_done below. The session attach stream uses it to treat "another tab
  // already holds this session's stream" as nothing to do, rather than painting
  // a failed turn the caller never started.
  onHttpError?: (status: number, body: string) => void,
): Promise<void> {
  // RequestInit already declares `signal`, so a caller may hand one over either
  // positionally or inside `opts`. Prefer the positional one but fall back to
  // `opts.signal`: spreading `opts` and then assigning a bare `signal` would
  // clobber the caller's controller with `undefined` and make the abort a
  // silent no-op.
  const abortSignal = signal ?? opts.signal
  let resp: Response
  try {
    resp = await fetch(url, { ...opts, signal: abortSignal })
  } catch (e) {
    // A cancel that lands before the response headers arrive rejects the fetch
    // itself, so it never reaches the read loop below. That is the intentional
    // stop (unmount, session switch), not a transport failure -- stay silent
    // rather than paint the user an "AbortError" they did not cause.
    if (abortSignal?.aborted || isAbortError(e)) return
    // Any other pre-response failure is real, and the caller still needs the
    // terminal event it resets its pending state on.
    emitDone(onEvent, String(e))
    return
  }
  if (!resp.ok) {
    const text = await resp.text().catch(() => '')
    if (onHttpError) {
      onHttpError(resp.status, text)
      return
    }
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
  let aborted = false
  for (;;) {
    let r: ReadableStreamReadResult<Uint8Array>
    try {
      r = await reader.read()
    } catch (e) {
      // Discriminate on the error, not on `abortSignal.aborted`: an AbortError
      // is this cancel and is a clean stop, while a connection reset that
      // merely happens to land after an unrelated abort is a real transport
      // failure and must not be swallowed.
      if (isAbortError(e)) {
        aborted = true
        break
      }
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
  // synthesize one so the caller can reset its state. `aborted` is set from the
  // AbortError above -- not from the signal flag -- so a truncation that races
  // an unrelated abort is still reported.
  if (!sawDone && !aborted) {
    emitDone(onEvent, streamError || 'connection closed before the turn finished')
  }
}