# Agent Allowlist Provenance Split — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop the instance allowlist from freezing itself off the platform builtin on first edit, and move learned `allow-always` grants out of `AgentInstance.spec` into a per-user ConfigMap.

**Architecture:** `allowlist.Effective` becomes an unconditional union of four sources (platform builtin, template, hand-authored instance rules, learned grants) instead of "the instance's own list wins". Learned grants get a new ConfigMap-backed store (`internal/grants`), written only by the API server and read by the resolver.

**Tech Stack:** Go 1.2x, controller-runtime `client.Client` (direct, uncached), `k8s.io/client-go/util/retry`, React 18 + TypeScript (web), Go stdlib `testing` with `fake.NewClientBuilder`.

**Spec:** `docs/superpowers/specs/2026-09-14-allowlist-provenance-design.md` (issue #185)

## Global Constraints

- **Pre-release, no compatibility promised.** No migration path and no fallback branches; existing materialized lists are not split, and pre-existing learned grants are simply lost on upgrade.
- **The v1 API contract is frozen** (PR #181). Every change to `/api/v1/...` shapes must be **additive** — never remove or rename a response field.
- **Displayed strings must not expose internal numbering.** No issue/PR numbers, requirement ids (`FR-M2-005`), milestone labels (`M4`), or roadmap phase names in any user-visible text, front-end or back-end. Such references belong in code comments only.
- **Commits** are English, signed off (`git commit -s`), with an `Assisted-by: Claude Code` trailer.
- **Go code** — comments and identifiers in English.
- Test command for Go: `go test ./internal/<pkg>/... -run <Name> -v`. Full gate before a PR: `go vet ./... && go test ./...`.

---

### Task 1: `allowlist.Effective` becomes an unconditional union

The fork fix. This is the standalone bug fix from the spec and is worth reviewing on its own.

**Files:**
- Modify: `internal/allowlist/allowlist.go:96-106`
- Test: `internal/allowlist/allowlist_test.go` (append)
- Modify: `internal/resolver/resolver.go:218`
- Test: `internal/resolver/resolver_test.go` (append)

**Interfaces:**
- Consumes: nothing (first task).
- Produces: `func Effective(templateAllowlist, instanceAllowlist, grants []v1alpha1.AllowlistRule) []v1alpha1.AllowlistRule` — replaces the old two-argument `Effective(owned, templateAllowlist)`.

- [ ] **Step 1: Write the failing test**

Append to `internal/allowlist/allowlist_test.go`:

```go
// TestEffectiveIsUnionNotOwnership is the regression test for the inherited
// allowlist being frozen on first edit (issue #185). An instance that has its
// own entries must still receive the platform builtin: without this, a later
// hardening of Default() silently does not reach that instance -- which is the
// fail-open direction.
func TestEffectiveIsUnionNotOwnership(t *testing.T) {
	instance := []v1alpha1.AllowlistRule{{Pattern: "helm"}}
	got := Effective(nil, instance, nil)

	if !hasPattern(got, "helm") {
		t.Fatal("instance rule dropped")
	}
	for _, b := range Default() {
		if !hasPattern(got, b.Pattern) {
			t.Errorf("builtin %q dropped for an instance that owns entries", b.Pattern)
		}
	}
}

// TestEffectiveUnionsAllThreeSources covers the template and grants arms, and
// the dedup that Merge already provides across them.
func TestEffectiveUnionsAllThreeSources(t *testing.T) {
	tmpl := []v1alpha1.AllowlistRule{{Pattern: "helm"}}
	instance := []v1alpha1.AllowlistRule{{Pattern: "terraform"}}
	grants := []v1alpha1.AllowlistRule{
		{Pattern: "terraform"}, // duplicate of the instance rule
		{Pattern: "kubectl", ArgPattern: "^apply -f prod.yaml$"}, // same pattern as a builtin, different argPattern
	}
	got := Effective(tmpl, instance, grants)

	for _, want := range []string{"helm", "terraform", "kubectl"} {
		if !hasPattern(got, want) {
			t.Errorf("missing %q", want)
		}
	}
	if n := countPattern(got, "terraform"); n != 1 {
		t.Errorf("terraform appears %d times, want 1", n)
	}
}

func hasPattern(rules []v1alpha1.AllowlistRule, pattern string) bool {
	return countPattern(rules, pattern) > 0
}

func countPattern(rules []v1alpha1.AllowlistRule, pattern string) int {
	n := 0
	for _, r := range rules {
		if r.Pattern == pattern {
			n++
		}
	}
	return n
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/allowlist/... -run TestEffective -v`

Expected: compile failure — `too many arguments in call to Effective`.

- [ ] **Step 3: Rewrite `Effective`**

Replace `internal/allowlist/allowlist.go:96-106` (the whole doc comment and function) with:

```go
// Effective returns the effective allowlist for an instance (issue #185): the
// platform builtin, the template's additions, the instance's hand-authored
// additions and the instance's learned grants, unioned.
//
// There is deliberately no "the instance owns its list" override. The previous
// design returned the instance list *instead of* the union whenever that list
// was non-empty, so the first edit of any kind -- including a removal --
// materialized the then-current builtin into the instance and froze it there.
// A later hardening of Default() then could not reach that instance, which is
// the fail-open direction. A union cannot freeze, for today's writers or any
// added later.
//
// Consequence, accepted deliberately: a builtin entry can no longer be removed
// per instance. AlwaysAsk is the strict posture.
func Effective(templateAllowlist, instanceAllowlist, grants []v1alpha1.AllowlistRule) []v1alpha1.AllowlistRule {
	all := make([]v1alpha1.AllowlistRule, 0, len(templateAllowlist)+len(instanceAllowlist)+len(grants))
	all = append(all, templateAllowlist...)
	all = append(all, instanceAllowlist...)
	all = append(all, grants...)
	return Merge(Default(), all)
}
```

- [ ] **Step 4: Update the resolver call site**

In `internal/resolver/resolver.go`, replace line 218:

```go
	cfg.Allowlist = allowlist.Effective(inst.Spec.Allowlist, tmplAllowlist)
```

with:

```go
	cfg.Allowlist = allowlist.Effective(tmplAllowlist, inst.Spec.Allowlist, nil)
```

(The `nil` third argument becomes the grant list in Task 4.)

Also update the comment above it (currently lines 213-217) to describe the union:

```go
	// Confirmation intent & allowlist (issue #185): an instance override wins
	// over the template default; the effective allowlist is the union of the
	// platform builtin, the template's additions and the instance's own
	// additions. Learned grants are added in Task 4.
```

- [ ] **Step 5: Write the resolver regression test**

Append to `internal/resolver/resolver_test.go`:

```go
// TestResolvedAllowlistKeepsBuiltinsWithInstanceEntries is the resolver-level
// half of the fork regression (issue #185): an instance with its own entries
// must still resolve the platform builtin.
func TestResolvedAllowlistKeepsBuiltinsWithInstanceEntries(t *testing.T) {
	inst := instance("alice", "t1", "")
	inst.Spec.Allowlist = []v1alpha1.AllowlistRule{{Pattern: "helm"}}
	r := testResolver(t, template("t1", nil), inst)

	cfg, err := r.Resolve(context.Background(), "alice", "t1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	byPattern := map[string]bool{}
	for _, rule := range cfg.Allowlist {
		byPattern[rule.Pattern] = true
	}
	if !byPattern["helm"] {
		t.Error("instance rule missing")
	}
	if !byPattern["kubectl"] || !byPattern["ls"] {
		t.Errorf("platform builtin missing from the resolved allowlist: %v", cfg.Allowlist)
	}
}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/allowlist/... ./internal/resolver/... -v 2>&1 | tail -30`

Expected: PASS. Some pre-existing resolver test that asserted the old "owned wins" behaviour may fail here — if so, update that assertion to the union expectation rather than reverting the code, and say which test in the commit message.

- [ ] **Step 7: Commit**

```bash
git add internal/allowlist/allowlist.go internal/allowlist/allowlist_test.go internal/resolver/resolver.go internal/resolver/resolver_test.go
git commit -s -m "fix(allowlist): union the effective list instead of letting the instance own it (issue #185)

The instance list used to win outright whenever it was non-empty, so the first
edit of any kind — including a removal — materialized the current builtin into
the instance and froze it there. A later hardening of Default() could then not
reach that instance, which is the fail-open direction.

Unconditionally union the platform builtin, the template additions, the
hand-authored instance rules and the learned grants instead.

Assisted-by: Claude Code"
```

---

### Task 2: Validate rule patterns on write

**Files:**
- Modify: `internal/allowlist/allowlist.go` (append)
- Test: `internal/allowlist/allowlist_test.go` (append)
- Modify: `internal/server/handlers_agent_approval.go:82-102`
- Test: `internal/server/handlers_agent_approval_test.go` (create)

**Interfaces:**
- Consumes: nothing from Task 1.
- Produces: `func Validate(r v1alpha1.AllowlistRule) error`.

- [ ] **Step 1: Write the failing test**

Append to `internal/allowlist/allowlist_test.go`:

```go
// TestValidate covers the write-path validation added in issue #185: an
// argPattern the gateway cannot compile must be rejected here, where the user
// can see the error, rather than stored and shipped. Pattern is a command
// name, not a regex, so only emptiness is checked.
func TestValidate(t *testing.T) {
	ok := []v1alpha1.AllowlistRule{
		{Pattern: "ls"},
		{Pattern: "kubectl", ArgPattern: `^get pods$`},
		{Pattern: "kubectl", ArgPattern: kubectlReadArgPattern},
	}
	for _, r := range ok {
		if err := Validate(r); err != nil {
			t.Errorf("Validate(%+v) = %v, want nil", r, err)
		}
	}

	bad := []v1alpha1.AllowlistRule{
		{Pattern: ""},
		{Pattern: "   "},
		{Pattern: "ls", ArgPattern: `^(.*$`},
	}
	for _, r := range bad {
		if err := Validate(r); err == nil {
			t.Errorf("Validate(%+v) = nil, want an error", r)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/allowlist/... -run TestValidate -v`

Expected: compile failure — `undefined: Validate`.

- [ ] **Step 3: Implement `Validate`**

Append to `internal/allowlist/allowlist.go` (add `"errors"`, `"fmt"`, `"regexp"`, `"strings"` to the imports):

```go
// Validate reports whether a rule is well formed. Pattern is a command name
// rather than a regular expression, so it is only checked for emptiness;
// ArgPattern is a regular expression compiled by the gateway at match time, so
// it is compiled here to reject it while the user is still looking at the form.
func Validate(r v1alpha1.AllowlistRule) error {
	if strings.TrimSpace(r.Pattern) == "" {
		return errors.New("pattern is required")
	}
	if r.ArgPattern == "" {
		return nil
	}
	if _, err := regexp.Compile(r.ArgPattern); err != nil {
		return fmt.Errorf("argPattern is not a valid regular expression: %w", err)
	}
	return nil
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/allowlist/... -run TestValidate -v`

Expected: PASS.

- [ ] **Step 5: Write the failing handler test**

Create `internal/server/handlers_agent_approval_test.go`:

```go
package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/config"
	"github.com/suanova/cubepilot/internal/k8s"
)

// approvalTestServer builds a Server whose only dependency is a fake client,
// plus one provisioned instance so the PUT path can resolve it.
func approvalTestServer(t *testing.T, objs ...client.Object) *Server {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add platform types: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core types: %v", err)
	}
	inst := &v1alpha1.AgentInstance{
		ObjectMeta: metav1.ObjectMeta{
			Name:      k8s.InstanceName("alice", v1alpha1.DefaultAgentName),
			Namespace: "cubepilot",
		},
		Spec: v1alpha1.AgentInstanceSpec{TemplateRef: "t1", Owner: "alice"},
	}
	all := append([]client.Object{inst}, objs...)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(all...).Build()
	// userOf falls back to cfg.DefaultUser when no X-CubePilot-User header is
	// set (internal/server/handlers.go:38-43), so tests address "alice" without
	// a header.
	return &Server{cfg: config.Config{Namespace: "cubepilot", DefaultUser: "alice"}, cr: cl}
}

// TestPutAgentApprovalRejectsInvalidArgPattern covers issue #185: the form
// accepted any regex and the platform shipped it to the gateway unvalidated.
func TestPutAgentApprovalRejectsInvalidArgPattern(t *testing.T) {
	s := approvalTestServer(t)
	body := bytes.NewBufferString(`{"approvalPolicy":"Allowlist","allowlist":[{"pattern":"ls","argPattern":"^(.*$"}]}`)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/agent/approval", body)
	w := httptest.NewRecorder()

	s.handleAgentApproval(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", w.Code, w.Body.String())
	}
}
```

The `context` import in that file is then only needed by the Task 5 tests, which use `context.Background()` directly — keep it.

- [ ] **Step 6: Run the test to verify it fails**

Run: `go test ./internal/server/... -run TestPutAgentApprovalRejectsInvalidArgPattern -v`

Expected: FAIL — status 200, because nothing validates yet.

- [ ] **Step 7: Validate in the PUT handler**

In `internal/server/handlers_agent_approval.go`, in the `http.MethodPut` branch, after the `json.Decode` error check and before the `approvalPolicy` switch, insert:

```go
		for _, rule := range body.Allowlist {
			if err := allowlist.Validate(rule); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return
			}
		}
```

- [ ] **Step 8: Run the tests to verify they pass**

Run: `go test ./internal/server/... ./internal/allowlist/... -v 2>&1 | tail -30`

Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/allowlist/allowlist.go internal/allowlist/allowlist_test.go internal/server/handlers_agent_approval.go internal/server/handlers_agent_approval_test.go
git commit -s -m "fix(api): validate allowlist argPattern before storing it (issue #185)

argPattern was free text from the form, shipped verbatim to the gateway, and
compiled there — so a typo was stored, pushed, and never reported. Compile it
on the write path instead and reject it with a 400 the form can show.

Assisted-by: Claude Code"
```

---

### Task 3: The `grants` store

A new package owning the per-user learned grants ConfigMap. Self-contained: no wiring yet, so it reviews on its own.

**Files:**
- Create: `internal/grants/grants.go`
- Test: `internal/grants/grants_test.go`
- Modify: `deploy/charts/cubepilot/templates/rbac.yaml:220-228`

**Interfaces:**
- Consumes: `k8s.ResourceName(prefix, user string) string`, `k8s.InstanceName(user, agent string) string`, `v1alpha1.DefaultAgentName`, `v1alpha1.GroupVersion`.
- Produces:
  - `const MaxGrants = 1000` (and the unexported `maxCommandBytes`)
  - `type Record struct { Pattern, ArgPattern, Command string; CreatedAt time.Time }` with method `Rule() v1alpha1.AllowlistRule`
  - `type Store struct{...}`, `func New(cr client.Client, namespace string) *Store`
  - `func (s *Store) Name(user string) string`
  - `func (s *Store) List(ctx context.Context, user string) ([]Record, error)` — oldest first
  - `func (s *Store) Add(ctx context.Context, user string, r v1alpha1.AllowlistRule, command string, now time.Time) error`
  - `func (s *Store) Remove(ctx context.Context, user string, r v1alpha1.AllowlistRule) error` — idempotent; a missing ConfigMap or key is not an error

- [ ] **Step 1: Write the failing test**

Create `internal/grants/grants_test.go`:

```go
package grants

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/k8s"
)

func testStore(t *testing.T, objs ...client.Object) *Store {
	t.Helper()
	return testStoreWithMax(t, MaxGrants, objs...)
}

// testStoreWithMax builds a Store with an overridden cap, so the eviction test
// drives three entries instead of a thousand.
func testStoreWithMax(t *testing.T, max int, objs ...client.Object) *Store {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core types: %v", err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add platform types: %v", err)
	}
	s := New(fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build(), "cubepilot")
	s.max = max
	return s
}

