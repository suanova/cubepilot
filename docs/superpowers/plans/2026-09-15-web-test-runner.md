# Web Test Runner Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give `web/` a behavioural test runner and a first suite that locks down the chat turn, so the `useChatThread` extraction that follows can be proven behaviour-preserving rather than asserted to be.

**Architecture:** Vitest runs the real components in jsdom. A single fake gateway replaces `globalThis.fetch`, serving the non-streaming routes as JSON and the turn route as a real SSE body, so tests exercise the real `streamSSE` parser and the real components rather than mocks of either.

**Tech Stack:** React 19, TypeScript, Vite 8, Vitest, jsdom, Testing Library.

## Global Constraints

- `web/` is React 19 + TypeScript + Vite 8; Node >= 24, npm >= 9.
- **No product code changes in this plan.** Every task either adds test-only files or wires an existing one into a runner. Nothing under `web/src` outside `web/src/test/` and `web/src/views/ChatView.test.tsx` may change.
- **Tests assert on what the user sees** -- rendered text, accessible roles and labels, and the requests the app made. Never on component internals, hook state, or CSS classes. This is what lets the same suite guard both the pre-extraction and post-extraction code.
- `web/tsconfig.json` has `noUnusedLocals`, `noUnusedParameters` and `strict` on, and `npm run build` runs `tsc -b` over `src` -- so test files are typechecked by the build. Unused imports in a test fail the build.
- Commit messages in English, `git commit -s` for DCO, plus an `Assisted-by: Claude Code` trailer.
- Run every command from `web/`.

## Scope

This is **step 1 of 4** in the design (`docs/superpowers/specs/2026-09-15-floating-assistant-widget-design.md`), and its own PR. Steps 2-4 (`refactor` the thread out of ChatView, `fix` the history 404, `feat` the widget) get their own plans, because step 2's exact code cannot be planned honestly until this harness exists to verify it against.

## File Structure

| File | Responsibility |
| --- | --- |
| `web/vitest.config.ts` | Merges the app's Vite config (so `@/` resolves) with the test environment. |
| `web/src/test/setup.ts` | jsdom-level setup: Testing Library's DOM matchers, and a `fetch` guard that fails a test that forgot to install the fake gateway. |
| `web/src/test/gateway.ts` | The fake gateway: routes, recorded requests, canned replies, an SSE body for the turn route. |
| `web/src/test/gateway.test.ts` | Proves the fake gateway itself behaves -- a test helper that lies would make every later test meaningless. |
| `web/src/views/ChatView.test.tsx` | The turn-level suite: streaming, tool cards, approval, question, Stop. |
| `.github/workflows/ci.yaml` | Runs the suite in the existing `web` job. |

---

### Task 1: A green runner

**Files:**
- Modify: `web/package.json`
- Create: `web/vitest.config.ts`
- Create: `web/src/test/setup.ts`
- Test: `web/src/test/runner.test.ts`

**Interfaces:**
- Consumes: nothing.
- Produces: `npm test` (single run, for CI) and `npm run test:watch` (local). Every later task runs `npm test`.

- [ ] **Step 1: Install the runner and its jsdom and Testing Library companions**

```bash
cd web
npm install -D vitest jsdom @testing-library/dom @testing-library/react @testing-library/jest-dom @testing-library/user-event
```

`@testing-library/dom` is a peer dependency of `@testing-library/react` v16 and must be installed explicitly; without it the React package fails to resolve at import time.

- [ ] **Step 2: Add the test scripts**

In `web/package.json`, the `scripts` block becomes:

```json
  "scripts": {
    "dev": "vite",
    "build": "tsc -b && vite build",
    "preview": "vite preview",
    "typecheck": "tsc -b",
    "test": "vitest run",
    "test:watch": "vitest"
  },
```

`vitest run` exits after one pass, which is what CI needs; a bare `vitest` would watch forever and hang the job.

- [ ] **Step 3: Add the Vitest config**

Create `web/vitest.config.ts`:

```ts
import { defineConfig, mergeConfig } from 'vitest/config'
import viteConfig from './vite.config'

// The test run reuses the app's Vite config so `@/` resolves to `src/` exactly
// as it does in a build -- a test that imports through `@/` is then exercising
// the same module graph the app ships, not a second one that merely looks like
// it.
export default mergeConfig(
  viteConfig,
  defineConfig({
    test: {
      environment: 'jsdom',
      setupFiles: ['./src/test/setup.ts'],
      include: ['src/**/*.test.{ts,tsx}'],
    },
  }),
)
```

