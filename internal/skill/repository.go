package skill

import (
	"archive/tar"
	"compress/gzip"
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

// ExtractTar unpacks an arbitrary gzip tar stream (a repo read, an HTTP
// fetch, or a temp file) into destDir, rejecting path traversal (".."
// escapes).
func ExtractTar(r io.Reader, destDir string) error {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
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
			perm := os.FileMode(hdr.Mode) & 0o777
			if perm == 0 {
				perm = 0o644
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
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

// writeTar packs the files of src (walked recursively) into a gzip tar on w.
func writeTar(w io.Writer, src fs.FS) error {
	gz := gzip.NewWriter(w)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close() // must flush the tar trailer before gz closes
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
