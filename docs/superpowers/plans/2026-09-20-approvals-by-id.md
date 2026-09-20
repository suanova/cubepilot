# Approvals Addressed by Id Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make an exec approval addressable by its own id end to end, so a decision
settles the approval the human clicked, and make the gateway -- not a process-local
map -- the source of truth for which approvals are pending.

**Architecture:** The platform stops keeping a pending-approval registry. The
pending list is read from the gateway (`exec.approval.list`, filtered to the
session), the decision is written back by id (`exec.approval.resolve`), and the
only local state left is a per-id in-flight set that serialises concurrent
decisions. The gateway's `exec.approval.resolved` broadcast already carries the
request, so its session comes from the wire -- no platform-side id -> session
routing table is needed, unlike the question path whose resolved event is a closed
union with no session key.

**Tech Stack:** Go 1.2x (net/http, coder/websocket), React 18 + TypeScript (Vite),
vitest, Ginkgo e2e.

**Spec:** https://github.com/suanova/cubepilot/issues/226 (issue #226)

## Global Constraints

- Code, comments, commit messages and anything written to GitHub are English.
- No user-facing string may carry an internal issue/PR/FR number or milestone
  label; numbers are allowed in source comments only.
- Pre-release: no compatibility or fallback branches for "old data", "older
  clients" or "an older gateway". A contract that changes, changes.
- `make test` does not run golangci-lint; run it separately before every commit.
- `go test ./...` skips `test/e2e`; run `make test` (vet + unit) and `make web`
  (type-check + build) for the web half.

## Design decisions

### 1. The gateway owns the pending set

Today `ApprovalService` holds `byID` + `bySession` and treats them as the truth:
`Pending` reads them, `Resolve` resolves through them, and reload recovery
reconstructs cards from them. Two failure modes follow directly from that, both
named in issue #226: `bySession` is a single slot, so a second approval for one
session silently replaces the first, and the maps are process-local, so a restart
loses every pending approval while the gateway still holds them.

The fix is to delete the registry. What remains is:

- `relayApprovalRequested(ev)` -- publishes `approval_pending` (as
  `relayQuestionRequested` does) and keeps no record.
- `Pending(ctx, user, session)` -- `exec.approval.list` on the caller's own
  connection, filtered by the canonical session key, oldest first.
- `Resolve(ctx, user, session, approvalID, decision)` -- re-reads the gateway,
  binds the id to the session, then `exec.approval.resolve`.
- `settleApprovals(captured)` -- publishes a neutral resolution for approvals a
  Stop made moot.
- `inflight` -- per-id, the only local state left. It serialises two decisions on
  one id (two tabs, a double click) and keeps a Stop from publishing over a
  decision that is already in flight.

Ownership comes from the connection: the list and the resolve both run over
`liveConn(user)`, the user's own gateway device, and the gateway filters what that
client may see. No platform-side owner check is needed or possible.

### 2. `exec.approval.list`, not `exec.approval.get`

The issue proposes `exec.approval.get` for the POST's re-verification. It cannot
carry that: the handler answers `{id, commandText, commandPreview,
allowedDecisions, host, nodeId, agentId, expiresAtMs}` -- the *display* text, and
no `sessionKey`. So it can neither bind the id to the session in the URL nor
supply the raw command that `allow-always` derives its rule from.
`exec.approval.list` is the same visibility-filtered read and returns the full
record: `{approvalKind, id, request{command, sessionKey, ...}, createdAtMs,
expiresAtMs}`. One read answers "is it still pending", "is it this session's" and
"what command was it".

### 3. The decision carries the id

`POST /api/v1/sessions/{key}/approval` takes `{approvalId, decision}` and
`approvalId` is required (400 without it). The route keeps its session in the
path -- it is the session's subresource -- but the id is what is settled, and the
service refuses an id whose record belongs to another session.

### 4. Events carry what the client needs to build a list

`approval_pending` gains `createdAtMs` and `expiresAtMs` from the gateway record,
so several cards have a stable order and a client can drop an expired one without
guessing. `GET .../approval/pending` becomes `{"approvals":[...]}` and keeps its
404 for "nothing pending", matching the sibling question endpoint -- two endpoints
with two meanings for an empty read would be worse than either.

