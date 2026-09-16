// A fake gateway for component tests.
//
// Tests drive the real components and the real `streamSSE` parser against this;
// nothing here stands in for our own code. It answers the routes the Portal
// calls, records what it was asked, and serves the turn route as a genuine SSE
// body so the parser meets the same byte stream it meets in production.
import type { HistoryMessage, PendingQuestion, SessionInfo, SSEEvent } from '@/api/types'

export interface RecordedRequest {
  path: string
  method: string
  body: unknown
}

export interface FakeGatewayInit {
  sessions?: SessionInfo[]
  history?: HistoryMessage[]
  turnActive?: boolean
  // A session parked on a human answer: what `/question/pending` serves, so the
  // restore-on-open path can be exercised without a stream.
  pendingQuestions?: PendingQuestion[]
  // A turn-status read that cannot answer (the API's 502), which is not the same
  // answer as "not running" and must not be rendered as one.
  turnCheckFails?: boolean
}

/** A turn whose stream stays open until the test closes it. */
export interface OpenTurn {
  push(frames: SSEEvent[]): void
  close(): void
}

export interface FakeGateway {
  install(): void
  restore(): void
  requests: RecordedRequest[]
  decisions: RecordedRequest[]
  setTurn(frames: SSEEvent[]): void
  setTurnRaw(chunks: string[]): void
  /**
   * Leaves the next turn's stream open instead of ending it, so a test can act
   * on a turn that is genuinely still running. Call it before the app sends.
   */
  openTurn(): OpenTurn
  /**
   * Replaces the history this session is served. A conversation is not frozen
   * while the user reads it: another client can add to it, and the view re-reads
   * it -- so a test that reloads has to be able to serve something new.
   */
  setHistory(items: HistoryMessage[]): void
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
  // An open turn's stream. `held` buffers anything pushed before the app has
  // issued the POST that will carry it -- the test cannot know exactly when
  // that lands, and a push that silently vanished would look like a view bug.
  const encoder = new TextEncoder()
  let openMode = false
  let controller: ReadableStreamDefaultController<Uint8Array> | null = null
  let held: string[] = []

  const handler = async (input: RequestInfo | URL, opts: RequestInit = {}): Promise<Response> => {
    const raw = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url
    // Decoded, because every session key carries colons and every caller
    // percent-encodes them -- `agent%3Amain%3Aconv-1` in an assertion says
    // nothing that `agent:main:conv-1` does not, and it is the decoded form the
    // Go handlers see (`r.URL.Path` arrives decoded).
    let path = new URL(raw, 'http://localhost').pathname
    try {
      path = decodeURIComponent(path)
    } catch {
      /* a malformed escape is left as it arrived */
    }
    const method = (opts.method ?? 'GET').toUpperCase()
    const body = typeof opts.body === 'string' ? JSON.parse(opts.body) : undefined
    const record: RecordedRequest = { path, method, body }
    requests.push(record)

    if (path === '/api/v1/sessions' && method === 'GET') {
      return json({ sessions })
    }
    if (path === '/api/v1/messages' && method === 'POST') {
      if (openMode) {
        openMode = false
        const body = new ReadableStream<Uint8Array>({
          start(c) {
            controller = c
            for (const chunk of held) c.enqueue(encoder.encode(chunk))
            held = []
          },
        })
        return new Response(body, {
          status: 200,
          headers: { 'Content-Type': 'text/event-stream' },
        })
      }
      const chunks = turnChunks ?? []
      turnChunks = null
      return sseBody(chunks)
    }
    const sub = /^\/api\/v1\/sessions\/([^/]+)\/(.+)$/.exec(path)
    if (sub) {
      const key = sub[1] ?? ''
      const known = sessions.some((s) => s.sessionKey === key)
      switch (sub[2]) {
        // Nested under `items`, which is the wire shape and not a detail: the
        // client unwraps it, so serving the array directly would test a
        // client that does not exist.
        case 'messages':
          return known ? json({ items: history }) : json(NOT_FOUND, 404)
        case 'turn':
          // 502, not `{active:false}`: "could not determine" is the API's own
          // answer when the gateway channel cannot be reached, and a fake that
          // answered "not running" would let the client collapse the two.
          if (init.turnCheckFails) return json({ error: 'cannot determine turn state' }, 502)
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
        // Nothing parked answers 404, not an empty object: the client unwraps
        // `d.approval` / `d.questions`, so a bare `{}` would hand the caller
        // `undefined` and make it read a field off nothing. This is the shape
        // the API documents (`no pending approval`), and the only one a caller
        // can be written against.
        case 'approval/pending':
          return json({ error: 'no pending approval' }, 404)
        case 'question/pending':
          return json({ questions: init.pendingQuestions ?? [] })
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
      // An open stream outlives the test that opened it unless it is closed
      // here; leaving one dangling would leak a pending reader into the next
      // test in the file.
      controller?.close()
      controller = null
      held = []
      openMode = false
    },
    requests,
    decisions,
    setTurn(frames: SSEEvent[]) {
      turnChunks = frames.map(frame)
    },
    setTurnRaw(chunks: string[]) {
      turnChunks = chunks
    },
    // In place, not reassigned: the handler closes over the array.
    setHistory(items: HistoryMessage[]) {
      history.splice(0, history.length, ...items)
    },
    openTurn() {
      openMode = true
      return {
        push(frames: SSEEvent[]) {
          const chunks = frames.map(frame)
          if (controller) {
            for (const chunk of chunks) controller.enqueue(encoder.encode(chunk))
          } else {
            held.push(...chunks)
          }
        },
        close() {
          controller?.close()
          controller = null
        },
      }
    },
  }
}
