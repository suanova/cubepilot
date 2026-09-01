# Design: Skill publish flow (issue #23)

Date: 2026-09-01
Status: draft (pending review)
Goal owner: zhujian

## 1. Context & goal

Issue #22 delivered the publish **backend**: `publishSkill` writes a gzip tar
atomically into the shared repository (versioned `v1`/`v2`, sha256 backfill),
upserts the Skill CRD (`status.phase=Available`), and the API self-seeds the
builtins through the same primitive. #23 is the **user-facing layer** of the
publish flow: a module owner uploads a skill directory on the Portal "Skills"
page and it is published to the market (Platform visibility, installable by
#24). None of the backend mechanics (atomic write / versioning / sha256 / CRD
upsert) is reimplemented — #23 only adds the upload face.

### Decisions (brainstorming, 2026-09-01)

| Decision | Choice | Why |
|---|---|---|
| Tar packaging | **Browser-side**: directory picker (`webkitdirectory`) → fflate `tarSync` + `gzipSync` | Matches the AC "upload a skill directory → tar packaged"; no server-side packing step; keeps the backend a thin wrapper. Cost: one ~7 KB frontend dependency (`fflate`, no transitive deps) |
| Publish endpoint | **New user-facing `POST /api/skills/{name}/publish`**; **delete** `/internal/skills/{name}/publish` | nginx proxies only `/api/` (web/nginx.conf), so the Portal cannot reach `/internal/*`; and the internal endpoint has no in-cluster consumer after #22 (seed calls `publishSkill` directly, supervisor only GETs the tar) → a single publish face |
| Validation | `ValidateTar` → **`ValidateSkillTar`** (gzip tar **+ top-level `SKILL.md`**) | The user-facing flow must reject a wrong-folder upload before anything is persisted |
| Publisher provenance | **`cubepilot/publisher` annotation** on the Skill CR, from `X-CubePilot-User` (`system` for the builtin seed) | The issue's "auth'd wrapper" concretely manifests as publish provenance in a no-auth phase 1; audit-friendly (M5 spirit) |
| Visibility | **Forced `Platform`** (phase 1) | Same as #22; `User`/`Tenant` are phase 2 |
| Page | New **"Skills"** nav entry + `/skills` route | #24 (market browse/install) extends the same page later |

## 2. Backend

### 2.1 `internal/skill`: `ValidateSkillTar`

Add next to the existing `ValidateTar`:

```go
// ValidateSkillTar reports whether data is a publishable skill archive: a
// readable gzip tar whose root contains SKILL.md. The user-facing publish
// endpoint uses this to reject a wrong-folder upload before it is persisted
// (the internal seed path already guarantees SKILL.md).
func ValidateSkillTar(data []byte) error
```

Implementation: `gzip.NewReader` → `tar.NewReader` → walk entries; require the
tar to be readable (`ValidateTar` semantics) **and** at least one entry whose
clean name is exactly `SKILL.md` at the root (i.e. no directory prefix).

### 2.2 `internal/server`: user-facing publish handler

`handlePublishSkill` moves from `/internal/skills/{name}/publish` to
**`POST /api/skills/{name}/publish`** (route changed in `server.go`; the tar
route `/internal/skills/{name}/tar` stays — the in-cluster supervisor pulls
it). The handler is the existing logic, tightened for the user-facing flow:

- Method `POST`; `name` (path) and `displayName` (query) required → 400.
- `visibility` query is **ignored in favor of the forced Platform default**:
  if supplied and not `Platform` → 400 ("phase 1 supports only
  visibility=Platform"). There is no way for a Portal upload to publish at any
  other visibility.
- `builtin` query is **dropped** — user uploads are never builtin.
- Body read with `io.LimitReader(r.Body, maxSkillTarSize+1)`; oversized → 413;
  empty → 400; `skill.ValidateSkillTar` failure → 400 (covers both malformed
  archives and missing `SKILL.md`).
- Call `s.publishSkill(ctx, name, displayName, description, Platform, body, s.userOf(r))`
  → 200 with the Skill CR.

### 2.3 `publishSkill` records the publisher

`publishSkill` gains a `publisher string` parameter; the CRD upsert (both the
create and update branches) sets the annotation:

```go
func addPublisherAnnotation(skillCR *v1alpha1.Skill, publisher string) {
    if skillCR.Annotations == nil {
        skillCR.Annotations = map[string]string{}
    }
    skillCR.Annotations["cubepilot/publisher"] = publisher
}
```

`internal/server/skill_seed.go` passes `"system"` for the builtin seed.

## 3. Frontend

### 3.1 Dependency

Add `fflate` to `web/package.json` (~7 KB, no transitive deps). Provides
`tarSync` (ustar) and `gzipSync` — the two primitives the pack step needs.

### 3.2 `web/src/utils/pack.ts` (new)

`packSkillDir(files: File[]): Uint8Array`:

- Reject when the selection has no `SKILL.md` at the root (client-side
  pre-check, before any upload): use `file.webkitRelativePath`, take the
  segment after the first `/` as the in-tar path, require a file whose path is
  exactly `SKILL.md`.
- Read each `File` → `arrayBuffer()` → `Uint8Array`.
- Build `{ path, data }[]`, then `gzipSync(tarSync(files))` → the gzip tar
  bytes the API expects.

The `name` slug (CR name) defaults to the picked directory's name and stays
editable in the form.

### 3.3 `web/src/api/index.ts`

```ts
publishSkill: (name: string, opts: { displayName: string; description?: string }, tar: Uint8Array) =>
  apiFetch<PlatformObject>(
    `/api/skills/${encodeURIComponent(name)}/publish?displayName=${encodeURIComponent(opts.displayName)}${opts.description ? `&description=${encodeURIComponent(opts.description)}` : ''}`,
    { method: 'POST', headers: { 'Content-Type': 'application/gzip' }, body: tar },
  ),
```

`apiFetch` already spreads arbitrary headers and sends the raw body; the
response (the Skill CR JSON) is decoded as before.

### 3.4 `web/src/views/SkillsView.tsx` (new)

"Skills" page, two cards following the existing `AgentView` patterns
(`.view`, `.card`, `.card-head`, `.input`, `.btn primary`, `.pill`, `.mono`,
toast via `showToast`):

- **Publish a Skill**: inputs `name` (slug, prefilled from the picked
  directory), `displayName` (required), `description`; `<input type="file"
  webkitdirectory>` folder picker; on submit → `packSkillDir(files)` →
  `api.publishSkill(...)` → success toast (e.g. "Skill 'harbor' published")
  → refresh the list; `ApiError` message → error toast.
- **Published Skills**: `api.listSkills()` (`GET /api/skills`, already
  exists) — displayName, mono `name`, `visibility` + `phase` pills; empty
  state "No published skills". Reloaded on mount and after each publish.

### 3.5 `web/src/App.tsx`

Add the "Skills" nav item (icon + label) and `<Route path="/skills"
element={<SkillsView />} />`; `VIEW_TITLES.skills = 'Skills'`.

## 4. Error handling

- **Frontend pre-check**: selection without root `SKILL.md` is rejected before
  any upload (friendly toast).
- **Backend**: 400 missing name/displayName, 400 invalid tar or missing
  `SKILL.md`, 400 non-Platform visibility, 413 oversized (> 10 MB), 500 write/
  k8s failure. All surfaced in the UI via the existing `ApiError` → toast.
- **Idempotency**: unchanged from #22 — `ResolveVersion` returns the existing
  `vN` when the sha256 matches (re-publish of identical content is a no-op
  write + CRD re-apply); changed content writes the next version.

## 5. Testing

- `internal/skill`: `TestValidateSkillTar` — valid (with root `SKILL.md`)
  passes; tar without `SKILL.md` fails; non-gzip fails.
- `internal/server`: `TestPublishSkill` (the existing `TestInternalPublishSkill`
  moved to `/api/skills/{name}/publish`) — success (v1 path, sha256 backfill,
  phase Available, `cubepilot/publisher` = header user), idempotent re-publish
  (same version), changed content → v2, missing displayName → 400, tar without
  `SKILL.md` → 400 (nothing persisted), malformed → 400, oversized → 413,
  `visibility=User` → 400.
- Frontend: `npm run build` (tsc + vite) green; manual `npm run dev` check of
  the publish flow against a local API.

## 6. Out of scope

- **#24 install flow** — market browse/search, install/uninstall UI, mutating
  `enabledSkills`. #24's backend (supervisor pull + extraction + OpenClaw
  hot-reload) is already shipped in #22.
- **Real publish authz** — phase 1 has no auth anywhere; only the identity
  header exists. The publisher annotation is provenance, not enforcement.
- **`visibility: User` private skills** — phase 2.
- **Server-side packaging** — the browser packs the tar; the endpoint only
  consumes gzip tar bytes (the #22 contract).

## 7. Docs to update

- `docs/superpowers/specs/2026-09-01-skill-crd-and-repository-design.md` §5:
  the publish endpoint note says "#23's Portal upload" uses `/internal/...` —
  update to the new `/api/skills/{name}/publish`.
- `docs/cubepilot/implementation-status.md`: mark the publish flow as
  implemented once merged.
