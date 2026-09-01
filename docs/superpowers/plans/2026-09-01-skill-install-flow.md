# Skill install flow (#24) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A user one-click installs/uninstalls a platform-level skill from a new "Market" page; the mutation lands in the caller's `AgentInstance.spec.enabledSkills` and the #22 supervisor syncs the workspace (pull tar → verify sha256 → extract → OpenClaw hot-reload), with no reload mechanics reimplemented.

**Architecture:** The loading end is already shipped (#22). #24 adds (a) two POST endpoints that mutate the caller's `enabledSkills` (same pattern as `applyModelOverride`), (b) a Market page + `api` functions, (c) a repoint of the AgentView's skills toggles onto the real endpoints. The resolver's `empty enabledSkills = all enabled` semantics are preserved: the first uninstall materializes the allow-list.

**Tech Stack:** Go (net/http, controller-runtime client, `internal/api/v1alpha1`), React 19 + TypeScript + Vite.

## Global Constraints

- Branch `feat/issue24-skill-install-ui` (worktree `.claude/worktrees/feat-issue24-skill-install-ui`), based on `upstream/main` @ `861fa80` (contains merged #22 + #23).
- `make test` = `go vet` + unit tests, **excludes** `test/e2e`. Run after each backend task.
- `make web` (`cd web && npm run build` = `tsc -b` then `vite build`). Run after each frontend task.
- Commits: `git commit -s` + `Assisted-by: Claude Code` trailer; English messages.
- No new Go or npm dependencies.
- Do not reimplement #22: `syncSkills` / tar pull / extract / hot-reload are consumed as-is.
- The resolver's `enabledSkills` semantics are **unchanged** (empty = all enabled).

---

### Task 1: install / uninstall endpoints

**Files:**
- Modify: `internal/server/handlers_platform.go` (add handlers + helpers after the publish section)
- Modify: `internal/server/server.go` (routes after `/api/skills/{name}/publish`)
- Test: `internal/server/handlers_platform_test.go`

**Interfaces:**
- Produces: `POST /api/skills/{name}/install` and `POST /api/skills/{name}/uninstall` → `{"enabledSkills": []string}`.
- Consumed by: `web/src/api/index.ts` (`installSkill` / `uninstallSkill`, Task 2).

- [ ] **Step 1: Write the failing tests**

Append to `internal/server/handlers_platform_test.go`:

```go
// TestInstallSkill verifies POST /api/skills/{name}/install: appends on a
// non-empty set, is a no-op on the all-enabled baseline, and rejects unknown /
// unreachable skills and unprovisioned users.
func TestInstallSkill(t *testing.T) {
	li := internalTestInstance("li.ming", v1alpha1.DefaultAgentName)
	li.Spec.EnabledSkills = []string{"harbor"}
	s := platformTestServer(t, li,
		internalTestCap("harbor", "skills/harbor/v1.tar.gz"),
		internalTestCap("scan", "skills/scan/v1.tar.gz"))

	// Append to a non-empty set.
	rec := doReq(t, s.Handler(), http.MethodPost, "/api/skills/scan/install", "li.ming", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("install: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got := decode[struct {
		EnabledSkills []string `json:"enabledSkills"`
	}](t, rec)
	if len(got.EnabledSkills) != 2 || got.EnabledSkills[0] != "harbor" || got.EnabledSkills[1] != "scan" {
		t.Errorf("enabledSkills = %v, want [harbor scan]", got.EnabledSkills)
	}
	var inst v1alpha1.AgentInstance
	if err := s.cr.Get(t.Context(), client.ObjectKey{Name: li.Name}, &inst); err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if len(inst.Spec.EnabledSkills) != 2 || inst.Spec.EnabledSkills[1] != "scan" {
		t.Errorf("instance enabledSkills = %v, want [harbor scan]", inst.Spec.EnabledSkills)
	}

	// Idempotent re-install.
	rec = doReq(t, s.Handler(), http.MethodPost, "/api/skills/scan/install", "li.ming", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("re-install: status = %d", rec.Code)
	}
	again := decode[struct {
		EnabledSkills []string `json:"enabledSkills"`
	}](t, rec)
	if len(again.EnabledSkills) != 2 {
		t.Errorf("re-install changed the set: %v", again.EnabledSkills)
	}

	// All-enabled baseline (empty set): installing is a no-op that stays empty.
	s2 := platformTestServer(t, internalTestInstance("zhang.wei", v1alpha1.DefaultAgentName),
		internalTestCap("harbor", "skills/harbor/v1.tar.gz"))
	rec = doReq(t, s2.Handler(), http.MethodPost, "/api/skills/harbor/install", "zhang.wei", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("baseline install: status = %d", rec.Code)
	}
	base := decode[struct {
		EnabledSkills []string `json:"enabledSkills"`
	}](t, rec)
	if len(base.EnabledSkills) != 0 {
		t.Errorf("baseline install should stay empty (all-enabled): %v", base.EnabledSkills)
	}

	// Unknown skill -> 404.
	rec = doReq(t, s.Handler(), http.MethodPost, "/api/skills/nope/install", "li.ming", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown skill status = %d, want 404", rec.Code)
	}

	// Unreachable skill -> 409.
	unreach := internalTestCap("broken", "skills/broken/v1.tar.gz")
	unreach.Status.Phase = v1alpha1.SkillPhaseUnreachable
	s3 := platformTestServer(t, li, unreach)
	rec = doReq(t, s3.Handler(), http.MethodPost, "/api/skills/broken/install", "li.ming", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("unreachable skill status = %d, want 409", rec.Code)
	}

	// No instance -> 409.
	s4 := platformTestServer(t, internalTestCap("harbor", "skills/harbor/v1.tar.gz"))
	rec = doReq(t, s4.Handler(), http.MethodPost, "/api/skills/harbor/install", "nobody", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("no-instance status = %d, want 409", rec.Code)
	}
}

// TestUninstallSkill verifies POST /api/skills/{name}/uninstall: removes from a
// non-empty set, materializes the allow-list from the all-enabled baseline, is
// idempotent, and rejects unprovisioned users.
func TestUninstallSkill(t *testing.T) {
	li := internalTestInstance("li.ming", v1alpha1.DefaultAgentName)
	li.Spec.EnabledSkills = []string{"harbor", "scan"}
	s := platformTestServer(t, li,
		internalTestCap("harbor", "skills/harbor/v1.tar.gz"),
		internalTestCap("scan", "skills/scan/v1.tar.gz"))

	// Remove one from a non-empty set.
	rec := doReq(t, s.Handler(), http.MethodPost, "/api/skills/scan/uninstall", "li.ming", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("uninstall: status = %d", rec.Code)
	}
	got := decode[struct {
		EnabledSkills []string `json:"enabledSkills"`
	}](t, rec)
	if len(got.EnabledSkills) != 1 || got.EnabledSkills[0] != "harbor" {
		t.Errorf("enabledSkills = %v, want [harbor]", got.EnabledSkills)
	}

	// Idempotent re-uninstall of an absent skill.
	rec = doReq(t, s.Handler(), http.MethodPost, "/api/skills/scan/uninstall", "li.ming", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("re-uninstall: status = %d", rec.Code)
	}

	// All-enabled baseline: uninstall materializes the visible skills minus the
	// one, so a later publish does not re-enable it.
	s2 := platformTestServer(t, internalTestInstance("zhang.wei", v1alpha1.DefaultAgentName),
		internalTestCap("harbor", "skills/harbor/v1.tar.gz"),
		internalTestCap("scan", "skills/scan/v1.tar.gz"))
	rec = doReq(t, s2.Handler(), http.MethodPost, "/api/skills/harbor/uninstall", "zhang.wei", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("baseline uninstall: status = %d", rec.Code)
	}
	base := decode[struct {
		EnabledSkills []string `json:"enabledSkills"`
	}](t, rec)
	if len(base.EnabledSkills) != 1 || base.EnabledSkills[0] != "scan" {
		t.Errorf("materialized = %v, want [scan]", base.EnabledSkills)
	}

	// No instance -> 409.
	s3 := platformTestServer(t, internalTestCap("harbor", "skills/harbor/v1.tar.gz"))
	rec = doReq(t, s3.Handler(), http.MethodPost, "/api/skills/harbor/uninstall", "nobody", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("no-instance status = %d, want 409", rec.Code)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/server/ -run 'TestInstallSkill|TestUninstallSkill' -v`
Expected: FAIL — both hit 404 (routes not registered).

- [ ] **Step 3: Implement the endpoints**

In `internal/server/handlers_platform.go`, after the `handlePublishSkill` section, add:

```go
// ---- Skill install/uninstall (design §3.2: enabledSkills subset) ----

// toggleSkillInstance fetches the caller's AgentInstance (one per user +
// DefaultAgentName). ok=false when the user has not provisioned yet.
func (s *Server) toggleSkillInstance(ctx context.Context, user string) (v1alpha1.AgentInstance, bool, error) {
	name := k8s.InstanceName(user, v1alpha1.DefaultAgentName)
	var inst v1alpha1.AgentInstance
	if err := s.cr.Get(ctx, client.ObjectKey{Name: name}, &inst); err != nil {
		if apierrors.IsNotFound(err) {
			return inst, false, nil
		}
		return inst, false, err
	}
	return inst, true, nil
}

// visibleSkillNames lists the currently available Platform skills (Unreachable
// excluded) — the baseline the resolver enables when enabledSkills is empty.
func (s *Server) visibleSkillNames(ctx context.Context) ([]string, error) {
	var list v1alpha1.SkillList
	if err := s.cr.List(ctx, &list); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list.Items))
	for _, sk := range list.Items {
		if sk.Status.Phase == v1alpha1.SkillPhaseUnreachable {
			continue
		}
		names = append(names, sk.Name)
	}
	return names, nil
}

func containsSkill(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

// withoutSkill returns names minus name (order preserved).
func withoutSkill(names []string, name string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n != name {
			out = append(out, n)
		}
	}
	return out
}

// handleInstallSkill serves POST /api/skills/{name}/install — adds the skill
// to the caller's enabledSkills. Empty enabledSkills means "all enabled"
// (resolver baseline), so installing an already-enabled skill is a no-op.
func (s *Server) handleInstallSkill(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST required"})
		return
	}
	name := r.PathValue("name")
	if name == "" || s.cr == nil {
		http.NotFound(w, r)
		return
	}
	var skillCR v1alpha1.Skill
	if err := s.cr.Get(r.Context(), client.ObjectKey{Name: name}, &skillCR); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	if skillCR.Status.Phase == v1alpha1.SkillPhaseUnreachable {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "skill is unreachable (missing content)"})
		return
	}
	inst, ok, err := s.toggleSkillInstance(r.Context(), s.userOf(r))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if !ok {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "provision your instance on the Agent Config page first"})
		return
	}
	// Idempotent: already enabled, or the all-enabled baseline, stays unchanged.
	if len(inst.Spec.EnabledSkills) == 0 || containsSkill(inst.Spec.EnabledSkills, name) {
		writeJSON(w, http.StatusOK, map[string]any{"enabledSkills": inst.Spec.EnabledSkills})
		return
	}
	inst.Spec.EnabledSkills = append(inst.Spec.EnabledSkills, name)
	if err := s.cr.Update(r.Context(), &inst); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabledSkills": inst.Spec.EnabledSkills})
}

// handleUninstallSkill serves POST /api/skills/{name}/uninstall — removes the
// skill from the caller's enabledSkills. On the all-enabled baseline (empty
// set) the allow-list is materialized to the visible skills minus the one, so
// only the uninstalled skill stops loading and later publishes do not
// re-enable it.
func (s *Server) handleUninstallSkill(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST required"})
		return
	}
	name := r.PathValue("name")
	if name == "" || s.cr == nil {
		http.NotFound(w, r)
		return
	}
	inst, ok, err := s.toggleSkillInstance(r.Context(), s.userOf(r))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if !ok {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "provision your instance on the Agent Config page first"})
		return
	}
	if len(inst.Spec.EnabledSkills) == 0 {
		names, err := s.visibleSkillNames(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		inst.Spec.EnabledSkills = withoutSkill(names, name)
	} else {
		inst.Spec.EnabledSkills = withoutSkill(inst.Spec.EnabledSkills, name)
	}
	if err := s.cr.Update(r.Context(), &inst); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabledSkills": inst.Spec.EnabledSkills})
}
```

In `internal/server/server.go`, after the publish route line:

```go
	mux.HandleFunc("/api/skills/{name}/install", s.handleInstallSkill)
	mux.HandleFunc("/api/skills/{name}/uninstall", s.handleUninstallSkill)
```

- [ ] **Step 4: Run it to verify it passes**

Run: `go test ./internal/server/`
Expected: PASS (both new tests + all existing server tests).

- [ ] **Step 5: Commit**

```bash
git add internal/server/handlers_platform.go internal/server/server.go internal/server/handlers_platform_test.go
git commit -s -m "feat(server): install/uninstall skills via enabledSkills mutation (#24)" -m "Assisted-by: Claude Code"
```

---

### Task 2: Frontend API + Market page + navigation

**Files:**
- Modify: `web/src/api/index.ts` (add `installSkill` / `uninstallSkill`)
- Create: `web/src/utils/skills.ts` (shared enabledSkills extraction)
- Create: `web/src/views/MarketView.tsx`
- Modify: `web/src/App.tsx` (Market nav + route)

**Interfaces:**
- Consumes: `POST /api/skills/{name}/install` + `/uninstall` (Task 1).
- Produces: `api.installSkill(name)`, `api.uninstallSkill(name)`, `enabledSkillsFromInstances(instances)`; `/market` route.
- Consumed by: `AgentView` (Task 3, for the same `enabledSkillsFromInstances`).

- [ ] **Step 1: Add the API functions**

In `web/src/api/index.ts`, after the `publishSkill:` entry:

```ts
  installSkill: (name: string) =>
    apiFetch<{ enabledSkills: string[] }>(`/api/skills/${encodeURIComponent(name)}/install`, { method: 'POST' })
      .then((d) => d.enabledSkills),
  uninstallSkill: (name: string) =>
    apiFetch<{ enabledSkills: string[] }>(`/api/skills/${encodeURIComponent(name)}/uninstall`, { method: 'POST' })
      .then((d) => d.enabledSkills),
```

- [ ] **Step 2: Add the shared helper**

Create `web/src/utils/skills.ts`:

```ts
// Shared skill helpers for the market page and the Agent Config skills toggles.
import type { PlatformObject } from '@/api/types'

// enabledSkillsFromInstances extracts the caller's AgentInstance enabledSkills.
// Empty (or a missing instance) is the resolver's "all enabled" baseline.
export function enabledSkillsFromInstances(instances: PlatformObject[]): string[] {
  const es = instances[0]?.spec?.enabledSkills
  return Array.isArray(es) ? (es as string[]) : []
}
```

- [ ] **Step 3: Write `web/src/views/MarketView.tsx`**

```tsx
// Market view -- browse/search platform skills and one-click install or
// uninstall (issue #24). The mutation lands in the caller's
// AgentInstance.spec.enabledSkills; the #22 supervisor syncs the workspace.
import { useEffect, useMemo, useState } from 'react'
import { api } from '@/api'
import type { PlatformObject } from '@/api/types'
import { enabledSkillsFromInstances } from '@/utils/skills'
import { showToast } from '@/stores/toast'

// specStr reads a string field from the CR spec (Record<string, unknown>).
function specStr(sk: PlatformObject, key: string): string {
  const v = sk.spec?.[key]
  return typeof v === 'string' ? v : ''
}

export default function MarketView() {
  const [skills, setSkills] = useState<PlatformObject[]>([])
  const [enabled, setEnabled] = useState<string[]>([])
  const [hasInstance, setHasInstance] = useState(false)
  const [query, setQuery] = useState('')
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')

  // Empty enabledSkills means "all enabled" per the resolver baseline.
  const isInstalled = (name: string) => enabled.length === 0 || enabled.includes(name)

  async function load() {
    try {
      const [skillList, instances] = await Promise.all([api.listSkills(), api.listInstances()])
      setSkills(skillList.filter((sk) => sk.status?.phase !== 'Unreachable'))
      setEnabled(enabledSkillsFromInstances(instances))
      setHasInstance(instances.length > 0)
      setError('')
    } catch (e) {
      console.error('loadMarket', e)
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    load()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const visible = useMemo(() => {
    const q = query.trim().toLowerCase()
    return skills.filter((sk) => {
      if (!q) return true
      return (
        (specStr(sk, 'displayName') || '').toLowerCase().includes(q) ||
        sk.metadata.name.toLowerCase().includes(q) ||
        (specStr(sk, 'description') || '').toLowerCase().includes(q)
      )
    })
  }, [skills, query])

  async function toggle(sk: PlatformObject) {
    const name = sk.metadata.name
    const installing = !isInstalled(name)
    try {
      const next = installing ? await api.installSkill(name) : await api.uninstallSkill(name)
      setEnabled(next)
      showToast(installing ? `Skill '${name}' installed` : `Skill '${name}' uninstalled`)
    } catch (e) {
      showToast((installing ? 'Install' : 'Uninstall') + ' failed: ' + (e instanceof Error ? e.message : String(e)))
    }
  }

  return (
    <div className="view active">
      <div className="view-head">
        <div>
          <div className="view-title">Market</div>
          <div className="view-desc">Browse and install platform skills - applied to your instance workspace (issue #24)</div>
        </div>
      </div>

      {!hasInstance && (
        <div className="card" style={{ marginBottom: 14 }}>
          <div className="card-pad">
            <div style={{ color: 'var(--danger)', fontSize: 13 }}>
              You have no instance yet - provision one on the Agent Config page before installing skills.
            </div>
          </div>
        </div>
      )}

      <div className="card">
        <div className="card-head">
          <span className="card-title">Skill Market</span>
          <span className="card-hint">Platform-visible skills - installed into your instance workspace on the next sync</span>
        </div>
        <div className="card-pad">
          <input
            className="input"
            placeholder="Search skills..."
            aria-label="Search skills"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
          />
        </div>
        <div className="card-pad" style={{ paddingTop: 4, paddingBottom: 10 }}>
          {error ? (
            <div style={{ color: 'var(--danger)', padding: '8px 0' }}>Failed to load skills: {error}</div>
          ) : loading ? (
            <div style={{ color: 'var(--muted)', padding: '8px 0' }}>Loading...</div>
          ) : visible.length ? (
            visible.map((sk) => {
              const installed = isInstalled(sk.metadata.name)
              const phase = typeof sk.status?.phase === 'string' ? sk.status.phase : ''
              return (
                <div key={sk.metadata.name} className="toggle">
                  <div className="toggle-info">
                    <div className="toggle-title">
                      {specStr(sk, 'displayName') || sk.metadata.name}{' '}
                      <span className="mono" style={{ color: 'var(--muted)', fontWeight: 500 }}>{sk.metadata.name}</span>
                    </div>
                    <div className="toggle-desc">{specStr(sk, 'description') || 'No description'}</div>
                  </div>
                  <span className={`pill ${phase === 'Available' ? 'success' : 'neutral'}`}>{phase || 'available'}</span>
                  <button
                    className={installed ? 'btn' : 'btn primary'}
                    disabled={!hasInstance}
                    onClick={() => toggle(sk)}
                  >
                    {installed ? 'Uninstall' : 'Install'}
                  </button>
                </div>
              )
            })
          ) : (
            <div style={{ color: 'var(--muted)', padding: '8px 0' }}>No skills match your search</div>
          )}
        </div>
      </div>
    </div>
  )
}
```

- [ ] **Step 4: Wire `web/src/App.tsx`**

1. Add `import MarketView from '@/views/MarketView'` to the view imports.
2. Add `market: 'Market',` to `VIEW_TITLES`.
3. Add a `MarketIcon` component next to the other icons:

```tsx
function MarketIcon() {
  return (
    <svg className="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round">
      <path d="M3 9l1.5-5h15L21 9M3 9a3 3 0 0 0 6 0 3 3 0 0 0 6 0 3 3 0 0 0 6 0M4 9v11h16V9M9 20v-6h6v6" />
    </svg>
  )
}
```

4. Add a nav item after the "Skills" `NavLink`:

```tsx
          <NavLink to="/market" className={navCls}>
            <MarketIcon />
            <span>Market</span>
          </NavLink>
```

5. Add the route next to the other `<Route>` entries:

```tsx
            <Route path="/market" element={<MarketView />} />
```

- [ ] **Step 5: Type-check + build**

Run: `npm --prefix web run build`
Expected: PASS — `tsc -b` + Vite emit `web/dist` with no errors.

- [ ] **Step 6: Commit**

```bash
git add web/src/api/index.ts web/src/utils/skills.ts web/src/views/MarketView.tsx web/src/App.tsx
git commit -s -m "feat(web): add Market page for install/uninstall (#24)" -m "Assisted-by: Claude Code"
```

---

### Task 3: Repoint the AgentView skills toggles

**Files:**
- Modify: `web/src/views/AgentView.tsx`

**Interfaces:**
- Consumes: `api.installSkill` / `api.uninstallSkill` / `api.listSkills` / `api.listInstances` (Task 2), `enabledSkillsFromInstances` (Task 2).

- [ ] **Step 1: Load the real enabled set**

In `web/src/views/AgentView.tsx`:

1. Add `enabledSkillsFromInstances` to the imports from `@/utils/skills`.
2. Replace the skills load inside `loadAgentView` so the toggle list comes from
   the platform skills + the real `enabledSkills` (empty = all on):

```tsx
  // The kubectl-platform builtin is shown locked in the System section above;
  // keep it out of the toggle list.
  const LOCKED_SYSTEM_SKILLS = ['kubectl-platform']

  async function loadSkills() {
    try {
      const [skillList, instances] = await Promise.all([api.listSkills(), api.listInstances()])
      const enabled = enabledSkillsFromInstances(instances)
      const on = enabled.length === 0 ? new Set(skillList.map((s) => s.metadata.name)) : new Set(enabled)
      const toggleable = skillList.filter((sk) => !LOCKED_SYSTEM_SKILLS.includes(sk.metadata.name))
      setSkills(toggleable.map((sk) => ({ name: sk.metadata.name, enabled: on.has(sk.metadata.name) })))
    } catch (e) {
      console.error('loadSkills', e)
    }
  }
```

In `loadAgentView`, replace `setSkills(c.skills || [])` with a `await loadSkills()` call:

```tsx
  async function loadAgentView() {
    try {
      const [c, st] = await Promise.all([api.agentConfig(), api.agentStatus()])
      setCfg(c)
      setStatus(st)
      await loadTemplate()
      await loadSkills()
    } catch (e) {
      console.error('loadAgentView', e)
    }
  }
```

- [ ] **Step 2: Toggle through the real endpoints**

Replace the local-only `toggleSkill`:

```tsx
  // Toggle writes the real enabledSkills (install/uninstall); the supervisor
  // picks it up on its next sync. Errors surface via toast.
  async function toggleSkill(name: string) {
    const cur = skills.find((s) => s.name === name)
    if (!cur) return
    try {
      if (cur.enabled) {
        await api.uninstallSkill(name)
      } else {
        await api.installSkill(name)
      }
      await loadSkills()
    } catch (e) {
      showToast('Skill update failed: ' + (e instanceof Error ? e.message : String(e)))
    }
  }
```

- [ ] **Step 3: Drop the inert store `skills` from the save**

The store `Skills` preference is not applied anywhere (the resolver reads
`enabledSkills`); stop sending it so the toggle list's only source of truth is
`enabledSkills`. In `saveAgentConfig`, change the call to:

```tsx
      await api.saveAgentConfig({ model: cfg.model, systemPrompt: cfg.systemPrompt })
```

(`AgentConfig.skills` is optional in `web/src/api/types.ts`, so this is type-safe.)

- [ ] **Step 4: Type-check + build**

Run: `npm --prefix web run build`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/src/views/AgentView.tsx
git commit -s -m "feat(web): wire AgentConfig skill toggles to install/uninstall (#24)" -m "Assisted-by: Claude Code"
```

---

### Task 4: Docs

**Files:**
- Modify: `docs/cubepilot/implementation-status.md` (skill-market entry)

- [ ] **Step 1: Mark the install flow implemented**

In `docs/cubepilot/implementation-status.md`, update the skill-market bullet
(its heading says "发布 UI 已建，安装 UI 未建") so the phase-1 market is fully
done: the Portal "Market" page + `POST /api/skills/{name}/install` /
`/uninstall` mutate `AgentInstance.spec.enabledSkills` (empty = all enabled;
first uninstall materializes the allow-list), the Agent Config skills toggles
now write the real `enabledSkills`, and the #22 supervisor syncs the workspace.
Remaining for the epic: object-storage S3 source and user-private skills
(`visibility: User`), both phase 2.

- [ ] **Step 2: Commit**

```bash
git add docs/cubepilot/implementation-status.md
git commit -s -m "docs(skill): mark install flow implemented (#24)" -m "Assisted-by: Claude Code"
```

---

### Final verification

- [ ] **Step 1: Full Go suite**

Run: `make test`
Expected: `go vet ./...` + all unit tests pass.

- [ ] **Step 2: Frontend build**

Run: `make web`
Expected: `tsc -b` + `vite build` pass.

- [ ] **Step 3: Lint**

Run: `golangci-lint run ./...`
Expected: 0 issues.

- [ ] **Step 4: Route sanity**

Run: `grep -n "skills" internal/server/server.go`
Expected: `GET /api/skills`, `POST /api/skills/{name}/publish`, `POST
/api/skills/{name}/install`, `POST /api/skills/{name}/uninstall`, and `GET
/internal/skills/{name}/tar`.

- [ ] **Step 5: Manual smoke (Portal)**

`make deploy` + `make web`, then on the Portal: open **Market** → search
filters the list → uninstall a skill → the row flips to "Install" (and the
instance's `enabledSkills` is now an explicit allow-list) → re-install restores
it; on the **Agent Config** page the skills toggles now reflect the same set.
Full end-to-end (workspace extraction + hot reload) runs in CI (`make
test-e2e`).
