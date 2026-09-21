// A fake gateway for component tests.
//
// Tests drive the real components and the real `streamSSE` parser against this;
// nothing here stands in for our own code. It answers the routes the Portal
// calls, records what it was asked, and serves the turn route as a genuine SSE
// body so the parser meets the same byte stream it meets in production.
import type { HistoryMessage, PendingApproval, PendingQuestion, SessionInfo, SSEEvent } from '@/api/types'

export interface RecordedRequest {
  path: string
  method: string
  body: unknown
  // The request's headers, when it was made with any. The SSE streams fetch
  // directly rather than through the API client, so who they identify as is
  // something only the request itself can answer.
  headers?: Record<string, string>
}

export interface FakeGatewayInit {
  sessions?: SessionInfo[]
  history?: HistoryMessage[]
  // Per-session history, for a test that needs two conversations to differ. A
  // session with no entry here falls back to `history`.
  historyFor?: Record<string, HistoryMessage[]>
  turnActive?: boolean
  // A session parked on a human answer: what the questions collection serves, so
  // the restore-on-open path can be exercised without a stream.
  pendingQuestions?: PendingQuestion[]
  // A session parked on write approvals: what the approvals collection serves. A
  // list, because one session can hold several pending approvals at once -- and
  // restoring only the newest of them is the bug this shape exists to prevent.
  pendingApprovals?: PendingApproval[]
  // A pending read that could not be answered (the API's 502). An empty set is a
  // 200 with an empty list now, so this is the only way the fake can say "we
  // could not tell" -- which the client must not render as "nothing is parked".
  pendingFails?: boolean
  // A turn-status read that cannot answer (the API's 502), which is not the same
  // answer as "not running" and must not be rendered as one.
  turnCheckFails?: boolean
  // A decision the API refuses (a 5xx, not the 404 of a card that was already
  // settled). The card stays pending and has to say why, so the explanation is
  // part of the card the user is still looking at.
  decisionFails?: boolean
  // The status a refused decision answers with. 409 is the platform saying
  // another client settled the approval first, which is an outcome rather than a
  // failure: the card closes neutrally, with no decision attributed to the user.
  decisionStatus?: number
  // What the attach stream answers instead of a stream: 409 when another tab
  // holds the session's, 500 for "streaming unsupported", and so on. Unset means
  // the request is served like the real one -- a stream while something is
  // parked, and the gate's 404 when nothing is.
  attachStatus?: number
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
   * Flips what /turn answers, so a test can end a turn the view holds no stream
   * for and assert that the view notices without being reloaded.
   */
  setTurnActive(active: boolean): void
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
  /**
   * Pushes frames onto the attach stream (`GET /sessions/{key}/turn/events`), the one
   * a card restored from the pending endpoint opens. The stream is held open
   * like the real one -- a parked run produces nothing until it is answered.
   */
  pushAttach(frames: SSEEvent[]): void
  /**
   * Ends the attach stream without a terminal, which is what the server does
   * when the decision was answered before its subscription was ready: the run's
   * output has already gone past, so there is none for the stream to report.
   */
  endAttach(): void
  /**
   * Holds every response whose path contains `fragment` until the returned
   * function is called. A test uses it to stand in the middle of a request whose
   * timing it cannot otherwise reach -- the read that is still in flight when
   * the user switches sessions, say. The request is recorded when it is made, so
   * the test can also see that it went out.
   */
  holdPath(fragment: string): () => void
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
  // What the questions collection serves. Mutable, because a question that is
  // answered is no longer pending on the gateway either -- and a fake that kept
  // serving it would answer a check the real one would not.
  const pendingQuestions = [...(init.pendingQuestions ?? [])]
  // What the approvals collection serves, mutable for the same reason: an approval
  // that was answered is no longer pending, and a fake still serving it would
  // answer a check the real gateway would not.
  const pendingApprovals = [...(init.pendingApprovals ?? [])]
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
  // The attach stream, held open the same way and for the same reason: the
  // server keeps it open for as long as the run is parked, so a test that does
  // not drive it sees an attach that is simply waiting.
  let attachController: ReadableStreamDefaultController<Uint8Array> | null = null
  let attachHeld: string[] = []
  // Responses a test has asked to hold, matched by the path they answer.
  const holds: { fragment: string; promise: Promise<void>; release: () => void }[] = []
  // What /turn answers. Mutable, so a test can end a turn the view is not
  // streaming and watch it notice.
  let turnActive = init.turnActive ?? false

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
    if (opts.headers) record.headers = { ...(opts.headers as Record<string, string>) }
    requests.push(record)

    // Held requests are recorded first, so a test can see that one went out
    // while it is still waiting for its answer.
    const hold = holds.find((h) => path.includes(h.fragment))
    if (hold) await hold.promise