- [ ] **Step 4: Add the setup file**

Create `web/src/test/setup.ts`:

```ts
import '@testing-library/jest-dom/vitest'

// A test that reaches the network is a bug, not a flaky test: jsdom has no
// server behind it, so a forgotten fake gateway would otherwise surface as a
// confusing parse error deep inside `streamSSE` instead of at the call site
// that made the request. Failing loudly here names the real problem.
//
// `src/test/gateway.ts` installs over this; it is installed per test, not here,
// because the recording it does is per-test state.
globalThis.fetch = (() => {
  throw new Error(
    'fetch() was called with no fake gateway installed -- call installFakeGateway() in the test',
  )
}) as unknown as typeof fetch
```

- [ ] **Step 5: Write the smoke test**

Create `web/src/test/runner.test.ts`:

```ts
import { describe, expect, it } from 'vitest'

// Proves the runner, the DOM environment and the DOM matchers are all wired up.
// Deliberately trivial: when this fails, the cause is configuration, and every
// other test's failure is noise on top of it.
describe('test runner', () => {
  it('runs with a DOM and the jest-dom matchers', () => {
    const el = document.createElement('div')
    el.textContent = 'ready'
    document.body.appendChild(el)

    expect(el).toBeInTheDocument()
    expect(el).toHaveTextContent('ready')
  })
})
```

- [ ] **Step 6: Run it and watch it pass**

Run: `npm test`
Expected: one test file, one passing test, exit code 0.

- [ ] **Step 7: Prove the test files do not break the build**

Run: `npm run build`
Expected: `tsc -b` and `vite build` both succeed. This is the step that catches an unused import or a missing type in a test file, because `npm run build` typechecks all of `src` including the tests.

- [ ] **Step 8: Commit**

```bash
git add web/package.json web/package-lock.json web/vitest.config.ts web/src/test/setup.ts web/src/test/runner.test.ts
git commit -s -m "test(web): add vitest and a jsdom test environment (issue #30)

Assisted-by: Claude Code"
```

---

### Task 2: The fake gateway

**Files:**
- Create: `web/src/test/gateway.ts`
- Test: `web/src/test/gateway.test.ts`

**Interfaces:**
- Consumes: `SSEEvent`, `SessionInfo`, `HistoryMessage` from `@/api/types`; `streamSSE` from `@/api/sse`.
- Produces: `installFakeGateway(init?: FakeGatewayInit): FakeGateway`, with the shape below. Tasks 3-5 call it in `beforeEach` and `gateway.restore()` in `afterEach`.

```ts
export interface RecordedRequest {
  path: string       // pathname only, query stripped, e.g. "/api/v1/messages"
  method: string     // upper case
  body: unknown      // parsed JSON body, or undefined
}

export interface FakeGatewayInit {
  sessions?: SessionInfo[]     // served by GET /api/v1/sessions
  history?: HistoryMessage[]   // served by GET /api/v1/sessions/{key}/messages
  turnActive?: boolean         // served by GET /api/v1/sessions/{key}/turn
}

export interface FakeGateway {
  install(): void
  restore(): void
  requests: RecordedRequest[]
  /** Frames the next POST /api/v1/messages streams. Consumed by that call. */
  setTurn(frames: SSEEvent[]): void
  /** Raw chunks for the next POST, for tests that split a frame across reads. */
  setTurnRaw(chunks: string[]): void
  /** Every frame the app POSTed to a /approval or /question route. */
  decisions: RecordedRequest[]
}

/** The wire form of one frame: an `event:` line, a `data:` line, a blank line. */
export function frame(ev: SSEEvent): string
```

- [ ] **Step 1: Write the failing test**

Create `web/src/test/gateway.test.ts`:

```ts
import { afterEach, describe, expect, it } from 'vitest'
import { streamSSE } from '@/api/sse'
import type { SSEEvent } from '@/api/types'
import { frame, installFakeGateway } from './gateway'

let gateway: ReturnType<typeof installFakeGateway>

afterEach(() => {
  gateway?.restore()
})

describe('fake gateway', () => {
  it('serves the session list', async () => {
    gateway = installFakeGateway({ sessions: [{ sessionKey: 'agent:main:conv-1', title: 'One' }] })
    gateway.install()

    const resp = await fetch('/api/v1/sessions')
    expect(await resp.json()).toEqual({ sessions: [{ sessionKey: 'agent:main:conv-1', title: 'One' }] })
  })

  it('streams the turn frames it was given, in order', async () => {
    gateway = installFakeGateway()
    gateway.install()
    const events: SSEEvent[] = [
      { type: 'message_start', sessionId: 'agent:main:conv-1' },
      { type: 'message_delta', sessionId: 'agent:main:conv-1', delta: 'hi' },
      { type: 'message_done', sessionId: 'agent:main:conv-1' },
    ]
    gateway.setTurn(events)

    const seen: SSEEvent[] = []
    await streamSSE('/api/v1/messages', { method: 'POST', body: '{}' }, (_n, ev) => seen.push(ev))

    expect(seen).toEqual(events)
  })

  it('reassembles a frame split across two reads', async () => {
    gateway = installFakeGateway()
    gateway.install()
    const whole = frame({ type: 'message_delta', sessionId: 's', delta: 'split' })
    const cut = Math.floor(whole.length / 2)
    gateway.setTurnRaw([whole.slice(0, cut), whole.slice(cut)])

    const seen: SSEEvent[] = []
    await streamSSE('/api/v1/messages', { method: 'POST', body: '{}' }, (_n, ev) => seen.push(ev))

    expect(seen).toEqual([{ type: 'message_delta', sessionId: 's', delta: 'split' }])
  })

  it('records the requests the app made', async () => {
    gateway = installFakeGateway()
    gateway.install()

    await fetch('/api/v1/messages', { method: 'POST', body: JSON.stringify({ content: 'hello' }) })

    expect(gateway.requests).toHaveLength(1)
    expect(gateway.requests[0]).toMatchObject({
      path: '/api/v1/messages',
      method: 'POST',
      body: { content: 'hello' },
    })
  })

  it('reports a missing conversation as a 404, like the gateway does', async () => {
    gateway = installFakeGateway()
    gateway.install()

    const resp = await fetch('/api/v1/sessions/agent:main:nope/messages')
    expect(resp.status).toBe(404)
  })
})
```

- [ ] **Step 2: Run it and watch it fail**

Run: `npm test -- gateway`
Expected: FAIL -- `Failed to resolve import "./gateway"`.

- [ ] **Step 3: Write the fake gateway**

Create `web/src/test/gateway.ts`:

```ts
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

// A session that does not exist is a 404 from the gateway, not a 5xx -- the
// widget depends on telling "this conversation has not started" apart from
// "the gateway is down", so the fake must not blur the two either.
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
    if (/^\/api\/v1\/sessions\/[^/]+\/messages$/.test(path) && method === 'GET') {
      return history.length ? json(history) : json(NOT_FOUND, 404)
    }
    if (/^\/api\/v1\/sessions\/[^/]+\/turn$/.test(path) && method === 'GET') {
      return json({ active: init.turnActive ?? false })
    }
    // In production `/abort` does not answer until the session has settled;
    // here it answers at once, so a test using it proves the request was made
    // rather than that the view copes with a slow stop.
    if (/^\/api\/v1\/sessions\/[^/]+\/abort$/.test(path) && method === 'POST') {
      return json({})
    }
    if (/^\/api\/v1\/sessions\/[^/]+\/(approval|question)$/.test(path) && method === 'POST') {
      decisions.push(record)
      return json({})
    }
    if (/^\/api\/v1\/sessions\/[^/]+\/(approval|question)\/pending$/.test(path) && method === 'GET') {
      return json({})
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
```

- [ ] **Step 4: Run it and watch it pass**

Run: `npm test -- gateway`
Expected: PASS -- 5 tests.

- [ ] **Step 5: Commit**

```bash
git add web/src/test/gateway.ts web/src/test/gateway.test.ts
git commit -s -m "test(web): add a fake gateway for component tests (issue #30)

Assisted-by: Claude Code"
```

---

### Task 3: The turn, end to end

**Files:**
- Test: `web/src/views/ChatView.test.tsx`

**Interfaces:**
- Consumes: `installFakeGateway`, `FakeGateway` from `@/test/gateway`.
- Produces: the suite file tasks 4 and 5 extend; the `renderChat()` and `send()` helpers they reuse.

- [ ] **Step 1: Write the failing test**

Create `web/src/views/ChatView.test.tsx`:

```tsx
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import ChatView from './ChatView'
import { installFakeGateway, type FakeGateway } from '@/test/gateway'

let gateway: FakeGateway

beforeEach(() => {
  localStorage.setItem('cubepilot.user', 'alice')
  gateway = installFakeGateway()
  gateway.install()
})

afterEach(() => {
  gateway.restore()
})

// The view is driven the way a user drives it: type, click, read the screen.
// Nothing here reaches into component state, so the same assertions hold after
// the thread is extracted out of ChatView.
async function send(text: string) {
  const user = userEvent.setup()
  await user.type(screen.getByLabelText('Message input'), text)
  await user.click(screen.getByLabelText('Send'))
}

describe('ChatView turn', () => {
  it('sends what the user typed and renders the reply as it streams', async () => {
    gateway.setTurn([
      { type: 'message_start', sessionId: 'agent:main:conv-1' },
      { type: 'message_delta', sessionId: 'agent:main:conv-1', delta: 'Cluster ' },
      { type: 'message_delta', sessionId: 'agent:main:conv-1', delta: 'is healthy.' },
      { type: 'message_done', sessionId: 'agent:main:conv-1' },
    ])

    render(<ChatView />)
    await send('how is the cluster?')

    // The request carried the text, and named no session: the first turn of a
    // conversation learns its key from `message_start`, which is the contract
    // the widget's fixed key has to fit into.
    expect(await screen.findByText('how is the cluster?')).toBeInTheDocument()
    expect(gateway.requests.find((r) => r.path === '/api/v1/messages')?.body).toMatchObject({
      sessionId: null,
      content: 'how is the cluster?',
    })

    // Both deltas landed, and the turn reached its terminal: the send control
    // is a Send again rather than a Stop.
    expect(await screen.findByText(/is healthy\./)).toBeInTheDocument()
    expect(await screen.findByLabelText('Send')).toBeInTheDocument()
  })

  it('shows the tool call and its result', async () => {
    gateway.setTurn([
      { type: 'message_start', sessionId: 'agent:main:conv-1' },
      { type: 'tool_call', sessionId: 'agent:main:conv-1', name: 'kubectl_get', callId: 'c1', arguments: '{"kind":"pods"}' },
      { type: 'tool_result', sessionId: 'agent:main:conv-1', callId: 'c1', name: 'kubectl_get', output: 'pod/nginx Running' },
      { type: 'message_done', sessionId: 'agent:main:conv-1' },
    ])

    render(<ChatView />)
    await send('list pods')

    expect(await screen.findByText(/kubectl_get/)).toBeInTheDocument()
    expect(await screen.findByText(/pod\/nginx Running/)).toBeInTheDocument()
  })
})
```

- [ ] **Step 2: Run it and watch it fail**

Run: `npm test -- ChatView`
Expected: FAIL. If the file compiles, the failure is an assertion -- most likely `findByText` timing out, because `send()` posts before the frames are queued. If it fails to compile, fix the import path first.

- [ ] **Step 3: Diagnose and fix**

There is no product code to write here -- the deliverable is the test file. A failure is one of three things, and which one it is decides the fix:

| Symptom | Cause | Fix |
| --- | --- | --- |
| `fake gateway has no route for GET /api/v1/...` | `ChatView` calls a route the fake does not serve | Add it to `gateway.ts`, following the shape of the routed blocks already there |
| `findByText` times out, but the request in `gateway.requests` looks right | the view really is not rendering it | Read the render path in `ChatView.tsx` and correct the assertion's expectation |
| `findByText` times out and `gateway.requests` is empty | the click never reached `sendMessage` | check the label, and check that the frames were queued before `send()` |

Do not weaken an assertion to get past a failure. Either the test is wrong about the contract or the view has a bug, and the second is exactly what this suite exists to catch before the refactor in step 2.

- [ ] **Step 4: Run it and watch it pass**

Run: `npm test -- ChatView`
Expected: PASS -- 2 tests.

- [ ] **Step 5: Commit**

```bash
git add web/src/views/ChatView.test.tsx web/src/test/gateway.ts
git commit -s -m "test(web): cover the chat turn end to end (issue #30)

Assisted-by: Claude Code"
```

---

### Task 4: The write-confirmation and question cards

**Files:**
- Modify: `web/src/views/ChatView.test.tsx`