func TestListMissingConfigMapIsEmpty(t *testing.T) {
	s := testStore(t)
	got, err := s.List(context.Background(), "alice")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d grants, want 0", len(got))
	}
}

func TestAddThenList(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	rule := v1alpha1.AllowlistRule{Pattern: "kubectl", ArgPattern: `^get pods$`}

	if err := s.Add(ctx, "alice", rule, "kubectl get pods", now); err != nil {
		t.Fatalf("Add: %v", err)
	}
	got, err := s.List(ctx, "alice")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d grants, want 1", len(got))
	}
	if got[0].Pattern != "kubectl" || got[0].ArgPattern != `^get pods$` {
		t.Errorf("round trip lost the rule: %+v", got[0])
	}
	if got[0].Command != "kubectl get pods" {
		t.Errorf("Command = %q", got[0].Command)
	}
	if !got[0].CreatedAt.Equal(now) {
		t.Errorf("CreatedAt = %v, want %v", got[0].CreatedAt, now)
	}
}

// TestAddIsIdempotent pins the dedup identity to pattern|argPattern: the same
// command approved twice must not grow the list, and must keep the original
// CreatedAt so the cap evicts by first-seen order.
func TestAddIsIdempotent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	rule := v1alpha1.AllowlistRule{Pattern: "helm"}
	first := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	second := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)

	if err := s.Add(ctx, "alice", rule, "helm install x", first); err != nil {
		t.Fatalf("Add first: %v", err)
	}
	if err := s.Add(ctx, "alice", rule, "helm install y", second); err != nil {
		t.Fatalf("Add second: %v", err)
	}
	got, err := s.List(ctx, "alice")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d grants, want 1", len(got))
	}
	if !got[0].CreatedAt.Equal(first) {
		t.Errorf("CreatedAt = %v, want the original %v", got[0].CreatedAt, first)
	}
}

