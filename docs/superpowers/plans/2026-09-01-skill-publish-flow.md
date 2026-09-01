# Skill publish flow (#23) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A module owner uploads a skill directory on the Portal "Skills" page; it is packed to a gzip tar in the browser, published via a new user-facing `POST /api/skills/{name}/publish`, and registered by the #22 `publishSkill` primitive (atomic write / versioning / sha256 / CRD upsert — none reimplemented).

**Architecture:** The publish backend already lives in `internal/server` (`publishSkill` + `internal/skill` repository primitives). #23 (a) moves the publish HTTP face from `/internal/skills/{name}/publish` to `/api/skills/{name}/publish` (nginx proxies only `/api/`), adding a root-`SKILL.md` check (`ValidateSkillTar`) and a `cubepilot/publisher` annotation; (b) packs the skill directory client-side with `fflate` and uploads the tar.

**Tech Stack:** Go (net/http, controller-runtime client, `internal/api/v1alpha1`), React 19 + TypeScript + Vite, `fflate`.

## Global Constraints

- Branch `feat/issue23-skill-publish-ui` (worktree `.claude/worktrees/feat-issue23-skill-publish-ui`), based on `upstream/main` @ `9be7801` (contains merged #22).
- `make test` = `go vet` + unit tests, **excludes** `test/e2e` (needs a live kind cluster). Run it after each backend task.
- `make web` (`cd web && npm run build` = `tsc -b` then `vite build`). Run it after each frontend task.
- Commits: `git commit -s` (signoff) with an `Assisted-by: Claude Code` trailer; English message.
- The **only** new dependency is `fflate` (frontend). No new Go deps.
- Do not reimplement #22: `skill.ResolveVersion`, `skill.PathRepository.WriteBytes`, CRD upsert, `patchSkillPhase`, `publishSpec` are consumed as-is.
- `/internal/skills/{name}/tar` (supervisor pull) is **unchanged**.

---

### Task 1: `ValidateSkillTar` — user-facing publish validation

Add `ValidateSkillTar` (gzip tar **+ root `SKILL.md`**) alongside the existing `ValidateTar`. `ValidateTar` is kept until Task 2 removes its last caller.

**Files:**
- Modify: `internal/skill/repository.go` (insert after `ValidateTar`, ~line 194)
- Test: `internal/skill/repository_test.go` (insert after `TestValidateTar`, ~line 124)

**Interfaces:**
- Produces: `func ValidateSkillTar(data []byte) error` — consumed by the /api publish handler (Task 2). Error text is surfaced to the Portal toast.

- [ ] **Step 1: Write the failing test**

Append to `internal/skill/repository_test.go`:

```go
// TestValidateSkillTar verifies the user-facing publish validation: a gzip tar
// with SKILL.md at its root passes; a tar without one (or nested only) and a
// non-gzip payload are rejected.
func TestValidateSkillTar(t *testing.T) {
	if err := ValidateSkillTar(mustPack(t, fstest.MapFS{"SKILL.md": {Data: []byte("x")}})); err != nil {
		t.Fatalf("valid skill tar rejected: %v", err)
	}
	if err := ValidateSkillTar(mustPack(t, fstest.MapFS{"scripts/x.sh": {Data: []byte("x")}})); err == nil {
		t.Fatal("tar without root SKILL.md should be rejected")
	}
	if err := ValidateSkillTar(mustPack(t, fstest.MapFS{"sub/SKILL.md": {Data: []byte("x")}})); err == nil {
		t.Fatal("nested SKILL.md should be rejected")
	}
	if err := ValidateSkillTar([]byte("not a tar")); err == nil {
		t.Fatal("non-gzip payload should be rejected")
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/skill/ -run TestValidateSkillTar -v`
Expected: FAIL — `undefined: ValidateSkillTar`.

- [ ] **Step 3: Implement `ValidateSkillTar`**

Insert after `ValidateTar` in `internal/skill/repository.go`:

```go
// ValidateSkillTar reports whether data is a publishable skill archive: a
// readable gzip tar whose root contains SKILL.md. The user-facing publish
// endpoint uses this to reject a wrong-folder upload before anything is
// persisted (the internal seed path already guarantees SKILL.md).
func ValidateSkillTar(data []byte) error {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return errors.New("skill archive has no SKILL.md")
		}
		if err != nil {
			return fmt.Errorf("tar: %w", err)
		}
		if filepath.Clean(filepath.FromSlash(hdr.Name)) == "SKILL.md" {
			return nil
		}
	}
}
```

(`errors`, `io`, `filepath` are already imported in this file.)

- [ ] **Step 4: Run it to verify it passes**

Run: `go test ./internal/skill/`
Expected: PASS (all tests in the package, including the new one).

- [ ] **Step 5: Commit**

```bash
git add internal/skill/repository.go internal/skill/repository_test.go
git commit -s -m "feat(skill): validate publishable tars require root SKILL.md (#23)" -m "Assisted-by: Claude Code"
```

---

### Task 2: Publish via `/api` with publisher provenance

Move the publish face to `POST /api/skills/{name}/publish`, drop the `builtin`/`visibility` params (user uploads are never builtin; visibility is forced `Platform`), record the publisher annotation, and thread `publisher` through `publishSkill`.

**Files:**
- Modify: `internal/server/server.go` (route line 67)
- Modify: `internal/server/handlers_platform.go` (`handlePublishSkill` ~423-474, `publishSkill` ~476-527, new `publishOptions` + `addPublisherAnnotation`)
- Modify: `internal/server/skill_seed.go` (line 41)
- Modify: `internal/server/internal_api_test.go` (`TestInternalPublishSkill` ~359-434 → `TestPublishSkill`; `doRawPost` ~445 → `doRawPostAs`; new `mustPackBytesNoSkill`; publisher assertion in `TestSeedBuiltinSkills` loop ~471-484)

**Interfaces:**
- Consumes: `skill.ValidateSkillTar` (Task 1).
- Produces: `POST /api/skills/{name}/publish` (body = gzip tar, query = `displayName` + `description`) → 200 with the Skill CR; `publishSkill(ctx, name, displayName, description, visibility, tar, publishOptions{Builtin, Publisher})`.
- Consumed by: `web/src/api/index.ts publishSkill` (Task 3).

- [ ] **Step 1: Write the failing test**

In `internal/server/internal_api_test.go`:

1. Replace `doRawPost` (line 445) with a user-header variant:

```go
func doRawPostAs(t *testing.T, h http.Handler, path string, data []byte, user string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/gzip")
	if user != "" {
		req.Header.Set("X-CubePilot-User", user)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
```

2. Replace `TestInternalPublishSkill` (lines 359-434) with `TestPublishSkill` hitting `/api/skills/harbor/publish`:

```go
// TestPublishSkill verifies the user-facing publish endpoint: it stores the
// tar atomically (versioned), upserts the Skill CRD (source.path + sha256 +
// publisher annotation), marks phase Available, and is idempotent per content.
func TestPublishSkill(t *testing.T) {
	skillsDir := t.TempDir()
	s := platformTestServerSkillsDir(t, skillsDir)

	// The identity header is recorded as the publisher on the Skill CR.
	tar1 := mustPackBytes(t, "# Harbor v1\n")
	rec := doRawPostAs(t, s.Handler(), "/api/skills/harbor/publish?displayName=Harbor", tar1, "li.ming")
	if rec.Code != http.StatusOK {
		t.Fatalf("publish #1: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var published v1alpha1.Skill
	if err := json.Unmarshal(rec.Body.Bytes(), &published); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if published.Spec.Source.Path != "skills/harbor/v1.tar.gz" {
		t.Errorf("source.path = %q, want skills/harbor/v1.tar.gz", published.Spec.Source.Path)
	}
	if published.Spec.Source.Sha256 == "" {
		t.Error("sha256 not backfilled")
	}
	if published.Spec.Visibility != v1alpha1.SkillVisibilityPlatform {
		t.Errorf("visibility = %q, want Platform", published.Spec.Visibility)
	}
	if published.Annotations["cubepilot/publisher"] != "li.ming" {
		t.Errorf("publisher = %q, want li.ming", published.Annotations["cubepilot/publisher"])
	}
	if published.Status.Phase != v1alpha1.SkillPhaseAvailable {
		t.Errorf("phase = %q, want Available", published.Status.Phase)
	}
	if _, err := os.Stat(filepath.Join(skillsDir, "skills", "harbor", "v1.tar.gz")); err != nil {
		t.Fatalf("tar not stored: %v", err)
	}

	// Same content -> same version, no rewrite.
	rec = doRawPostAs(t, s.Handler(), "/api/skills/harbor/publish?displayName=Harbor", tar1, "li.ming")
	if rec.Code != http.StatusOK {
		t.Fatalf("publish #2: status = %d", rec.Code)
	}
	var again v1alpha1.Skill
	_ = json.Unmarshal(rec.Body.Bytes(), &again)
	if again.Spec.Source.Path != "skills/harbor/v1.tar.gz" {
		t.Errorf("re-publish changed version: %q", again.Spec.Source.Path)
	}

	// New content -> v2.
	tar2 := mustPackBytes(t, "# Harbor v2\n")
	rec = doRawPostAs(t, s.Handler(), "/api/skills/harbor/publish?displayName=Harbor", tar2, "li.ming")
	if rec.Code != http.StatusOK {
		t.Fatalf("publish v2: status = %d", rec.Code)
	}
	var v2 v1alpha1.Skill
	_ = json.Unmarshal(rec.Body.Bytes(), &v2)
	if v2.Spec.Source.Path != "skills/harbor/v2.tar.gz" {
		t.Errorf("new content version = %q, want v2", v2.Spec.Source.Path)
	}

	// Missing displayName -> 400.
	rec = doRawPostAs(t, s.Handler(), "/api/skills/harbor/publish", tar1, "li.ming")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing displayName status = %d, want 400", rec.Code)
	}

	// A tar without root SKILL.md -> 400, nothing persisted.
	rec = doRawPostAs(t, s.Handler(), "/api/skills/harbor/publish?displayName=Harbor", mustPackBytesNoSkill(t, "# scripts only\n"), "li.ming")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no-SKILL.md status = %d, want 400", rec.Code)
	}

	// Malformed / non-gzip body -> 400, nothing persisted.
	rec = doRawPostAs(t, s.Handler(), "/api/skills/harbor/publish?displayName=Harbor", []byte("not a tar"), "li.ming")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid tar status = %d, want 400", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(skillsDir, "skills", "harbor", "v3.tar.gz")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("invalid tar should not be persisted: %v", err)
	}

	// Oversized body -> 413.
	rec = doRawPostAs(t, s.Handler(), "/api/skills/harbor/publish?displayName=Harbor", make([]byte, maxSkillTarSize+1), "li.ming")
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized status = %d, want 413", rec.Code)
	}

	// Non-Platform visibility -> 400 (phase 1).
	rec = doRawPostAs(t, s.Handler(), "/api/skills/harbor/publish?displayName=Harbor&visibility=User", tar1, "li.ming")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("visibility=User status = %d, want 400", rec.Code)
	}
}
```

3. Add `mustPackBytesNoSkill` next to `mustPackBytes` (line 436):

```go
func mustPackBytesNoSkill(t *testing.T, body string) []byte {
	t.Helper()
	data, err := skill.Pack(fstest.MapFS{"scripts/x.sh": {Data: []byte(body)}})
	if err != nil {
		t.Fatal(err)
	}
	return data
}
```

4. In `TestSeedBuiltinSkills`'s `for _, sk := range list.Items` loop (after the `cubepilot/builtin` label check, line 480), add:

```go
		if sk.Annotations["cubepilot/publisher"] != "system" {
			t.Errorf("skill %s: publisher = %q, want system", sk.Name, sk.Annotations["cubepilot/publisher"])
		}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/server/ -run 'TestPublishSkill|TestSeedBuiltinSkills' -v`
Expected: FAIL — `TestPublishSkill` gets 404 (the `/api/skills/{name}/publish` route does not exist yet) and `TestSeedBuiltinSkills` reports `publisher = "", want system`.

- [ ] **Step 3: Implement the endpoint + publisher**

1. `internal/server/server.go` line 67 — move the route to the user-facing tree:

```go
	mux.HandleFunc("/api/skills/{name}/publish", s.handlePublishSkill)
```

2. `internal/server/handlers_platform.go` — replace the `handlePublishSkill` function body (lines ~430-474) with:

```go
// handlePublishSkill is the user-facing skill publish endpoint: POST
// /api/skills/{name}/publish. The body is a gzip tar of the skill directory
// (packed client-side by the Portal); metadata comes via query params
// (displayName, description). Phase 1 forces visibility=Platform and records
// the publisher identity (X-CubePilot-User) on the Skill CR. The API owns the
// repository: atomic write, versioning and sha256 are the #22 publishSkill
// primitive, shared with the builtin seed.
func (s *Server) handlePublishSkill(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST required"})
		return
	}
	name := r.PathValue("name")
	q := r.URL.Query()
	displayName := q.Get("displayName")
	if name == "" || displayName == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "name and displayName are required"})
		return
	}
	if v := q.Get("visibility"); v != "" && v != string(v1alpha1.SkillVisibilityPlatform) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "phase 1 supports only visibility=Platform"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxSkillTarSize+1))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if len(body) > maxSkillTarSize {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "skill tar exceeds the size limit"})
		return
	}
	if len(body) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "empty skill tar"})
		return
	}
	// Reject malformed / wrong-folder archives (no root SKILL.md) before
	// anything is persisted.
	if err := skill.ValidateSkillTar(body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid skill tar: " + err.Error()})
		return
	}
	skillCR, err := s.publishSkill(r.Context(), name, displayName, q.Get("description"),
		v1alpha1.SkillVisibilityPlatform, body, publishOptions{Publisher: s.userOf(r)})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, skillCR)
}
```

3. Replace `publishSkill` (lines ~476-527) with the publisher-aware version, and add `publishOptions` + `addPublisherAnnotation`:

```go
// publishOptions carries the non-body dimensions of a publish: Builtin (seed
// only — user uploads are never builtin) and Publisher (the identity header
// for Portal uploads, "system" for the builtin seed), recorded on the CR.
type publishOptions struct {
	Builtin   bool
	Publisher string
}

// publishSkill is the shared publish primitive used by the HTTP endpoint and
// the startup seed: it writes the tar atomically (versioned, sha256) into the
// repository and upserts the Skill CRD (status.phase=Available).
func (s *Server) publishSkill(ctx context.Context, name, displayName, description string,
	visibility v1alpha1.SkillVisibility, tar []byte, opts publishOptions) (*v1alpha1.Skill, error) {
	if s.cr == nil {
		return nil, fmt.Errorf("k8s client unavailable")
	}
	repo := &skill.PathRepository{Root: s.cfg.SkillsDir}
	sha := sha256Hex(tar)
	ver, stored, err := skill.ResolveVersion(ctx, repo, name, sha)
	if err != nil {
		return nil, err
	}
	if !stored {
		if _, err := repo.WriteBytes(ctx, fmt.Sprintf("skills/%s/%s.tar.gz", name, ver), tar); err != nil {
			return nil, err
		}
	}

	// Upsert the Skill CRD.
	key := client.ObjectKey{Name: name}
	var skillCR v1alpha1.Skill
	err = s.cr.Get(ctx, key, &skillCR)
	switch {
	case err == nil:
		skillCR.Spec = publishSpec(displayName, description, visibility, name, ver, sha)
		if opts.Builtin {
			addBuiltinLabels(&skillCR)
		}
		addPublisherAnnotation(&skillCR, opts.Publisher)
		if err := s.cr.Update(ctx, &skillCR); err != nil {
			return nil, err
		}
	case apierrors.IsNotFound(err):
		skillCR = v1alpha1.Skill{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if opts.Builtin {
			addBuiltinLabels(&skillCR)
		}
		addPublisherAnnotation(&skillCR, opts.Publisher)
		skillCR.Spec = publishSpec(displayName, description, visibility, name, ver, sha)
		if err := s.cr.Create(ctx, &skillCR); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}
	// status subresource: mark Available.
	if err := s.patchSkillPhase(ctx, name, v1alpha1.SkillPhaseAvailable); err != nil {
		return nil, err
	}
	skillCR.Status.Phase = v1alpha1.SkillPhaseAvailable
	return &skillCR, nil
}

func addPublisherAnnotation(skillCR *v1alpha1.Skill, publisher string) {
	if skillCR.Annotations == nil {
		skillCR.Annotations = map[string]string{}
	}
	skillCR.Annotations["cubepilot/publisher"] = publisher
}
```

4. `internal/server/skill_seed.go` line 41 — pass the options struct:

```go
		if _, err := s.publishSkill(ctx, name, displayName, description, v1alpha1.SkillVisibilityPlatform, tar, publishOptions{Builtin: true, Publisher: "system"}); err != nil {
```

- [ ] **Step 4: Run it to verify it passes**

Run: `go test ./internal/server/`
Expected: PASS (TestPublishSkill + TestSeedBuiltinSkills + all other server tests).

- [ ] **Step 5: Commit**

```bash
git add internal/server/server.go internal/server/handlers_platform.go internal/server/skill_seed.go internal/server/internal_api_test.go
git commit -s -m "feat(server): publish skills via /api with publisher provenance (#23)" -m "Assisted-by: Claude Code"
```

- [ ] **Step 6: Remove the now-dead `ValidateTar`**

`ValidateTar` (and its test) have no callers left — the /api handler uses `ValidateSkillTar`.

1. `internal/skill/repository.go` — delete `ValidateTar` (lines ~180-194).
2. `internal/skill/repository_test.go` — delete `TestValidateTar` (lines ~117-124).
3. Verify: `go test ./internal/skill/ ./internal/server/` → PASS; `grep -rn "ValidateTar" --include="*.go" .` → no matches.

- [ ] **Step 7: Commit**

```bash
git add internal/skill/repository.go internal/skill/repository_test.go
git commit -s -m "refactor(skill): drop dead ValidateTar after /api publish switch (#23)" -m "Assisted-by: Claude Code"
```

---

### Task 3: Frontend foundation — fflate, `pack.ts`, `api.publishSkill`

**Files:**
- Modify: `web/package.json` + `web/package-lock.json` (add `fflate`)
- Create: `web/src/utils/pack.ts`
- Modify: `web/src/api/index.ts` (add `publishSkill` after `listSkills`)

**Interfaces:**
- Produces: `packSkillDir(files: File[]): Promise<Uint8Array>` (throws `PackError` when the root has no `SKILL.md`); `api.publishSkill(name, {displayName, description?}, tar)`.
- Consumed by: `SkillsView` (Task 4).

- [ ] **Step 1: Install `fflate`**

Run: `npm --prefix web install fflate`
Expected: `added 1 package` (and the project's node_modules is now populated).

- [ ] **Step 2: Write `web/src/utils/pack.ts`**

```ts
// Pack a picked skill directory into the gzip tar bytes the publish API
// accepts (SKILL.md at the archive root; scripts/ and references/ preserved).
// Browsers cannot produce a tar natively, so the ~7 KB fflate library supplies
// tarSync + gzipSync (no transitive dependencies).
import { gzipSync, tarSync } from 'fflate'

// PackError is thrown by packSkillDir for selections that are not a valid
// skill directory (no SKILL.md at the root).
export class PackError extends Error {}

// packSkillDir packs the File[] from an <input type="file" webkitdirectory>
// selection into gzip tar bytes, stripping the leading folder segment so the
// archive root holds SKILL.md / scripts / references directly.
export async function packSkillDir(files: File[]): Promise<Uint8Array> {
  const entries: { path: string; data: Uint8Array }[] = []
  let hasSkillMd = false
  for (const f of files) {
    const slash = f.webkitRelativePath.indexOf('/')
    if (slash === -1) continue // ignore files with no directory info
    const path = f.webkitRelativePath.slice(slash + 1)
    if (path === 'SKILL.md') hasSkillMd = true
    entries.push({ path, data: new Uint8Array(await f.arrayBuffer()) })
  }
  if (!hasSkillMd) {
    throw new PackError('The selected folder has no SKILL.md at its root')
  }
  return gzipSync(tarSync(entries))
}
```

- [ ] **Step 3: Add `publishSkill` to `web/src/api/index.ts`**

After the `listSkills:` entry in the `api` object:

```ts
  publishSkill: (name: string, opts: { displayName: string; description?: string }, tar: Uint8Array) =>
    apiFetch<PlatformObject>(
      `/api/skills/${encodeURIComponent(name)}/publish?displayName=${encodeURIComponent(opts.displayName)}${opts.description ? `&description=${encodeURIComponent(opts.description)}` : ''}`,
      { method: 'POST', headers: { 'Content-Type': 'application/gzip' }, body: tar },
    ),
```

(`PlatformObject` is already imported in this file; `apiFetch` spreads the headers and accepts a `Uint8Array` body.)

- [ ] **Step 4: Type-check + build**

Run: `npm --prefix web run build`
Expected: PASS — `tsc -b` reports no errors and Vite emits `web/dist`.

- [ ] **Step 5: Commit**

```bash
git add web/package.json web/package-lock.json web/src/utils/pack.ts web/src/api/index.ts
git commit -s -m "feat(web): pack skill dir client-side and publish via /api (#23)" -m "Assisted-by: Claude Code"
```

---

### Task 4: Skills page + navigation

**Files:**
- Create: `web/src/views/SkillsView.tsx`
- Modify: `web/src/App.tsx` (import, `VIEW_TITLES`, nav item, route)

**Interfaces:**
- Consumes: `packSkillDir` (Task 3), `api.publishSkill` + `api.listSkills` (Task 3 / existing).
- Produces: `/skills` route rendering the publish form + published list.

- [ ] **Step 1: Write `web/src/views/SkillsView.tsx`**

```tsx
// Skills view -- publish a skill directory to the market and list what is
// published (issue #23). The directory is packed to a gzip tar client-side
// (web/src/utils/pack.ts) and uploaded via the user-facing publish endpoint.
import { useEffect, useRef, useState } from 'react'
import type { ChangeEvent } from 'react'
import { api } from '@/api'
import type { PlatformObject } from '@/api/types'
import { packSkillDir } from '@/utils/pack'
import { showToast } from '@/stores/toast'

// specStr reads a string field from the CR spec (Record<string, unknown>).
function specStr(sk: PlatformObject, key: string): string {
  const v = sk.spec?.[key]
  return typeof v === 'string' ? v : ''
}

export default function SkillsView() {
  const [skills, setSkills] = useState<PlatformObject[]>([])
  const [name, setName] = useState('')
  const [displayName, setDisplayName] = useState('')
  const [description, setDescription] = useState('')
  const [dirFiles, setDirFiles] = useState<File[]>([])
  const [dirName, setDirName] = useState('')
  const [publishing, setPublishing] = useState(false)
  const dirInput = useRef<HTMLInputElement>(null)

  async function loadSkills() {
    try {
      setSkills(await api.listSkills())
    } catch (e) {
      console.error('loadSkills', e)
    }
  }

  useEffect(() => {
    loadSkills()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // A webkitdirectory picker reports every file with a folder-relative path;
  // the leading segment is the chosen directory's name (used as the slug).
  function onPickDir(e: ChangeEvent<HTMLInputElement>) {
    const files = Array.from(e.target.files ?? [])
    setDirFiles(files)
    const first = files.find((f) => f.webkitRelativePath)
    const folder = first ? first.webkitRelativePath.split('/')[0] : ''
    setDirName(folder)
    if (folder) setName(folder)
  }

  async function publish() {
    if (publishing) return
    const slug = name.trim()
    const title = displayName.trim()
    if (!slug || !title) {
      showToast('Name and display name are required')
      return
    }
    if (dirFiles.length === 0) {
      showToast('Pick a skill directory first')
      return
    }
    setPublishing(true)
    try {
      const tar = await packSkillDir(dirFiles)
      const sk = await api.publishSkill(slug, { displayName: title, description: description.trim() || undefined }, tar)
      showToast(`Skill '${sk.metadata.name}' published`)
      setName('')
      setDisplayName('')
      setDescription('')
      setDirFiles([])
      setDirName('')
      if (dirInput.current) dirInput.current.value = ''
      await loadSkills()
    } catch (e) {
      showToast('Publish failed: ' + (e instanceof Error ? e.message : String(e)))
    } finally {
      setPublishing(false)
    }
  }

  return (
    <div className="view active">
      <div className="view-head">
        <div>
          <div className="view-title">Skills</div>
          <div className="view-desc">Publish a skill directory to the market - installed into instances by the supervisor (issue #23)</div>
        </div>
      </div>

      <div className="config-grid">
        <div style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
          <div className="card">
            <div className="card-head">
              <span className="card-title">Publish a Skill</span>
              <span className="card-hint">Pick the skill directory - packed to a gzip tar in the browser</span>
            </div>
            <div className="card-pad">
              <div className="field">
                <label className="label">Skill name (slug)</label>
                <input
                  className="input"
                  placeholder="e.g. harbor-scan (prefilled from the folder name)"
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                />
              </div>
              <div className="field">
                <label className="label">Display name</label>
                <input
                  className="input"
                  placeholder="e.g. Harbor Image Scan"
                  value={displayName}
                  onChange={(e) => setDisplayName(e.target.value)}
                />
              </div>
              <div className="field">
                <label className="label">Description</label>
                <textarea
                  className="input"
                  rows={3}
                  placeholder="One line about what this skill does"
                  value={description}
                  onChange={(e) => setDescription(e.target.value)}
                />
              </div>
              <div className="field" style={{ marginBottom: 0 }}>
                <label className="label">Skill directory</label>
                <input
                  ref={dirInput}
                  type="file"
                  className="input"
                  {...({ webkitdirectory: '' } as Record<string, string>)}
                  onChange={onPickDir}
                />
                {dirName ? (
                  <div style={{ marginTop: 4, fontSize: 12, color: 'var(--muted)' }}>
                    {dirName}/ - {dirFiles.length} file{dirFiles.length === 1 ? '' : 's'}
                  </div>
                ) : null}
              </div>
              <button className="btn primary" style={{ width: '100%', marginTop: 12 }} disabled={publishing} onClick={publish}>
                {publishing ? 'Publishing...' : 'Publish to Market'}
              </button>
            </div>
          </div>
        </div>

        <div style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
          <div className="card">
            <div className="card-head">
              <span className="card-title">Published Skills</span>
              <span className="card-hint">Platform-visible skills in the market - installed per instance (issue #24)</span>
            </div>
            <div className="card-pad" style={{ paddingTop: 4, paddingBottom: 10 }}>
              {skills.length ? (
                skills.map((sk) => {
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
                      <span className="pill neutral">{specStr(sk, 'visibility') || 'Platform'}</span>
                      <span className={`pill ${phase === 'Available' ? 'success' : 'neutral'}`}>{phase || 'unknown'}</span>
                    </div>
                  )
                })
              ) : (
                <div style={{ color: 'var(--muted)', padding: '8px 0' }}>No published skills</div>
              )}
            </div>
          </div>
        </div>
      </div>
    </div>
  )
}
```

- [ ] **Step 2: Wire `web/src/App.tsx`**

1. Add `import SkillsView from '@/views/SkillsView'` to the view imports.
2. Add `skills: 'Skills',` to `VIEW_TITLES`.
3. Add a `SkillIcon` component (next to the other icons):

```tsx
function SkillIcon() {
  return (
    <svg className="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round">
      <path d="M12 2 3 7l9 5 9-5-9-5zM3 12l9 5 9-5M3 17l9 5 9-5" />
    </svg>
  )
}
```

4. Add a nav item after the "Agent Config" `NavLink`:

```tsx
          <NavLink to="/skills" className={navCls}>
            <SkillIcon />
            <span>Skills</span>
          </NavLink>
```

5. Add the route next to the other `<Route>` entries:

```tsx
            <Route path="/skills" element={<SkillsView />} />
```

- [ ] **Step 3: Type-check + build**

Run: `npm --prefix web run build`
Expected: PASS — `tsc -b` and Vite emit `web/dist` with no errors.

- [ ] **Step 4: Commit**

```bash
git add web/src/views/SkillsView.tsx web/src/App.tsx
git commit -s -m "feat(web): add Skills publish page (#23)" -m "Assisted-by: Claude Code"
```

---

### Task 5: Docs update

**Files:**
- Modify: `docs/superpowers/specs/2026-09-01-skill-crd-and-repository-design.md` (§5 publish-endpoint note)
- Modify: `docs/cubepilot/implementation-status.md` (skill-market status line 31)

- [ ] **Step 1: Point the #22 design doc's publish note at the user-facing endpoint**

In `docs/superpowers/specs/2026-09-01-skill-crd-and-repository-design.md`, find the §5 sentence that mentions the publish endpoint (contains `POST /internal/skills/{name}/publish`) and update it so the Portal upload path reads `POST /api/skills/{name}/publish` (user-facing, records `cubepilot/publisher`; the internal `publishSkill` primitive is shared by the seed). If the wording already says "#23's Portal upload" with the old path, change just the path + add the publisher note.

- [ ] **Step 2: Update the implementation-status snapshot**

In `docs/cubepilot/implementation-status.md`, the skill-market status line (~line 31) says the publish endpoint is `POST /internal/skills/{name}/publish` and that "内置预设由 operator 经发布接口 seed". Update it to:

- publish face is `POST /api/skills/{name}/publish` (Portal upload, records publisher, forces `visibility=Platform`);
- the builtin presets are API-seeded at startup via the shared `publishSkill` primitive;
- remaining for the epic: **Portal 发布/安装 UI（#23/#24）** — mark the publish UI as delivered by #23, install UI still #24; S3 source + user-private skills stay phase 2.

- [ ] **Step 3: Commit**

```bash
git add docs/superpowers/specs/2026-09-01-skill-crd-and-repository-design.md docs/cubepilot/implementation-status.md
git commit -s -m "docs(skill): mark publish flow implemented (#23)" -m "Assisted-by: Claude Code"
```

---

### Final verification

- [ ] **Step 1: Full Go suite**

Run: `make test`
Expected: `go vet ./...` and all unit tests (excluding `test/e2e`) pass.

- [ ] **Step 2: Frontend build**

Run: `make web`
Expected: `tsc -b` + `vite build` pass.

- [ ] **Step 3: Lint (optional but repo-standard)**

Run: `golangci-lint run ./...`
Expected: 0 issues. If the tool is not installed, report `make lint` (helm) instead and note the skip.

- [ ] **Step 4: Route sanity**

Run: `grep -n "skills" internal/server/server.go`
Expected: `GET /api/skills`, `POST /api/skills/{name}/publish`, and `GET /internal/skills/{name}/tar` — the old `/internal/skills/{name}/publish` line is gone.

- [ ] **Step 5: Manual smoke (Portal)**

`make deploy` + `make web`, then on the Portal: open **Skills** → pick a skill directory (e.g. `internal/skill/skills/harbor-scan`… any folder with `SKILL.md`) → Publish → toast success and the row appears under Published Skills; publish the same folder again → success (idempotent); pick a folder without `SKILL.md` → blocked client-side with the `PackError` toast.
