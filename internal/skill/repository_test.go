package skill

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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

func (brokenFS) Open(string) (fs.File, error)          { return nil, errors.New("boom") }
func (brokenFS) ReadDir(string) ([]fs.DirEntry, error) { return nil, errors.New("boom") }
