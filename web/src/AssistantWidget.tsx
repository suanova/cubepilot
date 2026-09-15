// The floating assistant (issue #30): a button on every page that opens a
// conversation with the agent without leaving what you were doing.
//
// Two things make it work, and both are deliberate:
//
//   - It is mounted outside the router (see App.tsx), so changing pages does
//     not unmount it and the conversation survives the navigation. Nothing is
//     persisted; there is no state to restore, because the component never went
//     away.
//   - Collapsing the panel hides it with CSS rather than unmounting it, so a
//     turn that is still streaming keeps streaming. An unmount would tear the
//     fetch down and leave the run invisibly executing server-side.
import { useCallback, useRef, useState } from 'react'
import { ChatThread } from './views/chat/ChatThread'
import { useChatThread } from './views/chat/useChatThread'
import { AssistantIcon } from './views/chat/icons'
import { invalidateSessions } from './stores/sessions'

// The conversation the widget is bound to, in the gateway's canonical form
// (`agent:<agentId>:<segment>`). Fixed rather than minted, and deliberately not
// private: it is an ordinary session that also appears in the Chat view's list,
// so a conversation that started here can be picked up at full width. Two views
// on one conversation are safe -- only one turn can be in flight per session,
// and the other view reports it as running elsewhere.
//
// The canonical form is used throughout rather than the bare segment, because
// the history and turn routes address the gateway's key directly; only the
// message route canonicalizes on the way in.
export const ASSISTANT_SESSION_KEY = 'agent:main:conv-assistant'

export default function AssistantWidget() {
  const [open, setOpen] = useState(false)
  const thread = useChatThread({
    initialSessionKey: ASSISTANT_SESSION_KEY,
    // The widget has no list of its own, but the Chat view does -- and this
    // conversation is meant to appear in it, so the first message here has to
    // say so. Otherwise the session exists server-side and nowhere else until
    // the user happens to reload.
    onSessionStarted: invalidateSessions,
  })
  // Re-read the conversation on the way in. It is shared with the Chat view, so
  // a message sent there while this panel was shut is not something the widget
  // can know about on its own, and reopening onto a stale thread would be
  // quietly wrong.
  //
  // Not on the *first* open: the hook read the conversation when it mounted, so
  // there is nothing newer to find. Collapsing never re-reads -- nothing can
  // have changed that this view did not do itself.
  const opened = useRef(false)
  const toggle = useCallback(() => {
    if (open) {
      setOpen(false)
      return
    }
    if (opened.current) thread.refresh()
    opened.current = true
    setOpen(true)
  }, [open, thread])

  return (
    <>
      <button
        className="assistant-fab"
        aria-label={open ? 'Close the assistant' : 'Open the assistant'}
        aria-expanded={open}
        onClick={toggle}
      >
        <AssistantIcon />
      </button>
      {/* Hidden, not unmounted: `display: none` keeps the component -- and its
          stream -- alive while the panel is collapsed. */}
      <div className={`assistant-panel ${open ? 'open' : ''}`} role="dialog" aria-label="CubePilot assistant">
        <ChatThread thread={thread} title="Assistant" />
      </div>
    </>
  )
}
