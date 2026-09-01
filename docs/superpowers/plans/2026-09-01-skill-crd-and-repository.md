# Skill CRD + Skill Repository Implementation Plan (issue #22)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the phase-1 catalog `Skill` CRD with the marketplace schema, add CEL source validation, build a shared-volume repository backend (atomic write + read-back + extract), seed the built-in presets into the repo as tars, and wire the supervisor to pull + extract skills into the instance PVC via the internal API.

**Architecture:** `Skill` CRD registers `displayName/description/visibility/source(type,path,sha256)` + `status.phase` (CEL guards the `source` discriminant). Skill content (multi-file dirs) is packaged as versioned tars in a control-plane repository (`PathRepository` over `CUBEPILOT_SKILLS_DIR`). The operator seeds the four embedded presets; the API serves tars over `/internal/skills/{name}/tar`; the supervisor (in the agent Pod) pulls tars only when a skill's content revision changes (per-skill `.sha256` marker on the PVC) and extracts into `workspace/skills/<name>/`, which OpenClaw hot-reloads.

**Tech Stack:** Go, controller-runtime (types + reconciler), controller-gen v0.19.0 (deepcopy + CRD yaml), net/http, archive/tar, React 18 (web).

**Spec:** [2026-09-01-skill-crd-and-repository-design.md](../specs/2026-09-01-skill-crd-and-repository-design.md)

## Global Constraints

- Branch: `feat/issue22-skill-crd-repo` (worktree `.claude/worktrees/feat-issue22-skill-crd-repo`). Base = upstream/main.
- No compatibility layer for the old catalog schema (first version not released) — replace cleanly.
- Agent Pods do **not** mount the skill repository; the supervisor fetches tars over HTTP from the internal API.
- `controller-gen` binary at `/home/zhujian/go/bin/controller-gen` (v0.19.0 — matches the CRD annotation).
- CEL rules on `SkillSource`: `type=Path → has(path) && !has(s3)`; `type=S3 → has(s3) && !has(path)`.
- Enums: `SkillSourceType` = Path|S3; `SkillVisibility` = Platform|Tenant|User; `SkillPhase` = Available|Unreachable. Phase 1 ships Path + Platform only.
- Repository backend: atomic writes (temp file + rename, temp removed on any failure); Extract rejects `..` traversal.
- Seed is idempotent (identical content not rewritten; content change bumps version `v1 → v2`).
- Supervisor: pull only when a skill's content revision differs (`.sha256` marker on the PVC); a failure fails the apply (revision not advanced, next poll retries).
- **Build-status windows:** `go build ./...` is intentionally broken between Task 3 and Task 7 (packages migrate one at a time). Run package-scoped `go build ./internal/<pkg>/...` + `go test ./internal/<pkg>/...` until Task 7 restores the full build. Do **not** "fix" unrelated packages to make the full build pass early.
- Commit per task, signed-off (`git commit -s`), English messages, `Assisted-by: Claude Code` line.
- Final verification: `go vet ./...`, `go test ./...`, `npm run build` (in `web/`), e2e on kind.

---

### Task 1: Repository backend

**Files:**
- Create: `internal/skill/repository.go`
- Test: `internal/skill/repository_test.go`

**Interfaces:**
- Produces: `type Repository interface { Write(ctx, relPath string, src fs.FS) (string, error); Open(ctx, relPath string) (io.ReadCloser, error); Extract(ctx, relPath, destDir string) error }`; `type PathRepository struct{ Root string }` (implements Repository); `func ExtractTar(r io.Reader, destDir string) error`; `func PackSha256(src fs.FS) (string, error)`.

- [ ] **Step 1: Write the failing test** — `internal/skill/repository_test.go`

```go
package skill

import (
	"archive/tar"
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func TestWriteThenExtract(t *testing.T) {
	root := t.TempDir()
	repo := &PathRepository{Root: root}
	src := fstest.MapFS{
		"SKILL.md":     {Data: []byte("# hello\n")},
		"scripts/a.sh": {Data: []byte("#!/bin/sh\n")},
	}
	sha, err := repo.Write(t.Context(), "skills/harbor/v1.tar.gz", src)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if sha == "" {
		t.Fatal("Write returned empty sha256")
	}
	dest := filepath.Join(t.TempDir(), "workspace", "skills", "harbor")
	if err := repo.Extract(t.Context(), "skills/harbor/v1.tar.gz", dest); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "SKILL.md")); err != nil || string(b) != "# hello\n" {
		t.Fatalf("SKILL.md = %q, %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "scripts", "a.sh")); err != nil || !strings.Contains(string(b), "#!/bin/sh") {
		t.Fatalf("scripts/a.sh = %q, %v", b, err)
	}
}

func TestWriteFailureLeavesNoTemp(t *testing.T) {
	root := t.TempDir()
	repo := &PathRepository{Root: root}
	_, err := repo.Write(t.Context(), "skills/x/v1.tar.gz", brokenFS{})
	if err == nil {
		t.Fatal("Write with broken src expected error")
	}
	matches, _ := filepath.Glob(filepath.Join(root, "skills", "x", "*.tmp-*"))
	if len(matches) != 0 {
		t.Fatalf("temp files left behind: %v", matches)
	}
	if _, err := os.Stat(filepath.Join(root, "skills", "x", "v1.tar.gz")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("target file should not exist after failure, stat err = %v", err)
	}
}

func TestExtractCorruptTar(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "bad.tar.gz"), []byte("not a tar"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo := &PathRepository{Root: root}
	if err := repo.Extract(t.Context(), "bad.tar.gz", t.TempDir()); err == nil {
		t.Fatal("corrupt tar expected error")
	}
}

func TestExtractRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	dest := filepath.Join(outside, "dest")
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "../evil", Mode: 0o644, Size: 4, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("evil")); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	if err := os.WriteFile(filepath.Join(root, "t.tar"), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	repo := &PathRepository{Root: root}
	if err := repo.Extract(t.Context(), "t.tar", dest); err == nil {
		t.Fatal("traversal entry expected error")
	}
	if _, err := os.Stat(filepath.Join(outside, "evil")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("traversal wrote outside dest: %v", err)
	}
}

func TestPackSha256Stable(t *testing.T) {
	src := fstest.MapFS{"SKILL.md": {Data: []byte("x")}}
	a, err := PackSha256(src)
	if err != nil {
		t.Fatal(err)
	}
	b, err := PackSha256(src)
	if err != nil {
		t.Fatal(err)
	}
	if a != b || a == "" {
		t.Fatalf("PackSha256 not deterministic: %q %q", a, b)
	}
}

type brokenFS struct{}

func (brokenFS) Open(string) (fs.File, error)                { return nil, errors.New("boom") }
func (brokenFS) ReadDir(string) ([]fs.DirEntry, error)       { return nil, errors.New("boom") }
func (brokenFS) ReadFile(string) ([]byte, error)             { return nil, errors.New("boom") }
func (brokenFS) Stat(string) (fs.FileInfo, error)            { return nil, errors.New("boom") }
func (brokenFS) ReadDirFS() fs.ReadDirFS                     { return nil }
func (brokenFS) Glob(string) ([]string, error)               { return nil, errors.New("boom") }
func (brokenFS) Sub(string) (fs.FS, error)                   { return nil, errors.New("boom") }
func (brokenFS) ReadFileFS() fs.ReadFileFS                   { return nil }
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/skill/ -run TestWriteThenExtract`
Expected: FAIL (undefined: PathRepository / repository.go missing)

