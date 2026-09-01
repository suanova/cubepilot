# Design: Skill install flow (issue #24)

Date: 2026-09-01
Status: draft (pending review)
Goal owner: zhujian

## 1. Context & goal

Issue #22 delivered the **loading end**: the supervisor pulls each enabled
skill's tar over HTTP (`GET /internal/skills/{name}/tar`), verifies
`source.sha256`, and extracts it into `workspace/skills/<name>/` (stage-then-
swap, per-skill `.sha256` marker, stale cleanup); OpenClaw hot-reloads via file
watch. Issue #23 delivered the publish face. #24 is the **install face**: a
user browses/searches the skill market and one-click installs/uninstalls a
platform-level skill, which mutates the instance's `enabledSkills` so the
already-shipped loading end takes effect immediately. None of the pull /
extract / hot-reload mechanics is reimplemented.

### Decisions (brainstorming, 2026-09-01)

| Decision | Choice | Why |
|---|---|---|
| Endpoint shape | **`POST /api/skills/{name}/install`** + **`POST /api/skills/{name}/uninstall`** | Resource-style, mirrors the publish endpoint; matches the issue's "install endpoint / uninstall"; the target instance is implicitly the caller's own (like `applyModelOverride`) |
| Market UI | **New "Market" page** (`/market`, nav entry) | #23's Skills page is the publish concern; install browsing gets its own clean page |
| AgentView toggle | **Repointed to install/uninstall** (writes real `enabledSkills`) | Today the toggle saves a global store preference that nothing applies — a UX trap; making it call the same endpoints removes the trap |
| `enabledSkills` semantics | **Keep `empty = all enabled`** (resolver §3.2 unchanged) | Least change; a fresh instance gets every platform skill. The **first uninstall materializes** the allow-list (visible skills minus the one) so a newly published skill does not auto-enable afterwards |
| Builtins | Installable/uninstallable like any platform skill | Consistent with the resolver's `enabledSkills` restrict; the AgentView "System" lock badge stays informational |
| Store `Skills []SkillToggle` | No longer the toggle source of truth; **field kept** | Avoids rippling the store contract; the toggle list now comes from `enabledSkills` |

## 2. Backend

### 2.1 `POST /api/skills/{name}/install`

- Resolve the caller's `AgentInstance` (`k8s.InstanceName(user, DefaultAgentName)`).
  No instance → **409** "provision your instance on the Agent Config page first".
- Look up the Skill CR. Unknown → **404**. `status.phase == Unreachable` → **409**
  (its tar is missing/moved; installing would be a no-op).
- Idempotent: if `name` is already in `spec.enabledSkills` **or** the set is
  empty (the "all enabled" baseline — `name` is on by definition), return the
  set unchanged. Materializing to `[name]` here would wrongly disable every
  other skill. Only a non-empty set without `name` appends it.
- Update the CR (`s.cr.Update`), return the updated `enabledSkills`.

### 2.2 `POST /api/skills/{name}/uninstall`

- Resolve the caller's `AgentInstance`. No instance → **409** (nothing to
  uninstall from).
- Remove `name` from `spec.enabledSkills` (idempotent — a missing entry is
  fine). **Empty-set materialization**: if the set was empty (baseline "all
  enabled"), list the currently visible Platform skills (not `Unreachable`) and
  set `enabledSkills` to every name **except** the one being uninstalled —
  so the removed skill is the only one that stops loading, and future
  publishes do not auto-enable.
- Update the CR, return the updated `enabledSkills`.

Both handlers reuse the `applyModelOverride` shape (fetch `AgentInstance` by
`userOf(r)` + `DefaultAgentName`, mutate `spec`, `Update`). The enabled set is
per-user (per-instance), so no store involvement.

## 3. Frontend

### 3.1 `web/src/api/index.ts`

```ts
installSkill: (name: string) =>
  apiFetch<{ enabledSkills: string[] }>(`/api/skills/${encodeURIComponent(name)}/install`, { method: 'POST' })
    .then((d) => d.enabledSkills),
uninstallSkill: (name: string) =>
  apiFetch<{ enabledSkills: string[] }>(`/api/skills/${encodeURIComponent(name)}/uninstall`, { method: 'POST' })
    .then((d) => d.enabledSkills),
```

### 3.2 `web/src/views/MarketView.tsx` (new)

- Load `GET /api/skills` (hide `phase=Unreachable`) + the caller's
  `enabledSkills` via `GET /api/instances` (own instance → `spec.enabledSkills`;
  empty = all installed).
- Client-side search box: filters by displayName / name / description.
- Per skill row: displayName, mono name, description, phase pill, and an
  **Install / Uninstall** button toggling membership; after the call, update
  the local enabled set from the response and refresh.
- No instance yet → banner "Provision your instance on the Agent Config page
  first" and disable the buttons.

### 3.3 `web/src/views/AgentView.tsx`

Repoint the "Platform Skills - Skill Catalog" toggles to the real mechanism:
the toggle list loads from `enabledSkills` (same `GET /api/instances` read,
empty = all on), and each switch calls `installSkill` / `uninstallSkill`
instead of only mutating local state + saving to the store. `saveAgentConfig`
continues to persist model / systemPrompt; the store `Skills` preference is no
longer the toggle source.

### 3.4 `web/src/App.tsx`

Add the "Market" nav item + `<Route path="/market" element={<MarketView />} />`.

## 4. Error handling

- 404 unknown skill, 409 Unreachable, 409 no instance — all surfaced via the
  existing `ApiError` → toast.
- Idempotent install/uninstall: toggling an already-applied state returns 200
  with the unchanged set (no error noise).
- Market page shows the no-instance banner instead of erroring.

## 5. Testing

- `internal/server`: `TestInstallSkill` / `TestUninstallSkill` — add/remove,
  idempotency, empty-set handling (install on the all-enabled baseline is a
  no-op; uninstall on the baseline materializes the allow-list minus the
  skill), unknown skill → 404, Unreachable → 409, no instance → 409, and the
  returned `enabledSkills`.
- Frontend: `make web` (tsc + vite) green; manual `npm run dev` check of the
  market page + the repointed AgentView toggle.

## 6. Out of scope

- User-private skills (`visibility: User`), S3 source — phase 2.
- Supervisor pull / extract / hot-reload — shipped in #22.
- Removing the store `Skills []SkillToggle` field — the AgentView stops relying
  on it, but the field stays to avoid rippling the store contract.
- Making builtin skills non-removable — the resolver has no such concept today;
  would be a separate product decision.

## 7. Docs to update

- `docs/cubepilot/implementation-status.md`: mark the install flow implemented
  (market UI + install/uninstall endpoints); the skill-market entry becomes
  fully done for phase 1.
