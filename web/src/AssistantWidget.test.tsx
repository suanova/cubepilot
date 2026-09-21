import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import AssistantWidget, { ASSISTANT_SESSION_KEY } from './AssistantWidget'
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

// The transcript reads, by method: one path now serves both halves of the
// conversation -- GET reads it, POST appends to it -- so counting by path alone
// would count a send as a read.
const historyReads = () =>
  gateway!.requests.filter(
    (r) => r.method === 'GET' && r.path === `/api/v1/sessions/${ASSISTANT_SESSION_KEY}/messages`,
  )

describe('the floating assistant', () => {
  it('opens the fixed conversation, and keeps it while the panel is collapsed', async () => {
    const turn = gateway!.openTurn()
    render(<AssistantWidget />)

    const user = userEvent.setup()
    await user.click(screen.getByLabelText('Open the assistant'))

    // Bound to one conversation from the first frame: no list to choose from,
    // and its history is already being read.
    expect(historyReads()).toHaveLength(1)

    await user.type(screen.getByLabelText('Message input'), 'hello')
    await user.click(screen.getByLabelText('Send'))
    turn.push([
      { type: 'message_start', sessionId: ASSISTANT_SESSION_KEY },
      { type: 'message_delta', sessionId: ASSISTANT_SESSION_KEY, delta: 'part one ' },
    ])
    expect(await screen.findByText(/part one/)).toBeInTheDocument()

    // Collapse mid-turn. The panel is hidden rather than unmounted, so the
    // stream survives and the rest of the reply still lands.
    await user.click(screen.getByLabelText('Close the assistant'))
    turn.push([{ type: 'message_delta', sessionId: ASSISTANT_SESSION_KEY, delta: 'and part two' }])
    expect(await screen.findByText(/part one and part two/)).toBeInTheDocument()

    turn.close()
    // The message names the fixed conversation in its path -- the widget's key
    // is the client's to choose, and that is what makes it the same
    // conversation as the Chat view's rather than a second one.
    const sent = gateway!.requests.find(
      (r) => r.method === 'POST' && r.path === `/api/v1/sessions/${ASSISTANT_SESSION_KEY}/messages`,
    )
    expect(sent?.body).toMatchObject({ content: 'hello' })
  })

  it('re-reads the conversation when it is reopened', async () => {
    render(<AssistantWidget />)
    const user = userEvent.setup()

    await user.click(screen.getByLabelText('Open the assistant'))
    expect(historyReads()).toHaveLength(1)

    await user.click(screen.getByLabelText('Close the assistant'))
    await user.click(screen.getByLabelText('Open the assistant'))
    expect(historyReads()).toHaveLength(2)

    // The re-read is a refresh, not a navigation away: the session on screen is
    // the same one, so the thread is not blanked while it is in flight.
    expect(historyReads().every((r) => r.path.endsWith('/messages'))).toBe(true)
  })

  it('does not re-read while a turn is streaming here', async () => {
    // The live thread is ahead of what the server would return, so a reopen
    // mid-turn must not reload over it.
    const turn = gateway!.openTurn()
    render(<AssistantWidget />)
    const user = userEvent.setup()

    await user.click(screen.getByLabelText('Open the assistant'))
    await user.type(screen.getByLabelText('Message input'), 'hello')
    await user.click(screen.getByLabelText('Send'))
    turn.push([
      { type: 'message_start', sessionId: ASSISTANT_SESSION_KEY },
      { type: 'message_delta', sessionId: ASSISTANT_SESSION_KEY, delta: 'working' },
    ])
    expect(await screen.findByText(/working/)).toBeInTheDocument()

    await user.click(screen.getByLabelText('Close the assistant'))
    await user.click(screen.getByLabelText('Open the assistant'))

    expect(historyReads()).toHaveLength(1)
    turn.close()
  })
})
