// Package arkfs implements ark.Source and ark.Sink for local filesystem
// directories. It is the filesystem counterpart to pkg/arkdocker.
//
// Usage with the path convenience functions:
//
//	snap, err := arkfs.Backup(ctx, repo, "/data/myapp")
//	result, err := arkfs.Restore(ctx, repo, snap.ID, "/tmp/restore")
//
// Usage with raw Source/Sink (for use with repo.Backup / repo.Restore):
//
//	src, err := arkfs.NewSource("/data/myapp",
//		arkfs.WithExclude("*.tmp", "/cache"),
//		arkfs.WithExcludeFile("/data/myapp.excludes"))
//	snap, err := repo.Backup(ctx, src)
//
// The exclude patterns follow restic's rules, see package arkglob, and are
// matched against the path relative to the source root. The snapshot records
// them.
//
//	sink, err := arkfs.NewSink("/tmp/restore")
//	result, err := repo.Restore(ctx, snap.ID, sink)
package arkfs

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkglob"
)

// fileSource implements ark.Source by walking a local directory tree.
type fileSource struct {
	root    string
	meta    ark.SourceMeta
	exclude *arkglob.Matcher

	skipCloud bool
	// dataless is the per-OS check, nil where none exists; a test swaps it.
	dataless func(os.FileInfo) bool
}

// SourceOption configures NewSource.
type SourceOption func(*fileSource) error

// WithExclude leaves out every path that matches one of patterns, and
// everything below a matched directory. Repeated options append.
func WithExclude(patterns ...string) SourceOption {
	return func(s *fileSource) error {
		s.meta.Excludes = append(s.meta.Excludes, patterns...)
		return nil
	}
}

// WithExcludeFile reads exclude patterns from the file at path, one per line.
// A leading UTF-8 byte order mark is dropped, surrounding whitespace is
// trimmed, blank lines and lines starting with "#" are skipped, and "$" stays
// literal. A bad pattern fails with an error that names the file and line,
// and so does a file that is not UTF-8 text, such as a UTF-16 one.
func WithExcludeFile(path string) SourceOption {
	return func(s *fileSource) error {
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("arkfs: exclude file: %w", err)
		}
		n := 0
		for line := range strings.Lines(strings.TrimPrefix(string(data), "\ufeff")) {
			n++
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if _, err := arkglob.New(line); err != nil {
				return fmt.Errorf("arkfs: exclude file %s line %d: %w", path, n, err)
			}
			s.meta.Excludes = append(s.meta.Excludes, line)
		}
		return nil
	}
}

// WithExcludeCloudFiles leaves out files that exist only in the cloud, because
// reading one makes the system download it and a backup would pull a whole
// cloud share onto the disk. It skips regular files named like ".name.icloud"
// (the iCloud Drive placeholders) on Linux and macOS and, on macOS, every
// entry whose st_flags carry SF_DATALESS (File Provider shares). A dataless
// directory is skipped with its subtree and never listed. Skipped entries
// appear in no stats counter and the snapshot does not record the option.
func WithExcludeCloudFiles() SourceOption {
	return func(s *fileSource) error {
		s.skipCloud = true
		return nil
	}
}

// cloudOnly reports whether the entry is a cloud-only placeholder. It returns
// the entry's info when it had to read it, so the caller reads it only once.
func (s *fileSource) cloudOnly(d os.DirEntry) (os.FileInfo, bool, error) {
	if !s.skipCloud {
		return nil, false, nil
	}
	name := d.Name()
	// ".<stem>.icloud" with a non-empty stem, so a real ".icloud" is kept.
	placeholder := len(name) > len("..icloud") &&
		strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".icloud")
	if placeholder && d.Type().IsRegular() {
		return nil, true, nil
	}
	if s.dataless == nil {
		return nil, false, nil
	}
	fi, err := d.Info()
	if err != nil {
		return nil, false, err
	}
	return fi, s.dataless(fi), nil
}

// NewSource creates an ark.Source that walks the directory tree rooted at root.
// The directory must exist. A bad exclude pattern fails with an error that
// wraps [path.ErrBadPattern].
func NewSource(root string, opts ...SourceOption) (ark.Source, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("arkfs: resolve path %q: %w", root, err)
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return nil, fmt.Errorf("arkfs: stat %q: %w", abs, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("arkfs: %q is not a directory", abs)
	}
	s := &fileSource{
		root: abs,
		meta: ark.SourceMeta{
			Name: abs,
			Type: ark.SourceTypeFile,
		},
		dataless: dataless,
	}
	for _, opt := range opts {
		if err := opt(s); err != nil {
			return nil, err
		}
	}
	if len(s.meta.Excludes) > 0 {
		if s.exclude, err = arkglob.New(s.meta.Excludes...); err != nil {
			return nil, fmt.Errorf("arkfs: %w", err)
		}
	}
	return s, nil
}

