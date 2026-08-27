# AgentTemplate CRD (issue #9) + Capability -> Skill alignment + e2e Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Complete issue #9 (AgentTemplate CRD) and make the repo's CRD set match the simplified design (`AgentTemplate, AgentInstance, Skill, TaskTemplate, Task, TaskRun`), then prove it with the e2e deploy path on a kind cluster.

**Architecture:** The AgentTemplate type/enums/revision already exist; we (a) finish the builtin template's inline Platform+External models and add unit tests, (b) rename the `Capability` CRD/type to `Skill` across Go, CRDs, RBAC, web and e2e while keeping the embedded SKILL.md catalog mechanism (the full skill marketplace stays a separate epic), (c) drop the stale `agents`/`models` CRD yamls, and (d) run `scripts/e2e.sh` (deploy path).

**Tech Stack:** Go (controller-runtime), kubebuilder markers + controller-gen v0.19.0, Helm chart CRDs, React/TS web, bash e2e on kind.

## Global Constraints

Apply this rename map everywhere (identifiers, comments, docs, yaml, RBAC, web, tests):

| Current | After |
|---|---|
| `Capability` / `CapabilitySpec` / `CapabilityStatus` / `CapabilityList` | `Skill` / `SkillSpec` / `SkillStatus` / `SkillList` |
| `CapabilityType` / `CapabilityAtomic` / `CapabilityDomain` | `SkillType` / `SkillAtomic` / `SkillDomain` |
| `CapabilityTarget` / `CapabilitySemantics` / `CapabilityFile` | `SkillTarget` / `SkillSemantics` / `SkillFile` |
| `ResolvedCapability` | `ResolvedSkill` |
| `BuiltinCapabilityDefinitions` / `BuiltinCapabilities` | `BuiltinSkillDefinitions` / `BuiltinSkills` |
| `ValidateCapability` | `ValidateSkill` |
| package `capability` (dir `internal/capability`) | package `skill` (dir `internal/skill`) |
| CRD resource `capabilities` (plural) | `skills` (group -> `ai.cubestack.io`, see below) |
| API `/api/capabilities`, JSON key `"capabilities"` | `/api/skills`, JSON key `"skills"` |
| `listCapabilities` / `handleCapabilities` | `listSkills` / `handleSkills` |
| `TaskTemplateSpec.Capabilities` (design §3.5 `skills`) | `TaskTemplateSpec.Skills` |
| `TaskRunStatus.CapabilityRevision` (design §3.5 `skillRevision`) | `TaskRunStatus.SkillRevision` |
| `capabilityRevisions` / `capabilityRev` (scheduler) | `skillRevisions` / `skillRev` |
| `v1alpha1.CapabilityFile` in resolver | `v1alpha1.SkillFile` |
| `ResolvedAgentConfig.Capabilities` | `ResolvedAgentConfig.Skills` |

Filenames: `internal/api/v1alpha1/capability_types.go` -> `skill_types.go`;
`internal/capability/` -> `internal/skill/`; CRD yamls
`assistant.suanova.io_capabilities.yaml` -> `assistant.suanova.io_skills.yaml`
(both `config/crd/bases/` and `deploy/charts/cubepilot/crds/`);
`internal/controller/capabilities/` (embedded SKILL.md dir) ->
`internal/controller/skills/`.

NOT renamed: Linux kernel capabilities (`corev1.Capabilities`, `drop ALL`) in
`internal/k8s/resources.go` -- these are not the CRD. Comments only changes:
`internal/config/config.go:19`, `internal/instances/manager.go:172`,
`internal/store/store.go:54,175`.

API group: ALL CRDs move to `ai.cubestack.io` (design §3.1). Change the
`+groupName` marker, every kubebuilder `groups=assistant.suanova.io` RBAC
marker, the catalog `SchemaFor` default group, CRD yaml filenames + contents
(controller-gen regenerates them from the marker; the stale
`assistant.suanova.io_*.yaml` files are deleted manually -- controller-gen
does not clean them), chart `rbac.yaml` `apiGroups`, `scripts/e2e.sh` CRD
assertions, and README / api.md / implementation-status docs. Web has no
group refs.

---

### Task 1: Rename the API types -- `capability_types.go` -> `skill_types.go`

**Files:**
- Create: `internal/api/v1alpha1/skill_types.go` (renamed content of `capability_types.go`, via the rename map)
- Delete: `internal/api/v1alpha1/capability_types.go`
- Modify: `internal/api/v1alpha1/groupversion_info.go` (package comment)
- Modify: `internal/api/v1alpha1/revision.go` (comment: "capabilities" -> "skills")
- Modify: `internal/api/v1alpha1/taskrun_types.go` (`CapabilityRevision` -> `SkillRevision`)
- Modify: `internal/api/v1alpha1/tasktemplate_types.go` (`Capabilities []string` -> `Skills []string`)
- Modify: `internal/api/v1alpha1/agenttemplate_types.go` (comments referencing "capability" -> "skill")
- Modify: `internal/api/v1alpha1/zz_generated.deepcopy.go` (regenerated)