- [ ] **Step 3: Write the implementation** — `internal/skill/repository.go`

```go
package skill

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Repository is the skill content store (design §3.4: shared file volume in
// phase 1; S3 in phase 2). The backend is transparent to the CRD and the
// loading flow — differences are only in addressing + how the injector gets
// the tar (mount read vs network pull).
type Repository interface {
	// Write packs src (a skill directory: SKILL.md + optional scripts/,
	// references/) into a tar at relPath, atomically (temp file + rename),
	// and returns the tar's sha256. On any error the temp file is removed.
	Write(ctx context.Context, relPath string, src fs.FS) (string, error)
	// Open returns a reader for the tar at relPath (read-back / serving).
	Open(ctx context.Context, relPath string) (io.ReadCloser, error)
	// Extract unpacks the tar at relPath into destDir, preserving the
	// directory structure and rejecting path traversal.
	Extract(ctx context.Context, relPath, destDir string) error
}

// PathRepository is the shared-file-volume implementation: relPath is
// relative to Root (the volume mount point).
type PathRepository struct{ Root string }

func (r *PathRepository) Write(ctx context.Context, relPath string, src fs.FS) (string, error) {
	abs := filepath.Join(r.Root, relPath)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", filepath.Dir(abs), err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(abs), "."+filepath.Base(abs)+".tmp-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after successful rename
	h := sha256.New()
	if err := writeTar(io.MultiWriter(tmp, h), src); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, abs); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (r *PathRepository) Open(ctx context.Context, relPath string) (io.ReadCloser, error) {
	return os.Open(filepath.Join(r.Root, relPath))
}

func (r *PathRepository) Extract(ctx context.Context, relPath, destDir string) error {
	f, err := os.Open(filepath.Join(r.Root, relPath))
	if err != nil {
		return err
	}
	defer f.Close()
	return ExtractTar(f, destDir)
}

// PackSha256 returns the sha256 of the tar that would be produced for src,
// without writing it. Used by the seed to detect content changes.
func PackSha256(src fs.FS) (string, error) {
	h := sha256.New()
	if err := writeTar(h, src); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ExtractTar unpacks an arbitrary tar stream (a repo read, an HTTP fetch, or
// a temp file) into destDir, rejecting path traversal (".." escapes).
func ExtractTar(r io.Reader, destDir string) error {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}
		name := filepath.Clean(filepath.FromSlash(hdr.Name))
		if name == "." || strings.HasPrefix(name, "..") || filepath.IsAbs(name) {
			return fmt.Errorf("tar entry escapes dest dir: %q", hdr.Name)
		}
		target := filepath.Join(destDir, name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		default:
			// Skip symlinks / hardlinks / other special entries.
		}
	}
}

// writeTar packs the files of src (walked recursively) into a tar on w.
func writeTar(w io.Writer, src fs.FS) error {
	tw := tar.NewWriter(w)
	defer tw.Close()
	return fs.WalkDir(src, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == "." || d.IsDir() {
			return nil // dirs are implicit
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{
			Name: filepath.ToSlash(path),
			Mode: int64(info.Mode().Perm()),
			Size: info.Size(),
		}); err != nil {
			return err
		}
		f, err := src.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tw, f)
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/skill/`
Expected: PASS (all 5 tests)

- [ ] **Step 5: Commit**

```bash
git add internal/skill/repository.go internal/skill/repository_test.go
git commit -s -m "feat(skill): add repository backend (atomic write, read-back, extract)

PathRepository over the shared volume with atomic tar writes (temp + rename),
read-back, traversal-safe Extract, and PackSha256 for idempotent seeding.
Assisted-by: Claude Code"
```

---

### Task 2: Config `SkillsDir`

**Files:**
- Modify: `internal/config/config.go`

**Interfaces:**
- Produces: `Config.SkillsDir string` (default `/var/lib/cubepilot/skills`, env `CUBEPILOT_SKILLS_DIR`).

- [ ] **Step 1: Write the failing test**

Add to `internal/config/config.go` (or a small `internal/config/config_test.go` if none exists — check first; if there is no config test file, create `config_test.go`):

```go
func TestLoadSkillsDir(t *testing.T) {
	t.Setenv("CUBEPILOT_SKILLS_DIR", "/mnt/skills")
	cfg := Load()
	if cfg.SkillsDir != "/mnt/skills" {
		t.Fatalf("SkillsDir = %q, want /mnt/skills", cfg.SkillsDir)
	}
}
```

Run: `go test ./internal/config/`
Expected: FAIL (cfg.SkillsDir undefined)

- [ ] **Step 2: Implement** — add to `Config` struct + `Load()`:

```go
	// SkillsDir is the skill repository root (shared volume). Control plane
	// only: the operator seeds it, the API serves tars from it; agent Pods
	// never mount it (the supervisor pulls over HTTP).
	SkillsDir string
```

In `Load()`: `SkillsDir: getenv("CUBEPILOT_SKILLS_DIR", "/var/lib/cubepilot/skills"),`

