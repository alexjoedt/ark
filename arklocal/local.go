// Package arklocal implements ark.Backend on the local filesystem.
//
// Objects live under <dir>/<namespace>/<name>, where the namespace is
// ark.HandleType.String(). Packs are sharded one level deep by the first two
// characters of their name, 256 directories created on first use:
// datapacks/aa/<name>.
package arklocal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/internal/rangeread"
)

// tmpPrefix marks in-flight objects written by Put. List skips them, as it
// skips every name ark.CheckName refuses.
const tmpPrefix = ".ark-tmp-"

// Compile-time check that *Backend satisfies ark.Backend.
var _ ark.Backend = (*Backend)(nil)

// Backend implements ark.Backend backed by a directory tree.
type Backend struct {
	basePath string
	root     *os.File // the root directory, open for Sync
	written  written  // what Put and Delete touched since the last Sync, with trackWrites
}

// New creates or opens a local backend rooted at dir.
// The directory (and its parents) is created if it does not already exist.
func New(dir string) (*Backend, error) {
	// existing is the deepest ancestor of dir that is there before MkdirAll.
	existing := dir
	for trackWrites && existing != filepath.Dir(existing) {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		existing = filepath.Dir(existing)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("arklocal: create backend directory %s: %w", dir, err)
	}
	root, err := os.Open(dir)
	if err != nil {
		return nil, fmt.Errorf("arklocal: open backend directory %s: %w", dir, err)
	}
	b := &Backend{basePath: dir, root: root}
	if existing != dir {
		// MkdirAll created dir, and maybe parents: the first Sync makes
		// their entries durable too, or a power loss drops the repository.
		b.written.record(existing, "", filepath.Dir(dir))
	}
	return b, nil
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
	if err := ark.CheckName(h.Name); err != nil {
		return "", fmt.Errorf("arklocal: %s: %w", h.Type, err)
	}
	if (h.Type == ark.DataPackFile || h.Type == ark.TreePackFile) && len(h.Name) >= 2 {
		return filepath.Join(dir, h.Name[:2], h.Name), nil
	}
	return filepath.Join(dir, h.Name), nil
}

// Put writes r to a temporary file in the target directory and renames it into
// place, so a reader never sees a partial object. Nothing is flushed to disk:
// the object becomes durable with the next Sync.
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
	if trackWrites {
		b.written.record(b.basePath, p, dir)
	}
	return nil
}

// Sync makes every Put and Delete that returned before the call durable and
// returns the first failure. On Linux that is syncfs on the root directory,
// whose error is returned; only a filesystem without syncfs falls back to the
// global sync. On macOS it fsyncs every file and directory written since the
// last Sync, then issues one F_FULLFSYNC on the root; after a failure the next
// Sync flushes them again.
func (b *Backend) Sync(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	files, dirs, err := b.written.flush()
	if err != nil {
		return err
	}
	if err := syncFS(b.root); err != nil {
		return err
	}
	b.written.forget(files, dirs)
	return nil
}

// Get opens the object stored under h.
func (b *Backend) Get(ctx context.Context, h ark.Handle) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := b.open(h)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// GetRange reads length bytes of the object stored under h at offset.
func (b *Backend) GetRange(ctx context.Context, h ark.Handle, offset, length int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := b.path(h); err != nil {
		return nil, err
	}
	if offset < 0 || length < 0 {
		return nil, fmt.Errorf("arklocal: invalid range %d+%d of %s", offset, length, h.Name)
	}
	f, err := b.open(h)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf, err := rangeread.Read(f, offset, length)
	if err != nil {
		return nil, fmt.Errorf("arklocal: range %d+%d of %s: %w", offset, length, h.Name, err)
	}
	return buf, nil
}

// open opens the object stored under h for reading.
func (b *Backend) open(h ark.Handle) (*os.File, error) {
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
// The removal becomes durable with the next Sync.
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
	if trackWrites {
		b.written.record(b.basePath, "", filepath.Dir(p))
	}
	return nil
}

