# Builtin `cubestack-platform` skill — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a third builtin skill, `cubestack-platform`, that gives chat agents a current, correct map of the CubeStack platform (generated CRD quick reference + hand-authored usage guide) so the chat e2e stops iterating `kubectl apply --dry-run=server` schema guesses when creating a `DevEnvironment`.

**Architecture:** The skill lives at `internal/skill/skills/cubestack-platform/` with two files: `SKILL.md` (hand-authored narrative + a known-good DevEnvironment example) and `crd-reference.md` (the whole generated per-kind schema map). A Go library `internal/skill/cubestackgen` parses the vendored CubeStack CRD YAMLs (`test/e2e/framework/testdata/cubestack-crds`, the `make update-crds` output) and renders `crd-reference.md`; a `make update-cubestack-skill` target rewrites it, and a unit guard test fails CI whenever the committed file is stale or the SKILL.md example manifest violates a CRD schema. The builtin set derives automatically from the embedded dirs (`skill.BuiltinSkillNames()`), so adding the directory registers the skill; the `go:embed` pattern broadens to `all:skills/*` so the supporting file ships. A repo-level Claude Code skill (`.claude/skills/refresh-cubestack-platform-skill/`) encodes the refresh runbook.

**Tech Stack:** Go (apiextensions v1 CRD types, `sigs.k8s.io/yaml`), Makefile, git.

## Global Constraints