**Interfaces:**
- Produces: `v1alpha1.Skill`, `SkillSpec`, `SkillStatus`, `SkillList`, `SkillType` (`SkillAtomic`/`SkillDomain`), `SkillTarget`, `SkillSemantics`, `SkillFile`, `Skill.Revision() string`, `TaskTemplateSpec.Skills []string`, `TaskRunStatus.SkillRevision string`.

- [ ] **Step 1: Install controller-gen v0.19.0**

```bash
GOBIN="$(go env GOPATH)/bin" go install sigs.k8s.io/controller-tools/cmd/controller-gen@v0.19.0
command -v controller-gen || ls "$(go env GOPATH)/bin"
```

- [ ] **Step 2: Create `skill_types.go`**

Rename every identifier in the current `capability_types.go` content per the Global Constraints map. Keep struct fields, kubebuilder markers, validation enums and the `Revision()` method identical except for names. Key renamed declarations:

```go
// SkillType is the capability layer (design §3.4: skill = domain knowledge +
// controlled scripts). Atomic = thin CRD-bound override; Domain = knowledge.
// +kubebuilder:validation:Enum=Atomic;Domain
type SkillType string

const (
	SkillAtomic SkillType = "Atomic"
	SkillDomain SkillType = "Domain"
)

type SkillTarget struct {
	Group   string `json:"group"`
	Version string `json:"version"`
	Kind    string `json:"kind"`
}

type SkillSemantics struct {
	Title       string   `json:"title,omitempty"`
	Description string   `json:"description,omitempty"`
	Examples    []string `json:"examples,omitempty"`
}

type SkillFile struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

type SkillSpec struct {
	Type         SkillType         `json:"type"`
	Title        string            `json:"title,omitempty"`
	Description  string            `json:"description,omitempty"`
	Override     bool              `json:"override,omitempty"`
	Target       *SkillTarget      `json:"target,omitempty"`
	Semantics    *SkillSemantics   `json:"semantics,omitempty"`
	Uses         []string          `json:"uses,omitempty"`
	Instructions string            `json:"instructions,omitempty"`
	Files        []SkillFile       `json:"files,omitempty"`
	ContentRef   string            `json:"contentRef,omitempty"`
	Agents       []string          `json:"agents,omitempty"`
	OwnerModule  string            `json:"ownerModule,omitempty"`
}

type SkillStatus struct {
	ObservedGeneration int64  `json:"observedGeneration,omitempty"`
	Valid              bool   `json:"valid,omitempty"`
	Message            string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Type",type="string",JSONPath=".spec.type"
// +kubebuilder:printcolumn:name="Target",type="string",JSONPath=".spec.target.kind"
// +kubebuilder:printcolumn:name="Valid",type="boolean",JSONPath=".status.valid"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
type Skill struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec   SkillSpec   `json:"spec,omitempty"`
	Status SkillStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type SkillList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Skill `json:"items"`
}

func init() { SchemeBuilder.Register(&Skill{}, &SkillList{}) }

func (in *Skill) Revision() string { return specRevision(in.Spec) }
```

Update the doc comment to reference design §3.4 (技能市场) instead of §3.3.1.
Delete `capability_types.go`.

- [ ] **Step 3: Rename the related type fields and comments**

`taskrun_types.go`: `CapabilityRevision string \`json:"capabilityRevision,omitempty"\`` -> `SkillRevision string \`json:"skillRevision,omitempty"\``, comment: "skill revision actually used".

`tasktemplate_types.go`: `Capabilities []string \`json:"capabilities,omitempty"\`` -> `Skills []string \`json:"skills,omitempty"\``, comment: "skills the task needs (resolved at run time, design §3.5)".

`groupversion_info.go`: package comment "Agent / AgentInstance / Capability / TaskTemplate / Task / TaskRun" -> "AgentTemplate / AgentInstance / Skill / TaskTemplate / Task / TaskRun", AND the group -> `ai.cubestack.io`:

```go
// +kubebuilder:object:generate=true
// +groupName=ai.cubestack.io
package v1alpha1

var GroupVersion = schema.GroupVersion{Group: "ai.cubestack.io", Version: "v1alpha1"}
```

`agenttemplate_types.go`: comments "skills" already; update any "capability" wording to "skill".

- [ ] **Step 4: Regenerate deepcopy**

