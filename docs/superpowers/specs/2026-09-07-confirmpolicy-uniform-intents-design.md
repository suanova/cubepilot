# confirmPolicy: uniform intents + template-default/per-instance-owned allowlist — design

Date: 2026-09-07 · Status: draft for review · Scope: issue #116 · Refs: #20 (HITL origin), #115 (truthful Confirm Rules display)

## Context

`AgentTemplate.spec.confirmPolicy` is the template-level confirmation policy (design §3.1: the policy lives on the template, not the skill). Today it has two values, `None | ConfirmWrites` (`internal/api/v1alpha1/agenttemplate_types.go`), with `ConfirmWrites` the default.

Facts motivating a reshape:

1. **`ConfirmWrites` is a misnomer.** Its real behavior — a guarded session (`permissionMode="guarded"` → OpenClaw `ask: on-miss`) plus an argv read-allowlist (`internal/server/policy.go`) — is: *read/commands on the effective allowlist auto-pass; anything else on an interactive turn asks.* Reads outside the allowlist ask too. It is not "writes only".
2. **The CRD is a public contract.** Portal/audit/ledger/SSE key off confirmation *events* and audit decisions, never the policy value; the value is read only by the runtime enforcement path (resolver → `ResolvedAgentConfig.ConfirmPolicy` → hitl manager). But the AgentTemplate CRD is the platform's first-class, externally-facing object (registry/visibility anticipate user-visible agents): runtime dialect must not become the platform schema.
3. **A frozen platform allowlist is too coarse.** Without a strict-still-safer or a wider-but-audited option you can neither quiet trusted reads nor go stricter. Tuning belongs to the instance owner; a template author editing a *shared* tuning list ripples to every user of the template (blast radius). But template *defaults* legitimately evolve — so the field needs an explicit inherit/own rule, not a frozen snapshot.

## Verified current state (source-grounded)

