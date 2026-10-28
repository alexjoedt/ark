// Package arklocal implements ark.Backend on the local filesystem.
//
// Objects live under <dir>/<namespace>/<name>, where the namespace is
// ark.HandleType.String(). Chunks are sharded two levels deep by the first
// four characters of their name: chunks/aa/bb/<name>.
package arklocal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/alexjoedt/ark"
)

// tmpPrefix marks in-flight objects written by Put. List skips them.
const tmpPrefix = ".ark-tmp-"

// Compile-time check that *Backend satisfies ark.Backend.
var _ ark.Backend = (*Backend)(nil)

// Backend implements ark.Backend backed by a directory tree.
type Backend struct {
	basePath string
}

// New creates or opens a local backend rooted at dir.
// The directory (and its parents) is created if it does not already exist.
func New(dir string) (*Backend, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("arklocal: create backend directory %s: %w", dir, err)
	}
	return &Backend{basePath: dir}, nil
}

// BasePath returns the root directory of the local backend.
func (b *Backend) BasePath() string { return b.basePath }

// namespace returns the directory holding objects of type t.
func (b *Backend) namespace(t ark.HandleType) (string, error) {
	ns := t.String()
	if ns == ark.UnknownNamespace {
		return "", fmt.Errorf("arklocal: unknown handle type %d", t)
	}
	return filepath.Join(b.basePath, ns), nil
}

// path maps a handle to its absolute file path. Names are opaque object names,
// never paths, so anything that could escape the namespace is rejected.
func (b *Backend) path(h ark.Handle) (string, error) {
	dir, err := b.namespace(h.Type)
	if err != nil {
		return "", err
	}
	if h.Name == "" || h.Name == "." || h.Name == ".." ||
		strings.ContainsAny(h.Name, `/\`) || strings.HasPrefix(h.Name, tmpPrefix) {
		return "", fmt.Errorf("arklocal: invalid object name %q", h.Name)
	}
	if h.Type == ark.BlobFile && len(h.Name) >= 4 {
		return filepath.Join(dir, h.Name[:2], h.Name[2:4], h.Name), nil
	}
	return filepath.Join(dir, h.Name), nil
}

// Put writes r to a temporary file in the target directory and renames it into
// place, so a reader never sees a partial object.
func (b *Backend) Put(ctx context.Context, h ark.Handle, r io.Reader) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p, err := b.path(h)
	if err != nil {
		return err
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("arklocal: mkdir %s: %w", dir, err)
	}

	f, err := os.CreateTemp(dir, tmpPrefix+"*")
	if err != nil {
		return fmt.Errorf("arklocal: create temp in %s: %w", dir, err)
	}
	tmp := f.Name()
	defer func() {
		_ = f.Close()
		_ = os.Remove(tmp) // no-op once the rename succeeded
	}()

	if _, err := io.Copy(f, r); err != nil {
		return fmt.Errorf("arklocal: write %s: %w", h.Name, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("arklocal: close %s: %w", h.Name, err)
	}
	// No chmod: CreateTemp already made the file 0600, and mounts without
	// unix permissions (cifs, vfat) reject the call.
	if err := os.Rename(tmp, p); err != nil {
		return fmt.Errorf("arklocal: rename %s: %w", h.Name, err)
	}
	return nil
}

// Get opens the object stored under h.
func (b *Backend) Get(ctx context.Context, h ark.Handle) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p, err := b.path(h)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ark.ErrNotFound, h.Name)
		}
		return nil, fmt.Errorf("arklocal: open %s: %w", h.Name, err)
	}
	return f, nil
}

// Exists reports whether the object identified by h is present.
func (b *Backend) Exists(ctx context.Context, h ark.Handle) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	p, err := b.path(h)
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(p); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("arklocal: stat %s: %w", h.Name, err)
	}
	return true, nil
}

// Delete removes the object identified by h. A missing object is not an error.
func (b *Backend) Delete(ctx context.Context, h ark.Handle) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p, err := b.path(h)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("arklocal: remove %s: %w", h.Name, err)
	}
	return nil
}

// List walks the namespace of t and calls fn for every object it holds.
// An absent namespace is empty, not an error.
func (b *Backend) List(ctx context.Context, t ark.HandleType, fn func(ark.BlobInfo) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dir, err := b.namespace(t)
	if err != nil {
		return err
	}
	err = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() || strings.HasPrefix(d.Name(), tmpPrefix) {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil {
			if errors.Is(statErr, fs.ErrNotExist) {
				return nil // deleted while walking
			}
			return fmt.Errorf("arklocal: stat %s: %w", d.Name(), statErr)
		}
		return fn(ark.BlobInfo{Name: d.Name(), Size: info.Size()})
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("arklocal: list %s: %w", t, err)
	}
	return nil
}

// Stat returns metadata for the object stored under h.
func (b *Backend) Stat(ctx context.Context, h ark.Handle) (ark.BlobInfo, error) {
	if err := ctx.Err(); err != nil {
		return ark.BlobInfo{}, err
	}
	p, err := b.path(h)
	if err != nil {
		return ark.BlobInfo{}, err
	}
	fi, err := os.Stat(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ark.BlobInfo{}, fmt.Errorf("%w: %s", ark.ErrNotFound, h.Name)
		}
		return ark.BlobInfo{}, fmt.Errorf("arklocal: stat %s: %w", h.Name, err)
	}
	return ark.BlobInfo{Name: h.Name, Size: fi.Size()}, nil
}

// Close is a no-op for the local backend.
func (b *Backend) Close() error { return nil }