- CRD group `ai.cubestack.io` / version `v1alpha1`, all CRDs vendored at `test/e2e/framework/testdata/cubestack-crds/` (four kinds: DevEnvironment, InferenceService, InferenceRuntimeProfile, ModelVersion).
- Never hardcode the builtin skill list — derive from `skill.BuiltinSkillNames()` (reads the embedded dirs). Tests that pin the set are updated, not duplicated. Do **not** edit `controller/builtin.go` — `BuiltinSkills` auto-derives.
- `crd-reference.md` is fully generated — never hand-edit; change the generator + `make update-cubestack-skill`.
- Commits: English messages, `git commit -s` (signoff), append `Assisted-by: Claude Code`.
- Code/comments in English; the agent-facing `workspace/AGENTS.md` and the skill's Chinese-facing notes keep their existing language conventions.
- Work happens in the worktree `.claude/worktrees/feat-issue98-cubestack-platform-skill` on branch `feat/issue98-cubestack-platform-skill` (issue #98, status In progress).

---

### Task 0: Save and commit the implementation plan

**Files:**
- Create: `docs/superpowers/plans/2026-09-03-cubestack-platform-skill.md` (this file)

- [ ] **Step 1: Commit the plan doc**

```bash
git add docs/superpowers/plans/2026-09-03-cubestack-platform-skill.md
git commit -s -m "docs: add cubestack-platform skill implementation plan (issue #98)

Assisted-by: Claude Code"
```

---

### Task 1: `cubestackgen` generator library

A small, focused package that turns the vendored CRD YAMLs into the
`crd-reference.md` markdown. Everything else (the `make` target, the guard
tests) calls into this one renderer so the committed output and the checked
output can never diverge.

**Files:**
- Create: `internal/skill/cubestackgen/cubestackgen.go`
- Test: `internal/skill/cubestackgen/cubestackgen_test.go`

**Interfaces:**
- Consumes: CRD YAML files at `test/e2e/framework/testdata/cubestack-crds/*.yaml` (already vendored).
- Produces:
  - `const Header string` — the fixed preamble of `crd-reference.md`.
  - `func LoadDir(dir string) ([]*apiextensionsv1.CustomResourceDefinition, error)`
  - `func RenderDocFromDir(dir string) (string, error)` — full `crd-reference.md` text (Header + per-kind map). Deterministic (kinds and fields sorted), so a later freshness test can compare byte-for-byte.

- [ ] **Step 1: Write the failing renderer tests**

Create `internal/skill/cubestackgen/cubestackgen_test.go`:

```go
package cubestackgen

import (
	"strings"
	"testing"
)

// testCRDDir points from the package dir up to the vendored CubeStack CRDs.
const testCRDDir = "../../../test/e2e/framework/testdata/cubestack-crds"

func TestRenderDocCoversAllKinds(t *testing.T) {
	doc, err := RenderDocFromDir(testCRDDir)
	if err != nil {
		t.Fatalf("RenderDocFromDir: %v", err)
	}
	for _, kind := range []string{"DevEnvironment", "InferenceService", "InferenceRuntimeProfile", "ModelVersion"} {
		if !strings.Contains(doc, "## "+kind+"\n") {
			t.Errorf("render should have a section for %s", kind)
		}
	}
}

func TestRenderDevEnvironmentEssentials(t *testing.T) {
	doc, err := RenderDocFromDir(testCRDDir)
	if err != nil {
		t.Fatalf("RenderDocFromDir: %v", err)
	}
	for _, want := range []string{
		"- resource `devenvironments.ai.cubestack.io`, apiVersion `ai.cubestack.io/v1alpha1`, scope Namespaced",
		"- spec requires: image, resources",
		"- `spec.image` — string · required",
		"- `spec.resources` — object · required",
		"- `spec.resources.cpu` — string",
		"- `spec.resources.gpuCount` — integer (int32) · default: 1 · min 1",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("render should contain %q", want)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/skill/cubestackgen/ -v`
Expected: FAIL — package `cubestackgen` has no Go files / `RenderDocFromDir` undefined.

- [ ] **Step 3: Implement the generator library**

Create `internal/skill/cubestackgen/cubestackgen.go`:

```go
// Package cubestackgen renders the generated "CubeStack CRD quick reference"
// of the builtin cubestack-platform skill (crd-reference.md) from the vendored
// CubeStack CRD YAMLs under test/e2e/framework/testdata/cubestack-crds.
//
// The map is machine-generated so the skill cannot drift from the CRDs the
// e2e suite installs: hack/gen-cubestack-skill rewrites crd-reference.md and
// the freshness unit test fails CI when the committed file is stale.
package cubestackgen

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// Header prefixes every generated crd-reference.md.
const Header = "# CubeStack CRD quick reference\n\n" +
	"Generated by make update-cubestack-skill from the vendored CubeStack CRDs\n" +
	"(test/e2e/framework/testdata/cubestack-crds). Do not edit by hand; the\n" +
	"freshness unit test in internal/skill/cubestackgen fails when this file is\n" +
	"stale.\n\n"

// LoadDir reads every *.yaml in dir as an apiextensions CustomResourceDefinition.
func LoadDir(dir string) ([]*apiextensionsv1.CustomResourceDefinition, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	out := make([]*apiextensionsv1.CustomResourceDefinition, 0, len(paths))
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := yaml.Unmarshal(raw, crd); err != nil {
			return nil, fmt.Errorf("decode %s: %w", filepath.Base(p), err)
		}
		out = append(out, crd)
	}
	return out, nil
}

// RenderDocFromDir renders a full crd-reference.md (Header + per-kind map) from
// the CRDs in dir. Deterministic output — kinds and fields are sorted — so the
// committed file and the freshness check always agree byte-for-byte.
func RenderDocFromDir(dir string) (string, error) {
	crds, err := LoadDir(dir)
	if err != nil {
		return "", err
	}
	return Header + render(crds), nil
}

func render(crds []*apiextensionsv1.CustomResourceDefinition) string {
	sort.Slice(crds, func(i, j int) bool {
		return crds[i].Spec.Names.Kind < crds[j].Spec.Names.Kind
	})
	var b strings.Builder
	for _, crd := range crds {
		renderCRD(&b, crd)
	}
	return b.String()
}

func renderCRD(b *strings.Builder, crd *apiextensionsv1.CustomResourceDefinition) {
	names := crd.Spec.Names
	// The vendored CRDs each carry one version; take the first.
	version := crd.Spec.Versions[0]
	spec, ok := version.Schema.OpenAPIV3Schema.Properties["spec"]
	if !ok {
		return
	}
	fmt.Fprintf(b, "## %s\n", names.Kind)
	fmt.Fprintf(b, "- resource `%s.%s`, apiVersion `%s/%s`, scope %s\n",
		names.Plural, crd.Spec.Group, crd.Spec.Group, version.Name, crd.Spec.Scope)
	if len(spec.Required) > 0 {
		fmt.Fprintf(b, "- spec requires: %s\n", strings.Join(spec.Required, ", "))
	}
	b.WriteString("\n")
	walkContainer(b, spec, "spec", 0)
	b.WriteString("\n")
}

// walkContainer emits one row per property of an object / array-of-object
// schema at prefix and recurses one level into container children (so
// spec.resources.* and spec.storage.* are visible, but the file stays shallow).
func walkContainer(b *strings.Builder, schema apiextensionsv1.JSONSchemaProps, prefix string, level int) {
	if len(schema.Properties) == 0 {
		return
	}
	names := make([]string, 0, len(schema.Properties))
	for name := range schema.Properties {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		prop := schema.Properties[name]
		path := prefix + "." + name
		fmt.Fprintf(b, "- `%s` — %s\n", path, describe(prop, contains(schema.Required, name)))
		if level >= 1 {
			continue
		}
		if sub, ok := arrayItemSchema(prop); ok {
			walkContainer(b, sub, path+"[]", level+1)
		} else if len(prop.Properties) > 0 {
			walkContainer(b, prop, path, level+1)
		}
	}
}

// arrayItemSchema returns the object schema of an array-of-object property.
func arrayItemSchema(prop apiextensionsv1.JSONSchemaProps) (apiextensionsv1.JSONSchemaProps, bool) {
	if prop.Type == "array" && prop.Items != nil && prop.Items.Schema != nil &&
		len(prop.Items.Schema.Properties) > 0 {
		return *prop.Items.Schema, true
	}
	return apiextensionsv1.JSONSchemaProps{}, false
}

// describe renders "type · required · default · enum · min" for one field.
func describe(prop apiextensionsv1.JSONSchemaProps, required bool) string {
	parts := []string{typeLabel(prop)}
	if required {
		parts = append(parts, "required")
	}
	if len(prop.Enum) > 0 {
		parts = append(parts, "enum: "+joinRaw(prop.Enum))
	}
	if prop.Default != nil {
		parts = append(parts, "default: "+trimRaw(prop.Default.Raw))
	}
	if prop.Minimum != nil {
		parts = append(parts, "min "+trimFloat(*prop.Minimum))
	}
	return strings.Join(parts, " · ")
}

func typeLabel(prop apiextensionsv1.JSONSchemaProps) string {
	if prop.Type == "object" && len(prop.Properties) == 0 {
		return "object"
	}
	if prop.Type == "" {
		if len(prop.Properties) > 0 {
			return "object"
		}
		return "?"
	}
	t := prop.Type
	if prop.Format != "" {
		t = fmt.Sprintf("%s (%s)", t, prop.Format)
	}
	return t
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// joinRaw joins JSON-scalar raw values (enum entries) comma-separated, with
// string quotes stripped (e.g. ["\"ssh\"","\"vscode\""] -> "ssh, vscode").
func joinRaw(vals []apiextensionsv1.JSON) string {
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		out = append(out, trimRaw(v.Raw))
	}
	return strings.Join(out, ", ")
}

func trimRaw(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

func trimFloat(f float64) string {
	if f == float64(int64(f)) {
		return fmt.Sprintf("%d", int64(f))
	}
	return fmt.Sprintf("%g", f)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/skill/cubestackgen/ -v`
Expected: PASS — both renderer tests. If a substring assert fails, look at the actual render (add a temporary `t.Logf(doc)`) — the map must reflect the *current* vendored CRDs; adjust the asserted substring to match only if the generator is producing something objectively wrong (e.g. a missing kind).

- [ ] **Step 5: Commit**

```bash
git add internal/skill/cubestackgen/
git commit -s -m "feat(skills): add cubestackgen CRD-map generator library (issue #98)

Parses the vendored CubeStack CRDs into the markdown used by the
cubestack-platform skill's crd-reference.md. Shared renderer means the
committed file and the freshness guard can never diverge.

Assisted-by: Claude Code"
```

---

### Task 2: Add the builtin skill (SKILL.md + generated crd-reference.md) and all wiring

Registers the third builtin skill end to end: two skill files, the generator
command + Makefile target that produces `crd-reference.md`, the broadened
`go:embed`, and every place that pins the builtin set.

**Files:**
- Create: `internal/skill/skills/cubestack-platform/SKILL.md`
- Create: `internal/skill/skills/cubestack-platform/crd-reference.md` (generated in Step 8)
- Create: `hack/gen-cubestack-skill/main.go`
- Modify: `internal/skill/skill_source.go` (embed pattern)
- Modify: `internal/skill/skill_source_test.go` (want map)
- Modify: `internal/store/store.go:175-186`
- Modify: `web/src/views/AgentView.tsx:9-13`
- Modify: `workspace/AGENTS.md` (capability list + principle 6)
- Modify: `docs/cubepilot/api.md` (lines ~193, 234, 252)
- Modify: `Makefile` (variable + target + .PHONY)

**Interfaces:**
- Consumes: `cubestackgen.RenderDocFromDir` (Task 1), vendored CRD YAMLs.
- Produces: builtin set `{cluster-inspection, kubectl-platform, cubestack-platform}`; a `make update-cubestack-skill` target; agent-facing SKILL.md + crd-reference.md consumed by Task 3's guard tests.

- [ ] **Step 1: Write the failing builtin-set test**

In `internal/skill/skill_source_test.go`, change the `want` map in `TestBuiltinSkillNames` (lines ~13-18) to:

```go
	want := map[string]bool{
		"cluster-inspection": true,
		"kubectl-platform":   true,
		"cubestack-platform": true,
	}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/skill/ -run TestBuiltinSkillNames -v`
Expected: FAIL — `builtin names = [cluster-inspection kubectl-platform], want 3` (the new dir does not exist yet).

- [ ] **Step 3: Broaden the go:embed pattern**

In `internal/skill/skill_source.go`, change:

```go
//go:embed skills/*/SKILL.md
```

to:

```go
//go:embed all:skills/*
```

and update the comment on the line above it from "Each skills/<name>/ is the" to keep it accurate (the embed now includes each skill's supporting files, e.g. `cubestack-platform/crd-reference.md`):

```go
// skillsFS embeds the preset skill directories. Each skills/<name>/ is the
// single source of truth for one builtin skill (design §3.4): the API seeds it
// into the repository at startup, and the operator references the names. The
// whole directory is embedded so a skill may carry supporting files (e.g.
// crd-reference.md) alongside its SKILL.md.
```

- [ ] **Step 4: Author the narrative SKILL.md**

Create `internal/skill/skills/cubestack-platform/SKILL.md`:

```markdown
---
name: cubestack-platform
description: "Use when operating the CubeStack platform — creating, inspecting or connecting to ai.cubestack.io resources (DevEnvironment / InferenceService / ModelVersion / InferenceRuntimeProfile). Gives the generated CRD map (crd-reference.md) plus a known-good DevEnvironment manifest and usage guidance, so you do not guess ai.cubestack.io schemas via repeated kubectl apply --dry-run=server"
---

# CubeStack Platform Usage

This cluster exposes the CubeStack platform under the `ai.cubestack.io` group:
dev machines (`DevEnvironment`), model serving (`InferenceService` +
`InferenceRuntimeProfile`), and the model catalog (`ModelVersion`). Read this
skill before creating any such resource.

The per-kind schema map — required fields, defaults, enums and types — is
generated and lives in **`crd-reference.md`** in this skill directory. Open it
and trust it over guessing; it is regenerated from the exact CRDs this
environment installs (`make update-cubestack-skill`).

## Creating a DevEnvironment (known-good example)

A `DevEnvironment` is a containerized dev machine ("开发机"). Key semantics:

- `spec.type` picks the container entry — `ssh` (default), `jupyter`, or
  `vscode`.
- `spec.running` (default `false`) is the desired state: `true` = Running,
  `false` = Stopped. Omit it unless you must start the machine now.
- `spec.resources` is **flat** — `cpu`/`memory` are top-level strings here, NOT
  a k8s `requests`/`limits` map. `gpuCount` is an integer (≥ 1, default 1);
  `gpuType` is `nvidia` (default) or `metax`.
- Omit `spec.storage` to skip a managed workspace PVC (default 10Gi mounted at
  `/workspace` when present); omit `spec.volumes` unless you mount an existing
  PVC as the workspace.

A minimal manifest matching "create a dev machine with N CPU / M memory and
image X in namespace Y" (created Stopped, no extra storage):

```yaml
apiVersion: ai.cubestack.io/v1alpha1
kind: DevEnvironment
metadata:
  name: dev-cuda
  namespace: default
spec:
  image: pytorch/pytorch:2.3.1-cuda12.1-cudnn8-runtime
  resources:
    cpu: "4"
    memory: 16Gi
```

Add `running: true` (and, for a persistent workspace,
`storage: {size: 10Gi}`) to actually start it.

## After creation: reading the resource back

A DevEnvironment may exist while still Stopped/Pending. Read `status` to know
what to hand the user:

- `status.phase.name`: `Pending` / `Running` / `Stopped` / `Failed` /
  `Terminating`. `status.conditions` (e.g. `PodScheduled`, `StorageReady`,
  `Ready`) explains why.
- `status.endpoints` lists access addresses once Running — Jupyter as a URL,
  SSH as `host:port`, and extra `ports[].name` entries likewise. Report these
  to the user rather than inventing URLs.
- SSH access only works if the image runs an sshd and the environment exposes
  it (the `ssh` type exposes SSH by default).

## Other kinds

- `InferenceRuntimeProfile` — a serving profile: an `engine`, the `roles`
  workloads, `endpoint` selection, `modelRequirements`, and any
  user-adjustable `overrides`. Usually created by an admin first.
- `ModelVersion` — a model artifact: `model` + `version` identify it;
  `storage` says where it lives; `architecture` / `quantization` describe it.
- `InferenceService` — the running service: reference a `modelRef`
  (ModelVersion) and a `profileRef` (InferenceRuntimeProfile); the controller
  reconciles the workload from the profile.

Required fields and defaults for each kind are in `crd-reference.md`.

## Common mistakes

- Wrapping `spec.resources` in `requests`/`limits` — it is flat; unknown
  fields are rejected by the API server.
- Passing `cpu`/`memory` as numbers — they are strings (`cpu: "4"`,
  `memory: 16Gi`).
- Using the wrong group/version — everything here is `ai.cubestack.io/v1alpha1`.
- Assuming `running` is implicit — a DevEnvironment is created Stopped unless
  you set `running: true`.
- Guessing kinds the map does not cover — fall back to the `kubectl-platform`
  skill's generic schema-discovery recipe instead.
```

- [ ] **Step 5: Run the builtin-set test to verify it passes**

Run: `go test ./internal/skill/ -run 'TestBuiltinSkillNames|TestPackBuiltinSkill' -v`
Expected: PASS — names = 3, and the new skill packs with non-empty displayName/description.

- [ ] **Step 6: Add the generator command**

Create `hack/gen-cubestack-skill/main.go`:

```go
// Command gen-cubestack-skill regenerates the builtin cubestack-platform
// skill's crd-reference.md from the vendored CubeStack CRD YAMLs. Wired to
// `make update-cubestack-skill`; the committed output is guarded by the
// freshness test in internal/skill/cubestackgen.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/suanova/cubepilot/internal/skill/cubestackgen"
)

func main() {
	crdDir := flag.String("crd-dir", "test/e2e/framework/testdata/cubestack-crds",
		"directory holding the vendored CubeStack CRD YAMLs")
	out := flag.String("out", "internal/skill/skills/cubestack-platform/crd-reference.md",
		"path to write the generated crd-reference.md")
	flag.Parse()

	doc, err := cubestackgen.RenderDocFromDir(*crdDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gen-cubestack-skill: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, []byte(doc), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "gen-cubestack-skill: write %s: %v\n", *out, err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (%d bytes)\n", *out, len(doc))
}
```

- [ ] **Step 7: Add the Makefile target**

In `Makefile`:

1. After the `CUBESTACK_CRD_BASES` line (~line 40), add:

```make
# Generated crd-reference.md of the cubestack-platform builtin skill.
CUBESTACK_SKILL_REF ?= internal/skill/skills/cubestack-platform/crd-reference.md
```

2. Add `update-cubestack-skill` to the `.PHONY` line (~line 50):

```make
.PHONY: build test test-e2e web images push update-crds update-cubestack-skill lint deploy undeploy clean
```

3. Add a target after the `update-crds` recipe block (~line 103):

```make
## Regenerate the builtin cubestack-platform skill's crd-reference.md from the
## vendored CubeStack CRDs (run `make update-crds` first when upstream changed).
update-cubestack-skill:
	$(GO) run ./hack/gen-cubestack-skill -crd-dir $(CUBESTACK_CRD_DIR) -out $(CUBESTACK_SKILL_REF)
```

- [ ] **Step 8: Generate crd-reference.md**

Run: `make update-cubestack-skill`
Expected: `wrote internal/skill/skills/cubestack-platform/crd-reference.md (NNNN bytes)`. Spot-check:

```bash
head -20 internal/skill/skills/cubestack-platform/crd-reference.md
grep -c '^## ' internal/skill/skills/cubestack-platform/crd-reference.md
grep 'spec requires: image, resources' internal/skill/skills/cubestack-platform/crd-reference.md
```

Expected: 4 `## ` sections; the DevEnvironment `spec requires: image, resources` line present.

- [ ] **Step 9: Update the store default agent config**

In `internal/store/store.go` `DefaultAgentConfig` (lines ~175-186), add the new skill to the toggle list:

```go
func DefaultAgentConfig() AgentConfig {
	return AgentConfig{
		Model: "deepseek-v4-flash",
		Skills: []SkillToggle{
			{Name: "kubectl-platform", Enabled: true},
			{Name: "cluster-inspection", Enabled: true},
			{Name: "cubestack-platform", Enabled: true},
		},
	}
}
```

- [ ] **Step 10: Update the web skill label map**

In `web/src/views/AgentView.tsx` (`SKILL_LABELS`, lines ~9-13), add the label:

```ts
const SKILL_LABELS: Record<string, string> = {
  'kubectl-platform': 'Platform Resource Operations',
  'cluster-inspection': 'Smart Inspection',
  'cubestack-platform': 'CubeStack Platform',
}
```

- [ ] **Step 11: Update the agent persona (workspace/AGENTS.md)**

1. Add a capability bullet under `## 能力目录（Skills）`:

```markdown
- `cubestack-platform`：CubeStack 平台资源（`ai.cubestack.io` 组）的 schema 速查与使用指南——含 `crd-reference.md` 生成的各 CR 必填/默认/枚举，及已知可用的 DevEnvironment 清单。
```

2. Rewrite principle 6 (`## 执行原则`):

```markdown
6. **平台 CRD 先查 `cubestack-platform`**：操作 `ai.cubestack.io` 组 CRD（如 DevEnvironment / InferenceService）前，先查阅 `cubestack-platform` skill（`crd-reference.md` 的 schema 速查与已知可用清单），不要从零 `dry-run` 猜字段。仅当该 kind 不在其速查范围内时，才回退到 `kubectl-platform` 的通用发现流程（`api-resources` → `explain` / `--dry-run=server` → apply）。
```

- [ ] **Step 12: Update the API docs examples**

In `docs/cubepilot/api.md`, update the three skill examples (lines ~193, 234, 252) to include the new name — each occurrence of the two-skill list becomes the three-skill list. Apply these three replacements:

Line 193:
```text
    "skills": ["kubectl-platform", "cluster-inspection", "cubestack-platform"],
```
Line 234:
```text
    "enabledSkills": ["kubectl-platform", "cluster-inspection", "cubestack-platform"],
```
Line 252:
```text
{ "spec": { "selectedModel": "qwen2.5-72b", "enabledSkills": ["kubectl-platform", "cluster-inspection", "cubestack-platform"], "userInstructions": "…" } }
```

- [ ] **Step 13: Verify everything compiles and unit tests pass**

Run:

```bash
go vet ./internal/... ./hack/...
go test ./cmd/... ./internal/...
cd web && npx tsc --noEmit && cd ..
```

Expected: `go vet` clean; every `ok` line; `tsc` reports `0 errors`. (`go vet ./...` covers `hack/gen-cubestack-skill` too.)

- [ ] **Step 14: Commit**

```bash
git add internal/skill/skills/cubestack-platform internal/skill/skill_source.go internal/skill/skill_source_test.go internal/store/store.go web/src/views/AgentView.tsx workspace/AGENTS.md docs/cubepilot/api.md Makefile hack/gen-cubestack-skill
git commit -s -m "feat(skills): add builtin cubestack-platform skill (issue #98)

Third builtin skill: hand-authored SKILL.md usage guide plus a generated
crd-reference.md (schema map from the vendored CubeStack CRDs via
make update-cubestack-skill). Embed broadens to all:skills/* so the
supporting file ships; wiring (store defaults, web labels, persona,
api.md) updated.

Assisted-by: Claude Code"
```

---

### Task 3: Guard tests — freshness + example-manifest structure

Two unit tests that make future schema drift impossible and keep the narrative
example honest. Both live next to the generator so they reuse `LoadDir`.

**Files:**
- Modify: `internal/skill/cubestackgen/cubestackgen_test.go`

**Interfaces:**
- Consumes: `RenderDocFromDir`, `LoadDir`, committed `SKILL.md` + `crd-reference.md` (Task 2), vendored CRDs.
- Produces: CI gate — `TestCommittedCRDReferenceIsFresh` fails with "run make update-cubestack-skill" on any staleness; `TestSKILLExampleManifestValid` fails if the SKILL.md DevEnvironment example violates a vendored CRD.

- [ ] **Step 1: Write the failing freshness test (demonstrate RED)**

Append to `internal/skill/cubestackgen/cubestackgen_test.go`:

```go
func TestCommittedCRDReferenceIsFresh(t *testing.T) {
	committed := "../../../internal/skill/skills/cubestack-platform/crd-reference.md"
	want, err := RenderDocFromDir(testCRDDir)
	if err != nil {
		t.Fatalf("RenderDocFromDir: %v", err)
	}
	got, err := os.ReadFile(committed)
	if err != nil {
		t.Fatalf("read committed crd-reference.md: %v (run make update-cubestack-skill)", err)
	}
	if string(got) != want {
		t.Fatalf("committed crd-reference.md is stale\nrun: make update-cubestack-skill")
	}
}
```

Add `"os"` to the test file's imports.

To prove the guard bites, temporarily break the committed file, then restore it:

```bash
echo "stale" >> internal/skill/skills/cubestack-platform/crd-reference.md
go test ./internal/skill/cubestackgen/ -run TestCommittedCRDReferenceIsFresh -v
git checkout -- internal/skill/skills/cubestack-platform/crd-reference.md
```

Expected: the test FAILS while the file is touched, then PASSES after the restore.

- [ ] **Step 2: Write the example-manifest structural test**

Append to `internal/skill/cubestackgen/cubestackgen_test.go`:

```go
func TestSKILLDevEnvironmentExampleIsValid(t *testing.T) {
	skill, err := os.ReadFile("../../../internal/skill/skills/cubestack-platform/SKILL.md")
	if err != nil {
		t.Fatalf("read SKILL.md: %v", err)
	}
	raw, err := extractFencedYAMLContaining(string(skill), "kind: DevEnvironment")
	if err != nil {
		t.Fatalf("extract example: %v", err)
	}
	var obj map[string]any
	if err := yaml.Unmarshal([]byte(raw), &obj); err != nil {
		t.Fatalf("decode example: %v", err)
	}
	crds, err := LoadDir(testCRDDir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	var specSchema apiextensionsv1.JSONSchemaProps
	for _, crd := range crds {
		if crd.Spec.Names.Kind != "DevEnvironment" {
			continue
		}
		specSchema = crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	}
	spec, ok := obj["spec"].(map[string]any)
	if !ok {
		t.Fatalf("example must carry a spec object")
	}
	if errs := validateRequiredAndEnums(specSchema, spec, "spec"); len(errs) > 0 {
		t.Fatalf("example violates the DevEnvironment CRD:\n  %s", strings.Join(errs, "\n  "))
	}
}

// extractFencedYAMLContaining returns the first fenced ``` block whose body
// contains needle (used to locate the known-good example in SKILL.md).
func extractFencedYAMLContaining(md, needle string) (string, error) {
	lines := strings.Split(md, "\n")
	in := false
	var cur []string
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			if !in {
				in = true
				cur = nil
				continue
			}
			if text := strings.Join(cur, "\n"); strings.Contains(text, needle) {
				return text, nil
			}
			in = false
			continue
		}
		if in {
			cur = append(cur, line)
		}
	}
	return "", fmt.Errorf("no fenced block containing %q found", needle)
}

