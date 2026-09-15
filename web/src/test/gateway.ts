// A fake gateway for component tests.
//
// Tests drive the real components and the real `streamSSE` parser against this;
// nothing here stands in for our own code. It answers the routes the Portal
// calls, records what it was asked, and serves the turn route as a genuine SSE
// body so the parser meets the same byte stream it meets in production.
import type { HistoryMessage, SessionInfo, SSEEvent } from '@/api/types'

export interface RecordedRequest {
  path: string
  method: string
  body: unknown
}

export interface FakeGatewayInit {
  sessions?: SessionInfo[]
  history?: HistoryMessage[]
  turnActive?: boolean
}

export interface FakeGateway {
  install(): void
  restore(): void
  requests: RecordedRequest[]
  decisions: RecordedRequest[]
  setTurn(frames: SSEEvent[]): void
  setTurnRaw(chunks: string[]): void
}

/** The wire form of one frame: an `event:` line, a `data:` line, a blank line. */
export function frame(ev: SSEEvent): string {
  return `event: ${ev.type}\ndata: ${JSON.stringify(ev)}\n\n`
}

// A session that does not exist is a 404 from the gateway, not a 5xx. The
// widget has to tell "this conversation has not started" apart from "the
// gateway is down", so the fake must not blur the two either -- it answers 404
// for a session it was never told about, and 200 with whatever history it has
// for one it was.
const NOT_FOUND = {
  ok: false,
  error: { type: 'not_found', message: 'Session not found' },
}

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

function sseBody(chunks: string[]): Response {
  const encoder = new TextEncoder()
  const body = new ReadableStream<Uint8Array>({
    start(controller) {
      for (const c of chunks) controller.enqueue(encoder.encode(c))
      controller.close()
    },
  })
  return new Response(body, {
    status: 200,
    headers: { 'Content-Type': 'text/event-stream' },
  })
}

export function installFakeGateway(init: FakeGatewayInit = {}): FakeGateway {
  const real = globalThis.fetch
  const requests: RecordedRequest[] = []
  const decisions: RecordedRequest[] = []
  const sessions = init.sessions ?? []
  const history = init.history ?? []
  // A turn given by `setTurn` is consumed by the next POST, so a test that
  // sends twice gets the frames it queued rather than the previous turn's.
  let turnChunks: string[] | null = null

  const handler = async (input: RequestInfo | URL, opts: RequestInit = {}): Promise<Response> => {
    const raw = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url
    const path = new URL(raw, 'http://localhost').pathname
    const method = (opts.method ?? 'GET').toUpperCase()
    const body = typeof opts.body === 'string' ? JSON.parse(opts.body) : undefined
    const record: RecordedRequest = { path, method, body }
    requests.push(record)

    if (path === '/api/v1/sessions' && method === 'GET') {
      return json({ sessions })
    }
    if (path === '/api/v1/messages' && method === 'POST') {
      const chunks = turnChunks ?? []
      turnChunks = null
      return sseBody(chunks)
    }
    const sub = /^\/api\/v1\/sessions\/([^/]+)\/(.+)$/.exec(path)
    if (sub) {
      const key = decodeURIComponent(sub[1] ?? '')
      const known = sessions.some((s) => s.sessionKey === key)
      switch (sub[2]) {
        case 'messages':
          return known ? json(history) : json(NOT_FOUND, 404)
        case 'turn':
          return json({ active: init.turnActive ?? false })
        // In production `/abort` does not answer until the session has settled;
        // here it answers at once, so a test using it proves the request was
        // made rather than that the view copes with a slow stop.
        case 'abort':
          return json({})
        case 'approval':
        case 'question':
          decisions.push(record)
          return json({})
        case 'approval/pending':
        case 'question/pending':
          return json({})
        default:
          break
      }
    }
    throw new Error(`fake gateway has no route for ${method} ${path}`)
  }

  return {
    install() {
      globalThis.fetch = handler as unknown as typeof fetch
    },
    restore() {
      globalThis.fetch = real
    },
    requests,
    decisions,
    setTurn(frames: SSEEvent[]) {
      turnChunks = frames.map(frame)
    },
    setTurnRaw(chunks: string[]) {
      turnChunks = chunks
    },
  }
}
