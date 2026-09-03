# Design: builtin `cubestack-platform` skill — CubeStack usage map for agents

Date: 2026-09-03
Issue: #98
Status: draft (pending review)
Goal owner: zhujian

## 1. Context & goal

Issue #86 removed the per-CRD `dev-environment` / `inference-service` skills and
made the chat agent drive any `ai.cubestack.io` CRD through **generic schema
discovery** (`kubectl-platform`: `api-resources` → `explain` /
`--dry-run=server` → apply). The builtin set is now exactly `cluster-inspection`
+ `kubectl-platform`.

Observed failure (chat e2e, real-LLM run): when a user asks in natural language
for a dev machine (DevEnvironment), the agent iterates on server-side dry-run
errors many times — guessing the CRD shape field-by-field (e.g. `resources` is a
flat `cpu`/`memory` string pair, not a k8s `requests`/`limits` map; `gpuCount`
is an `int32` with `minimum:1`; `type`/`running`/`storage` all carry defaults
the agent does not know) before landing a valid manifest. Each guess is a fresh
LLM round-trip; CubeStack is still under active development, so the schema is
large and its conventions are not intuitive.

Goal: add a third builtin skill, `cubestack-platform`, that gives the agent a
**current, correct map of the CubeStack platform** — how each
`ai.cubestack.io` CR is meant to be created (required fields, defaults, enums,
conventions) and how to read a running resource back (status / endpoints /
connect) — so the chat flow stops burning round-trips on schema guessing.

### Decisions (brainstorming, 2026-09-03)

| Decision | Choice | Why |
|---|---|---|
| Where the knowledge lives | **Inside cubepilot**, schema input = the already-vendored CRD YAMLs under `test/e2e/framework/testdata/cubestack-crds/` (refreshed from upstream by the existing `make update-crds`) | Single in-repo source; the skill and the CRDs the e2e installs can never drift from each other — they are generated from the same files |
| Skill count / shape | **One** domain skill (not per-CRD skills) covering the whole `ai.cubestack.io` map | Stays inside the §3.4 "domain skill" model; does not resurrect the #86 anti-pattern of a skill per CRD |
| Content scope | Full platform CRD map **+** broader platform usage guide | Agent needs both "how to build each CR correctly" and "what the running platform looks like / how to connect" |
| Development-status ledger | **Excluded** | Nothing like "this field is not implemented yet" — content describes the intended API surface and runtime behavior only |
| Freshness (schema) | **Generator + guard test**, driven from the vendored CRDs | No rote manual upkeep; deterministic regeneration; CI fails when stale |
| Freshness (narrative) | A **repo-level Claude Code skill** (`.claude/skills/`) encodes the refresh runbook so an agent, not a human, performs updates | Behavior semantics have no machine source; the *process* is what is de-rote'd |

## 2. Skill content: `internal/skill/skills/cubestack-platform/`

Two files, cleanly split into generated vs hand-authored (a generated region
inside SKILL.md would need HTML-comment markers, which markdown renderers
swallow along with their content — a separate file avoids that and is the
standard "heavy reference" shape for a skill):

### 2.1 Generated file: `crd-reference.md` — "CubeStack CRD quick reference"

Produced **wholesale** by the generator from the vendored CRD YAMLs (§3); never
hand-edited. For each kind currently shipped (`DevEnvironment`,
`InferenceService`, `InferenceRuntimeProfile`, `ModelVersion`) it renders, from
the CRD `openAPIV3Schema`:

- group / version / plural / scope (e.g. `devenvironments.ai.cubestack.io` /
  `v1alpha1` / Namespaced),
- the set of **required** `spec` fields,
- per-field **type**, **default**, **enum**, **format** where present
  (e.g. `resources.cpu` is a *string*, `gpuCount` is an *integer(int32)* with
  `minimum:1`),
- nested container children to depth 2 (e.g. `spec.resources.*`,
  `spec.storage.size`), so the "flat cpu/memory strings, not
  `requests`/`limits`" trap is visible from the map itself.

This is the part that removes schema-guessing round-trips: the agent reads the
map once and applies a valid manifest on the first try.

### 2.2 Hand-authored file: `SKILL.md` — narrative platform usage guide

Hand-authored, stable product semantics:

- What CubeStack is and which resources it exposes; the conceptual model of a
  DevEnvironment (container `type` picks the entry/image; `running:false` means
  Stopped; `storage` creates a managed workspace PVC, default 10Gi mounted at
  `/workspace`, `pvcRetention` retain/delete; stopping scales to zero but keeps
  the PVC).
- A **known-good example manifest** for the primary scenario (DevEnvironment)
  matching the chat e2e prompt, with each field explained.
- After creation: how to read `status.phase`, `status.conditions`,
  `status.endpoints` and interpret them (Jupyter URL, SSH `host:port`,
  extra ports).
- The roles of the other kinds (InferenceService / InferenceRuntimeProfile /
  ModelVersion) and their relationships, at whatever depth is currently true —
  **without** a "development status" ledger.
- Common mistakes section (the guessing traps the observed e2e fell into).

`type: domain`-style guidance in prose; the skill keeps `< 500` words where
possible, cross-references `kubectl-platform` for how to run kubectl / which
identity to use (never re-documents the dual-kubeconfig rules), and points the
agent at `crd-reference.md` for the schema map.

## 3. Freshness mechanism (schema map)

### 3.1 Generator

