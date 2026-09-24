# Workspace Artifact Ownership Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the pod-side copy of the platform's config -- rendered skills, the
AGENTS.md managed block, `openclaw.json` -- converge back to the platform's state
within one poll, and stop the platform from touching what the agent authored.

**Architecture:** One ownership rule decides everything: an artifact carrying the
platform's marker is the platform's and converges; anything without it is the
agent's and is never read, rewritten or deleted. Skills carry a `.cubepilot.json`
marker (ownership, plus a record of the installed revision and content hash) while
the value drift is judged against is derived per process from the fetched tarball,
never read back from the writable marker. `openclaw.json` is compared against the
bytes on disk. The agent's operating conventions and the resolved instructions
share one managed block in `AGENTS.md`, and the image seed becomes copy-if-missing
so agent edits survive a pod start.

**Tech Stack:** Go (supervisor + `internal/skill` + `internal/gateway`), shell in
the pod-spec initContainer, Ginkgo/Gomega e2e against kind with client-go
`remotecommand` for in-pod assertions.

**Spec:** `docs/superpowers/specs/2026-09-24-workspace-artifact-ownership-design.md`
(issue #238)

## Global Constraints

- Code, comments, commit messages and anything written to GitHub are English.
- No user-facing string may carry an issue/PR/FR number or a milestone label;
  numbers are allowed in source comments only.
- Pre-release: no compatibility or fallback branches for older data, older clients
  or an older gateway. No migration code for a pre-existing marker or a
  pre-existing AGENTS.md.
- `make test` does not run golangci-lint; run `golangci-lint run` separately before
  every commit.
- `go test ./...` skips `test/e2e`; run `make test` (vet + unit) for the Go half.
- Every commit carries `-s` (DCO) and, when AI-assisted, an `Assisted-by: Claude
  Code` trailer.
- The poll interval is 10s (`Config.PollInterval`); "within one poll" is the
  guarantee the design claims, so tests assert on the poll, not on a timer.

## Design decisions

The spec is the authority; these are the points the tasks must not drift from.

1. **The marker is a record, not a trust anchor.** `.cubepilot.json` lives in the
   same agent-writable directory as the skill. Drift is judged against a tree hash
   this process computed from the fetched tarball; the marker only says "the
   platform put this directory here" (for cleanup) and records what was installed.
2. **The expectation is keyed by resolved identity**, not by name: `revision`,
   tar `path`, `sha256`. Keyed by name alone, a new platform revision would match
   the previous content forever.
3. **A pod start re-fetches every wanted skill** rather than trusting a marker
   match, so the check always begins from platform content.
4. **Cleanup removes only marked directories** that left the resolved set. An
   unmarked directory is an agent-authored skill. One narrow edge is accepted and
   commented: a platform name that leaves the set and is re-created by the agent
   before that poll's cleanup runs is removed as a platform one.
5. **`skills.workshop.autonomous.mode: "off"`** (with `approvalPolicy: "auto"`),
   rendered explicitly so platform behaviour does not depend on an upstream
   default, plus the create-then-apply convention stated in the managed block.
6. **The seed is copy-if-missing**, mirroring OpenClaw's own `writeFileIfMissing`
   for workspace bootstrap files.

## File map

| File | Change |
|---|---|
| `internal/skill/repository.go` | add `TreeHash` next to `ExtractTar` |
| `internal/skill/repository_test.go` | `TestTreeHash` |
| `internal/supervisor/supervisor.go` | skill marker + per-process expectations, `syncSkill` rewrite, ownership-aware cleanup, `applyConfig` verifies every poll, `applyGatewayConfig` compares disk, persona embed + managed block |
| `internal/supervisor/supervisor_test.go` | rewrite the marker/skill/managed-block tests, add drift + forged-marker + revision-change + pod-start cases |
| `internal/supervisor/persona.md` | new: the platform's operating conventions (moved out of the image workspace) |
| `workspace/AGENTS.md` | deleted (its content becomes `internal/supervisor/persona.md`) |
| `workspace/SOUL.md` | trimmed to tone only |
| `internal/k8s/resources.go` | seed initContainer: `cp -a` -> `cp -a -n` |
| `internal/k8s/resources_test.go` | assert copy-if-missing |
| `internal/gateway/render.go` | render the `skills.workshop` block |
| `internal/gateway/render_test.go` | assert both workshop values |
| `deploy/openclaw-image.Dockerfile` | comment: the workspace seed is the tone file now |
| `test/e2e/framework/exec.go` | new: `Exec` over client-go `remotecommand` |
| `test/e2e/workspace_ownership_test.go` | new: drift converges, agent-authored skill survives |

---

### Task 1: `skill.TreeHash`

**Files:**
- Modify: `internal/skill/repository.go` (add below `ExtractTar`, which ends at the
  `default: // Skip symlinks / hardlinks / other special entries.` branch)
- Test: `internal/skill/repository_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `func TreeHash(dir, skip string) (string, error)` -- a sha256 hex over
  the directory tree rooted at `dir`, excluding the entry whose path relative to
  `dir` equals `skip` (`""` skips nothing). Entries are rendered
  `f\x00<rel>\x00<sha256 of content>` for a regular file, `d\x00<rel>` for a
  directory, and `o\x00<rel>` for anything else (symlink, fifo) -- `ExtractTar`
  skips those, so their appearance is drift the re-extract removes. The entry list
  is sorted before hashing, so the result does not depend on walk order.

- [ ] **Step 1: Write the failing test**

```go
func TestTreeHash(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("SKILL.md", "# a\n")
	write("refs/one.md", "one\n")
	write(".cubepilot.json", `{"tree":"whatever"}`)
	if err := os.MkdirAll(filepath.Join(dir, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	// hash is the current tree hash; a failure here is a test failure, not
	// something to compare against "".
	hash := func() string {
		t.Helper()
		got, err := TreeHash(dir, ".cubepilot.json")
		if err != nil {
			t.Fatalf("TreeHash: %v", err)
		}
		return got
	}
	base := hash()
	if again := hash(); again != base {
		t.Fatalf("TreeHash not stable: %q vs %q", base, again)
	}
	// The skipped entry is not part of the tree: rewriting it changes nothing.
	write(".cubepilot.json", `{"tree":"another"}`)
	if got := hash(); got != base {
		t.Error("the skipped file changed the tree hash")
	}
	// Each of these is drift, so each must change the hash. Each mutation is
	// compared against the hash taken immediately before it: comparing every step
	// against base would let an earlier mutation mask a later one that does nothing.
	prev := hash()
	write("SKILL.md", "# b\n")
	if got := hash(); got == prev {
		t.Error("a content change did not change the hash")
	}
	prev = hash()
	write("SKILL.md", "# a\n")
	write("extra.md", "x\n")
	if got := hash(); got == prev {
		t.Error("an added file did not change the hash")
	}
	prev = hash()
	if err := os.Remove(filepath.Join(dir, "extra.md")); err != nil {
		t.Fatal(err)
	}
	if got := hash(); got == prev {
		t.Error("a removed file did not change the hash")
	}
	prev = hash()
	if err := os.MkdirAll(filepath.Join(dir, "empty2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := hash(); got == prev {
		t.Error("an added empty directory did not change the hash")
	}
	prev = hash()
	if err := os.Remove(filepath.Join(dir, "refs", "one.md")); err != nil {
		t.Fatal(err)
	}
	if got := hash(); got == prev {
		t.Error("a removed nested file did not change the hash")
	}
}

func TestTreeHashMissingDir(t *testing.T) {
	if _, err := TreeHash(filepath.Join(t.TempDir(), "gone"), ".cubepilot.json"); err == nil {
		t.Fatal("a missing directory should error, not report a hash")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/skill/ -run TestTreeHash -v`
Expected: FAIL -- `undefined: TreeHash`

- [ ] **Step 3: Implement `TreeHash`**

Add to `internal/skill/repository.go` (`io/fs`, `crypto/sha256`, `encoding/hex`,
`fmt`, `io`, `os` and `path/filepath` are already imported there; **`sort` is not --
add it to the import block**):

```go
// TreeHash returns a sha256 over the directory tree rooted at dir, skipping the
// entry whose path relative to dir equals skip ("" skips nothing). It answers
// "is the content on disk the content we installed": the supervisor records the
// tree of a freshly extracted skill and compares the tree it finds later.
//
// The rendering is a fixed line per entry rather than the file bytes alone, so
// adding an empty directory or a symlink counts as a difference too. File modes
// are deliberately not part of an entry: they cannot change what a text skill
// instructs, and including them invites churn that re-extracts identical content.
func TreeHash(dir, skip string) (string, error) {
	var entries []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." || rel == skip {
			return nil
		}
		switch {
		case d.IsDir():
			entries = append(entries, "d\x00"+rel)
		case d.Type().IsRegular():
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(b)
			entries = append(entries, "f\x00"+rel+"\x00"+hex.EncodeToString(sum[:]))
		default:
			// ExtractTar skips symlinks and other special entries, so one
			// appearing here is drift that the re-extract removes.
			entries = append(entries, "o\x00"+rel)
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("hash tree %s: %w", dir, err)
	}
	sort.Strings(entries)
	h := sha256.New()
	for _, e := range entries {
		// One line per entry, so a path can never merge with the next entry's
		// rendering (Write on a hash.Hash has no error to check).
		h.Write([]byte(e + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/skill/ -v`
Expected: PASS (both new tests and the existing package tests)

- [ ] **Step 5: Commit**

```bash
git add internal/skill/repository.go internal/skill/repository_test.go
git commit -s -m "feat(skill): hash an installed skill directory tree (issue #238)"
```

---

### Task 2: Verify every installed skill on every poll

**Files:**
- Modify: `internal/supervisor/supervisor.go` (`Supervisor` struct, `New`,
  `applyConfig` at 463-477, `syncSkills` at 694-720, `syncSkill` at 726-790)
- Test: `internal/supervisor/supervisor_test.go` (`TestSyncSkills` at 78,
  `TestSyncSkillPreservesOldOnFailure` at 155, `TestPollSyncsOnChange` at 220, and
  a new harness helper)

**Interfaces:**
- Consumes: `skill.TreeHash(dir, skip string) (string, error)` from Task 1.
- Produces: `syncSkill` no longer consults the marker for its decision;
  `applyConfig` runs the verification on every poll and returns only whether the
  resolved *revision* changed (its caller uses that for one log line).

- [ ] **Step 1: Rewrite the marker-reading tests to the new marker**

In `internal/supervisor/supervisor_test.go`, replace the marker assertion in
`TestSyncSkills` (the block reading `.sha256`) with:

```go
	raw, err := os.ReadFile(filepath.Join(ws, "skills", "cluster-inspection", skillMarker))
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	var marker skillMarkerFile
	if err := json.Unmarshal(raw, &marker); err != nil {
		t.Fatalf("decode marker: %v", err)
	}
	if marker.Skill != "cluster-inspection" || marker.Revision != "rev1" || len(marker.Tree) != 64 {
		t.Errorf("marker = %+v; want skill cluster-inspection, revision rev1, 64-char tree", marker)
	}
```

In `TestSyncSkillPreservesOldOnFailure`, drop the `.sha256` write (the directory
just has to pre-exist) and keep everything else.

In `TestPollSyncsOnChange`, replace the marker assertion with a decode of
`skillMarkerFile` expecting `Revision == "r2"`.

- [ ] **Step 2: Add the new failing tests**

```go
// TestSyncSkillRestoresDrift verifies an edit to an installed platform skill is
// corrected on the next sync, and that a clean tree is not re-fetched.
func TestSyncSkillRestoresDrift(t *testing.T) {
	ws := t.TempDir()
	repo := &skill.PathRepository{Root: t.TempDir()}
	sha := seedTar(t, repo, "cluster-inspection/v1.tar.gz", "# Inspection\n\nOriginal content.")
	srv, requests := testAPIWithCounter(t, nil, "", repo.Root)
	s := New(Config{Workspace: ws, APIURL: srv.URL})
	s.http = srv.Client()
	cfg := &resolver.ResolvedAgentConfig{Revision: "rev1", Skills: []resolver.ResolvedSkill{
		{Name: "cluster-inspection", Path: "cluster-inspection/v1.tar.gz", Sha256: sha, Revision: "rev1"},
	}}
	if err := s.syncSkills(context.Background(), cfg); err != nil {
		t.Fatalf("install: %v", err)
	}
	if got := requests(); got != 1 {
		t.Fatalf("install fetches = %d, want 1", got)
	}
	// A clean tree verifies without fetching.
	if err := s.syncSkills(context.Background(), cfg); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got := requests(); got != 1 {
		t.Errorf("a clean tree re-fetched (fetches = %d, want 1)", got)
	}
	// Drift: the agent edits the installed SKILL.md.
	body := filepath.Join(ws, "skills", "cluster-inspection", "SKILL.md")
	if err := os.WriteFile(body, []byte("# Inspection\n\nTampered."), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.syncSkills(context.Background(), cfg); err != nil {
		t.Fatalf("heal: %v", err)
	}
	if got := requests(); got != 2 {
		t.Errorf("drift did not re-fetch (fetches = %d, want 2)", got)
	}
	restored, err := os.ReadFile(body)
	if err != nil || !strings.Contains(string(restored), "Original content.") {
		t.Errorf("drift not corrected: %q, %v", restored, err)
	}
}

// TestSyncSkillDetectsForgedMarker verifies the marker cannot be used to make
// drift permanent: an agent that rewrites the skill and recomputes the marker's
// tree, leaving the revision alone, is still corrected.
func TestSyncSkillDetectsForgedMarker(t *testing.T) {
	ws := t.TempDir()
	repo := &skill.PathRepository{Root: t.TempDir()}
	sha := seedTar(t, repo, "cluster-inspection/v1.tar.gz", "# Inspection\n\nOriginal content.")
	srv, _ := testAPIWithCounter(t, nil, "", repo.Root)
	s := New(Config{Workspace: ws, APIURL: srv.URL})
	s.http = srv.Client()
	cfg := &resolver.ResolvedAgentConfig{Revision: "rev1", Skills: []resolver.ResolvedSkill{
		{Name: "cluster-inspection", Path: "cluster-inspection/v1.tar.gz", Sha256: sha, Revision: "rev1"},
	}}
	if err := s.syncSkills(context.Background(), cfg); err != nil {
		t.Fatalf("install: %v", err)
	}
	dir := filepath.Join(ws, "skills", "cluster-inspection")
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("# Inspection\n\nTampered."), 0o644); err != nil {
		t.Fatal(err)
	}
	forged, err := skill.TreeHash(dir, skillMarker)
	if err != nil {
		t.Fatal(err)
	}
	forgedMarker, err := json.Marshal(skillMarkerFile{Skill: "cluster-inspection", Revision: "rev1", Tree: forged})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, skillMarker), forgedMarker, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.syncSkills(context.Background(), cfg); err != nil {
		t.Fatalf("heal: %v", err)
	}
	restored, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil || !strings.Contains(string(restored), "Original content.") {
		t.Errorf("forged marker defeated the check: %q, %v", restored, err)
	}
}

// TestSyncSkillRefetchesOnNewRevision verifies new platform content lands even
// though the installed tree matched the previous expectation.
func TestSyncSkillRefetchesOnNewRevision(t *testing.T) {
	ws := t.TempDir()
	repo := &skill.PathRepository{Root: t.TempDir()}
	sha1 := seedTar(t, repo, "cluster-inspection/v1.tar.gz", "# Inspection\n\nVersion 1.")
	srv, _ := testAPIWithCounter(t, nil, "", repo.Root)
	s := New(Config{Workspace: ws, APIURL: srv.URL})
	s.http = srv.Client()
	cfg1 := &resolver.ResolvedAgentConfig{Revision: "rev-1", Skills: []resolver.ResolvedSkill{
		{Name: "cluster-inspection", Path: "cluster-inspection/v1.tar.gz", Sha256: sha1, Revision: "r1"},
	}}
	if err := s.syncSkills(context.Background(), cfg1); err != nil {
		t.Fatalf("install: %v", err)
	}
	// The platform publishes v2 at the same path; the resolved identity changes.
	sha2 := seedTar(t, repo, "cluster-inspection/v1.tar.gz", "# Inspection\n\nVersion 2.")
	cfg2 := &resolver.ResolvedAgentConfig{Revision: "rev-2", Skills: []resolver.ResolvedSkill{
		{Name: "cluster-inspection", Path: "cluster-inspection/v1.tar.gz", Sha256: sha2, Revision: "r2"},
	}}
	if err := s.syncSkills(context.Background(), cfg2); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(ws, "skills", "cluster-inspection", "SKILL.md"))
	if err != nil || !strings.Contains(string(got), "Version 2.") {
		t.Errorf("new revision not installed: %q, %v", got, err)
	}
}

// TestSyncSkillsVerifiesAtPodStart verifies a fresh supervisor process does not
// trust the marker it finds on the PVC: it re-fetches and re-verifies.
func TestSyncSkillsVerifiesAtPodStart(t *testing.T) {
	ws := t.TempDir()
	repo := &skill.PathRepository{Root: t.TempDir()}
	sha := seedTar(t, repo, "cluster-inspection/v1.tar.gz", "# Inspection\n\nOriginal content.")
	srv, requests := testAPIWithCounter(t, nil, "", repo.Root)
	cfg := &resolver.ResolvedAgentConfig{Revision: "rev1", Skills: []resolver.ResolvedSkill{
		{Name: "cluster-inspection", Path: "cluster-inspection/v1.tar.gz", Sha256: sha, Revision: "rev1"},
	}}
	first := New(Config{Workspace: ws, APIURL: srv.URL})
	first.http = srv.Client()
	if err := first.syncSkills(context.Background(), cfg); err != nil {
		t.Fatalf("install: %v", err)
	}
	// A new process: same workspace, empty expectations.
	second := New(Config{Workspace: ws, APIURL: srv.URL})
	second.http = srv.Client()
	if err := second.syncSkills(context.Background(), cfg); err != nil {
		t.Fatalf("pod start: %v", err)
	}
	if got := requests(); got != 2 {
		t.Errorf("a pod start did not re-fetch (fetches = %d, want 2)", got)
	}
}
```

Add the counting harness next to `testAPI`:

```go
// testAPI is testAPIWithCounter without the counter, for tests that do not care.
func testAPI(t *testing.T, cfg *resolver.ResolvedAgentConfig, user, skillsDir string) *httptest.Server {
	t.Helper()
	srv, _ := testAPIWithCounter(t, cfg, user, skillsDir)
	return srv
}

// testAPIWithCounter serves the same routes as before plus a count of
// skill-tar requests, so a test can assert the verification path did (or did
// not) go back to the API.
func testAPIWithCounter(t *testing.T, cfg *resolver.ResolvedAgentConfig, user, skillsDir string) (*httptest.Server, func() int) {
	t.Helper()
	var n int64
	mux := http.NewServeMux()
	mux.HandleFunc("/internal/agents/"+user+"/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"config": cfg}); err != nil {
			t.Errorf("encode: %v", err)
		}
	})
	mux.HandleFunc("/internal/skills/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&n, 1)
		rest := strings.TrimPrefix(r.URL.Path, "/internal/skills/")
		name, tail, ok := strings.Cut(rest, "/")
		if !ok || tail != "tar" || name == "" {
			http.NotFound(w, r)
			return
		}
		rc, err := (&skill.PathRepository{Root: skillsDir}).Open(r.Context(), name+"/v1.tar.gz")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer rc.Close()
		w.Header().Set("Content-Type", "application/gzip")
		if _, err := io.Copy(w, rc); err != nil {
			t.Errorf("copy tar: %v", err)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, func() int { return int(atomic.LoadInt64(&n)) }
}
```

Add `"sync/atomic"` to the test file's imports. Note `seedTar` overwrites an
existing repository path, so the v2 case above works without a second route.

- [ ] **Step 3: Run the new tests to verify they fail**

Run: `go test ./internal/supervisor/ -run 'TestSyncSkill|TestSyncSkills|TestPollSyncsOnChange' -v`
Expected: FAIL -- `undefined: skillMarker`, `undefined: skillMarkerFile`,
`undefined: testAPIWithCounter` (the harness compiles once the step-5 code lands;
run it now to see the missing symbols rather than a wrong assertion)

- [ ] **Step 4: Implement the marker, the expectations and the verification**

Add to `internal/supervisor/supervisor.go`, above `Supervisor`:

```go
// skillMarker is the file the supervisor writes into every skill directory it
// renders. Its presence is what marks a directory as the platform's own (cleanup
// removes only marked directories), and it records which revision was installed
// and what that content hashed to. It is deliberately not the authority for drift
// detection: it is writable by the agent, so a value read back from it is a claim
// by whoever could write it. The authority is the tree this process derived from
// the platform's tarball.
const skillMarker = ".cubepilot.json"

// skillMarkerFile is the marker's on-disk shape.
type skillMarkerFile struct {
	Skill    string `json:"skill"`
	Revision string `json:"revision"`
	Tree     string `json:"tree"`
}

// skillExpectation is what this process last installed for one skill: the
// resolved identity it came from and the tree hash of the content it wrote.
type skillExpectation struct {
	identity string
	tree     string
}

// skillIdentity is the resolved artifact identity a skill's content came from.
// The expectation is keyed by it so that a new platform revision -- or the same
// revision served under a different path or digest -- re-fetches instead of
// matching the previous content for as long as the process lives.
func skillIdentity(rs resolver.ResolvedSkill) string {
	return rs.Revision + "\x00" + rs.Path + "\x00" + rs.Sha256
}
```

Add the field to the `Supervisor` struct (next to `current`):

```go
	// expectations records what this process installed for each skill, keyed by
	// name. Only touched by the poll goroutine (syncSkills runs under s.mu via
	// applyConfig) and by tests.
	expectations map[string]skillExpectation
```

Initialize it in `New`:

```go
func New(cfg Config) *Supervisor {
	return &Supervisor{
		cfg:          cfg,
		http:         &http.Client{Timeout: 15 * time.Second},
		expectations: map[string]skillExpectation{},
	}
}
```

Replace `applyConfig` (the early return on an unchanged revision is what used to
skip the verification):

```go
// applyConfig verifies the installed skills against the resolved config on every
// poll -- drift does not announce itself through a revision change -- and records
// the revision. The returned bool still means "the resolved revision changed",
// which is what the caller logs.
func (s *Supervisor) applyConfig(ctx context.Context, cfg *resolver.ResolvedAgentConfig) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := s.current != cfg.Revision
	if changed {
		log.Printf("supervisor: config revision %s -> %s", revisionLabel(s.current), cfg.Revision)
		s.current = cfg.Revision
	}
	if err := s.syncSkills(ctx, cfg); err != nil {
		return changed, fmt.Errorf("sync skills: %w", err)
	}
	return changed, nil
}
```

Replace `syncSkill` with a fetch helper plus the verified installer:

```go
// fetchSkillTar pulls a skill's tar from the internal API.
func (s *Supervisor) fetchSkillTar(ctx context.Context, name string) ([]byte, error) {
	u := fmt.Sprintf("%s/internal/skills/%s/tar", strings.TrimRight(s.cfg.APIURL, "/"), url.PathEscape(name))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: %d", u, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// syncSkill installs one skill and verifies it on every later poll. The tree it
// compares against is the one this process computed from the platform's tarball;
// the marker on disk is never consulted for the decision, because it is writable
// by the agent and a forged one would make drift permanent.
//
// A skill with no stored expectation -- every wanted skill after a pod start -- is
// fetched rather than trusted to its marker, so verification always starts from
// platform content. The expectation is keyed by the resolved identity, not by
// name: keyed by name alone, a new revision would match the old content forever.
func (s *Supervisor) syncSkill(ctx context.Context, rs resolver.ResolvedSkill, skillsDir string) error {
	dir := filepath.Join(skillsDir, rs.Name)
	identity := skillIdentity(rs)
	if want, ok := s.expectations[rs.Name]; ok && want.identity == identity {
		got, err := skill.TreeHash(dir, skillMarker)
		switch {
		case err != nil:
			// Unreadable -- most often the directory is gone. That is drift like
			// any other, so reinstall rather than propagate: a skill that left
			// the resolved set and returned still lands.
			log.Printf("supervisor: skill %s unreadable (%v); reinstalling", rs.Name, err)
		case got == want.tree:
			return nil // verified -- no fetch
		default:
			log.Printf("supervisor: skill %s content drifted; reinstalling", rs.Name)
		}
	}
	tarBytes, err := s.fetchSkillTar(ctx, rs.Name)
	if err != nil {
		return err
	}
	if rs.Sha256 != "" {
		h := sha256.Sum256(tarBytes)
		if got := hex.EncodeToString(h[:]); got != rs.Sha256 {
			return fmt.Errorf("skill %s: sha256 mismatch (%s != %s)", rs.Name, got, rs.Sha256)
		}
	}
	// Stage into a temp dir first; the installed dir stays untouched.
	tmpDir, err := os.MkdirTemp(skillsDir, "."+rs.Name+".tmp-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	if err := skill.ExtractTar(bytes.NewReader(tarBytes), tmpDir); err != nil {
		return err
	}
	// Hash the staged content, then record it in the marker. The marker itself is
	// skipped by TreeHash, so the hash stays true after the marker lands -- which
	// is what makes the post-swap directory match the expectation exactly.
	tree, err := skill.TreeHash(tmpDir, skillMarker)
	if err != nil {
		return err
	}
	body, err := json.Marshal(skillMarkerFile{Skill: rs.Name, Revision: rs.Revision, Tree: tree})
	if err != nil {
		return err
	}
	// Written into the staged dir, so a marker failure aborts before the swap and
	// the installed skill stays untouched.
	if err := os.WriteFile(filepath.Join(tmpDir, skillMarker), body, 0o644); err != nil {
		return err
	}
	// Swap: move the current dir aside, bring the staged dir in, drop the backup
	// only after the swap succeeds.
	backup := dir + ".backup"
	if _, err := os.Lstat(dir); err == nil {
		if err := os.Rename(dir, backup); err != nil {
			return err
		}
	}
	if err := os.Rename(tmpDir, dir); err != nil {
		_ = os.Rename(backup, dir) // best-effort restore
		return err
	}
	if err := os.RemoveAll(backup); err != nil {
		return err
	}
	s.expectations[rs.Name] = skillExpectation{identity: identity, tree: tree}
	log.Printf("supervisor: skill %s/%s installed (tree %.12s)", rs.Name, rs.Revision, tree)
	return nil
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/supervisor/ -v`
Expected: PASS. `TestSyncSkills` now asserts the `.cubepilot.json` marker; the
"stale" directory it pre-creates is still unmarked, so the cleanup in `syncSkills`
(unchanged in this task) still removes it.

- [ ] **Step 6: Commit**

```bash
git add internal/supervisor/supervisor.go internal/supervisor/supervisor_test.go
git commit -s -m "fix(supervisor): verify installed skills against platform content every poll (issue #238)"
```

---

### Task 3: Cleanup stops deleting what the agent authored

**Files:**
- Modify: `internal/supervisor/supervisor.go` (`syncSkills`, the `entries` loop)
- Test: `internal/supervisor/supervisor_test.go`

**Interfaces:**
- Consumes: `skillMarker` from Task 2.
- Produces: `syncSkills` removes only directories carrying `skillMarker`.

- [ ] **Step 1: Update the existing cleanup test and add the new one**

In `TestSyncSkills`, give the pre-existing stale directory a marker so it stays a
cleanup case:

```go
	// Pre-existing stale skill dir (platform-rendered: it carries the marker) that
	// must be cleared once its name leaves the resolved set.
	staleDir := filepath.Join(ws, "skills", "stale")
	if err := os.MkdirAll(staleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staleDir, "SKILL.md"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staleDir, skillMarker),
		[]byte(`{"skill":"stale","revision":"r0","tree":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
```

Add:

```go
// TestSyncSkillsKeepsAgentAuthoredSkill verifies a skill directory the platform
// did not render is left alone: no marker, no ownership, no deletion.
func TestSyncSkillsKeepsAgentAuthoredSkill(t *testing.T) {
	ws := t.TempDir()
	repo := &skill.PathRepository{Root: t.TempDir()}
	sha := seedTar(t, repo, "cluster-inspection/v1.tar.gz", "# Inspection\n")
	srv, _ := testAPIWithCounter(t, nil, "", repo.Root)
	s := New(Config{Workspace: ws, APIURL: srv.URL})
	s.http = srv.Client()
	// The agent's own skill, authored through skill_workshop: no marker.
	own := filepath.Join(ws, "skills", "my-own-skill")
	if err := os.MkdirAll(own, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(own, "SKILL.md"), []byte("# Mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &resolver.ResolvedAgentConfig{Revision: "rev1", Skills: []resolver.ResolvedSkill{
		{Name: "cluster-inspection", Path: "cluster-inspection/v1.tar.gz", Sha256: sha, Revision: "rev1"},
	}}
	if err := s.syncSkills(context.Background(), cfg); err != nil {
		t.Fatalf("syncSkills: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(own, "SKILL.md")); err != nil || string(b) != "# Mine\n" {
		t.Errorf("agent-authored skill was touched: %q, %v", b, err)
	}
	// A later revision change (the cleanup runs on every sync) must still leave it.
	cfg.Revision = "rev2"
	cfg.Skills[0].Revision = "rev2"
	if err := s.syncSkills(context.Background(), cfg); err != nil {
		t.Fatalf("syncSkills after revision change: %v", err)
	}
	if _, err := os.Stat(own); err != nil {
		t.Errorf("agent-authored skill removed on a revision change: %v", err)
	}
}
```

- [ ] **Step 2: Run to verify the new test fails**

Run: `go test ./internal/supervisor/ -run 'TestSyncSkillsKeepsAgentAuthoredSkill' -v`
Expected: FAIL -- "agent-authored skill was touched", because the current cleanup
removes every entry that is not in the resolved set.

- [ ] **Step 3: Implement ownership-aware cleanup**

In `syncSkills`, replace the removal loop with:

```go
	for _, e := range entries {
		if _, ok := wanted[e.Name()]; ok {
			continue
		}
		// Ownership decides: a directory carrying our marker is one the platform
		// rendered, so it is ours to remove once its name leaves the resolved set.
		// Anything else is the agent's own skill and stays.
		//
		// The shared name is the accepted edge: a platform name that leaves the
		// set and is re-created by the agent before this pass runs is removed as a
		// platform one. The window is one poll.
		if _, err := os.Stat(filepath.Join(skillsDir, e.Name(), skillMarker)); err != nil {
			continue
		}
		if err := os.RemoveAll(filepath.Join(skillsDir, e.Name())); err != nil {
			return err
		}
	}
```

Update the `syncSkills` doc comment to state the rule (the current one says
"clearing stale dirs first"):

```go
// syncSkills pulls the enabled skills' tars from the internal API, installs them
// under Workspace/skills/<name>/, verifies the installed content on every call,
// and removes the skill directories it rendered that are no longer in the
// resolved set. Directories it did not render (no marker) are the agent's and are
// left untouched.
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/supervisor/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/supervisor/supervisor.go internal/supervisor/supervisor_test.go
git commit -s -m "fix(supervisor): only remove skill directories the platform rendered (issue #238)"
```

---

### Task 4: `openclaw.json` is compared against the file, not against our own memory

**Files:**
- Modify: `internal/supervisor/supervisor.go` (`Supervisor.lastCfgHash` at 145-147,
  `applyGatewayConfig` at 404-425)
- Test: `internal/supervisor/supervisor_test.go` (`TestApplyGatewayConfig` at 262)

**Interfaces:**
- Consumes: nothing.
- Produces: `applyGatewayConfig([]byte) (bool, error)` with the same signature and
  the same meaning for the bool ("something was written").

- [ ] **Step 1: Add the failing drift tests**

Append to `TestApplyGatewayConfig`:

```go
	// A hand-edited file is rewritten: the comparison is against the bytes on
	// disk, so a write the supervisor did not make is visible.
	if err := os.WriteFile(path, []byte(`{"models":{"providers":{"rogue":{}}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	want := `{"models":{"providers":{"my-glm":{"api":"openai-completions"}}}}`
	changed, err = s.applyGatewayConfig([]byte(want))
	if err != nil {
		t.Fatalf("apply after edit: %v", err)
	}
	if !changed {
		t.Error("a hand-edited file should be rewritten")
	}
	if got, _ = os.ReadFile(path); string(got) != want {
		t.Errorf("edited file not restored: %q, want %q", got, want)
	}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/supervisor/ -run TestApplyGatewayConfig -v`
Expected: FAIL -- "a hand-edited file should be rewritten", because the current
comparison is against `lastCfgHash` and sees no change.

- [ ] **Step 3: Implement the disk comparison**

Delete the `lastCfgHash` field and its comment from the `Supervisor` struct, and
replace `applyGatewayConfig`:

```go
// applyGatewayConfig writes the gateway config to ConfigPath when the bytes on
// disk differ from the desired bytes, and reports whether it wrote. Comparing
// against the file -- not against the hash of what this process last wrote -- is
// what makes a write nobody asked for visible: the file is on the PVC, the
// gateway watches it, and a comparison against in-memory state would never notice
// it changed.
func (s *Supervisor) applyGatewayConfig(data []byte) (bool, error) {
	if s.cfg.ConfigPath == "" {
		return false, nil
	}
	current, err := os.ReadFile(s.cfg.ConfigPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("read %s: %w", s.cfg.ConfigPath, err)
	}
	if bytes.Equal(current, data) {
		return false, nil // already the desired bytes
	}
	if err := os.MkdirAll(filepath.Dir(s.cfg.ConfigPath), 0o755); err != nil {
		return false, err
	}
	if err := os.WriteFile(s.cfg.ConfigPath, data, 0o644); err != nil {
		return false, err
	}
	log.Printf("supervisor: gateway config written (%d bytes)", len(data))
	return true, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/supervisor/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/supervisor/supervisor.go internal/supervisor/supervisor_test.go
git commit -s -m "fix(supervisor): compare openclaw.json against the file on disk (issue #238)"
```

---

### Task 5: One managed block carrying the persona and the instructions

**Files:**
- Create: `internal/supervisor/persona.md`
- Delete: `workspace/AGENTS.md`
- Modify: `workspace/SOUL.md`, `internal/supervisor/supervisor.go` (`syncInstructions`
  at 489-527, `reconcileInstructions` at 593-617, the `systemPrompt*` consts at 115-131)
- Test: `internal/supervisor/supervisor_test.go` (`TestReconcileInstructions` at 434,
  `TestSyncInstructions` at 518)

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `managedBlockBody(instructions string) string` (persona + optional
  instructions section); `reconcileManagedBlock(current []byte, desired string) []byte`
  (the renamed `reconcileInstructions`); `syncAgentsFile(cfg *resolver.ResolvedAgentConfig) error`
  (the renamed `syncInstructions`).

- [ ] **Step 1: Write `internal/supervisor/persona.md`**

This is the image's `workspace/AGENTS.md` (moved, verbatim), with the SOUL.md
operational bullets folded into 执行原则 (they restated the same rules; the merged
text states each rule once) and the new ownership section appended. Its first
section and everything up to `## 输出` is unchanged text from that file:

```markdown
# CubePilot 操作约定

你是 CubePilot，CubeStack 智算云平台的智能助手，运行在 OpenClaw 运行时中。你的核心工具是 `exec`（执行 shell 命令），通过 `kubectl` 操作当前集群。

## 你的定位

- 你是用户的运维与操作伙伴：把用户的自然语言意图翻译成对平台能力的正确调用，并把结果用简洁的中文解释清楚。
- 你以用户身份通过 `exec` 执行 `kubectl` 操作平台资源；权限由集群 RBAC 强制，无权限时如实说明。
- 涉及资源时，给出明确的资源名与命名空间，避免含糊其辞。

## 能力目录（Skills）

平台能力以 Skills 形式注入，见 `skills/` 目录。当你需要操作平台资源时，先查阅对应 Skill 了解该能力的用途与调用方式，再据此构造 `kubectl` 命令。主要能力：

- `kubectl-platform`：集群资源（节点/Pod/命名空间/事件）的查询与操作，以及通用 CRD 的 schema 发现。
- `cluster-inspection`：集群健康巡检清单与异常分级。
- `cubestack-platform`：CubeStack 平台资源（`ai.cubestack.io` 组）的 schema 速查与使用指南——含 `crd-reference.md` 生成的各 CR 必填/默认/枚举，及已知可用的 DevEnvironment 清单。

## 工作区所有权

工作区里哪些东西归你、哪些归平台，规则很简单：

- `skills/` 下**由平台注入的 skill**（`kubectl-platform`、`cluster-inspection`、`cubestack-platform` 等）是平台的内容：**不要修改它们的文件**。改了平台会在很短时间内改回，你看到的"修改成功"不会保留。
- 平台 skill 不对或不够用时，如实告诉用户，并建议用户在平台上重新发布该 skill；不要自己动手改。
- **你可以有自己的 skill**：需要把一套做法沉淀下来时用 `skill_workshop` 工具，先 `create` 再 `apply` 完成落地——不要只创建提案就停下，提案需要有人审核才能生效，而当前没有这个界面。
- `AGENTS.md` 中标记块之外的内容、`SOUL.md`、以及你自己新建的 skill 目录都属于你，平台不会删改。需要长期记住的东西可以写在这些地方。

## 执行原则

1. **先查后答**：涉及集群状态的问题，先执行 `kubectl` 拿到真实数据再回答，不要凭猜测。
2. **只读直放，写操作谨慎**：只读查询（get/list/describe/logs）直接执行；写操作（apply/create/delete/scale）执行前，在回复中说明动作与影响范围；无权限时如实说明并被 RBAC 拒绝。
3. **证据链**：给出结论时附带你执行的命令与关键输出，便于用户复核。
4. **命名空间**：默认操作 `default` 命名空间；用户指定 `project`/命名空间时以用户为准；全局查询用 `-A` 或 `--all-namespaces`。
5. **异常归因**：命令报错时，区分权限不足 / 资源不存在 / 超时 / 集群异常，并给出可执行的下一步。
6. **平台 CRD 先查 `cubestack-platform`**：操作 `ai.cubestack.io` 组 CRD（如 DevEnvironment / InferenceService）前，先查阅 `cubestack-platform` skill（`crd-reference.md` 的 schema 速查与已知可用清单），不要从零 `dry-run` 猜字段。仅当该 kind 不在其速查范围内时，才回退到 `kubectl-platform` 的通用发现流程（`api-resources` → `explain` / `--dry-run=server` → apply）。
7. **双身份边界**：默认 `kubectl` 走**用户自己的凭证**（`~/.kube/config`，RBAC 是最终闸门）；`$CUBEPILOT_PLATFORM_KUBECONFIG` 只用于 schema 发现，不得用它执行真实业务操作或绕过用户 RBAC。
8. **只读路径规范**：只读 shell 命令（ls/cat/grep/head/tail 等）用绝对路径（把 `~` 自行展开成完整路径）。通配符要区分：作为**参数模式**的（如 `grep` 正则里的 `*`/`?`/`[`）加引号；**文件路径中的 glob 不要加引号**——加引号会当字面量、匹配不到文件，而不加引号又无法被安全自动放行，所以路径匹配请改用显式路径或 `find <目录> -name '<模式>'`。参数里出现未加引号的 `~` 或通配符时，命中白名单的只读命令也无法被安全自动放行，会反复要求用户确认；按此规范写即可直接执行、无需确认。
9. **工具结果三分**：`exec`/`write` 的返回只可能是三类，分别对待：
   - **自动放行**：命中只读白名单（kubectl 读动词、只读 shell 工具），命令已执行，直接用结果。
   - **待人工批准**：界面出现带 id 的批准请求（`confirm_pending`）。命令形态可绑定（多为单条、文件操作数明确的写命令，如 `kubectl apply -f <工作区文件>`）。让用户在批准界面上批准/拒绝即可。
   - **系统级拒绝**（形如 `SYSTEM_RUN_DENIED: approval cannot safely bind this command` 或 `Path escapes sandbox root`）：命令**不可批准、没有批准 id**——内容经 heredoc/管道/重定向写入（绑不上），或路径超出工作区沙箱。**不要要求用户回复 `/approve`**（那只对真正的待批准请求有效），也不要假装它在等批准；如实说明该命令在你的沙箱/可授权范围外、无法授权执行，并给出可行替代：
     - 需写文件供随后读取/应用时，先用 `write` 工具把内容写入工作区（沙箱根目录），再用单条 `kubectl apply -f <工作区文件>` 应用；manifest 用**固定文件名并覆盖**——命名空间级资源 `manifests/<kind>_<namespace>_<name>.yaml`，集群级资源 `manifests/<kind>_<name>.yaml`；别每次换新名字，避免工作区里的文件越攒越多；
     - 确需写工作区之外的路径时，改用单条、参数明确的写命令（可进入待批准流程）。

## 输出

- 简体中文。
- 结构化：结论 → 证据（命令/输出摘要）→ 建议。
```

- [ ] **Step 2: Delete the image's copy and trim `SOUL.md`**

```bash
git rm workspace/AGENTS.md
```

Replace `workspace/SOUL.md` with the tone file (the identity and operational
bullets moved into the persona file above; what remains is the voice):

```markdown
# CubePilot

你是 CubePilot，CubeStack 智算云平台的智能助手。

## 语气

- 简体中文，专业、克制、直接。
- 结论先行，再给证据与建议。
- 不确定时明确说不确定，不臆造集群状态。
```

- [ ] **Step 3: Rewrite the pure-function tests**

In `supervisor_test.go`, replace `TestReconcileInstructions` and
`TestSyncInstructions` with versions against the new shape. The persona is no
longer the file's prefix -- it is inside the block -- so the assertions become:

```go
// TestManagedBlockBody covers the composed platform text: the persona always, the
// instructions section only when there is one.
func TestManagedBlockBody(t *testing.T) {
	body := managedBlockBody("")
	if !strings.Contains(body, "CubePilot 操作约定") || strings.Contains(body, systemPromptHeader) {
		t.Errorf("empty instructions should yield the persona alone:\n%s", body)
	}
	body = managedBlockBody("Answer in Chinese.")
	if !strings.Contains(body, "CubePilot 操作约定") ||
		!strings.Contains(body, systemPromptHeader) ||
		!strings.Contains(body, "Answer in Chinese.") {
		t.Errorf("persona and instructions should both be present:\n%s", body)
	}
}

// TestReconcileManagedBlock covers the block rewrite: written on first sync,
// replaced in place, and agent-authored content outside it preserved verbatim.
func TestReconcileManagedBlock(t *testing.T) {
	t.Run("writes the block into an empty file", func(t *testing.T) {
		got := string(reconcileManagedBlock(nil, "persona text"))
		if !strings.Contains(got, systemPromptStart) || !strings.Contains(got, "persona text") {
			t.Fatalf("block missing:\n%s", got)
		}
	})
	t.Run("replaces the block and keeps content around it", func(t *testing.T) {
		agentNotes := "## 我的笔记\n\n记住昨天那个 Pod 的问题。\n"
		first := reconcileManagedBlock([]byte(agentNotes), "old")
		second := string(reconcileManagedBlock(first, "new"))
		if strings.Contains(second, "old") || !strings.Contains(second, "new") {
			t.Errorf("block not replaced:\n%s", second)
		}
		if !strings.Contains(second, agentNotes) {
			t.Errorf("agent content lost:\n%s", second)
		}
	})
	t.Run("tolerates an unterminated block", func(t *testing.T) {
		broken := systemPromptStart + "\nleft over\n"
		got := string(reconcileManagedBlock([]byte(broken), "fresh"))
		if strings.Count(got, systemPromptStart) != 1 || !strings.Contains(got, "fresh") {
			t.Errorf("unterminated block not repaired:\n%s", got)
		}
	})
}

// TestSyncAgentsFile verifies the file is written from the persona and the
// resolved instructions, that an unchanged file is left alone, and that a
// rejected instruction set keeps the last-good file instead of replacing the
// persona with a broken block.
func TestSyncAgentsFile(t *testing.T) {
	ws := t.TempDir()
	s := New(Config{Workspace: ws})
	agentsPath := filepath.Join(ws, agentsFileName)

	if err := s.syncAgentsFile(&resolver.ResolvedAgentConfig{Instructions: "Answer in Chinese."}); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	raw, err := os.ReadFile(agentsPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(raw), "CubePilot 操作约定") || !strings.Contains(string(raw), "Answer in Chinese.") {
		t.Fatalf("persona and instructions not written:\n%s", raw)
	}
	// Agent content outside the block survives a resync.
	appended := string(raw) + "\n## 我的笔记\n\nkeep me\n"
	if err := os.WriteFile(agentsPath, []byte(appended), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.syncAgentsFile(&resolver.ResolvedAgentConfig{Instructions: "Answer in Chinese."}); err != nil {
		t.Fatalf("resync: %v", err)
	}
	after, _ := os.ReadFile(agentsPath)
	if !strings.Contains(string(after), "keep me") {
		t.Errorf("agent content outside the block was lost:\n%s", after)
	}
	// A hand-edited block is restored.
	if err := os.WriteFile(agentsPath, []byte(systemPromptStart+"\nrogue\n"+systemPromptEnd), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.syncAgentsFile(&resolver.ResolvedAgentConfig{Instructions: "Answer in Chinese."}); err != nil {
		t.Fatalf("heal: %v", err)
	}
	healed, _ := os.ReadFile(agentsPath)
	if strings.Contains(string(healed), "rogue") || !strings.Contains(string(healed), "CubePilot 操作约定") {
		t.Errorf("edited block not restored:\n%s", healed)
	}
	// An oversized instruction set is skipped (last-good file kept).
	before := healed
	oversized := &resolver.ResolvedAgentConfig{Instructions: strings.Repeat("x", instructions.MaxChars+1)}
	if err := s.syncAgentsFile(oversized); err != nil {
		t.Fatalf("oversized: %v", err)
	}
	kept, _ := os.ReadFile(agentsPath)
	if !bytes.Equal(kept, before) {
		t.Error("an oversized instruction set should leave the last-good file in place")
	}
}
```

- [ ] **Step 4: Run to verify they fail**

Run: `go test ./internal/supervisor/ -run 'TestManagedBlockBody|TestReconcileManagedBlock|TestSyncAgentsFile' -v`
Expected: FAIL -- `undefined: managedBlockBody`, `undefined: reconcileManagedBlock`,
`undefined: syncAgentsFile`

- [ ] **Step 5: Implement the embed and the block**

In `internal/supervisor/supervisor.go`, add next to the other consts:

```go
// personaText is the platform's operating conventions for every agent: the
// unchangeable part of the managed block. It lives in this binary rather than in
// the image's workspace seed, because the block is the only place the platform
// owns: a copy seeded into the workspace would also sit outside the block, and
// the same text would then appear twice in AGENTS.md.
//
//go:embed persona.md
var personaText string
```

(`_ "embed"` must be added to the import block: a `//go:embed` of a `string`
needs the blank import. The local below is named `text`, not `instructions`,
so it does not shadow the `instructions` package this function calls.)

Add the composer, and rename `syncInstructions`/`reconcileInstructions`:

```go
// managedBlockBody composes the platform-owned text of the AGENTS.md managed
// block: the operating conventions followed by the resolved instructions (the
// template's and the user's). Both are platform-rendered, so both converge.
func managedBlockBody(instructions string) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(personaText))
	if s := strings.TrimSpace(instructions); s != "" {
		b.WriteString("\n\n" + systemPromptHeader + "\n\n" + s)
	}
	return b.String()
}

// syncAgentsFile reconciles the marker-guarded managed block of the workspace
// AGENTS.md with the platform's desired text (persona + instructions). The block
// is (re)written whenever the on-disk content differs and everything outside the
// markers is preserved verbatim, so the agent's own notes in that file survive.
// Idempotent and content-hash guarded: an unchanged file is left untouched. A
// rejected instruction set is skipped (keep the last-good file) rather than
// corrupting the persona.
func (s *Supervisor) syncAgentsFile(cfg *resolver.ResolvedAgentConfig) error {
	path := filepath.Join(s.cfg.Workspace, agentsFileName)
	text := ""
	if cfg != nil {
		text = strings.TrimSpace(cfg.Instructions)
	}
	desired := managedBlockBody(text)
	// Validate what is actually written. The block carries the persona too, so the
	// budget that matters is the whole block's: OpenClaw truncates an oversized
	// AGENTS.md, and a truncated file would take the operating conventions with it.
	if err := instructions.Validate(desired); err != nil {
		log.Printf("supervisor: %v; skipping AGENTS.md sync", err)
		return nil
	}
	current, err := readNoFollow(path)
	if err != nil {
		return err
	}
	target := reconcileManagedBlock(current, desired)
	if bytes.Equal(target, current) {
		return nil // no change (or file absent + nothing to write)
	}
	if err := writeTempAndRename(path, target); err != nil {
		return err
	}
	log.Printf("supervisor: AGENTS.md managed block synced (%d bytes)", len(target))
	return nil
}
```

`reconcileManagedBlock` is `reconcileInstructions` unchanged except its name and
its doc comment's subject ("the managed instructions block" -> "the managed block
whose desired body is `desired`"). Delete the now-unreachable
`if len(target) == 0 { log.Printf("supervisor: removed ...") }` branch from
`syncAgentsFile` -- the persona makes an empty block impossible -- and leave
`spliceSections` as it is.

Update the caller in `poll` (`s.syncInstructions(cfg)` -> `s.syncAgentsFile(cfg)`)
and the comment above it, which still says the managed block is the
"per-user instructions".

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/supervisor/ -v`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add internal/supervisor/persona.md internal/supervisor/supervisor.go internal/supervisor/supervisor_test.go workspace/
git commit -s -m "refactor(supervisor): one managed block for the persona and the instructions (issue #238)"
```

(`git rm workspace/AGENTS.md` in Step 2 staged the deletion; `git add workspace/`
carries it into this commit along with the trimmed `SOUL.md`.)

---

### Task 6: The seed stops overwriting what the agent wrote

**Files:**
- Modify: `internal/k8s/resources.go:166`, `deploy/openclaw-image.Dockerfile` (the
  workspace-seed comment near `COPY --chown=node:node workspace/`)
- Test: `internal/k8s/resources_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: nothing for other tasks; the pod spec's initContainer command changes.

- [ ] **Step 1: Write the failing assertion**

Add `"strings"` to `internal/k8s/resources_test.go`'s imports (it is not there
yet), then add this to `TestPodForSecurityBaseline`, next to the existing
`seed-workspace` line (`assertNonPrivilegedContainer(t, containerByName(t, pod, "seed-workspace"))`):

```go
	// The seed must not clobber what the agent wrote: only missing files are
	// copied (OpenClaw's own workspace bootstrap does the same with
	// writeFileIfMissing). A plain `cp -a` would overwrite AGENTS.md and SOUL.md
	// on every pod start.
	seed := containerByName(t, pod, "seed-workspace")
	if len(seed.Command) != 3 || !strings.Contains(seed.Command[2], "cp -a -n ") {
		t.Errorf("seed-workspace must copy only missing files, got %q", seed.Command)
	}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/k8s/ -run TestPodForSecurityBaseline -v`
Expected: FAIL -- the command carries `cp -a `

- [ ] **Step 3: Implement**

In `internal/k8s/resources.go`, change the initContainer command and its comment:

```go
				InitContainers: []corev1.Container{
					// Seed the workspace from the image's read-only layer into the
					// per-instance PVC (design §3.6: runtime caches live on the
					// instance PVC, not the image). The copy is no-clobber: a file
					// the agent has since written -- SOUL.md, its own notes -- is
					// left alone, matching OpenClaw's own writeFileIfMissing
					// semantics for workspace bootstrap files.
					{
						Name:            "seed-workspace",
						Image:           s.Image,
						ImagePullPolicy: s.PullPolicy,
						Command:         []string{"sh", "-c", "mkdir -p /mnt/data/workspace && cp -a -n /opt/cubepilot/workspace/. /mnt/data/workspace/ 2>/dev/null || true"},
```

In `deploy/openclaw-image.Dockerfile`, update the comment above the copy:

```dockerfile
# Workspace seed for the per-instance PVC: SOUL.md (the agent's tone file) only --
# the operating conventions are part of the supervisor binary and are rendered
# into the AGENTS.md managed block, so they are not seeded here. The
# seed-workspace initContainer copies this with --no-clobber, once.
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/k8s/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/k8s/resources.go internal/k8s/resources_test.go deploy/openclaw-image.Dockerfile
git commit -s -m "fix(agent-pod): seed the workspace without clobbering agent files (issue #238)"
```

---

### Task 7: Render the workshop configuration explicitly

**Files:**
- Modify: `internal/gateway/render.go` (the `cfg` map at 109-161)
- Test: `internal/gateway/render_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: the rendered `openclaw.json` carries
  `skills.workshop.autonomous.mode == "off"` and `skills.workshop.approvalPolicy == "auto"`.

- [ ] **Step 1: Write the failing test**

Add to `internal/gateway/render_test.go`:

```go
// TestRenderWorkshop verifies the skill-workshop settings are rendered rather than
// inherited: the runtime's defaults are autonomous, which would let the agent
// accumulate skills in the workspace on its own, and a platform behaviour that
// depends on an upstream default changes when that default changes.
func TestRenderWorkshop(t *testing.T) {
	b, err := Render("tok", "m", nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var cfg struct {
		Skills struct {
			Workshop struct {
				Autonomous struct {
					Mode string `json:"mode"`
				} `json:"autonomous"`
				ApprovalPolicy string `json:"approvalPolicy"`
			} `json:"workshop"`
		} `json:"skills"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// "off" removes the runtime's autonomous path (capture and repair), so a skill
	// only lands when the user asks for it. "auto" keeps the explicit
	// create-then-apply flow working: there is no proposal-review surface in this
	// product, so a "pending" policy would leave every apply waiting for an
	// approval nobody can give.
	if got := cfg.Skills.Workshop.Autonomous.Mode; got != "off" {
		t.Errorf("skills.workshop.autonomous.mode = %q, want off", got)
	}
	if got := cfg.Skills.Workshop.ApprovalPolicy; got != "auto" {
		t.Errorf("skills.workshop.approvalPolicy = %q, want auto", got)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/gateway/ -run TestRenderWorkshop -v`
Expected: FAIL -- `mode = ""`, want off

- [ ] **Step 3: Implement**

Add to the `cfg` map in `Render`, next to `"tools"`:

```go
		// Skill authoring is user-requested only. The runtime defaults this block
		// to autonomous (capture and repair both on), which would let the agent
		// accumulate its own skills in the workspace unasked -- instructions it
		// then follows, invisible to the platform. "off" disables the autonomous
		// path while leaving the explicit create-then-apply flow, which is what a
		// user request uses. approvalPolicy stays "auto" because there is no
		// proposal-review surface here: a "pending" policy would make every apply
		// wait for an approval nobody can give. Rendered rather than left to the
		// default so an upstream default change cannot silently alter platform
		// behaviour.
		"skills": map[string]any{
			"workshop": map[string]any{
				"autonomous":     map[string]any{"mode": "off"},
				"approvalPolicy": "auto",
			},
		},
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/gateway/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/gateway/render.go internal/gateway/render_test.go
git commit -s -m "feat(gateway): render the skill-workshop configuration explicitly (issue #238)"
```

---

### Task 8: Prove it in a real pod

**Files:**
- Create: `test/e2e/framework/exec.go`
- Create: `test/e2e/workspace_ownership_test.go`

**Interfaces:**
- Consumes: the pod name from the user's `AgentInstance` status
  (`k8s.InstanceName(user, v1alpha1.DefaultAgentName)`, `inst.Status.PodName`), as
  `agent_helpers_test.go` does.
- Produces: `func (f *Framework) Exec(ctx context.Context, pod, container string, cmd ...string) (string, error)`.

- [ ] **Step 1: Add the exec helper**

```go
package framework

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

// Exec runs cmd in container of pod and returns its stdout, with stderr folded
// into the error. It exists so a test can observe and perturb what the supervisor
// installs inside a running agent pod: the pod-side ownership rules can only be
// verified against the real supervisor/gateway pair, not a fake.
func (f *Framework) Exec(ctx context.Context, pod, container string, cmd ...string) (string, error) {
	req := f.KubeClient.CoreV1().RESTClient().Post().
		Resource("pods").Name(pod).Namespace(f.Namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   cmd,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)
	exec, err := remotecommand.NewSPDYExecutor(f.RestConfig, http.MethodPost, req.URL())
	if err != nil {
		return "", fmt.Errorf("exec setup: %w", err)
	}
	var stdout, stderr bytes.Buffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr}); err != nil {
		return stdout.String(), fmt.Errorf("exec %s: %w (stderr: %s)",
			strings.Join(cmd, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
```

- [ ] **Step 2: Write the e2e test**

```go
package e2e

import (
	"context"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/types"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/k8s"
)

const (
	agentContainer  = "supervisor"
	agentWorkspace  = "/home/node/.openclaw/workspace"
	agentConfigPath = "/home/node/.openclaw/openclaw.json"
	// A poll is 10s; two of them leave room for a slow fetch without hiding a
	// supervisor that never converges.
	convergeTimeout = 45 * time.Second
)

var _ = Describe("Workspace artifact ownership", Label("workspace"), func() {
	It("restores platform content the agent changed and leaves the agent's own skill alone", func() {
		ctx := context.Background()
		user := fw.DefaultUser
		name := k8s.InstanceName(user, v1alpha1.DefaultAgentName)

		var pod string
		Eventually(func() error {
			if err := agentStabilityErr(ctx, user); err != nil {
				return err
			}
			var inst v1alpha1.AgentInstance
			if err := fw.CtrlClient.Get(ctx, types.NamespacedName{Name: name, Namespace: fw.Namespace}, &inst); err != nil {
				return err
			}
			pod = inst.Status.PodName
			return nil
		}, 5*time.Minute, 5*time.Second).Should(Succeed())

		exec := func(cmd string) (string, error) {
			return fw.Exec(ctx, pod, agentContainer, "sh", "-c", cmd)
		}

		// The platform config is the artifact every instance has.
		before, err := exec("cat " + agentConfigPath)
		Expect(err).NotTo(HaveOccurred())
		Expect(before).To(ContainSubstring(`"gateway"`))

		// A skill the agent authored itself: unmarked, so the platform must not
		// delete it.
		_, err = exec("mkdir -p " + agentWorkspace + "/skills/e2e-agent-skill && " +
			"printf '# agent skill\\n' > " + agentWorkspace + "/skills/e2e-agent-skill/SKILL.md")
		Expect(err).NotTo(HaveOccurred())

		// The instance has platform skills installed (the builtin template ships
		// them). Require one: if none were installed the tamper step below would
		// silently skip, and the skill half of this test would pass without
		// exercising convergence at all.
		out, err := exec("ls -1d " + agentWorkspace + "/skills/*/ | grep -v e2e-agent-skill | head -1")
		Expect(err).NotTo(HaveOccurred())
		platformSkill := strings.TrimSpace(out)
		Expect(platformSkill).NotTo(BeEmpty(),
			"no platform skill installed: the skill-drift half of this test would pass vacuously")

		// Tamper: rewrite the platform's gateway config and that skill. `ls -1d` on
		// a directory glob yields a trailing slash, hence `platformSkill + "SKILL.md"`.
		_, err = exec("printf '{\"rogue\":true}' > " + agentConfigPath)
		Expect(err).NotTo(HaveOccurred())
		_, err = exec("printf '\\ntampered\\n' >> " + platformSkill + "SKILL.md")
		Expect(err).NotTo(HaveOccurred())

		// Within a poll or two the platform content is back, and the agent's skill
		// is untouched.
		Eventually(func() error {
			got, err := exec("cat " + agentConfigPath)
			if err != nil {
				return err
			}
			if !strings.Contains(got, `"gateway"`) {
				return fmt.Errorf("openclaw.json still tampered: %s", got)
			}
			if _, err := exec("test -f " + agentWorkspace + "/skills/e2e-agent-skill/SKILL.md"); err != nil {
				return fmt.Errorf("agent-authored skill removed: %w", err)
			}
			return nil
		}, convergeTimeout, 5*time.Second).Should(Succeed())

		// The tampered skill is back to the platform's content.
		out, err = exec("grep -l tampered " + platformSkill + "SKILL.md || true")
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(out)).To(BeEmpty())
	})
})
```

The `grep -v e2e-agent-skill` guard on the platform-skill lookup keeps the
agent-authored directory from being mistaken for the one to tamper with.

- [ ] **Step 3: Run it against the cluster**

Run (setup.sh deploys the stack, then the compiled suite runs; `CUBEPILOT_LLM_APIKEY`
is required by the target, `sk-placeholder` is fine for a deploy-only run):

```bash
CUBEPILOT_LLM_APIKEY=sk-placeholder make test-e2e
```

To iterate on this spec alone without the full suite:

```bash
go test -c ./test/e2e -o bin/e2e.test && \
  ./bin/e2e.test -test.v -ginkgo.v -ginkgo.focus="Workspace artifact ownership"
```

Expected: PASS. If it fails on the skill half, check that the instance actually has
platform skills installed (`ls /home/node/.openclaw/workspace/skills` in the pod)
before suspecting the supervisor -- with no skill installed the tamper step is a
no-op and only the config half of the assertion is exercising the supervisor.

- [ ] **Step 4: Commit**

```bash
git add test/e2e/framework/exec.go test/e2e/workspace_ownership_test.go
git commit -s -m "test(e2e): a tampered pod config converges and agent skills survive (issue #238)"
```

---

## Verification before the PR is updated

- [ ] `make test` (vet + unit) -- green.
- [ ] `golangci-lint run` -- `make test` does not cover it.
- [ ] `go test ./internal/supervisor/ ./internal/skill/ ./internal/k8s/ ./internal/gateway/ -v` -- the new tests are present and pass.
- [ ] `grep -rn "\.sha256" internal/ --include=*.go` -- no leftovers of the old marker.
- [ ] `grep -rn "syncInstructions\|reconcileInstructions\|lastCfgHash" internal/ --include=*.go` -- no callers of the removed names.
- [ ] `git status` -- `workspace/AGENTS.md` is deleted, `internal/supervisor/persona.md` is added.
- [ ] Push to `feat/issue238-workspace-artifact-ownership` and let CI (unit, e2e, DCO) run; reply to every CodeRabbit comment on PR #240.
