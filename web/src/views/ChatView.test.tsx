import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
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

// A card restored after a page load (or any turn this view did not start) is
// answered through a stream the tab attaches to itself: the request that carried
// the turn died with the page (issue #167).
describe('ChatView re-attach', () => {
  const SESSIONS = [{ sessionKey: 'agent:main:conv-1', title: 'Dev environment for nginx' }]
  const parkedCard = [
    {
      id: 'q1',
      questions: [
        {
          questionId: 'specs',
          header: 'Compute',
          question: 'What compute spec?',
          options: [{ label: '4C / 16Gi' }],
        },
      ],
    },
  ]

  // The attach stream is opened by the app rather than by a click, so a test
  // waits for the request instead of driving it.
  async function attachRequest() {
    await waitFor(() => {
      expect(gateway!.requests.some((r) => r.path.endsWith('/stream'))).toBe(true)
    })
    return gateway!.requests.find((r) => r.path.endsWith('/stream'))!
  }

  it('identifies itself as the user the card was restored for', async () => {
    // streamSSE fetches directly rather than through the API client, so the
    // identity every other request carries has to be put on this one by hand.
    // Without it the server resolves the default user, and a card restored under
    // a selected one is attached on a gateway that holds nothing for the
    // session: a 404, and the answer's output has nowhere to land.
    localStorage.setItem('cubepilot.user', 'bob')
    gateway = installFakeGateway({
      sessions: SESSIONS,
      turnActive: true,
      pendingQuestions: parkedCard,
    })
    gateway.install()

    const user = userEvent.setup()
    render(<ChatView />)
    await user.click(await screen.findByText('Dev environment for nginx'))
    expect(await screen.findByText('What compute spec?')).toBeInTheDocument()

    const attach = await attachRequest()
    expect(attach.headers?.['X-CubePilot-User']).toBe('bob')
    // The same identity the card was restored with, which is what makes the two
    // halves find the same session.
    expect(
      gateway!.requests.find((r) => r.path.endsWith('/question/pending'))?.headers?.['X-CubePilot-User'],
    ).toBe('bob')
  })

  it('catches up on a run another tab holds the stream for', async () => {
    // 409 means the other tab receives every event, so this one shows nothing
    // live -- but it still has to end up with the answer. Nothing here observes
    // the run, so the state has to be asked for, and the reload that follows must
    // not re-attach: the other tab still holds the stream, and a reload that
    // re-opens the card's stream would be refused again for the same reason,
    // which is a catch-up that feeds itself.
    gateway = installFakeGateway({
      sessions: SESSIONS,
      attachStatus: 409,
      history: [{ role: 'assistant', content: [{ type: 'text', text: 'Checking the nodes.' }] }],
      pendingQuestions: parkedCard,
    })
    gateway.install()

    const release = gateway.holdPath('conv-1/stream')

    const user = userEvent.setup()
    render(<ChatView />)
    await user.click(await screen.findByText('Dev environment for nginx'))
    expect(await screen.findByText('What compute spec?')).toBeInTheDocument()
    await attachRequest()

    // The other tab answers and the run finishes while this one's attach is
    // still in flight.
    gateway.setHistory([
      { role: 'assistant', content: [{ type: 'text', text: 'Checking the nodes.' }] },
      { role: 'assistant', content: [{ type: 'text', text: 'All nodes are ready.' }] },
    ])
    release()

    expect(await screen.findByText(/All nodes are ready\./, undefined, { timeout: 5000 })).toBeInTheDocument()
    // Exactly one attach: the catch-up reload drew the card again and attached
    // nothing, so the refusal did not repeat.
    expect(gateway.requests.filter((r) => r.path.endsWith('conv-1/stream'))).toHaveLength(1)
  }, 15000)

  it('catches up when the card is answered before the attach arrives', async () => {
    // The answer can beat the attach request to the gate, which then answers 404
    // -- "nothing is parked". Only a 409 means another tab owns the stream, so
    // reading every refusal as one leaves this tab on a truncated reply while the
    // run it was watching finishes in the transcript.
    gateway = installFakeGateway({
      sessions: SESSIONS,
      history: [{ role: 'assistant', content: [{ type: 'text', text: 'Checking the nodes.' }] }],
      pendingQuestions: parkedCard,
    })
    gateway.install()

    // The attach is held at the gate until the answer has gone out.
    const release = gateway.holdPath('conv-1/stream')

    const user = userEvent.setup()
    render(<ChatView />)
    await user.click(await screen.findByText('Dev environment for nginx'))
    expect(await screen.findByText('What compute spec?')).toBeInTheDocument()
    await attachRequest()

    await user.click(screen.getByRole('button', { name: '4C / 16Gi' }))
    await user.click(screen.getByRole('button', { name: 'Submit' }))
    gateway.setHistory([
      { role: 'assistant', content: [{ type: 'text', text: 'Checking the nodes.' }] },
      { role: 'assistant', content: [{ type: 'text', text: 'All nodes are ready.' }] },
    ])
    release()

    expect(await screen.findByText(/All nodes are ready\./, undefined, { timeout: 5000 })).toBeInTheDocument()
  }, 15000)

  it('catches up when the attach ends without ever observing the run', async () => {
    // The decision can be answered between the server's parked check and the
    // subscription it opens, and the resumed run's output then goes out to
    // nobody. The attach reports that by ending without a terminal -- and the
    // tab has to catch up from the transcript, which is the only place that
    // output exists now. Without it the card settles and the tab sits on a
    // truncated reply, which is the bug the attach exists to fix.
    gateway = installFakeGateway({
      sessions: SESSIONS,
      history: [{ role: 'assistant', content: [{ type: 'text', text: 'Checking the nodes.' }] }],
      pendingQuestions: parkedCard,
    })
    gateway.install()

    const user = userEvent.setup()
    render(<ChatView />)
    await user.click(await screen.findByText('Dev environment for nginx'))
    expect(await screen.findByText('What compute spec?')).toBeInTheDocument()
    await attachRequest()

    // The answer landed before the attach was ready, the run went on to finish,
    // and its output is in the history by now.
    gateway.setHistory([
      { role: 'assistant', content: [{ type: 'text', text: 'Checking the nodes.' }] },
      { role: 'assistant', content: [{ type: 'text', text: 'All nodes are ready.' }] },
    ])
    gateway.endAttach()

    expect(await screen.findByText(/All nodes are ready\./, undefined, { timeout: 5000 })).toBeInTheDocument()
  }, 15000)
})

