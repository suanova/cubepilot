# Issue #86 — e2e: chat creates DevEnvironment via generic CRD discovery (drop dev-environment/inference-service skills) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `chat`-labeled e2e spec in which the agent creates a `DevEnvironment` **purely through generic schema discovery** against cubestack CRDs (installed by the spec), and drop the stale `dev-environment` / `inference-service` skills so the builtin seed registers only `cluster-inspection` + `kubectl-platform`.

**Architecture:** The builtin skill set is derived at build time from the embedded dirs under `internal/skill/skills/` (`skill.BuiltinSkillNames()`) — deleting the two dirs propagates automatically (AgentTemplate.Skills, API startup seed → Skill CRDs, supervisor HTTP pull). The agent's write path is `exec kubectl` against `ai.cubestack.io`, where the agent SA already has wildcard RBAC; generic discovery (api-resources → explain / `--dry-run=server` → apply) is taught in the `kubectl-platform` skill + AGENTS.md. The e2e spec vendors the four cubestack CRD YAMLs into `test/e2e/framework/testdata`, installs them via the apiextensions client, chats via the existing `ChatSSE` helper, and asserts the created CR via a new framework `DynamicClient`.

**Tech Stack:** Go, Ginkgo v2 / Gomega, controller-runtime client, client-go `dynamic`, apiextensions clientset, `sigs.k8s.io/yaml`.

## Global Constraints

- CRD group `ai.cubestack.io` / version `v1alpha1`. `DevEnvironment` requires `spec.image` + `spec.resources`; `InferenceService` requires `spec.modelRef` + `spec.profileRef`.
- Chat specs must `Skip` unless `CUBEPILOT_E2E_CHAT == "1"` and be tagged `Label("chat")` (CI runs `-ginkgo.label-filter='!chat'` without a key).
- Never hardcode the builtin skill list — always derive from `skill.BuiltinSkillNames()` (it reads the embedded dirs). Tests that pin the set are updated, not duplicated.
- Keep `cluster-inspection` (backs the `daily-inspection` TaskTemplate, anchor 2) and `kubectl-platform` (the generic executor). Only `dev-environment` + `inference-service` are removed.
- No RBAC change: the `cubepilot-agent` SA already has wildcard verbs on apiGroup `ai.cubestack.io` (`deploy/charts/cubepilot/templates/rbac.yaml:180-183`). The agent SA has **no** `apiextensions.k8s.io` access, so `kubectl get crd` is RBAC-denied — the discovery recipe must fall back to `kubectl explain` / server-side `--dry-run=server`.
- Commits: `git commit -s` (signoff) and append `Assisted-by: Claude Code`.
- Code/comments in English.

> **Revision (post-implementation feedback):** The four CubeStack CRDs are provisioned by the TEST as a precondition, not by the platform deploy — cubepilot must not deploy/manage CubeStack CRDs (the CubeStack operator does, in production). They are vendored under `test/e2e/framework/testdata/cubestack-crds/` and refreshed via `make update-crds`. The chat spec (Task 4) installs only CRDs that are absent (create-if-absent), deletes only the ones it created (pre-existing CRDs are left alone), and chats with a natural-language user prompt (no CRD / kubectl / discovery jargon), asserting the requested name/image/cpu/memory.

---

### Task 0: Save and commit the implementation plan

**Files:**
- Create: `docs/superpowers/plans/2026-09-02-e2e-crd-discovery.md` (this file)

- [ ] **Step 1: Commit the plan doc**

```bash
git add docs/superpowers/plans/2026-09-02-e2e-crd-discovery.md
git commit -s -m "docs: add e2e generic CRD discovery implementation plan (issue #86)

Assisted-by: Claude Code"
```

---

### Task 1: Drop the `dev-environment` / `inference-service` skills and update all references

Removes the two stale CRD skills. The builtin seed then registers only `cluster-inspection` + `kubectl-platform`. Unit-testable end-to-end.

