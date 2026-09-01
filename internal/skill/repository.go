package skill

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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

// safeJoin joins rel under root and rejects paths that would escape root
// (absolute or ".." traversal), so a CRD-supplied source.path can never
// read/write outside the repository volume.
func safeJoin(root, rel string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(rel))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes repository root: %q", rel)
	}
	return filepath.Join(root, clean), nil
}

func (r *PathRepository) Write(ctx context.Context, relPath string, src fs.FS) (string, error) {
	abs, err := safeJoin(r.Root, relPath)
	if err != nil {
		return "", err
	}
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

// WriteBytes writes data (a gzip tar) to relPath atomically (temp + rename)
// and returns its sha256. On any error the temp file is removed.
func (r *PathRepository) WriteBytes(ctx context.Context, relPath string, data []byte) (string, error) {
	abs, err := safeJoin(r.Root, relPath)
	if err != nil {
		return "", err
	}
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
	if _, err := io.Copy(tmp, io.TeeReader(bytes.NewReader(data), h)); err != nil {
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
	abs, err := safeJoin(r.Root, relPath)
	if err != nil {
		return nil, err
	}
	return os.Open(abs)
}

func (r *PathRepository) Extract(ctx context.Context, relPath, destDir string) error {
	abs, err := safeJoin(r.Root, relPath)
	if err != nil {
		return err
	}
	f, err := os.Open(abs)
	if err != nil {
		return err
	}
	defer f.Close()
	return ExtractTar(f, destDir)
}

// Pack builds the gzip tar bytes for src (a skill directory), for callers
// that publish the content over the wire (operator -> API publish endpoint).
func Pack(src fs.FS) ([]byte, error) {
	var buf bytes.Buffer
	if err := writeTar(&buf, src); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ResolveVersion returns the version label for publishing name with content
// hash sha: the existing vN whose stored tar content matches sha (stored=true,
// no write needed), or the next unused vN (stored=false, caller writes it).
func ResolveVersion(ctx context.Context, repo Repository, name, sha string) (string, bool, error) {
	for v := 1; ; v++ {
		p := fmt.Sprintf("skills/%s/v%d.tar.gz", name, v)
		rc, err := repo.Open(ctx, p)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return fmt.Sprintf("v%d", v), false, nil
			}
			return "", false, err
		}
		h := sha256.New()
		if _, err := io.Copy(h, rc); err != nil {
			rc.Close()
			return "", false, err
		}
		rc.Close()
		if hex.EncodeToString(h.Sum(nil)) == sha {
			return fmt.Sprintf("v%d", v), true, nil
		}
	}
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
// Close errors from the tar and gzip writers are propagated so a truncated
// archive is never reported as a successful write.
func writeTar(w io.Writer, src fs.FS) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	walkErr := fs.WalkDir(src, ".", func(path string, d fs.DirEntry, err error) error {
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
	// Flush the tar trailer, then the gzip stream (order matters).
	twCloseErr := tw.Close()
	gzCloseErr := gz.Close()
	if walkErr != nil {
		return walkErr
	}
	if twCloseErr != nil {
		return twCloseErr
	}
	return gzCloseErr
}