// A request that outlives the session it was made for. Both halves of the race
// are ordinary: the user answers or switches while something is still in
// flight, and what comes back belongs to the conversation they left.
describe('ChatView requests that outlive their session', () => {
  const SESSIONS = [
    { sessionKey: 'agent:main:conv-a', title: 'nginx dev environment' },
    { sessionKey: 'agent:main:conv-b', title: 'redis dev environment' },
  ]

  // A macrotask boundary: everything the view does with an answer it has just
  // been given runs in microtasks, so one turn of the event loop is past all of
  // it. That is what lets an assertion below be about what did *not* happen.
  const settle = () => new Promise((resolve) => setTimeout(resolve, 10))

  it('does not paint a history response onto the session the user moved to', async () => {
    gateway = installFakeGateway({
      sessions: SESSIONS,
      historyFor: {
        'agent:main:conv-a': [
          { role: 'assistant', content: [{ type: 'text', text: 'The nginx namespace is ready.' }] },
        ],
        'agent:main:conv-b': [
          { role: 'assistant', content: [{ type: 'text', text: 'The redis namespace is ready.' }] },
        ],
      },
    })
    gateway.install()

    // The first conversation's read is held: the user leaves it while it is
    // still in flight, which is the whole of how a slow answer loses this race.
    const release = gateway.holdPath('agent:main:conv-a/messages')

    const user = userEvent.setup()
    render(<ChatView />)
    await user.click(await screen.findByText('nginx dev environment'))
    await user.click(await screen.findByText('redis dev environment'))
    expect(await screen.findByText(/The redis namespace is ready\./)).toBeInTheDocument()

    release()
    await settle()

    // The read that was in flight for the conversation the user left landed
    // after the one they moved to. Rendering it would put nginx's thread under
    // the redis header -- history is the truth for the session it was read for,
    // and for no other.
    expect(screen.getByText(/The redis namespace is ready\./)).toBeInTheDocument()
    expect(screen.queryByText(/The nginx namespace is ready\./)).not.toBeInTheDocument()
  })

  it('does not attach a stream for a card whose session the user left', async () => {
    // A restored question card is drawn before the approval lookup that follows
    // it is answered. Switching sessions in that window leaves the attach with a
    // session to open a stream for -- and that stream is the session's only one:
    // it would hold it against every legitimate client until the next switch,
    // with nothing on screen to explain why.
    gateway = installFakeGateway({
      sessions: SESSIONS,
      pendingQuestions: [
        {
          id: 'q1',
          questions: [
            {
              questionId: 'specs',
              header: 'Compute',
              question: 'What compute spec?',
              options: [{ label: '4C / 16Gi' }],
            },
          ],
        },
      ],
    })
    gateway.install()

    // No pending approval, and the answer is held until the user has moved on.
    const release = gateway.holdPath('agent:main:conv-a/approval/pending')

    const user = userEvent.setup()
    render(<ChatView />)
    await user.click(await screen.findByText('nginx dev environment'))
    expect(await screen.findByText('What compute spec?')).toBeInTheDocument()

    await user.click(await screen.findByText('redis dev environment'))
    release()
    await settle()

    expect(gateway.requests.filter((r) => r.path.includes('conv-a/stream'))).toEqual([])
  })
})

