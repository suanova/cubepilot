# HITL: write-operation confirmation (issue #20) — design

Date: 2026-09-04 · Status: approved for implementation · Scope: issue #20 (phase-1
simple HITL on interactive chat turns)

## Context

Issue #20 asks for a best-effort HITL: rule-matched write operations (e.g.
`kubectl delete`) pause for a human decision from the Portal; rejection prevents
execution; reads pass through. `AgentTemplate.confirmPolicy` (`ConfirmWrites`
default) is declarative only today; `confirm_pending`/`confirm_resolved` events
are reserved but never emitted (see `docs/cubepilot/implementation-status.md`).

Issue #104 (closed) established the surrounding constraints: chat runs over
OpenClaw's OpenAI-compatible HTTP `/v1/chat/completions` ingress, which runs the
**full agent loop server-side** and never surfaces per-tool-call execution to
the caller. Native OpenClaw approvals are delivered/resolved only over the
gateway WebSocket protocol.

This design implements HITL for **interactive Portal chat turns** using OpenClaw
native exec approval, driven by a platform-controlled gateway WebSocket device
client. Scheduled tasks and one-shot inspections are **not** gated.

## Verified facts (OpenClaw gateway v2026.8.2, source-grounded)

- The `/v1/chat/completions` ingress runs the agent loop and native tools
  (including `exec kubectl`) in-process; only text deltas return. The platform
  cannot intercept a write before execution over HTTP.
- OpenClaw has built-in **exec approval** (`tools.exec`/per-agent policy with
  `ask: off|on-miss|always`). On the OpenAI-http turn the message channel is
  `webchat`, which is a native approval channel → a matched exec takes the
  **blocking/inline** path: the exec tool call parks and the HTTP turn stays
  open until resolved (`exec.approval.resolve`) or expired (default 30 min,
  deny on no-route/timeout).
- Pending approvals are broadcast to approval-capable gateway WS connections
  (`exec.approval.requested`) and resolved only via WS methods
  (`exec.approval.resolve` / `approval.resolve`). No HTTP approve endpoint.
- Exec approval supports an **argv-aware allowlist**: per-agent persisted policy
  entries `{ pattern, argPattern }` where `pattern` glob-matches the executable
  and `argPattern` is a regex over the remaining argv. `exec.approvals.get/set`
  read/write that policy (base-hash CAS). `tools.exec` in openclaw.json exposes
  no allowlist array.
- Effective exec `ask` is resolved per **session**: a session with
  `permissionMode = "guarded"` gets `ask: on-miss` (security allowlist); a
  session without `permissionMode` inherits the agent/tools default (`ask: off`,
  security `full`). `permissionMode` is set via `sessions.create` /
  `sessions.patch`. There is no channel/trigger-scoped knob, so gating must be
  expressed per session, not per agent.
- The OpenAI-http run with model `openclaw/default` and only `agents.defaults`
  resolves to agent id **`"main"`** — the exec allowlist must be written under
  `agents."main"` (or `agents."*"`).
- WS auth: a device-less remote connection presenting only the shared
  `mode:token` secret is admitted as role `operator` with **scopes cleared to
  `[]`**; it cannot call approvals methods. Device identity (signed proof) is
  required to hold `operator.admin`/`operator.approvals` over the network.
  `exec.approvals.get/set` require `operator.admin`; approval resolution requires
  `operator.approvals` (admin short-circuits). A loopback `gateway-client`/
  `backend` connection with declared scopes is the supported headless carve-out
  (`createOperatorApprovalsGatewayClient` pattern).

## Decisions

1. **Route**: native OpenClaw exec approval; chat stays on the existing HTTP
   `/v1/chat/completions` path (model override, scheduler, seed/replay, tool
   polling all unchanged). The platform adds one **gateway-protocol WebSocket
   device client per user agent**.
2. **WS placement & auth**: the WS client runs in the **platform API server**
   (already the single chat coordinator) and authenticates as a **paired
   device** (keypair, signed proof) whose approved scopes include
   `operator.admin`. The device is provisioned per gateway pod out-of-band
   (preferred: a one-time boot-time loopback admin connection approves the
   device pairing via `device.pair.approve`, using only public methods;
   fallback: operator seeds the device row in the gateway state DB). The
   private key never leaves the platform. No OpenClaw source changes.
3. **Rule carrier**: argv allowlist in the per-agent exec-approvals policy under
   `agents."main"`, written via `exec.approvals.get` → merge → `set` (CAS).
   Allowlist = read verbs / safe read-only commands; everything else on a gated
   session asks (superset of issue #20's "matched writes ask", and stricter for
   write variants).