**Files:**
- Delete: `internal/skill/skills/dev-environment/`, `internal/skill/skills/inference-service/`
- Modify: `internal/skill/skill_source_test.go:13-18`
- Modify: `internal/store/store.go:178-186`
- Modify: `internal/resolver/resolver_test.go:181`
- Modify: `web/src/views/AgentView.tsx:9-13`
- Modify: `docs/cubepilot/api.md:193,234,252`
- Modify: `workspace/AGENTS.md:9-13` (capability list — remove the two, rename `inspection` → `cluster-inspection`)

**Interfaces:**
- Consumes: `skill.BuiltinSkillNames()` (reads embedded `skills/*` dirs) — unchanged signature.
- Produces: a builtin set of exactly `{cluster-inspection, kubectl-platform}`; Task 2 adds discovery content to `kubectl-platform`'s SKILL.md.

- [ ] **Step 1: Write the failing test — shrink the expected builtin set**

In `internal/skill/skill_source_test.go`, change the `want` map (lines 13-18) to:

```go
	want := map[string]bool{
		"cluster-inspection": true,
		"kubectl-platform":   true,
	}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/skill/ -run TestBuiltinSkillNames -v`
Expected: FAIL — `builtin names = [cluster-inspection dev-environment inference-service kubectl-platform], want 2` (the two dirs still exist).

- [ ] **Step 3: Delete the two skill directories**

```bash
git rm -r internal/skill/skills/dev-environment internal/skill/skills/inference-service
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/skill/ -run TestBuiltinSkillNames -v`
Expected: PASS — the embedded set is now exactly `cluster-inspection` + `kubectl-platform`. (The `//go:embed skills/*/SKILL.md` pattern still matches the two remaining dirs; `TestPackBuiltinSkill` covers them.)

- [ ] **Step 5: Update `internal/store/store.go` default agent config**

Replace lines 178-186 (`DefaultAgentConfig`) with:

```go
func DefaultAgentConfig() AgentConfig {
	return AgentConfig{
		Model: "deepseek-v4-flash",
		Skills: []SkillToggle{
			{Name: "kubectl-platform", Enabled: true},
			{Name: "cluster-inspection", Enabled: true},
		},
	}
}
```

(`dev-environment`/`inference-service` removed; `inspection` renamed to the real skill name `cluster-inspection`.)

- [ ] **Step 6: Update the resolver test fixture**

In `internal/resolver/resolver_test.go:181`, change:

```go
		skillWithSource("dev-environment", "skills/dev-environment/v1.tar.gz"),
```

to:

```go
		skillWithSource("kubectl-platform", "skills/kubectl-platform/v1.tar.gz"),
```

(The test only verifies the `enabledSkills` subset filter; any second skill name works.)

- [ ] **Step 7: Update the web skill-label map**

In `web/src/views/AgentView.tsx` (lines 9-13), replace the `SKILL_LABELS` map with:

```ts
const SKILL_LABELS: Record<string, string> = {
  'kubectl-platform': 'Platform Resource Operations',
  'cluster-inspection': 'Smart Inspection',
}
```

(`dev-environment`/`inference-service` removed; `inspection` key fixed to the real skill name.)

- [ ] **Step 8: Update the API docs examples**

In `docs/cubepilot/api.md`:
- Line 193: `"skills": ["dev-environment", "inference-service", "cluster-inspection"],` → `"skills": ["kubectl-platform", "cluster-inspection"],`
- Line 234: `"enabledSkills": ["dev-environment", "inference-service"],` → `"enabledSkills": ["kubectl-platform", "cluster-inspection"],`
- Line 252: `"enabledSkills": ["dev-environment", "inference-service", "cluster-inspection"]` → `"enabledSkills": ["kubectl-platform", "cluster-inspection"]`

- [ ] **Step 9: Update the agent persona capability list**

In `workspace/AGENTS.md`, replace the `## 能力目录（Skills）` list (lines 9-13) with:

```markdown
## 能力目录（Skills）

平台能力以 Skills 形式注入，见 `skills/` 目录。当你需要操作平台资源时，先查阅对应 Skill 了解该能力的用途与调用方式，再据此构造 `kubectl` 命令。主要能力：

- `kubectl-platform`：集群资源（节点/Pod/命名空间/事件）的查询与操作，以及通用 CRD 的 schema 发现。
- `cluster-inspection`：集群健康巡检清单与异常分级。
```

- [ ] **Step 10: Verify everything compiles and unit tests pass**

```bash
go vet ./...
go test ./cmd/... ./internal/...
cd web && npx tsc --noEmit && cd ..
```

Expected: `go vet` clean; every `ok` line; `tsc` reports `0 errors`.

- [ ] **Step 11: Commit**

```bash
git add -A
git commit -s -m "feat(skills): drop stale dev-environment/inference-service skills (issue #86)

The builtin set now derives to cluster-inspection + kubectl-platform;
generic discovery (Task 2) covers all CRDs per design §3.4/§5.3.

Assisted-by: Claude Code"
```

---

### Task 2: Teach generic schema discovery in `kubectl-platform` + AGENTS.md

Content change, plus a guard test that pins it in CI.

**Files:**
- Modify: `internal/skill/skills/kubectl-platform/SKILL.md`
- Modify: `internal/skill/skill_source_test.go` (add `TestKubectlPlatformSkillTeachesDiscovery`)
- Modify: `workspace/AGENTS.md` (add discovery principle)

**Interfaces:**
- Consumes: Task 1's reduced builtin set.
- Produces: agent-facing discovery recipe consumed by Task 4's chat.

- [ ] **Step 1: Write the failing guard test**

Append to `internal/skill/skill_source_test.go`:

```go
// TestKubectlPlatformSkillTeachesDiscovery verifies the built-in generic
// skill teaches schema discovery (design §5.3), so the agent can operate any
// CRD without a per-CRD skill (issue #86).
func TestKubectlPlatformSkillTeachesDiscovery(t *testing.T) {
	raw, err := skillsFS.ReadFile("skills/kubectl-platform/SKILL.md")
	if err != nil {
		t.Fatalf("read kubectl-platform skill: %v", err)
	}
	for _, want := range []string{"api-resources", "--dry-run=server"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("kubectl-platform SKILL.md should mention %q (schema discovery)", want)
		}
	}
}
```

Add `"strings"` to the file's imports if not already present.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/skill/ -run TestKubectlPlatformSkillTeachesDiscovery -v`
Expected: FAIL — `should mention "api-resources"` (the current SKILL.md has neither string).

- [ ] **Step 3: Add the discovery recipe to `kubectl-platform/SKILL.md`**

In `internal/skill/skills/kubectl-platform/SKILL.md`, insert this section between the `## Write Operations` block and `## Principles`:

```markdown
## Schema Discovery (zero-registration CRD support)

Platform CRDs live under the `ai.cubestack.io` group (`DevEnvironment`, `InferenceService`, `ModelVersion`, `InferenceRuntimeProfile`, …). There is no per-CRD skill — discover the kind / group / schema before applying:

1. Find the resource type: `kubectl api-resources | grep -i <keyword>` (e.g. `grep -i deve` → `devenvironments.ai.cubestack.io`).
2. Read the schema if RBAC allows: `kubectl explain devenvironment --api-version=ai.cubestack.io/v1alpha1` (or `kubectl get crd devenvironments.ai.cubestack.io -o yaml`). The agent SA may lack discovery / crd-read permission — then skip to step 3.
3. Validate against the server before creating (needs no extra permission): apply with `--dry-run=server` and iterate on the error — the API server lists the required fields (e.g. `spec.image`, `spec.resources`) and rejects unknown ones:

   ```bash
   kubectl apply --dry-run=server -f - -n <ns> <<'EOF'
   apiVersion: ai.cubestack.io/v1alpha1
   kind: DevEnvironment
   metadata:
     name: dev
   spec:
     image: pytorch/pytorch:2.3.1-cuda12.1-cudnn8-runtime
     resources:
       requests:
         cpu: "4"
         memory: 16Gi
   EOF
   ```

4. Only then apply for real and verify: `kubectl get devenvironments -n <ns>`.
```