**Interfaces:**
- Consumes: the `send()` helper and `gateway` from Task 3.
- Produces: nothing new. These tests are the safety net the `useChatThread` extraction is verified against.

- [ ] **Step 1: Write the failing tests**

Append to `web/src/views/ChatView.test.tsx`:

```tsx
describe('ChatView write confirmation', () => {
  it('offers Approve and Reject, and posts the decision the user picked', async () => {
    gateway.setTurn([
      { type: 'message_start', sessionId: 'agent:main:conv-1' },
      { type: 'approval_pending', sessionId: 'agent:main:conv-1', callId: 'a1', name: 'kubectl_apply', command: 'kubectl apply -f dev.yaml', level: 'write' },
    ])

    render(<ChatView />)
    await send('create a dev environment')

    const approve = await screen.findByRole('button', { name: 'Approve' })
    await userEvent.setup().click(approve)

    expect(gateway.decisions).toHaveLength(1)
    expect(gateway.decisions[0]).toMatchObject({
      path: '/api/v1/sessions/agent:main:conv-1/approval',
      body: { decision: 'approve' },
    })
  })

  it('posts a rejection when the user picks Reject', async () => {
    gateway.setTurn([
      { type: 'message_start', sessionId: 'agent:main:conv-1' },
      { type: 'approval_pending', sessionId: 'agent:main:conv-1', callId: 'a1', name: 'kubectl_apply', command: 'kubectl apply -f dev.yaml', level: 'write' },
    ])

    render(<ChatView />)
    await send('create a dev environment')

    await userEvent.setup().click(await screen.findByRole('button', { name: 'Reject' }))

    expect(gateway.decisions[0]).toMatchObject({
      path: '/api/v1/sessions/agent:main:conv-1/approval',
      body: { decision: 'reject' },
    })
  })

  it('renders the resolved decision rather than leaving live buttons', async () => {
    gateway.setTurn([
      { type: 'message_start', sessionId: 'agent:main:conv-1' },
      { type: 'approval_pending', sessionId: 'agent:main:conv-1', callId: 'a1', name: 'kubectl_apply', command: 'kubectl apply -f dev.yaml', level: 'write' },
      { type: 'approval_resolved', sessionId: 'agent:main:conv-1', callId: 'a1', approved: true },
      { type: 'message_done', sessionId: 'agent:main:conv-1' },
    ])

    render(<ChatView />)
    await send('create a dev environment')

    expect(await screen.findByText('Approved')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Approve' })).not.toBeInTheDocument()
  })
})

describe('ChatView question', () => {
  it('offers the options and posts the one the user picked', async () => {
    gateway.setTurn([
      { type: 'message_start', sessionId: 'agent:main:conv-1' },
      {
        type: 'question_pending',
        sessionId: 'agent:main:conv-1',
        callId: 'q1',
        question: {
          questions: [
            {
              questionId: 'env',
              header: 'Environment',
              question: 'Which environment?',
              options: [{ label: 'dev' }, { label: 'prod' }],
            },
          ],
        },
      },
    ])

    render(<ChatView />)
    await send('deploy something')

    await userEvent.setup().click(await screen.findByRole('button', { name: 'dev' }))

    expect(gateway.decisions).toHaveLength(1)
    expect(gateway.decisions[0]).toMatchObject({
      path: '/api/v1/sessions/agent:main:conv-1/question',
      body: { id: 'q1', answers: { env: ['dev'] } },
    })
  })
})
```

- [ ] **Step 2: Run them and watch them fail**

Run: `npm test -- ChatView`
Expected: FAIL on the new tests.

- [ ] **Step 3: Diagnose and fix**

The card markup lives in `ChatView.tsx`: the write card around lines 1700-1780 (`Approve`, `Reject`, and the allow-always button, with the settled label at ~1724), the question card's `QuestionCard` component from line 189. Read the branch you are failing on, then:

| Symptom | Cause | Fix |
| --- | --- | --- |
| the card never appears | the event payload does not match the field names the view reads | check the event against `SSEApprovalPending` / `SSEQuestionPending` in `web/src/api/types.ts` and correct the test |
| the option button's name is not `dev` | the question card labels options differently | read `QuestionCard`'s option rendering and use what it actually renders |
| `gateway.decisions` is empty after the click | the decision route is missing, or the click did not land | check the decision routes in the fake gateway's `handler` -- if they are there, the click missed |