func (s *fileSource) Meta() ark.SourceMeta {
	m := s.meta
	m.Excludes = slices.Clone(m.Excludes)
	return m
}

// ConcurrentOpen reports that the open functions Walk passes stay valid after
// fn returns: each opens its file by path, so a backup reads files in parallel.
func (s *fileSource) ConcurrentOpen() bool { return true }

var _ ark.ConcurrentSource = (*fileSource)(nil)

// Walk visits every entry under the source root that no exclude pattern
// matches; an excluded directory is not read. Regular files, directories,
// symlinks and hardlinks are yielded; device nodes, fifos and sockets are
// yielded as ark.FileTypeUnsupported so the core can count them.
//
// A file with more than one name reaches fn as a regular file under the name
// the walk sees first and as ark.FileTypeHardlink under every later one.
func (s *fileSource) Walk(ctx context.Context, fn ark.WalkFunc) error {
	linked := make(map[inodeKey]string)

	return filepath.WalkDir(s.root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("arkfs: walk error at %s: %w", path, walkErr)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		rel, err := filepath.Rel(s.root, path)
		if err != nil {
			return fmt.Errorf("arkfs: relative path for %s: %w", path, err)
		}
		if rel == "." {
			return nil // skip root directory itself
		}
		// Normalize to forward slashes for consistent snapshot format.
		rel = filepath.ToSlash(rel)

		// Before any stat or xattr read, and before the hardlink map learns
		// the entry, so a later name of an excluded file is stored as content.
		if s.exclude != nil && s.exclude.Match(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		fi, skip, err := s.cloudOnly(d)
		if err != nil {
			return fmt.Errorf("arkfs: info %s: %w", path, err)
		}
		if skip {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if fi == nil {
			// An lstat: WalkDir does not follow symlinks.
			if fi, err = d.Info(); err != nil {
				return fmt.Errorf("arkfs: info %s: %w", path, err)
			}
		}

		if d.Type()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return fmt.Errorf("arkfs: readlink %s: %w", path, err)
			}
			uid, gid := fileOwner(fi)
			return fn(ark.FileInfo{
				Path:     rel,
				Type:     ark.FileTypeSymlink,
				Mode:     fi.Mode().Perm(),
				ModTime:  fi.ModTime(),
				UID:      uid,
				GID:      gid,
				LinkDest: target,
			}, nil)
		}

		// Directories.
		if d.IsDir() {
			xattrs, err := readXattrs(path)
			if err != nil {
				return err
			}
			uid, gid := fileOwner(fi)
			return fn(ark.FileInfo{
				Path:    rel,
				Type:    ark.FileTypeDirectory,
				Mode:    fi.Mode().Perm(),
				ModTime: fi.ModTime(),
				UID:     uid,
				GID:     gid,
				Xattrs:  xattrs,
			}, nil)
		}

		uid, gid := fileOwner(fi)
		entry := ark.FileInfo{
			Path:    rel,
			Type:    ark.FileTypeRegular,
			Size:    fi.Size(),
			Mode:    fi.Mode().Perm(),
			ModTime: fi.ModTime(),
			UID:     uid,
			GID:     gid,
		}

		// Devices, fifos and sockets: the core counts them and drops them.
		if !fi.Mode().IsRegular() {
			entry.Type = ark.FileTypeUnsupported
			entry.Size = 0
			return fn(entry, nil)
		}

		// A second name for an inode already yielded stores no content again.
		if key, nlink, ok := fileID(fi); ok && nlink > 1 {
			if first, seen := linked[key]; seen {
				entry.Type = ark.FileTypeHardlink
				entry.LinkDest = first
				return fn(entry, nil)
			}
			linked[key] = rel
		}

		// Only a file with content uses them, so only it carries them.
		entry.ChangeTime, entry.Inode = changeID(fi)

		if entry.Xattrs, err = readXattrs(path); err != nil {
			return err
		}

		// The file is opened only when the core reads it, which it skips
		// for a file the parent snapshot holds unchanged, and possibly after
		// fn returned (ConcurrentOpen), so the caller owns the close.
		return fn(entry, func() (io.ReadCloser, error) {
			f, err := os.Open(path)
			if err != nil {
				return nil, fmt.Errorf("arkfs: open %s: %w", path, err)
			}
			return struct {
				io.Reader
				io.Closer
			}{io.LimitReader(f, entry.Size), f}, nil
		})
	})
}
