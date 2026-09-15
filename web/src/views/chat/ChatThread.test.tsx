// The conversation used on its own, with no session list beside it.
//
// This is the floating widget's shape (issue #30), and the reason the thread
// was extracted at all. It is also the test that would have caught the hook
// quietly depending on something only ChatView provided.
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { installFakeGateway, type FakeGateway } from '@/test/gateway'
import { ChatThread } from './ChatThread'
import { useChatThread } from './useChatThread'

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

const KEY = 'agent:main:conv-assistant'

// What the widget will render: one hook, one thread, no list.
function Standalone({ sessionKey }: { sessionKey?: string }) {
  const thread = useChatThread({ initialSessionKey: sessionKey, onSessionStarted: () => {} })
  return <ChatThread thread={thread} title="Assistant" />
}

describe('a conversation with no session list', () => {
  it('opens the given conversation and continues it', async () => {
    gateway = installFakeGateway({
      sessions: [{ sessionKey: KEY, title: 'Assistant' }],
      history: [{ role: 'user', content: 'earlier question' }],
    })
    gateway.install()
    gateway.setTurn([
      { type: 'message_start', sessionId: KEY },
      { type: 'message_delta', sessionId: KEY, delta: 'and the answer' },
      { type: 'message_done', sessionId: KEY },
    ])

    render(<Standalone sessionKey={KEY} />)

    // Its history is there from the first frame, without the user navigating
    // to it: the widget is bound to one conversation from the moment it opens.
    expect(await screen.findByText('earlier question')).toBeInTheDocument()

    const user = userEvent.setup()
    await user.type(screen.getByLabelText('Message input'), 'follow-up')
    await user.click(screen.getByLabelText('Send'))

    // The first message addresses the fixed conversation rather than leaving
    // the server to mint one -- which is what makes the widget and the Chat
    // view the same conversation instead of two.
    expect(gateway.requests.find((r) => r.path === '/api/v1/messages')?.body).toMatchObject({
      sessionId: KEY,
      content: 'follow-up',
    })
    expect(await screen.findByText(/and the answer/)).toBeInTheDocument()
  })

  it('renders a conversation that has not started as empty, not as an error', async () => {
    // The gateway has never heard of this key: it is minted by the first
    // message. A 404 here is "nothing yet", and painting it as a failure would
    // tell the user their history was erased the moment the runtime hiccuped.
    render(<Standalone sessionKey={KEY} />)

    expect(await screen.findByText('Start a new conversation')).toBeInTheDocument()
    expect(screen.queryByText(/History load failed/)).not.toBeInTheDocument()
  })
})
