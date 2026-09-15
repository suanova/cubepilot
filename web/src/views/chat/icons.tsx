// The view's inline SVGs.
//
// Split out so the shell (the session list) and the thread can each import
// only what they draw, rather than one file holding every prop the other needs.

export function ChatBubbleIcon() {
  return (
    <svg className="s-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round">
      <path d="M21 11.5a8.38 8.38 0 0 1-.9 3.8 8.5 8.5 0 0 1-7.6 4.7 8.38 8.38 0 0 1-3.8-.9L3 21l1.9-5.7a8.38 8.38 0 0 1-.9-3.8 8.5 8.5 0 0 1 4.7-7.6 8.38 8.38 0 0 1 3.8-.9h.5a8.48 8.48 0 0 1 8 8v.5z" />
    </svg>
  )
}


// QuestionCard renders an ask_user question (issue #161): one block per
// question with its options as selectable buttons, plus Submit and Dismiss.
// The countdown is presentation only -- the gateway's resolve response is what
// settles the card, and the server relays its expiry as question_resolved.

export function SearchIcon() {
  return (
    <svg className="icon" style={{ width: 14, height: 14 }} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round">
      <circle cx="11" cy="11" r="8" />
      <path d="M21 21l-4.35-4.35" />
    </svg>
  )
}

export function NewChatIcon() {
  return (
    <svg className="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round">
      <path d="M12 5v14M5 12h14" />
    </svg>
  )
}

export function SendIcon() {
  return (
    <svg className="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round">
      <path d="M22 2 11 13M22 2l-7 20-4-9-9-4z" />
    </svg>
  )
}

// A filled square, unlike the outline icons it sits beside. The shared `.icon`
// class paints outlines (`fill:none`), and a CSS rule beats any presentation
// attribute, so the fill is set inline where it wins.
export function StopIcon() {
  return (
    <svg
      className="icon"
      viewBox="0 0 24 24"
      aria-hidden="true"
      style={{ width: 13, height: 13, fill: 'currentColor', stroke: 'none' }}
    >
      <rect x="5" y="5" width="14" height="14" rx="2.5" />
    </svg>
  )
}

export function DoneCheckIcon() {
  return (
    <svg className="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round">
      <path d="M20 6L9 17l-5-5" />
    </svg>
  )
}

export function EmptyChatIcon() {
  return (
    <svg className="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.4" strokeLinecap="round" strokeLinejoin="round">
      <path d="M21 11.5a8.38 8.38 0 0 1-.9 3.8 8.5 8.5 0 0 1-7.6 4.7 8.38 8.38 0 0 1-3.8-.9L3 21l1.9-5.7a8.38 8.38 0 0 1-.9-3.8 8.5 8.5 0 0 1 4.7-7.6 8.38 8.38 0 0 1 3.8-.9h.5a8.48 8.48 0 0 1 8 8v.5z" />
    </svg>
  )
}

export function ToolIcon() {
  return (
    <svg className="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round">
      <path d="M4 17l6-6-6-6M12 19h8" />
    </svg>
  )
}

