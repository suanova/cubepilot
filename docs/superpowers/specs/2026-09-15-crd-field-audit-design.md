# The six CRDs: an audit of every field

Issue: [#194](https://github.com/suanova/cubepilot/issues/194)

## Context

The six `ai.cubestack.io` CRDs were written ahead of the implementation and have
accumulated fields that no code reads and no consumer sees. A field describing
behaviour the platform does not have is a lie in the schema, not a harmless
placeholder.

**Premise: v1 is unreleased.** There is no compatibility to keep and no data to
migrate -- a CRD change here is a delete-and-reinstall, and the objects are
recreated. Nothing in this design is constrained by what exists on a cluster
today, and nothing needs a migration path, a default, or a fallback branch. This
is stated up front because it was violated twice while writing the document: an
early draft argued a field should stay because `api.md` mentioned it, and a later
one worked out what happens to existing non-conforming `Task` objects when the
new validation lands. Both are recorded below rather than quietly deleted, so the
reasoning is not re-derived.

This is a full pass over every field of all six objects. 15 fields come out, 1
(`taskDTO.lastRunId`) goes in, 3 defects are fixed and several fields change
shape.

## The criterion

Each field is judged on two questions:

- **Needed** -- does dropping it take something away from a consumer?
- **Sound** -- even if useful, is its current form defensible? (k8s API
  conventions, derivability, duplication, safety.)

Plus one structural rule:

> **A CRD field must be generic. Adding an AgentTemplate or TaskTemplate must
> never require a CRD change.**

`summary{p0,p1,p2}` is the field that fails this: the grading vocabulary is
defined in exactly one place -- the builtin template's prompt string
(`internal/controller/builtin.go:145`, "classify by P0/P1/P2") -- and then
hard-coded in the CRD schema, the scheduler, the API DTO, the UI and the docs.

### What is not evidence of need

Two things were used as arguments during this review and both are wrong.

**Our own UI reading a field.** The bundled Portal is ours and changes freely.
It is the weakest evidence available, not the standard.

**`docs/cubepilot/api.md` mentioning a field.** That document is already known
to lag the implementation -- issue #178 exists to rewrite it for exactly this
reason. Pre-release there is no promise to break: dropping a field costs a doc
edit and a Bruno fixture update, nothing more. (This entry exists because the
first draft of this design used "it is in the contract" to argue a field
survived, which re-invented a compatibility constraint the project does not
have.)

### What the API surface actually is

Worth stating precisely, because it is asymmetric and it decides which fields a
client can even see:

| Endpoint | Shape | Source |
| --- | --- | --- |
| `/api/v1/agenttemplates` | raw `AgentTemplate` | `internal/server/handlers_platform.go:47` |
| `/api/v1/tasktemplates` | raw `TaskTemplate` | `internal/server/handlers_platform.go:68` |
| `/api/v1/instances` | raw `AgentInstance` | `internal/server/handlers_platform.go:127` |
| `/api/v1/skills` | raw `Skill` | `internal/server/handlers_platform.go:221` |
| `/api/v1/taskruns`, `/api/v1/taskruns/{name}` | raw `TaskRun` | `internal/server/handlers_platform.go:259`, `:286` |
| `/api/v1/tasks` | flat `taskDTO` | `internal/server/handlers_tasks.go:161`, `:411` |
| `/api/v1/tasks/{id}/reports` | flat `reportDTO` | `internal/server/handlers_tasks.go:441` |
| `/api/v1/agent/status` | curated map | `internal/server/handlers_agent.go:171-178` |

Where an endpoint returns the CR itself, every field of that object is API
surface. Where it returns a DTO, only the mapped fields are. So a Task's
`status.phase` is not reachable over HTTP at all, while a TaskRun's
`status.summary` is -- and `Task.status.lastStatus` is, twice over (DTO and
`api.md:651`).

That asymmetry is not a reason to keep fields; it is a reason to check each
field against the right surface before calling it unused.

## Drop

### AgentTemplate -- 5 fields, none with a behaviour branch

| Field | Why it goes |
| --- | --- |
| `spec.memory` (+`MemorySpec`) | Written once (`builtin.go:111`), read by nothing. Memory is carried by the resident data PVC; the toggle gates no behaviour. `enabled` is also `bool`, against the repo's k8s API conventions. |
| `spec.identity` (+`AgentIdentitySpec`) | Written once (`builtin.go:112-113`), read by nothing. `scope` has zero references anywhere in the repo. |
| `spec.registry` (+`AgentRegistrySpec`) | Written once (`builtin.go:116`), read by nothing in Go -- its only consumer is the `Builtin` printcolumn. Two subfields, both listed below. |
| `spec.quotas` (+`QuotaSpec`) | Written once (`builtin.go:120`) with a constant `1`, read by nothing. The instance name is `owner+template` (`k8s.InstanceName`), so the cap is structurally 1 and can never fire. |
| `spec.policyRefs` | Zero references. The comment says it is for a phase-2 policy engine that does not exist. |

`spec.registry` deserves its own note because it is not merely unused -- it is
invented. The design doc's `AgentTemplate` example (design §3.1) lists
`runtime / displayName / defaultModel / providers / instructions / skills /
approvalPolicy / allowlist`; there is no `registry`. The type comment cites
"design §4.6", and **design §4.6 does not exist** -- §4 has no numbered
subsections at all. Its two subfields both fail on their own:

- `builtin` -- the comment claims builtin templates "cannot be deleted". There
  is no admission webhook in the repo, so nothing enforces that; and the fact
  itself already has a mechanism, the `cubepilot/builtin: "true"` label, used by
  Skills (`handlers_platform.go:621`), the builtin AgentInstance
  (`builtin.go:241`) and the per-user ServiceAccounts. `spec.registry.builtin`
  is a second mechanism for a fact the label owns.
- `visibility` (`system | platform-reviewed | public`) -- zero readers, not in
  the design, and a free string with no enum.

The `Builtin` printcolumn goes with the field, since it reads it. Builtin
templates remain identifiable by label.

Also dropped: the `IdentityMode` type and its constants, which exist only for
the two `identity` fields.

`AgentTemplate.status` **stays**, even though `observedGeneration` has no
writer -- see "Keep".

### AgentInstance -- 5 fields

| Field | Why it goes |
| --- | --- |
| `spec.identity` (+`IdentitySpec`, +`PrincipalRef`) | See below. Also removed from the schema's `required` list. |
| `spec.credentials` (+`CredentialSpec`) | Reads the wrong thing entirely. The real credential path is the template's providers: `resolver.go:171-181` builds `ResolvedAgentConfig.Credentials` from `AgentTemplate.spec.providers[].credentialRef`. Nothing reads `inst.Spec.Credentials`. The only thing that mentions the field is its own comment, citing a design section that does not exist (see "Dangling design references"). |
| `spec.lifecycle` (+`LifecycleSpec`) | Residue of the idle-reclaim surface removed in #134. `strategy` is a free string set to `"resident"` by both writers (`builtin.go:252`, `handlers_platform.go:185`) and read by nothing. |
| `spec.dataVolume.pvc` | See "Fix" -- it is a safety defect, not just dead weight. |
| `status.lastActivity` | Written but never read. Its name says "last activity"; the value is the last *status change*, set inside the same `if` that guards the status write (`agentinstance_controller.go:239-246`, `:348`), so an agent that is actively chatting but whose Pod state is stable does not refresh it. The comment (`agentinstance_types.go:140`) describes an `agentKey -> activity` map, which the type has not been for a long time. Its originating feature -- idle reclaim -- is gone with #134. |

Dead helpers removed alongside: `AgentInstance.CredentialFor`
(`agentinstance_types.go:213`), `ReadyCondition` (`:203`), `PodResources`
(`:223`, which returns an empty struct unconditionally). `EffectiveDataVolume`
stays -- it has a live caller.

#### `spec.identity` in detail

It is the field that took the most discussion, so the reasoning is recorded in
full.

**It is a designed field**, unlike `spec.registry`: design §3.2's `AgentInstance`
example carries `identity: { mode: user, principalRef: { userRef: zhang.wei } }`,
and design §6 states its semantics -- "AgentInstance 的 `owner` 与
`identity.userRef` **必须一致**" -- with §8.2 listing 服务身份 as the phase-2
extension that "扩展 identity 与凭据派生".

That statement of the invariant is itself the argument against the field in
phase one: two fields that must always be equal mean one of them carries no
information.

What the code does:

- **Writers**: `builtin.go:246-250` and `handlers_platform.go:179-183`, both
  hard-coding `mode: user` and `userRef: <owner>`. A client cannot set it
  through the API at all.
- **Readers**: none in production code. Three tests read it -- `builtin_test.go:66`,
  `builtin_test.go:190` and `e2e/bootstrap_test.go:94` -- and every one of them
  asserts *that it was written*, not that it drives anything. That is the
  signature of a write-only field: tests locking in the write.
- **Behaviour branches**: none. There is no `if mode == ...` anywhere.
  `IdentityModeService` is referenced only by its own definition.
- **The invariant is unenforced.** Nothing validates `userRef == owner`. A
  hand-written CR with `owner: a` and `userRef: b` is accepted, and the platform
  silently executes as `a`.
- **It is `required`** (`agentinstances.yaml`), so every client is forced to
  send a value nothing reads.

And the credential chain -- the one thing the design ties `identity` to -- runs
entirely off `spec.owner`:

- `agentinstance_controller.go:133` -> `k8s.UserKubeconfigSecretFor(inst.Spec.Owner)`
- `agentinstance_controller.go:122` -> `AgentUser: inst.Spec.Owner`
- `k8s/userkubeconfig.go:26` -> Secret named `user-<sanitize(user)>-<hash(user)>`

plus the per-user ServiceAccount `user-<sanitize>` bound to `view` and
`cubepilot-user-crds` created in `builtin.go`. `spec.identity` contributes
nothing to any of it.

**Decision: drop it.** Phase one has exactly one identity mode and the principal
*is* the owner. The phase-2 service identity can reintroduce the abstraction when
there is a second mode for it to express.

### Task -- 1 field and 1 dangling column

| Item | Why it goes |
| --- | --- |
| `status.phase` (`TaskPhase`, `Ready`/`Paused`) | Duplicates `spec.state` (`Enabled`/`Paused`), in a second vocabulary. Nothing consumes it: `taskToDTO` maps `State` from `t.Spec.State` (`handlers_tasks.go:65`) and never touches `Status.Phase`; the UI reads `t.state` (`TasksView.tsx:90`). Its only real function today is as the scheduler's patch-dedup sentinel, which "Fix" replaces with a better-aimed one. |
| printcolumn `Trigger` | Reads `.spec.trigger`, which does not exist -- the cron field's own comment explains the trigger field was deliberately removed as derivable. The column prints empty for every Task. |

Note that design §3.5's `Task` example has **no `status` block at all**, so the
shape of `TaskStatus` was never specified; `lastRunTime` and `nextRunTime` stay
because they have real readers, and `lastStatus`/`lastTaskRunName` are kept but
reshaped -- both in "Fix".

### TaskRun -- 3 fields

| Item | Why it goes |
| --- | --- |
| `status.summary` (`TaskRunSummary`, entirely) | See below. |
| `status.items` (+`FindingItem`, +`EvidenceEntry`) | See below. |
| `status.conditions` | No writer anywhere in the repo, so it is permanently an empty array. A field that can never carry information is noise: a client asking "why is this run like this?" sees an empty `conditions` and concludes the platform forgot to set it. Design §3.5's `TaskRun` example does not list it. |

#### `status.summary` in detail

`p0/p1/p2` are not a design decision so much as a guess the scheduler makes about
prose. The agent is told (in the builtin template's prompt) to "classify by
P0/P1/P2"; it returns natural language; the platform counts:

```go
summary.p0       = strings.Count(content, "P0")          // scheduler.go:356
summary.total    = lines starting "- P0"/"- P1"/"- P2"   // scheduler.go:360
                   or "### P0"/"### P1"/"### P2"
summary.abnormal = P0 + P1 + P2                          // scheduler.go:211
```

Three separate problems:

1. **The numbers are wrong, not merely coarse.** A report containing the sentence
   "no P0 issues were found" yields `p0: 1`, because the count is a substring
   count. Presenting a wrong number is worse than presenting none.
2. **They contradict each other.** `p0/p1/p2` count substrings; `total` counts a
   line shape. A report that mentions `P0` only in prose gives `total: 0` with
   `p0: 1` -- `total < p0+p1+p2`, which cannot be a summary of anything.
   `abnormal` is a literal `P0+P1+P2`, so it adds nothing to the three it sums.
3. **The vocabulary is template-private and welded into the schema.** See the
   criterion above. A second TaskTemplate that grades `Blocker/Critical/Minor`,
   or a deployment task reporting `succeeded/failed`, or a cost report with
   currency, gets either garbage or a CRD change.

The consumption is also thinner than it looks: the counts reach the UI in exactly
one place -- the severity chips in the report detail
(`web/src/views/TasksView.tsx:438-443`). The report list is a `<select>` of time
and status (`:406-411`) and the export writes name/time/status/content (`:286`),
neither of which uses them.

**Decision: drop `summary` entirely.** `TaskRun` reports the *execution* --
`phase`, the timestamps, the resolved revisions, `content`, `error` -- and does
not interpret the content. This is consistent with the earlier decision not to
restructure the runner's output contract for one report's sake: if the counts
cannot be computed honestly from prose, the platform should not publish them.

The consequence is recorded plainly: a P0/P1/P2-graded report (issue #27's
deliverable) stays exactly that -- a report graded in its text -- but the
platform stops surfacing a machine-readable severity. Recovering it requires the
structured-report work below, which is deliberately not in this change.

Also dropped with the field: the three `P0`/`P1`/`P2` printcolumns
(`taskrun_types.go:116-118`), `countSeverity` and `countSeverityTotal`, the three
`reportDTO` fields and their mapping (`handlers_tasks.go:53-55`, `:128-132`), the
three `Report` fields in the web types, and the doc/fixture mentions
(`api.md`, `bruno/cubepilot-api/4-tasks/04-task-reports.bru`).

`countSeverity`'s comment claims it is "shared with the server's report" -- the
server has no counting function, only field mappings. The comment goes with it.

#### `status.items` in detail

It is the structured form of an inspection report -- one entry per finding with a
category, a level, the finding text and an evidence chain
(`taskrun_types.go:32-49`). Its comment cites design §3.3.4 for that shape, and
that section does not exist (see "Dangling design references"); the real support
is design §7, which lists evidence references among TaskRun's minimum record
("证据引用").

It has never been written. A repo-wide search for `Status.Items`, `FindingItem`
and `EvidenceEntry` across `.go`, `.ts`, `.tsx` and `.md` -- tests included --
returns only the type definition itself. The evidence chain exists solely as
marked-up prose inside `status.content`, which is what the builtin template's
prompt actually asks the agent for.

Wiring it is not a field change: it needs an output contract for the agent, a
`runner` that returns more than a `content string`, and UI to render it. It also
cannot cover free-form tasks (`Task.spec.instruction` is arbitrary user text), so
"structured" would be a property of template-bound tasks only.

**Decision: drop it**, and record the deviation from design §7. When structured
reports are wanted, the contract belongs in the template's `instruction` (the
domain vocabulary is template data) with, if a machine consumer ever appears, one
generic opaque field to carry the payload -- not a pre-carved schema for a shape
nothing produces.

### TaskTemplate -- 1 field

`paramsSchema[].type`: `resolveTemplateParams` (`handlers_tasks.go:263-283`)
reads only `name`, `default` and `enum`. And `Task.spec.params` is
`map[string]string`, so an `int` type is not representable even in principle.

The builtin template's `ParamSchema` value is a free string with no enum, which
is also against convention, but the field goes rather than being constrained.

## Fix

### Two printcolumns that read fields that do not exist

`TaskRun`'s `Type` column reads `.spec.type` (`taskrun_types.go:113`), which
never existed -- the field is `trigger`. Every TaskRun prints an empty Type.
Repointed at `.spec.trigger`.

`Task`'s `Trigger` column reads `.spec.trigger` (`task_types.go:83`), removed
deliberately. Deleted, not repointed: `kubectl get tasks` already shows `State`,
and the schedule is visible as `spec.cron`.

### `spec.dataVolume.pvc` is a deletion primitive

`AgentInstance.EffectiveDataVolume` (`agentinstance_types.go:188-200`) returns
the name from `spec.dataVolume.pvc` when set, and the instance finalizer deletes
whatever name it returns (`agentinstance_controller.go:322-326`). So a writer of
that field chooses which PVC the platform deletes on instance teardown -- any PVC
in the namespace, including one belonging to something else.

No product code writes it: neither the builtin bootstrap nor `POST
/api/v1/instances` sets `dataVolume`, so `EffectiveDataVolume` always takes the
generated `data-<instance>` branch in practice. The field's only live effect is
the unsafe one.

**Decision: drop the field**; the finalizer deletes only the platform-generated
name, and `spec.dataVolume.size` stays (it is read at
`agentinstance_controller.go:152`). This deviates from design §3.2's example,
which carries `pvc:` -- recorded below. The example's value
(`pvc-zhang-wei-cubepilot`) is the platform-generated name anyway, so the intent
"a per-instance PVC named after the instance" is preserved; only the
user-selectable override goes.

### `Task.status.lastStatus` becomes an enum

It is a free string today (`"success"` / `"failed"`, set at `scheduler.go:231-233`)
and the repo's convention is a typed string enum with a
`+kubebuilder:validation:Enum` marker, as every other enum in these CRDs has.
The field itself is kept -- see "Keep".

### `Task.status.lastTaskRunName` gets exposed

It is written by the scheduler (`scheduler.go:230`, `:301`) and read by nothing:
not in `taskDTO`, not in `api.md`, not in the UI. "The latest run" is available
only by listing TaskRuns and sorting, which is what the UI does.

Two honest options were available: drop it as write-only, or expose it. It is
kept and exposed, because it carries real information ("jump to the latest
report" is the natural companion to `lastStatus`) and the scheduler already
maintains it at no cost. Dropping it while keeping `lastStatus` would leave the
list view able to say "the last run failed" with no way to reach it.

So: a `lastRunId` field on `taskDTO`, the corresponding entry in `api.md`, and a
link from the task list to the latest report.

### Two CEL rules on `TaskSpec`

Both mirror rules the API handler already enforces in `handlers_tasks.go:182`
and `:213-216`, moved into the schema so a hand-written CR is held to the same
contract.

**Presence is not enough, and the first draft of this rule got that wrong.**
`has(self.instruction)` is true for `instruction: ""`: `has()` tests whether the
key exists in the serialized object, and an explicitly empty string is a present
key. So a hand-written `instruction: ""` would pass a rule the handler rejects --
and a `params` map with `templateRef: ""` would pass the second rule too.

The mirror is worse than "non-empty" as well: the handler trims before comparing
(`handlers_tasks.go:175-176`, `:182`), so a whitespace-only instruction is a 400
there and must fail validation here.

```
(has(self.templateRef) && self.templateRef != "")
  || (has(self.instruction) && !self.instruction.matches('^\s*$'))
```

`!matches('^\s*$')` also rejects the empty string, since the pattern matches zero
whitespace characters; the `!= ""` beside it is for symmetry, not necessity.

The blank test uses `matches` rather than `trim()`. `trim()` belongs to the CEL
`ext.Strings` library -- the same library the `lowerAscii()` call in
`AgentTemplateSpec`'s rule comes from, so it is very likely available -- but
`github.com/google/cel-go` is not in this module's build graph, there is no local
copy of it and there is no network here, so that could not be confirmed. `matches`
is core CEL, and its escaping convention is already proven in this repo by the
`matches('.*\s.*')` rule on `Providers`: the Go marker carries `\\\\s`, which
controller-gen unquotes to `\\s` in the generated YAML, which CEL reads as the
regex escape `\s`. The new rule copies that convention.

The rule is **at least one**, not exactly one: a template-bound Task legitimately
carries both, because the stored `instruction` is the rendered snapshot
(`handlers_tasks.go:207`), kept for display and as the fallback when the template
is deleted, while the scheduler re-renders from the template at fire time
(`scheduler.go:164-171`).

The second rule, with the same value test:

```
!has(self.params) || (has(self.templateRef) && self.templateRef != "")
```

Both are written with `has()` guards: these fields are `omitempty`, and CEL errors
on a missing key rather than treating it as empty.

### Stale comments

- `TaskManualRunAnnotation` (`task_types.go:120-122`) still says the scheduler
  fires the task once "(trigger=manual)" -- `trigger` is no longer a field of
  `Task`.
- `resolver.go:185-188` describes instruction composition as three layers
  ("platform safety & execution constraints + template instructions + user
  instructions") while the code composes two: `cfg.Instructions =
  def.Spec.Instructions` then the user's appended (`:164`, `:189-193`). The
  supervisor's own comment says two
  ("template instructions + per-user UserInstructions, merged by the resolver",
  `supervisor.go:468-470`). The comment is corrected to two -- see "Out of
  scope" for why the missing layer is not being built here.
- The §4.4 and §4.6 design references die with the fields that cite them.

### Dangling design references

Found while verifying the citations in this document, and worth recording because
it is bigger than the two references the audit set out to remove.

The design doc's only numbered subsections are §1.1-1.3, §2.1-2.2, §3.1-3.6,
§5.1-5.3 and §8.1-8.4. **§4 has no subsections at all**, and there is no §10 or
§11.

Code comments cite, by occurrence count:

| Cited | Occurrences | Exists? |
| --- | --- | --- |
| §3.3.1, §3.3.2, §3.3.3, §3.3.4 | 7, 10, 5, 15 | no -- §3.3 has no subsections |
| §4.1, §4.2.1, §4.4, §4.5, §4.6 | 1, 1, 3, 1, 1 | no -- §4 has no subsections |
| §5.4 | 1 | no -- §5 stops at §5.3 |
| §10, §11.1 | 2 | no -- §9 is the last section |

About 47 occurrences in total. The `§3.3.x` family is the interesting one: it is
not merely missing but points at the **wrong subject**. §3.3 is now the model
provider (inline providers, no Model CRD); the TaskTemplate/Task/TaskRun domain
those comments describe lives in §3.5. So an earlier design revision numbered the
task domain §3.3.x, the doc was rewritten, and the references were never swept.
The bare `design §3.3` references (5) resolve, but are liable to mean the old
subject too.

Not fixed here: it is a mechanical sweep across many files unrelated to this
change, and mixing it in would bury the field audit in comment edits. Recorded so
it is not rediscovered as a new finding. The two references attached to fields
this change removes (§4.6 on `registry`, §4.4 on `credentials`) disappear with
them.

## Keep

| Field | Why |
| --- | --- |
| Every `Skill` field, including `source.type`/`source.s3` and the `Tenant`/`User` visibility values | These are seams design §3.4 plans explicitly, and the design states the switch procedure ("切对象存储时改 `source.type` 为 `S3` 并填 `source.s3`，其余不变"). They are not implementation inventions like `spec.registry`. Deleting them would mean redrawing the `source` shape in phase 2. The Go guards that reject the phase-2 values (`catalog.go:166-171`, `:173-184`) go with them, unchanged. |
| `status.observedGeneration` (AgentTemplate, AgentInstance, Skill) | k8s convention. Unread by the platform, but conventional and not misleading -- unlike `lastActivity`, whose name is wrong. AgentTemplate's has no writer because there is no template controller; the subresource stays for the controller that will exist. |
| `TaskRun.spec.owner` | Not in design §3.5's example, but read for authorization (`handlers_platform.go:248`, `:282`). Recorded as an implementation addition. |
| `Task.status.lastRunTime`, `nextRunTime` | `lastRunTime` is the cron base for the next fire time (`scheduler.go:106-107`) and is surfaced as `lastRunAt`; `nextRunTime` is the scheduler's patch-dedup guard (`:130-137`) and is recomputed by the API for display. |
| `TaskRunCancelled` | Recorded for completeness rather than changed: the enum value exists, the API maps it (`handlers_tasks.go:106`), and nothing ever sets it -- cancelling a run is not implemented, and design §3.5 does not list it. Kept rather than removed because a run-cancel path is plausible (the repo already has chat-turn stop/redirect), but it is a read-only branch today. |
| `AgentTemplate.status` | Has only `observedGeneration`, which no controller writes -- there is no template controller. The subresource stays for the controller that will exist, rather than being removed and re-added. |
| `Task.templateRef` / `instruction` / `params` / `owner` / `cron` / `state`, `TaskRun.spec.creatorTaskRef` / `trigger` | All in design §3.5 and all with live readers. |

## Recorded deviations from the design doc

The design doc is the source of truth and is not being edited to match. These
five deviations are deliberate:

1. **`AgentInstance.spec.dataVolume.pvc`** -- the design §3.2 example carries the
   field. Dropped for the deletion-primitive reason above.
2. **TaskRun evidence references** -- design §7 lists "证据引用" in TaskRun's
   minimum record. `status.items` is dropped, so the evidence chain lives only in
   `content`.
3. **`TaskRun.spec.owner`** -- not in the design; kept because it has a real
   reader.
4. **`AgentInstance.spec.identity`** -- the design §3.2 example carries it and §6
   states its invariant (owner and identity.userRef must be equal). Dropped: in
   phase one it is a second name for `owner`, the invariant was enforced nowhere,
   and the credential chain -- the user kubeconfig Secret, the per-user
   ServiceAccount -- runs entirely off `spec.owner`.
5. **`TaskRun.status.summary`** -- the design §3.5 example shows
   `summary: { p0: 0, p1: 1, p2: 3 }`. Dropped: p0/p1/p2 were substring counts
   over prose, so a report saying "no P0 issues were found" produced `p0: 1`, and
   `total` counted a different shape again so it could contradict them. The
   report keeps its P0/P1/P2 grading in its text.

## Rejected alternatives

### Keep `summary` as `map[string]int32` with a template-declared vocabulary

`TaskTemplate.spec.grading: [P0, P1, P2]`, and the scheduler counts whatever the
template declares. This does satisfy the generic-field rule -- adding a template
would not change the CRD -- and it would preserve the report detail's severity
chips.

Rejected on two counts. The counts would still be a regex over prose, so problem
(1) above is unfixed: the field would be generic and still wrong. And there is no
second template; this designs for an imagined one, against the repo's
"用到再加" stance. When structured reports are actually wanted, `items`-style
output is the honest route, and a generic opaque field can carry it then.

### Keep `Task.status.phase`

Defensible reading: a scheduler *observing* "this task is ready/paused" is
observed state, and k8s status is for observed state.

Rejected because it carries nothing `spec.state` does not: the two differ only in
the reconciliation window between a spec write and the scheduler's next pass, and
that window is invisible to every consumer. The parallel is deliberate -- k8s's
own CronJob has no phase either, and "suspended" lives in `spec.suspend` for the
same reason. Keeping it would also mean maintaining two vocabularies for one
fact, which is the objection the web type already records for `enabled` vs
`state` (`web/src/api/types.ts:31-33`).

### Keep `spec.identity` as the source of truth

Instead of deleting it: make `identity.principalRef.userRef` authoritative for
the credential chain, demote `owner` to a bookkeeping field, and enforce
`userRef == owner` with CEL.

Rejected as premature. It gives the phase-2 abstraction a real job in phase one,
at the cost of rewriting the credential chain and the six authorization
call sites that read `spec.owner`, for a second identity mode that does not
exist. The same is achieved by reintroducing the field when the mode arrives.

### Keep `summary.total`

Its first defence is sound: `total` is *not* derivable from `p0/p1/p2` -- it
counts a different thing by a different method. `abnormal` is a tautology, but
`total` genuinely answers "how much was inspected?"

Rejected on the counting, not on the concept: the number it produces can
contradict the components beside it (problem (2) above), and nothing consumes it.
A field that can be self-contradictory is worse than an absent one. If the
inspection volume ever needs reporting, it should come from a report format that
makes it derivable.

### Wire `status.items` now

Rejected -- it changes the agent's output contract, the runner's return shape and
the UI, to make one template's report structured. Design §3.3.4 wants it; design
§3.5's example does not show it; the cost is a feature, not a field. See "Drop".

## Migration

None. The CRDs are deleted and reinstalled and the objects recreated -- there is
no data to preserve and no compatibility to keep.

An earlier draft of this section argued at length about what happens to an
existing non-conforming `Task` when the new CEL rules land: which writes trigger
validation, whether the scheduler's next status write would be rejected, whether
CRD validation ratcheting applies. All of that is an artifact of treating
existing objects as a constraint, and they are not one. That draft is recorded
here only so the reasoning is not re-derived.

Note the CEL rules themselves are not a compatibility question: they are
compiled by the API server at CRD install time, so `make test` cannot see them
and the kind e2e job -- which installs the CRDs and then creates Tasks through
them -- is what verifies them.

## Testing

- **Deletion is compile-checked** for every dropped Go field: the build fails at
  each writer (`builtin.go`, `handlers_platform.go`) and the tests that assert on
  them (`builtin_test.go:66`, `:190`, `e2e/bootstrap_test.go:94`) are removed
  with them.
- **The `dataVolume` fix**: a finalizer test asserting the deleted PVC is
  `data-<instance>` even when `spec.dataVolume` is absent, plus a test that the
  field no longer exists on the schema.
- **The `lastActivity` removal** requires rewriting the no-write-amplification
  assertion it currently anchors (`agentinstance_controller_test.go:167-181`),
  which uses `LastActivity` as the proof that a no-change reconcile did not write
  status. The replacement asserts `resourceVersion` is unchanged -- verified to
  be a real assertion, since the controller-runtime fake client bumps
  `metadata.resourceVersion` on a status write.
- **The scheduler's pause change**: a paused task writes `nextRunTime = nil`
  once, and a second reconcile with the same state issues no status write.
- **The Task CEL rules**, against a cluster with the CRD installed (the kind e2e
  job -- `make test` cannot compile CEL). Rejected: neither field present;
  `instruction: ""`; `instruction: "   "` (spaces); `instruction: "\n\t"`
  (whitespace that is not a space); `templateRef: ""`. Accepted: a real
  `instruction`; a real `templateRef`; both together (the snapshot case); a real
  `templateRef` with `params`. And separately: `params` with no `templateRef` is
  rejected, with one is accepted.
- **CRD regeneration**: `make manifests` leaves `config/crd/bases` and
  `deploy/charts/cubepilot/crds` byte-identical to each other and matching the
  markers.
- **API/doc sync**: `api.md`'s `taskDTO`/`reportDTO` field lists and the Bruno
  task-reports fixture match the new DTOs.

## Out of scope

- **A platform-level output-contract instruction layer.** Whether the platform
  should emit design §3.2's "platform safety & execution constraints" as prompt
  text is deferred. This change only corrects the comment claiming it exists.
  Worth noting while deferring: the safety boundary is already enforced by
  mechanism -- per-user read-only RBAC, the allowlist, the HITL gate -- rather
  than by prompt text, so the missing layer is not obviously a gap. If a
  platform-level *output* contract is ever wanted (for structured reports), that
  is a separate decision about a layer named for what it does, not a "safety"
  layer retrofitted.
- **Curated DTOs for `/api/v1/taskruns` and `/api/v1/instances`.** They return raw
  CRs today, which is the root cause of "every CRD field is API surface".
  Switching them to DTOs would stop future field deletions from being
  API-visible changes. That is an API redesign, not a field audit.
- **Structured reports** (`status.items`, an agent output contract).
- **The P0/P1/P2 counting inconsistency** as a report-format question: `total`
  and `p0/p1/p2` disagree by construction. Both go away here, so there is nothing
  left to fix; it returns only with structured reports.
- **The missing `+optional` markers.** About 29 fields carry `omitempty` without
  the `+optional` marker -- `AgentTemplate.spec.displayName`, most of
  `TaskTemplateSpec`, `SkillSource.path`, `TaskRunStatus.phase` and others. It is
  cosmetic: `omitempty` alone already makes controller-gen treat a field as
  optional, and the generated schema is byte-identical either way (confirmed
  against the checked-in CRDs, where none of these appear in a `required` list).
  The marker is documentation, and a sweep would touch a field set unrelated to
  this audit. `TaskRunStatus.phase` was identified during review as one instance;
  it stays as-is with the rest.

## Open questions

None outstanding. The two that arose during review were settled:

1. **Does `Task.status.lastTaskRunName` get exposed or dropped?** Exposed -- see
   "Fix". "Keep it but do not surface it" was considered and rejected as
   incoherent: a write-only field nothing reads is exactly what this audit
   removes.
2. **Is `Skill`'s `source.type`/`s3`/`visibility` the same kind of placeholder as
   `spec.registry`?** No -- the design states the switch procedure for them, which
   is what distinguishes a planned seam from an invented field.