// The agent narrates between tool calls: what it found, what it is about to do
// (issue #216). The narration belongs between the cards it introduces -- that
// ordering is the whole reason it is not just more reply text -- and it must
// stay out of the reply, which is what the answer panel is for.
describe('ChatView narration', () => {
  // Reads a position out of the rendered thread: the assertions here are about
  // what comes before what, and a text offset says that without depending on
  // which element a piece of text happens to live in.
  function atOf(thread: HTMLElement): (needle: string) => number {
    const text = thread.textContent || ''
    return (needle: string) => text.indexOf(needle)
  }

  it('draws each narration between the tool cards it introduces', async () => {
    gateway!.setTurn([
      { type: 'message_start', sessionId: 'agent:main:conv-1' },
      { type: 'narration', sessionId: 'agent:main:conv-1', blockId: '1', text: '先看看 default 有哪些 Pod。' },
      {
        type: 'tool_call',
        sessionId: 'agent:main:conv-1',
        name: 'exec',
        callId: 'c1',
        arguments: '{"command":"kubectl get pods -n default"}',
      },
      { type: 'tool_result', sessionId: 'agent:main:conv-1', callId: 'c1', name: 'exec', output: 'qwen38-vllm-0 Running' },
      { type: 'narration', sessionId: 'agent:main:conv-1', blockId: '2', text: '有一个没就绪，再看事件。' },
      {
        type: 'tool_call',
        sessionId: 'agent:main:conv-1',
        name: 'exec',
        callId: 'c2',
        arguments: '{"command":"kubectl get events -n default"}',
      },
      { type: 'tool_result', sessionId: 'agent:main:conv-1', callId: 'c2', name: 'exec', output: 'Normal Scheduled' },
      { type: 'message_delta', sessionId: 'agent:main:conv-1', delta: '结论：一切正常。' },
      { type: 'message_done', sessionId: 'agent:main:conv-1' },
    ])

    render(<ChatView />)
    await send('看看 default')

    const thread = document.querySelector('.thread-inner') as HTMLElement
    const at = atOf(thread)
    expect(await within(thread).findByText(/先看看 default 有哪些 Pod。/)).toBeInTheDocument()
    expect(at('先看看 default 有哪些 Pod。')).toBeLessThan(at('kubectl get pods -n default'))
    expect(at('kubectl get pods -n default')).toBeLessThan(at('有一个没就绪，再看事件。'))
    expect(at('有一个没就绪，再看事件。')).toBeLessThan(at('kubectl get events -n default'))

    // The reply is drawn last, and the narration is not part of it: text that
    // lands in the answer panel would also land in the stopped-turn evidence the
    // view keeps about a reply, which is why it is a segment of its own.
    const panel = thread.querySelector('.answer-panel') as HTMLElement
    expect(panel).not.toBeNull()
    expect(panel.textContent).toContain('结论：一切正常。')
    expect(panel.textContent).not.toContain('先看看 default')
  })

  it('replaces a narration block that the gateway resends', async () => {
    const turn = gateway!.openTurn()
    render(<ChatView />)
    await send('看看 default')

    turn.push([
      { type: 'message_start', sessionId: 'agent:main:conv-1' },
      { type: 'narration', sessionId: 'agent:main:conv-1', blockId: '1', text: '先看看' },
      { type: 'narration', sessionId: 'agent:main:conv-1', blockId: '1', text: '先看看 default 有哪些 Pod。' },
    ])

    // The lane is snapshot-per-block, so the second frame is the same block
    // written out again -- not a second paragraph.
    expect(await screen.findByText(/先看看 default 有哪些 Pod。/)).toBeInTheDocument()
    expect(screen.queryByText(/^先看看$/)).not.toBeInTheDocument()
    turn.close()
  }, 15000)

  it('keeps the narration block exactly as the gateway wrote it', async () => {
    gateway!.setTurn([
      { type: 'message_start', sessionId: 'agent:main:conv-1' },
      // An indented block is a code block because of its indentation: a view
      // that trims the snapshot sends it back as prose.
      { type: 'narration', sessionId: 'agent:main:conv-1', blockId: '1', text: '    kubectl get pods -n default\n' },
      { type: 'message_done', sessionId: 'agent:main:conv-1' },
    ])

    render(<ChatView />)
    await send('看看 default')

    const narration = (await waitFor(() => {
      const el = document.querySelector('.narration')
      expect(el).not.toBeNull()
      return el as HTMLElement
    })) as HTMLElement
    expect(narration.querySelector('pre')?.textContent).toContain('kubectl get pods -n default')
  })

  it('keeps a decided confirmation where it happened, not after every card', async () => {
    gateway!.setTurn([
      { type: 'message_start', sessionId: 'agent:main:conv-1' },
      { type: 'narration', sessionId: 'agent:main:conv-1', blockId: '1', text: '这个要删 Pod，需要你确认。' },
      {
        type: 'tool_call',
        sessionId: 'agent:main:conv-1',
        name: 'exec',
        callId: 'c1',
        arguments: '{"command":"kubectl delete pod dzprobe -n default"}',
      },
      {
        type: 'approval_pending',
        sessionId: 'agent:main:conv-1',
        callId: 'a1',
        name: 'exec',
        command: 'kubectl delete pod dzprobe -n default',
        level: 'write',
      },
      { type: 'approval_resolved', sessionId: 'agent:main:conv-1', callId: 'a1', approved: true },
      { type: 'tool_result', sessionId: 'agent:main:conv-1', callId: 'c1', name: 'exec', output: 'pod deleted' },
      {
        type: 'tool_call',
        sessionId: 'agent:main:conv-1',
        name: 'exec',
        callId: 'c2',
        arguments: '{"command":"kubectl get pods -n default"}',
      },
      { type: 'tool_result', sessionId: 'agent:main:conv-1', callId: 'c2', name: 'exec', output: 'dzprobe Terminating' },
      { type: 'message_done', sessionId: 'agent:main:conv-1' },
    ])

    render(<ChatView />)
    await send('把这个 Pod 删了')

    const thread = document.querySelector('.thread-inner') as HTMLElement
    const at = atOf(thread)
    expect(await within(thread).findByText('Approved')).toBeInTheDocument()
    // The record sits with the call it gated -- after that call's card and
    // before the next one -- rather than under every card of the turn.
    expect(at('kubectl delete pod dzprobe')).toBeLessThan(at('Approved'))
    expect(at('Approved')).toBeLessThan(at('kubectl get pods -n default'))
  })
})