- [ ] **Step 3: Run test to verify it passes**

Run: `go test ./internal/config/`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add internal/config/config.go
git commit -s -m "feat(config): add SkillsDir (skill repository root)

Assisted-by: Claude Code"
```

---

### Task 3: Skill CRD marketplace schema + catalog rework

> This task migrates `internal/api/v1alpha1` + `internal/skill`. The rest of the repo intentionally does not build until Tasks 4–7.

**Files:**
- Modify: `internal/api/v1alpha1/skill_types.go`
- Modify: `internal/api/v1alpha1/zz_generated.deepcopy.go` (regenerate)
- Modify: `config/crd/bases/ai.cubestack.io_skills.yaml` (regenerate)
- Modify: `deploy/charts/cubepilot/crds/ai.cubestack.io_skills.yaml` (copy of regenerated CRD)
- Modify: `internal/skill/catalog.go`
- Modify: `internal/skill/catalog_test.go`
- Test: `internal/api/v1alpha1/skill_types_test.go` (new)

**Interfaces:**
- Produces: `SkillSourceType` (Path|S3), `SkillVisibility` (Platform|Tenant|User), `SkillPhase` (Available|Unreachable), `SkillSource{Type,Path,S3,Sha256}` with CEL markers, `SkillSpec{DisplayName,Description,Visibility,Source}`, `SkillStatus{Phase,Conditions,ObservedGeneration}`. Removed: `SkillType`, `SkillTarget`, `SkillSemantics`, `SkillFile`, and the catalog `SkillSpec` fields.
- `catalog.ValidateSkill(skill)` rewritten; `ToolSetForAgent` simplified.

- [ ] **Step 1: Write the failing test** — `internal/api/v1alpha1/skill_types_test.go` (new):

```go
package v1alpha1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSkillRevisionHashesSource(t *testing.T) {
	s := &Skill{
		ObjectMeta: metav1.ObjectMeta{Name: "harbor"},
		Spec: SkillSpec{
			DisplayName: "Harbor",
			Visibility:  SkillVisibilityPlatform,
			Source:      SkillSource{Type: SkillSourcePath, Path: "skills/harbor/v1.tar.gz", Sha256: "abc"},
		},
	}
	if s.Revision() == "" {
		t.Fatal("empty revision")
	}
	changed := s.DeepCopy()
	changed.Spec.Source.Path = "skills/harbor/v2.tar.gz"
	if changed.Revision() == s.Revision() {
		t.Fatal("revision should change when source.path changes")
	}
}

