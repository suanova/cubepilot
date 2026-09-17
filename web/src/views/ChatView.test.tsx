import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { render, screen, within } from '@testing-library/react'
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

  it('shows the tool call as a one-line card that opens on a click', async () => {
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

    // The call is on screen as a headline; its output is not, because a turn
    // that ran a dozen tools would otherwise bury the reply it produced.
    const card = await screen.findByRole('button', { name: /kubectl_get/ })
    expect(card).toHaveAttribute('aria-expanded', 'false')
    expect(screen.queryByText(/pod\/nginx Running/)).not.toBeInTheDocument()

    await userEvent.setup().click(card)

    expect(screen.getByText(/pod\/nginx Running/)).toBeInTheDocument()
  })

  it('leaves a card the reader opened in the conversation they opened it in', async () => {
    gateway = installFakeGateway({
      sessions: [
        { sessionKey: 'agent:main:conv-1', title: 'Dev environment for nginx' },
        { sessionKey: 'agent:main:conv-2', title: 'GPU utilization' },
      ],
      history: [
        { role: 'user', content: 'what is running in the cluster?' },
        {
          role: 'assistant',
          content: [
            { type: 'text', text: 'Checking the default namespace.' },
            { type: 'toolCall', id: 'h1', name: 'kubectl_get', arguments: '{"kind":"pods"}' },
          ],
        },
        { role: 'toolResult', content: [{ type: 'text', text: 'pod/nginx Running' }] },
      ],
    })
    gateway.install()

    const user = userEvent.setup()
    render(<ChatView />)

    await user.click(await screen.findByText('Dev environment for nginx'))
    const card = await screen.findByRole('button', { name: /kubectl_get/ })
    expect(card).toHaveAttribute('aria-expanded', 'false')

    await user.click(card)
    expect(card).toHaveAttribute('aria-expanded', 'true')

    // A card is opened in a conversation, not at a place on the page. The other
    // conversation's cards are ones the reader never touched, even where its
    // transcript puts a card in the same position as the one they opened.
    await user.click(screen.getByText('GPU utilization'))

    expect(await screen.findByRole('button', { name: /kubectl_get/ })).toHaveAttribute('aria-expanded', 'false')
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

// A parked write whose card is taller than the dock can give it: a command long
// enough to fill the cap, plus the agent's own explanation of what it does.
const TALL_APPROVAL = [
  { type: 'message_start', sessionId: 'agent:main:conv-1' },
  {
    type: 'approval_pending',
    sessionId: 'agent:main:conv-1',
    callId: 'a1',
    name: 'kubectl_apply',
    command: `kubectl apply -f ${'dev-'.repeat(30)}env.yaml`,
    level: 'write',
    message: 'This rewrites the dev environment in place.',
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

  it('asks from the composer, where the controls cannot scroll away', async () => {
    const turn = gateway!.openTurn()

    render(<ChatView />)
    await send('create a dev environment')
    turn.push(APPROVAL)

    // Approving a card that sits above the reply meant scrolling up to find the
    // buttons and back down to read what they did. The pending card is under
    // the composer instead, which does not scroll.
    const approve = await screen.findByRole('button', { name: 'Approve' })
    expect(approve.closest('.composer')).not.toBeNull()

    turn.push([{ type: 'approval_resolved', sessionId: 'agent:main:conv-1', callId: 'a1', approved: true }])

    // Once decided the buttons are gone and what is left is a record of the
    // decision, in the thread, where the write happened.
    const record = await screen.findByText('Approved')
    expect(record.closest('.thread')).not.toBeNull()
    expect(screen.queryByRole('button', { name: 'Approve' })).not.toBeInTheDocument()

    turn.close()
  })

  it('keeps the buttons of a write confirmation taller than the dock outside the scrolling part', async () => {
    // The same docking as the ask-user form, and the same failure: the dock caps
    // the card, the card is a flex column, and `.tool-card`'s `overflow:hidden`
    // clips what no longer fits. The decision row is the last child, so a command
    // long enough to reach the cap pushed it out of the card -- a parked write
    // with no visible controls, and a turn that waits on a decision nobody can
    // send. What scrolls is the command and the agent's explanation of it.
    gateway!.setTurn(TALL_APPROVAL)

    render(<ChatView />)
    await send('create a dev environment')

    const approve = await screen.findByRole('button', { name: 'Approve' })
    const body = approve.closest('.tool-card')?.querySelector('.approval-body')
    expect(body).not.toBeNull()
    expect(body!.contains(approve)).toBe(false)
    expect(body!.contains(screen.getByText(/dev-env\.yaml/))).toBe(true)
    expect(body!.contains(screen.getByText('This rewrites the dev environment in place.'))).toBe(true)
  })

  it('keeps the reason a decision failed out of the scrolling part as well', async () => {
    // A decision the API refused leaves the card pending and says why. That
    // explanation is the reason to try again, so it belongs with the buttons and
    // shares their fate: pinned, rather than scrolled to wherever in a long
    // command the reader happens to be.
    gateway = installFakeGateway({ decisionFails: true })
    gateway.install()
    gateway.setTurn(TALL_APPROVAL)

    render(<ChatView />)
    await send('create a dev environment')

    await userEvent.setup().click(await screen.findByRole('button', { name: 'Approve' }))

    const failure = await screen.findByText(/decision not recorded/)
    const body = failure.closest('.tool-card')?.querySelector('.approval-body')
    expect(body).not.toBeNull()
    expect(body!.contains(failure)).toBe(false)
    expect(screen.getByRole('button', { name: 'Approve' })).toBeInTheDocument()
  })
})

describe('ChatView question', () => {
  it('keeps the buttons of a form taller than the dock outside the scrolling part', async () => {
    // A real ask-user form: several questions, several options each, most with a
    // description. It is taller than any dock the composer can give it, and the
    // dock is a flex column -- which shrank the card to its own height while
    // `.tool-card`'s `overflow:hidden` clipped what no longer fit. The options
    // stayed, the buttons went, and the user was left having picked an answer
    // with nothing to submit it with. The part that scrolls is the options.
    gateway!.setTurn([
      { type: 'message_start', sessionId: 'agent:main:conv-1' },
      {
        type: 'question_pending',
        sessionId: 'agent:main:conv-1',
        callId: 'q1',
        question: {
          questions: [
            {
              questionId: 'specs',
              header: 'Compute',
              question: 'What compute spec?',
              options: [{ label: '4C / 16Gi', description: 'enough for CUDA work' }, { label: '8C / 32Gi' }],
            },
            {
              questionId: 'image',
              header: 'Image',
              question: 'Which image?',
              options: [{ label: 'pytorch/pytorch:2.3.1' }, { label: 'I will provide one' }],
            },
          ],
        },
      },
    ])

    render(<ChatView />)
    await send('deploy something')

    const submit = await screen.findByRole('button', { name: 'Submit' })
    const body = submit.closest('.tool-card')?.querySelector('.question-body')
    expect(body).not.toBeNull()
    expect(body!.contains(submit)).toBe(false)
    expect(body!.contains(screen.getByText('What compute spec?'))).toBe(true)
  })

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

// Where a turn's state and its controls are drawn decides whether the user can
// see them at all: the thread scrolls, the header and the composer do not.
describe('ChatView live turn status', () => {
  const SESSIONS = [{ sessionKey: 'agent:main:conv-1', title: 'Dev environment for nginx' }]

  it('reports the running turn in the header, not in the scrollback', async () => {
    const turn = gateway!.openTurn()

    render(<ChatView />)
    await send('deploy the dev environment')
    turn.push([
      { type: 'message_start', sessionId: 'agent:main:conv-1' },
      {
        type: 'tool_call',
        sessionId: 'agent:main:conv-1',
        name: 'kubectl_apply',
        callId: 'c1',
        arguments: '{"command":"kubectl apply -f dev.yaml"}',
      },
    ])

    // The tool has not returned, so its card is open: what it is running is the
    // one thing worth reading before it finishes.
    expect(await screen.findByText('kubectl apply -f dev.yaml')).toBeInTheDocument()

    // The turn's state is on the header, which output cannot push out of view
    // -- and not in the thread, which can.
    const head = document.querySelector('.chat-head') as HTMLElement
    expect(within(head).getByText(/Running 1 tool/)).toBeInTheDocument()
    expect(document.querySelector('.thread')?.textContent).not.toMatch(/Running 1 tool/)

    turn.close()
  })

  it('reports a turn running in another view in the header, and stops it from the composer', async () => {
    // A turn this view holds no stream for -- started in another tab, or left
    // running across a reload. It is still the session on screen, and its state
    // belongs with the conversation, in the line the header keeps for it: the
    // strip that used to carry it sat directly above the input, in the one place
    // the user is reading what they are typing.
    gateway = installFakeGateway({ sessions: SESSIONS, turnActive: true })
    gateway.install()

    const user = userEvent.setup()
    render(<ChatView />)
    await user.click(await screen.findByText('Dev environment for nginx'))

    const head = document.querySelector('.chat-head') as HTMLElement
    expect(await within(head).findByText(/Still running/)).toBeInTheDocument()
    expect(document.querySelector('.composer')?.textContent).not.toMatch(/Still running/)

    // Ending it is the composer's button, the same control that ends a turn this
    // view streams -- one button for "stop the turn", wherever it came from.
    await user.click(screen.getByLabelText('Stop'))

    expect(gateway.requests.some((r) => r.path === '/api/v1/sessions/agent:main:conv-1/abort')).toBe(true)
  })

  it('catches up on a turn it is not streaming once that turn ends', async () => {
    // A turn with no stream of this view's own is only visible through the
    // history that was loaded when the view arrived, so the view has to notice
    // the turn ending: while it does not, the reply stays as truncated as that
    // snapshot was, and the header keeps claiming a turn that is already over.
    gateway = installFakeGateway({
      sessions: SESSIONS,
      turnActive: true,
      history: [{ role: 'assistant', content: [{ type: 'text', text: 'Investigating the node.' }] }],
    })
    gateway.install()

    const user = userEvent.setup()
    render(<ChatView />)
    await user.click(await screen.findByText('Dev environment for nginx'))

    const head = document.querySelector('.chat-head') as HTMLElement
    expect(await within(head).findByText(/Still running/)).toBeInTheDocument()

    // The run ends in the gateway, and the rest of its reply is now in the
    // history. Nothing tells this view directly -- it holds no stream.
    //
    // History first, then the answer that says the turn is over: the view asks
    // on its own schedule, and a poll that lands between the two must find the
    // turn still running rather than reload a history that is missing its tail.
    gateway.setHistory([
      { role: 'assistant', content: [{ type: 'text', text: 'Investigating the node.' }] },
      { role: 'assistant', content: [{ type: 'text', text: 'The node is healthy.' }] },
    ])
    gateway.setTurnActive(false)

    // A regex, like every assertion on assistant text: the reply is rendered
    // through the markdown path, which does not leave it as one text node.
    expect(await screen.findByText(/The node is healthy\./, undefined, { timeout: 5000 })).toBeInTheDocument()
    expect(within(head).queryByText(/Still running/)).not.toBeInTheDocument()
    // The view asks on a timer, so this test waits on one. Its budget is
    // generous so that a slow machine fails the assertion (which says what is
    // missing) rather than the test (which says only that it timed out).
  }, 15000)

  it('keeps the way out beside the header status when the check itself failed', async () => {
    // The API answers 502 when it cannot determine whether the turn is still
    // running. That is not "idle", so it is reported, and the only controls that
    // can get back to an answer are Retry and Dismiss -- in the header, beside
    // the status they belong to.
    gateway = installFakeGateway({ sessions: SESSIONS, turnCheckFails: true })
    gateway.install()

    const user = userEvent.setup()
    render(<ChatView />)
    await user.click(await screen.findByText('Dev environment for nginx'))

    const head = document.querySelector('.chat-head') as HTMLElement
    expect(await within(head).findByText(/Could not check/)).toBeInTheDocument()
    expect(within(head).getByRole('button', { name: 'Retry' })).toBeInTheDocument()
    expect(within(head).getByRole('button', { name: 'Dismiss' })).toBeInTheDocument()

    // Nothing confirmed a turn here, and an abort with no gateway channel to
    // issue its RPC over provably cannot work -- so the composer keeps Send
    // rather than offering a Stop that cannot do anything.
    expect(screen.getByLabelText('Send')).toBeInTheDocument()
    expect(screen.queryByLabelText('Stop')).not.toBeInTheDocument()
  })

  it('says what the turn is parked on rather than that it is still running', async () => {
    // The live case: the platform reports the session as running, and its run is
    // parked on an answer. "Still running" is true and useless -- the answer is
    // what the user is being asked for -- and the parked turn is not a finished
    // one, so the header must not pair it with the done check either.
    gateway = installFakeGateway({
      sessions: SESSIONS,
      turnActive: true,
      pendingQuestions: [
        {
          id: 'q1',
          questions: [
            {
              questionId: 'specs',
              header: 'Compute',
              question: 'What compute spec (CPU/memory/GPU)?',
              options: [{ label: '4C / 16Gi' }],
            },
          ],
        },
      ],
    })
    gateway.install()

    const user = userEvent.setup()
    render(<ChatView />)
    await user.click(await screen.findByText('Dev environment for nginx'))

    const head = document.querySelector('.chat-head') as HTMLElement
    expect(await within(head).findByText('Awaiting your answer...')).toBeInTheDocument()
    expect(within(head).queryByText(/Still running/)).not.toBeInTheDocument()
    expect(head.querySelector('.chat-head-status.done')).toBeNull()
  })
})

// The gateway rewrites the run's commentary after a tool executes. The rewrite
// must not erase what the user was reading when they approved the tool.
describe('ChatView rewritten reply text', () => {
  it('keeps the text a rewrite superseded', async () => {
    gateway!.setTurn([
      { type: 'message_start', sessionId: 'agent:main:conv-1' },
      { type: 'message_delta', sessionId: 'agent:main:conv-1', delta: 'Checking whether dev exists.' },
      {
        type: 'tool_call',
        sessionId: 'agent:main:conv-1',
        name: 'kubectl_get',
        callId: 'c1',
        arguments: '{"kind":"namespaces"}',
      },
      { type: 'tool_result', sessionId: 'agent:main:conv-1', callId: 'c1', name: 'kubectl_get', output: 'dev missing' },
      { type: 'text_replace', sessionId: 'agent:main:conv-1', delta: 'Created namespace dev.' },
      { type: 'message_done', sessionId: 'agent:main:conv-1' },
    ])

    render(<ChatView />)
    await send('create a dev namespace')

    expect(await screen.findByText('Created namespace dev.')).toBeInTheDocument()
    // Superseded, not deleted: still there, behind a disclosure.
    expect(screen.getByText('Earlier (1)')).toBeInTheDocument()
    expect(screen.getByText('Checking whether dev exists.')).toBeInTheDocument()
  })

  it('calls the reply final only once the turn is over', async () => {
    const turn = gateway!.openTurn()

    render(<ChatView />)
    await send('what is the cluster doing?')
    turn.push([
      { type: 'message_start', sessionId: 'agent:main:conv-1' },
      {
        type: 'tool_call',
        sessionId: 'agent:main:conv-1',
        name: 'kubectl_get',
        callId: 'c1',
        arguments: '{"kind":"nodes"}',
      },
      { type: 'message_delta', sessionId: 'agent:main:conv-1', delta: 'Reading the nodes now.' },
    ])

    // Mid-turn text is a reply in progress, and saying otherwise is what made a
    // rewrite look like the answer being taken away.
    expect(await screen.findByText('Reading the nodes now.')).toBeInTheDocument()
    expect(screen.queryByText('Final result')).not.toBeInTheDocument()

    turn.push([{ type: 'message_done', sessionId: 'agent:main:conv-1' }])

    expect(await screen.findByText('Final result')).toBeInTheDocument()
    turn.close()
  })
})
