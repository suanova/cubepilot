# Workspace artifact ownership and convergence -- design

Date: 2026-09-24 · Status: approved for implementation · Scope: agent pod supervisor (skills, AGENTS.md / SOUL.md, openclaw.json)

## Context

The platform renders three things into each agent pod's workspace. All three land
on the writable per-instance PVC, all three are writable by the agent (the
supervisor runs the OpenClaw gateway as a child process under the same uid), and
none of them is guarded by an ownership rule or a check that reads the file back:

| Artifact | Source of truth | Pod copy | Guard today |
|---|---|---|---|
| `skills/<name>/` | Skill CR + repository tar, pulled by the supervisor | `$WORKSPACE/skills/<name>/` | a `.sha256` marker holding the source *revision*; the content on disk is never checked |
| `AGENTS.md` managed block | AgentTemplate instructions + per-user instructions | `$WORKSPACE/AGENTS.md` | reconciled every poll against disk |
| `AGENTS.md` outside the block / `SOUL.md` | the image's `workspace/` seed | PVC | none; the seed initContainer re-copies the pristine file on every pod start |
| `openclaw.json` | AgentTemplate providers, rendered by the operator | `$HOME/.openclaw/openclaw.json` | `applyGatewayConfig` compares the *fetched* bytes against its own last-written hash, never against the file |

Three consequences, each read out of the code:

1. **Skills never heal.** `syncSkill` skips the pull whenever the on-disk
   `.sha256` equals the desired revision. The marker sits in the same writable
   directory, so an edit to `SKILL.md` that leaves the marker alone is not
   corrected -- not even by a pod restart, because the marker survives on the PVC
   and short-circuits the sync again.
2. **`openclaw.json` never heals.** The comparison is against `lastCfgHash`, so a
   write to the file is invisible to the supervisor. OpenClaw's config reloader
   watches that file (chokidar), so the edit takes effect immediately and persists
   until the desired config changes or the pod restarts.
3. **The agent's own writes are discarded.** The seed initContainer re-copies the
   image's `AGENTS.md` / `SOUL.md` over the PVC on every pod start, discarding
   whatever the agent wrote there -- including `SOUL.md`, which OpenClaw documents
   as an agent-rewritable file and re-reads at the start of every run.

Net effect: the pod's copy is authoritative in fact while the platform treats it as
a cache, so the Portal shows one thing and the agent runs another for as long as the
pod lives.

### A conflict found while investigating

OpenClaw ships a `skill_workshop` tool. Our rendered `openclaw.json` has no `skills`
block, so its defaults apply -- `autonomous.mode: "auto"`, `approvalPolicy: "auto"` --
and the agent creates and patches skills in `workspace/skills/` on its own
initiative. Our `syncSkills` deletes every directory that is not in the resolved
set, so the platform silently deletes what the runtime just created.

The same tool carries the ownership rule this design reuses: an agent-originated
update to a skill directory the workshop did not create is refused at apply
(`Skill Workshop does not own this skill path`).

## The one rule

**A file or directory carrying our marker is the platform's, and converges to the
platform's desired state. Anything without the marker belongs to the agent, and the
platform never reads, rewrites or deletes it -- with two stated exceptions:
`openclaw.json`, which is platform-owned by definition (it is the runtime's own
config file, not workspace content, and needs no marker), and the name collision in
"Skills" below, where a platform skill name that returns to the resolved set wins
over an agent-authored directory holding it.**

Every decision below follows from this. It is also the rule OpenClaw itself applies
to skills (workshop-owned vs user-authored), which is why the two layers agree
instead of fighting.

## Design

### Skills

Replace the revision-only marker with one file that carries both facts:

```json
{"skill": "kubectl-platform", "revision": "<resolved revision>", "tree": "<sha256>"}
```

`.cubepilot.json`, inside the skill directory. `revision` records which platform
revision was installed, `tree` records the content fingerprint at install time, and
the file's presence is the ownership marker cleanup keys on. It is a record for
diagnosis and for cleanup, not a trust anchor -- the next paragraph says why.