describe('ChatView history step boundaries', () => {
  it('reads an unmarked tool row as a tool call plus the answer, not as a step', async () => {
    // The gateway can serve a row holding a tool call and the finished answer
    // (`[toolCall, {text: "Done."}]`), unmarked. Its text is the answer; reading
    // it as narration would invent a step and swallow the reply.
    gateway = installFakeGateway({
      sessions: [{ sessionKey: 'agent:main:conv-1', title: 'Dev environment for nginx' }],
      history: [
        { role: 'user', content: '看看 default' },
        {
          role: 'assistant',
          content: [{ type: 'toolCall', id: 'c1', name: 'exec', arguments: { command: 'kubectl get pods -n default' } }],
        },
        { role: 'toolResult', content: [{ type: 'text', text: 'qwen38-vllm-0 Running' }] },
        {
          role: 'assistant',
          content: [
            { type: 'toolCall', id: 'c2', name: 'exec', arguments: { command: 'kubectl get events' } },
            { type: 'text', text: 'Done.' },
          ],
        },
      ],
    })
    gateway.install()

    const user = userEvent.setup()
    render(<ChatView />)
    await user.click(await screen.findByText('Dev environment for nginx'))

    const thread = document.querySelector('.thread-inner') as HTMLElement
    expect(await within(thread).findByText(/Done\./)).toBeInTheDocument()
    // It is the reply, not a step: the answer panel is where a tool-running turn
    // puts its closing text.
    const panel = thread.querySelector('.answer-panel') as HTMLElement
    expect(panel.textContent).toContain('Done.')
    expect(thread.querySelectorAll('.narration')).toHaveLength(0)
  })

  it('draws nothing for a step the gateway recorded as empty', async () => {
    // `replacementText` is authoritative: an empty one means the gateway
    // published this step with nothing in it, and falling back to the row's own
    // content would resurrect a line the gateway said was empty.
    gateway = installFakeGateway({
      sessions: [{ sessionKey: 'agent:main:conv-1', title: 'Dev environment for nginx' }],
      history: [
        { role: 'user', content: '看看 default' },
        {
          role: 'assistant',
          content: [{ type: 'text', text: '先看看' }],
          openclawStreamFallback: { itemId: 'commentary-0', source: 'segment', replacementText: '' },
        },
      ],
    })
    gateway.install()

    const user = userEvent.setup()
    render(<ChatView />)
    await user.click(await screen.findByText('Dev environment for nginx'))

    const thread = document.querySelector('.thread-inner') as HTMLElement
    expect(await within(thread).findByText('看看 default')).toBeInTheDocument()
    expect(thread.querySelectorAll('.narration')).toHaveLength(0)
  })

  it('keeps a replayed step exactly as the gateway recorded it', async () => {
    gateway = installFakeGateway({
      sessions: [{ sessionKey: 'agent:main:conv-1', title: 'Dev environment for nginx' }],
      history: [
        { role: 'user', content: '看看 default' },
        {
          role: 'assistant',
          content: [{ type: 'text', text: '    kubectl get pods -n default\n' }],
          openclawStreamFallback: {
            itemId: 'commentary-0',
            source: 'segment',
            replacementText: '    kubectl get pods -n default\n',
          },
        },
      ],
    })
    gateway.install()

    const user = userEvent.setup()
    render(<ChatView />)
    await user.click(await screen.findByText('Dev environment for nginx'))

    // An indented block is a code block because of its indentation: a view that
    // trims the recorded text sends it back as prose.
    const narration = (await waitFor(() => {
      const el = document.querySelector('.narration')
      expect(el).not.toBeNull()
      return el as HTMLElement
    })) as HTMLElement
    expect(narration.querySelector('pre')?.textContent).toContain('kubectl get pods -n default')
  })
})

