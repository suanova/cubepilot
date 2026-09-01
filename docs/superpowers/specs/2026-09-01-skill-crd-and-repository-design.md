# Design: Skill CRD + skill repository (issue #22)

Date: 2026-09-01
Status: draft (pending review)
Goal owner: zhujian

## 1. Context & goal

Issue #22 ("Skill CRD + skill repository (shared file volume, atomic write, CEL validation)") is the first story of the Skill-market epic (#21). Phase 1 ships the skill market: the `Skill` CRD registers "what skill exists, where, which version, who can see it" while skill content (a multi-file directory: `SKILL.md` + optional `scripts/` / `references/`) lives in a repository as versioned, immutable tars. Publish / install flows are separate stories (#23 / #24).

This story delivers:

- The marketplace `Skill` CRD schema — replacing the phase-1 catalog schema. The first version is not released, so no compatibility layer is kept.
- CEL validation of the `source` discriminant (invalid combos rejected by the API server).
- The repository backend: atomic tar writes (temp file + rename), read-back, and extract.
- Preset skills converted to repo seed tars, so a built-in skill is verifiably usable from a workspace (publishable = usable).
- The loading end wired into the supervisor: it pulls skill tars from the internal API and extracts them into the instance PVC (`workspace/skills/<name>/`), which OpenClaw discovers + hot-reloads.

### Decisions (brainstorming, 2026-09-01)

| Decision | Choice | Why |
|---|---|---|
| Schema | **Replace** catalog schema with marketplace; no compat | No released v1; design §3.4 is authoritative; end state is replacement |
| Presets | Convert to **repo seed tars** (written by the operator at bootstrap) | Exercises the backend with real content; content source becomes the repo |
| Loading end | Wired into the **supervisor in this story** | Phase-1 "skills in the PVC and usable" is a hard done-condition |
| Tar transport | **HTTP pull from the internal API** (design §3.4 "network pull" branch) | Agent Pods don't mount the shared volume; kind e2e needs no RWX volume; pre-implements the phase-2 S3 fetch path |
| Repository location | Control plane only (operator RW + API RW) | Consistent with the network-pull choice |
| Pull cadence | Tar pulled **only when a skill's content hash changes** (per-skill `.sha256` marker on the PVC) | 10 s poll is change detection only; unchanged tars are never re-pulled |
| S3 | Type + CEL defined now; **backend not implemented** | Phase 2 |
| Failure semantics | Any skill fetch/extract failure **fails the apply**; revision not advanced; next poll retries | Same semantics as today's render failure; self-healing |

## 2. Skill CRD (marketplace schema)

`internal/api/v1alpha1/skill_types.go` is rewritten. The catalog types (`SkillType` Atomic/Domain, `SkillTarget`, `SkillSemantics`, `SkillFile`) are removed; the catalog fields (override/target/semantics/uses/instructions/files/contentRef/agents/ownerModule) are removed.

```go
type SkillSourceType string // enum: Path | S3
type SkillVisibility string // enum: Platform | Tenant | User
type SkillPhase string      // enum: Available | Unreachable

type SkillS3Source struct {
    Bucket string `json:"bucket"`
    Key    string `json:"key"`
}

type SkillSource struct {
    // Type is the discriminant (design §3.4). Phase 1 only Path.
    Type SkillSourceType `json:"type"`
    // Path is the repo-relative tar path (e.g. skills/harbor/v1.tar.gz),
    // versioned and immutable. Required when type=Path.
    Path string `json:"path,omitempty"`
    // S3 is the object-store addressing. Phase 2; forbidden when type=Path.
    S3 *SkillS3Source `json:"s3,omitempty"`
    // Sha256 is the content fingerprint, backfilled by publish/seed. Optional:
    // manual kubectl apply may leave it empty (audit via the versioned path).
    Sha256 string `json:"sha256,omitempty"`

    // +kubebuilder:validation:XValidation:rule="self.type=='Path' ? has(self.path) && !has(self.s3) : true",message="source.type=Path requires source.path and forbids source.s3"
    // +kubebuilder:validation:XValidation:rule="self.type=='S3' ? has(self.s3) && !has(self.path) : true",message="source.type=S3 requires source.s3 and forbids source.path"
}

type SkillSpec struct {
    // DisplayName is the market-facing title.
    DisplayName string `json:"displayName"`
    Description string `json:"description,omitempty"`
    // Visibility is Platform | Tenant | User; phase 1 only Platform.
    Visibility SkillVisibility `json:"visibility"`
    Source     SkillSource     `json:"source"`
}

type SkillStatus struct {
    // Phase is Available | Unreachable. Set by the seed/publish flow
    // (Available after the tar is written to the repository).
    Phase               SkillPhase         `json:"phase,omitempty"`
    Conditions          []metav1.Condition `json:"conditions,omitempty"`
    ObservedGeneration  int64              `json:"observedGeneration,omitempty"`
}
```

