# HITL Write-Confirmation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement issue #20 — interactive-chat HITL: OpenClaw native exec write approvals surfaced to the Portal via a platform WebSocket device client; approve/reject through the existing SSE chat stream.

**Architecture:** Platform API server adds one OpenClaw gateway-protocol WS device client per user (paired device, `operator.admin`). On interactive sessions of `ConfirmWrites` templates the platform sets `permissionMode="guarded"` and writes an argv read-allowlist into the per-agent exec policy (`agents."main"`). A gated exec write pauses (native approval); the platform translates `exec.approval.requested` into `confirm_pending` SSE through a new per-session hub, and Portal decisions resolve via `exec.approval.resolve`. Chat stays on the existing HTTP `/v1/chat/completions` path.

**Tech Stack:** Go (API server), OpenClaw gateway v2026.8.2 (unchanged), React 18 web. WS client uses `nhooyr.io/websocket` (or stdlib-less Caddy fork `github.com/coder/websocket`).

**Spec:** `docs/superpowers/specs/2026-09-04-hitl-write-confirmation-design.md`

## Global Constraints

- Do NOT modify OpenClaw source or image. All changes live in this repo; gateway is a client-controlled black box.
- OpenClaw protocol facts are pinned to gateway **v2026.8.2**; port from the TS reference under the sibling repo `/home/zhujian/code/github.com/openclaw/openclaw` (tag v2026.8.2).
- Interactive-only gating: scheduled `task-*` / `inspect-*` sessions stay ungated (`ask: off`).
- GitHub surface English; commits `-s` + `Assisted-by: Claude Code`.
- Follow existing patterns in `internal/`, `web/`; k8s API conventions from the repo's global guidance.

---

## File Structure

**Go — WS protocol client (new package `internal/openclaw/ws`):**
- `internal/openclaw/ws/client.go` — gateway-protocol WS client: connect.challenge + device auth, method call/response plumbing, read pump, reconnect, callback dispatch.
- `internal/openclaw/ws/device.go` — device keypair, `deriveDeviceId`, canonical signed-proof builder (port of OpenClaw TS).
- `internal/openclaw/ws/methods.go` — typed methods: `GetApprovalsPolicy`/`SetApprovalsPolicy` (exec.approvals.get/set), `EnsureSessionGuarded` (sessions.create/patch `permissionMode`), `ResolveApproval`, `GetApproval`.
- `internal/openclaw/ws/events.go` — approval event types + subscriptions (`exec.approval.requested/resolved`).

**Go — server (platform API):**
- `internal/server/approvals.go` — ApprovalService: pending registry, gateway-event → SSE translation, decision handling, ledger/audit recording.
- `internal/server/ssehub.go` — per-session SSE hub (single-writer streams, publish, heartbeat, register/unregister).
- Modify `internal/server/handlers.go` — `handleMessages` streams via hub; ConfirmWrites turn gating (`EnsureSessionGuarded`).
- Modify `internal/server/server.go` — routes + server fields (approvals service, hub, ws connection manager).
- Modify `internal/server/handlers_platform.go` (or new) — policy applier + device provisioning hook wiring.

**Go — misc:**
- Modify `internal/openclaw/events.go` — extend `Event` with `Command/Level/Message/Approved`.
- Modify `internal/audit/audit.go` (read first) — decision entries if shape allows.
- Modify `internal/metrics/` as needed.

**Web (phase C; read `web/src` first):**
- api client method(s); chat SSE event handling for `confirm_pending`/`confirm_resolved`; a confirmation card component; reload recovery via `GET .../confirm/pending`.

---

### Task 1: Extend the SSE event model

**Files:**
- Modify: `internal/openclaw/events.go`
- Test: `internal/openclaw/events_test.go` (new)

**Interfaces:**
- Produces: `openclaw.Event` gains `Command string` (`json:"command,omitempty"`), `Level string` (`json:"level,omitempty"`), `Message string` (`json:"message,omitempty"`), `Approved bool` (`json:"approved,omitempty"`). Constants `EventConfirmPending`, `EventConfirmResolved` already exist.

