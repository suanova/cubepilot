# confirmPolicy: uniform public intent enum + runtime-bound enforcement — design

Date: 2026-09-07 · Status: draft for review · Scope: issue #116 · Refs: #20 (HITL origin), #115 (truthful Confirm Rules display)

## Context

`AgentTemplate.spec.confirmPolicy` is the template-level confirmation policy (design §3.1: the policy lives on the template, not the skill). Today it has two values, `None | ConfirmWrites` (`internal/api/v1alpha1/agenttemplate_types.go`), with `ConfirmWrites` the default.

Two facts motivate a reshape:

1. **`ConfirmWrites` is a misnomer.** Its real behavior — implemented as a guarded session (`permissionMode="guarded"` → OpenClaw `ask: on-miss`) plus an argv read-allowlist (`internal/server/policy.go`) — is: *read/commands on the platform safe-allowlist auto-pass; anything else on an interactive turn asks a human.* Reads outside the allowlist ask too. It is not "writes only".
2. **The value today is read by the internal runtime path only, but the CRD is a public contract.** The Portal, audit, ledger and SSE cards key off the confirmation *events* (`confirm_pending` / `confirm_resolved`) and audit decisions — they never read `confirmPolicy`. The runtime enforcement path does (resolver → `ResolvedAgentConfig.ConfirmPolicy` → hitl manager). But the AgentTemplate CRD is the platform's first-class, externally-facing object (registry/visibility schema anticipates user-visible agents), so its vocabulary is a public API. A runtime dialect must not become the platform schema.

## Verified current state (source-grounded)

- Enum + defaults in `internal/api/v1alpha1/agenttemplate_types.go`: `ConfirmPolicyNone`, `ConfirmPolicyConfirmWrites`; `+kubebuilder:default=ConfirmWrites`. Note on the type: "the policy lives on the AgentTemplate, not on the skill, so different templates reusing the same skill can have different confirmation rules".
- `ConfirmWrites` enforcement (OpenClaw, interactive turns only): session `permissionMode="guarded"` + argv allowlist under `agents."main"`, written at instance warm / config-revision change (`internal/server/hitl.go`, `applyAllowlist`); default allowlist = kubectl read verbs + read-only shell bins (`internal/server/policy.go`, merged, union with existing allow-always grants). Scheduled (`task-*`) and inspect (`inspect-*`) sessions are never gated.
- `None`: no guard, no allowlist write — runtime default `ask: off` (pass-through, audited).
- Runtime selection already exists: `spec.runtime` (`RuntimeOpenClaw` default, `RuntimeHermes` reserved phase 3); `internal/openclaw/client.go` `AgentRuntime` interface is the runtime seam (chat surface today).

## Decision

**Public contract stays uniform and runtime-agnostic; enforcement stays bound to the selected runtime.**

### Public `confirmPolicy` values

| Value | Meaning | OpenClaw today | Note |
|---|---|---|---|
| `None` | nothing asks (pass-through, audited) | `ask: off` | |
| `Allowlist` (default) | platform safe-allowlist matches auto-pass; everything else on interactive turns asks | guard + `ask: on-miss` + argv read allowlist | replaces `ConfirmWrites` |
| `AlwaysAsk` | every operation asks (strict) | `ask: always` | **reserved — not added now**; additive and non-breaking when a runtime needs it |

The two shipped values are the same axis as OpenClaw `off` / `on-miss(+allowlist)`, expressed as platform intent, and are self-describing (the name says what gates: an allowlist, else ask). This also fixes the `ConfirmWrites` misnomer without changing the default's behavior.

Because the enum is **uniform** (not conditioned on `runtime`):
- no runtime-conditional CEL / cross-field validation is needed;
- the static `+kubebuilder:default` stays valid (`Allowlist`), so defaulting keeps living in the CRD schema rather than moving into the controller.

### Allowlist is decoupled from the enum

`Allowlist`'s gate list is the platform default safe-read allowlist (`policy.go`). It stays platform-owned and is **not part of the enum's meaning**: it is separate, defaulted configuration. Tuning/exposing it is out of scope here; issue #115 covers making the effective policy truthfully displayable (which requires the API to surface it read-only).

### Enforcement is runtime-bound

For the OpenClaw runtime nothing changes: the existing hitl/policy path is the `Allowlist` enforcement. A per-runtime enforcement adapter is extracted **only when the second runtime lands** (the `AgentRuntime` seam). Until then a runtime switch is enough.

## Cross-runtime mapping (documented fidelity, not fabricated parity)

Each runtime adapts the *same* public values to its own primitives; gaps are recorded, not hidden:

| Public value | OpenClaw | Hermes (future) | DeepSeek Harness (future) |
|---|---|---|---|
| `None` | `ask: off` | `approvals.mode: off` | approval `never` |
| `Allowlist` | guard + `on-miss` + argv read allowlist | `approvals.mode: manual` + injected `command_allowlist` (**not** `smart` — the guardian LLM self-approves, breaking the "else ask" promise) | sandbox-ask on escalation (approximation: no command allowlist; workspace writes auto-run) |
| `AlwaysAsk` (reserved) | `ask: always` | `approvals.mode: manual` for every op | ask on every tool |

## Work (implementation PR, follows this design PR)

- Rename `ConfirmWrites` → `Allowlist` throughout: constants + enum value + comments in `internal/api/v1alpha1/agenttemplate_types.go`, builtin seeding, `internal/resolver`, `internal/server/hitl.go` comments, tests.
- Update kubebuilder enum / default markers and regenerate CRDs (`config/crd/bases`, helm CRDs) with corrected doc comments.
- No behavior change for the default path; confirm via existing tests + e2e.

## Out of scope

- `AlwaysAsk` value (add only when a runtime needs it).
- User-configurable allowlist.
- Hermes / DeepSeek Harness drivers (only when those runtimes land).
- The Portal Confirm Rules card copy / effective-policy display (issue #115).

## Acceptance (issue #116)

- `AgentTemplate` accepts `confirmPolicy: None | Allowlist` (default `Allowlist`); CRD docs state the corrected semantics.
- Default behavior is unchanged (guard + on-miss + argv read allowlist) — existing HITL e2e stays green.
- No `ConfirmWrites` tokens remain in code/CRDs/docs.
- Design doc merged (this document).