The answer body for a question is `{ id, answers: { <questionId>: [<label>] } }`, which is what `api.postQuestion` puts on the wire (`web/src/api/index.ts`:68-70).

- [ ] **Step 4: Run them and watch them pass**

Run: `npm test -- ChatView`
Expected: PASS -- 6 tests.

- [ ] **Step 5: Commit**

```bash
git add web/src/views/ChatView.test.tsx web/src/test/gateway.ts
git commit -s -m "test(web): cover the approval and question cards (issue #30)

Assisted-by: Claude Code"
```

---

### Task 5: Stop

**Files:**
- Modify: `web/src/views/ChatView.test.tsx`

**Interfaces:**
- Consumes: the `send()` helper and `gateway` from Task 3.
- Produces: nothing new.

- [ ] **Step 1: Write the failing test**

Append to `web/src/views/ChatView.test.tsx`:

```tsx
describe('ChatView stop', () => {
  it('replaces Send with Stop while a turn runs, and posts an abort', async () => {
    // No `message_done`: the turn stays open for the duration of the test, so
    // the view is in its streaming state when Stop is pressed.
    gateway.setTurn([
      { type: 'message_start', sessionId: 'agent:main:conv-1' },
      { type: 'message_delta', sessionId: 'agent:main:conv-1', delta: 'working' },
    ])

    render(<ChatView />)
    await send('do something long')

    const stop = await screen.findByLabelText('Stop')
    await userEvent.setup().click(stop)

    expect(gateway.requests.some((r) => r.path === '/api/v1/sessions/agent:main:conv-1/abort')).toBe(true)
  })
})
```

- [ ] **Step 2: Run it and watch it fail**

Run: `npm test -- ChatView`
Expected: FAIL on the new test.

- [ ] **Step 3: Diagnose and fix**

`stopTurn` (`web/src/views/ChatView.tsx`:943) returns early when `currentSessionId` is null, so the abort is only posted once `message_start` has named the session -- which the queued frames do before the test clicks. Two likely failures:

| Symptom | Cause | Fix |
| --- | --- | --- |
| `findByLabelText('Stop')` times out | the turn already ended, so Send is showing | the queued frames must have no `message_done` |
| no `/abort` in `gateway.requests` | the route is missing from the fake, or `stopTurn` returned early | add the route; if it is present, assert on `currentSessionId` being set by checking the `message_start` frame landed first |

`/abort` in production does not answer until the session settles, while the fake answers immediately -- so this test proves the request was made, not that the view behaves correctly against a slow abort. That case is covered by the existing `/turn` banner path, not here.

- [ ] **Step 4: Run it and watch it pass**

Run: `npm test -- ChatView`
Expected: PASS -- 7 tests.

- [ ] **Step 5: Commit**

```bash
git add web/src/views/ChatView.test.tsx web/src/test/gateway.ts
git commit -s -m "test(web): cover stopping a running turn (issue #30)

Assisted-by: Claude Code"
```

---

### Task 6: CI runs the suite

**Files:**
- Modify: `.github/workflows/ci.yaml` (the `web` job, around lines 99-134)

**Interfaces:**
- Consumes: `npm test` from Task 1.
- Produces: nothing.

- [ ] **Step 1: Add the test step to the web job**

In `.github/workflows/ci.yaml`, in the `web` job, immediately after the existing `npm run build` step:

```yaml
      - name: Test
        run: npm test
        working-directory: web
```

Read the surrounding block first and copy its exact indentation -- the steps are nested under `jobs.web.steps`, and a step at the wrong indent is silently ignored by GitHub rather than reported as an error.

- [ ] **Step 2: Verify it locally the way CI runs it**

Run: `cd web && npm ci && npm test`
Expected: PASS, exit code 0. Do not skip the `npm ci`: it proves the lockfile carries the new devDependencies, which `npm install` alone would not catch if the lockfile were left stale.

- [ ] **Step 3: Commit**

```bash
git add .github/workflows/ci.yaml
git commit -s -m "ci(web): run the web test suite (issue #30)

Assisted-by: Claude Code"
```

---

## What this plan does not do

- It does not touch `ChatView.tsx`. The suite exists to make the *next* plan's refactor safe, and a test suite written alongside the change it is meant to guard guards nothing.
- It does not test the widget, because the widget does not exist yet.
- It does not add coverage thresholds or a coverage reporter. There is no number worth defending until the suite has grown past one component.