```bash
controller-gen object:headerFile="hack/boilerplate.go.txt" paths="./internal/api/..."
grep -c "Capability" internal/api/v1alpha1/zz_generated.deepcopy.go || echo "0 Capability refs remain"
```

- [ ] **Step 5: Verify the API package compiles**

Run: `go build ./internal/api/...`
Expected: PASS (the v1alpha1 package no longer references `Capability`). Note: other packages still break -- that is resolved in Task 2.

- [ ] **Step 6: Commit**

```bash
git add internal/api/v1alpha1
git commit -s -m "refactor(api): rename Capability CRD to Skill (design §3.4)

Rename Capability/Skill type, TaskTemplateSpec.Capabilities -> Skills,
TaskRunStatus.CapabilityRevision -> SkillRevision. Assisted-by: Claude Code"
```

---

### Task 2: Rename Go consumers + tests (package sweep)

**Files:**
- Rename dir: `internal/capability/` -> `internal/skill/` (keep `catalog.go`, `catalog_test.go`; package statement + comments updated)
- Modify: `internal/controller/skill_source.go`, `internal/controller/builtin.go`, `internal/controller/agentinstance_controller.go` (RBAC markers), `internal/controller/builtin_test.go`
- Modify: `internal/resolver/resolver.go`, `internal/resolver/resolver_test.go`
- Modify: `internal/scheduler/scheduler.go`, `internal/scheduler/scheduler_test.go`
- Modify: `internal/supervisor/supervisor.go`, `internal/supervisor/supervisor_test.go`
- Modify: `internal/server/server.go`, `internal/server/handlers_platform.go`, `internal/server/internal_api_test.go`
- Modify: `cmd/cubepilot-api/main.go`, `cmd/cubepilot-operator/main.go`, `cmd/cubepilot-supervisor/main.go` (comments)
- Rename dir: `internal/controller/capabilities/` -> `internal/controller/skills/` (embedded SKILL.md dir)

**Interfaces:**
- Consumes: `v1alpha1.Skill` types from Task 1.
- Produces: `skill.ValidateSkill(*v1alpha1.Skill) error`, `skill.ToolSetForAgent(*v1alpha1.AgentTemplate, []v1alpha1.Skill) []string`, `resolver.ResolvedSkill`, `resolver.RenderSkill(resolver.ResolvedSkill) (string, error)`, `controller.BuiltinSkillDefinitions() ([]*v1alpha1.Skill, error)`, scheduler `skillRevisions(ctx, []string) string`.

- [ ] **Step 1: Rename the catalog package**

```bash
git mv internal/capability internal/skill
sed -i 's/^package capability$/package skill/' internal/skill/catalog.go internal/skill/catalog_test.go
```

In `catalog.go`: `ValidateCapability(cap *v1alpha1.Capability)` -> `ValidateSkill(skill *v1alpha1.Skill)`; all `v1alpha1.Capability`/`CapabilityAtomic`/`CapabilityDomain` refs per the map; error strings "capability %s" -> "skill %s"; `SchemaFor` default group `"assistant.suanova.io"` -> `"ai.cubestack.io"`. Keep generic-tool constants, `CRDSchema`, `Catalog`, `ToolSetForAgent` (its signature uses `[]v1alpha1.Skill` now). In `catalog_test.go` rename all `Capability` refs.

- [ ] **Step 2: Update the embedded skill source + bootstrap**

`internal/controller/skill_source.go`:
- `//go:embed capabilities/*/SKILL.md` -> `//go:embed skills/*/SKILL.md`
- `fs.ReadDir(capabilitiesFS, "capabilities")` -> `fs.ReadDir(capabilitiesFS, "skills")`
- `BuiltinCapabilityDefinitions` -> `BuiltinSkillDefinitions`; `[]*v1alpha1.Capability` -> `[]*v1alpha1.Skill`; `v1alpha1.CapabilitySpec` -> `v1alpha1.SkillSpec`; `CapabilityDomain` -> `SkillDomain`; "Capability CRD" comments -> "Skill CRD".

`internal/controller/builtin.go`:
- `BuiltinCapabilities` var -> `BuiltinSkills` (still populated from embedded names).
- `BuiltinCapabilityDefinitions()` call -> `BuiltinSkillDefinitions()`.
- RBAC markers (group -> `ai.cubestack.io`, drop `agents`/`models`):
```go
// +kubebuilder:rbac:groups=ai.cubestack.io,resources=skills;tasktemplates;agentinstances;tasks;taskruns,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ai.cubestack.io,resources=agentinstances/status;skills/status;tasks/status;taskruns/status,verbs=get;update;patch
```
- `BuiltinTaskTemplate()`: `Capabilities: []string{"cluster-inspection"}` -> `Skills: []string{"cluster-inspection"}`.
- "preset Capabilities" comments -> "preset Skills".