func TestSkillSpecCompilesWithMarketplaceShape(t *testing.T) {
	s := &Skill{
		ObjectMeta: metav1.ObjectMeta{Name: "harbor"},
		Spec: SkillSpec{
			DisplayName: "Harbor",
			Visibility:  SkillVisibilityPlatform,
			Source:      SkillSource{Type: SkillSourcePath, Path: "skills/harbor/v1.tar.gz"},
		},
		Status: SkillStatus{Phase: SkillPhaseAvailable},
	}
	_ = s
}
```

Run: `go test ./internal/api/v1alpha1/`
Expected: FAIL (SkillSpec has no DisplayName/Source; SkillType removed causes compile errors in the test only if referenced — this test only uses new fields, so it fails on undefined fields)

- [ ] **Step 2: Rewrite the types** — `internal/api/v1alpha1/skill_types.go`

Replace the whole file body with:

```go
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SkillSourceType is the discriminant of the skill content address (design
// §3.4). Phase 1 supports only Path; S3 is phase 2.
// +kubebuilder:validation:Enum=Path;S3
type SkillSourceType string

const (
	SkillSourcePath SkillSourceType = "Path"
	SkillSourceS3   SkillSourceType = "S3"
)

// SkillVisibility controls who may see a skill (design §3.4). Phase 1 ships
// only Platform.
// +kubebuilder:validation:Enum=Platform;Tenant;User
type SkillVisibility string

const (
	SkillVisibilityPlatform SkillVisibility = "Platform"
	SkillVisibilityTenant   SkillVisibility = "Tenant"
	SkillVisibilityUser     SkillVisibility = "User"
)

// SkillPhase is the observed reachability of the skill content.
// +kubebuilder:validation:Enum=Available;Unreachable
type SkillPhase string

const (
	SkillPhaseAvailable   SkillPhase = "Available"
	SkillPhaseUnreachable SkillPhase = "Unreachable"
)

// SkillS3Source is the object-store addressing (phase 2).
type SkillS3Source struct {
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
}

// SkillSource addresses the skill content in the repository (design §3.4:
// content lives in the repo; the CRD registers where + which version).
type SkillSource struct {
	// Type is the discriminant: Path | S3.
	Type SkillSourceType `json:"type"`
	// Path is the repo-relative tar path (e.g. skills/harbor/v1.tar.gz),
	// versioned and immutable. Required when type=Path.
	Path string `json:"path,omitempty"`
	// S3 is the object-store addressing. Forbidden when type=Path (phase 2).
	S3 *SkillS3Source `json:"s3,omitempty"`
	// Sha256 is the content fingerprint, backfilled by publish/seed. Optional:
	// manual kubectl apply may leave it empty (audit via the versioned path).
	Sha256 string `json:"sha256,omitempty"`

	// +kubebuilder:validation:XValidation:rule="self.type=='Path' ? has(self.path) && !has(self.s3) : true",message="source.type=Path requires source.path and forbids source.s3"
	// +kubebuilder:validation:XValidation:rule="self.type=='S3' ? has(self.s3) && !has(self.path) : true",message="source.type=S3 requires source.s3 and forbids source.path"
}

// SkillSpec is a marketplace skill (design §3.4). It registers "what skill
// exists, where, which version, who can see it"; the content lives in the
// repository.
type SkillSpec struct {
	// DisplayName is the market-facing title.
	DisplayName string `json:"displayName"`
	// Description explains when to use the skill.
	Description string `json:"description,omitempty"`
	// Visibility is Platform | Tenant | User; phase 1 only Platform.
	Visibility SkillVisibility `json:"visibility"`
	// Source addresses the skill content (discriminant type).
	Source SkillSource `json:"source"`
}

// SkillStatus is the observed state of a Skill.
type SkillStatus struct {
	// Phase is Available | Unreachable. Set by the seed/publish flow after the
	// tar is written to the repository.
	Phase SkillPhase `json:"phase,omitempty"`
	// Conditions carry condition details.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// ObservedGeneration is the most recent generation observed.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="DisplayName",type="string",JSONPath=".spec.displayName"
// +kubebuilder:printcolumn:name="Visibility",type="string",JSONPath=".spec.visibility"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// Skill is the skill catalog entry (design §3.4). Generic tools are
// platform-provided and need no registration.
type Skill struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SkillSpec   `json:"spec,omitempty"`
	Status SkillStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SkillList contains a list of Skill.
type SkillList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Skill `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Skill{}, &SkillList{})
}

// Revision returns an immutable content fingerprint of the skill spec
// (design §3.4/§3.5: TaskRuns record the skill revision actually used at run
// time for audit/rollback). Content hash -- deterministic across object
// re-creation, spec-only (status updates never change the revision).
func (c *Skill) Revision() string {
	return specRevision(c.Spec)
}
```

- [ ] **Step 3: Regenerate deepcopy + CRD yaml**

```bash
cd /home/zhujian/code/github.com/suanova/cubepilot/.claude/worktrees/feat-issue22-skill-crd-repo
/home/zhujian/go/bin/controller-gen object paths=./internal/api/v1alpha1/...
/home/zhujian/go/bin/controller-gen crd paths=./internal/api/v1alpha1/... output:crd:artifacts:config=config/crd/bases
```

Verify: `grep -c "x-kubernetes-validations" config/crd/bases/ai.cubestack.io_skills.yaml` → 2. `git diff --stat` → skills yaml changed; other CRD yamls unchanged (controller-gen v0.19.0 matches the annotation, so unchanged types are byte-stable — if others changed, `git checkout` them).
Copy the regenerated CRD to the chart: `cp config/crd/bases/ai.cubestack.io_skills.yaml deploy/charts/cubepilot/crds/ai.cubestack.io_skills.yaml`.

- [ ] **Step 4: Rework `internal/skill/catalog.go`**

Replace `ValidateSkill` and `ToolSetForAgent` with the marketplace logic:

```go
// ValidateSkill validates a Skill registration (design §3.4): the source
// discriminant mirrors the CRD CEL rules (guards the logic without an API
// server) and phase 1 admits only Platform visibility.
func (c *Catalog) ValidateSkill(skill *v1alpha1.Skill) error {
	switch skill.Spec.Visibility {
	case v1alpha1.SkillVisibilityPlatform:
		// OK — phase 1 only.
	case v1alpha1.SkillVisibilityTenant, v1alpha1.SkillVisibilityUser:
		return fmt.Errorf("skill %s: visibility %q is phase 2 (only Platform in phase 1)", skill.Name, skill.Spec.Visibility)
	default:
		return fmt.Errorf("skill %s: invalid visibility %q", skill.Name, skill.Spec.Visibility)
	}
	switch skill.Spec.Source.Type {
	case v1alpha1.SkillSourcePath:
		if skill.Spec.Source.Path == "" {
			return fmt.Errorf("skill %s: source.type=Path requires source.path", skill.Name)
		}
		if skill.Spec.Source.S3 != nil {
			return fmt.Errorf("skill %s: source.type=Path forbids source.s3", skill.Name)
		}
	case v1alpha1.SkillSourceS3:
		return fmt.Errorf("skill %s: source.type=S3 is phase 2", skill.Name)
	default:
		return fmt.Errorf("skill %s: invalid source.type %q", skill.Name, skill.Spec.Source.Type)
	}
	return nil
}

// ToolSetForAgent computes the effective tool set for an AgentTemplate: the
// generic tools are always available; each referenced Skill (all
// Platform-visible in phase 1) contributes its name.
func ToolSetForAgent(agent *v1alpha1.AgentTemplate, skills []v1alpha1.Skill) []string {
	set := map[string]bool{}
	for _, t := range GenericTools {
		set[t] = true
	}
	for _, ref := range agent.Spec.Skills {
		for _, skill := range skills {
			if skill.Name == ref {
				set[ref] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}
```

Remove the `contains` helper if now unused (check). Keep the generic-layer `CRDSchema` / `Refresh` / `SchemaFor` / `FindKind` / `List` / `Get` / `Create` / `Delete` / `CRDExists` — unchanged.

- [ ] **Step 5: Update `internal/skill/catalog_test.go`**

Rewrite `TestValidateSkill` for the marketplace rules and simplify `TestToolSetForAgent` (drop `Spec.Uses`/`Spec.Agents`):

```go
func TestValidateSkill(t *testing.T) {
	c := &Catalog{}

	ok := &v1alpha1.Skill{
		ObjectMeta: metav1.ObjectMeta{Name: "harbor"},
		Spec: v1alpha1.SkillSpec{
			DisplayName: "Harbor",
			Visibility:  v1alpha1.SkillVisibilityPlatform,
			Source:      v1alpha1.SkillSource{Type: v1alpha1.SkillSourcePath, Path: "skills/harbor/v1.tar.gz"},
		},
	}
	if err := c.ValidateSkill(ok); err != nil {
		t.Errorf("valid skill rejected: %v", err)
	}

	noPath := ok.DeepCopy()
	noPath.Spec.Source.Path = ""
	if err := c.ValidateSkill(noPath); err == nil {
		t.Error("type=Path without path accepted")
	}

	withS3 := ok.DeepCopy()
	withS3.Spec.Source.S3 = &v1alpha1.SkillS3Source{Bucket: "b", Key: "k"}
	if err := c.ValidateSkill(withS3); err == nil {
		t.Error("type=Path with s3 accepted")
	}

	userVis := ok.DeepCopy()
	userVis.Spec.Visibility = v1alpha1.SkillVisibilityUser
	if err := c.ValidateSkill(userVis); err == nil {
		t.Error("visibility:User accepted in phase 1")
	}

	s3 := ok.DeepCopy()
	s3.Spec.Source = v1alpha1.SkillSource{Type: v1alpha1.SkillSourceS3}
	if err := c.ValidateSkill(s3); err == nil {
		t.Error("source.type=S3 accepted in phase 1")
	}
}

func TestToolSetForAgent(t *testing.T) {
	agent := &v1alpha1.AgentTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-for-cloud"},
		Spec: v1alpha1.AgentTemplateSpec{Skills: []string{"harbor", "cluster-inspection"}},
	}
	skills := []v1alpha1.Skill{
		{ObjectMeta: metav1.ObjectMeta{Name: "harbor"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "cluster-inspection"}},
	}
	got := ToolSetForAgent(agent, skills)
	want := map[string]bool{
		"list-kinds": true, "describe-kind": true, "resource-manager": true,
		"kubectl-raw": true, "harbor": true, "cluster-inspection": true,
	}
	if len(got) != len(want) {
		t.Fatalf("tool set = %v, want %v", got, want)
	}
	for _, tool := range got {
		if !want[tool] {
			t.Errorf("unexpected tool %q", tool)
		}
	}
}
```

`TestFindKind` stays unchanged.

- [ ] **Step 6: Run package tests to verify green**

Run: `go build ./internal/api/v1alpha1/... ./internal/skill/... && go test ./internal/api/v1alpha1/... ./internal/skill/...`
Expected: PASS. (Other packages still broken — expected.)

- [ ] **Step 7: Commit**

```bash
git add internal/api/v1alpha1/skill_types.go internal/api/v1alpha1/zz_generated.deepcopy.go internal/api/v1alpha1/skill_types_test.go internal/skill/catalog.go internal/skill/catalog_test.go config/crd/bases/ai.cubestack.io_skills.yaml deploy/charts/cubepilot/crds/ai.cubestack.io_skills.yaml
git commit -s -m "feat(api,skill): marketplace Skill CRD schema + CEL + catalog rework

Replace the phase-1 catalog schema with displayName/description/visibility/
source(type,path,sha256) + status.phase; CEL guards the source discriminant;
ValidateSkill mirrors the CEL rules; ToolSetForAgent simplified.
Assisted-by: Claude Code"
```

---

### Task 4: Controller — preset seeding + bootstrap

**Files:**
- Modify: `internal/controller/skill_source.go`
- Modify: `internal/controller/builtin.go`
- Modify: `internal/controller/builtin_test.go`
- Modify: `cmd/cubepilot-operator/main.go`

**Interfaces:**
- Produces: `SeedBuiltinSkills(ctx context.Context, repo skill.Repository) ([]*v1alpha1.Skill, error)`; `BuiltinBootstrapReconciler.Repo skill.Repository` field.
- Consumes: `skill.Repository`, `skill.PackSha256`, `skill.PathRepository`.

- [ ] **Step 1: Write the failing test** — `internal/controller/skill_source_test.go` (new):

```go
package controller

import (
	"testing"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/skill"
)

func TestSeedBuiltinSkills(t *testing.T) {
	repo := &skill.PathRepository{Root: t.TempDir()}
	skills, err := SeedBuiltinSkills(t.Context(), repo)
	if err != nil {
		t.Fatalf("SeedBuiltinSkills: %v", err)
	}
	if len(skills) == 0 {
		t.Fatal("no preset skills seeded")
	}
	for _, s := range skills {
		if s.Spec.Visibility != v1alpha1.SkillVisibilityPlatform {
			t.Errorf("skill %s: visibility = %q, want Platform", s.Name, s.Spec.Visibility)
		}
		if s.Spec.Source.Type != v1alpha1.SkillSourcePath || s.Spec.Source.Path == "" {
			t.Errorf("skill %s: source = %+v, want Path with a path", s.Name, s.Spec.Source)
		}
		if s.Spec.Source.Sha256 == "" {
			t.Errorf("skill %s: sha256 not backfilled", s.Name)
		}
		// Idempotent: re-seeding writes no new version and returns the same path.
		again, err := SeedBuiltinSkills(t.Context(), repo)
		if err != nil {
			t.Fatalf("re-seed: %v", err)
		}
		if again[0].Spec.Source.Path != skills[0].Spec.Source.Path {
			t.Errorf("re-seed changed path %q -> %q", skills[0].Spec.Source.Path, again[0].Spec.Source.Path)
		}
	}
}
```

Run: `go test ./internal/controller/ -run TestSeedBuiltinSkills`
Expected: FAIL (SeedBuiltinSkills undefined)

- [ ] **Step 2: Rewrite `internal/controller/skill_source.go`**

Keep `skillsFS` (go:embed), `skillMeta`, `parseSKILL`, `skillTitle`, `loadSkill`, `presetSkillNames`. Replace `BuiltinSkillDefinitions` with:

```go
// SeedBuiltinSkills packs the embedded preset skill directories into the
// repository as versioned tars (skills/<name>/v1.tar.gz) and returns the
// Skill CRDs referencing them (source.path + source.sha256 backfilled).
// Idempotent: an existing version whose content matches is not rewritten; a
// content change writes the next version.
func SeedBuiltinSkills(ctx context.Context, repo skill.Repository) ([]*v1alpha1.Skill, error) {
	names, err := presetSkillNames()
	if err != nil {
		return nil, err
	}
	var out []*v1alpha1.Skill
	for _, name := range names {
		meta, body, err := loadSkill(name)
		if err != nil {
			return nil, err
		}
		sub, err := fs.Sub(skillsFS, "skills/"+name)
		if err != nil {
			return nil, err
		}
		ver, sha, err := seedVersion(ctx, repo, name, sub)
		if err != nil {
			return nil, err
		}
		out = append(out, &v1alpha1.Skill{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
				Labels: map[string]string{
					"app.kubernetes.io/part-of": "cubepilot",
					"cubepilot/builtin":         "true",
				},
			},
			Spec: v1alpha1.SkillSpec{
				DisplayName: skillTitle(body),
				Description: meta.Description,
				Visibility:  v1alpha1.SkillVisibilityPlatform,
				Source: v1alpha1.SkillSource{
					Type:   v1alpha1.SkillSourcePath,
					Path:   fmt.Sprintf("skills/%s/%s.tar.gz", name, ver),
					Sha256: sha,
				},
			},
			Status: v1alpha1.SkillStatus{Phase: v1alpha1.SkillPhaseAvailable},
		})
	}
	return out, nil
}

// seedVersion writes the skill dir as skills/<name>/vN.tar.gz, bumping N when
// the packed content differs from an existing version; returns the version
// label and the tar sha256.
func seedVersion(ctx context.Context, repo skill.Repository, name string, sub fs.FS) (string, string, error) {
	packed, err := skill.PackSha256(sub)
	if err != nil {
		return "", "", err
	}
	for v := 1; ; v++ {
		p := fmt.Sprintf("skills/%s/v%d.tar.gz", name, v)
		rc, err := repo.Open(ctx, p)
		if err != nil {
			sha, err := repo.Write(ctx, p, sub)
			return fmt.Sprintf("v%d", v), sha, err
		}
		h := sha256.New()
		if _, err := io.Copy(h, rc); err != nil {
			rc.Close()
			return "", "", err
		}
		rc.Close()
		if hex.EncodeToString(h.Sum(nil)) == packed {
			return fmt.Sprintf("v%d", v), packed, nil
		}
	}
}
```

Add imports: `context`, `crypto/sha256`, `encoding/hex`, `fmt`, `io`, `io/fs` (already), `github.com/suanova/cubepilot/internal/skill`.

- [ ] **Step 3: Wire the reconciler** — `internal/controller/builtin.go`

Add field + seed step:

```go
type BuiltinBootstrapReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Cfg    config.Config
	Repo   skill.Repository // skill repository (shared volume), for seeding presets
}
```

In `ensureBuiltin`, replace step 2 (currently `BuiltinSkillDefinitions()`) with:

```go
	// 2. Domain skills (packed into the skill repository + Skill CRDs).
	skills, err := SeedBuiltinSkills(ctx, r.Repo)
	if err != nil {
		return fmt.Errorf("seed skills: %w", err)
	}
	for _, s := range skills {
		if err := r.createIfMissing(ctx, s); err != nil {
			return err
		}
		// status subresource: mark the seeded skill Available (patch is
		// idempotent).
		if err := r.patchSkillPhase(ctx, s.Name, v1alpha1.SkillPhaseAvailable); err != nil {
			return err
		}
	}
```

Add helper (in builtin.go):

```go
// patchSkillPhase sets status.phase via the status subresource (idempotent).
func (r *BuiltinBootstrapReconciler) patchSkillPhase(ctx context.Context, name string, phase v1alpha1.SkillPhase) error {
	skill := &v1alpha1.Skill{ObjectMeta: metav1.ObjectMeta{Name: name}}
	return r.Status().Patch(ctx, skill, client.RawPatch(types.MergePatchType, []byte(
		fmt.Sprintf(`{"status":{"phase":%q}}`, phase))))
}
```

Update imports in builtin.go (add `skill` package, `types` from apimachinery).

If `r.Repo` is nil (unit tests), guard `SeedBuiltinSkills` — in `ensureBuiltin`, `if r.Repo == nil { return fmt.Errorf("skill repository not configured") }`.

- [ ] **Step 4: Update `cmd/cubepilot-operator/main.go`**

Where `BuiltinBootstrapReconciler` is constructed, add `Repo: &skill.PathRepository{Root: cfg.SkillsDir}`. Add the `skill` import.

- [ ] **Step 5: Update `internal/controller/builtin_test.go`**

`BuiltinSkillDefinitions` is gone. If the test calls it (via `ensureBuiltin`), construct the reconciler with `Repo: &skill.PathRepository{Root: t.TempDir()}`. Assert the builtin Skill CRDs now carry `source.path` + `visibility: Platform`. Adjust the fake client / expectations accordingly.

- [ ] **Step 6: Run package tests**

Run: `go build ./internal/controller/... ./cmd/... && go test ./internal/controller/...`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add internal/controller/skill_source.go internal/controller/skill_source_test.go internal/controller/builtin.go internal/controller/builtin_test.go cmd/cubepilot-operator/main.go
git commit -s -m "feat(controller): seed builtin skills into the repository + Skill CRDs

SeedBuiltinSkills packs the embedded preset SKILL.md dirs into the repo as
versioned tars and returns Skill CRDs (source.path + sha256 + phase Available);
the builtin bootstrap reconciler seeds then creates the CRDs.
Assisted-by: Claude Code"
```

---

### Task 5: Resolver — `ResolvedSkill` source refs

**Files:**
- Modify: `internal/resolver/resolver.go`
- Modify: `internal/resolver/resolver_test.go`

**Interfaces:**
- Produces: `ResolvedSkill{Name, Path, Revision string}`; `resolver.RenderSkill` **removed**.
- Consumes: `v1alpha1.SkillSpec.Source.Path`.

- [ ] **Step 1: Write the failing test** — update `internal/resolver/resolver_test.go`:

Assert `ResolvedAgentConfig.Skills` entries carry the skill's `source.path` and are filtered by `enabledSkills` (drop assertions on `Instructions`/`Title`).

```go
func TestResolveSkillsCarrySourcePath(t *testing.T) {
	// Build a Resolver over a fake client with one Skill:
	//   name=harbor, Spec.Source.Path=skills/harbor/v1.tar.gz
	// and an AgentInstance with EnabledSkills=[harbor].
	// cfg.Skills[0].Path must == "skills/harbor/v1.tar.gz".
}
```

Run: `go test ./internal/resolver/ -run TestResolveSkillsCarrySourcePath`
Expected: FAIL (compile — old ResolvedSkill fields in existing tests)

- [ ] **Step 2: Implement**

`ResolvedSkill` becomes:

```go
// ResolvedSkill is one enabled skill's content reference (design §3.4: the
// content lives in the repository; the supervisor pulls the tar by name).
type ResolvedSkill struct {
	Name     string `json:"name"`
	Path     string `json:"path,omitempty"` // repo-relative tar path
	Revision string `json:"revision"`
}
```

In `resolveAgentConfig`, replace the skill loop body with:

```go
	for i := range skills.Items {
		skill := &skills.Items[i]
		if len(restrict) > 0 && !restrict[skill.Name] {
			continue
		}
		cfg.Skills = append(cfg.Skills, ResolvedSkill{
			Name:     skill.Name,
			Path:     skill.Spec.Source.Path,
			Revision: skill.Revision(),
		})
	}
```

Remove `RenderSkill` and the `uses`/`files` helpers it used (keep `contains` if still referenced — check). Remove the `v1alpha1.SkillFile` import if unused.

- [ ] **Step 3: Update `internal/resolver/resolver_test.go`**

Existing tests referencing `ResolvedSkill.Instructions` / `resolver.RenderSkill` must be updated: skills are now name/path/revision refs. Adjust assertions.

- [ ] **Step 4: Run package tests**

Run: `go build ./internal/resolver/... && go test ./internal/resolver/...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/resolver/resolver.go internal/resolver/resolver_test.go
git commit -s -m "refactor(resolver): ResolvedSkill carries source path only

ResolvedSkill is now {Name, Path, Revision}; RenderSkill removed (the tar's
own SKILL.md is the content). enabledSkills subsetting stays.
Assisted-by: Claude Code"
```

---

### Task 6: Internal API — skill tar endpoint

**Files:**
- Modify: `internal/server/handlers_platform.go`
- Modify: `internal/server/server.go`
- Modify: `internal/server/internal_api_test.go`

**Interfaces:**
- Produces: `GET /internal/skills/{name}/tar` → tar bytes (gzip).
- Consumes: `Config.SkillsDir`, `v1alpha1.Skill`, `skill.PathRepository`.

- [ ] **Step 1: Write the failing test** — extend `internal/server/internal_api_test.go`:

```go
func TestInternalSkillTar(t *testing.T) {
	// Build a Server with cfg.SkillsDir = t.TempDir(); write a tar at
	// skills/harbor/v1.tar.gz via &skill.PathRepository{...}.Write(...).
	// GET /internal/skills/harbor/tar -> 200, body is the tar.
	// GET /internal/skills/missing/tar -> 404.
}
```

Run: `go test ./internal/server/ -run TestInternalSkillTar`
Expected: FAIL (handler not registered)

- [ ] **Step 2: Implement the handler** — `internal/server/handlers_platform.go`:

```go
// handleInternalSkillTar serves the tar for one skill: GET
// /internal/skills/{name}/tar (internal API, cluster-only). The supervisor
// pulls this to extract skills into the instance PVC.
func (s *Server) handleInternalSkillTar(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET required"})
		return
	}
	name := r.PathValue("name")
	if name == "" {
		http.NotFound(w, r)
		return
	}
	if s.cr == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "k8s client unavailable"})
		return
	}
	var skillCR v1alpha1.Skill
	if err := s.cr.Get(r.Context(), client.ObjectKey{Name: name}, &skillCR); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	repo := &skill.PathRepository{Root: s.cfg.SkillsDir}
	rc, err := repo.Open(r.Context(), skillCR.Spec.Source.Path)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "skill tar not found"})
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/gzip")
	if _, err := io.Copy(w, rc); err != nil {
		return
	}
}
```

Register the route in `server.go` `Handler()`:

```go
	mux.HandleFunc("/internal/skills/{name}/tar", s.handleInternalSkillTar)
