# Extract the chat thread from ChatView Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Split `ChatView.tsx` (1914 lines) into a session-list shell plus a reusable conversation unit, so the floating widget can render the same conversation machinery instead of a second copy of it.

**Architecture:** A `useChatThread()` hook owns one conversation -- its key, its bubbles, its SSE turn, its HITL cards -- and a `<ChatThread>` component renders it. `ChatView` keeps the session list and becomes a thin shell. The move is behaviour-preserving: the 14 tests written in the previous commit group are the contract, and they are not edited by this plan.

**Tech Stack:** React 19, TypeScript, Vitest, Testing Library.

## Global Constraints

- **Behaviour-preserving.** No test in `web/src/views/ChatView.test.tsx` may change. If one fails, the refactor is wrong, not the test.
- Comments move with the code they explain. They are the record of why this code is shaped the way it is (the redirect race, the synthetic terminal, the settled-card rules); dropping one to make a move easier loses the only explanation of a deliberate decision.
- Every file's imports must be exact: `tsconfig` has `noUnusedLocals` and `noUnusedParameters`, and `npm run build` typechecks `src`.
- Run from `web/`. Commit messages in English, `git commit -s`, plus `Assisted-by: Claude Code`.

## File map

| File | Holds | Approx. lines |
| --- | --- | --- |
| `web/src/views/chat/model.ts` | `BubbleMsg`, `BubblePhase`, `BubbleConfirm`, `BubbleQuestion`, `ToolCallVM`, `StopEvidence`; the pure helpers `newBubbleQuestion`, `questionAnswered`, `attachToolResult`, `settleBubbleCards`, `setPhase`, `redact`, `summarizeArgs`, `toolArgsDisplay` | 240 |
| `web/src/views/chat/icons.tsx` | The nine inline SVG components | 90 |
| `web/src/views/chat/QuestionCard.tsx` | The ask-user card, including its countdown and group semantics | 150 |
| `web/src/views/chat/useChatThread.ts` | The hook: one conversation's state, effects and actions | 1000 |
| `web/src/views/chat/ChatThread.tsx` | `.chat-main`: header, thread, tool cards, composer; plus `MdText` | 400 |
| `web/src/views/ChatView.tsx` | The shell: session list, search, new chat; renders `<ChatThread>` | 140 |

`useChatThread.ts` stays large because one conversation's lifecycle is one unit: the stream handler creates the HITL cards, and the cards' actions settle the turn. Splitting them would put a callback boundary through the middle of a state machine.

## Interfaces

```ts
// web/src/views/chat/useChatThread.ts

export interface ChatThreadApi {
  // The conversation on screen. `null` means a new one, whose key the server
  // mints on the first `message_start` -- so this is the thread's, not the
  // caller's, and the shell reads it only to highlight the list.
  sessionId: string | null
  bubbles: BubbleMsg[]
  loadingHistory: boolean
  streaming: boolean
  // A turn with no stream of this view's own (another tab, or a reload).
  bannerUp: boolean
  runningElsewhere: boolean
  turnCheckFailed: boolean
  // The banner Stop is waiting on `/abort` for the session on screen.
  stoppingElsewhere: boolean
  allowAlwaysOk: boolean

  // Refs the render attaches.
  threadEl: React.RefObject<HTMLDivElement | null>
  inputEl: React.RefObject<HTMLTextAreaElement | null>

  switchSession(id: string): Promise<void>
  newChat(): void
  sendMessage(): Promise<void>
  stopTurn(): Promise<boolean>
  stopElsewhere(): Promise<void>
  retryTurnCheck(): void
  dismissTurnCheck(): void
  autoGrow(): void
  decide(confirm: BubbleConfirm, decision: 'approve' | 'reject' | 'allow-always'): Promise<void>
  pick(q: BubbleQuestion, item: QuestionItem, label: string): void
  submitQuestion(q: BubbleQuestion): Promise<void>
  dismissQuestion(q: BubbleQuestion): Promise<void>
}

export function useChatThread(opts: {
  /**
   * A new conversation's key is minted mid-turn and reported in
   * `message_start`; the shell's list has to learn about it, or the
   * conversation the user is looking at is missing from its own sidebar.
   */
  onSessionStarted: () => void
}): ChatThreadApi
```

```tsx
// web/src/views/chat/ChatThread.tsx

export function ChatThread({ thread, title }: { thread: ChatThreadApi; title: string })
```

`title` is a prop rather than hook state because the shell derives it from the
session list (`sessions.find(...)`), which the hook does not have.

## What stays in the shell

`ChatView` keeps exactly: `sessions`, `sessionSearch`, `loadSessions`,
`filteredSessions`, `chatTitle`, the sidebar JSX, and one `useEffect` that loads
the list on mount. It calls `thread.switchSession(key)` and `thread.newChat()`
from the sidebar, and passes `onSessionStarted: loadSessions`.

## Order of moves

Each step ends with `npm test` and `npm run build` green.

- [ ] **Task 1: the pure layer.** Create `model.ts` and `icons.tsx` by moving the
  declarations above out of `ChatView.tsx`; import them back. No behaviour
  change, no JSX change. This is the step that proves the test suite survives
  module boundaries.
- [ ] **Task 2: the card and the markdown.** Move `QuestionCard` and `MdText`
  into their own files. Still no structural change to `ChatView`.
- [ ] **Task 3: the hook.** Move the conversation state, effects and actions
  into `useChatThread.ts`. `ChatView` calls it and destructures. The JSX is
  still inline in `ChatView`, so this step is where the interface is settled
  under the tests rather than under a new component.
- [ ] **Task 4: the component.** Move the `.chat-main` JSX into
  `ChatThread.tsx`; `ChatView` renders `<ChatThread thread={thread} title={chatTitle} />`.
  Now the shell is the session list and nothing else.
- [ ] **Task 5: prove the extraction was worth it.** Add
  `web/src/views/chat/useChatThread.test.tsx` — a throwaway component that uses
  the hook with no session list and drives one turn. It is the widget's shape,
  and it fails today because there is no such unit.

## Verification

- The 14 existing tests pass unchanged after every task.
- Mutation check at the end: unhook one behaviour the extraction moved (the
  composer's Stop) inside `ChatThread.tsx` and confirm the suite still goes red.
  A refactor can pass every test by deleting the code the tests exercise.
- `npm run build` after every task: `noUnusedLocals` is what catches a
  forgotten import in a move this size.

## Risks

- **A silent loss.** The move touches ~1900 lines; the failure mode is dropping
  a branch nobody tests (the stopped-turn marker, the superseded-stream
  handling). Mitigation: move by contiguous ranges rather than retyping, and
  diff the moved text against the original before committing.
- **Stale closures.** Several refs exist to defeat stale closures
  (`bubblesRef`, `activeSessionRef`, `streamGenRef`). They move as a block; the
  hook must not introduce a dependency array the original did not have.