`internal/controller/agentinstance_controller.go` RBAC markers (group -> `ai.cubestack.io`, drop the stale `agents` resource -- the operator reads `agenttemplates` + `skills`):
```go
// +kubebuilder:rbac:groups=ai.cubestack.io,resources=agenttemplates,verbs=get;list;watch
// +kubebuilder:rbac:groups=ai.cubestack.io,resources=skills,verbs=get;list;watch
```

`internal/scheduler/scheduler.go` RBAC markers (group -> `ai.cubestack.io`):
```go
// +kubebuilder:rbac:groups=ai.cubestack.io,resources=tasks,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=ai.cubestack.io,resources=taskruns,verbs=get;list;watch;create;update;patch
```

- [ ] **Step 3: Update resolver + scheduler + supervisor**

`internal/resolver/resolver.go`:
- `ResolvedCapability` -> `ResolvedSkill`; `Files []v1alpha1.CapabilityFile` -> `Files []v1alpha1.SkillFile`.
- `ResolvedAgentConfig.Capabilities []ResolvedCapability` -> `Skills []ResolvedSkill`.
- `var caps v1alpha1.CapabilityList` -> `var skills v1alpha1.SkillList`; `r.cr.List(ctx, &caps)` -> `r.cr.List(ctx, &skills)`; error "list capabilities" -> "list skills".
- loop vars `cap`/`caps.Items` -> `skill`/`skills.Items`; `cap.Spec.Type != v1alpha1.CapabilityDomain` -> `skill.Spec.Type != v1alpha1.SkillDomain`; `append(cfg.Capabilities, ...)` -> `append(cfg.Skills, ResolvedSkill{...})`; `cap.Revision()` -> `skill.Revision()`.
- `RenderSkill(cap ResolvedCapability)` -> `RenderSkill(skill ResolvedSkill)`; "capability has no name" -> "skill has no name".
- Comment "Model catalog + Capabilities" -> "Model catalog + Skills".

`internal/scheduler/scheduler.go`:
- `capabilityRev` -> `skillRev`; `r.capabilityRevisions(...)` -> `r.skillRevisions(...)`; `tpl.Spec.Capabilities` -> `tpl.Spec.Skills`.
- `run.Status.CapabilityRevision = capabilityRev` -> `run.Status.SkillRevision = skillRev`.
- `capabilityRevisions` func -> `skillRevisions`; `var cap v1alpha1.Capability` -> `var skill v1alpha1.Skill`; error strings "capability %s" -> "skill %s".

`internal/supervisor/supervisor.go`: `for _, cap := range cfg.Capabilities` -> `for _, skill := range cfg.Skills`; log strings "capability" -> "skill" (code lines only).

- [ ] **Step 4: Update server + cmd**

`internal/server/server.go`: import `"github.com/suanova/cubepilot/internal/capability"` -> `".../internal/skill"`; route `mux.HandleFunc("/api/capabilities", s.handleCapabilities)` -> `mux.HandleFunc("/api/skills", s.handleSkills)`; `handleCapabilities` field refs in Server struct -> `handleSkills`.

`internal/server/handlers_platform.go`:
- `handleCapabilities` -> `handleSkills`; `/api/capabilities` -> `/api/skills`; JSON keys `"capabilities"` -> `"skills"`.
- `v1alpha1.CapabilityList` -> `v1alpha1.SkillList`; comment "Capability catalog ... Capability CRs" -> "Skill catalog ... Skill CRs".
- `listCapabilities(ctx) ([]v1alpha1.Capability, error)` -> `listSkills(ctx) ([]v1alpha1.Skill, error)`.

`cmd/cubepilot-api/main.go`: import `internal/capability` -> `internal/skill`; `capability.NewCatalog` -> `skill.NewCatalog`; comments "Capability catalog"/"Capability atomic/domain" -> "Skill catalog"/"Skill atomic/domain". `cmd/cubepilot-operator/main.go` + `cmd/cubepilot-supervisor/main.go`: comment updates only ("capabilities" -> "skills").

- [ ] **Step 5: Update Go test files**

Per the map in these files: `internal/controller/builtin_test.go`, `internal/resolver/resolver_test.go`, `internal/scheduler/scheduler_test.go`, `internal/supervisor/supervisor_test.go`, `internal/server/internal_api_test.go`, `internal/skill/catalog_test.go`. E.g. `cfg.Capabilities` -> `cfg.Skills`, `v1alpha1.Capability` -> `v1alpha1.Skill`, `capabilityRevisions`/`CapabilityRevision` -> `skill*`.

- [ ] **Step 6: Rename the embedded skills dir**