// List reads the namespace of t and calls fn for every object it holds: the
// files of a flat namespace, or the files in each shard dir of a pack
// namespace. Symlinks and Windows junctions to a namespace, a shard or an
// object are followed. Entries outside that layout are skipped. An absent
// namespace is empty while the repository root is there; a dir or an entry
// that cannot be read fails the listing, so it is never partial.
func (b *Backend) List(ctx context.Context, t ark.HandleType, fn func(ark.BlobInfo) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dir, err := b.namespace(t)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) && !dangling(dir) {
		// The namespace was never written, unless the root is gone with it,
		// as behind a link to a disk that is not mounted.
		if _, err = os.Stat(b.basePath); err == nil {
			return nil
		}
	}
	if err == nil {
		err = listDir(ctx, dir, entries, t == ark.DataPackFile || t == ark.TreePackFile, "", fn)
	}
	if err != nil {
		return fmt.Errorf("arklocal: list %s: %w", t, err)
	}
	return nil
}

// listDir calls fn for every object among the entries of dir. With sharded,
// dir is a pack namespace: its two-character dirs are shards, listed in turn,
// and only a name shorter than a shard is an object there (see path). An
// object in a shard starts with the shard's name.
func listDir(ctx context.Context, dir string, entries []fs.DirEntry, sharded bool, shard string,
	fn func(ark.BlobInfo) error,
) error {
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := e.Name()
		// Temporary objects and files another tool leaves, such as a
		// .DS_Store, are no objects: a caller could not read or delete them.
		if ark.CheckName(name) != nil {
			skip(dir, name)
			continue
		}
		info, err := follow(dir, e)
		switch {
		case err != nil:
			return err
		case info == nil && sharded && len(name) == 2:
			// A shard that vanished mid-listing leaves it partial: a caller
			// would take the objects it held as not stored.
			return fmt.Errorf("%s vanished while listing: %w", filepath.Join(dir, name), fs.ErrNotExist)
		case info == nil:
			// deleted since the dir was read
		case info.IsDir() && sharded && len(name) == 2:
			sub := filepath.Join(dir, name)
			objects, err := os.ReadDir(sub)
			if err != nil {
				return err
			}
			if err := listDir(ctx, sub, objects, false, name, fn); err != nil {
				return err
			}
		case info.Mode().IsRegular() && strings.HasPrefix(name, shard) && (!sharded || len(name) < 2):
			if err := fn(ark.BlobInfo{Name: name, Size: info.Size()}); err != nil {
				return err
			}
		default:
			skip(dir, name)
		}
	}
	return nil
}

// follow returns the file info of e in dir, through a symlink or a Windows
// junction (ModeIrregular since Go 1.23), or nil when the entry is gone. A link whose target is gone fails: it may lead to a disk that
// is not mounted, whose objects are stored all the same.
func follow(dir string, e fs.DirEntry) (fs.FileInfo, error) {
	p := filepath.Join(dir, e.Name())
	var info fs.FileInfo
	var err error
	if e.Type()&linkMode != 0 {
		info, err = os.Stat(p)
	} else {
		info, err = e.Info()
	}
	if errors.Is(err, fs.ErrNotExist) && !dangling(p) {
		return nil, nil //nolint:nilnil // a nil info is the entry that is gone
	}
	if err != nil {
		return nil, err // a PathError names the full path
	}
	return info, nil
}

// linkMode marks an entry List follows: a symlink, or a Windows junction.
const linkMode = fs.ModeSymlink | fs.ModeIrregular

// dangling reports whether p is a link whose target is gone.
func dangling(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.Mode()&linkMode != 0
}

// skip logs an entry List passes over because it is outside the layout.
func skip(dir, name string) {
	slog.Debug("arklocal: skipping an entry outside the repository layout", "dir", dir, "name", name)
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

// Close releases the root directory. The backend cannot Sync afterwards.
func (b *Backend) Close() error { return b.root.Close() }