describe('ChatView narration from history', () => {
  it('rebuilds the steps a reloaded turn narrated', async () => {
    gateway = installFakeGateway({
      sessions: [{ sessionKey: 'agent:main:conv-1', title: 'Dev environment for nginx' }],
      history: [
        { role: 'user', content: '看看 default' },
        // The shape the gateway actually records: the step is its own assistant
        // row, marked as commentary, and the tool call is the next one. The
        // marker is what says "this is a step", since a step and the answer are
        // both assistant text.
        {
          role: 'assistant',
          content: [{ type: 'text', text: '先看看 default 有哪些 Pod。' }],
          openclawStreamFallback: {
            itemId: 'commentary-0',
            source: 'segment',
            replacementText: '\n\n先看看 default 有哪些 Pod。\n\n',
          },
        },
        {
          role: 'assistant',
          content: [{ type: 'toolCall', id: 'c1', name: 'exec', arguments: { command: 'kubectl get pods -n default' } }],
        },
        { role: 'toolResult', content: [{ type: 'text', text: 'qwen38-vllm-0 Running' }] },
        { role: 'assistant', content: [{ type: 'text', text: '结论：一切正常。' }] },
      ],
    })
    gateway.install()

    const user = userEvent.setup()
    render(<ChatView />)
    await user.click(await screen.findByText('Dev environment for nginx'))

    const thread = document.querySelector('.thread-inner') as HTMLElement
    expect(await within(thread).findByText(/先看看 default 有哪些 Pod。/)).toBeInTheDocument()
    const text = thread.textContent || ''
    expect(text.indexOf('先看看 default 有哪些 Pod。')).toBeLessThan(text.indexOf('kubectl get pods -n default'))
    // One narration block, and the answer stands alone: the transcript's
    // commentary must not be fused into the reply, which is what a reload did
    // when a step's text was appended to the bubble's text.
    const panel = thread.querySelector('.answer-panel') as HTMLElement
    if (panel) {
      expect(panel.textContent).toContain('结论：一切正常。')
      expect(panel.textContent).not.toContain('先看看 default')
    } else {
      expect(within(thread).getByText(/结论：一切正常。/)).toBeInTheDocument()
    }
  })
})
