# Re-attach to a Parked Turn -- Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement issue #167 -- a question (or write confirmation) restored after a page load can be answered and the resumed turn's output is rendered in the tab that answered it; a turn ending is no longer silent on the server.

**Architecture:** The API gains `GET /api/sessions/{key}/stream`, an SSE stream that observes a turn the caller did not start: it subscribes the session's live message stream and registers a live turn whose sink writes to the new stream. The Portal opens it whenever recovery restores a card, so answering in that tab streams the continuation. Both turn ends and drops of a question/confirmation push that had no stream to carry it are logged.

**Tech Stack:** Go (API server), OpenClaw gateway (unchanged), React 19 web.

**Spec:** `docs/superpowers/specs/2026-09-10-reattach-parked-turn-design.md`

## Global Constraints

- Do NOT modify OpenClaw source or image. Protocol facts are pinned to the sibling checkout at `/home/zhujian/code/github.com/openclaw/openclaw`.
- English in code, comments and GitHub surface; ASCII punctuation (`->`, `--`). Commits `-s` with `Assisted-by: Claude Code`.
- Follow existing patterns: `internal/server/questions.go` for the session sub-resource shape, `hitlManager.RunLiveTurn` for the live-turn plumbing, `web/src/views/ChatView.tsx` for the card and SSE handling.
- `go build ./...`, `go vet ./...`, `go test ./...` from the repo root; `npm run build` from `web/`.

---

## File Structure

- `internal/server/hitl.go` -- `AttachLiveTurn` on the manager; `attachLive` on the `hitlGateway` interface and its fake.
- `internal/server/attach.go` (new) -- the `/stream` handler and its parked-decision gate.
- `internal/server/server.go` -- route the `/stream` sub-resource.
- `internal/server/handlers.go` -- log the turn-end reason.
- `internal/server/questions.go`, `internal/server/approvals.go` -- log a push dropped for lack of a stream.
- `web/src/api/sse.ts` -- optional `onHttpError`.
- `web/src/views/ChatView.tsx` -- `applyTurnEvent` extraction, attach on recovery, abort on session change.

---

## Tasks

- [x] **1. Diagnostics first** (they are what makes the rest debuggable)
  - `handlers.go`: one log line per turn end -- normal, client disconnected (`ctx.Err()`), or the `RunLiveTurn` error.
  - `questions.go` / `approvals.go`: check `hub.PublishTo`'s return and log the dropped push with its id and session.
  - Test: a relay with no open stream logs the drop.

- [x] **2. `AttachLiveTurn`** (`internal/server/hitl.go`)
  - Manager method mirroring `RunLiveTurn` minus prep/send; seed `runID`; wait on `t.done` / `ctx.Done()`; `defer releaseLive`.
  - Add it to the `hitlGateway` interface only if the fake needs it (it does not -- the method is on the manager, not the gateway); keep the interface unchanged unless a test needs otherwise.
  - Test: subscribing and forwarding to the sink; a foreign run id is rejected when the turn was seeded.

- [x] **3. `/stream` route** (`internal/server/attach.go`, `server.go`)
  - Gate on a pending question or approval -> else 404; 409 on an active stream; 503 without HITL; 400 without a session key.
  - Open the hub stream, subscribe, `AttachLiveTurn`, `message_done` on terminal / disconnect / cap.
  - Tests: each gate, plus the happy path with the fake gateway.

- [x] **4. Web** (`web/src/api/sse.ts`, `web/src/views/ChatView.tsx`)
  - `onHttpError` on `streamSSE`; extract `applyTurnEvent`; attach on recovery with an `AbortController`; 409 is quiet.

- [x] **5. Verification**
  - `go build ./...`, `go vet ./...`, `go test ./...`, `npm run build`.
  - Deploy to the local kind cluster and re-run the reported flow: reload while a question is parked, answer, confirm the output renders without a further reload.
