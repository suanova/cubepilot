// The ask-user card's own free-text input.
//
// ask_user declares with `isOther` that the human may answer with their own
// words instead of one of the options, and a question with no options offers
// nothing else at all. Until the card had an input, both were unanswerable:
// the options were the only control, and a question without any was dropped
// server-side.
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { installFakeGateway, type FakeGateway } from '@/test/gateway'
import type { QuestionItem } from '@/api/types'
import { ChatThread } from './ChatThread'
import { useChatThread } from './useChatThread'

let gateway: FakeGateway | undefined

beforeEach(() => {
  // No fake installed here: openQuestion() installs the test's own.
  localStorage.setItem('cubepilot.user', 'alice')
})

afterEach(() => {
  gateway?.restore()
  gateway = undefined
})

const KEY = 'agent:main:conv-question'
const TEXT_LABEL = 'Or type your own answer'

function Standalone({ sessionKey }: { sessionKey?: string }) {
  const thread = useChatThread({ initialSessionKey: sessionKey, onSessionStarted: () => {} })
  return <ChatThread thread={thread} title="Assistant" />
}

// A session parked on one question, served by the recovery path (which is how
// a card is restored with no live stream).
function openQuestion(item: QuestionItem) {
  gateway = installFakeGateway({
    sessions: [{ sessionKey: KEY, title: 'Assistant' }],
    pendingQuestions: [{ id: 'ask_1', questions: [item] }],
  })
  gateway.install()
}

const OPTIONS_QUESTION: QuestionItem = {
  questionId: 'where',
  header: 'Target',
  question: 'Where should I write it?',
  options: [{ label: 'workspace' }, { label: 'home' }],
  isOther: true,
}

function answerBody(): unknown {
  return gateway?.decisions.find((d) => d.path === `/api/v1/sessions/${KEY}/questions/answer`)?.body
}

describe("answering a question with the human's own text", () => {
  it('offers a text input beside the options', async () => {
    openQuestion(OPTIONS_QUESTION)
    render(<Standalone sessionKey={KEY} />)

    expect(await screen.findByLabelText(TEXT_LABEL)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'workspace' })).toBeInTheDocument()
  })

  it("submits the typed answer as the question's answer", async () => {
    openQuestion(OPTIONS_QUESTION)
    render(<Standalone sessionKey={KEY} />)
    const user = userEvent.setup()

    // The card is restored from the pending endpoint, so it is not in the DOM
    // on the first frame.
    const submit = await screen.findByRole('button', { name: 'Submit' })
    expect(submit).toBeDisabled() // nothing chosen or typed yet

    await user.type(screen.getByLabelText(TEXT_LABEL), 'under /srv/data')
    expect(submit).toBeEnabled()
    await user.click(submit)

    expect(answerBody()).toMatchObject({ id: 'ask_1', answers: { where: ['under /srv/data'] } })
  })

  it('drops the typed text once an option is picked', async () => {
    openQuestion(OPTIONS_QUESTION)
    render(<Standalone sessionKey={KEY} />)
    const user = userEvent.setup()

    const input = await screen.findByLabelText(TEXT_LABEL)
    await user.type(input, 'under /srv/data')
    await user.click(screen.getByRole('button', { name: 'workspace' }))

    // One answer per question, so choosing an option discards what was typed.
    // (The gateway would take both on a multiSelect question, but this card does
    // not offer mixing.)
    expect(input).toHaveValue('')
    await user.click(screen.getByRole('button', { name: 'Submit' }))
    expect(answerBody()).toMatchObject({ id: 'ask_1', answers: { where: ['workspace'] } })
  })

  it('renders only the text input for a question with no options', async () => {
    openQuestion({ ...OPTIONS_QUESTION, options: [] })
    render(<Standalone sessionKey={KEY} />)
    const user = userEvent.setup()

    const input = await screen.findByLabelText(TEXT_LABEL)
    expect(screen.queryByRole('group', { name: OPTIONS_QUESTION.question })).not.toBeInTheDocument()

    await user.type(input, 'somewhere else')
    await user.click(screen.getByRole('button', { name: 'Submit' }))

    expect(answerBody()).toMatchObject({ id: 'ask_1', answers: { where: ['somewhere else'] } })
  })
})
