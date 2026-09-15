import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import ChatView from './ChatView'
import { installFakeGateway, type FakeGateway } from '@/test/gateway'

let gateway: FakeGateway | undefined

beforeEach(() => {
  localStorage.setItem('cubepilot.user', 'alice')
  gateway = installFakeGateway()
  gateway.install()
})

afterEach(() => {
  gateway?.restore()
  gateway = undefined
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
    gateway!.setTurn([
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
    expect(gateway!.requests.find((r) => r.path === '/api/v1/messages')?.body).toMatchObject({
      sessionId: null,
      content: 'how is the cluster?',
    })

    // Both deltas landed, and the turn reached its terminal: the send control
    // is a Send again rather than a Stop.
    expect(await screen.findByText(/is healthy\./)).toBeInTheDocument()
    expect(await screen.findByLabelText('Send')).toBeInTheDocument()
  })

  it('shows the tool call and its result', async () => {
    gateway!.setTurn([
      { type: 'message_start', sessionId: 'agent:main:conv-1' },
      {
        type: 'tool_call',
        sessionId: 'agent:main:conv-1',
        name: 'kubectl_get',
        callId: 'c1',
        arguments: '{"kind":"pods"}',
      },
      { type: 'tool_result', sessionId: 'agent:main:conv-1', callId: 'c1', name: 'kubectl_get', output: 'pod/nginx Running' },
      { type: 'message_done', sessionId: 'agent:main:conv-1' },
    ])

    render(<ChatView />)
    await send('list pods')

    expect(await screen.findByText(/kubectl_get/)).toBeInTheDocument()
    expect(await screen.findByText(/pod\/nginx Running/)).toBeInTheDocument()
  })
})
