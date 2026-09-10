# Ask-user questions in Portal chat (issue #161) -- design

Date: 2026-09-10 · Status: approved for implementation · Scope: issue #161

## Context

When the agent calls OpenClaw's `ask_user` tool, the Portal chat receives it as
a plain `tool_call` SSE event and renders a read-only tool card. There is no
option chooser and no endpoint to submit an answer, so the turn parks until the
OpenClaw question times out (default 900s) and then fails -- while the UI shows
a question the user cannot answer. Reproduced live on a local kind cluster.

The surrounding machinery already exists: the write-confirmation flow (issue
#20) established gateway-driven HITL over the WebSocket device channel, with
`confirm_pending` / `confirm_resolved` SSE events and a
`POST /api/sessions/{key}/confirm` route; the live chat projection (issue #130)
folds gateway session-message frames into those SSE events. Questions follow the
same shape, so this design reuses that structure rather than inventing one.

## Verified facts (OpenClaw, source-grounded)

- `ask_user` arguments are
  `{questions:[{id,header,question,options:[{label,description?}],multiSelect?}],
  timeoutSeconds?}`. They do **not** carry the gateway question id, so the
  `question.requested` push is the only way to learn it. `options` is required
  (2-4 entries); `multiSelect` is optional. `isOther` / `isSecret` /
  `secretStore` are not produced by `ask_user` -- they exist only on the
  `question.request` RPC used by other producers (e.g. the secrets tool).
- Gateway RPCs: `question.resolve` (`{id, answers:{answers:{qid:[labels]}},
  resolvedBy?}` or `{id, cancel:true, resolvedBy?}`), `question.get`
  (`{question: QuestionRecord}`), `question.list` (`{questions:[...]}`).
- Broadcasts: `question.requested` carries a full `QuestionRecord` (including
  `sessionKey`, `questions`, `expiresAtMs`); `question.resolved` carries only
  `{id, status, answers?}` -- `QuestionResolvedEventSchema` is a `closedObject`
  union of `answered` / `cancelled` / `expired`, so `sessionKey` is not on the
  wire and cannot be added by us.
- Deliverability: `question.requested` is not in `SESSION_SUBSCRIPTION_EVENTS`,
  so filtering runs through `canReceiveSessionEvent`, which returns true for
  `isGatewayAdmin` (`scopes.includes("operator.admin")`). CubePilot's device
  already holds `operator.admin`, so the push arrives regardless of session
  subscription. `question.resolve` method authorization passes the same way.
- Expiry calls `onResolved`, so a `question.resolved` with `status:"expired"`
  reaches us and a card can settle itself instead of staying clickable.
- A resolved/terminal record survives for 15s
  (`QUESTION_RESOLVED_ENTRY_GRACE_MS`, documented as the grace for late
  `question.waitAnswer` and `question.get` calls), so `question.get` still
  answers shortly after resolution.
- Error reasons arrive as `details.reason` on the wire error frame:
  `QUESTION_NOT_FOUND`, `QUESTION_ALREADY_TERMINAL`, `QUESTION_INVALID_ANSWER`.
- The parked turn holds the session's SSE stream open (a second
  `POST /api/messages` returns 409), so publishing a question event onto that
  stream works.

## Design

### 1. WS layer (`internal/openclaw/ws`)

`frames.go` -- one `QuestionRecord` type shared by `question.requested`,
`question.get` and `question.list` (all three return full records), plus a
small dedicated type for the resolved event, which is not a record:

```go
type QuestionOption struct {
    Label       string `json:"label"`
    Description string `json:"description,omitempty"`
}
// Question keeps the variants we do not project (isOther/isSecret/secretStore)
// so an unsupported record is filtered explicitly rather than silently decoded
// into a lossy shape.
type Question struct {
    QuestionID  string           `json:"questionId"`
    Header      string           `json:"header"`
    Question    string           `json:"question"`
    Options     []QuestionOption `json:"options"`
    MultiSelect bool             `json:"multiSelect,omitempty"`
    IsOther     bool             `json:"isOther,omitempty"`
    IsSecret    bool             `json:"isSecret,omitempty"`
    SecretStore json.RawMessage  `json:"secretStore,omitempty"`
}
type QuestionRecord struct {
    ID          string     `json:"id"`
    Questions   []Question `json:"questions"`
    AgentID     string     `json:"agentId,omitempty"`
    SessionKey  string     `json:"sessionKey,omitempty"`
    RunID       string     `json:"runId,omitempty"`
    CreatedAtMs int64      `json:"createdAtMs"`
    ExpiresAtMs int64      `json:"expiresAtMs"`
    Status      string     `json:"status"`
}
type QuestionResolved struct {
    ID     string `json:"id"`
    Status string `json:"status"` // answered | cancelled | expired
}
```

`methods.go` -- three methods beside `ResolveApproval`:

```go
ResolveQuestion(ctx, id string, answers map[string][]string, resolvedBy string) error
CancelQuestion(ctx, id, resolvedBy string) error
GetQuestion(ctx, id string) (*QuestionRecord, error)
ListQuestions(ctx) ([]QuestionRecord, error)   // decodes the {questions:[...]} wrapper
```

`client.go` -- `questionRequested` / `questionResolved` event constants and
`OnQuestionRequested` / `OnQuestionResolved` registrars, dispatched next to the
exec-approval branches. `rpcError` gains `Reason`, read from the already-decoded
`frameError.Details`.

### 2. Server

**SSE contract.** `runtime.Event` gains a typed `Question *QuestionPrompt` plus
two event constants, rather than a JSON-encoded string: `confirm_pending`
already puts domain fields (`Command`, `Level`, `Approved`) on the same
transport-neutral contract, and a typed payload gives the frontend a real
contract instead of a parse step.

```go
type QuestionOption struct { Label string `json:"label"`; Description string `json:"description,omitempty"` }
type QuestionItem struct {
    QuestionID  string           `json:"questionId"`
    Header      string           `json:"header"`
    Question    string           `json:"question"`
    Options     []QuestionOption `json:"options"`
    MultiSelect bool             `json:"multiSelect,omitempty"`
}
type QuestionPrompt struct {
    Questions      []QuestionItem `json:"questions"`
    TimeoutSeconds int            `json:"timeoutSeconds,omitempty"`
}
```

`TimeoutSeconds` is the remaining time computed server-side at relay time
(`(expiresAtMs - now)` clamped at 0), not an absolute timestamp, so the browser
countdown is not skewed by clock drift between the gateway pod and the user's
browser.

```jsonc
// question_pending
{ "type": "question_pending", "session_id": "...", "call_id": "<gateway question id>",
  "question": { "questions": [ { "questionId": "where", "header": "Target",
                  "question": "...", "options": [ {"label": "..."} ], "multiSelect": false } ],
                "timeoutSeconds": 842 } }
// question_resolved
{ "type": "question_resolved", "session_id": "...", "call_id": "...",
  "message": "answered|cancelled|expired" }
```

`call_id` is the gateway question id, exactly as `confirm_pending` uses it for
the approval id.

**Projectable predicate.** One `projectableQuestion(record)` gates both the live
relay and reload recovery: every question in the record must have no `isOther`,
no `isSecret`, no `secretStore`, and at least one option. This is not defensive
coding -- the admin connection receives *every* `question.*` event on that
gateway, so a `question.request` from another producer reaches this bridge too.
A non-projectable record is not projected and is logged.

**Relay and routing.** In `hitl.go`'s `conn()`, `OnEvent` routes
`question.requested` / `question.resolved` to a new `questionBridge` (symmetric
with the existing approval `bridge`); everything else keeps going to
`routeLive`.

`question.requested` is routed by its own `sessionKey`. `question.resolved` is
not -- see the routing index below.

**Routing index.** The server keeps a small `question id -> sessionKey` table
(`internal/server/questions.go`), written when a question is relayed and when
one is recovered by `GET .../question/pending`, and deleted when a
`question.resolved` for that id is routed. This is a routing table, not a copy
of gateway state: it stores no answers, and every authoritative decision
(whether the question is still pending, whether it has expired, whether it
belongs to the session) is still made by the gateway. It is bounded by the
number of in-flight questions. An unknown id on `question.resolved` is logged
and dropped -- reachable only when a card was restored after an API restart and
the API then restarted again before resolution, in which case the SSE stream
and its card are gone anyway.

Rejected alternative: route `question.resolved` statelessly by calling
`question.get(id)` and reading its `sessionKey`. That works inside the manager's
15s post-resolution grace window, but a miss degrades silently -- the card stays
clickable until the user reloads -- and the grace period is documented for late
`waitAnswer` / `get` calls, not as a routing mechanism. The index cannot widen
the deployment envelope either: the SSE hub, the live turn and the approval
bridge are already process-local.

**HTTP routes** (`handleSessionSubresource` in `server.go`):

- `POST /api/sessions/{key}/question`, body `{id, answers?}` or `{id,
  cancel:true}`. Before resolving or cancelling, load the record with
  `question.get` and require: `record.SessionKey == route key`, `status ==
  "pending"`, and `expiresAtMs > now`. `resolvedBy` is set to the calling user
  as audit metadata only -- it is not authorization. This gate is what stops a
  stale card for one session from resolving another session's question.
- `GET /api/sessions/{key}/question/pending` -> `question.list` filtered by
  session key, `status == "pending"`, not expired, and the projectable
  predicate; 200 with `{questions: [{id, questions:[...], timeoutSeconds}]}` in
  gateway order (oldest first), 404 when nothing matches. Each entry carries its
  own remaining `timeoutSeconds`, computed the same way as the live
  `question_pending` event, so a restored card counts down without needing the
  gateway's absolute `expiresAtMs`. The frontend uses the 404 to decide whether
  to render a card, matching `pendingConfirm`. Returning every matching record removes any
  "which record wins" ambiguity: the SSE path already pushes each record as it
  arrives, the UI renders one card per record, and resolve/cancel always act on
  an explicit id.
- `s.hitl == nil` -> 503, the same failure surface as approvals.

Error mapping: session mismatch / not found -> 404, not pending or expired ->
409, malformed body / empty answer set -> 400, `QUESTION_INVALID_ANSWER` -> 400,
otherwise 502. The `question.get` gate answers most of these directly; the
resolved `details.reason` backstops the get/resolve race.

**hitlManager.** `ResolveQuestion`, `CancelQuestion`, `GetQuestion` and
`ListQuestions` (resolve the user's conn, then delegate -- same shape as
`ResolveApproval`), all four added to the `hitlGateway` interface so the test
fake covers them.

### 3. Web

- `web/src/api/types.ts`: `SSEQuestionPending` / `SSEQuestionResolved` join
  `SSEEvent`; `PendingQuestion` mirrors the recovery response.
- `web/src/api/index.ts`: `postQuestion(sessionKey, id, answers)`,
  `postQuestionCancel(sessionKey, id)` and `pendingQuestions(sessionKey)`,
  mirroring `postConfirm` / `pendingConfirm`.
- `ChatView.tsx`: a `BubbleQuestion` on the bubble (mirroring `BubbleConfirm`),
  rendering the question text, option buttons (toggling when `multiSelect`), a
  Submit button enabled once every question has a selection, a Cancel button,
  and a countdown from `timeoutSeconds`. The countdown is presentation only:
  the gateway resolve response is authoritative, and after a 404/409 the client
  refetches `question/pending` and re-syncs the cards. `question_resolved`
  settles the card (`answered` / `cancelled` / `expired`) and clears its timer.
- `recoverPending(id)` also restores pending questions after a reload, alongside
  the existing confirm recovery, rendering one card per returned record.

### 4. Livetools

The projector emits no `tool_call` / `tool_result` for `name == "ask_user"`: its
arguments fully overlap the question card, and the card would otherwise sit on
"Running..." for the whole wait because the tool blocks on `waitAnswer`.

## Testing

- `ws`: `ResolveQuestion` / `CancelQuestion` / `GetQuestion` / `ListQuestions`
  params and decoding against the fake gateway (including the
  `{questions:[...]}` wrapper); `rpcError.Reason` extraction.
- `server`: the relay folds `question.requested` into a `question_pending`
  event on the right session with the remaining seconds, and routes
  `question.resolved` via the index; handler tests for resolve / cancel /
  pending; status mapping per failure. Negative tests: session mismatch,
  non-projectable `isSecret` / `isOther` record, expired record on recovery, and
  multiple pending records for one session.
- `livetools`: an `ask_user` `tool_call` / `tool_result` pair produces no tool
  card, while other tools are unaffected.

## Out of scope

- `isOther` / `isSecret` / `secretStore` questions: not produced by `ask_user`,
  and not projected when they arrive from another producer.
- Any change on the OpenClaw side.