```bash
git mv internal/controller/capabilities internal/controller/skills
```

- [ ] **Step 7: Verify the whole Go tree**

Run: `go build ./... && go vet ./...`
Expected: PASS with no references to `Capability`/`capabilities` (except `corev1.Capabilities`).

Then: `go test ./...`
Expected: PASS.

Confirm no stale refs:
- `grep -rn "Capabilit" --include='*.go' internal/ cmd/ | grep -v corev1` should show only comments (config/manager/store) -- fix those comments too if you like.
- `grep -rn "assistant.suanova.io" --include='*.go' internal/ cmd/` should return NOTHING.

- [ ] **Step 8: Commit**

```bash
git add -A
git commit -s -m "refactor: rename Capability CRD to Skill across Go consumers

Rename catalog package to skill, resolver/scheduler/supervisor/server refs,
RBAC markers, embedded skills dir. Assisted-by: Claude Code"
```

---

### Task 3: Regenerate CRD yamls + sync chart + chart RBAC

**Files:**
- Regenerate: `config/crd/bases/*` (controller-gen)
- Modify: `deploy/charts/cubepilot/crds/*` (copy from regenerated config)
- Modify: `deploy/charts/cubepilot/templates/rbac.yaml`

**Interfaces:**
- Produces: 6 CRDs `agenttemplates agentinstances skills tasktemplates tasks taskruns` under `ai.cubestack.io` (no `agents`, `models`, `capabilities`).

- [ ] **Step 1: Delete stale CRD yamls, then regenerate CRDs from types**

```bash
git rm config/crd/bases/assistant.suanova.io_*.yaml
controller-gen crd paths="./..." output:crd:artifacts:config=config/crd/bases
ls config/crd/bases/
```

Expected: exactly `ai.cubestack.io_{agentinstances,agenttemplates,skills,taskruns,tasks,tasktemplates}.yaml` -- the old `assistant.suanova.io_*` files (including stale `agents`, `models`, `capabilities`) are gone.

- [ ] **Step 2: Confirm `skills` CRD + `agenttemplates` CRD contents**

```bash
grep -E "name: |plural:|kind:|group:" config/crd/bases/ai.cubestack.io_skills.yaml config/crd/bases/ai.cubestack.io_agenttemplates.yaml
```
`skills` should have `group: ai.cubestack.io`, `plural: skills`, `kind: Skill`, enums `Atomic;Domain`. `agenttemplates` should have `group: ai.cubestack.io` and the runtime/provider/confirmPolicy enums (plus the Models `XValidation` rule after Task 5).

- [ ] **Step 3: Sync regenerated CRDs into the Helm chart**

```bash
git rm deploy/charts/cubepilot/crds/assistant.suanova.io_*.yaml
cp config/crd/bases/ai.cubestack.io_*.yaml deploy/charts/cubepilot/crds/
ls deploy/charts/cubepilot/crds/
```
Expected: 6 files, matching config (verify: `md5sum config/crd/bases/*.yaml | awk '{print $1}'` equal across both dirs).

- [ ] **Step 4: Update chart RBAC**

In `deploy/charts/cubepilot/templates/rbac.yaml` replace every `assistant.suanova.io` apiGroup with `ai.cubestack.io`, every `capabilities` resource with `skills`, and drop `agents`/`models` if present:
- operator ClusterRole `apiGroups: ["ai.cubestack.io"]`, resources: `["agenttemplates", "agentinstances", "skills", "tasktemplates", "tasks", "taskruns"]`
- operator status resources: `["agenttemplates/status", "agentinstances/status", "skills/status", "tasks/status", "taskruns/status"]`
- api ClusterRole `apiGroups: ["ai.cubestack.io"]`, resources: `["agenttemplates", "skills", "tasktemplates", "taskruns"]`

- [ ] **Step 5: Verify chart renders**

Run: `helm template cubepilot deploy/charts/cubepilot -n cubepilot > /dev/null && echo OK` (requires helm; install in Task 7 if not present).
Expected: renders without error; `helm template ... | grep skills` shows `skills` in the RBAC rules.

- [ ] **Step 6: Commit**

```bash
git add config/crd/bases deploy/charts/cubepilot
git commit -s -m "chore(crd): regenerate CRDs for Skill rename; drop stale agents/models

config/crd/bases now emits agenttemplates/agentinstances/skills/tasktemplates/tasks/taskruns;
chart crds + rbac synced. Assisted-by: Claude Code"
```

---

### Task 4: Rename in the web UI

**Files:**
- Modify: `web/src/api/index.ts`
- Modify: `web/src/views/AgentView.tsx`
- Modify: `web/src/App.tsx` (placeholder label, optional)
- Modify: `web/src/views/ChatView.tsx` (placeholder label, optional)