    if (path === '/api/v1/sessions' && method === 'GET') {
      return json({ sessions })
    }
    const sub = /^\/api\/v1\/sessions\/([^/]+)\/(.+)$/.exec(path)
    if (sub) {
      const key = sub[1] ?? ''
      const known = sessions.some((s) => s.sessionKey === key)
      switch (sub[2]) {
        // The conversation, both halves on one path. POST appends a message and
        // answers with the SSE stream of the turn it starts -- a new
        // conversation is named by the client in this very path, so there is no
        // separate create to fake. GET reads the transcript, nested under
        // `items`, which is the wire shape and not a detail: the client unwraps
        // it, so serving the array directly would test a client that does not
        // exist.
        case 'messages':
          if (method === 'POST') {
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
          return known ? json({ items: init.historyFor?.[key] ?? history }) : json(NOT_FOUND, 404)
        case 'turn':
          // 502, not `{active:false}`: "could not determine" is the API's own
          // answer when the gateway channel cannot be reached, and a fake that
          // answered "not running" would let the client collapse the two.
          if (init.turnCheckFails) return json({ error: 'cannot determine turn state' }, 502)
          return json({ active: turnActive })
        // In production `/abort` does not answer until the session has settled;
        // here it answers at once, so a test using it proves the request was
        // made rather than that the view copes with a slow stop.
        case 'abort':
          return json({})
        case 'approvals/decision':
        case 'questions/answer':
        case 'questions/cancel':
          decisions.push(record)
          // The request was made either way, so it is recorded either way; only
          // the answer differs.
          if (init.decisionFails) return json({ error: 'decision not recorded' }, init.decisionStatus ?? 500)
          if (sub[2] === 'questions/answer') pendingQuestions.length = 0
          if (sub[2] === 'questions/cancel') pendingQuestions.length = 0
          if (sub[2] === 'approvals/decision') {
            const settled = (record.body as { approvalId?: string })?.approvalId
            const at = pendingApprovals.findIndex((a) => a.approvalId === settled)
            if (at >= 0) pendingApprovals.splice(at, 1)
          }
          // A decision names its approval, and the answer says which one was
          // settled -- the client checks the two agree, so a fake that did not
          // echo the id would test a client that cannot.
          return json({ approved: (record.body as { decision?: string })?.decision !== 'reject',
                        decision: (record.body as { decision?: string })?.decision,
                        approvalId: (record.body as { approvalId?: string })?.approvalId })
        // Both are collection reads now: an empty set is 200 with an empty list,
        // not a 404. The 502 the server answers when it cannot read the gateway
        // is the answer that must stay distinct, and `pendingFails` covers it in
        // the tests that care.
        case 'approvals':
          if (init.pendingFails) return json({ error: 'gateway unreadable' }, 502)
          return json({ approvals: pendingApprovals })
        case 'questions':
          return json({ questions: pendingQuestions })
        // The re-attach stream. Held open: a parked run produces nothing while
        // it waits for the human, and the stream the browser opened for it stays
        // up for as long as that lasts.
        case 'turn/events': {
          if (init.attachStatus) {
            return json({ error: 'attach refused' }, init.attachStatus)
          }
          // The route's gate, as the server applies it: a session with nothing
          // parked is a 404, which is what a card answered before the request
          // arrived gets. (The fake's approval lookup always answers 404, so a
          // pending question is the whole of what it can be parked on.)
          if (!pendingQuestions.length) {
            return json({ error: 'no parked turn for this session' }, 404)
          }
          const body = new ReadableStream<Uint8Array>({
            start(c) {
              attachController = c
              for (const chunk of attachHeld) c.enqueue(encoder.encode(chunk))
              attachHeld = []
            },
            cancel() {
              attachController = null
            },
          })
          return new Response(body, {
            status: 200,
            headers: { 'Content-Type': 'text/event-stream' },
          })
        }
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
      attachController?.close()
      attachController = null
      attachHeld = []
      // A request still held would keep its promise pending into the next test.
      for (const h of holds.splice(0)) h.release()
    },
    requests,
    decisions,
    setTurn(frames: SSEEvent[]) {
      turnChunks = frames.map(frame)
    },
    setTurnRaw(chunks: string[]) {
      turnChunks = chunks
    },
    /**
     * Flips what /turn answers, so a test can end a turn the view holds no
     * stream for and assert that the view notices without being reloaded.
     */
    setTurnActive(active: boolean) {
      turnActive = active
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
    pushAttach(frames: SSEEvent[]) {
      const chunks = frames.map(frame)
      if (attachController) {
        for (const chunk of chunks) attachController.enqueue(encoder.encode(chunk))
      } else {
        attachHeld.push(...chunks)
      }
    },
    endAttach() {
      attachController?.close()
      attachController = null
    },
    holdPath(fragment: string) {
      let release!: () => void
      const promise = new Promise<void>((resolve) => {
        release = resolve
      })
      holds.push({ fragment, promise, release })
      return () => {
        const i = holds.findIndex((h) => h.promise === promise)
        if (i >= 0) holds.splice(i, 1)
        release()
      }
    },
  }
}
