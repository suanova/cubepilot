# Agent allowlist: provenance split and the inherit-or-own fork

Issue: [#185](https://github.com/suanova/cubepilot/issues/185)

## Context

The agent safe-command allowlist is assembled from three layers:

| Layer | Source | Written by |
| --- | --- | --- |
| Platform builtin | `allowlist.Default()` -- code | platform release |
| Template default | `AgentTemplate.spec.allowlist` | template admin |
| Instance owned | `AgentInstance.spec.allowlist` | **two writers**: the user (AgentView) and the machine (the `allow-always` button) |

`allowlist.Effective` resolves them with an "owned is authoritative" rule
(`internal/allowlist/allowlist.go:101`):

```go
func Effective(owned, templateAllowlist []v1alpha1.AllowlistRule) []v1alpha1.AllowlistRule {
	if len(owned) > 0 {
		return owned // authoritative; the builtin is dropped entirely
	}
	return Merge(Default(), templateAllowlist)
}
```

## Problem

### The fork

`len(owned) > 0` doubles as the ownership sentinel, so **the first write of any
kind** -- an `allow-always` click, a manual add, or even a manual *remove*
(`web/src/views/AgentView.tsx:164-166`) -- snapshots the currently effective list
into `spec`:

```go
// internal/server/handlers_agent_approval.go:239-245
base := inst.Spec.Allowlist
if len(base) == 0 && s.mgr != nil {
    base = cfg.Allowlist // materialize the inherited default on first ownership
}
inst.Spec.Allowlist = allowlist.Merge(base, []v1alpha1.AllowlistRule{rule})
```

After that `len(owned) > 0` is permanently true and the builtin is never
consulted again for that instance. The freeze is asymmetric:

| Platform change | Reaches a frozen instance? | Direction |
| --- | --- | --- |
| `Default()` removes a command (hardening) | no -- the snapshot still holds it | **fail-open** |
| Template `allowlist` removes an entry | no | fail-open |
| builtin / template adds an entry | no | fail-closed (annoyance) |

A user keeps auto-passing a command the platform later judged unsafe, and
nothing tells them. The only way back is the Reset button
(`web/src/views/AgentView.tsx:169-172`), which silently clears ownership.

### The root cause

Machine-written state was given desired-state semantics. The ambiguity is not in
the click path -- it is that one field carries two provenances and expresses
ownership by being empty or not. The fix is to stop the machine writing that
field, not to add another special case to it.

### Secondary

- **No validation.** `argPattern` is free text from the UI
  (`web/src/views/AgentView.tsx:152`) shipped verbatim to the gateway
  (`internal/server/approvals.go:533`). Regex compilation happens in OpenClaw,
  so an invalid pattern is stored and pushed with no feedback.
- **Unbounded growth, but human-bounded.** No cap, TTL, LRU or subsumption;
  `Merge` dedups on exact `pattern|argPattern` only, and
  `deriveAllowAlwaysRule` anchors the exact argv so `-n a` / `-n b` are separate
  entries. Realistic volume is a few hundred entries (~50 KB) against the 1.5 MiB
  etcd limit, so this is **not** the urgent problem. Stated plainly so it is not
  mistaken for one.

## Design

### Resolution becomes an unconditional union

```go
// internal/allowlist
func Effective(templateAllowlist, instanceAllowlist, grants []v1alpha1.AllowlistRule) []v1alpha1.AllowlistRule {
	all := append(append(append([]v1alpha1.AllowlistRule{}, templateAllowlist...),
		instanceAllowlist...), grants...)
	return Merge(Default(), all)
}
```

`spec.allowlist` becomes purely additive: empty means "add nothing", not
"inherit and take over".

#### The union alone does not fix the fork -- the writers must change too

An earlier revision of this spec treated the union as the whole fix. It is not,
and the gap is worth recording because it is easy to re-derive wrongly.

`allowlistAlways` and the Portal's `addRule`/`removeRule` all copy the *resolved
effective list* -- platform builtin included -- into `spec` on first use. A union
then faithfully includes that snapshot, so a builtin **removed** from `Default()`
(a hardening) still auto-passes for that instance, reached through the spec
instead of through `Effective`. Same fail-open direction, different route.

So the fork fix is both halves, and they belong in one change:

1. `Effective` unions instead of letting the instance own the list.
2. Neither writer copies the inherited list in -- `allowlistAlways` appends to
   `inst.Spec.Allowlist` alone, and the Portal writes only `allowlistOwned`.

With the builtin supplied live by the union and no snapshot left in the spec to
be unioned back in, nothing can freeze, for today's writers or any added later.

#### Consequence: a builtin entry can no longer be deleted

Today a user can remove a builtin from their list and it stops auto-passing.
Under a union it comes straight back.

**Decision: accept the loss.** A per-entry opt-out of commands the platform has
vetted read-only is a rare want, and `AlwaysAsk` already covers the strict
posture completely and unambiguously. A `spec.allowlistDeny` overlay would need
its own composition rules (does deny beat a template add? a builtin?) for a
capability with no demonstrated demand. If a concrete need appears, add the
overlay then -- it is additive.

### Learned grants move to a per-user ConfigMap

```
AgentTemplate.spec.allowlist     unchanged, declarative
AgentInstance.spec.allowlist     unchanged, hand-authored only
ConfigMap cubepilot-grants-<user>  new, sole writer is the API server
```

**Layout -- one key per grant**, so a write is a single-key patch and cannot
conflict with a concurrent grant:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: cubepilot-grants-<Sanitize(user)>   # internal/k8s/client.go:158
  namespace: <install namespace>
  ownerReferences:                          # GC'd with the instance
    - apiVersion: ai.cubestack.io/v1alpha1
      kind: AgentInstance
      name: <Sanitize(user)>-<agent>
      uid: <instance UID>                   # GC matches owners by UID
      controller: false
data:
  <sha256(pattern|argPattern)[:32]>: |
    {"pattern":"kubectl",
     "argPattern":"^get pods -n foo$",
     "command":"kubectl get pods -n foo",
     "createdAt":"2026-09-14T10:00:00Z"}
```

The owner reference has to carry the instance's UID, not just its name:
Kubernetes garbage collection identifies an owner by UID, and a reference with an
empty one is dangling -- the dependent is then liable to be deleted rather than
collected with its owner. With no instance to point at, the ConfigMap is created
without an owner reference instead of with a broken one.

Why a ConfigMap and not a CRD, `status`, or the runtime:

- **Single writer.** A ConfigMap is written only by the API server. Putting
  grants in `AgentInstance.status` would make the controller and the API both
  perform whole-object `Status().Update()`, which is the same
  full-object-overwrite bug class that `applyPolicy` already exhibits (see
  "Latent, not live" below) -- introduced knowingly this time.
- **No schema churn.** `createdAt` (and later `lastUsedAt`/`hitCount`) are JSON
  fields, not CRD fields. That metadata is the prerequisite for the cap below.
- **Auditable.** `kubectl get cm` answers "what has this agent been
  auto-allowed", which matters for a multi-tenant platform.
- **One object kind and ~6 lines of RBAC**, versus a CRD's codegen, scheme
  registration and lifecycle.

The value of not using the runtime's own store is argued under "Rejected
alternatives".

### Write paths

`allow-always` keeps its current logic and changes only its destination --
`internal/server/handlers_agent_approval.go`:

```go
// was: inst.Spec.Allowlist = allowlist.Merge(base, []v1alpha1.AllowlistRule{rule})
if err := s.grants.Add(ctx, user, rule); err != nil { ... }
```

`PUT /api/v1/agent/approval` keeps writing `spec.allowlist`. An ordinary PUT
leaves grants alone -- so the Reset button no longer discards learned grants as a
side effect, which it does today. A PUT carrying the additive `revokeGrants`
field is the exception: after the spec change is saved, those grants are removed
from the store, because a grant no longer lives in `spec.allowlist` and
rewriting that list cannot express dropping one. The order matters and is
deliberate -- both operations are idempotent, so saving first leaves a retryable
state (the policy persisted, the grant still present) where dropping the grant
first would answer 500 having already deleted it.

`grants.Add` writes one ConfigMap key via a patch. On exceeding `MaxGrants`
(1000) it deletes the oldest keys by `createdAt` in the same operation. The cap
is insurance against a pathological click loop, not a response to normal use: it
sits far above the order-of-300 entries a heavy user accumulates in a year, or
it would evict grants people still rely on. At roughly 250 bytes per entry
(32-char key plus the JSON value, with the command text truncated) 1000 entries
is about a quarter of the 1 MiB ConfigMap ceiling. The cap bounds the entry
count, not the bytes: the per-entry bounds (512-byte command, 4 KB rule)
multiplied by 1000 is several MiB, so `Add` also evicts against a payload budget
well under the ceiling, and refuses a single rule too large to store rather than
truncating a regex that the gateway matches.

### Read paths

- `resolver.ResolveForUser` (`internal/resolver/resolver.go:218`) gains one
  ConfigMap Get and calls the new `Effective`. Grants already flow into
  `cfg.Revision` via `fingerprint()`, so changing a grant still bumps the
  revision and the next `PreTurn` pushes it -- the existing propagation is
  unchanged.
- `agentApprovalView` (`internal/server/handlers_agent_approval.go:145-175`)
  reads the ConfigMap to render the learned group.

`cmd/cubepilot-api/main.go:48` builds the client with `client.New` -- a **direct,
uncached** client, so each of these is a live API call. One extra Get per turn
(`PreTurn`) and per supervisor poll is acceptable; a cached client is not
warranted yet, and is a separate change if it becomes one.

### API view shape

Additive only, since the v1 contract was just frozen (#181):

```jsonc
"allowlist":        [{"pattern","argPattern?","label?","source"}],  // + source
"allowlistOwned":   [...],   // unchanged
"allowlistLearned": [...]    // new
```

`source` is `builtin | template | user | learned`, replacing the client-side
`isOwned` guess so the UI can group by provenance instead of rendering one
merged list with an ownership flag.

### Validation

`argPattern` is compiled with `regexp.Compile` in the API on every write path
(manual add and `deriveAllowAlwaysRule`) and rejected with a clear error.
`pattern` is a command name, not a regex -- it stays unvalidated beyond
non-emptiness, matching what the runtime expects.

## Latent, not live

Two things I initially recorded as live bugs, corrected after re-deriving them:

**`ws.AllowlistEntry` drops entry fields -- latent.**
`internal/openclaw/ws/frames.go:122-128` declares only
`ID`/`Pattern`/`ArgPattern`/`Source`, while the runtime's entry also carries
`lastUsedAt`, `commandText`, `lastUsedCommand`, `lastResolvedPath`. Go drops
unknown JSON fields on decode, so a get->set round trip erases them. But the
platform's `applyPolicy` replaces the whole allowlist with entries it authored
itself, which have none of those fields, and nothing else writes to the
allowlist under the current design. So the erasure is unreachable today -- it
becomes real only if we ever preserve runtime-minted entries. **Not fixed by
this design; recorded so it is not rediscovered as a new bug.** It becomes a
prerequisite the moment the "preserve runtime grants" alternative is revisited.

**`applyPolicy` clobbers runtime-minted grants -- intentional, but silent.**
`agent.Allowlist = toWSEntries(allow)` (`internal/server/approvals.go:505-512`)
is a wholesale replace and `exec.approvals.set` has no server-side merge keyed
on `source`, so any grant made through OpenClaw's own surfaces (TUI,
`openclaw approvals allow-always`, ACP, Slack) is deleted on the next push. That
is consistent with the deliberate "the platform's bookkeeping is the instance
allowlist" stance recorded at `internal/server/approvals.go:486-493`, and it is
fail-closed. The gap is that it is **silent**.

**Decision: keep the wholesale replace, document it.** The Portal is the only
surface cubepilot exposes, so in practice nothing else writes. Adding a
`source`-keyed partition to preserve entries the platform cannot render or
enforce bounds on would trade a clean authority boundary for a maintenance
obligation.

## Rejected alternatives

### Store grants in the runtime (OpenClaw owns them)

Pass `allow-always` through to `exec.approval.resolve` instead of downgrading it
to `allow-once` (`internal/openclaw/ws/methods.go:46-48`), delete
`deriveAllowAlwaysRule` + `allowlistAlways`, and stop storing grants in the
platform at all.

It is defensible -- losing a grant is fail-closed, so grants need none of the
durability desired state needs -- and it is the smallest platform diff. Rejected
because the runtime's entry shape is a poor fit for what we need to show and
manage:

- An `allow-always` entry is `argPattern = sha256:cwd-argv:v1:<64hex>`: opaque.
  The UI cannot render, edit, or explain it; the current readable
  "kubectl -- read-only operations" affordances are lost.
- The hash binds the **cwd**, so the same command in a different directory
  re-prompts. `docs/tools/exec-approvals.md:425` calls the decision "Always
  allow here" for this reason.
- Subsumption is impossible: the platform cannot tell whether a readable rule
  covers an opaque hash, so growth control reduces to "evict oldest by
  `lastUsedAt`".
- Effective policy stops being reconstructible from the API server, and an
  admin can no longer audit what a tenant's agent has been allowed to run.
- **Retracted:** "`AlwaysAsk` writes `nil`
  (`internal/server/approvals.go:506-509`), which would discard grants on a
  policy toggle." True as the code stands, but a fixable implementation choice
  rather than a property of the runtime, so it does not weigh against this
  option. See "Adjacent fix" below.

Also weighed and discarded: the argument that the platform's
`deriveAllowAlwaysRule` duplicates runtime semantics and will drift. Its rule is
an exact, `QuoteMeta`'d argv anchor, which is fail-closed on its own; the
runtime's extra protections (cwd binding, the one-shot downgrade for
inline-eval/heredoc/unplanned commands) add little at that shape. The drift
argument does not carry this decision.

### Keep grants in `AgentInstance.status`

Fewest new objects, but it needs the same RBAC addition as the ConfigMap (the
API Role has no `agentinstances/status`; `deploy/charts/cubepilot/templates/rbac.yaml:170`)
so it saves nothing there, and it hands the controller and the API server
concurrent whole-object status writes. Also: authorization is an irreplaceable
user decision, and `status` is conventionally reconstructible observed state
alongside `Phase`/`PodName`/`Conditions`.

### Keep grants in `spec.allowlist` with a `source` marker

No storage change at all. Rejected because it leaves the machine writing the
field that carries ownership semantics -- the change would have to redefine that
sentinel and special-case the machine's entries inside it, which is the bug
rather than the fix.

## Migration

None. Pre-release, no compatibility promised, and existing `spec` lists cannot
be split by provenance anyway (the entries carry no marker).

An earlier draft of this section claimed that "existing learned grants are
simply lost on upgrade". That is false, and the error is worth recording because
it points at a migration that does not exist. After this change `spec.allowlist`
is still unioned into the effective list, so a legacy entry keeps auto-passing;
it is merely no longer distinguishable from a hand-authored rule. The real
consequence is on the hardening path: a snapshot of a builtin that `Default()`
later removes keeps auto-passing until the user Resets, because a snapshot
cannot be told apart from a rule the user typed. That is the fail-open direction
this design exists to close, and it is bounded only by the Reset button.

A one-time cleanup of legacy entries cannot make that distinction either -- with
no marker in the data, "delete the machine-written ones" and "delete the rules
the user wrote by hand" are the same operation. Doing nothing is the honest
choice; a user who wants a clean list clicks Reset and re-adds what they meant.

## Testing

- `allowlist.Effective`: union, not ownership. A hardening change to `Default()`
  reaches an instance that has its own entries -- the regression test for the
  fork.
- `grants.Add`: idempotent on the same `pattern|argPattern`; cap eviction drops
  the oldest `createdAt`; a concurrent add does not lose a grant.
- API: an invalid `argPattern` is rejected with 400 rather than stored.
- `PUT /api/v1/agent/approval` with an empty allowlist leaves learned grants
  intact (today's Reset wipes them).
- Resolver: changing a grant changes `cfg.Revision`, so the policy is re-pushed.

## Adjacent fix: express `AlwaysAsk` with `ask: "always"`

Not required by this design. Recorded because it concerns the same function and
because the concern recorded in the code -- `applyPolicy` implements `AlwaysAsk`
by emptying the allowlist "which needs no unverified ask:always semantics"
(`internal/server/approvals.go:489-492`) -- can now be settled with evidence.

The runtime has a first-class **per-agent** `ask` field, and `ask: "always"` is
a true "ask about everything" mode:

- `requiresExecApproval` short-circuits at `exec-approvals-policy.ts:18` --
  `if (params.ask === "always") return true` -- **before** reading
  `allowlistSatisfied` (`:19-28`) and before the `durableApprovalSatisfied`
  escape (`:21`). Neither an allowlist hit nor a durable grant suppresses the
  prompt. `docs/tools/exec-approvals.md:206` states the same rule.
- `ask` is settable per agent through the same `exec.approvals.set` call the
  platform already issues (`exec-approvals-config.ts:373-374` persists
  `"always"`; `exec-approvals-resolver.ts:115-147` reads it per agent).

So `agent.Ask = "always"` yields the same strictness **without rewriting the
list**, which removes the destructive `nil` branch entirely and keeps the
runtime's allowlist stable across a policy toggle. One companion change is
needed: the `Allowlist` branch must set `ask` back to `on-miss`, since the
platform currently never writes `ask` at all and relies on the default.

Worth doing, but it is a behaviour change to an existing gate and belongs in its
own PR -- see "Out of scope".

## Out of scope

- The `ask: "always"` change above.
- Cap/TTL policy beyond a fixed `MaxGrants` -- the growth numbers above do not
  justify more.
- A `spec.allowlistDeny` overlay.
- Moving to a cached client.
- `source`-keyed preservation of runtime-minted grants, and the
  `ws.AllowlistEntry` field completion that goes with it.

## Open questions

Both questions this section originally carried were settled while writing the
implementation plan
(`docs/superpowers/plans/2026-09-14-allowlist-provenance.md`):

1. **Cap and eviction -- `MaxGrants = 1000`, oldest `createdAt` first.** LRU would
   need a `lastUsedAt` write on every use, and the gateway does not report
   allowlist hits, so it is not available at any reasonable cost; oldest-first is
   free. The number is sized against the ConfigMap ceiling, not against expected
   use, since expected use sits well below it.
2. **Per-entry revoke -- required, not optional.** Moving grants out of
   `spec.allowlist` removed the only path the Portal had to drop one:
   `persistConfirm` can only write the hand-authored list. The PUT body therefore
   gains an additive `revokeGrants` field. Without it the change would silently
   delete an existing capability.
