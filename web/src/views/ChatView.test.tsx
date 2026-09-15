import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import ChatView from './ChatView'
import { installFakeGateway, type FakeGateway } from '@/test/gateway'
import type { SSEEvent } from '@/api/types'

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

const APPROVAL = [
  { type: 'message_start', sessionId: 'agent:main:conv-1' },
  {
    type: 'approval_pending',
    sessionId: 'agent:main:conv-1',
    callId: 'a1',
    name: 'kubectl_apply',
    command: 'kubectl apply -f dev.yaml',
    level: 'write',
  },
] satisfies SSEEvent[]

describe('ChatView write confirmation', () => {
  it('posts the approval the user picked', async () => {
    gateway!.setTurn(APPROVAL)

    render(<ChatView />)
    await send('create a dev environment')

    await userEvent.setup().click(await screen.findByRole('button', { name: 'Approve' }))

    expect(gateway!.decisions).toHaveLength(1)
    expect(gateway!.decisions[0]).toMatchObject({
      path: '/api/v1/sessions/agent:main:conv-1/approval',
      body: { decision: 'approve' },
    })
  })

  it('posts a rejection when the user picks Reject', async () => {
    gateway!.setTurn(APPROVAL)

    render(<ChatView />)
    await send('create a dev environment')

    await userEvent.setup().click(await screen.findByRole('button', { name: 'Reject' }))

    expect(gateway!.decisions[0]).toMatchObject({
      path: '/api/v1/sessions/agent:main:conv-1/approval',
      body: { decision: 'reject' },
    })
  })

  it('renders the resolved decision rather than leaving live buttons', async () => {
    gateway!.setTurn([
      ...APPROVAL,
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
  it('posts the option the user picked and submitted', async () => {
    gateway!.setTurn([
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

    // Picking is not answering: the option only toggles the selection, and
    // Submit stays disabled until something is picked.
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'dev' }))
    await user.click(screen.getByRole('button', { name: 'Submit' }))

    expect(gateway!.decisions[0]).toMatchObject({
      path: '/api/v1/sessions/agent:main:conv-1/question',
      body: { id: 'q1', answers: { env: ['dev'] } },
    })
  })
})

describe('ChatView stop', () => {
  it('replaces Send with Stop while a turn runs, and posts an abort', async () => {
    // The stream has to stay open for the turn to be genuinely running: a
    // stream that merely ends without `message_done` closes the turn in the
    // view too (the request resolves, so the view leaves its streaming state),
    // which is the bug this test would otherwise be written around.
    const turn = gateway!.openTurn()

    render(<ChatView />)
    await send('do something long')
    turn.push([
      { type: 'message_start', sessionId: 'agent:main:conv-1' },
      { type: 'message_delta', sessionId: 'agent:main:conv-1', delta: 'working' },
    ])

    await userEvent.setup().click(await screen.findByLabelText('Stop'))
    // Only now: the turn is over from the view's side, so nothing later in the
    // test depends on a stream still being open.
    turn.close()

    expect(gateway!.requests.some((r) => r.path === '/api/v1/sessions/agent:main:conv-1/abort')).toBe(
      true,
    )
  })
})