- [ ] **Step 1:** Add the fields to `Event` in `internal/openclaw/events.go` (mirror existing `Delta`/`Error` style).
- [ ] **Step 2:** Add a unit test asserting a `confirm_pending` and a `confirm_resolved` event marshal to the JSON keys documented in `docs/cubepilot/api.md` (436–437).
- [ ] **Step 3:** `go test ./internal/openclaw/ -run TestConfirmEventMarshal -v` passes.
- [ ] **Step 4:** Commit `feat(openclaw): confirm_pending/resolved SSE event fields (issue #20)`.

### Task 2: Gateway WS client — connect & device auth

**Files:**
- Create: `internal/openclaw/ws/device.go`, `internal/openclaw/ws/client.go`, `internal/openclaw/ws/client_test.go`
- Test: same

**Interfaces:**
- Consumes: gateway WS URL `ws://<host>:<port>` + shared token + `DeviceIdentity{PrivateKeyPEM string}` (provisioned).
- Produces:
  - `type Device struct { ID string; sign func(..., token string) (signature []byte, signedAt int64, nonce string, err error) }`
  - `func NewClient(ctx context.Context, url, token string, dev Device) *Client`
  - `Client.Connect(ctx) error`, `Client.Close()`, `Client.Call(ctx, method string, params any) (json.RawMessage, error)`
  - `Client.OnApprovalRequested(func(ev ApprovalRequested))`, `Client.OnApprovalResolved(func(ev ApprovalResolved))`

- [ ] **Step 1:** Read the OpenClaw TS reference for the connect wire shape and device proof: `/home/zhujian/code/github.com/openclaw/openclaw/packages/gateway-protocol/src/schema/frames.ts` and `src/gateway/server/ws-connection/connect-device-proof.ts` + `connect-device-proof.ts` canonical payload. Record exact JSON field names, the signature message layout, `deriveDeviceId`, and scope/device param names.
- [ ] **Step 2:** Implement `device.go` to match (private key → public → `device.id` = derived id; build the signed payload exactly as TS does; PEM or raw bytes).
- [ ] **Step 3:** Implement `client.go`: dial, send `connect.challenge` with the device + token + declared scopes/caps, handle the challenge/response frames, then drive request/response + subscription reads. Verify connect params against `operator-approvals-client.ts` (client id `gateway-client`, mode `backend`, caps `[APPROVALS,EXEC_APPROVALS]`, scopes `[operator.admin,operator.read,operator.write,operator.approvals]`).
- [ ] **Step 4:** Unit test the frame encode/decode + device signature determinism against a fixture captured from TS (if a fixture cannot be produced without a live gateway, assert structural invariants and mark live-verify in the PR).
- [ ] **Step 5:** `go vet ./...`; `go test ./internal/openclaw/ws/ -v` green; commit.

### Task 3: Gateway WS client — approvals methods & event dispatch

**Files:**
- Create: `internal/openclaw/ws/methods.go`, `internal/openclaw/ws/events.go`, `internal/openclaw/ws/methods_test.go`

**Interfaces:**
- Consumes: `Client.Call` from Task 2.
- Produces:
  - `GetApprovalsPolicy(ctx) (policy RawMessage, baseHash string, err error)` → `exec.approvals.get`
  - `SetApprovalsPolicy(ctx, policy RawMessage, baseHash string) error` → `exec.approvals.set`
  - `EnsureSessionGuarded(ctx, sessionKey string) error` → list sessions; `sessions.create` if absent; else `sessions.patch {permissionMode:"guarded"}` (non-`"full"` value needs only `operator.write`; we hold admin).
  - `ResolveApproval(ctx, approvalID, decision string) error` → `exec.approval.resolve` with `{id, decision}` (`"allow-once"` | `"deny"`).
  - Event structs `ApprovalRequested{ID, SessionKey, Command, Title, Description, Kind}` and `ApprovalResolved{ID, Decision}` mapped from the `exec.approval.requested/resolved` broadcast payloads.