- [ ] **Step 4: Run the guard test to verify it passes**

Run: `go test ./internal/skill/ -run TestKubectlPlatformSkillTeachesDiscovery -v`
Expected: PASS.

- [ ] **Step 5: Add the discovery principle to AGENTS.md**

In `workspace/AGENTS.md`, append to the `## 执行原则` numbered list:

```markdown
6. **未知 CRD 先发现**：操作平台 CRD（`ai.cubestack.io` 组，如 DevEnvironment / InferenceService）没有专用 skill——先 `kubectl api-resources` 找 kind/group，再用 `kubectl explain` 或 `kubectl apply --dry-run=server` 确认 schema，然后才 apply（详见 `kubectl-platform` skill）。
```

- [ ] **Step 6: Verify**

```bash
go vet ./internal/skill/... && go test ./internal/skill/
```

Expected: `vet` clean, all `ok`.

- [ ] **Step 7: Commit**

```bash
git add internal/skill/skills/kubectl-platform/SKILL.md internal/skill/skill_source_test.go workspace/AGENTS.md
git commit -s -m "feat(skills): teach generic schema discovery in kubectl-platform (issue #86)

kubectl-platform now covers api-resources -> explain / --dry-run=server ->
apply, so any ai.cubestack.io CRD is operable without a per-CRD skill
(design §5.3).

Assisted-by: Claude Code"
```

---

### Task 3: e2e framework — DynamicClient + vendored cubestack CRDs + install/delete helpers

**Files:**
- Modify: `test/e2e/framework/framework.go`
- Create: `test/e2e/framework/cubestack_crds.go`
- Create: `test/e2e/framework/testdata/cubestack-crds/*.yaml` (4 files)
- Modify: `go.mod` / `go.sum` (promote `sigs.k8s.io/yaml` to a direct dep)

**Interfaces:**
- Consumes: existing `Framework` clients (`ApiExtClient`, `RestConfig`).
- Produces: `Framework.DynamicClient dynamic.Interface`, `(*Framework).InstallCubestackCRDs(ctx)`, `(*Framework).DeleteCubestackCRDs(ctx)` — consumed by Task 4.

- [ ] **Step 1: Vendor the four cubestack CRD YAMLs**

```bash
if [ ! -d /tmp/cubestack ]; then git clone --depth 1 git@github.com:suanova/cubestack.git /tmp/cubestack; fi
mkdir -p test/e2e/framework/testdata/cubestack-crds
cp /tmp/cubestack/operator/config/crd/bases/*.yaml test/e2e/framework/testdata/cubestack-crds/
ls test/e2e/framework/testdata/cubestack-crds/
```

Expected: 4 files — `ai.cubestack.io_devenvironments.yaml`, `ai.cubestack.io_inferenceservices.yaml`, `ai.cubestack.io_inferenceruntimeprofiles.yaml`, `ai.cubestack.io_modelversions.yaml`.

- [ ] **Step 2: Add the DynamicClient to the framework**

In `test/e2e/framework/framework.go`:
- Add import `"k8s.io/client-go/dynamic"`.
- Add field to the `Framework` struct (after `CtrlClient`):

```go
	CtrlClient   crclient.Client
	DynamicClient dynamic.Interface
```

- In `New`, after the controller-runtime client is built (after line 64), add:

```go
	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("dynamic client: %w", err)
	}
```

- In the returned struct literal, add:

```go
		DynamicClient: dc,
```

- [ ] **Step 3: Add the CRD install/delete helpers**

Create `test/e2e/framework/cubestack_crds.go`:

```go
package framework

import (
	"context"
	"embed"
	"fmt"
	"io/fs"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

//go:embed testdata/cubestack-crds/*.yaml
var cubestackCRDs embed.FS

// cubestackCRDNames are the four ai.cubestack.io CRDs vendored from the
// suanova/cubestack operator (operator/config/crd/bases) so the suite can
// install them deterministically (issue #86).
var cubestackCRDNames = []string{
	"devenvironments.ai.cubestack.io",
	"inferenceservices.ai.cubestack.io",
	"inferenceruntimeprofiles.ai.cubestack.io",
	"modelversions.ai.cubestack.io",
}

// InstallCubestackCRDs applies the four cubestack CRDs from the embedded
// testdata. Idempotent: an already-existing CRD is a no-op.
func (f *Framework) InstallCubestackCRDs(ctx context.Context) error {
	entries, err := fs.ReadDir(cubestackCRDs, "testdata/cubestack-crds")
	if err != nil {
		return err
	}
	for _, e := range entries {
		raw, err := cubestackCRDs.ReadFile("testdata/cubestack-crds/" + e.Name())
		if err != nil {
			return err
		}
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := yaml.Unmarshal(raw, crd); err != nil {
			return fmt.Errorf("decode %s: %w", e.Name(), err)
		}
		_, err = f.ApiExtClient.ApiextensionsV1().CustomResourceDefinitions().Create(ctx, crd, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create %s: %w", crd.Name, err)
		}
	}
	return nil
}

// DeleteCubestackCRDs removes the four cubestack CRDs (tolerating absence).
func (f *Framework) DeleteCubestackCRDs(ctx context.Context) error {
	for _, name := range cubestackCRDNames {
		err := f.ApiExtClient.ApiextensionsV1().CustomResourceDefinitions().
			Delete(ctx, name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 4: Promote `sigs.k8s.io/yaml` to a direct dependency**

Run: `go mod tidy`
Expected: only the `// indirect` marker on `sigs.k8s.io/yaml` changes (it becomes a direct dep). If `go.mod`/`go.sum` churn beyond that, inspect and revert unrelated changes.

- [ ] **Step 5: Verify the e2e packages compile**

```bash
go build ./test/e2e/... && go vet ./test/e2e/...
```

Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add test/e2e/framework go.mod go.sum
git commit -s -m "test(e2e): add DynamicClient + vendored cubestack CRD helpers (issue #86)

InstallCubestackCRDs applies the four ai.cubestack.io CRDs from embedded
testdata (sourced from suanova/cubestack), idempotently; DeleteCubestackCRDs
removes them.

Assisted-by: Claude Code"
```

---

### Task 4: e2e spec — chat creates a DevEnvironment via generic discovery

**Files:**
- Create: `test/e2e/cubestack_chat_test.go`

**Interfaces:**
- Consumes: `fw.ChatSSE` (framework/http.go), `fw.DynamicClient`, `fw.InstallCubestackCRDs`, `fw.DeleteCubestackCRDs`, `openclaw.Event*` constants.
- Produces: the `Label("chat")` spec that fulfills #86's acceptance criteria.

- [ ] **Step 1: Write the spec**

Create `test/e2e/cubestack_chat_test.go`:

```go
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/rand"

	"github.com/suanova/cubepilot/internal/openclaw"
)

const (
	devEnvName      = "dev-cuda-e2e"
	devEnvNamespace = "default"
	devEnvImage     = "pytorch/pytorch:2.3.1-cuda12.1-cudnn8-runtime"
)

// devEnvGVR is the DevEnvironment kind under the cubestack ai.cubestack.io
// group (not part of the platform's v1alpha1 scheme, so asserted via the
// dynamic client).
var devEnvGVR = schema.GroupVersionResource{
	Group: "ai.cubestack.io", Version: "v1alpha1", Resource: "devenvironments",
}

