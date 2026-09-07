# confirmPolicy: uniform public intent enum + per-instance allowlist — design

Date: 2026-09-07 · Status: draft for review · Scope: issue #116 · Refs: #20 (HITL origin), #115 (truthful Confirm Rules display)

## Context

`AgentTemplate.spec.confirmPolicy` is the template-level confirmation policy (design §3.1: the policy lives on the template, not the skill). Today it has two values, `None | ConfirmWrites` (`internal/api/v1alpha1/agenttemplate_types.go`), with `ConfirmWrites` the default.

Three facts motivate a reshape:

1. **`ConfirmWrites` is a misnomer.** Its real behavior — implemented as a guarded session (`permissionMode="guarded"` → OpenClaw `ask: on-miss`) plus an argv read-allowlist (`internal/server/policy.go`) — is: *read/commands on the platform safe-allowlist auto-pass; anything else on an interactive turn asks a human.* Reads outside the allowlist ask too. It is not "writes only".
2. **The value is read by the internal runtime path only, but the CRD is a public contract.** The Portal, audit, ledger and SSE cards key off the confirmation *events* (`confirm_pending` / `confirm_resolved`) and audit decisions — they never read `confirmPolicy`. The runtime enforcement path does (resolver → `ResolvedAgentConfig.ConfirmPolicy` → hitl manager). But the AgentTemplate CRD is the platform's first-class, externally-facing object (registry/visibility schema anticipates user-visible agents), so its vocabulary is a public API. A runtime dialect must not become the platform schema.
3. **A fixed platform-only allowlist is too coarse either way.** With only `None | Allowlist` and a frozen allowlist, an operator cannot quiet *trusted* reads (short of `None`, which drops write confirmation too) nor go stricter. Tuning belongs to the instance owner, not to a template default: a template-author edit to a shared list would ripple to every user of the template (the same reason personal preferences live on the instance — like `userInstructions`, `selectedModel`, `enabledSkills`).

## Verified current state (source-grounded)

- Enum + defaults in `internal/api/v1alpha1/agenttemplate_types.go`: `ConfirmPolicyNone`, `ConfirmPolicyConfirmWrites`; `+kubebuilder:default=ConfirmWrites`. Note on the type: "the policy lives on the AgentTemplate, not on the skill, so different templates reusing the same skill can have different confirmation rules".
- `ConfirmWrites` enforcement (OpenClaw, interactive turns only): session `permissionMode="guarded"` + argv allowlist under `agents."main"`, written at instance warm / config-revision change (`internal/server/hitl.go`, `applyAllowlist`); default allowlist = kubectl read verbs + read-only shell bins (`internal/server/policy.go`). `mergeAllowlists` unions existing and desired entries keyed by `pattern|argPattern` and preserves existing grants. Scheduled (`task-*`) and inspect (`inspect-*`) sessions are never gated.
- `None`: no guard, no allowlist write — runtime default `ask: off` (pass-through, audited).
- Per-instance override precedent: `AgentInstance.spec` already carries `templateRef`, `selectedModel`, `userInstructions`, `enabledSkills`; `ResolvedAgentConfig` merges template default + per-instance override (`internal/resolver/resolver.go`).
- Runtime selection already exists: `spec.runtime` (`RuntimeOpenClaw` default, `RuntimeHermes` reserved); `internal/openclaw/client.go` `AgentRuntime` is the runtime seam.

## Decision

**Public contract stays uniform and runtime-agnostic; enforcement stays bound to the selected runtime; the *tunable* surface lives per instance, not per template.**

### Public `confirmPolicy` values

| Value | Meaning | OpenClaw today | Note |
|---|---|---|---|
| `None` | nothing asks (pass-through, audited) | `ask: off` | |
| `Allowlist` (default) | operations on the **effective** allowlist auto-pass; everything else on interactive turns asks | guard + `ask: on-miss` + argv allowlist | replaces `ConfirmWrites` |
| `AlwaysAsk` | every operation asks (strict) | guard + `ask: always` (allowlist moot) | new value |

Uniform axis (`off` / `on-miss(+allowlist)` / `always`), expressed as platform intent, self-describing, and fixing the `ConfirmWrites` misnomer. Because the enum is **uniform** (not conditioned on `runtime`) no runtime-conditional CEL/cross-field validation is needed and the static default stays valid.

### Authority: template default, per-instance override

- `AgentTemplate.spec.confirmPolicy` sets the **default posture** (`Allowlist`). 
- `AgentInstance` may **override** it (additive value semantics; like `selectedModel`). Empty override = follow the template.
- End users therefore own their instance's confirmation posture; a template author changing the template default is a revision-tracked change that re-defaults instances (existing template-revision machinery), not a silent per-user edit.

### Allowlist is layered and user-editable at the instance level only

Effective allowlist = **platform default** ∪ **user delta**:

- **Platform default** (`policy.go`): the vetted safe-read list. **Immutable to users** — never deletable/modifiable through the user surface (mirrors the locked System-skills rows in the Agent Config UI).
- **User delta** (`AgentInstance.spec.allowlist`, new): entries the instance owner adds, and may **delete** freely (full CRUD over their own delta). Deleting removes only their entry — platform defaults always remain.
- **Store the delta, not the merged union.** Merging happens at resolve/enforcement time (`mergeAllowlists`), which is what makes delete meaningful (otherwise a delete cannot distinguish "mine" from "platform").
- No **template-level** allowlist base in phase 1: an author edit to a shared list ripples to all instances of the template (blast radius) — the exact failure the user called out. The platform default is the only global knob; anything else is per-instance.
- **User-added write patterns are allowed but self-authorizing**: a user who adds `kubectl delete *` disables confirmation for that pattern on their own guarded instance. This is acceptable because (a) confirmation and audit are independent rails — audit records what happened regardless of policy, (b) instance actions are bounded by per-user RBAC/identity, and (c) it is the instance owner's deliberate, in-band choice. UI should warn that a non-read pattern disables confirmation for it. A platform **non-negotiable floor** (deny / hardline list users cannot override — Hermes `approvals.deny` + hardline analog) is **reserved, out of scope for phase 1**, for when the platform must guarantee certain operations always confirm/deny.

### Enforcement is runtime-bound

For OpenClaw nothing about the current mechanism changes: `Allowlist` → guard + `on-miss` + effective argv allowlist; `AlwaysAsk` → guard + `ask: always` (allowlist irrelevant); `None` → no guard. A per-runtime enforcement adapter is extracted only when the second runtime lands. Entry grammar on `AgentInstance.spec.allowlist` is OpenClaw-shaped (`{pattern, argPattern?}`) today, matching `policy.go`; it is flagged as runtime-shaped and revisited with the second runtime.

## Cross-runtime mapping (documented fidelity, not fabricated parity)

| Public value | OpenClaw | Hermes (future) | DeepSeek Harness (future) |
|---|---|---|---|
| `None` | `ask: off` | `approvals.mode: off` | approval `never` |
| `Allowlist` (+ per-instance delta) | guard + `on-miss` + effective allowlist | `approvals.mode: manual` + injected `command_allowlist` (user delta → that profile's allowlist) (**not** `smart` — the guardian LLM self-approves, breaking the "else ask" promise) | sandbox-ask on escalation (approximation: no command allowlist; workspace writes auto-run) |
| `AlwaysAsk` | guard + `ask: always` | `approvals.mode: manual` for every op | ask on every tool |

## Work (implementation PRs, follow this design PR)

1. Rename `ConfirmWrites` → `Allowlist`; add `AlwaysAsk`; update constants, enum, kubebuilder markers + comments in `internal/api/v1alpha1/agenttemplate_types.go`, builtin seeding, resolver/hitl comments, tests; regenerate CRDs (`config/crd/bases`, helm CRDs).
2. `AgentInstance.spec.confirmPolicy` (optional override) + `AgentInstance.spec.allowlist` (user delta; entries `{pattern, argPattern?}`): CRD schema + resolver merge/override + revision.
3. Enforcement: hitl/policy branch for `AlwaysAsk` (`ask: always`), and effective-allowlist merge (platform default ∪ user delta) via existing `mergeAllowlists`.
4. Service API: read/write the instance override + allowlist delta (GET/PUT); read-only effective policy for #115.
5. Web Agent Config: confirmPolicy select (default from template + per-instance override + "use template default"); allowlist section with **System (locked)** vs **Mine (add/remove)** rows — reusing the existing Skills split UI.
6. No behavior change for the default path (default `Allowlist`, empty user delta); existing HITL e2e stays green.

## Out of scope (phase 2 / later)

- `AlwaysAsk` is in scope (new value); further values deferred.
- Platform non-negotiable floor / deny list (reserved for compliance guarantees).
- Template-level allowlist base (rejected: cross-user blast radius).
- Hermes / DeepSeek Harness drivers (only when those runtimes land).
- Runtime-neutral allowlist entry grammar (revisit with the second runtime).
- The Portal Confirm Rules card copy / effective-policy display mechanics (issue #115).

## Acceptance (issue #116)

- `AgentTemplate` and `AgentInstance` CRDs document `confirmPolicy: None | Allowlist | AlwaysAsk` (default `Allowlist`), with per-instance override and per-instance allowlist delta semantics as above.
- Default behavior is unchanged (guard + on-miss + platform read allowlist); existing HITL e2e stays green.
- User can add and delete their own allowlist entries; platform defaults cannot be removed through the user surface.
- No `ConfirmWrites` tokens remain in code/CRDs/docs.
- Design doc merged (this document).