- [ ] **Step 1: Rename the API client**

In `web/src/api/index.ts`:
```ts
listSkills: () =>
  apiFetch<{ skills: PlatformObject[] }>('/api/skills').then((d) => d.skills),
```

- [ ] **Step 2: Update AgentView labels**

`listCapabilities` has no consumer in the web app (AgentView reads skills from `agentConfig().skills`), so `web/src/views/AgentView.tsx` needs only the two user-facing strings updated: "Platform Capabilities - Capability Catalog" -> "Platform Skills - Skill Catalog" (line 267) and "No registered capabilities" -> "No registered skills" (line 288).

- [ ] **Step 3: Update placeholder labels (optional but nice)**

`web/src/App.tsx:129` "Search resources, logs, capabilities..." -> "Search resources, logs, skills..."; `web/src/views/ChatView.tsx:442` "platform capabilities" -> "platform skills".

- [ ] **Step 4: Build the SPA**

Run: `cd web && npm run build`
Expected: `vue-tsc` type-check + vite build PASS.

- [ ] **Step 5: Commit**

```bash
git add web/src
git commit -s -m "feat(web): rename capabilities to skills (api + labels)

Assisted-by: Claude Code"
```

---

### Task 5: Complete issue #9 -- External model, CEL validation, unit tests

**Files:**
- Modify: `internal/controller/builtin.go` (`BuiltinModels`)
- Modify: `internal/controller/builtin_test.go`
- Modify: `internal/api/v1alpha1/agenttemplate_types.go` (add `Validate()` + CEL marker)
- Create: `internal/api/v1alpha1/agenttemplate_types_test.go`

**Interfaces:**
- Produces: `(m TemplateModelSpec) Validate() error`; CEL rule on `AgentTemplateSpec.Models`; `BuiltinModels()` returns 2 entries (Platform + External).

- [ ] **Step 1: Add External model to the builtin template**

Replace `BuiltinModels()` in `internal/controller/builtin.go`:
```go
func BuiltinModels() []v1alpha1.TemplateModelSpec {
	return []v1alpha1.TemplateModelSpec{
		{
			Name:     "deepseek-v4-flash",
			Provider: v1alpha1.ModelProviderPlatform,
			ModelID:  "cuberouter/deepseek-v4-flash-0731",
		},
		{
			Name:          "qwen2.5-72b",
			Provider:      v1alpha1.ModelProviderExternal,
			Endpoint:      "https://api.example.com/v1",
			CredentialRef: "cred-llm-org",
			ModelID:       "qwen2.5-72b",
		},
	}
}
```
(design §3.1: inline Platform + External entries.)

- [ ] **Step 2: Add model validation + CEL marker**

In `internal/api/v1alpha1/agenttemplate_types.go`, add to `TemplateModelSpec`:
```go
// Validate enforces the inline-model invariants (design §3.3): an External
// model requires endpoint + credentialRef; the provider must be known.
func (m TemplateModelSpec) Validate() error {
	switch m.Provider {
	case ModelProviderPlatform:
		return nil
	case ModelProviderExternal:
		if m.Endpoint == "" || m.CredentialRef == "" {
			return fmt.Errorf("external model %q requires endpoint and credentialRef", m.Name)
		}
		return nil
	default:
		return fmt.Errorf("model %q: unknown provider %q", m.Name, m.Provider)
	}
}
```
(add `"fmt"` to imports.)

On `AgentTemplateSpec.Models`, add the CEL rule (API-server enforcement, matches "非法组合被 API server 拒绝" in design §3.3):
```go
	// Models is the inline model list (design §3.3: models are inlined in the
	// template -- no standalone Model CRD). External models require endpoint +
	// credentialRef (enforced by CEL).
	// +kubebuilder:validation:XValidation:rule="self.all(m, m.provider != 'External' || (has(m.endpoint) && has(m.credentialRef)))",message="external models require endpoint and credentialRef"
	// +optional
	Models []TemplateModelSpec `json:"models,omitempty"`
```

- [ ] **Step 3: Write the unit tests**

