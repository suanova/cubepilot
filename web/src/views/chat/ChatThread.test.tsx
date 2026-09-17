// The conversation used on its own, with no session list beside it.
//
// This is the floating widget's shape (issue #30), and the reason the thread
// was extracted at all. It is also the test that would have caught the hook
// quietly depending on something only ChatView provided.
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { installFakeGateway, type FakeGateway } from '@/test/gateway'
import type { HistoryMessage } from '@/api/types'
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

// The same, plus the widget's reopen: closing the panel and opening it again
// re-reads the conversation, which is the one refresh a user performs.
function Reopenable({ sessionKey }: { sessionKey?: string }) {
  const thread = useChatThread({ initialSessionKey: sessionKey, onSessionStarted: () => {} })
  return (
    <>
      <button onClick={thread.refresh}>reopen</button>
      <ChatThread thread={thread} title="Assistant" />
    </>
  )
}

// One tool card, found by what it ran. Its command is on the card either way --
// in the header while closed, in the body once open -- so this names the same
// card in both states.
function cardFor(cmd: string): HTMLElement {
  const card = [...document.querySelectorAll('.tool-card')].find((c) => c.textContent?.includes(cmd))
  if (!card) throw new Error(`no tool card for ${cmd}`)
  return card as HTMLElement
}
function expandStateOf(cmd: string): string | null {
  return cardFor(cmd).querySelector('button.tool-head')?.getAttribute('aria-expanded') ?? null
}

const PODS_TURN = [
  { role: 'user', content: 'is nginx up?' },
  {
    role: 'assistant',
    content: [
      { type: 'text', text: 'Checking the pods.' },
      { type: 'toolCall', id: 'c1', name: 'kubectl_get', arguments: '{"kind":"pods"}' },
    ],
  },
  { role: 'toolResult', content: [{ type: 'text', text: 'pod/nginx Running' }] },
] satisfies HistoryMessage[]

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

  it("keeps the reader's open card on the card they opened when the transcript is reloaded", async () => {
    gateway = installFakeGateway({
      sessions: [{ sessionKey: KEY, title: 'Assistant' }],
      history: PODS_TURN,
    })
    gateway.install()

    render(<Reopenable sessionKey={KEY} />)
    const pods = await screen.findByText('Checking the pods.')
    expect(pods).toBeInTheDocument()

    const user = userEvent.setup()
    await user.click(cardFor('pods').querySelector('button.tool-head')!)
    expect(expandStateOf('pods')).toBe('true')

    // Another client's turn lands in the same conversation, in front of the card
    // the reader has open. Reopening the panel re-reads the transcript, and a
    // card's *place* in it is not an identity: the new turn pushes the reader's
    // card along, and carries a card of its own into the spot the reader's open
    // card used to hold. Keyed by position, the reader's choice is handed to a
    // card they never touched -- and taken away from the one they did.
    gateway.setHistory([
      { role: 'user', content: 'how many nodes?' },
      {
        role: 'assistant',
        content: [
          { type: 'text', text: 'Checking the nodes.' },
          { type: 'toolCall', id: 'c0', name: 'kubectl_get', arguments: '{"kind":"nodes"}' },
        ],
      },
      { role: 'toolResult', content: [{ type: 'text', text: 'node/cube-control-plane Ready' }] },
      ...PODS_TURN,
    ])
    await user.click(screen.getByText('reopen'))

    expect(await screen.findByText('Checking the nodes.')).toBeInTheDocument()
    expect(expandStateOf('nodes')).toBe('false')
    expect(expandStateOf('pods')).toBe('true')
  })
})