```

Add imports (`io`, `v1alpha1`, `skill`) as needed to `handlers_platform.go`.

- [ ] **Step 3: Run package tests**

Run: `go build ./internal/server/... && go test ./internal/server/...`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add internal/server/handlers_platform.go internal/server/server.go internal/server/internal_api_test.go
git commit -s -m "feat(server): internal skill tar endpoint

GET /internal/skills/{name}/tar streams the skill tar from the repository for
the supervisor's extract-into-PVC loading.
Assisted-by: Claude Code"
```

---

### Task 7: Supervisor — HTTP pull + extract + sha256 markers

> `go build ./...` and `go test ./...` turn green at the end of this task.

**Files:**
- Modify: `internal/supervisor/supervisor.go`
- Modify: `internal/supervisor/supervisor_test.go`

**Interfaces:**
- Consumes: `resolver.ResolvedSkill{Name,Path,Revision}`, `skill.ExtractTar`, `Config.APIURL`, `Config.Workspace`.

- [ ] **Step 1: Write the failing test** — update `internal/supervisor/supervisor_test.go`:

```go
func TestSyncSkillsExtractsAndSkipsUnchanged(t *testing.T) {
	// httptest server serving /internal/skills/{name}/tar from a PathRepository.
	// Supervisor cfg: Workspace=tmp/ws, APIURL=srv.URL, User=u.
	// cfg1.Skills = [{Name:"harbor", Path:"skills/harbor/v1.tar.gz", Revision:"r1"}]
	// syncSkills -> workspace/skills/harbor/SKILL.md exists, .sha256 == "r1".
	// cfg2 same skill same revision -> syncSkills no-op (no HTTP hit; use a
	//    counting handler -> count stays 1).
	// cfg3 revision "r2" -> re-pull (count == 2), .sha256 == "r2".
}

func TestSyncSkillsStaleRemoval(t *testing.T) {
	// cfg has one skill; pre-existing workspace/skills/stale dir -> removed.
}

func TestSyncSkillsFailureKeepsRevisionUnapplied(t *testing.T) {
	// 404 on the tar -> syncSkills returns error; workspace unchanged.
}
```