Create `internal/api/v1alpha1/agenttemplate_types_test.go`:
```go
package v1alpha1

import (
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestAgentTemplateSerializationRoundTrip verifies JSON round-trip of an
// AgentTemplate with inline Platform + External models (design §3.1/§3.3).
func TestAgentTemplateSerializationRoundTrip(t *testing.T) {
	in := &AgentTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-for-cloud"},
		Spec: AgentTemplateSpec{
			Runtime:       RuntimeOpenClaw,
			DefaultModel:  "deepseek-v4-flash",
			ConfirmPolicy: ConfirmPolicyConfirmWrites,
			Models: []TemplateModelSpec{
				{Name: "deepseek-v4-flash", Provider: ModelProviderPlatform, ModelID: "cuberouter/deepseek-v4-flash-0731"},
				{Name: "qwen2.5-72b", Provider: ModelProviderExternal, Endpoint: "https://api.example.com/v1", CredentialRef: "cred-llm-org"},
			},
			Skills: []string{"dev-environment", "inference-service"},
		},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out AgentTemplate
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Spec.Runtime != RuntimeOpenClaw || out.Spec.ConfirmPolicy != ConfirmPolicyConfirmWrites {
		t.Errorf("scalar round-trip mismatch: %+v", out.Spec)
	}
	if len(out.Spec.Models) != 2 || out.Spec.Models[1].Provider != ModelProviderExternal ||
		out.Spec.Models[1].Endpoint == "" || out.Spec.Models[1].CredentialRef == "" {
		t.Errorf("inline models not round-tripped: %+v", out.Spec.Models)
	}
}

// TestAgentTemplateRevision verifies the revision is a spec-only content hash:
// deterministic across re-creation, unchanged by status, changed by spec.
func TestAgentTemplateRevision(t *testing.T) {
	a := &AgentTemplate{ObjectMeta: metav1.ObjectMeta{Name: "agent-for-cloud"}, Spec: AgentTemplateSpec{DefaultModel: "deepseek-v4-flash"}}
	base := a.Revision()
	if len(base) != 12 {
		t.Fatalf("revision = %q, want 12 hex chars", base)
	}
	// Deterministic across re-creation (metadata ignored).
	b := &AgentTemplate{ObjectMeta: metav1.ObjectMeta{Name: "renamed"}, Spec: AgentTemplateSpec{DefaultModel: "deepseek-v4-flash"}}
	if b.Revision() != base {
		t.Errorf("revision depends on metadata: %q != %q", b.Revision(), base)
	}
	// Status change does not alter the revision.
	a.Status.ObservedGeneration = 42
	if a.Revision() != base {
		t.Errorf("status change altered revision: %q != %q", a.Revision(), base)
	}
	// Spec change does.
	a.Spec.ConfirmPolicy = ConfirmPolicyConfirmWrites
	if a.Revision() == base {
		t.Error("spec change did not alter revision")
	}
}

// TestTemplateModelValidate rejects invalid combinations (design §3.3):
// External requires endpoint + credentialRef; unknown provider is rejected.
func TestTemplateModelValidate(t *testing.T) {
	ok := []TemplateModelSpec{
		{Name: "platform", Provider: ModelProviderPlatform},
		{Name: "ext", Provider: ModelProviderExternal, Endpoint: "https://x", CredentialRef: "cred"},
	}
	for _, m := range ok {
		if err := m.Validate(); err != nil {
			t.Errorf("Validate(%s) = %v, want nil", m.Name, err)
		}
	}
	bad := []TemplateModelSpec{
		{Name: "no-endpoint", Provider: ModelProviderExternal, CredentialRef: "cred"},
		{Name: "no-cred", Provider: ModelProviderExternal, Endpoint: "https://x"},
		{Name: "weird", Provider: ModelProvider("Mystery")},
	}
	for _, m := range bad {
		if err := m.Validate(); err == nil {
			t.Errorf("Validate(%s) = nil, want error", m.Name)
		}
	}
}
```

- [ ] **Step 4: Update the builtin-shape test**

In `internal/controller/builtin_test.go`, `TestBuiltinAgentShape`: change `len(agent.Spec.Models) != 1 || agent.Spec.Models[0].Name != "deepseek-v4-flash"` to expect 2 entries:
```go
	if len(agent.Spec.Models) != 2 || agent.Spec.Models[0].Name != "deepseek-v4-flash" {
		t.Errorf("inline models = %v, want [deepseek-v4-flash, qwen2.5-72b]", agent.Spec.Models)
	}
	if agent.Spec.Models[1].Provider != v1alpha1.ModelProviderExternal ||
		agent.Spec.Models[1].Endpoint == "" || agent.Spec.Models[1].CredentialRef == "" {
		t.Errorf("builtin External model incomplete: %+v", agent.Spec.Models[1])
	}
```
`TestBootstrapEnsure` already asserts `len(tmpl.Spec.Models) == len(BuiltinModels())` -- keep.

- [ ] **Step 5: Re-run CRD generation for the CEL rule**

Run: `controller-gen crd paths="./..." output:crd:artifacts:config=config/crd/bases && cp config/crd/bases/ai.cubestack.io_*.yaml deploy/charts/cubepilot/crds/`
Verify the `XValidation` rule appears: `grep -n "external models require" config/crd/bases/ai.cubestack.io_agenttemplates.yaml`.

- [ ] **Step 6: Verify tests**