`TreeHash(dir)`: sha256 over the sorted list of entries under the directory, each
entry rendered as `f\x00<relative path>\x00<sha256 of file content>` for a regular
file and `d\x00<relative path>` for a directory; the marker file itself is excluded,
and file modes are deliberately not included. Modes are excluded because they cannot
change what a text skill instructs and including them invites spurious
re-extraction churn; directories are included so an empty added directory is still
drift.

**The expected tree is derived from platform content, never from the marker.** The
marker lives in the same agent-writable directory as the skill, so an agent that
edits `SKILL.md` can recompute the tree and rewrite the marker while leaving
`revision` alone -- if the check trusted the marker, that forgery would be accepted
on every later poll and the drift would never be corrected. So the value the
convergence decision compares against is computed from the fetched tarball (the tree
of the staged directory, which is what the swap installs) and held in supervisor
memory, keyed by skill name. The marker keeps `revision` and `tree` as the
ownership marker and as a readable record for diagnosis; it is never the authority.

This moves the trust anchor to the platform repository. That is the intended
boundary -- content served by the platform is what the pod must match -- but it does
mean the repository's own write path has to be trustworthy; that is the subject of
the API-authentication issue filed alongside this one, and a repository an attacker
can write is a compromise of the skill content itself rather than of this check.

The expectation is keyed by the skill's **resolved identity** -- its `revision`, tar
`path` and `sha256` -- not by name alone. Per poll, for each wanted skill, in this
order:

- the resolved identity differs from the identity stored with the expectation, or no
  expectation is stored (every wanted skill after a pod start): fetch, extract, and
  replace the expectation with the staged tree;
- otherwise: compare `TreeHash(dir)` against the stored expectation, and re-extract
  when it differs.

The identity has to be part of the key, not just the name: keyed by name alone, a new
platform revision would leave the installed content matching the stale expectation,
so the poll would find "no difference" forever and the new content would never land --
the same class of bug this design exists to remove. When the repository is
unreachable, the last verified expectation stays in memory and the check continues
against it; only a pod start with the repository down leaves a wanted skill
unverified, and that resolves on the first poll that reaches the API.

Cleanup keeps the ownership rule: a directory under `skills/` is removed only when
the marker says the platform put it there **and** the name is no longer in the
resolved set. Anything unmarked -- an agent-authored skill -- is left alone, which
is the fix for the deletion conflict above.

Collision rule: **the platform name wins.** If an agent-authored directory holds a
name that later returns to the resolved set, the platform renders into that
directory and overwrites it.

### `openclaw.json`

`applyGatewayConfig` compares the fetched bytes against the bytes on disk instead of
against `lastCfgHash`. Same content stays a no-op, so the poll still never restarts
the gateway for an unchanged config; a changed file is rewritten, and the gateway's
own watcher reloads it.

### AGENTS.md and SOUL.md

The platform's ownership is expressed as one managed block, now carrying both
platform-authored parts:

- the persona / operating conventions (today `workspace/AGENTS.md`), and
- the resolved instructions (template + per-user), still under
  `## User-configured instructions`.

The block mechanics are unchanged: reconciled every poll against the bytes on disk,
idempotent, content-hash guarded, removed only when the desired content is empty
(which the persona makes unreachable in practice), and skipped with the last-good
file kept when validation fails.

Everything outside the block stays the agent's.

The seed initContainer changes from `cp -a` to `cp -a -n` (copy-if-missing), so a
pod start no longer overwrites `AGENTS.md` or `SOUL.md`. This mirrors OpenClaw's own
`writeFileIfMissing` semantics for workspace bootstrap files.

Two moves follow:

- **The persona text leaves the image's workspace directory.** It moves to
  `internal/supervisor/persona.md`, embedded into the supervisor binary
  (`//go:embed`). Keeping it in `workspace/AGENTS.md` while also rendering it into
  the managed block breaks the single-source rule: a fresh PVC would get the
  unmarked original from the seed and then a second copy inside the block. The
  repository's `workspace/` directory then holds `SOUL.md` alone, and the runtime
  image's comment about seeding a workspace persona is updated to match.
  `syncInstructions` is renamed to say what it now does (it writes the whole
  managed block, not instructions alone).
- **`workspace/SOUL.md` shrinks to tone.** Today it mixes two kinds of content: the
  identity paragraph ("you act as the user, the platform kubeconfig is for schema
  discovery only, state blast radius before writes") is operational and already
  restated in `AGENTS.md`'s operating principles, while the tone section is
  persona. The operational part moves into the persona file, the tone part stays.
  `SOUL.md` becomes the agent's own file -- seeded once, then its to rewrite, which
  is what OpenClaw documents it to be.

This also removes a small inconsistency: `SOUL.md` tells the agent that write
operations "are executed directly in phase one", while `AGENTS.md` requires stating
action and blast radius first. The merged text states the behaviour once, without
the phase label -- the agent can echo its own instructions into a chat answer, and
internal roadmap labels do not belong there.

### `skills.workshop`

Rendered explicitly rather than inherited from runtime defaults -- platform
behaviour should not be a function of an upstream default:

```json
"skills": {"workshop": {"autonomous": {"mode": "off"}, "approvalPolicy": "auto"}}
```

`mode: "off"` removes the automatic paths: autonomous capture is off, and `patch`
(foreground repair) is refused, so the runtime no longer lands a skill by itself.
`create` and `apply` are not gated by the mode, which is what keeps user-requested
authoring working -- and also means the mode alone does not *prevent* an agent from
deciding to create-and-apply on its own initiative. That case is covered by the
managed block's convention below, not by the config. `approvalPolicy` stays `auto`
because there is no proposal-review surface in this product: a `pending` policy would
make every apply wait for an approval nobody can give.

The managed block states the two conventions this depends on:

- Do not edit platform skills under `skills/`; to correct one, tell the user to
  republish it on the platform.
- To capture a new skill, create it and then apply it -- do not stop at the proposal.

## What is enforced where

Reviewers should not read this as one uniform mechanism; three layers do different
work, and only one of them is ours.

| Layer | Stops | Enforced by |
|---|---|---|
| Workshop | agent-originated `update` / `patch` to a platform skill dir, including `apply` | OpenClaw, inside the apply commit lock, independent of `autonomous.mode` |
| Workshop | an agent `create` that would land inside an existing platform skill dir | OpenClaw (`Skill already exists at ...`) |
| Supervisor | direct file writes by the agent (`exec` / `write`) and file add/remove inside a platform skill dir, including a forged marker | our `TreeHash` check against the tree computed from the fetched tarball, converged within one poll |
| Supervisor | the platform deleting an agent-authored skill | ownership-aware cleanup |
| Managed block | the agent rewriting its operating conventions | the existing per-poll reconcile |

Layer 3 is detect-and-converge, not prevention, and that is a deliberate choice
rather than a shortfall: the supervisor, the gateway and the agent share one uid in
one container, so a read-only mount or an immutable flag is not available to us
(`chattr` needs CAP_LINUX_IMMUTABLE, a bind mount needs mount privileges, both
dropped), and same-uid permissions can be undone by the other side. The guarantee we
can give is "a drift is corrected within one poll", not "a drift is impossible".

## Per-poll sequence for these artifacts (10s)

1. Reconcile the managed block in `AGENTS.md` against persona + resolved instructions.
2. For each wanted skill: compare the on-disk tree against the expected tree derived
   from platform content, fetching first when that expectation is not yet known;
   re-extract on any difference.
3. Remove marked skill directories that are no longer resolved.
4. Rewrite `openclaw.json` if the bytes on disk differ from the desired bytes.

## Error handling

- Resolved config or repository unreachable: log, retry next poll, leave existing
  content in place. A poll failure never clears what is already on the PVC.
- Extraction failure: the existing staged-swap keeps the installed skill; the
  revision is not recorded, so the next poll retries.
- Marker write failure: abort before the swap (today's ordering, kept).
- Drift detected: log it (skill name, and whether the content drifted or the platform
  published a new revision) and converge. No platform-side event is raised -- the
  Portal has no place to show one, and the persona constraint is what keeps the case
  rare.
- A wanted skill that has never been verified against platform content -- a pod start
  with the repository down -- is not counted as verified. Its on-disk content is left
  in place and it is verified on the first poll that reaches the API.

## Testing

Unit (supervisor / skill packages):

- `TreeHash`: nested directories, an empty directory, a deleted file, a changed
  file, and stability across repeated calls on an unchanged tree.
- Skills: an edited `SKILL.md` under an intact marker is restored on the next poll;
  a deleted marker is rewritten with the content restored; **an edited `SKILL.md`
  whose marker was rewritten in step (content plus recomputed `tree`, same
  `revision`) is still detected and restored** -- the marker is not trusted; an
  unmarked directory survives a revision change and a cleanup pass; a marked
  directory whose name left the resolved set is removed; a pod start re-fetches and
  re-verifies every wanted skill rather than skipping on a marker match; **a platform
  revision change re-fetches and installs the new content even though the on-disk tree
  matched the previous expectation** -- the expectation is keyed by the resolved
  identity, not by name.
- `openclaw.json`: a hand-edited file is rewritten with the desired content; an
  unchanged file is left untouched (no write).
- Managed block: persona + instructions are written together; a hand-edited block is
  restored; content outside the block is preserved byte for byte.
- Render: `skills.workshop.autonomous.mode == "off"` and `approvalPolicy == "auto"`
  are present, so an accidental omission fails a test rather than silently
  reverting to upstream defaults.
- Pod spec: the seed initContainer uses copy-if-missing.

e2e (extending the existing gateway/skill coverage): inside a running pod, edit a
platform skill file and `openclaw.json`, wait one poll interval, assert both are
back, and assert a hand-created skill directory is still there.

## Upstream dependencies to re-check on a base image bump

Four behaviours this design leans on live in the runtime image, which is pinned by
tag (`OPENCLAW_IMAGE_TAG=2026.8.2`). Read against that tag:

- apply-time ownership refusal for agent-originated updates (`src/skills/workshop/apply-transition.ts`).
- workshop tool gating by `autonomous.mode` (`src/agents/tools/skill-workshop-tool.ts`,
  `src/skills/workshop/config.ts`).
- bootstrap-file semantics for `AGENTS.md` / `SOUL.md` (`src/agents/workspace.ts`).
- the `skills.workshop` config schema (`src/config/types.skills.ts`).

A tag bump should re-read those four and confirm `off` still means "no autonomous
landing, explicit create+apply still works".

## Accepted costs

- Platform skills are not read-only on disk; drift is corrected, not prevented.
- A platform skill's directory may hold nothing of the agent's: a file the agent
  adds there is removed by the next re-extract. Skills the agent wants to keep go in
  their own directory.
- `SOUL.md` updates shipped in a new image do not reach an existing PVC (the file is
  the agent's now). Platform control over behaviour is unaffected -- the operating
  conventions travel in the converging managed block.
- The create-then-apply convention is model behaviour, not enforcement. If it proves
  unreliable, the options are to fall back to a platform-exclusive workspace (no
  agent-authored skills) or to build a proposal-review surface in the Portal.
- Narrow edge: if a platform skill leaves the resolved set and the agent creates a
  skill of the same name before that poll's cleanup runs, the marked directory is
  removed as a platform one. One poll wide; commented at the cleanup site.

## Out of scope

- Authentication on the API surface, including the repository write path (#239).
- Surfacing agent-authored skills in the Portal. Accepted for now: the user manages
  them by chatting with the agent, which is why the managed block states the two
  conventions above.