// validateRequiredAndEnums checks that every field the schema marks required is
// present, and that any provided leaf matching an enum-valued schema field is
// one of the enum entries. Recurses into nested object values.
func validateRequiredAndEnums(schema apiextensionsv1.JSONSchemaProps, obj map[string]any, path string) []string {
	var errs []string
	for _, req := range schema.Required {
		if _, ok := obj[req]; !ok {
			errs = append(errs, fmt.Sprintf("%s: missing required field %q", path, req))
		}
	}
	for name, val := range obj {
		prop, ok := schema.Properties[name]
		if !ok {
			continue
		}
		if len(prop.Enum) > 0 {
			vs := fmt.Sprint(val)
			legal := false
			for _, e := range prop.Enum {
				if trimRaw(e.Raw) == vs {
					legal = true
					break
				}
			}
			if !legal {
				errs = append(errs, fmt.Sprintf("%s.%s: value %q not in enum %v", path, name, vs, prop.Enum))
			}
		}
		if child, ok := val.(map[string]any); ok {
			errs = append(errs, validateRequiredAndEnums(prop, child, path+"."+name)...)
		}
	}
	return errs
}
```

After appending both Task-3 test functions, the test file's import block (merged with the Task-1 imports `strings`, `testing`) must be exactly:

```go
import (
	"fmt"
	"os"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)