Run: `go test ./internal/api/... ./internal/controller/...`
Expected: PASS (new + existing).

- [ ] **Step 7: Commit**

```bash
git add internal/api/v1alpha1 internal/controller config/crd/bases deploy/charts/cubepilot/crds
git commit -s -m "feat(api): finish AgentTemplate CRD for issue #9

Builtin agent-for-cloud gains an inline External model (Platform + External),
External models validated via CEL + Go helper, unit tests for serialization,
revision and invalid combinations. Assisted-by: Claude Code"
```

---

### Task 6: Update e2e script + docs

**Files:**
- Modify: `scripts/e2e.sh`
- Modify: `docs/cubepilot/implementation-status.md`, `README.md`, `docs/cubepilot/api.md`

- [ ] **Step 1: Update e2e CRD list**

In `scripts/e2e.sh`, the 6-CRD loop (group -> `ai.cubestack.io`):
```bash
step "verify chart CRDs (ai.cubestack.io)"
for c in agenttemplates agentinstances skills tasktemplates tasks taskruns; do
  kubectl get crd "$c.ai.cubestack.io" >/dev/null 2>&1 || fail "CRD $c.ai.cubestack.io missing"
done
ok "6 CRDs"
```

- [ ] **Step 2: Update docs**

- `README.md`: the "Platform objects are Kubernetes CRDs" line -> `AgentTemplate`, `AgentInstance`, `Skill`, `TaskTemplate`, `Task`, `TaskRun` under `ai.cubestack.io`; any `Capability` -> `Skill`; any `assistant.suanova.io` -> `ai.cubestack.io`.
- `docs/cubepilot/implementation-status.md`: rename the gap-#1 title "技能市场（Skill CRD + 对象存储）未建" body to note the CRD is now `Skill` (renamed from `Capability`) but the marketplace (source path/S3, shared-volume repo, publish/install, visibility) is still deferred; update the "已对齐" bullet "能力目录 + Skill 落盘" and the "数据真源" row `Capability` -> `Skill`; update group `assistant.suanova.io` -> `ai.cubestack.io`; keep the rest.
- `docs/cubepilot/api.md`: `/api/capabilities` -> `/api/skills`, any `Capability` -> `Skill`, any `assistant.suanova.io` -> `ai.cubestack.io`.

- [ ] **Step 3: Validate the script**

Run: `bash -n scripts/e2e.sh`
Expected: no syntax errors.

- [ ] **Step 4: Commit**

```bash
git add scripts/e2e.sh README.md docs/cubepilot
git commit -s -m "docs(e2e): align CRD list and docs with Skill rename

e2e asserts skills.ai.cubestack.io; README/api/implementation-status updated.
Assisted-by: Claude Code"
```

---

### Task 7: Run the e2e deploy path

**Files:** none (environment + verification only)

**Interfaces:**
- Consumes: chart CRDs from Task 3, web from Task 4, docs from Task 6.

- [ ] **Step 1: Install helm (missing locally)**

```bash
if ! command -v helm >/dev/null; then
  curl -fsSL https://get.helm.sh/helm-v3.16.4-linux-amd64.tar.gz | sudo tar -xz -C /usr/local/bin --strip-components=1 linux-amd64/helm
fi
helm version --short
```

- [ ] **Step 2: Run the e2e deploy path**

Run: `cd "$(git rev-parse --show-toplevel)" && CUBEPILOT_MODEL_PROVIDERS='{"platform":{"base_url":"http://127.0.0.1:9999/v1","api_key":"placeholder"}}' scripts/e2e.sh`
Expected: deploy-phase assertions PASS -- kind cluster `cube`, namespace, 6 CRDs (incl. `skills.ai.cubestack.io`, no `capabilities`), secrets, operator/api/web rollouts, `/healthz`, portal HTML. Ends with `E2E PASS (deploy only)`.

If `CUBEPILOT_E2E_CHAT=1` with a real key is available, run it too; otherwise deploy-only is the acceptance gate.

- [ ] **Step 3: Confirm the new-group CRDs and clean stale ones**

The kind cluster may still hold `*.assistant.suanova.io` CRDs from prior deploys. Remove them, then assert the new set:
```bash
kubectl get crd --no-headers | awk '{print $1}' | grep '\.assistant\.suanova\.io$' | xargs -r kubectl delete crd
kubectl get crd | grep ai.cubestack.io
```
Expected: exactly `agenttemplates, agentinstances, skills, tasktemplates, tasks, taskruns` under `ai.cubestack.io` (no `agents`, `models`, `capabilities`, no `assistant.suanova.io`).

- [ ] **Step 4: Report results**

Summarize the e2e output in the PR/commit message; note whether chat phase ran.