// TestAddEvictsOldestPastCap: growth is human-driven, so the cap is a safety
// net against a loop rather than a working limit. The store's cap is
// overridden to 3 so this exercises the eviction path in three writes.
func TestAddEvictsOldestPastCap(t *testing.T) {
	s := testStoreWithMax(t, 3)
	ctx := context.Background()
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	for i := 0; i < 3; i++ {
		rule := v1alpha1.AllowlistRule{Pattern: "cmd", ArgPattern: "arg-" + strconv.Itoa(i)}
		if err := s.Add(ctx, "alice", rule, "", base.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}
	// One more pushes past the cap.
	newest := v1alpha1.AllowlistRule{Pattern: "newest"}
	if err := s.Add(ctx, "alice", newest, "", base.Add(time.Hour)); err != nil {
		t.Fatalf("Add newest: %v", err)
	}

	got, err := s.List(ctx, "alice")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d grants, want 3", len(got))
	}
	// The very first rule added must be gone, the newest kept.
	for _, r := range got {
		if r.Pattern == "cmd" && r.ArgPattern == "arg-0" {
			t.Error("oldest grant was not evicted")
		}
	}
	if got[len(got)-1].Pattern != "newest" {
		t.Errorf("newest grant missing; last = %+v", got[len(got)-1])
	}
}

// TestAddTruncatesLongCommand: the command text is display-only, and leaving it
// unbounded would make the per-entry size — and so the MaxGrants arithmetic —
// meaningless.
func TestAddTruncatesLongCommand(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	long := strings.Repeat("a", maxCommandBytes*2)
	if err := s.Add(ctx, "alice", v1alpha1.AllowlistRule{Pattern: "bash"}, long, time.Now()); err != nil {
		t.Fatalf("Add: %v", err)
	}
	got, err := s.List(ctx, "alice")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d grants, want 1", len(got))
	}
	// Truncated to the cap, with a visible marker so the UI can tell the text
	// is incomplete.
	want := maxCommandBytes + len("…")
	if len(got[0].Command) != want {
		t.Errorf("Command length = %d, want %d", len(got[0].Command), want)
	}
	if !strings.HasSuffix(got[0].Command, "…") {
		t.Errorf("truncated command has no marker: %q", got[0].Command)
	}
}

// TestRemoveDropsTheGrantAndIsIdempotent: revoking a learned grant must work
// from the UI, and revoking one that is already gone must not error.
func TestRemoveDropsTheGrantAndIsIdempotent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	keep := v1alpha1.AllowlistRule{Pattern: "terraform"}
	drop := v1alpha1.AllowlistRule{Pattern: "helm"}
	if err := s.Add(ctx, "alice", keep, "", time.Now()); err != nil {
		t.Fatalf("Add keep: %v", err)
	}
	if err := s.Add(ctx, "alice", drop, "", time.Now()); err != nil {
		t.Fatalf("Add drop: %v", err)
	}

	if err := s.Remove(ctx, "alice", drop); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := s.Remove(ctx, "alice", drop); err != nil {
		t.Fatalf("Remove again should be a no-op: %v", err)
	}
	// Removing from a user with no ConfigMap at all is also a no-op.
	if err := s.Remove(ctx, "bob", drop); err != nil {
		t.Fatalf("Remove for a user without grants: %v", err)
	}

	got, err := s.List(ctx, "alice")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].Pattern != "terraform" {
		t.Fatalf("grants = %+v, want only terraform", got)
	}
}

// TestAddSetsOwnerReference: the ConfigMap is garbage-collected with the
// instance it belongs to.
func TestAddSetsOwnerReference(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Add(ctx, "alice", v1alpha1.AllowlistRule{Pattern: "helm"}, "", time.Now()); err != nil {
		t.Fatalf("Add: %v", err)
	}
	var cm corev1.ConfigMap
	if err := s.cr.Get(ctx, types.NamespacedName{Namespace: "cubepilot", Name: s.Name("alice")}, &cm); err != nil {
		t.Fatalf("get ConfigMap: %v", err)
	}
	if got := cm.Name; got != k8s.ResourceName("cubepilot-grants", "alice") {
		t.Errorf("ConfigMap name = %q", got)
	}
	if len(cm.OwnerReferences) != 1 {
		t.Fatalf("got %d owner references, want 1", len(cm.OwnerReferences))
	}
	ref := cm.OwnerReferences[0]
	if ref.Kind != "AgentInstance" || ref.Name != k8s.InstanceName("alice", v1alpha1.DefaultAgentName) {
		t.Errorf("owner reference = %+v", ref)
	}
}

```

The test file's imports are `context`, `strconv`, `strings`, `testing`, `time`, `corev1`, `metav1`, `runtime`, `apierrors`, `types`, `client`, `fake`, `v1alpha1`, `k8s`. `Key` is exercised indirectly by `Add`/`List`; import nothing you do not use.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/grants/... -v`

Expected: compile failure — `undefined: New`, `undefined: MaxGrants`, `undefined: Key`.

- [ ] **Step 3: Implement the package**

Create `internal/grants/grants.go`:

```go
// Package grants stores the learned "allow always" grants for a user: rules
// the agent proposed, the user approved in chat, and the platform recorded so
// the command auto-passes from then on (issue #185).
//
// Grants are recorded state, not desired state. They are kept out of
// AgentInstance.spec so that a machine write can no longer flip the "the
// instance owns its list" sentinel, and because a lost grant is fail-closed:
// the command simply asks again. Storage is a per-user ConfigMap whose only
// writer is the API server, which keeps the whole-object-overwrite failure
// mode out of a field the controller also writes.
package grants

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/k8s"
)

// MaxGrants bounds a user's learned grants. Growth is driven by human clicks
// rather than by the agent, so this is a safety net against a pathological
// loop, not a working limit — it has to sit far above what a heavy user
// accumulates in a year (order of 300) or it silently evicts grants people
// still rely on.
//
// Sizing: roughly 250 bytes per entry (32-char key plus the JSON value), so
// 1000 entries is about a quarter of the 1 MiB ConfigMap ceiling. maxCommandBytes
// keeps that per-entry figure honest.
const (
	MaxGrants = 1000
	// maxCommandBytes bounds the stored command text, which is display-only and
	// plays no part in matching. Without a bound a single `bash -c` with a long
	// heredoc could push the ConfigMap past the 1 MiB API-server ceiling, and
	// the failure would not be confined to that entry: every later Add for that
	// user would fail too.
	maxCommandBytes = 512
)

// The data keys of the grants ConfigMap are opaque digests; the values are
// Records. One key per grant makes an add a single-key write that cannot lose
// a concurrent add of a different grant.
const managedByLabel = "cubepilot-grants"

// Record is one stored grant.
type Record struct {
	Pattern    string    `json:"pattern"`
	ArgPattern string    `json:"argPattern,omitempty"`
	// Command is the invocation that produced the grant, kept for the UI so a
	// learned rule can be shown as something a human recognises. It is display
	// only — matching uses Pattern and ArgPattern — and is truncated by
	// maxCommandBytes, with a trailing ellipsis when it was.
	Command   string    `json:"command,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