Run: `go test ./internal/supervisor/ -run TestSyncSkills`
Expected: FAIL (syncSkills undefined; existing TestRenderSkills also fails)

- [ ] **Step 2: Implement** — replace `renderSkills` in `internal/supervisor/supervisor.go`:

```go
// syncSkills pulls the enabled skills' tars from the internal API and
// extracts them into Workspace/skills/<name>/ (clearing stale dirs first).
// A skill is re-pulled only when its content revision differs from the
// .sha256 marker on the PVC (survives pod restarts). The gateway hot-reloads
// the extracted files itself; the supervisor never restarts it.
func (s *Supervisor) syncSkills(ctx context.Context, cfg *resolver.ResolvedAgentConfig) error {
	skillsDir := filepath.Join(s.cfg.Workspace, "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		return err
	}
	wanted := make(map[string]*resolver.ResolvedSkill, len(cfg.Skills))
	for i := range cfg.Skills {
		wanted[cfg.Skills[i].Name] = &cfg.Skills[i]
	}
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if _, ok := wanted[e.Name()]; !ok {
			if err := os.RemoveAll(filepath.Join(skillsDir, e.Name())); err != nil {
				return err
			}
		}
	}
	for name, skill := range wanted {
		if err := s.syncSkill(ctx, *skill, skillsDir); err != nil {
			return fmt.Errorf("sync skill %s: %w", name, err)
		}
	}
	return nil
}

// syncSkill pulls one skill's tar and extracts it, unless the .sha256 marker
// already matches the desired revision.
func (s *Supervisor) syncSkill(ctx context.Context, skill resolver.ResolvedSkill, skillsDir string) error {
	dir := filepath.Join(skillsDir, skill.Name)
	marker := filepath.Join(dir, ".sha256")
	if b, err := os.ReadFile(marker); err == nil && string(b) == skill.Revision {
		return nil // unchanged
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	u := fmt.Sprintf("%s/internal/skills/%s/tar", strings.TrimRight(s.cfg.APIURL, "/"), url.PathEscape(skill.Name))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch %s: %d", u, resp.StatusCode)
	}
	tmpDir, err := os.MkdirTemp(skillsDir, "."+skill.Name+".tmp-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	if err := skill.ExtractTar(resp.Body, tmpDir); err != nil {
		return err
	}
	if err := os.Rename(tmpDir, dir); err != nil {
		return err
	}
	if err := os.WriteFile(marker, []byte(skill.Revision), 0o644); err != nil {
		return err
	}
	log.Printf("supervisor: skill %s/%s extracted", skill.Name, skill.Revision)
	return nil
}
```