4. **Coverage**: interactive Portal chat turns only. `ask` is enabled **per
   interactive session** by setting `permissionMode = "guarded"`; scheduled task
   (`task-*`) and inspect (`inspect-*`) sessions are never gated and keep the
   agent default `ask: off`.
5. **confirmPolicy mapping**: `ConfirmWrites` → apply allowlist + guard
   interactive sessions. `None` → do nothing (today's behavior).

## Architecture

```
 Browser (Portal) ──HTTP/SSE──▶ cubepilot-api (platform; single coordinator)
   ▲                                  │  ├─ HTTP POST /v1/chat/completions  (existing)
   │                                  │  └─ WS gateway protocol, device auth (new)
   └── confirm_pending / confirm_resolved SSE       ▼
                                         OpenClaw gateway (per-user Pod, v2026.8.2)
                                          exec write-miss on a guarded session
                                          → native exec approval (blocking; HTTP turn open)
```

### Components (all new code lives in cubepilot, not OpenClaw)

- **`internal/gateway/ws` (new package)** — Go gateway-protocol WS client:
  - `connect.challenge` with device identity (keypair + signed proof over the
    canonical payload), declared `client.id="gateway-client", mode="backend"`,
    scopes `[operator.admin, operator.read, operator.write, operator.approvals]`,
    caps `[APPROVALS, EXEC_APPROVALS]`.
  - Method calls: `exec.approvals.get/set`, `exec.approval.resolve`,
    `sessions.create/patch`, and approval lookups as needed.
  - Subscribes to `exec.approval.requested`/`resolved` broadcasts.
  - Reconnect with backoff; the gateway auto-expires pending approvals
    (`no-approval-route`) while disconnected → fail-closed.
- **`internal/server/approvals.go` (new)** — `ApprovalService`:
  - Holds `pendingRegistry{ approvalID → {user, sessionKey, command, …} }`,
    indexed by sessionKey.
  - Translates gateway events ↔ platform SSE events and calls the WS client on
    Portal decisions.
- **`internal/server/ssehub.go` (new)** — per-session SSE hub (single writer per
  active chat stream) so approval events arriving on the WS connection can be
  injected into the browser's open SSE stream. `handleMessages` registers its
  session stream and publishes every event through it; the hub owns
  `writeSSE`+flush, heartbeats (`: ping` every ~15 s while idle), and
  unregister on end/disconnect.
- **`internal/server/handlers.go` (modify)** — `handleMessages` streams events
  through the hub (local refactor of the I/O path; ledger logic unchanged); on a
  ConfirmWrites turn it waits (bounded) for the WS client, then applies
  `permissionMode=guarded` to the session.
- **HTTP endpoints**:
  - `POST /api/sessions/{key}/confirm` body `{decision: "approve"|"reject"}` →
    resolve via WS (`allow-once` | `deny`); publish `confirm_resolved`; record
    ledger + audit.
  - `GET /api/sessions/{key}/confirm/pending` → pending record for reload
    recovery.
- **`internal/instances` (modify)** — device provisioning hook at pod warm and
  WS connection lifecycle per user (connect/reconnect), keyed by the gateway
  base URL the instance manager already resolves.
- **`web/` (modify)** — confirmation card in the chat stream, approve/reject
  actions, pending-recovery after reload.

## Confirmation lifecycle (interactive session S, user U, ConfirmWrites)

```
1  handleMessages: append user row to ledger (existing)
2  ensure WS client ready (bounded); ensure session S exists; sessions.patch
   S.permissionMode="guarded"  (idempotent)
3  StreamChat over HTTP (existing); events streamed via the session hub
4  agent issues a write; exec policy (guarded → ask on-miss) + argv allowlist
   miss → native exec approval; blocking: HTTP turn stays open
5  gateway broadcasts exec.approval.requested → platform WS client receives
6  ApprovalService stores pending {approvalID, sessionKey, command}; hub
   injects confirm_pending {session_id, call_id=approvalID, tool:"exec",
   command, level:"write", message}
7  Portal shows card → user picks approve/reject
8  POST /api/sessions/S/confirm → ApprovalService resolves over WS
   (allow-once | deny)
     approve → exec runs; result returns into the still-open HTTP turn; existing
               transcript poller picks tool_call/tool_result for ledger
     reject  → exec returns a denied tool result; the write never executes; the
               agent observes the denial (may continue; phase-1 does not promise
               no-retry / run abort)
9  hub injects confirm_resolved {session_id, call_id, approved}
10 turn completes normally (message_done, TurnEnd)
```

## Event & data model

- Extend `internal/openclaw/events.go` `Event` with fields used by
  `confirm_pending`: `Command string`, `Level string` (`"read"|"write"`),
  `Message string`; and by `confirm_resolved`: `Approved bool`. `Tool`/`CallID`
  reuse existing `Name`/`CallID`. Constants `EventConfirmPending`/
  `EventConfirmResolved` already exist.
- Ledger: append a `role:"tool"`, `EventType:confirm_resolved` row per decision
  (Content = command) so history can show the decision; **seed replay keeps only
  `user`/`assistant` rows** so confirm rows never reach the model.
- Audit: record each decision (user, session, command, decision, timestamp)
  following the existing M5 audit entries (`internal/audit`).
- Metrics (`cubepilot_*`): confirmations pending/decided, decision outcome
  (approve/reject/timeout/no-route), WS connect failures, approval wait latency.

## Policy application

On instance warm and on resolved-config revision change (per user):
- `ConfirmWrites`: `exec.approvals.get` (base hash) → merge the phase-1 default
  allowlist into `agents."main".allowlist` (preserve other fields) →
  `exec.approvals.set`; retry on base-hash conflict. Per interactive turn:
  `sessions.patch permissionMode="guarded"`.
- `None`: no writes, no patches.

### Phase-1 default allowlist (shape; exact regex tuned in implementation)

- kubectl read verbs, tolerant of leading global flags (CRD discovery runs
  `kubectl --kubeconfig=<read-only> get crd …`):
  `get|list|watch|describe|api-resources|explain|version|logs|events|top`.
- Read-only safe commands by basename: `ls cat pwd grep head tail wc jq date env
  echo printf`. `curl` is intentionally not allowlisted (can write).

## Failure handling

- Browser disconnects mid-pending → hub unregisters → platform cancels the HTTP
  turn → the gateway approval aborts/expires → deny (fail-closed); pending row
  cleared.
- Platform WS client drops mid-pending → gateway `no-approval-route` → deny;
  client reconnects with backoff.
- Approval timeout (native, ~30 min) → deny.
- WS readiness gate times out before a ConfirmWrites turn → surface a
  `message_done` error rather than start a turn that could auto-deny writes.

## Open verification items (first tasks of the implementation plan)

1. Device connect protocol port (deriveDeviceId, canonical signed payload,
   nonce/skew) — port from TS, test against a real gateway.
2. Headless device provisioning via boot-time loopback admin +
   `device.pair.approve`; fallback to seeding the gateway state DB.
3. `exec.approval.requested` payload carries the gateway sessionKey (route to
   `conv-*`); else resolve via `exec.approval.get/<id>`.
4. Allowlisted commands short-circuit the safety analysis (`analysisOk`) so
   reads never ask.
5. Whether the OpenAI-http turn auto-creates its session (so `sessions.patch`
   has a target) or `sessions.create` is required first; exact create/patch
   payload for `permissionMode`.
6. Confirm resolved agent id is `"main"` for this deployment.
7. Web SSE parser's current event types and card render points.
8. No gateway read-timeout on a blocking approval over the OpenAI-http stream
   (heartbeat covers proxies).
9. Whether the OpenAI stream signals `waiting-approval` (optional UI hint).

## Acceptance mapping (issue #20)

- Matched writes (delete/create/apply …) pause and emit `confirm_pending` —
  via allowlist-miss on a guarded session (superset of "matched").
- Portal approve → `confirm_resolved` → execution continues; reject → write
  does not execute.
- Reads on the allowlist pass without confirmation. Reads outside the allowlist
  on a gated session ask (stricter than "all reads pass"; documented).
- With `confirmPolicy=ConfirmWrites` writes require confirmation; write variants
  are caught more often (non-listed → ask); compound/`sh -c`-wrapped commands
  remain a known limitation (issue #20 Notes).
- Unit tests: allowlist matcher, pending registry, SSE hub, /confirm handler,
  event mapping, policy CAS; e2e against a real gateway.

## Out of scope (phase 2 / later)

- No-retry / reject-aborts-run, fail-closed-on-timeout wording of issue #20
  (native behavior already fails closed; the "no retry" policy is not added).
- `allow-always` (durable per-command grants) — conflicts with the platform-owned
  allowlist bookkeeping.
- Full WS chat migration (`chat.send`); model override, scheduler and
  seed/replay stay on the HTTP path.