var _ = Describe("Chat creates a DevEnvironment via generic CRD discovery", Label("chat"), func() {
	ctx := context.Background()

	BeforeEach(func() {
		if os.Getenv("CUBEPILOT_E2E_CHAT") != "1" {
			Skip("CUBEPILOT_E2E_CHAT != 1 (needs a real LLM key); skipping chat e2e")
		}
		By("installing the cubestack CRDs (ai.cubestack.io)")
		Expect(fw.InstallCubestackCRDs(ctx)).To(Succeed())
	})

	AfterEach(func() {
		By("deleting the DevEnvironment created by the chat")
		err := fw.DynamicClient.Resource(devEnvGVR).Namespace(devEnvNamespace).
			Delete(ctx, devEnvName, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			Fail(fmt.Sprintf("delete DevEnvironment: %v", err))
		}
		Eventually(func() bool {
			_, err := fw.DynamicClient.Resource(devEnvGVR).Namespace(devEnvNamespace).
				Get(ctx, devEnvName, metav1.GetOptions{})
			return apierrors.IsNotFound(err)
		}).Should(BeTrue(), "DevEnvironment should be gone after delete")

		By("removing the cubestack CRDs")
		Expect(fw.DeleteCubestackCRDs(ctx)).To(Succeed())
	})

	It("creates a DevEnvironment from natural language via generic discovery", func() {
		chatCtx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
		defer cancel()

		prompt := fmt.Sprintf(
			"请创建以下开发环境（DevEnvironment）：\n- 名称：%s\n- 命名空间：%s\n- 镜像：%s\n- 资源请求：cpu 4、内存 16Gi\n"+
				"请先用 kubectl api-resources 发现该 CRD 的 kind 与 apiVersion，必要时用 kubectl explain 或 kubectl apply --dry-run=server 确认字段，"+
				"再用 kubectl apply 创建。不要修改名称、命名空间或镜像。",
			devEnvName, devEnvNamespace, devEnvImage)

		events, err := fw.ChatSSE(chatCtx, fw.Users[0], "e2e-"+rand.String(6), prompt)
		Expect(err).NotTo(HaveOccurred())

		// The turn must finish cleanly (message_done with no error).
		var done map[string]any
		foundDone := false
		for _, ev := range events {
			if ev.Event == openclaw.EventMessageDone {
				foundDone = true
				Expect(json.Unmarshal(ev.Data, &done)).To(Succeed())
			}
		}
		Expect(foundDone).To(BeTrue(), "message_done should terminate the turn")
		Expect(done["error"]).To(SatisfyAny(BeNil(), BeEmpty()), "message_done should carry no error")

		// Evidence the agent kubectl-applied the *discovered* kind (generic
		// discovery, not a per-CRD skill).
		var applied bool
		for _, ev := range events {
			if ev.Event == openclaw.EventToolCall &&
				strings.Contains(strings.ToLower(string(ev.Data)), "devenvironment") {
				applied = true
			}
		}
		Expect(applied).To(BeTrue(), "the agent should have kubectl-applied a DevEnvironment (discovery evidence)")

		// The CR must exist with the requested spec.
		Eventually(func() error {
			_, err := fw.DynamicClient.Resource(devEnvGVR).Namespace(devEnvNamespace).
				Get(ctx, devEnvName, metav1.GetOptions{})
			return err
		}).Should(Succeed(), "DevEnvironment %s/%s should exist", devEnvNamespace, devEnvName)

		obj, err := fw.DynamicClient.Resource(devEnvGVR).Namespace(devEnvNamespace).
			Get(ctx, devEnvName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		spec, ok := obj.Object["spec"].(map[string]any)
		Expect(ok).To(BeTrue(), "DevEnvironment should carry a spec")
		Expect(spec["image"]).To(Equal(devEnvImage))
		Expect(spec["resources"]).NotTo(BeNil())
	})
})
```

- [ ] **Step 2: Verify the e2e package compiles**

```bash
go vet ./test/e2e/... && go build ./test/e2e/...
```

Expected: clean.

- [ ] **Step 3: Commit**

```bash
git add test/e2e/cubestack_chat_test.go
git commit -s -m "test(e2e): chat creates DevEnvironment via generic CRD discovery (issue #86)

Installs the four cubestack CRDs, chats with the agent to create a
DevEnvironment (no dev-environment skill present), asserts the CR via the
dynamic client, and cleans up.

Assisted-by: Claude Code"
```

---

### Task 5: Assert the reduced builtin skill set in the e2e bootstrap spec

Tightens the existing bootstrap e2e so the post-removal seed set is verified against the live cluster.

**Files:**
- Modify: `test/e2e/bootstrap_test.go:45-60`

**Interfaces:**
- Consumes: `skill.BuiltinSkillNames()` (Task 1's reduced set).
- Produces: `Label`-independent assertion that a fresh stack seeds exactly the builtin set.

- [ ] **Step 1: Update the "bootstraps builtin skills" spec**

In `test/e2e/bootstrap_test.go`, replace the `It("bootstraps builtin skills and the daily-inspection task template", ...)` body's skill-check block (lines 45-60) with:

```go
	It("bootstraps builtin skills and the daily-inspection task template", func() {
		var list v1alpha1.SkillList
		Eventually(func() error {
			if err := fw.CtrlClient.List(ctx, &list); err != nil {
				return err
			}
			if len(list.Items) == 0 {
				return fmt.Errorf("no builtin skills yet")
			}
			for _, s := range list.Items {
				if s.Labels["cubepilot/builtin"] != "true" {
					return fmt.Errorf("skill %s missing builtin label", s.Name)
				}
			}
			return nil
		}).Should(Succeed())

		// The seeded set must match the embedded builtin set exactly (issue
		// #86: only cluster-inspection + kubectl-platform after the drop).
		want := map[string]bool{}
		for _, n := range skill.BuiltinSkillNames() {
			want[n] = true
		}
		got := map[string]bool{}
		for _, s := range list.Items {
			got[s.Name] = true
		}
		Expect(got).To(Equal(want), "seeded Skill CRDs should equal the embedded builtin set")

		tt := &v1alpha1.TaskTemplate{}
		Eventually(func() error {
			return fw.CtrlClient.Get(ctx, types.NamespacedName{Name: controller.BuiltinTaskTemplateName}, tt)
		}).Should(Succeed())
		Expect(tt.Labels).To(HaveKeyWithValue("cubepilot/builtin", "true"))
	})
```

Add the import `"github.com/suanova/cubepilot/internal/skill"` to `test/e2e/bootstrap_test.go`.

- [ ] **Step 2: Verify it compiles**

```bash
go vet ./test/e2e/... && go build ./test/e2e/...
```

Expected: clean.

- [ ] **Step 3: Commit**

```bash
git add test/e2e/bootstrap_test.go
git commit -s -m "test(e2e): assert seeded Skill CRDs equal the embedded builtin set (issue #86)

Fresh stacks now seed exactly cluster-inspection + kubectl-platform.

Assisted-by: Claude Code"
```

---

### Task 6: Full-suite verification and PR

- [ ] **Step 1: Run the full unit + vet gate**

```bash
go vet ./...
go test ./cmd/... ./internal/...
```

Expected: vet clean, all packages `ok`.

- [ ] **Step 2: Compile the e2e suite binary**

```bash
go test -c ./test/e2e -o bin/e2e.test
```

Expected: binary produced, no errors.

- [ ] **Step 3: Type-check the web app**

```bash
cd web && npx tsc --noEmit && cd ..
```

Expected: `0 errors`.

- [ ] **Step 4: Manual sanity of the discovery recipe text**

Read `internal/skill/skills/kubectl-platform/SKILL.md` and confirm the `## Schema Discovery` section reads correctly and the fenced bash block is well-formed.

- [ ] **Step 5: Note the live-cluster e2e (cannot run here)**

The chat spec (`CUBEPILOT_E2E_CHAT=1`) and the bootstrap skill-set assertion need a deployed stack (`make test-e2e`). They are compile-verified here; run them on a real cluster before merging (or note as a follow-up). Also flag whether `kubectl explain` worked under the agent SA or the `--dry-run=server` fallback kicked in (open item from #86).
