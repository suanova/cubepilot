// The Chat view and the floating widget are two entries into one list of
// conversations, and either can create one.
//
// They hold separate thread instances, so neither can see the other's turn
// start: a conversation created in the widget would be missing from the sidebar
// until the page was reloaded, which is exactly the "pick it up at full width"
// path issue #30 is built around.
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import AssistantWidget, { ASSISTANT_SESSION_KEY } from './AssistantWidget'
import ChatView from './views/ChatView'
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

const listReads = () => gateway!.requests.filter((r) => r.path === '/api/v1/sessions').length

describe('a conversation created in the widget', () => {
  it('reaches the Chat view’s session list without a route change', async () => {
    render(
      <>
        <ChatView />
        <AssistantWidget />
      </>,
    )
    const user = userEvent.setup()

    // The list is read once, when the Chat view mounts.
    expect(listReads()).toBe(1)

    // `message_start` is what creates the conversation server-side, and it is
    // the event the widget has to pass on.
    gateway!.setTurn([
      { type: 'message_start', sessionId: ASSISTANT_SESSION_KEY },
      { type: 'message_delta', sessionId: ASSISTANT_SESSION_KEY, delta: 'hi' },
      { type: 'message_done', sessionId: ASSISTANT_SESSION_KEY },
    ])

    await user.click(screen.getByLabelText('Open the assistant'))
    // Scoped to the panel: both entries render a composer, and the point of the
    // test is that this one is theirs and the sidebar is the other's.
    const panel = document.querySelector('.assistant-panel') as HTMLElement
    await user.type(within(panel).getByLabelText('Message input'), 'hello')
    await user.click(within(panel).getByLabelText('Send'))
    // The turn starting is what creates the conversation server-side.
    expect(await within(panel).findByText('hello')).toBeInTheDocument()

    expect(listReads()).toBeGreaterThan(1)
  })
})

describe('the widget’s conversation key', () => {
  it('is the one the Chat view lists', () => {
    // Both entries address the same conversation; if these ever drift apart,
    // the widget becomes a private thread and issue #30's acceptance criterion
    // is quietly lost.
    expect(ASSISTANT_SESSION_KEY).toBe('agent:main:conv-assistant')
  })
})
