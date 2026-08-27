# Design: AgentTemplate CRD (issue #9) completion + Capability -> Skill alignment + e2e

Date: 2026-08-27
Status: approved (user: "Yes, proceed")
Goal owner: zhujian

## 1. Context & goal

Issue suanova/cubepilot#9 (AgentTemplate CRD) is mostly implemented already, but
has gaps against its acceptance criteria. Separately, the repo ships CRDs that
are not in the simplified design ([`docs/cubepilot/cubepilot-design.md`](../../cubepilot/cubepilot-design.md)
-- CRD set: AgentTemplate / AgentInstance / Skill / TaskTemplate / Task / TaskRun):

- `config/crd/bases` still carries stale `agents` + `models` CRD yamls (design
  has no Agent CRD -- the object is AgentTemplate -- and explicitly forbids a
  Model CRD).
- The capability catalog CRD is named `capabilities`; the design calls this
  object `Skill` (design §3.4).

Goal: complete issue #9, align the CRD set with the design (rename
Capability -> Skill, drop stale agents/models), and prove it with the existing
e2e path on a kind cluster.

## 2. Part A -- Complete issue #9 (AgentTemplate CRD)

The AgentTemplate type, enum validation (runtime / provider / confirmPolicy),
revision mechanism (`specRevision` content hash) and builtin `agent-for-cloud`
template already exist. Remaining acceptance gaps:

1. **Inline Platform + External models**: `BuiltinModels()` currently returns
   only one Platform entry (`deepseek-v4-flash`). Add an External entry
   (e.g. `qwen2.5-72b`, provider `External`, with `endpoint` + `credentialRef`),
   matching design §3.1's template shape (Platform + External).
2. **Unit tests** (new `internal/api/v1alpha1/agenttemplate_types_test.go`):
   - JSON round-trip serialization of AgentTemplate / TemplateModelSpec.
   - Revision computation: spec change -> new revision; status-only change ->
     same revision; deterministic across re-creation (content hash).
   - Rejection of invalid combinations: External model missing endpoint or
     credentialRef; unknown enum values for runtime / provider / confirmPolicy.
3. Update `internal/controller/builtin_test.go` (currently asserts
   `len(Models)==1`).
4. Sync `agenttemplates.assistant.suanova.io` CRD yaml into `config/crd/bases`
   (it exists only in `deploy/charts/cubepilot/crds/` today), keeping the enum
   validation already present there.

## 3. Part B -- Remove CRDs not in the design doc

### 3.1 Capability -> Skill rename (name-level, schema kept)

Rename the capability catalog object to `Skill` so the deployed CRD set matches
the design exactly. The rich phase-1 catalog schema (type / title /
description / instructions / files / target) is **kept as-is** -- it powers the
embedded SKILL.md catalog rendered by the supervisor. The design §3.4
marketplace fields (`source` path/S3, `sha256`, `visibility`) and the
shared-volume skill repo are **deferred** to the separate Skill-market epic
(issue #8 "Out: Skill CRD (Epic: Skill market)"); the divergence is documented
in `docs/cubepilot/implementation-status.md`.

Rename map:

| Current | After |
|---|---|
| `Capability` / `CapabilitySpec` / `CapabilityStatus` / `CapabilityList` | `Skill` / `SkillSpec` / `SkillStatus` / `SkillList` |
| `CapabilityType` (Atomic/Domain), `CapabilityTarget`, `CapabilitySemantics`, `CapabilityFile` | `SkillType`, `SkillTarget`, `SkillSemantics`, `SkillFile` |
| `internal/capability` package | `internal/skill` |
| CRD `capabilities.assistant.suanova.io` | `skills.assistant.suanova.io` |
| API `/api/capabilities` | `/api/skills` |

Files touched:

- Go: `internal/api/v1alpha1/capability_types.go` -> `skill_types.go`;
  `internal/controller/skill_source.go` (`BuiltinCapabilityDefinitions` ->
  `BuiltinSkillDefinitions`); `internal/controller/builtin.go`;
  `internal/capability/catalog.go` -> `internal/skill/catalog.go`;
  `internal/resolver/resolver.go` (`ResolvedCapability` ->
  `ResolvedSkill`); `internal/scheduler/scheduler.go`;
  `internal/supervisor/supervisor.go`; `internal/k8s/resources.go`;
  `internal/server/handlers_platform.go`; `zz_generated.deepcopy.go` (regenerate
  by hand -- controller-gen not in toolchain).
- CRD yamls: rename `config/crd/bases/*capabilities.yaml` and
  `deploy/charts/cubepilot/crds/*capabilities.yaml` to `*skills.yaml`; delete
  stale `config/crd/bases/*agents.yaml` and `*models.yaml`; add
  `config/crd/bases/assistant.suanova.io_agenttemplates.yaml`.
- RBAC: kubebuilder markers in `builtin.go` / `agentinstance_controller.go`;
  `deploy/charts/cubepilot/templates/rbac.yaml` (`capabilities` -> `skills`,
  drop `agents`/`models` refs).
- Web UI: `web/src/api/types.ts`, `web/src/api/index.ts`, `web/src/App.tsx`,
  `web/src/views/AgentView.tsx`, `web/src/views/ChatView.tsx`.
- e2e: `scripts/e2e.sh` 6-CRD assertion (`capabilities` -> `skills`).
- Docs: `docs/cubepilot/implementation-status.md`, `README.md`,
  `docs/cubepilot/api.md`, design-cross-ref comments.

### 3.2 API group -> `ai.cubestack.io`

All CRDs move to the design's API group `ai.cubestack.io` (design §3.1
examples / §4.4). Scope: the `+groupName` marker in
`internal/api/v1alpha1/groupversion_info.go`, all kubebuilder RBAC markers
(`builtin.go`, `agentinstance_controller.go`, `scheduler.go`), the catalog
`SchemaFor` default group, CRD yaml filenames + contents (regenerated by
controller-gen from the group marker; the stale `assistant.suanova.io_*.yaml`
files are deleted -- controller-gen does not clean them), chart `rbac.yaml`
`apiGroups`, `scripts/e2e.sh` CRD assertions, and README / api.md /
implementation-status docs. The web UI has no group references.

### 3.3 Out of scope (noted, not changed)

- Full Skill marketplace / Skill-market epic (source path/S3, sha256,
  visibility, shared-volume skill repo, publish/install).

## 4. Part C -- E2E test

- Install helm locally (missing; `setup.sh`/`e2e.sh` require helm v3).
- Update `scripts/e2e.sh` CRD list: `agenttemplates agentinstances skills
  tasktemplates tasks taskruns` (skills replaces capabilities).
- Run the deploy path on the running `cube` kind cluster:
  `CUBEPILOT_MODEL_PROVIDERS='<placeholder>' scripts/e2e.sh`.
- Chat phase only if a real provider key is available
  (`CUBEPILOT_E2E_CHAT=1`).

## 5. Verification

- `go vet ./...` and `go test ./...` green.
- `npm run build` (web, from `web/`) green.
- e2e deploy path green; deployed CRD set =
  `agenttemplates agentinstances skills tasktemplates tasks taskruns`.