- Input: `test/e2e/framework/testdata/cubestack-crds/*.yaml` (the four vendored
  CRDs; refreshed from `suanova/cubestack` by the existing `make update-crds`,
  Makefile lines 95–103).
- Tool: a small Go program under `hack/gen-cubestack-skill/` invoked by a `make
  update-cubestack-skill` target. It parses each CRD, renders the
  quick-reference markdown, and rewrites
  `internal/skill/skills/cubestack-platform/crd-reference.md` in place
  (the whole file is generated). The guard test reuses the same Go code
  (generation exposed as a library function), so the committed output and the
  checked output can never diverge.
- SKILL.md is never touched by the generator.
- Ordering contract: run `make update-crds` first if the upstream CRDs changed,
  then `make update-cubestack-skill`.

### 3.2 Guard test (unit)

`internal/skill/cubestackgen/cubestackgen_test.go`:

1. **Freshness**: regenerate `crd-reference.md` in memory from the vendored CRDs
   and assert it equals the committed file. On mismatch it fails with "run `make
   update-cubestack-skill`". This is what makes a future `make update-crds` bump
   fail CI in the same PR until the skill is regenerated — schema drift becomes
   impossible.
2. **Example-manifest structure check**: extract the fenced example manifest(s)
   from the narrative SKILL.md, decode as unstructured YAML, and validate
   against the vendored CRD schemas: required `spec` fields present, enum
   values legal, integer fields integer-typed, string fields string-typed.
   Lightweight structural validation only (no cluster, no full openAPI
   validation engine).

## 4. Maintenance runbook skill (repo-level, not a platform skill)

New repo skill `.claude/skills/refresh-cubestack-platform-skill/SKILL.md`
(in cubepilot's own `.claude/skills/`, which does not exist yet — first entry).
It is a Claude Code skill for **repo developers/agents**, **not** shipped to
platform agents (it is not under `internal/skill/skills/`).

Description: use when the `cubestack-platform` guard test fails, or after
CubeStack CRD types / runtime behavior change, to regenerate or edit the
builtin `cubestack-platform` skill.

Body (runbook):

1. If upstream CubeStack CRDs changed: `make update-crds`.
2. Regenerate the schema map: `make update-cubestack-skill` (rewrites
   `crd-reference.md`; never hand-edit that file).
3. If runtime behavior changed (narrative): edit
   `internal/skill/skills/cubestack-platform/SKILL.md` (facts sourced from the
   CubeStack operator code / CRD field docs).
4. Run `go test ./internal/skill/...` — the freshness guard must pass.
5. Review the diff; note in the commit which CubeStack behavior the narrative
   change tracks.

## 5. Wiring changes

The builtin set derives automatically from the embedded dirs
(`skill.BuiltinSkillNames()` reads `skills/*`), so adding the directory is the
main registration step. Because the skill now ships a supporting file, the
embed pattern broadens:

- `internal/skill/skill_source.go` — the `//go:embed` changes from
  `skills/*/SKILL.md` to `all:skills/*` so `crd-reference.md` (and any future
  supporting file) ships in the skill tar. `BuiltinSkillNames()` /
  `PackBuiltinSkill` are unchanged in behavior (they still read SKILL.md /
  pack the whole dir).
- `internal/skill/skill_source_test.go` — `TestBuiltinSkillNames` `want` map
  gains `"cubestack-platform": true`.
- `internal/store/store.go` `DefaultAgentConfig` (~lines 177–183) — add
  `{Name: "cubestack-platform", Enabled: true}`.
- `web/src/views/AgentView.tsx` `SKILL_LABELS` (~lines 9–13) — add
  `'cubestack-platform': '<display label>'`.
- `workspace/AGENTS.md` — capability list adds `cubestack-platform`; **principle
  6 is rewritten**: platform CRDs now *do* have a dedicated skill — consult it
  before schema discovery; fall back to the `kubectl-platform` discovery recipe
  only for kinds the map does not cover.
- `docs/cubepilot/api.md` — skill examples (~lines 193, 234, 252) gain the new
  name.
- `test/e2e/bootstrap_test.go` — unchanged (derives from
  `skill.BuiltinSkillNames()`).

Default `enabledSkills` is empty = all enabled, so existing instances/agents
pick up the skill automatically; the chat agent loads it by its description when
it needs to operate `ai.cubestack.io` resources.

## 6. Testing / verification

- Unit: freshness + structure guard test; `go vet ./...`;
  `go test ./cmd/... ./internal/...`; `cd web && npx tsc --noEmit`.
- e2e behavior gate (real cluster + `CUBEPILOT_E2E_CHAT=1`): the existing
  `cubestack_chat_test.go` spec must still pass (agent creates the DevEnvironment
  in one clean turn). Tool-call/message counts may be logged as informational
  evidence of fewer round-trips vs. the pre-skill baseline, but are **not** a
  gate (transcript reconstruction is provider-lossy, cf. #86).
- The chat e2e cannot run in CI without an LLM key; it is compile-verified and
  flagged for a live-cluster run before merge, as in #86.

## 7. Out of scope

- Publishing the platform map from the CubeStack repo (single-source in
  cubepilot; revisit if CubeStack grows its own agent-facing docs).
- Deep per-kind narrative for every CR — depth grows as CubeStack behavior is
  stabilized.
- "Development status / controller implements?" ledger — explicitly excluded per
  requirements.
- A per-CRD skill per kind (anti-pattern removed in #86).
