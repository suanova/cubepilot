// The conversation: header, thread, tool cards, composer.
//
// Rendered by ChatView beside a session list, and (issue #30) by the floating
// widget on its own. It draws what `useChatThread` reports; the only state it
// holds is which tool cards the reader opened, which is a property of looking
// at the thread rather than of the conversation, and is why the two callers can
// share one implementation of the turn.
import { useState } from 'react'
import { getCurrentUser } from '@/api/client'
import { ApprovalCard } from './ApprovalCard'
import { MdText } from './MdText'
import { QuestionCard } from './QuestionCard'
import { ChevronIcon, DoneCheckIcon, EmptyChatIcon, SendIcon, StopIcon, ToolIcon } from './icons'
import { hasTools, headline, pendingCards, statusLine } from './model'
import type { ChatThreadApi } from './useChatThread'

const user = getCurrentUser()
const userInitials = user
  .split(/[._-]/)
  .map((p) => p[0]?.toUpperCase() ?? '')
  .slice(0, 2)
  .join('')

export function ChatThread({ thread, title }: { thread: ChatThreadApi; title: string }) {
  // Bound to the names the markup below was written against, so moving it out
  // of ChatView did not have to touch a line of it -- the diff for that move is
  // a deletion and an insertion, not 300 edits that each have to be read.
  const {
    sessionId: currentSessionId,
    bubbles,
    loadingHistory,
    streaming,
    noStreamTurn,
    runningElsewhere,
    turnCheckFailed,
    stoppingElsewhere,
    allowAlwaysOk,
    threadEl,
    inputEl,
    autoGrow,
    sendMessage,
    stopTurn,
    stopElsewhere,
    retryTurnCheck,
    dismissTurnCheck,
    decide,
    pick,
    typeAnswer,
    submitQuestion,
    dismissQuestion,
  } = thread
  const chatTitle = title

  // The tool cards the reader opened, by position within the conversation. A
  // card's resting state is derived -- open while its tool is in flight, closed
  // once it has returned -- so only the reader's own choices are recorded here,
  // and a card left open is the one thing that does not change under them.
  const [openedTools, setOpenedTools] = useState<Record<string, boolean>>({})

  // What the conversation is doing right now, and what it is waiting on. Both
  // are drawn outside the scrolling thread: the header and the composer are the
  // two parts of the screen that cannot be scrolled away from, which is the
  // whole reason the state of a turn -- and the buttons that let it continue --
  // live there rather than inside the bubble that produced them.
  const turn = headline(bubbles)
  const pending = pendingCards(bubbles)
  const waiting = pending.confirms.length > 0 || pending.questions.length > 0
  // What the header says the conversation is doing, most actionable first.
  //
  // The stop in flight wins the line: a confirmed turn is still running while
  // its abort waits, so testing that first would show "Still running…" for the
  // whole wait and mask the only feedback the click has -- the composer's Stop
  // is a glyph, and cannot say so itself.
  //
  // A turn parked on a human outranks one merely running: the agent is blocked
  // on the user, the controls that unblock it are directly below in the dock,
  // and "still running…" is true and useless next to that. It is also what the
  // header already said about such a turn -- but it said it beside the green
  // done check, because the parked card belongs to a bubble whose phase is
  // done. Nothing here is finished: this branch carries no done check.
  const status = stoppingElsewhere
    ? { tone: 'running', text: 'Stopping…' }
    : turnCheckFailed
      ? // "cannot tell" is not "idle", and the API answers 502 exactly when it
        // could not determine -- so it is reported as what it is.
        { tone: 'lost', text: 'Could not check whether this chat is still running.' }
      : waiting
        ? {
            tone: 'waiting',
            text: pending.questions.length ? 'Awaiting your answer...' : 'Awaiting your approval...',
          }
        : runningElsewhere
          ? { tone: 'running', text: 'Still running…' }
          : turn

  return (
    <div className="chat-main">
      <div className="chat-head">
        <div className="chat-head-main">
          <div className="chat-head-title-block">
            <div className="chat-head-title">{chatTitle}</div>
            <div className="chat-head-meta">
              <span className="chat-head-meta-text">
                {loadingHistory ? 'Loading history...' : currentSessionId ? 'History loaded - continue the conversation' : 'Not started yet'}
              </span>
              {/* The state of the conversation, in the line the header already
                  keeps for it -- out of the thread, which scrolls, and out of
                  the composer, where it sat between the user and what they were
                  typing. */}
              {status && (
                <span className={`chat-head-status ${status.tone}`}>
                  {status.tone === 'running' && <span className="spin" />}
                  {status.tone === 'done' && <DoneCheckIcon />}
                  {/* The line is cut to fit beside the meta text, and the panel
                      is narrow enough that it will be: the full text stays
                      available on hover rather than being lost to the
                      ellipsis. */}
                  <span className="chat-head-status-text" title={status.text}>
                    {status.text}
                  </span>
                </span>
              )}
              {/* The two ways on from a check that could not answer, beside the
                  status they belong to. Retry is the way back to an answer, and
                  it is the control that re-dials: the /turn read establishes the
                  gateway connection, where an abort would need one already.
                  Dismiss is the way out of an alarm that cannot resolve itself --
                  a channel this process cannot use fails every retry the same
                  way, and without it the header would carry it forever.
                  Disabled for the window the composer's controls are: while an
                  abort is in flight, the header is the wait's only feedback. */}
              {turnCheckFailed && (
                <>
                  <button className="btn sm ghost" onClick={retryTurnCheck}>
                    Retry
                  </button>
                  <button className="btn sm ghost" onClick={dismissTurnCheck} disabled={stoppingElsewhere}>
                    Dismiss
                  </button>
                </>
              )}
            </div>
          </div>
        </div>
      </div>

      <div ref={threadEl} className="thread">
        <div className="thread-inner">
          {bubbles.length ? (
            bubbles.map((b, i) => {
              // The turn is over and its outcome is its own: a stopped turn
              // neither finished nor failed, and a turn whose stream was lost is
              // still executing somewhere. Only a clean terminal gets to call
              // the text it produced the final result of anything.
              const settled = b.phase === 'done' && !b.stopped && !b.error && !b.transportLost
              return (
                <div key={i} className={`msg ${b.kind}`}>
                  <div className="avatar">{b.kind === 'user' ? userInitials : 'AI'}</div>
                  <div className="bubble">
                    {b.transportLost && (
                      // Amber, not the red of `b.error` and not the green Done
                      // check: the turn's outcome is unknown, not failed, and
                      // the reason the stream gave up is kept underneath rather
                      // than presented as the turn's error.
                      <div className="tool-status lost-mark">
                        <span>{statusLine(b)}</span>
                      </div>
                    )}
                    {b.transportLost && (
                      <div style={{ fontSize: 12.5, color: 'var(--muted)', marginTop: 4, whiteSpace: 'pre-wrap' }}>
                        {b.transportLost}
                      </div>
                    )}
                    {b.phase === 'done' && b.stopped && (
                      // A stopped turn gets the muted marker, not the green Done
                      // check: it neither finished nor failed.
                      <div className="tool-status">
                        <StopIcon />
                        {statusLine(b)}
                      </div>
                    )}
                    {b.phase === 'done' && !b.stopped && !b.error && !b.transportLost && (
                      <div className="tool-status done-mark">
                        <DoneCheckIcon />
                        {statusLine(b)}
                      </div>
                    )}
                    {/* One item per thing the turn produced, in the order it
                        produced them: the agent's narration, the card for the
                        command it announced, the write it had to have approved,
                        the question it asked -- and the next one, until it
                        answers. Drawing every card first and the text last put
                        each card above the narration that introduced it (issue
                        #216) and a decided confirmation below every card of the
                        turn rather than beside the call it gated. */}
                    {b.items.map((item, ti) => {
                      if (item.kind === 'narration') {
                        return (
                          <div key={'n' + ti} className="narration">
                            <MdText text={item.text} />
                          </div>
                        )
                      }
                      if (item.kind === 'approval') {
                        // While it is pending the card is drawn in the composer
                        // dock, where it cannot scroll away; this item is the
                        // place the record settles into. Drawing it here too
                        // would be the same card twice.
                        return item.confirm.resolved ? (
                          <ApprovalCard key={'a' + ti} confirm={item.confirm} allowAlwaysOk={allowAlwaysOk} onDecide={decide} />
                        ) : null
                      }
                      if (item.kind === 'question') {
                        return item.question.resolved ? (
                          <QuestionCard
                            key={item.question.questionId}
                            question={item.question}
                            onPick={pick}
                            onType={typeAnswer}
                            onSubmit={submitQuestion}
                            onDismiss={dismissQuestion}
                          />
                        ) : null
                      }
                      const t = item.tool
                      // Keyed by the tool call itself, because that is what the
                      // reader opened: its id survives a re-read of the
                      // conversation, where a position does not. A reload that
                      // shifts the transcript -- another client's turn landing
                      // in it, a stopped turn persisting its partial -- would
                      // otherwise hand the reader's open card to whatever moved
                      // into its place and take it away from the call they
                      // opened. The conversation is part of the key, since ids
                      // are only unique within a session; the position is left
                      // as the fallback for a call the gateway gave no id.
                      const key = `${currentSessionId ?? ''}-${t.callID || `p${i}-${ti}`}`
                      const running = !t.done && b.phase !== 'done'
                      const open = openedTools[key] ?? running
                      return (
                        <div key={'t' + ti} className={`tool-card ${running ? 'tool-running' : ''} ${open ? 'tool-open' : ''}`}>
                          <button
                            className="tool-head"
                            aria-expanded={open}
                            onClick={() => setOpenedTools((prev) => ({ ...prev, [key]: !open }))}
                          >
                            <ToolIcon />
                            <span className="tool-name">{t.name}</span>
                            {/* Closed, the command is the one thing that says
                                what the card is; open, the body carries it in
                                full and repeating it here would only truncate
                                it twice. */}
                            {!open && t.cmd && <span className="tool-arg">{t.cmd}</span>}
                            {running ? (
                              <span className="pill accent">Running...</span>
                            ) : !t.done ? (
                              // The turn was stopped while this tool was still
                              // in flight: `message_done{stopped}` freezes the
                              // phase to done, so the neutral "Done" pill would
                              // claim completion for work that was interrupted.
                              <span className="pill neutral">Stopped</span>
                            ) : (
                              <span className="pill neutral">Done</span>
                            )}
                            <ChevronIcon />
                          </button>
                          {open && (
                            <div className="tool-body">
                              {t.cmd && (
                                <span className="mono" style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-all' }}>{t.cmd}</span>
                              )}
                              {t.result && (
                                <div
                                  className="mono"
                                  style={{
                                    marginTop: t.cmd ? 8 : 0,
                                    background: 'rgba(0,0,0,.04)',
                                    border: '1px solid var(--border)',
                                    borderRadius: 6,
                                    padding: '8px 10px',
                                    maxHeight: 220,
                                    overflow: 'auto',
                                    fontSize: 12,
                                    lineHeight: 1.6,
                                    whiteSpace: 'pre-wrap',
                                    wordBreak: 'break-all',
                                  }}
                                >
                                  {t.result}
                                </div>
                              )}
                            </div>
                          )}
                        </div>
                      )
                    })}
                    {/* Text the gateway rewrote while the turn was running.
                        Superseded is not lost: it is what the user was reading
                        when the rewrite landed, so it stays one click away
                        instead of being replaced out of existence (issue
                        #204). */}
                    {b.superseded?.length ? (
                      <details className="superseded">
                        <summary>Earlier ({b.superseded.length})</summary>
                        {b.superseded.map((prev, pi) => (
                          <div key={pi} className="superseded-item">
                            <MdText text={prev} />
                          </div>
                        ))}
                      </details>
                    ) : null}
                    {/* The reply, always last: it is terminal, so nothing the
                        turn produces can follow it. When the turn ran tools, it
                        is the takeaway and gets a highlighted panel so it stands
                        out from the log above. Assistant text renders as
                        Markdown; user messages stay plain text. Until the turn
                        is over the panel is neutral and says so -- a mid-turn
                        snapshot is a reply being written, and labelling it the
                        final result is what made the next rewrite read as the
                        answer disappearing. */}
                    {b.kind === 'assistant' && hasTools(b) && b.text ? (
                      <div className={`answer-panel ${settled ? '' : 'in-progress'}`}>
                        <span className="answer-label">{settled ? 'Final result' : 'Reply'}</span>
                        <MdText text={b.text} />
                      </div>
                    ) : b.kind === 'assistant' && b.text ? (
                      <MdText text={b.text} />
                    ) : b.text ? (
                      <div style={{ fontSize: 13.5, lineHeight: 1.7, whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>{b.text}</div>
                    ) : null}
                    {b.error && <div style={{ fontSize: 13, color: 'var(--danger)', whiteSpace: 'pre-wrap' }}>{b.error}</div>}
                  </div>
                </div>
              )
            })
          ) : (
            <div className="thread-empty">
              <EmptyChatIcon />
              <div className="thread-empty-title">Start a new conversation</div>
              <div className="thread-empty-desc">Type your request below; CubePilot will use platform skills to troubleshoot, deploy or query resources for you.</div>
            </div>
          )}
        </div>
      </div>

      <div className="composer">
        {/* The cards the agent is parked on (issue #204). Under the composer
            rather than in the bubble that raised them: the thread scrolls, so a
            card drawn in it is a card the user has to go looking for -- and the
            turn stays parked for exactly as long as they are looking. */}
        {waiting && (
          <div className="hitl-dock">
            {/* Every pending approval, not just the newest: one turn can raise
                several, and each is answered on its own. The dock is where a
                card cannot scroll away, which is the whole reason it is here. */}
            {pending.confirms.map((c) => (
              <ApprovalCard key={c.approvalId} confirm={c} allowAlwaysOk={allowAlwaysOk} onDecide={decide} />
            ))}
            {pending.questions.map((q) => (
              <QuestionCard key={q.questionId} question={q} onPick={pick} onType={typeAnswer} onSubmit={submitQuestion} onDismiss={dismissQuestion} />
            ))}
          </div>
        )}
        <div className="composer-inner">
          <textarea
            ref={inputEl}
            rows={1}
            placeholder="Type a command, e.g. `check GPU utilization` or `create a development environment`..."
            aria-label="Message input"
            onInput={autoGrow}
            onKeyDown={(e) => {
              if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
                e.preventDefault()
                sendMessage()
              }
            }}
          />
          {/* While a turn is running the send button becomes Stop. The
              textarea stays enabled so the user can type the redirect they
              want to send next.

              The turn this ends is not always one this view streams: a turn
              started in another tab, or left running across a reload, is still
              the session on screen, and its Stop is the same button -- the
              header says which turn it is. A *confirmed* one only: when the
              check failed, nothing established that a turn is running, and the
              abort would need the very gateway channel whose absence is what
              failed the check, so Send stays and the header offers Retry
              instead. */}
          {streaming || (noStreamTurn && runningElsewhere) ? (
            <button
              className="send-btn"
              aria-label="Stop"
              onClick={streaming ? stopTurn : stopElsewhere}
              disabled={stoppingElsewhere}
            >
              <StopIcon />
            </button>
          ) : (
            // Disabled while the Stop above is in flight, so the refusal in
            // `sendMessage` is something the user can see. Enter in the
            // textarea still reaches it -- the button is a shortcut, not the
            // only path -- which is why the refusal lives there too.
            <button className="send-btn" aria-label="Send" onClick={sendMessage} disabled={stoppingElsewhere}>
              <SendIcon />
            </button>
          )}
        </div>
        <div className="composer-hint">
          Operate platform resources via natural language - type <span className="mono">@</span> to reference a resource - write operations ask for your approval before they run
        </div>
      </div>
    </div>
  )
}