```

- [ ] **Step 3: Run the guard tests to verify they pass**

Run: `go test ./internal/skill/cubestackgen/ -v`
Expected: PASS — freshness matches the committed file; the DevEnvironment example satisfies the vendored CRD's required fields and enums.

- [ ] **Step 4: Commit**

```bash
git add internal/skill/cubestackgen/cubestackgen_test.go
git commit -s -m "test(skills): guard cubestack-platform freshness + example validity (issue #98)

Freshness regenerates crd-reference.md in memory and fails when the
committed file is stale, so a future make update-crds bump cannot land
without regenerating the skill. The example check keeps the SKILL.md
DevEnvironment manifest structurally valid against the vendored CRD.

Assisted-by: Claude Code"
```

---

### Task 4: Repo-level maintenance skill for the refresh runbook

Encodes the "who keeps this fresh" story as a Claude Code skill for repo
developers/agents (NOT a platform skill — it must not live under
`internal/skill/skills/`). This is the first entry in the repo's
`.claude/skills/`.

**Files:**
- Create: `.claude/skills/refresh-cubestack-platform-skill/SKILL.md`

**Interfaces:**
- Consumes: `make update-crds` / `make update-cubestack-skill`, guard tests in `internal/skill/cubestackgen`.
- Produces: a runbook that future agents (or the human) follow instead of hand-editing the generated file.

- [ ] **Step 1: Author the maintenance skill**

Create `.claude/skills/refresh-cubestack-platform-skill/SKILL.md`:

```markdown
---
name: refresh-cubestack-platform-skill
description: Use when the cubestack-platform freshness guard test fails, or when CubeStack CRD types / platform runtime behavior change, to regenerate or edit the builtin cubestack-platform skill (crd-reference.md is generated; SKILL.md is narrative)
---

