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

func TestValidateTar(t *testing.T) {
	if err := ValidateTar([]byte("not a tar")); err == nil {
		t.Fatal("non-gzip payload should be rejected")
	}
	if err := ValidateTar(mustPack(t, fstest.MapFS{"SKILL.md": {Data: []byte("x")}})); err != nil {
		t.Fatalf("valid tar rejected: %v", err)
	}
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