// Rule is the public-API allowlist rule this grant contributes.
func (r Record) Rule() v1alpha1.AllowlistRule {
	return v1alpha1.AllowlistRule{Pattern: r.Pattern, ArgPattern: r.ArgPattern}
}

// Store reads and writes the per-user grants ConfigMap. max is a field rather
// than the MaxGrants constant directly so the eviction test can drive a small
// cap instead of looping a thousand times.
type Store struct {
	cr  client.Client
	ns  string
	max int
}

// New returns a Store backed by cr in namespace ns.
func New(cr client.Client, namespace string) *Store {
	return &Store{cr: cr, ns: namespace, max: MaxGrants}
}

// Name is the grants ConfigMap name for a user.
func (s *Store) Name(user string) string {
	return k8s.ResourceName("cubepilot-grants", user)
}

// Key is the stable data key for a rule: a hex digest of the same
// pattern|argPattern identity allowlist.Merge dedups on.
func Key(r v1alpha1.AllowlistRule) string {
	sum := sha256.Sum256([]byte(r.Pattern + "|" + r.ArgPattern))
	return hex.EncodeToString(sum[:16])
}

// List returns the user's grants, oldest first. A missing ConfigMap is an
// empty list rather than an error: most instances never record one.
func (s *Store) List(ctx context.Context, user string) ([]Record, error) {
	var cm corev1.ConfigMap
	err := s.cr.Get(ctx, types.NamespacedName{Namespace: s.ns, Name: s.Name(user)}, &cm)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]Record, 0, len(cm.Data))
	for _, raw := range cm.Data {
		var rec Record
		if err := json.Unmarshal([]byte(raw), &rec); err != nil || rec.Pattern == "" {
			// A malformed value is skipped rather than failing the resolve: a
			// dropped grant asks again, which is the safe direction.
			continue
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].Pattern < out[j].Pattern
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// Add records a grant. It is idempotent on (pattern, argPattern): recording
// the same rule again keeps the original CreatedAt, so the cap evicts by
// first-seen order rather than being kept alive by repeats.
func (s *Store) Add(ctx context.Context, user string, r v1alpha1.AllowlistRule, command string, now time.Time) error {
	if r.Pattern == "" {
		return nil
	}
	raw, err := json.Marshal(Record{
		Pattern:    r.Pattern,
		ArgPattern: r.ArgPattern,
		Command:    truncate(command, maxCommandBytes),
		CreatedAt:  now.UTC(),
	})
	if err != nil {
		return err
	}
	key := Key(r)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cm, err := s.ensure(ctx, user)
		if err != nil {
			return err
		}
		if _, exists := cm.Data[key]; exists {
			return nil
		}
		cm.Data[key] = string(raw)
		evict(cm, s.max)
		return s.cr.Update(ctx, cm)
	})
}

// truncate bounds s to about n bytes, cutting on a byte boundary. The value is
// display-only — matching uses Pattern and ArgPattern — so a split rune at the
// cut is acceptable and not worth the extra code to avoid. The ellipsis is what
// matters: a silently shortened command reads as a complete one, and the user
// would be looking at a rule whose text they cannot trust.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Remove revokes a grant. It is idempotent: a missing ConfigMap, or a key that
// is not there, is a no-op rather than an error, so a double-click or a stale
// UI does not surface a failure.
func (s *Store) Remove(ctx context.Context, user string, r v1alpha1.AllowlistRule) error {
	if r.Pattern == "" {
		return nil
	}
	key := Key(r)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cm corev1.ConfigMap
		err := s.cr.Get(ctx, types.NamespacedName{Namespace: s.ns, Name: s.Name(user)}, &cm)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		if _, exists := cm.Data[key]; !exists {
			return nil
		}
		delete(cm.Data, key)
		if len(cm.Data) == 0 {
			return s.cr.Delete(ctx, &cm)
		}
		return s.cr.Update(ctx, &cm)
	})
}

// ensure returns the user's grants ConfigMap, creating it when absent. The
// owner reference ties it to the instance, so it is garbage-collected with it.
func (s *Store) ensure(ctx context.Context, user string) (*corev1.ConfigMap, error) {
	name := s.Name(user)
	var cm corev1.ConfigMap
	err := s.cr.Get(ctx, types.NamespacedName{Namespace: s.ns, Name: name}, &cm)
	if err == nil {
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		return &cm, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}
	cm = corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: s.ns,
			Labels:    map[string]string{"app.kubernetes.io/managed-by": managedByLabel},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: v1alpha1.GroupVersion.String(),
				Kind:       "AgentInstance",
				Name:       k8s.InstanceName(user, v1alpha1.DefaultAgentName),
			}},
		},
		Data: map[string]string{},
	}
	if err := s.cr.Create(ctx, &cm); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, err
		}
		// Lost a create race: re-read and use the existing object.
		if err := s.cr.Get(ctx, types.NamespacedName{Namespace: s.ns, Name: name}, &cm); err != nil {
			return nil, err
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
	}
	return &cm, nil
}

