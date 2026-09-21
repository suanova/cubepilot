import { afterEach, describe, expect, it } from 'vitest'
import { streamSSE } from '@/api/sse'
import type { SSEEvent } from '@/api/types'
import { frame, installFakeGateway, type FakeGateway } from './gateway'

let gateway: FakeGateway | undefined

afterEach(() => {
  gateway?.restore()
  gateway = undefined
})

describe('fake gateway', () => {
  it('serves the session list', async () => {
    gateway = installFakeGateway({ sessions: [{ sessionKey: 'agent:main:conv-1', title: 'One' }] })
    gateway.install()

    const resp = await fetch('/api/v1/sessions')
    expect(await resp.json()).toEqual({
      sessions: [{ sessionKey: 'agent:main:conv-1', title: 'One' }],
    })
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
    await streamSSE('/api/v1/sessions/agent:main:conv-1/messages', { method: 'POST', body: '{}' }, (_n, ev) => seen.push(ev))

    expect(seen).toEqual(events)
  })

  it('reassembles a frame split across two reads', async () => {
    gateway = installFakeGateway()
    gateway.install()
    // Two frames, cut inside the first: the parser has to hold a partial frame
    // until the rest of it arrives. The terminal has to be a real one -- a
    // stream that ends without `message_done` makes `streamSSE` synthesize one,
    // which would be the thing under test instead of the reassembly.
    const whole =
      frame({ type: 'message_delta', sessionId: 's', delta: 'split' }) +
      frame({ type: 'message_done', sessionId: 's' })
    gateway.setTurnRaw([whole.slice(0, 10), whole.slice(10)])

    const seen: SSEEvent[] = []
    await streamSSE('/api/v1/sessions/agent:main:conv-1/messages', { method: 'POST', body: '{}' }, (_n, ev) => seen.push(ev))

    expect(seen).toEqual([
      { type: 'message_delta', sessionId: 's', delta: 'split' },
      { type: 'message_done', sessionId: 's' },
    ])
  })

  it('records the requests the app made', async () => {
    gateway = installFakeGateway()
    gateway.install()

    await fetch('/api/v1/sessions/agent:main:conv-1/messages', { method: 'POST', body: JSON.stringify({ content: 'hello' }) })

    expect(gateway.requests).toHaveLength(1)
    expect(gateway.requests[0]).toMatchObject({
      path: '/api/v1/sessions/agent:main:conv-1/messages',
      method: 'POST',
      body: { content: 'hello' },
    })
  })

  it('holds a turn open until the test closes it', async () => {
    gateway = installFakeGateway()
    gateway.install()
    const turn = gateway.openTurn()

    const seen: SSEEvent[] = []
    const streamed = streamSSE(
      '/api/v1/sessions/agent:main:conv-1/messages',
      { method: 'POST', body: '{}' },
      (_n, ev) => seen.push(ev),
    )
    // Pushed before the POST is issued: the helper buffers rather than drops,
    // because the test cannot know when the request lands.
    turn.push([{ type: 'message_start', sessionId: 's' }])
    await Promise.resolve()
    turn.push([{ type: 'message_done', sessionId: 's' }])
    turn.close()
    await streamed

    expect(seen).toEqual([
      { type: 'message_start', sessionId: 's' },
      { type: 'message_done', sessionId: 's' },
    ])
  })

  it('reports a session it does not know as a 404, like the gateway does', async () => {
    gateway = installFakeGateway({ sessions: [{ sessionKey: 'agent:main:conv-1', title: 'One' }] })
    gateway.install()

    const resp = await fetch('/api/v1/sessions/agent:main:nope/messages')
    expect(resp.status).toBe(404)
  })
})