- [ ] **Step 1:** Read TS for exact method names/params/results: `server-methods/exec-approvals.ts`, `server-methods/approval-shared.ts` (resolve), `core-descriptors.ts` scopes, `sessions-create.ts` / `sessions-patch.ts` payloads.
- [ ] **Step 2:** Implement `methods.go`/`events.go` to match the recorded shapes.
- [ ] **Step 3:** Unit test request/response marshaling (fixtures where derivable; structural otherwise).
- [ ] **Step 4:** `go vet ./...`; tests green; commit.

### Task 4: Per-session SSE hub

**Files:**
- Create: `internal/server/ssehub.go`, `internal/server/ssehub_test.go`

**Interfaces:**
- Consumes: `openclaw.Event`.
- Produces:
  - `type Hub struct{ ... }`; `NewHub() *Hub`
  - `(h *Hub) Register(sessionKey string) *Stream` (errors if already active for that session)
  - `(*Stream) Publish(openclaw.Event) error` (context-aware, buffered)
  - `(*Stream) Heartbeat(d time.Duration)` / writer loop `(*Stream) Run(w io.Writer, flush func(), done <-chan struct{})`; `(*Stream) Close()`
  - `(h *Hub) PublishTo(sessionKey string, ev openclaw.Event) bool` (false when no active stream)
  - `(h *Hub) Active(sessionKey string) bool`
- Enforces single active stream per session; second `Register` for a live key returns an error (caller 409s a concurrent turn).

- [ ] **Step 1:** Write `ssehub_test.go`: publish→single writer ordering, buffered backpressure (drop when full? no—block), unregister frees the key, duplicate register fails.
- [ ] **Step 2:** Implement `ssehub.go` to pass.
- [ ] **Step 3:** `go test ./internal/server/ -run TestSSEHub -v` green; commit.

### Task 5: ApprovalService + pending registry + /confirm endpoints

**Files:**
- Create: `internal/server/approvals.go`, `internal/server/approvals_test.go`
- Modify: `internal/server/server.go` (routes + wiring)

**Interfaces:**
- Consumes: `ws.Client` (Task 3), `Hub` (Task 4), `s.store`, `audit`.
- Produces:
  - `type ApprovalService struct{ hub *Hub; store *store.Store; logf func(...); resolve func(ctx, user, id, decision) error }`
  - `(*ApprovalService) HandleRequested(ev openclaw.WSApprovalRequested)` — store pending, `hub.PublishTo(session, confirm_pending)`.
  - `(*ApprovalService) Pending(user, sessionKey) (PendingApproval, bool)`
  - Handlers: `handleConfirm` (`POST /api/sessions/{key}/confirm`, body `{decision}`), `handlePendingConfirm` (`GET /api/sessions/{key}/confirm/pending`).
- Modifies `Event` usage: build `confirm_pending` (fields from Task 1) and `confirm_resolved`.

- [ ] **Step 1:** Write failing handler tests (approve path resolves + publishes `confirm_resolved`; reject path denies; unknown/expired pending → 404/409; not-owner guarded by the same `userOf` trust model as chat).
- [ ] **Step 2:** Implement service + handlers; wire routes in `server.go`.
- [ ] **Step 3:** `go test ./internal/server/ -run 'TestConfirm|TestApproval' -v` green; commit.

### Task 6: handleMessages via hub + ConfirmWrites gating

**Files:**
- Modify: `internal/server/handlers.go` (`handleMessages`, ~188–366), `internal/server/handlers_platform.go` or new `internal/server/policy.go`

