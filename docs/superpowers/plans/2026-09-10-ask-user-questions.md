# Ask-User Questions Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement issue #161 -- an `ask_user` question asked by the agent becomes an answerable card in Portal chat, mirroring the existing `confirm_pending` approval flow.

**Architecture:** The gateway broadcasts `question.requested` / `question.resolved` over the per-user WebSocket device connection. The API relays them onto the parked turn's SSE stream as `question_pending` / `question_resolved`; the browser renders option buttons and posts the answer back to a new `POST /api/sessions/{key}/question` route, which resolves the question over the same connection with `question.resolve`. The API keeps no question state beyond a small `id -> sessionKey` routing table, because the resolved event carries no session key.

**Tech Stack:** Go (API server), OpenClaw gateway (unchanged), React 19 web.

**Spec:** `docs/superpowers/specs/2026-09-10-ask-user-questions-design.md`

## Global Constraints

- Do NOT modify OpenClaw source or image. Protocol facts are pinned to the sibling checkout at `/home/zhujian/code/github.com/openclaw/openclaw`.
- English in code, comments and GitHub surface; ASCII punctuation. Commits `-s` with `Assisted-by: Claude Code`.
- Follow existing patterns: `internal/openclaw/ws/methods.go` for RPC wrappers, `internal/server/approvals.go` + `handlers_confirm.go` for the HTTP/SSE shape, `web/src/views/ChatView.tsx` for the card.
- `go vet ./...`, `go test ./...` from the repo root; `npm run build` from `web/`.

---

## File Structure

- `internal/openclaw/ws/frames.go` -- question record types (shared `QuestionRecord` + dedicated `QuestionResolved`).
- `internal/openclaw/ws/client.go` -- question event constants, callbacks, dispatch; `rpcError.Reason`.
- `internal/openclaw/ws/methods.go` -- `ResolveQuestion`, `CancelQuestion`, `GetQuestion`, `ListQuestions`.
- `internal/runtime/contracts.go` -- `QuestionPrompt` / `QuestionItem` / `QuestionOption`, two event constants, `Event.Question`.
- `internal/server/questions.go` (new) -- routing index, projectable predicate, SSE relay, HTTP handlers.
- `internal/server/hitl.go` -- `questionBridge` wiring in `conn()`, 4 manager methods, `hitlGateway` additions.
- `internal/server/server.go` -- routes, bridge wiring in `EnableHITL`.
- `internal/server/livetools.go` -- suppress `ask_user` tool cards.
- `web/src/api/types.ts`, `web/src/api/index.ts`, `web/src/views/ChatView.tsx` -- card + API.

---

## Tasks

- [ ] **1. WS types and RPCs** (`internal/openclaw/ws`)
  - `frames.go`: `QuestionOption`, `Question`, `QuestionRecord`, `QuestionResolved`. Keep `isOther` / `isSecret` / `secretStore` on `Question`.
  - `client.go`: `questionRequested` / `questionResolved` constants, `OnQuestionRequested` / `OnQuestionResolved`, dispatch branches; add `Reason` to `rpcError` sourced from `frameError.Details`.
  - `methods.go`: `ResolveQuestion`, `CancelQuestion`, `GetQuestion`, `ListQuestions` (decode the `{questions:[...]}` wrapper).
  - Tests: params sent and payloads decoded against the fake gateway.

- [ ] **2. Runtime contract** (`internal/runtime`)
  - `QuestionPrompt`, `QuestionItem`, `QuestionOption`; `EventQuestionPending` / `EventQuestionResolved`; `Event.Question *QuestionPrompt`.

- [ ] **3. Relay and routing index** (`internal/server/questions.go`, `hitl.go`)
  - `projectableQuestion(record)`: no `isOther` / `isSecret` / `secretStore` and at least one option, on every question.
  - Routing table `id -> sessionKey`, written on relay and on recovery, deleted on resolved.
  - `questionBridge` wired in `conn()`'s `OnEvent`; `question.requested` routes by its own `sessionKey`, `question.resolved` via the table.
  - `hitlManager.ResolveQuestion` / `CancelQuestion` / `GetQuestion` / `ListQuestions`; `hitlGateway` additions.
  - Tests: relay produces `question_pending` with `timeoutSeconds`; resolved routes by id; non-projectable records are dropped and logged.

- [ ] **4. HTTP routes** (`internal/server/questions.go`, `server.go`)
  - `POST /api/sessions/{key}/question` with the `question.get` gate (session match, pending, not expired).
  - `GET /api/sessions/{key}/question/pending` filtered by session, pending, not expired, projectable; `{questions:[{id, questions, timeoutSeconds}]}`, 404 when empty.
  - Error mapping: 404 / 409 / 400 / 502; 503 when `s.hitl == nil`.
  - Tests: each gate failure plus the happy path.

- [ ] **5. Suppress the ask_user tool card** (`internal/server/livetools.go`)
  - Emit no `tool_call` / `tool_result` for `name == "ask_user"`.
  - Test: an `ask_user` call+result pair yields no events; other tools unaffected.

- [ ] **6. Web** (`web/src/api/*`, `web/src/views/ChatView.tsx`)
  - Types, three API methods, `BubbleQuestion`, card rendering with option buttons / multi-select / Submit / Cancel / countdown, resolve-and-refetch on 404/409, recovery on reload.

- [ ] **7. Verification**
  - `go vet ./...`, `go test ./...`, `npm run build`.