`exec.approval.resolved` is decoded with its `request`, which carries `sessionKey`.
That is what lets the relay address the event; the alternative -- an id -> session
routing table like `questionRoutes` -- exists there only because
`question.resolved` is a closed union with no session key.

### 5. Stop settles what it captured, not what it can still read

A Stop aborts the run; the gateway cancels the approvals bound to it and
broadcasts their resolutions. So by the time the platform would want to "settle
the session's approvals", the gateway has none left to list -- the records must be
captured *before* the abort RPC. The handler does that, and the settle then
publishes a neutral `approval_resolved` per captured id (skipping any id being
decided right now, whose own decision owns the outcome).

Over-settling is now cheap and self-healing: an approval that is still pending
gateway-side comes back on the next pending read, because the card's source is the
gateway rather than a deleted record.

### 6. Fail loudly

There is no fallback to a memory copy, because there is no memory copy. A read
that cannot reach the gateway is a 502 with the error, and a decision with no live
channel is a 503 -- the same contract the question endpoints already implement.
`errNoApprovalChannel` is the sentinel, mirroring `errNoQuestionChannel`.

## File map

| File | Change |
|---|---|
| `internal/openclaw/ws/frames.go` | `ExecApprovalRequest` named type; `ApprovalResolved` decodes `request` |
| `internal/openclaw/ws/methods.go` | `ListApprovals` (`exec.approval.list`) |
| `internal/openclaw/ws/approvals_test.go` | New: list decode, empty params, resolved-with-request |
| `internal/server/gateway.go` | `gatewayClient` gains `ListApprovals`; `gatewayConns.ListApprovals` |
| `internal/server/approvals.go` | Registry deleted; list/resolve/settle by id; SSE + HTTP handlers |
| `internal/server/settle.go` | Broadcast relay reads the session from the event; settle takes captured ids |
| `internal/server/attach.go` | `parkedTurn` uses the pending list |
| `internal/server/abort.go` | Capture pending approvals before the abort RPC |
| `internal/server/server.go` | Bridge calls the relay, not `Begin` |
| `internal/runtime/contracts.go` | `Event.CreatedAtMs` / `Event.ExpiresAtMs` |
| `internal/store/store.go` | `AuditEntry.ApprovalID` |
| `internal/server/*_test.go` | Fake gateway gains `ListApprovals`; approval/attach/settle/abort tests |
| `test/e2e/framework/http.go` | Decision posts the approval id |
| `web/src/api/types.ts`, `web/src/api/index.ts` | List-shaped pending read; id on the decision |
| `web/src/views/chat/model.ts`, `useChatThread.ts`, `ChatThread.tsx` | Cards are a list; dedupe and order by id |
| `web/src/test/gateway.ts`, `web/src/views/ChatView.test.tsx` | Fake + tests |
| `bruno/cubepilot-api/3-chat/05-*,06-*.bru` | Request body and docs |

## Tasks

### Task 1: Protocol -- list approvals, decode the resolved request

**Files:** `internal/openclaw/ws/frames.go`, `methods.go`, new `approvals_test.go`

**Interfaces produced:**
- `ws.ExecApprovalRequest{Command, SessionKey, AgentID, Security, Ask, WarningText string}`
- `ws.ApprovalRequested{Kind, ID string; Request ExecApprovalRequest; CreatedAtMs, ExpiresAtMs int64}`
- `ws.ApprovalResolved{ID, Decision, ResolvedBy string; TS int64; Request ExecApprovalRequest}`
- `(*ws.Client).ListApprovals(ctx) ([]ApprovalRequested, error)` -- `exec.approval.list`, no params, bare-array payload

- [ ] Test: a gateway stub answering `exec.approval.list` with a two-entry array;
      assert both entries decode (id, sessionKey, command, createdAtMs/expiresAtMs)
      and that the request carried no params.
- [ ] Test: an `exec.approval.resolved` frame carrying `request.sessionKey` decodes
      into `Request.SessionKey` through the event dispatcher.