- `+kubebuilder:subresource:status` and the `+kubebuilder:resource:scope=Cluster` markers stay; print columns change to `DisplayName` / `Visibility` / `Phase` / `Age`.
- `Skill.Revision()` (content hash of the spec) stays — `source.path` + `source.sha256` feed it, so TaskRun `skillRevision` audit keeps working and a content change produces a new revision.
- `zz_generated.deepcopy.go` is regenerated with `controller-gen` (`controller-gen object`), and the CRD yaml with `controller-gen crd` (`config/crd/bases/ai.cubestack.io_skills.yaml` + the chart copy in `deploy/charts/cubepilot/crds/`).

CEL validation: the two `XValidation` rules on `SkillSource` make `type=Path` require `path` and forbid `s3` (and the converse). Invalid combos are rejected by the API server. The `type` / `visibility` / `phase` enums are `kubebuilder:validation:Enum`.

## 3. Repository backend

New package `internal/skill/repository.go`:

```go
// Repository is the skill content store (design §3.4: shared file volume in
// phase 1; S3 in phase 2). The backend is transparent to the CRD and the
// loading flow — differences are only in addressing + how the injector gets
// the tar (mount read vs network pull). Tars are gzip-compressed (.tar.gz).
type Repository interface {
    // Write packs src (a skill directory: SKILL.md + optional scripts/,
    // references/) into a gzip tar at relPath, atomically (temp file +
    // rename), and returns the tar's sha256. On any error the temp file is
    // removed.
    Write(ctx context.Context, relPath string, src fs.FS) (string, error)
    // Open returns a reader for the tar at relPath (for read-back / serving).
    Open(ctx context.Context, relPath string) (io.ReadCloser, error)
    // Extract unpacks the tar at relPath into destDir, preserving the
    // directory structure and rejecting path traversal (".." escapes).
    // Implemented as Open + ExtractTar.
    Extract(ctx context.Context, relPath, destDir string) error
}

// ExtractTar unpacks an arbitrary gzip tar stream (from a repo read, an HTTP
// fetch, or a temp file) into destDir with the same traversal protection.
// Used by PathRepository.Extract and by the supervisor's HTTP-pull path.
func ExtractTar(r io.Reader, destDir string) error

// Pack builds the gzip tar bytes for src (a skill directory), for callers
// that publish the content over the wire (operator -> API publish endpoint).
func Pack(src fs.FS) ([]byte, error)

// ResolveVersion returns the version label for publishing name with content
// hash sha: the existing vN whose stored tar content matches sha (stored=true,
// no write needed), or the next unused vN (stored=false, caller writes it).
func ResolveVersion(ctx context.Context, repo Repository, name, sha string) (string, bool, error)

// PathRepository is the shared-file-volume implementation: relPath is relative
// to root (the volume mount point).
type PathRepository struct{ Root string }
```