**Interfaces:**
- Consumes: Hub (Task 4), a `ensureWSReady(ctx, user) (*ws.Client, error)` and `applyTurnPolicy(ctx, user, sessionKey)` helper.
- Produces: `handleMessages` publishes turn events through the session `Stream`; before streaming a `ConfirmWrites` turn it calls `ensureWSReady` (bounded) + `EnsureSessionGuarded`; on failure emits a `message_done` error.
- `applyTurnPolicy` derives gating from the user's resolved config `confirmPolicy` (`ConfirmWrites`) — read where `ResolvedConfigForUser` exposes it.

- [ ] **Step 1:** Read `internal/server/handlers.go` current I/O path and `internal/instances/manager.go` (`Ensure`, `BaseURL`, `ResolvedConfigForUser`) and `internal/resolver` to confirm where `confirmPolicy` surfaces.
- [ ] **Step 2:** Refactor `handleMessages` to publish events into a hub stream (single writer) instead of writing `w` directly; keep ledger/metrics/error semantics identical; add the two helper calls behind a small interface so tests inject a stub.
- [ ] **Step 3:** Keep `go test ./internal/server/...` + `go build ./...` green; commit.

### Task 7: Policy applier + device provisioning hook

**Files:**
- Create: `internal/server/policy.go` (allowlist builder + apply), `internal/server/policy_test.go`

**Interfaces:**
- Consumes: `ws.Client` `GetApprovalsPolicy`/`SetApprovalsPolicy`; resolved config `confirmPolicy`.
- Produces: `defaultReadAllowlist() []ws.AllowlistEntry`; `applyExecPolicy(ctx, user) error` (get→merge `agents."main".allowlist`→set, CAS retry).
- Also a provisioning hook stub/command that performs boot-time device pairing (loopback admin) — implement after a live-gateway verify in CI/e2e; documented env-flag gated.

- [ ] **Step 1:** Write allowlist builder test (read verbs + safe bins; tolerate `--kubeconfig=` prefix regex).
- [ ] **Step 2:** Implement `policy.go`; call `applyExecPolicy` at instance warm / revision change behind the same stub interface as Task 6.
- [ ] **Step 3:** Tests green; commit.

### Task 8: Web — confirm card + reload recovery

**Files:** (paths finalized after reading `web/src`)
- Modify: web api client, chat SSE consumer, a `ConfirmCard` component, styles.

**Interfaces:**
- Consumes: `POST /api/sessions/{key}/confirm`, `GET /api/sessions/{key}/confirm/pending`, SSE `confirm_pending`/`confirm_resolved`.
- Produces: user sees a card with the command + 批准/拒绝; card reflects resolved state; after reload a pending card is restored from the pending endpoint.

- [ ] **Step 1:** Read `web/src` chat/api structure; locate SSE parse + render points and the tool-call card to mirror styling.
- [ ] **Step 2:** Add api methods + event handling + card component (TDD where the frontend test setup allows; otherwise manual + typecheck).
- [ ] **Step 3:** `npm run build` (or repo equivalent) green; commit.

### Task 9: Cross-phase tests, vet, build, PR

- [ ] **Step 1:** `go vet ./...`, `go test ./...`, web build green.
- [ ] **Step 2:** Update `docs/cubepilot/implementation-status.md` HITL row and `docs/cubepilot/api.md` event table if needed.
- [ ] **Step 3:** Review the diff for the spec's acceptance items; note live-gateway-verification items in the PR body.
- [ ] **Step 4:** Commit remaining; push; open PR `Closes #20`; move issue to In review.

---

## Self-Review Notes (author)

- **Spec coverage vs tasks:** event fields (T1), WS client+auth (T2), approvals methods+events (T3), hub (T4), ApprovalService+/confirm (T5), handleMessages gating (T6), policy applier+provisioning (T7), web (T8), verification+PR (T9). Device provisioning live path and gateway-connect exactness are the two items that need a real gateway — flagged in T2/T7 and the PR body; every other item is unit-testable locally.
- **Type consistency:** Event fields (T1) are consumed in T5/T6; Hub API (T4) consumed in T5/T6; ws.Client methods (T2/T3) consumed in T5/T7. Signatures above are the single source of truth.