- [ ] Test: an empty array decodes to an empty slice (not an error).
- [ ] Implement; run `go test ./internal/openclaw/ws/`.
- [ ] Commit.

### Task 2: Gateway connection surface

**Files:** `internal/server/gateway.go`, `internal/server/gateway_test.go` (fake)

**Interfaces produced:**
- `gatewayClient` interface += `ListApprovals(ctx context.Context) ([]ws.ApprovalRequested, error)`
- `(*gatewayConns).ListApprovals(ctx, user) ([]ws.ApprovalRequested, error)` -- `liveConn`, else `errNoApprovalChannel`
- `errNoApprovalChannel` lives in `approvals.go` next to `errNoQuestionChannel`'s sibling reasoning

- [ ] Extend `fakeGatewayClient` with `ListApprovals` (canned list + error switch).
- [ ] Test: no live connection reports `errNoApprovalChannel`; a live one returns the
      fake's records.
- [ ] Implement; run `go test ./internal/server/ -run Gateway`.
- [ ] Commit.

### Task 3: ApprovalService -- list, resolve by id, settle captured

**Files:** `internal/server/approvals.go`, `internal/server/approvals_test.go`

**Interfaces produced (replacing today's):**
```go
// ApprovalGateway is the gateway-facing half: the pending approvals the user's
// connection can see, and the decision written back over it.
type ApprovalGateway interface {
    ListApprovals(ctx context.Context, user string) ([]ws.ApprovalRequested, error)
    ResolveApproval(ctx context.Context, user, approvalID, decision string) error
}

type pendingApproval struct {
    ApprovalID  string
    SessionKey  string
    Tool        string
    Command     string
    Level       string
    Message     string
    CreatedAtMs int64
    ExpiresAtMs int64
}

func (s *ApprovalService) SetGateway(g ApprovalGateway)
func (s *ApprovalService) RelayRequested(user string, ev ws.ApprovalRequested)
func (s *ApprovalService) Pending(ctx context.Context, user, sessionKey string) ([]pendingApproval, error)
func (s *ApprovalService) Resolve(ctx context.Context, user, sessionKey, approvalID, decision string) (pendingApproval, error)
func (s *ApprovalService) SettleApprovals(captured []pendingApproval) []pendingApproval
```

Errors: `errNoPending` (404), `errApprovalInFlight` (409), `errNoApprovalChannel` (503),
`errNoGateway` (approval channel absent -- folded into `errNoApprovalChannel`).

- [ ] Test: `Pending` filters the gateway list to the session, canonicalises the
      record's key, orders oldest first and drops nothing else.
- [ ] Test: `Resolve` settles by id -- with two pending on one session, deciding the
      older posts *that* id to the gateway (the fake records it) and publishes
      `approval_resolved` with that call id.
- [ ] Test: `Resolve` refuses an id belonging to another session (404) and an id the
      gateway no longer lists (404).
- [ ] Test: two concurrent `Resolve` calls on one id -- one reaches the gateway, the
      other gets `errApprovalInFlight`.
- [ ] Test: a failed gateway resolve returns the error and publishes nothing, and
      the id is not left in flight (a retry works).
- [ ] Test: `SettleApprovals` returns every captured id except one being decided.
- [ ] Test: `recordDecision` writes the approval id into the audit entry.
- [ ] Implement.
- [ ] Commit.

### Task 4: HTTP surface

**Files:** `internal/server/approvals.go` (handlers), `internal/server/approvals_test.go`

- [ ] Test: `POST` with no `approvalId` is 400.
- [ ] Test: `POST` settles the named id and answers `{approved, decision, approvalId}`.
- [ ] Test: `POST` for an unknown/expired id is 404; for one already resolved
      (`APPROVAL_ALREADY_RESOLVED`) is 409; with no channel is 503.
- [ ] Test: `GET .../approval/pending` answers `{"approvals":[...]}` with both
      records, oldest first, and 404 when the session has none.
- [ ] Test: the session key in the URL is canonicalised, so a short key works here
      exactly as it does on `/turn` and `/abort`.
- [ ] Implement (`writeApprovalGatewayError`, `approvalErrorStatus`).
- [ ] Commit.

### Task 5: Attach, settle and abort wiring

**Files:** `internal/server/attach.go`, `settle.go`, `abort.go`, `server.go`, tests

- [ ] `parkedTurn`: `Pending(ctx, ...)` non-empty means parked; `errNoApprovalChannel`
      means "nothing pending"; any other error propagates (502).
- [ ] `settleApprovalResolved(user, ev)`: publish to `ev.Request.SessionKey` (logged
      and dropped when empty), with the same `Approved` derivation as today.
      No record lookup.
- [ ] `abort.go`: capture the session's pending approvals before the abort RPC;
      pass them to `settlePendingForSession`, which publishes one neutral
      `approval_resolved` per captured record the decision path is not holding.
- [ ] Test: attach answers "parked" from a gateway-only list (platform state fresh,
      i.e. the restart case).
- [ ] Test: a gateway broadcast for an approval this process never saw still drops
      the card on the session's stream.
- [ ] Test: Stop with two pending publishes two neutral resolutions; Stop with a
      decision in flight publishes one.
- [ ] Commit.

### Task 6: Audit attribution

**Files:** `internal/store/store.go`, `internal/server/approvals.go`, tests

- [ ] `AuditEntry.ApprovalID string \`json:"approvalId,omitempty"\``.
- [ ] `recordDecision` sets it; test asserts it round-trips through `AddAudit` /
      `ListAudit`.
- [ ] Commit.

### Task 7: Web client

**Files:** `web/src/api/types.ts`, `web/src/api/index.ts`,
`web/src/views/chat/model.ts`, `useChatThread.ts`, `ChatThread.tsx`,
`web/src/test/gateway.ts`, `web/src/views/ChatView.test.tsx`

- [ ] `pendingApprovals(sessionKey)` -> `PendingApproval[]` (`{approvals}` shape, 404
      means empty); `postApproval(sessionKey, approvalId, decision)`.
- [ ] `BubbleConfirm` gains `createdAtMs` / `expiresAtMs`; `pendingCards` returns
      `confirms: BubbleConfirm[]` -- every unresolved card, deduped by approval id,
      oldest first; `openApproval` becomes `openApprovals`.
- [ ] `approval_pending` pushes a card unless that id already has one; `recoverPending`
      merges the restored list by id; `approval_resolved` settles the card whose id
      matches.
- [ ] `decide` posts the clicked card's id, treats 404 *and* 409 as "settled
      underneath the click" (neutral close), and refuses to paint `approved` when the
      response names a different approval.
- [ ] The composer dock draws every pending card.
- [ ] Tests: two pending approvals draw two cards; deciding one posts its id and
      leaves the other pending; a restored list plus a live event for the same id is
      one card; a resolve event for one of two cards settles only that one.
- [ ] Run `npm test` and `make web`.
- [ ] Commit.

### Task 8: e2e helper, bruno collection, docs

**Files:** `test/e2e/framework/http.go`, `bruno/cubepilot-api/3-chat/05-*.bru`,
`06-*.bru`, `bruno/cubepilot-api/WALKTHROUGH.md` (if it shows the body)

- [ ] `resolveApproval` posts `{approvalId: pending.CallID, decision}` and fails the
      helper when the event carried no call id.
- [ ] bruno: body gains the id, docs explain that the id comes from the event's
      `callId` and that several approvals can be pending at once.
- [ ] Commit.

## Acceptance mapping (issue #226)

| Acceptance criterion | Covered by |
|---|---|
| Two approvals on one session, approved in either order, each settling the one clicked | Task 3 (service), Task 4 (HTTP), Task 7 (web) |
| Same, with one rejected | Task 3 |
| Restart with an approval pending, then approve it | Task 3 + Task 5 (no platform state is consulted) |
| Stop a turn holding two approvals -- both cards go, neither stays clickable | Task 5 |
| Double-click, and two tabs on one id -- one decision, no double resolve | Task 3 (`inflight`) |
| The gateway expiring an approval while a resolve is in flight | Task 4 (`APPROVAL_ALREADY_RESOLVED` -> 409) |
