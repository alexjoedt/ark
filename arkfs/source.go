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
//	src, err := arkfs.NewSource("/data/myapp")
//	snap, err := repo.Backup(ctx, src)
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

	"github.com/alexjoedt/ark"
)

// fileSource implements ark.Source by walking a local directory tree.
type fileSource struct {
	root string
	meta ark.SourceMeta
}

// NewSource creates an ark.Source that walks the directory tree rooted at root.
// The directory must exist.
func NewSource(root string) (ark.Source, error) {
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
	return &fileSource{
		root: abs,
		meta: ark.SourceMeta{
			Name: abs,
			Type: ark.SourceTypeFile,
		},
	}, nil
}

func (s *fileSource) Meta() ark.SourceMeta { return s.meta }

// Walk visits every entry under the source root. Regular files, directories,
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

		// Symlinks: filepath.WalkDir uses Lstat, so d.Type() has ModeSymlink.
		if d.Type()&os.ModeSymlink != 0 {
			linfo, err := os.Lstat(path)
			if err != nil {
				return fmt.Errorf("arkfs: lstat %s: %w", path, err)
			}
			target, err := os.Readlink(path)
			if err != nil {
				return fmt.Errorf("arkfs: readlink %s: %w", path, err)
			}
			uid, gid := fileOwner(linfo)
			return fn(ark.FileInfo{
				Path:     rel,
				Type:     ark.FileTypeSymlink,
				Mode:     linfo.Mode().Perm(),
				ModTime:  linfo.ModTime(),
				UID:      uid,
				GID:      gid,
				LinkDest: target,
			}, nil)
		}

		// Directories.
		if d.IsDir() {
			fi, err := d.Info()
			if err != nil {
				return fmt.Errorf("arkfs: info %s: %w", path, err)
			}
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

		fi, err := d.Info()
		if err != nil {
			return fmt.Errorf("arkfs: info %s: %w", path, err)
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

		if entry.Xattrs, err = readXattrs(path); err != nil {
			return err
		}

		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("arkfs: open %s: %w", path, err)
		}
		defer f.Close()

		return fn(entry, io.LimitReader(f, entry.Size))
	})
}