# Refresh the builtin cubestack-platform skill

The builtin `cubestack-platform` skill (issue #98) has two parts:

- `internal/skill/skills/cubestack-platform/crd-reference.md` — **generated**.
  Do not hand-edit it; the freshness guard
  (`internal/skill/cubestackgen/cubestackgen_test.go:TestCommittedCRDReferenceIsFresh`)
  fails CI when it disagrees with the generator.
- `internal/skill/skills/cubestack-platform/SKILL.md` — hand-authored narrative
  (usage guide, known-good example, common mistakes).

## When to use this skill

- The freshness guard test fails.
- CubeStack's CRD types changed upstream (you just bumped the vendored CRDs).
- CubeStack's runtime behavior changed (narrative facts are now wrong).

## Runbook

1. If upstream CubeStack CRDs changed, refresh the vendored snapshots:
   `make update-crds`
2. Regenerate the schema map: `make update-cubestack-skill`
3. If runtime behavior changed, edit the narrative in `SKILL.md`, sourcing facts
   from the CubeStack operator code / CRD field docs. Never put facts in
   `crd-reference.md` that belong in SKILL.md.
4. Run the guards: `go test ./internal/skill/cubestackgen/ ./internal/skill/`
5. Review the diff; in the commit message, note which CubeStack behavior/CRD
   change the update tracks.
```

- [ ] **Step 2: Verify the skill file is well-formed**

Read `.claude/skills/refresh-cubestack-platform-skill/SKILL.md` and confirm the frontmatter has `name` and `description` and the markdown body is well-formed.

- [ ] **Step 3: Commit**

```bash
git add .claude/skills/refresh-cubestack-platform-skill/SKILL.md
git commit -s -m "docs(skills): add repo maintenance skill for cubestack-platform refresh (issue #98)

Encodes the runbook (make update-crds -> update-cubestack-skill -> edit
narrative -> guards) so refreshing the builtin skill is agent-driven, not
rote manual upkeep.

Assisted-by: Claude Code"
```

---

### Task 5: Full-suite verification and finalize the PR

- [ ] **Step 1: Run the full unit + vet gate**

Run:

```bash
go vet ./...
go test ./cmd/... ./internal/...
```

Expected: vet clean, all packages `ok` (including `internal/skill/cubestackgen`).

- [ ] **Step 2: Compile the e2e suite binary (chat spec is compile-only here)**

Run: `go test -c ./test/e2e -o bin/e2e.test`
Expected: binary produced, no errors. (Before this, make sure the working file
`test/e2e/cubestack_chat_test.go` is not left with the accidental `  := false`
syntax corruption seen in the main clone — fix/restore if present in this
worktree so the suite compiles.)

- [ ] **Step 3: Type-check the web app**

Run: `cd web && npx tsc --noEmit && cd ..`
Expected: `0 errors`.

- [ ] **Step 4: Review the generated file reads correctly**

Read `internal/skill/skills/cubestack-platform/crd-reference.md` and confirm:
4 `## ` kind sections, the DevEnvironment `spec requires: image, resources`
line, and the `spec.resources.cpu`/`memory` rows show type `string` (the
flat-not-requests/limits trap is visible from the map).

- [ ] **Step 5: Final review of the skill files**

Read `internal/skill/skills/cubestack-platform/SKILL.md` once more: description
is third-person "Use when...", the known-good example matches the chat e2e
prompt shape, no "development status / implemented?" ledger anywhere.

- [ ] **Step 6: Final commit (if any review fixes were applied)**

```bash
git add -A
git commit -s -m "fix(skills): final review fixes for cubestack-platform skill (issue #98)

Assisted-by: Claude Code"
```

(If nothing changed, skip.)

- [ ] **Step 7: Push and open the PR (upstream-fork-pr workflow)**

```bash
git push -u origin HEAD
gh pr create -R suanova/cubepilot --base main --head zhujian7:feat-issue98-cubestack-platform-skill \
  --title "feat: builtin cubestack-platform skill — CubeStack usage map (issue #98)" \
  --body "Closes #98."
```

Then note the live-cluster e2e: `cubestack_chat_test.go` (the chat behavior
gate) needs a real LLM key + cluster (`CUBEPILOT_E2E_CHAT=1`) and cannot run in
CI — flag it for a live run in the PR body before merge, as in #86. The skill
is enabled by default (empty `enabledSkills` = all), so no per-instance change
is required.