- **Write**: create the tar from `src` (walk `SKILL.md` + optional `scripts/`, `references/`), write to a temp file in the *same directory* (`.<name>.tmp-*`), `fsync`, compute sha256, then `rename` onto `relPath`. On any error, remove the temp file. Missing parent dirs are created.
- **Extract / ExtractTar**: `tar.Reader` over the stream; for each header, `filepath.Clean(dest)` must stay under `destDir` (reject `..`); write files, create subdirs, skip symlinks/hardlinks. A corrupt tar (bad header / truncated) returns an error and leaves a partial dest (caller's workspace sync clears on the next successful sync).

### Repository tests (`internal/skill/repository_test.go`)

- Write → tar exists at `relPath`, sha256 returned; tar reads back with `SKILL.md` + `scripts/` + `references/` entries.
- No temp files left behind after success or failure (failed rename: target directory removed after the temp was created).
- Extract → correct structure in `destDir`; corrupt tar (truncated) → error; traversal entry (`../evil`) → error, nothing written outside `destDir`.

## 4. Preset publishing

The four embedded presets (`internal/controller/skills/{cluster-inspection,dev-environment,inference-service,kubectl-platform}/SKILL.md`) become repo tars. The **API owns the repository**; the operator only supplies the content by calling the publish endpoint (which is also the #23 Portal publish primitive).

`internal/controller/skill_source.go` is rewritten:

- `skillsFS` (go:embed) stays; `parseSKILL` / `skillTitle` / `presetSkillNames` stay (the operator decides what is builtin + parses the metadata).
- `BuiltinSkillDefinitions()` is replaced by `PublishBuiltinSkills(ctx, apiURL, httpClient)`:

```go
// PublishBuiltinSkills publishes the embedded preset skills to the skill API
// (POST /internal/skills/{name}/publish, gzip tar body + metadata) and
// returns the resulting Skill CRs. The API owns the repository and the Skill
// CRD registration.
func PublishBuiltinSkills(ctx context.Context, apiURL string, httpClient *http.Client) ([]*v1alpha1.Skill, error)
```

- The `BuiltinBootstrapReconciler` (`internal/controller/builtin.go`) gains an `APIURL string` field (from `config.APIURL`). `ensureBuiltin` publishes each preset (pack to a gzip tar via `skill.Pack`, POST to the API with `displayName` / `description` / `builtin=true`). If the API is not up yet, the periodic reconcile retries (self-healing).
- The operator does **not** mount the shared volume and does **not** register Skill CRDs — the API does both.

## 5. Internal API: skill endpoints

The API server owns the repository (write + serve). Internal endpoints:

- `GET /internal/skills/{name}/tar` → looks up the `Skill` CRD, resolves `spec.source.path`, streams the tar bytes via `repo.Open`. 404 when the Skill or its tar is missing.
- `POST /internal/skills/{name}/publish` → the publish primitive: body is a gzip tar of the skill directory; metadata via query (`displayName`, `description`, `visibility`, `builtin`). Behavior:
  1. sha256 of the body; `skill.ResolveVersion` finds an existing version with matching content (no rewrite) or the next unused `vN`.
  2. `repo.WriteBytes` the tar atomically when the content is new.
  3. Upsert the `Skill` CRD (`source.path` = `skills/<name>/vN.tar.gz`, `source.sha256`, labels when `builtin=true`), set `status.phase=Available`.
  4. Returns the `Skill` CR.
  This endpoint is the #23 Portal publish primitive; the operator seeds presets through it, and #23 adds the user-facing wrapper + upload UI.
- The API server mounts the shared repo volume (read-write: publish + serve). The operator does **not** mount it.

Alternative considered: `GET /internal/skills/tar?path=...`. Rejected — the supervisor knows the skill *name* from the resolved config; name-based keeps the API the resolver of `source` (consistent with "the CRD registers where").

## 6. Resolver + supervisor loading (repo → PVC)

### 6.1 Resolver (`internal/resolver/resolver.go`)

- `ResolvedSkill` becomes `{ Name, Path, Revision }` — `Path` is `skill.Spec.Source.Path`; content no longer inlines into the config.
- `resolveAgentConfig` keeps the enabled-skills subsetting (template `skills` ∩ instance `enabledSkills`), drops the per-agent `spec.Agents` filter (the marketplace schema has `visibility`, not per-agent lists; phase 1 = all Platform skills visible to all agents).
- `RenderSkill` is removed (the tar's own `SKILL.md` is the content).

### 6.2 Supervisor (`internal/supervisor/supervisor.go`)

- `Config` gains `APIURL` reuse (already present) + `RepoPath` is **not** needed (no mount). Add a small HTTP helper to fetch the tar.
- `renderSkills` → `syncSkills`: for each `cfg.Skills`, compare the skill's content hash against a **`.sha256` marker file** on the PVC, and pull + extract only when it differs:

```go
func (s *Supervisor) syncSkills(ctx, cfg) error {
    skillsDir := workspace/skills
    // 1. Remove skill dirs no longer enabled (stale).
    // 2. For each cfg.Skills:
    //    marker := skillsDir/<name>/.sha256
    //    if file(marker) == skill.Revision { continue }        // unchanged: no pull
    //    tar, err := GET {API}/internal/skills/<name>/tar      // stream to temp file
    //    skill.ExtractTar(tempReader, skillsDir/<name>)
    //    write marker = skill.Revision
}
```

- The trigger is unchanged: `applyConfig` runs when `cfg.Revision != s.current`; a failure in `syncSkills` returns an error, `s.current` is not advanced, and the next 10 s poll retries. `cfg.Revision` already changes whenever a skill's `source.path`/`sha256` changes (via `Skill.Revision()`), so the fingerprint is a reliable change signal.
- The `.sha256` marker survives pod restarts (it is on the PVC), so a restart does not re-pull unchanged tars.
- Fetch strategy: stream the tar to a temp file (no full-buffer in memory), `Extract`, then move the temp marker into place. The existing 15 s HTTP client timeout applies; a failed fetch/extract returns an error (→ apply fails → retry next poll).
- OpenClaw discovers `workspace/skills` at startup and hot-reloads on file changes (chokidar) — no gateway restart, same as today.

### 6.3 Supervisor tests

- `TestRenderSkills` → `TestSyncSkills`: seed a temp "repo" via `PathRepository`, build `cfg.Skills`, assert extract into a temp workspace; stale dirs cleared; unchanged skill (same marker) not re-extracted.
- `TestPollRendersOnChange` → `TestPollExtractsOnChange`: revision change → re-pull; same revision → no-op.
- Failure: missing tar (API returns 404) → error, revision not advanced, workspace untouched.
- Marker persistence: after a "restart" (new `Supervisor` over the same workspace), unchanged skills are not re-pulled.

## 7. Consumer rework

- `internal/skill/catalog.go`:
  - `ValidateSkill` rewritten for the marketplace schema: `visibility` in `{Platform, Tenant, User}` (phase 1 rejects Tenant/User with a clear message), `source.type` in `{Path, S3}`, `type=Path` requires `source.path` (mirror of the CEL rules; guards the logic without an API server). The atomic/domain validation is removed.
  - `ToolSetForAgent` simplified: generic tools + every referenced skill (all Platform-visible). The `spec.Uses` / `spec.Agents` orchestration is gone.
  - The generic-layer `CRDSchema` / `Refresh` / `SchemaFor` / `List` / `Get` / `Create` / `Delete` stay (generic kubectl tools, zero registration).
- `internal/server/handlers_platform.go`: `handleSkills` (GET `/api/skills`) returns the marketplace shape (`displayName` / `visibility` / `source` / `status.phase`). Add the internal `/internal/skills/{name}/tar` handler (§5).
- Web UI (`web/src/api/types.ts`, `web/src/api/index.ts`, `web/src/views/...`): follow the renamed fields (`title` → `displayName`, drop `instructions`, add `visibility`/`source`/`phase`). `listSkills` stays.
- e2e (`scripts/e2e.sh`, `test/e2e/bootstrap_test.go`): CRD set assertions unchanged (`skills`); builtin-skill label assertion stays; add a CEL rejection case (§9).
- RBAC: the operator already has `skills;skills/status` verbs; the API server already has full `skills` + `skills/status` grants (`deploy/charts/cubepilot/templates/rbac.yaml`) for `/api/skills`, so the new internal tar endpoint needs no RBAC change.

## 8. Config & deployment

- `internal/config/config.go`: add `SkillsDir string` (env `CUBEPILOT_SKILLS_DIR`, default `/var/lib/cubepilot/skills`) — consumed only by the API server (publish + serve); and `APIURL string` (env `CUBEPILOT_API_URL`, default `http://cubepilot-api.cubepilot.svc:8080`) — consumed by the operator (publish). The agent Pods do **not** mount the repo.
- Chart (`deploy/charts/cubepilot/`): a skill-repo volume (`values.api.skillRepo`: `storageClassName` / `accessModes` / `size`, or `existingClaim`) mounted into the **API server** (RW) at `CUBEPILOT_SKILLS_DIR`. The operator needs no volume — it publishes over HTTP. AccessModes default to `ReadWriteOnce` (the API is single-replica and kind's default StorageClass is RWO-only); production multi-replica API sets `[ReadWriteMany]` with a CephFS StorageClass (design §2.2). This is platform-install provisioning; the code only consumes `SkillsDir`.

## 9. Testing

- Repository backend unit tests: atomic write + read-back + extract, incl. failed rename, corrupt tar, path traversal (§3).
- `ValidateSkill` unit tests mirroring the CEL discriminant rules (§7).
- Supervisor `syncSkills` unit tests incl. marker persistence and failure (§6.3).
- CEL: one e2e case applies an invalid Skill (`source.type=Path` with `s3` set) and asserts the API server rejects it (`Invalid`), exercising the deployed CRD yaml. No envtest (kept simple).
- `go vet ./...` and `go test ./...` green; `npm run build` (web) green.

## 10. Out of scope

- **#23** publish flow (Portal upload → tar → repository + CRD + sha256) — consumes this story's backend rather than reimplementing it.
- **#24** install flow (browse/install → `enabledSkills` mutation) — the extraction side is in this story; the market UI + enabledSkills mutation belongs to #24.
- S3 source backend; user-private skills (`visibility: User`) — phase 2.
- A dedicated Skill status controller — `status.phase` is set by the seed/publish flow; a reconciler for user-published skills can come with #23/#24.
- Tenant/user visibility enforcement at load time — phase 1 ships Platform-only.