- Enum/defaults in `internal/api/v1alpha1/agenttemplate_types.go`: `ConfirmPolicyNone`, `ConfirmPolicyConfirmWrites`, `+kubebuilder:default=ConfirmWrites`. Type comment: "the policy lives on the AgentTemplate, not on the skill, so different templates reusing the same skill can have different confirmation rules".
- `ConfirmWrites` enforcement (OpenClaw, interactive turns only): guard session + argv allowlist under `agents."main"` written at instance warm / config-revision change (`internal/server/hitl.go` `applyAllowlist`); default allowlist = kubectl read verbs + read-only shell bins (`internal/server/policy.go`); `mergeAllowlists` unions entries keyed by `pattern|argPattern` and preserves existing grants. Scheduled (`task-*`) and inspect (`inspect-*`) sessions never gated. `None`: no guard (`ask: off`, pass-through, audited).
- Per-instance override precedent (`AgentInstance.spec`): `selectedModel` (empty = template default), `userInstructions` (appended after template, cannot weaken security/identity bounds), `enabledSkills` (empty = inherit the template's declared set; first restriction materializes an owned allow-list — issue #24). `TemplateRef` comment: "template updates take effect on the next reconcile/restart".
- Runtime seam exists: `spec.runtime` (`RuntimeOpenClaw` default, `RuntimeHermes` reserved); `internal/openclaw/client.go` `AgentRuntime` interface.

## Decision

**Public contract stays uniform and runtime-agnostic; enforcement stays bound to the selected runtime; template holds *defaults*, instance holds *ownership* (inherit-or-own, live), and a platform immutable floor is reserved.**

### Public `confirmPolicy` values

| Value | Meaning | OpenClaw today | Note |
|---|---|---|---|
| `None` | nothing asks (pass-through, audited) | `ask: off` | |
| `Allowlist` (default) | operations on the **effective** allowlist auto-pass; everything else on interactive turns asks | guard + `ask: on-miss` + argv allowlist | replaces `ConfirmWrites` |
| `AlwaysAsk` | every operation asks (strict) | guard + `ask: always` (allowlist moot) | new |

Uniform axis (`off` / `on-miss(+allowlist)` / `always`) expressed as platform intent. Because the enum is uniform (not conditioned on `runtime`), no runtime-conditional validation is needed and the static default stays valid.

**Scope: interactive sessions only.** As today (issue #20), `task-*` and `inspect-*` sessions are never gated, and `AlwaysAsk` is no stricter in reach — it raises the bar *within* interactive turns only. Non-interactive execution keeps its runtime default regardless of the policy value.

### Authority: template default; instance inherit-or-own

- `AgentTemplate.spec.confirmPolicy` sets the default (`Allowlist`).
- `AgentInstance.spec.confirmPolicy` (new, optional): empty = follow the template (live); set = the instance's own posture. Same shape as `selectedModel`.
- `AgentInstance.spec.allowlist` (new, optional): empty = inherit the template's effective default allowlist (live); non-empty = the instance **owns** its full allowlist (materialized on first edit; authoritative thereafter).

### Effective allowlist

```
effective_default(runtime) = platform builtin (policy.go) ∪ AgentTemplate.spec.allowlist (optional, author default)
effective(runtime)         = AgentInstance.spec.allowlist if non-empty
                             else effective_default(runtime)
```

- Template-level allowlist is a *default for that template type*, not a shared tuning list. It is inherited live by un-owned instances and ignored by owned ones — which answers the blast-radius objection: only instances that never took ownership follow a default change.
- **First explicit edit materializes**: when an un-owned instance adds or removes an entry, the controller writes the current effective list into `AgentInstance.spec.allowlist` and applies the edit. Afterwards the stored list is authoritative; template/platform default changes do not flow in, and a removed entry stays removed (the controller never re-seeds an owned list).
- Clearing the instance list returns the instance to inheriting (an explicit "follow template default again" affordance — optional in phase 1).
- Platform builtin entries are not special at storage: once owned, an instance may delete even a platform read (the result is *more* asking — the safe direction). The real safety rails are audit + per-user RBAC + the reserved floor below.
- Entry grammar is OpenClaw-shaped (`{pattern, argPattern?}`) matching `policy.go` today; flagged runtime-shaped, revisited with the second runtime.

### allow-once / allow-always

- **allow-once** is today's Portal **Approve** (`exec.approval.resolve` with `allow-once`); not persistent; a reload mid-pending recovers via `GET .../confirm/pending`.
- **allow-always** becomes an **allowlist append**, not a separate grant store: a pending confirmation card offers **"Always allow"** → the platform derives a conservative entry for the observed command and appends it to the instance allowlist (materializing first if the instance was inheriting), then allows the pending call. Thereafter the command auto-passes under `Allowlist`. This is the same gesture as OpenClaw's allow-always (mints an allowlist entry) and Hermes's `always` (writes `command_allowlist`). Issue #20's "allow-always conflicts with platform-owned allowlist bookkeeping" dissolves because the bookkeeping is the instance allowlist itself, rewritten by the platform each revision.
- "Always allow" is offered only when the effective mode is `Allowlist` (under `AlwaysAsk` everything asks; under `None` nothing does). User-added patterns are self-authorizing (see below).

### Self-authorization and the platform floor

A user who adds a write pattern (e.g. `kubectl delete *`) disables confirmation for it on their own guarded instance. Acceptable because: confirmation and audit are independent rails (audit records what happened regardless of policy); instance actions are bounded by per-user RBAC/identity; and it is the instance owner's deliberate in-band choice — the UI warns that a non-read pattern disables confirmation for it. A platform **non-negotiable floor** (deny / hardline list users cannot override — the Hermes `approvals.deny` + hardline analog, guaranteeing certain operations always confirm/deny) is **reserved, out of scope for phase 1**.

## Generalized inheritance rule (applies to all override-able fields)

The allowlist "empty = inherit / first edit = own" rule is one application of a convention that already governs the other per-instance override fields and should be stated once:

> **Instance state = template default, unless the instance has explicitly taken ownership. Ownership is visible (non-empty) and authoritative; template updates flow only to fields the instance has not owned, and never silently rewrite a user's divergence.**

Fields take three shapes; the convention is applied per shape:

| Shape | Fields | Semantics |
|---|---|---|
| Scalar override | `selectedModel`, `confirmPolicy` (new) | empty = inherit template default (live); set = owned. No materialization needed. |
| Additive text | `userInstructions` | user content is disjoint from the template base (appended after; cannot weaken security/identity bounds) — inherently isolated; template updates never touch the user's slice. |
| Whole-set default | `allowlist` (new), `enabledSkills` | empty = inherit the template's effective default (live); the **first explicit edit materializes** the current effective value into the instance and applies the edit; afterwards the stored list is authoritative and template default changes do not flow in. Materialization is what makes removing a base entry meaningful. |

Empty semantics must be uniform and stated per field. The current `enabledSkills` comment ("empty = all declared" / "all enabled baseline" wording) is normalized to **empty = inherit the template default** (for the builtin template that default is all platform skills). `Lifecycle` (pointer) follows the scalar rule; instance-scoped fields (`identity`, `credentials`, `dataVolume`, `owner`, `templateRef`) are not template defaults and are outside this rule.

Documentation: promote this to design doc §3.2 as an "Instance override & inheritance conventions" subsection, as part of the issue #116 implementation PR (which already updates CRD comments).

## Enforcement (runtime-bound)

OpenClaw path is unchanged in mechanism: `Allowlist` → guard + `on-miss` + effective argv allowlist; `AlwaysAsk` → guard + `ask: always` (allowlist irrelevant); `None` → no guard. A per-runtime enforcement adapter is extracted only when the second runtime lands. Approval events (`confirm_pending` / `confirm_resolved`), ledger and audit are unchanged and runtime-agnostic.

## Cross-runtime mapping (documented fidelity, not fabricated parity)

| Public value | OpenClaw | Hermes (future) | DeepSeek Harness (future) |
|---|---|---|---|
| `None` | `ask: off` | `approvals.mode: off` | approval `never` |
| `Allowlist` (+ owned list → allowlist) | guard + `on-miss` + effective allowlist | `approvals.mode: manual` + `command_allowlist` = platform default ∪ template allowlist ∪ user delta (**not** `smart` — the guardian LLM self-approves, breaking the "else ask" promise) | **unsupported** — sandbox-ask on escalation is *not* an `Allowlist` implementation: it has no command allowlist and auto-runs workspace writes, so it cannot honor "operations outside the allowlist ask". Any DSH support needs its own public semantics, deferred |
| `AlwaysAsk` | guard + `ask: always` | `approvals.mode: manual` for every op | ask on every tool |

## Work (implementation PRs, follow this design PR)

1. Rename `ConfirmWrites` → `Allowlist`; add `AlwaysAsk`; update constants, enum, kubebuilder markers + comments (`agenttemplate_types.go`), builtin seeding, resolver/hitl comments, tests; regenerate CRDs (`config/crd/bases`, helm CRDs).
2. `AgentTemplate.spec.allowlist` (author default) + `AgentInstance.spec.confirmPolicy` (override) + `AgentInstance.spec.allowlist` (owned list): schema, resolver effective computation, materialize-on-first-edit in the controller, revision.
3. Enforcement: hitl/policy branch for `AlwaysAsk` (`ask: always`); allow-always append path (derive entry, materialize if inheriting); effective-allowlist merge.
4. Service API: read/write confirmPolicy override + allowlist; read-only effective policy (for #115).
5. Web Agent Config: confirmPolicy select (template default + per-instance override + "use template default"); allowlist section showing effective list with platform-builtin vs user-added grouping at render; add/remove; confirm card gains "Always allow".
6. Normalize `enabledSkills` empty comment; add design doc §3.2 "Instance override & inheritance conventions".
7. No behavior change for the default path (default `Allowlist`, empty overrides/lists); existing HITL e2e stays green.

## Out of scope (phase 2 / later)

- Platform non-negotiable floor / deny list (reserved for compliance guarantees).
- Hermes / DeepSeek Harness drivers (only when those runtimes land).
- Runtime-neutral allowlist entry grammar (revisit with the second runtime).
- A "rebase instance to new template default" affordance for owned instances.
- The Portal Confirm Rules card copy / effective-policy display mechanics (issue #115).
- **Upgrade/conversion of stored `ConfirmWrites` values.** Pre-release (v1 not shipped) — no compatibility/conversion branch (repo convention, cf. issue #100); leftover dev objects carrying the old value are re-seeded on redeploy. If a stray `ConfirmWrites` is ever read it must not be silently treated as `Allowlist` — the resolver leaves it unknown and `PreTurn` does not gate, which is the safe (fail-closed) reading.

## Acceptance (issue #116)

- CRDs document `confirmPolicy: None | Allowlist | AlwaysAsk`; **only `AgentTemplate.spec.confirmPolicy` carries the `Allowlist` default** — `AgentInstance.spec.confirmPolicy` stays unset when omitted so it inherits the template value (template `None`/`AlwaysAsk` must not be masked by an instance default).
- Template default allowlist update reaches inheriting instances and does not rewrite owned ones.
- User can add, and delete, allowlist entries; deleting an owned entry sticks; enforcement reconciles the allowlist to the effective state (a deleted entry stops auto-passing).
- allow-always appends to the instance allowlist; allow-once is unchanged.
- Default behavior unchanged (guard + on-miss + platform read allowlist); existing HITL e2e stays green.
- **Service API is owner-scoped:** GET/PUT `/api/agent/confirm` operate only on the caller's own default instance (resolved from the request identity, `s.userOf`) — there is no cross-owner target selector, and a request for a user with no instance is not-provisioned/409. Audit records do not substitute for this authorization boundary.
- No `ConfirmWrites` tokens remain in active code / CRDs / API docs (this design doc intentionally references it to describe the rename).
- Design doc merged (this document) and the §3.2 inheritance conventions section landed.
