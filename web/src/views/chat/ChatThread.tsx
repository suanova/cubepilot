// The conversation: header, thread, tool cards, composer.
//
// Rendered by ChatView beside a session list, and (issue #30) by the floating
// widget on its own. It draws what `useChatThread` reports and holds no state
// of its own, which is what lets the two callers share one implementation of
// the turn rather than two.
import { getCurrentUser } from '@/api/client'
import { MdText } from './MdText'
import { QuestionCard } from './QuestionCard'
import { DoneCheckIcon, EmptyChatIcon, SendIcon, StopIcon, ToolIcon } from './icons'
import { statusLine } from './model'
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
    bannerUp,
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
    submitQuestion,
    dismissQuestion,
  } = thread
  const chatTitle = title

  return (
    <div className="chat-main">
      <div className="chat-head">
        <div className="chat-head-main">
          <div className="chat-head-title">{chatTitle}</div>
          <div className="chat-head-meta">
            {loadingHistory ? 'Loading history...' : currentSessionId ? 'History loaded - continue the conversation' : 'Not started yet'}
          </div>
        </div>
      </div>

      <div ref={threadEl} className="thread">
        <div className="thread-inner">
          {bubbles.length ? (
            bubbles.map((b, i) => (
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
                  {b.phase && b.phase !== 'done' && !b.transportLost && (
                    <div className="tool-status">
                      <span className="spin" />
                      {statusLine(b)}
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
                  {b.tools.map((t, ti) => (
                    <div key={'t' + ti} className={`tool-card ${!t.done && b.phase !== 'done' ? 'tool-running' : ''}`}>
                      <div className="tool-head">
                        <ToolIcon />
                        <span className="tool-cmd">{t.name}</span>
                        {!t.done && b.phase !== 'done' ? (
                          <span className="pill accent">Running...</span>
                        ) : !t.done ? (
                          // The turn was stopped while this tool was still in
                          // flight: `message_done{stopped}` freezes the phase
                          // to done, so the neutral "Done" pill would claim
                          // completion for work that was interrupted.
                          <span className="pill neutral">Stopped</span>
                        ) : (
                          <span className="pill neutral">Done</span>
                        )}
                      </div>
                      {t.cmd && (
                        <div className="tool-body">
                          <span className="mono" style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-all' }}>{t.cmd}</span>
                        </div>
                      )}
                      {t.result && (
                        <div
                          className="mono"
                          style={{
                            marginTop: 8,
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
                  ))}
                  {/* A write awaiting (or resolved by) a human decision
                      (issue #20 HITL). */}
                  {b.confirm && (
                    <div className="tool-card" style={{ borderColor: 'rgba(245,158,11,.45)' }}>
                      <div className="tool-head">
                        <ToolIcon />
                        <span className="tool-cmd">Write confirmation</span>
                        {b.confirm.resolved && b.confirm.approved === undefined ? (
                          // Settled without a decision: the turn was stopped
                          // while this write was parked. Neutral, not a
                          // rejection the user never made.
                          <span
                            style={{
                              fontSize: 12,
                              borderRadius: 999,
                              padding: '2px 10px',
                              background: 'rgba(0,0,0,.07)',
                              color: 'rgba(0,0,0,.55)',
                            }}
                          >
                            Stopped
                          </span>
                        ) : b.confirm.resolved ? (
                          <span
                            style={{
                              fontSize: 12,
                              borderRadius: 999,
                              padding: '2px 10px',
                              background: b.confirm.approved ? 'rgba(34,197,94,.15)' : 'rgba(239,68,68,.15)',
                              color: b.confirm.approved ? '#15803d' : '#b91c1c',
                            }}
                          >
                            {b.confirm.approved ? 'Approved' : 'Rejected'}
                          </span>
                        ) : (
                          <span
                            style={{
                              fontSize: 12,
                              borderRadius: 999,
                              padding: '2px 10px',
                              background: 'rgba(245,158,11,.15)',
                              color: '#b45309',
                            }}
                          >
                            Awaiting your decision
                          </span>
                        )}
                      </div>
                      {b.confirm.command && (
                        <div className="tool-body">
                          <span className="mono" style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-all' }}>{b.confirm.command}</span>
                        </div>
                      )}
                      {b.confirm.message && (
                        <div style={{ fontSize: 12.5, color: 'var(--muted, rgba(0,0,0,.55))', marginTop: 6, lineHeight: 1.5 }}>{b.confirm.message}</div>
                      )}
                      {!b.confirm.resolved && (
                        <div style={{ display: 'flex', gap: 8, marginTop: 10 }}>
                          <button
                            onClick={() => decide(b.confirm!, 'reject')}
                            disabled={!!b.confirm.busy}
                            style={{ background: 'none', border: '1px solid var(--danger)', color: 'var(--danger)', borderRadius: 6, padding: '6px 14px', cursor: 'pointer', fontSize: 13 }}
                          >
                            Reject
                          </button>
                          <button
                            onClick={() => decide(b.confirm!, 'approve')}
                            disabled={!!b.confirm.busy}
                            style={{ background: 'var(--accent, #3b82f6)', border: 'none', color: '#fff', borderRadius: 6, padding: '6px 14px', cursor: 'pointer', fontSize: 13 }}
                          >
                            {b.confirm.busy ? 'Sending…' : 'Approve'}
                          </button>
                          {allowAlwaysOk && (
                            <button
                              onClick={() => decide(b.confirm!, 'allow-always')}
                              disabled={!!b.confirm.busy}
                              title="Approve and add this command to your allowlist so it no longer asks"
                              style={{ background: 'none', border: '1px solid var(--accent, #3b82f6)', color: 'var(--accent, #3b82f6)', borderRadius: 6, padding: '6px 14px', cursor: 'pointer', fontSize: 13 }}
                            >
                              Always allow
                            </button>
                          )}
                        </div>
                      )}
                      {b.confirm.error && (
                        <div style={{ fontSize: 12.5, color: 'var(--danger)', marginTop: 6 }}>{b.confirm.error}</div>
                      )}
                    </div>
                  )}
                  {/* Questions the agent is blocked on until answered, one
                      card each (issue #161). */}
                  {(b.questions || []).map((q) => (
                    <QuestionCard key={q.questionId} question={q} onPick={pick} onSubmit={submitQuestion} onDismiss={dismissQuestion} />
                  ))}
                  {/* When an assistant reply ran tools, its closing text is the
                      takeaway: render it as a highlighted panel so it stands out
                      from the tool log. Assistant text renders as Markdown; user
                      messages stay plain text. */}
                  {b.kind === 'assistant' && b.tools.length > 0 && b.text ? (
                    <div className="answer-panel">
                      <span className="answer-label">Final result</span>
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
            ))
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
        {/* A turn with no stream attached to this view (another tab, or a
            reload). It sits above the input, and is hidden while this view
            streams its own turn: the composer's Stop is the control for that
            one. */}
        {bannerUp && (
          <div className="turn-banner">
            {runningElsewhere && <span className="spin" />}
            <span>
              {/* The stop in flight wins the headline. A confirmed turn is
                  still running while its abort waits, so testing that first
                  would show "Still running…" for the whole wait and mask the
                  only feedback the wait has. The branch stays (rather than
                  being dropped as redundant) because `stoppingElsewhere` is
                  not a subset of `runningElsewhere`: it is also true on the
                  cannot-check banner, whose Stop-less wait is driven by the
                  composer's send. */}
              {stoppingElsewhere
                ? 'Stopping…'
                : runningElsewhere
                  ? 'Still running…'
                  : 'Could not check whether this chat is still running.'}
            </span>
            {turnCheckFailed && (
              <button className="btn sm ghost" onClick={retryTurnCheck}>
                Retry
              </button>
            )}
            {/* The way out of an alarm that cannot resolve itself: a check
                that keeps failing would otherwise sit over a usable
                conversation forever. Disabled for the same window the Stop
                is: while the abort is in flight the banner is the wait's only
                feedback, and dropping it would make the composer's disabled
                Send look unexplained. */}
            {turnCheckFailed && (
              <button className="btn sm ghost" onClick={dismissTurnCheck} disabled={stoppingElsewhere}>
                Dismiss
              </button>
            )}
            {/* Stop only for a turn the server *confirmed* is running
                (`runningElsewhere`), and still on screen while the abort is
                in flight, unlike the composer's glyph button which has no
                label to change.

                The two controls on this banner are not interchangeable. When
                the check failed, nothing confirmed a turn and the reason it
                failed is the abort's own precondition: `/abort` issues its
                RPC over the user's existing gateway connection and never
                dials one (see hitlManager.Abort), so a Stop with no channel
                is guaranteed to answer 502 -- a control that provably cannot
                work, next to the one that can. Retry is that one: the /turn
                read establishes the connection, so it is what turns this
                banner back into a confirmed one with a real Stop. The
                composer's Send also re-dials, which is why the banner stays
                usable without a Stop on it. */}
            {runningElsewhere && (
              <button className="btn sm" onClick={stopElsewhere} disabled={stoppingElsewhere}>
                {stoppingElsewhere ? 'Stopping…' : 'Stop'}
              </button>
            )}
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
              want to send next. */}
          {streaming ? (
            <button className="send-btn" aria-label="Stop" onClick={stopTurn}>
              <StopIcon />
            </button>
          ) : (
            // Disabled while a banner Stop is in flight, so the refusal in
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