Rename the call in `applyConfig` (`renderSkills` → `syncSkills`). Add imports: `net/url`, `github.com/suanova/cubepilot/internal/skill` (as `skill`). Remove the `resolver.RenderSkill` usage. Note: `skill.ExtractTar` is the pure helper from Task 1.

- [ ] **Step 3: Update `internal/supervisor/supervisor_test.go`**

`TestRenderSkills` → use the new sync flow; `TestPollRendersOnChange` → `TestPollSyncsOnChange` (revision change → re-pull). The existing test server must serve `/internal/agents/{user}/config` **and** `/internal/skills/{name}/tar` (or the sync test uses its own httptest server).

- [ ] **Step 4: Run the full suite**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS (full repo green again)

- [ ] **Step 5: Commit**

```bash
git add internal/supervisor/supervisor.go internal/supervisor/supervisor_test.go
git commit -s -m "feat(supervisor): pull + extract skills from the internal API

syncSkills fetches each enabled skill's tar only when its content revision
differs (per-skill .sha256 marker on the PVC) and extracts into
workspace/skills; OpenClaw hot-reloads. Failure fails the apply and retries.
Assisted-by: Claude Code"
```

---

### Task 8: Web UI field updates

**Files:**
- Modify: `web/src/api/types.ts`, `web/src/api/index.ts`, `web/src/views/AgentView.tsx`, `web/src/views/ChatView.tsx` (anywhere the old skill shape is rendered)