// evict drops the oldest entries until at most max remain.
func evict(cm *corev1.ConfigMap, max int) {
	if len(cm.Data) <= max {
		return
	}
	type keyed struct {
		key string
		at  time.Time
	}
	all := make([]keyed, 0, len(cm.Data))
	for k, raw := range cm.Data {
		var rec Record
		if err := json.Unmarshal([]byte(raw), &rec); err != nil {
			// Undecodable entries can never be ordered; drop them first.
			delete(cm.Data, k)
			continue
		}
		all = append(all, keyed{key: k, at: rec.CreatedAt})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })
	for i := 0; i < len(all) && len(cm.Data) > max; i++ {
		delete(cm.Data, all[i].key)
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/grants/... -v`

Expected: PASS for all seven tests.

**On concurrency:** `Add` and `Remove` wrap get-modify-update in `retry.RetryOnConflict`, so a concurrent write of a *different* grant retries instead of being lost. There is deliberately no unit test for it — the controller-runtime fake client does not reproduce optimistic-concurrency conflicts, and a test that cannot fail is worse than none. The retry wrapper is the guarantee; say so in the PR body rather than claiming coverage.

- [ ] **Step 5: Grant the API server ConfigMap access**

In `deploy/charts/cubepilot/templates/rbac.yaml`, the API's namespaced Role currently lists only `secrets` (around line 220-228). Add a rule beside it:

```yaml
  # Learned allow-always grants (issue #185): the API is the sole writer of the
  # per-user grants ConfigMap.
  - apiGroups: [""]
    resources: ["configmaps"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
```

- [ ] **Step 6: Verify the chart still renders**

Run: `helm template cubepilot deploy/charts/cubepilot >/dev/null && echo OK`

Expected: `OK` with no template error.

- [ ] **Step 7: Commit**

```bash
git add internal/grants/ deploy/charts/cubepilot/templates/rbac.yaml
git commit -s -m "feat(grants): add the per-user learned-grants store (issue #185)

Learned allow-always grants get their own ConfigMap-backed store, one key per
grant, written only by the API server. Keeping them out of AgentInstance.spec
stops a machine write from flipping the list-ownership sentinel, and a
single-writer object avoids the whole-object-overwrite failure mode that a
status subresource shared with the controller would reintroduce.

Not wired up yet: nothing reads or writes the store until the next two tasks.

Assisted-by: Claude Code"
```

---

### Task 4: The resolver reads grants

**Files:**
- Modify: `internal/resolver/resolver.go` (struct + `Resolve` allowlist call)
- Test: `internal/resolver/resolver_test.go` (helper + append)

**Interfaces:**
- Consumes: `grants.New(cr client.Client, namespace string) *Store`, `(*Store).List(ctx, user) ([]Record, error)`, `(Record).Rule()`.
- Produces: `ResolvedAgentConfig.Allowlist` now includes learned grants; changing a grant changes `ResolvedAgentConfig.Revision` (no code change needed — `fingerprint()` already hashes `Allowlist`).

- [ ] **Step 1: Extend the test scheme to core types**

In `internal/resolver/resolver_test.go`, in `testResolver`, add `corev1` to the scheme (the grants ConfigMap is a core type, and the fake client rejects reads of unregistered kinds):

```go
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
```

Add `corev1 "k8s.io/api/core/v1"` to that file's imports.

- [ ] **Step 2: Write the failing test**

Append to `internal/resolver/resolver_test.go`:

```go
// TestResolvedAllowlistIncludesLearnedGrants covers the fourth arm of the
// union (issue #185): a recorded grant auto-passes without appearing in
// AgentInstance.spec.
func TestResolvedAllowlistIncludesLearnedGrants(t *testing.T) {
	inst := instance("alice", "t1", "")
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      k8s.ResourceName("cubepilot-grants", "alice"),
			Namespace: "",
		},
		Data: map[string]string{
			"deadbeef": `{"pattern":"helm","argPattern":"^install x$","createdAt":"2026-09-14T10:00:00Z"}`,
		},
	}
	r := testResolver(t, template("t1", nil), inst, cm)

	cfg, err := r.Resolve(context.Background(), "alice", "t1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	var found bool
	for _, rule := range cfg.Allowlist {
		if rule.Pattern == "helm" && rule.ArgPattern == "^install x$" {
			found = true
		}
	}
	if !found {
		t.Errorf("learned grant missing from the resolved allowlist: %v", cfg.Allowlist)
	}
}

// TestGrantChangesTheRevision: the revision is what gates the gateway push
// (internal/server/hitl.go PreTurn), so a grant edit must move it.
func TestGrantChangesTheRevision(t *testing.T) {
	r := testResolver(t, template("t1", nil), instance("alice", "t1", ""))
	before, err := r.Resolve(context.Background(), "alice", "t1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: k8s.ResourceName("cubepilot-grants", "alice")},
		Data: map[string]string{
			"deadbeef": `{"pattern":"helm","createdAt":"2026-09-14T10:00:00Z"}`,
		},
	}
	if err := r.cr.Create(context.Background(), cm); err != nil {
		t.Fatalf("create grants ConfigMap: %v", err)
	}

	after, err := r.Resolve(context.Background(), "alice", "t1")
	if err != nil {
		t.Fatalf("Resolve after: %v", err)
	}
	if before.Revision == after.Revision {
		t.Errorf("revision did not change after recording a grant (%q)", after.Revision)
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./internal/resolver/... -run 'TestResolvedAllowlistIncludesLearnedGrants|TestGrantChangesTheRevision' -v`

Expected: FAIL — the grant is not in the resolved allowlist (the second test fails on the unchanged revision).

- [ ] **Step 4: Add the store to the Resolver**

In `internal/resolver/resolver.go`, the struct and constructor are currently (lines 111-120):

```go
type Resolver struct {
	cr client.Client
	ns string
}

// New returns a Resolver backed by the controller-runtime client, reading
// platform CRs from namespace (namespaced CRD scope, issue #146).
func New(cr client.Client, namespace string) *Resolver {
	return &Resolver{cr: cr, ns: namespace}
}
```

Add the grants store — the only change is the new field and the extra `New` argument:

```go
type Resolver struct {
	cr     client.Client
	ns     string
	grants *grants.Store
}

// New returns a Resolver backed by the controller-runtime client, reading
// platform CRs from namespace (namespaced CRD scope, issue #146).
func New(cr client.Client, namespace string) *Resolver {
	return &Resolver{cr: cr, ns: namespace, grants: grants.New(cr, namespace)}
}
```

Import `"github.com/suanova/cubepilot/internal/grants"`. The constructor call sites (`cmd/cubepilot-api/main.go`, `internal/instances/manager.go`) do not change — only the returned struct gains a field.

- [ ] **Step 5: Feed the grants into `Effective`**

In `internal/resolver/resolver.go`, replace the call added in Task 1:

```go
	cfg.Allowlist = allowlist.Effective(tmplAllowlist, inst.Spec.Allowlist, nil)
```

with:

```go
	records, err := r.grants.List(ctx, user)
	if err != nil {
		return nil, fmt.Errorf("list grants for %s: %w", user, err)
	}
	learnedRules := make([]v1alpha1.AllowlistRule, 0, len(records))
	for _, rec := range records {
		learnedRules = append(learnedRules, rec.Rule())
	}
	cfg.Allowlist = allowlist.Effective(tmplAllowlist, inst.Spec.Allowlist, learnedRules)
```

Update the comment above it to mention the fourth arm (drop the "added in Task 4" note from Task 1).

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/resolver/... -v 2>&1 | tail -30`

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/resolver/
git commit -s -m "feat(resolver): union learned grants into the effective allowlist (issue #185)

The resolver now reads the per-user grants ConfigMap and folds it into the
effective list, so a recorded grant auto-passes without living in
AgentInstance.spec. The grants are part of the config fingerprint already, so
recording one still moves the revision that gates the gateway push.

Assisted-by: Claude Code"
```

---

### Task 5: `allow-always` writes a grant instead of the spec

**Files:**
- Modify: `internal/server/handlers_agent_approval.go:177-250`
- Modify: `internal/server/approvals.go:352-368`
- Test: `internal/server/handlers_agent_approval_test.go` (append)

**Interfaces:**
- Consumes: `grants.New`, `(*Store).Add(ctx, user, rule, command, now)`, `(*Store).List(ctx, user)`, `deriveAllowAlwaysRule(command string) (v1alpha1.AllowlistRule, bool)` (existing).
- Produces: `func (s *Server) grantsStore() *grants.Store`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/server/handlers_agent_approval_test.go`:

```go
// TestAllowAlwaysWritesAGrantNotTheSpec covers issue #185: the machine-written
// grant must land in the grants store, and the instance spec must stay
// untouched so the list-ownership sentinel cannot be flipped.
func TestAllowAlwaysWritesAGrantNotTheSpec(t *testing.T) {
	s := approvalTestServer(t)
	ctx := context.Background()
	rule, ok := deriveAllowAlwaysRule("kubectl get pods -n foo")
	if !ok {
		t.Fatal("deriveAllowAlwaysRule returned !ok")
	}
	if _, err := s.allowlistAlways(ctx, "alice", rule); err != nil {
		t.Fatalf("allowlistAlways: %v", err)
	}

	got, err := s.grantsStore().List(ctx, "alice")
	if err != nil {
		t.Fatalf("List grants: %v", err)
	}
	if len(got) != 1 || got[0].Pattern != "kubectl" {
		t.Fatalf("grants = %+v, want one kubectl grant", got)
	}

	var inst v1alpha1.AgentInstance
	if err := s.cr.Get(ctx, types.NamespacedName{Namespace: "cubepilot", Name: k8s.InstanceName("alice", v1alpha1.DefaultAgentName)}, &inst); err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if len(inst.Spec.Allowlist) != 0 {
		t.Errorf("instance spec was written: %+v", inst.Spec.Allowlist)
	}
}

// TestClearOwnedAllowlistKeepsGrants: today the Reset button discards learned
// grants as a side effect of clearing ownership. Keeping the two stores apart
// makes Reset mean what it says.
func TestClearOwnedAllowlistKeepsGrants(t *testing.T) {
	s := approvalTestServer(t)
	ctx := context.Background()
	rule, _ := deriveAllowAlwaysRule("helm install x")
	if err := s.grantsStore().Add(ctx, "alice", rule, "helm install x", time.Now()); err != nil {
		t.Fatalf("Add grant: %v", err)
	}

	if err := s.saveConfirm(ctx, "alice", v1alpha1.ApprovalPolicyAllowlist, nil); err != nil {
		t.Fatalf("saveConfirm: %v", err)
	}

	got, err := s.grantsStore().List(ctx, "alice")
	if err != nil {
		t.Fatalf("List grants: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("Reset discarded learned grants: %+v", got)
	}
}
```

Add `"time"`, `"k8s.io/apimachinery/pkg/types"` to that file's imports if absent.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/server/... -run 'TestAllowAlwaysWritesAGrantNotTheSpec|TestClearOwnedAllowlistKeepsGrants' -v`

Expected: compile failure — `s.grantsStore undefined`.

- [ ] **Step 3: Add the store accessor**

In `internal/server/handlers_agent_approval.go` (near `allowlistAlways`), add:

```go
// grantsStore returns the learned-grants store. It is derived from the client
// and namespace on each call rather than held as a field: it is two values, and
// a lazily-initialised field would be a data race on a Server the HTTP server
// drives concurrently.
func (s *Server) grantsStore() *grants.Store {
	return grants.New(s.cr, s.cfg.Namespace)
}
```

Import `"github.com/suanova/cubepilot/internal/grants"`.

- [ ] **Step 4: Retarget `allowlistAlways`**

In `internal/server/handlers_agent_approval.go`, replace the body of `allowlistAlways` (currently lines ~223-250) with:

```go
// allowlistAlways records an allow-always entry in the user's grants store
// (issue #185). The grant lives outside AgentInstance.spec: writing it into the
// spec used to materialize the whole inherited list on first use, freezing that
// instance off the platform builtin for good. No-op (false) when the effective
// policy is not Allowlist (under AlwaysAsk everything asks anyway).
func (s *Server) allowlistAlways(ctx context.Context, user string, rule v1alpha1.AllowlistRule) (bool, error) {
	if s.cr == nil {
		return false, nil
	}
	if s.mgr != nil {
		if cfg, err := s.mgr.ResolvedConfigForUser(ctx, user); err == nil && cfg != nil && !cfg.Empty() {
			if cfg.ApprovalPolicy != v1alpha1.ApprovalPolicyAllowlist {
				return false, nil
			}
		}
	}
	if err := s.grantsStore().Add(ctx, user, rule, command, time.Now()); err != nil {
		return false, err
	}
	return true, nil
}
```

Import `"time"`.

The signature widens by one parameter so the stored grant can carry the command text for display:

```go
func (s *Server) allowlistAlways(ctx context.Context, user, command string, rule v1alpha1.AllowlistRule) (bool, error) {
```

Its only call site is in `internal/server/approvals.go`, in the `body.Decision == "allow-always"` branch, which already has `p.Command` in scope. Change:

```go
				if ok, err := s.allowlistAlways(r.Context(), user, rule); err != nil {
```

to:

```go
				if ok, err := s.allowlistAlways(r.Context(), user, p.Command, rule); err != nil {
```

- [ ] **Step 5: Let the UI revoke a learned grant**

`persistConfirm` in the Portal can only write `spec.allowlist`, so a learned grant has no revoke path once it stops living in the spec. Add one, additively: the PUT body accepts an optional list of grants to drop.

In `internal/server/handlers_agent_approval.go`, extend the PUT body struct:

```go
		var body struct {
			ApprovalPolicy v1alpha1.ApprovalPolicy  `json:"approvalPolicy"`
			Allowlist      []v1alpha1.AllowlistRule `json:"allowlist"`
			// RevokeGrants drops learned grants (issue #185). Grants are a
			// separate store, so revoking one cannot be expressed by rewriting
			// the hand-authored list.
			RevokeGrants []v1alpha1.AllowlistRule `json:"revokeGrants"`
		}
```

After the `Allowlist` validation loop added in Task 2, validate these the same way and apply them before `saveConfirm`:

```go
		for _, rule := range body.RevokeGrants {
			if err := allowlist.Validate(rule); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return
			}
		}
		for _, rule := range body.RevokeGrants {
			if err := s.grantsStore().Remove(r.Context(), s.userOf(r), rule); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
		}
```

- [ ] **Step 6: Write the revoke test**

Append to `internal/server/handlers_agent_approval_test.go`:

```go
// TestPutAgentApprovalRevokesLearnedGrant covers the revoke path added in
// issue #185: a learned grant is dropped from the grants store without the
// hand-authored list being rewritten.
func TestPutAgentApprovalRevokesLearnedGrant(t *testing.T) {
	s := approvalTestServer(t)
	ctx := context.Background()
	rule, _ := deriveAllowAlwaysRule("helm install x")
	if err := s.grantsStore().Add(ctx, "alice", rule, "helm install x", time.Now()); err != nil {
		t.Fatalf("Add grant: %v", err)
	}

	body := bytes.NewBufferString(`{"approvalPolicy":"Allowlist","allowlist":[],"revokeGrants":[{"pattern":"helm","argPattern":"^install x$"}]}`)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/agent/approval", body)
	w := httptest.NewRecorder()
	s.handleAgentApproval(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	got, err := s.grantsStore().List(ctx, "alice")
	if err != nil {
		t.Fatalf("List grants: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("grant not revoked: %+v", got)
	}
}
```

- [ ] **Step 7: Stop `saveConfirm` from touching grants**

`saveConfirm` (lines 177-195) already only writes `inst.Spec.ApprovalPolicy` and `inst.Spec.Allowlist` — it never touched the grants store, so no code change is needed there. Update its doc comment to say so explicitly:

```go
// saveConfirm writes the instance's confirmation override and hand-authored
// allowlist. An empty approvalPolicy clears the override (inherit the
// template); an empty allowlist clears the hand-authored rules. Learned grants
// are a separate store and are deliberately untouched (issue #185) — clearing
// your own rules must not discard what you approved in chat.
```

- [ ] **Step 8: Run the tests to verify they pass**

Run: `go test ./internal/server/... -v 2>&1 | tail -40`

Expected: PASS. If a pre-existing test asserted that allow-always lands in `inst.Spec.Allowlist`, update it to the grants-store expectation and name it in the commit message.

- [ ] **Step 9: Commit**

```bash
git add internal/server/
git commit -s -m "feat(api): record allow-always in the grants store, not the instance spec (issue #185)

Allow-always used to append to AgentInstance.spec.allowlist, materializing the
whole inherited list on first use and freezing that instance off the platform
builtin. It now records into the per-user grants store instead, so the spec
holds only hand-authored rules. Clearing your own rules no longer discards what
you approved in chat.

Assisted-by: Claude Code"
```

---

### Task 6: Serve provenance on the approval view, and document it

**Files:**
- Modify: `internal/server/handlers_agent_approval.go:36-66, 145-175`
- Modify: `web/src/api/types.ts:273-292`
- Modify: `docs/cubepilot/api.md:525-536`
- Test: `internal/server/handlers_agent_approval_test.go` (append)

**Interfaces:**
- Consumes: `grants.Store.List`, `allowlist.BuiltinLabel`, `Default()`.
- Produces: `approvalRule` gains `Source string \`json:"source,omitempty"\`` with values `builtin`/`template`/`user`/`learned`; `approvalView` gains `AllowlistLearned []approvalRule \`json:"allowlistLearned,omitempty"\``.

- [ ] **Step 1: Write the failing test**

Append to `internal/server/handlers_agent_approval_test.go`:

```go
// TestApprovalViewTagsProvenance covers issue #185: the UI needs to say where a
// rule came from instead of guessing from an isOwned flag.
func TestApprovalViewTagsProvenance(t *testing.T) {
	s := approvalTestServer(t)
	ctx := context.Background()

	var inst v1alpha1.AgentInstance
	if err := s.cr.Get(ctx, types.NamespacedName{Namespace: "cubepilot", Name: k8s.InstanceName("alice", v1alpha1.DefaultAgentName)}, &inst); err != nil {
		t.Fatalf("get instance: %v", err)
	}
	inst.Spec.Allowlist = []v1alpha1.AllowlistRule{{Pattern: "terraform"}}
	if err := s.cr.Update(ctx, &inst); err != nil {
		t.Fatalf("update instance: %v", err)
	}
	rule, _ := deriveAllowAlwaysRule("helm install x")
	if err := s.grantsStore().Add(ctx, "alice", rule, "helm install x", time.Now()); err != nil {
		t.Fatalf("Add grant: %v", err)
	}

	view, err := s.approvalView(ctx, "alice")
	if err != nil {
		t.Fatalf("approvalView: %v", err)
	}

	byPattern := map[string]string{}
	for _, r := range view.Allowlist {
		byPattern[r.Pattern] = r.Source
	}
	if got := byPattern["kubectl"]; got != "builtin" {
		t.Errorf("kubectl source = %q, want builtin", got)
	}
	if got := byPattern["terraform"]; got != "user" {
		t.Errorf("terraform source = %q, want user", got)
	}
	if got := byPattern["helm"]; got != "learned" {
		t.Errorf("helm source = %q, want learned", got)
	}
	if len(view.AllowlistLearned) != 1 || view.AllowlistLearned[0].Pattern != "helm" {
		t.Errorf("allowlistLearned = %+v", view.AllowlistLearned)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/server/... -run TestApprovalViewTagsProvenance -v`

Expected: compile failure — `view.AllowlistLearned` and `r.Source` undefined.

- [ ] **Step 3: Add `Source` and `AllowlistLearned`**

In `internal/server/handlers_agent_approval.go`, extend the two structs:

```go
type approvalView struct {
	Exists           bool                    `json:"exists"`
	ApprovalPolicy   v1alpha1.ApprovalPolicy `json:"approvalPolicy"`
	Override         v1alpha1.ApprovalPolicy `json:"override"`
	TemplatePolicy   v1alpha1.ApprovalPolicy `json:"templatePolicy"`
	Allowlist        []approvalRule          `json:"allowlist,omitempty"`
	AllowlistOwned   []approvalRule          `json:"allowlistOwned,omitempty"`
	AllowlistLearned []approvalRule          `json:"allowlistLearned,omitempty"`
	Channel          string                  `json:"channel"`
}

// approvalRule is one allowlist rule served to the Portal. Source says where
// the rule came from — builtin, template, user or learned (issue #185) — so the
// UI can group by origin rather than guessing from an ownership flag. Label is
// set by the server ONLY for rules that exactly match a platform builtin
// read-only rule, so the UI never guesses that a user-added rule (which may
// allow a write) is read-only.
type approvalRule struct {
	Pattern    string `json:"pattern"`
	ArgPattern string `json:"argPattern,omitempty"`
	Label      string `json:"label,omitempty"`
	Source     string `json:"source,omitempty"`
}

const (
	sourceBuiltin  = "builtin"
	sourceTemplate = "template"
	sourceUser     = "user"
	sourceLearned  = "learned"
)

// ruleID is the identity allowlist.Merge dedups on.
func ruleID(r v1alpha1.AllowlistRule) string { return r.Pattern + "|" + r.ArgPattern }

// toSourcedRules tags each rule of the effective list with the source it came
// from. The tag is derived by membership rather than by rebuilding the union,
// so the view cannot drift from what the resolver enforces. Later arguments win
// on an exact collision, matching the union order — a rule both the user and
// the template declare shows as the user's.
func toSourcedRules(effective, learned, owned, tmpl []v1alpha1.AllowlistRule) []approvalRule {
	origin := make(map[string]string, len(tmpl)+len(owned)+len(learned))
	for _, r := range tmpl {
		origin[ruleID(r)] = sourceTemplate
	}
	for _, r := range owned {
		origin[ruleID(r)] = sourceUser
	}
	for _, r := range learned {
		origin[ruleID(r)] = sourceLearned
	}
	out := make([]approvalRule, 0, len(effective))
	for _, r := range effective {
		source, ok := origin[ruleID(r)]
		if !ok {
			source = sourceBuiltin
		}
		out = append(out, approvalRule{
			Pattern:    r.Pattern,
			ArgPattern: r.ArgPattern,
			Label:      allowlist.BuiltinLabel(r),
			Source:     source,
		})
	}
	return out
}

// toGroupRules converts one source's rules for the per-source group lists.
func toGroupRules(rules []v1alpha1.AllowlistRule, source string) []approvalRule {
	out := make([]approvalRule, 0, len(rules))
	for _, r := range rules {
		out = append(out, approvalRule{
			Pattern:    r.Pattern,
			ArgPattern: r.ArgPattern,
			Label:      allowlist.BuiltinLabel(r),
			Source:     source,
		})
	}
	return out
}
```

Delete the now-unused `toApprovalRules`: `toGroupRules` replaces both of its call sites, and a dead helper will be flagged by the linter. `BuiltinLabel` is still used, from `toGroupRules` and `toSourcedRules`.

- [ ] **Step 4: Populate them in `approvalView`**

Replace the whole function (currently lines 143-175) with this. The effective list is built with the same `allowlist.Effective` the resolver calls, so the view and the enforcement cannot drift; the resolver is then consulted only for the effective policy.

```go
// approvalView resolves the effective + owned confirmation state for a user's
// default instance.
func (s *Server) approvalView(ctx context.Context, user string) (approvalView, error) {
	var view approvalView
	if s.cr == nil {
		return view, nil
	}
	name := k8s.InstanceName(user, v1alpha1.DefaultAgentName)
	var inst v1alpha1.AgentInstance
	err := s.cr.Get(ctx, types.NamespacedName{Namespace: s.cfg.Namespace, Name: name}, &inst)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return view, nil // not provisioned
		}
		return view, err
	}
	view.Exists = true
	view.Override = inst.Spec.ApprovalPolicy

	var tmplAllowlist []v1alpha1.AllowlistRule
	if inst.Spec.TemplateRef != "" {
		var def v1alpha1.AgentTemplate
		if err := s.cr.Get(ctx, types.NamespacedName{Namespace: s.cfg.Namespace, Name: inst.Spec.TemplateRef}, &def); err == nil {
			view.TemplatePolicy = def.Spec.ApprovalPolicy
			tmplAllowlist = def.Spec.Allowlist
		}
	}

	records, err := s.grantsStore().List(ctx, user)
	if err != nil {
		return view, err
	}
	learned := make([]v1alpha1.AllowlistRule, 0, len(records))
	for _, rec := range records {
		learned = append(learned, rec.Rule())
	}

	// Same union the resolver enforces (issue #185), tagged by source so the
	// Portal can group by origin instead of guessing from an ownership flag.
	view.Allowlist = toSourcedRules(allowlist.Effective(tmplAllowlist, inst.Spec.Allowlist, learned), learned, inst.Spec.Allowlist, tmplAllowlist)
	view.AllowlistOwned = toGroupRules(inst.Spec.Allowlist, sourceUser)
	view.AllowlistLearned = toGroupRules(learned, sourceLearned)

	if s.mgr != nil {
		if cfg, err := s.mgr.ResolvedConfigForUser(ctx, user); err == nil && cfg != nil && !cfg.Empty() {
			view.ApprovalPolicy = cfg.ApprovalPolicy
		}
	}
	return view, nil
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/server/... -v 2>&1 | tail -40`

Expected: PASS.

- [ ] **Step 6: Update the TypeScript type**

In `web/src/api/types.ts`, extend `AllowlistRule` (around line 273) and the approval view interface (around line 285):

```ts
export interface AllowlistRule {
  pattern: string
  argPattern?: string
  label?: string
  // Where the rule came from (issue #185). 'learned' rules are recorded from an
  // allow-always in chat; the rest are declarative.
  source?: 'builtin' | 'template' | 'user' | 'learned'
}
```

and add to the approval view type beside `allowlistOwned`:

```ts
  allowlistLearned: AllowlistRule[]
```

- [ ] **Step 7: Type-check the web build**

Run: `cd web && npm run build`

Expected: build succeeds.

- [ ] **Step 8: Document the shape**

In `docs/cubepilot/api.md`, in the section listing the approval view fields (around lines 520-540), add `source` to the `allowlist` entry shape and document the new field. Match the file's existing table/bullet style — do not restructure it. At minimum:

- `allowlist` entries now carry `source`: `builtin | template | user | learned`
- `allowlistLearned` is a new optional field listing the learned grants, present only when non-empty (same "omitted when empty" convention as `allowlist`/`allowlistOwned`)

- [ ] **Step 9: Commit**

```bash
git add internal/server/ web/src/api/types.ts docs/cubepilot/api.md
git commit -s -m "feat(api): tag allowlist rules with their provenance (issue #185)

The view could only say whether a rule was the user's or not, so the UI guessed
at origin from an ownership flag. Tag each rule with source
(builtin/template/user/learned) and add allowlistLearned for the groups that
need their own affordances. Additive only — no v1 field is removed or renamed.

Assisted-by: Claude Code"
```

---

### Task 7: Group the allowlist by provenance in the UI

**Files:**
- Modify: `web/src/views/AgentView.tsx:542-585` (the allowlist card body)
- Modify: `web/src/views/AgentView.tsx` (the `withConfirmDefaults` normalizer, ~line 95)

**Interfaces:**
- Consumes: `ApprovalRule.source`, `ApprovalRule.allowlistLearned` from Task 6.
- Produces: no new exports; purely presentational.

- [ ] **Step 1: Normalize the new field**

In `web/src/views/AgentView.tsx`, in the function that normalizes the approval view (the one with the comment "The API may omit allowlist/allowlistOwned (null); normalize to []"), add:

```ts
    allowlistLearned: v.allowlistLearned ?? [],
```

- [ ] **Step 2: Render four groups**

Replace the block inside `{confirm?.approvalPolicy === 'Allowlist' ? (<> ... </>) : ( ... )}` that currently maps `confirm.allowlist` (lines ~545-583, the `<div style={{ display: 'flex', flexDirection: 'column', ... }}>` and its contents) with:

```tsx
                    <div style={{ display: 'flex', flexDirection: 'column', gap: 6, marginBottom: 8 }}>
                      {(
                        [
                          ['Your rules', 'user', true],
                          ['Learned from Allow always', 'learned', true],
                          ['From the template', 'template', false],
                          ['Platform safe commands', 'builtin', false],
                        ] as const
                      ).map(([title, source, editable]) => {
                        const rows = (confirm.allowlist || []).filter((r) => (r.source || 'builtin') === source)
                        if (!rows.length) return null
                        return (
                          <div key={source}>
                            <div className="label" style={{ marginBottom: 4 }}>{title}</div>
                            {rows.map((r) => (
                              <div key={ruleKey(r)} className="rule-row" style={{ display: 'block', padding: '8px 12px', marginBottom: 6 }}>
                                <div style={{ display: 'flex', alignItems: 'center', gap: 9 }}>
                                  <WarnIcon />
                                  <span className="mono" style={{ flex: 1, minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={allowlistLabel(r)}>
                                    {allowlistLabel(r)}
                                  </span>
                                  {editable && (
                                    <button className="btn" style={{ padding: '2px 8px', flex: 'none' }} disabled={confirmBusy} onClick={() => removeRule(ruleKey(r))}>
                                      Remove
                                    </button>
                                  )}
                                </div>
                                {r.argPattern ? (
                                  <div className="mono" title={r.argPattern} style={{ marginTop: 4, fontSize: 11, color: 'var(--muted)', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                                    argPattern: {r.argPattern}
                                  </div>
                                ) : null}
                              </div>
                            ))}
                          </div>
                        )
                      })}
                      {confirm.allowlist.length === 0 && (
                        <div style={{ color: 'var(--muted)', fontSize: 13 }}>Empty allowlist — every command asks.</div>
                      )}
                    </div>
```

- [ ] **Step 3: Stop materializing the inherited list on every edit**

`addRule` and `removeRule` both fall back to `confirm.allowlist` (the whole effective list) when nothing is owned. That fallback is what wrote the platform builtin into `spec` on the first edit — the fork the whole change removes. With `spec.allowlist` now purely additive, both should only ever touch `allowlistOwned`.

Extend the API client in `web/src/api/index.ts` (line 126):

```ts
  saveAgentApproval: (body: { approvalPolicy?: string; allowlist?: AllowlistRule[]; revokeGrants?: AllowlistRule[] }) =>
```

Extend `persistConfirm`:

```tsx
  // Every change PUTs the full desired owned state; the server treats an empty
  // list as clearing the hand-authored rules. revokeGrants drops learned
  // grants, which live in their own store (issue #185).
  async function persistConfirm(owned: AllowlistRule[] | null, policy?: string, revokeGrants?: AllowlistRule[]) {
    if (!confirm || confirmBusy) return
    setConfirmBusy(true)
    try {
      const pol = policy !== undefined ? policy : policySel
      const v = await api.saveAgentApproval({ approvalPolicy: pol, allowlist: owned ?? [], revokeGrants })
      setConfirm(withConfirmDefaults(v))
      setPolicySel(v.override || '')
    } catch (e) {
      showToast('Save confirmation config failed: ' + (e instanceof Error ? e.message : String(e)))
    } finally {
      setConfirmBusy(false)
    }
  }
```

Replace `addRule`'s base:

```tsx
  function addRule() {
    const pattern = ruleForm.pattern.trim()
    if (!pattern) {
      showToast('Command pattern is required')
      return
    }
    const entry: AllowlistRule = { pattern, argPattern: ruleForm.argPattern.trim() || undefined }
    // Only the hand-authored list: the effective list is a union now, so
    // copying it in would just duplicate what the platform already adds.
    const base = confirm ? confirm.allowlistOwned : []
    if (base.some((r) => ruleKey(r) === ruleKey(entry))) {
      showToast('That command is already on the allowlist')
      return
    }
    void persistConfirm([...base, entry])
    setRuleForm({ pattern: '', argPattern: '' })
  }
```

Replace `removeRule` so a learned grant revokes rather than rewriting the list:

```tsx
  function removeRule(key: string) {
    if (!confirm) return
    const learned = confirm.allowlistLearned.find((r) => ruleKey(r) === key)
    if (learned) {
      void persistConfirm(confirm.allowlistOwned, undefined, [learned])
      return
    }
    // Only hand-authored rules are removable: the platform builtin and the
    // template's rules are a floor (issue #185).
    void persistConfirm(confirm.allowlistOwned.filter((r) => ruleKey(r) !== key))
  }
```

- [ ] **Step 4: Check the build and lint**

Run: `cd web && npm run build && npm run lint 2>/dev/null || npm run build`

Expected: build succeeds with no TypeScript error.

- [ ] **Step 5: Verify by hand**

Run the portal, open Agent Config, and confirm: the four groups render in order, Remove shows only under "Your rules" and "Learned from Allow always", and the platform group has no Remove button.

- [ ] **Step 6: Commit**

```bash
git add web/src/views/AgentView.tsx
git commit -s -m "feat(web): group the allowlist by provenance (issue #185)

Replace the merged list plus an ownership pill with four groups — yours,
learned, template, platform — and only offer Remove where removal is meaningful.
Removal no longer materializes the inherited list as a side effect.

Assisted-by: Claude Code"
```

---

## Final verification

- [ ] `go vet ./... && go test ./...`
- [ ] `cd web && npm run build`
- [ ] `helm template cubepilot deploy/charts/cubepilot >/dev/null`

Then open the PR (see the `upstream-fork-pr` workflow): `go vet`/`go test` green, `helm template` clean, and the PR body should carry the fork scenario as the motivation — a user who clicked allow-always once keeps auto-passing a command the platform later hardened.
