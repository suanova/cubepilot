package skill

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

func TestExtractCorruptTar(t *testing.T) {
	if err := ExtractTar(bytes.NewReader([]byte("not a tar")), t.TempDir()); err == nil {
		t.Fatal("corrupt tar expected error")
	}
}

func TestExtractRejectsTraversal(t *testing.T) {
	outside := t.TempDir()
	dest := filepath.Join(outside, "dest")
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "../evil", Mode: 0o644, Size: 4, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("evil")); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()
	if err := ExtractTar(bytes.NewReader(buf.Bytes()), dest); err == nil {
		t.Fatal("traversal entry expected error")
	}
	if _, err := os.Stat(filepath.Join(outside, "evil")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("traversal wrote outside dest: %v", err)
	}
}

func TestWriteBytesThenExtract(t *testing.T) {
	root := t.TempDir()
	repo := &PathRepository{Root: root}
	data := mustPack(t, fstest.MapFS{"SKILL.md": {Data: []byte("# x\n")}})
	sha, err := repo.WriteBytes(t.Context(), "skills/a/v1.tar.gz", data)
	if err != nil {
		t.Fatalf("WriteBytes: %v", err)
	}
	// read back: sha matches and ExtractTar works.
	f, err := repo.Open(t.Context(), "skills/a/v1.tar.gz")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	dest := filepath.Join(t.TempDir(), "ws", "skills", "a")
	if err := ExtractTar(f, dest); err != nil {
		t.Fatalf("ExtractTar: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "SKILL.md")); err != nil || string(b) != "# x\n" {
		t.Fatalf("SKILL.md = %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(root, "skills", "a", "v1.tar.gz")); err != nil {
		t.Fatalf("target not written: %v", err)
	}
	if sha == "" {
		t.Fatal("WriteBytes returned empty sha")
	}
	// no temp files left
	matches, _ := filepath.Glob(filepath.Join(root, "skills", "a", "*.tmp-*"))
	if len(matches) != 0 {
		t.Fatalf("temp files left: %v", matches)
	}
}

func TestResolveVersion(t *testing.T) {
	root := t.TempDir()
	repo := &PathRepository{Root: root}
	v1 := mustPack(t, fstest.MapFS{"SKILL.md": {Data: []byte("v1")}})
	v2 := mustPack(t, fstest.MapFS{"SKILL.md": {Data: []byte("v2")}})
	sha1 := sha256Hex(v1)
	sha2 := sha256Hex(v2)

	// Nothing stored -> next version v1, not stored.
	ver, stored, err := ResolveVersion(t.Context(), repo, "a", sha1)
	if err != nil || stored || ver != "v1" {
		t.Fatalf("empty repo: ver=%q stored=%v err=%v", ver, stored, err)
	}
	if _, err := repo.WriteBytes(t.Context(), "skills/a/v1.tar.gz", v1); err != nil {
		t.Fatal(err)
	}

	// Same content -> v1 already stored.
	ver, stored, err = ResolveVersion(t.Context(), repo, "a", sha1)
	if err != nil || !stored || ver != "v1" {
		t.Fatalf("same content: ver=%q stored=%v err=%v", ver, stored, err)
	}
	// New content -> next version v2, not stored.
	ver, stored, err = ResolveVersion(t.Context(), repo, "a", sha2)
	if err != nil || stored || ver != "v2" {
		t.Fatalf("new content: ver=%q stored=%v err=%v", ver, stored, err)
	}
}

func mustPack(t *testing.T, src fs.FS) []byte {
	t.Helper()
	data, err := Pack(src)
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	return data
}

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
	// A root SKILL.md that is a symlink (not a regular file) is rejected.
	sym := mustTarWithHeader(t, &tar.Header{Name: "SKILL.md", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd", Mode: 0o777})
	if err := ValidateSkillTar(sym); err == nil {
		t.Fatal("symlink SKILL.md should be rejected")
	}
}

// TestValidateSkillTarLimits verifies the decompression-bomb guards: too many
// entries or too many decompressed bytes are rejected.
func TestValidateSkillTarLimits(t *testing.T) {
	oldEntries, oldBytes := maxSkillEntries, maxSkillDecompressed
	defer func() { maxSkillEntries, maxSkillDecompressed = oldEntries, oldBytes }()

	maxSkillDecompressed = 32 // any file over 32 bytes trips the cap
	big := mustPack(t, fstest.MapFS{"SKILL.md": {Data: bytes.Repeat([]byte("x"), 64)}})
	if err := ValidateSkillTar(big); err == nil {
		t.Fatal("archive above the decompressed cap should be rejected")
	}

	maxSkillDecompressed = 100 << 20
	maxSkillEntries = 2
	many := mustPack(t, fstest.MapFS{"SKILL.md": {Data: []byte("x")}, "a": {Data: []byte("1")}, "b": {Data: []byte("2")}})
	if err := ValidateSkillTar(many); err == nil {
		t.Fatal("archive above the entry cap should be rejected")
	}
}

// mustTarWithHeader builds a one-entry gzip tar with the given header (a
// zero-size entry; used for the symlink SKILL.md case).
func mustTarWithHeader(t *testing.T, hdr *tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// TestPathTraversalRejected verifies a CRD-supplied source.path can never
// read/write outside the repository root.
func TestPathTraversalRejected(t *testing.T) {
	root := t.TempDir()
	repo := &PathRepository{Root: root}
	data := mustPack(t, fstest.MapFS{"SKILL.md": {Data: []byte("x")}})
	for _, rel := range []string{"../../etc/passwd", "/etc/passwd", "../x.tar.gz", "a/../../x.tar.gz"} {
		if _, err := repo.Open(t.Context(), rel); err == nil {
			t.Errorf("Open(%q) escaped root", rel)
		}
		if _, err := repo.WriteBytes(t.Context(), rel, data); err == nil {
			t.Errorf("WriteBytes(%q) escaped root", rel)
		}
	}
}

type failWriter struct{}

func (failWriter) Write(p []byte) (int, error) { return 0, errors.New("boom") }

// TestWriteTarPropagatesWriterError verifies an output-writer failure during
// archive creation is returned (a truncated archive is never a success).
func TestWriteTarPropagatesWriterError(t *testing.T) {
	err := writeTar(failWriter{}, fstest.MapFS{"SKILL.md": {Data: []byte("x")}})
	if err == nil {
		t.Fatal("writeTar with a failing writer should error")
	}
}

// TestTreeHash verifies the tree hash answers "is the content on disk the
// content we installed": it is stable, it ignores the entry the caller skips
// (the platform's own bookkeeping file), and every drift a re-extract would
// fix -- changed content, an added or removed file, an added empty directory --
// changes the result.
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

	base, err := TreeHash(dir, ".cubepilot.json")
	if err != nil {
		t.Fatalf("TreeHash: %v", err)
	}
	if again, err := TreeHash(dir, ".cubepilot.json"); err != nil || again != base {
		t.Fatalf("TreeHash not stable: %q vs %q (%v)", base, again, err)
	}
	// The skipped entry is not part of the tree: rewriting it changes nothing.
	write(".cubepilot.json", `{"tree":"another"}`)
	if got, _ := TreeHash(dir, ".cubepilot.json"); got != base {
		t.Error("the skipped file changed the tree hash")
	}
	// Each of these is drift, so each must change the hash.
	write("SKILL.md", "# b\n")
	if got, _ := TreeHash(dir, ".cubepilot.json"); got == base {
		t.Error("a content change did not change the hash")
	}
	write("SKILL.md", "# a\n")
	write("extra.md", "x\n")
	if got, _ := TreeHash(dir, ".cubepilot.json"); got == base {
		t.Error("an added file did not change the hash")
	}
	if err := os.Remove(filepath.Join(dir, "extra.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "empty2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, _ := TreeHash(dir, ".cubepilot.json"); got == base {
		t.Error("an added empty directory did not change the hash")
	}
	if err := os.Remove(filepath.Join(dir, "refs", "one.md")); err != nil {
		t.Fatal(err)
	}
	if got, _ := TreeHash(dir, ".cubepilot.json"); got == base {
		t.Error("a removed file did not change the hash")
	}
}

// TestTreeHashMissingDir verifies a directory that is not there is an error,
// not an empty tree: reporting a hash for missing content would let the
// supervisor believe an uninstalled skill is in place.
func TestTreeHashMissingDir(t *testing.T) {
	if _, err := TreeHash(filepath.Join(t.TempDir(), "gone"), ".cubepilot.json"); err == nil {
		t.Fatal("a missing directory should error, not report a hash")
	}
}