**Interfaces:**
- Consumes: `/api/skills` now returns `{displayName, description, visibility, source{type,path,sha256}, status.phase}`.

- [ ] **Step 1: Update the skill type + usages**

In `web/src/api/types.ts`, the skill object becomes `{ name, displayName, description, visibility, source: { type, path, sha256 }, phase }`. Update `listSkills` consumers in `AgentView.tsx` / `ChatView.tsx` (they render `title` → `displayName`; drop any `instructions` display).

Run: `npm run build` (from `web/`)
Expected: PASS

- [ ] **Step 2: Commit**

```bash
git add web/src/api/types.ts web/src/api/index.ts web/src/views/AgentView.tsx web/src/views/ChatView.tsx
git commit -s -m "feat(web): follow marketplace Skill fields (displayName/source/phase)

Assisted-by: Claude Code"
```

---

### Task 9: e2e CEL rejection + full verification

**Files:**
- Modify: `test/e2e/bootstrap_test.go`
- Modify: `scripts/e2e.sh` (only if the CRD set changed — it did not)

- [ ] **Step 1: Add a CEL rejection e2e case**

In `test/e2e/bootstrap_test.go` (or a new `test/e2e/skill_cel_test.go`), apply an invalid Skill and assert the API server rejects it:

```go
var _ = Describe("Skill CEL validation", func() {
	It("rejects source.type=Path with s3 set", func() {
		bad := &v1alpha1.Skill{
			ObjectMeta: metav1.ObjectMeta{Name: "bad-skill", GenerateName: "bad-skill-"},
			Spec: v1alpha1.SkillSpec{
				DisplayName: "Bad",
				Visibility:  v1alpha1.SkillVisibilityPlatform,
				Source: v1alpha1.SkillSource{
					Type: v1alpha1.SkillSourcePath,
					S3:   &v1alpha1.SkillS3Source{Bucket: "b", Key: "k"},
				},
			},
		}
		err := framework.Client.Create(ctx, bad)
		Expect(err).To(HaveOccurred()) // Invalid: forbidden by CEL
	})
})
```

Run the e2e suite on kind (`scripts/e2e.sh` or the existing e2e path). Also verify the bootstrap still creates the 4 builtin Skill CRDs with `source.path` + `visibility=Platform`.

- [ ] **Step 2: Full verification**

```bash
go vet ./...
go test ./...
cd web && npm run build
```

Expected: all green.

- [ ] **Step 3: Commit**

```bash
git add test/e2e/ scripts/e2e.sh
git commit -s -m "test(e2e): CEL rejects invalid Skill source combos

Assisted-by: Claude Code"
```

---

## Self-review notes

- Spec coverage: §2 (CRD+CEL) → Task 3; §3 (repository) → Task 1; §4 (seed) → Task 4; §5 (tar endpoint) → Task 6; §6 (resolver + supervisor) → Tasks 5+7; §7 (consumers) → Tasks 3–8; §8 (config/deploy) → Task 2 (+ chart note below); §9 (testing) → per-task + Task 9.
- Chart shared-volume provisioning (§8) is intentionally left as follow-up (platform-install concern); the code only consumes `SkillsDir`. Note in the PR description.
- `ToolSetForAgent` is only exercised by its unit test (no production caller) — keep it as the tool-set contract.
