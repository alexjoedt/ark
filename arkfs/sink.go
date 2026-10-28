package arkfs

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/alexjoedt/ark"
)

// fileSink implements ark.Sink by writing entries to a local directory.
type fileSink struct {
	root  string
	dirs  []dirMeta           // deferred directory mode and mtime restoration
	links map[string]struct{} // slash paths of the symlinks this restore created
}

type dirMeta struct {
	path    string
	mode    os.FileMode
	modTime time.Time
}

// NewSink creates an ark.Sink that writes restored entries under root.
// The directory is created (along with parents) when Open is called.
func NewSink(root string) (ark.Sink, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("arkfs: resolve path %q: %w", root, err)
	}
	return &fileSink{root: abs}, nil
}

// Open creates the root directory. The filesystem sink accepts snapshots from
// any source type — it is a universal output target (useful for inspecting
// docker-volume snapshots on disk, for example).
func (s *fileSink) Open(_ context.Context, meta ark.SourceMeta) error {
	// A directory holds a filesystem backup and nothing else. Restoring another
	// medium's snapshot here would silently reinterpret what it means.
	if meta.Type != ark.SourceTypeFile {
		return fmt.Errorf("%w: expected %q, got %q",
			ark.ErrIncompatibleSourceType, ark.SourceTypeFile, meta.Type)
	}
	return os.MkdirAll(s.root, 0o755)
}

// Receive writes one filesystem entry to disk.
func (s *fileSink) Receive(_ context.Context, entry ark.FileInfo, r io.Reader) error {
	target := filepath.Join(s.root, filepath.FromSlash(entry.Path))

	if !isInsideDir(s.root, target) {
		return fmt.Errorf("arkfs: path %q escapes root directory", entry.Path)
	}
	// A symlink restored earlier redirects every write at or below its path,
	// past the lexical check above. A symlink itself replaces what is there.
	through := entry.Path
	if entry.Type == ark.FileTypeSymlink {
		through = path.Dir(path.Clean(entry.Path))
	}
	if s.underLink(through) {
		return fmt.Errorf("arkfs: path %q runs through a restored symlink", entry.Path)
	}

	switch entry.Type {
	case ark.FileTypeDirectory:
		if err := os.MkdirAll(target, entry.Mode|0o700); err != nil {
			return fmt.Errorf("arkfs: mkdir %s: %w", target, err)
		}
		setOwner(target, entry.UID, entry.GID)
		writeXattrs(target, entry.Xattrs)
		// Defer mode and mtime: the dir must stay writable for its children,
		// and creating them updates its mtime.
		s.dirs = append(s.dirs, dirMeta{path: target, mode: entry.Mode, modTime: entry.ModTime})

	case ark.FileTypeSymlink:
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("arkfs: mkdir parent of %s: %w", target, err)
		}
		_ = os.Remove(target) // remove stale entry if present
		if err := os.Symlink(entry.LinkDest, target); err != nil {
			return fmt.Errorf("arkfs: symlink %s -> %s: %w", target, entry.LinkDest, err)
		}
		if s.links == nil {
			s.links = make(map[string]struct{})
		}
		s.links[path.Clean(entry.Path)] = struct{}{}
		setOwner(target, entry.UID, entry.GID)

	case ark.FileTypeRegular:
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("arkfs: mkdir parent of %s: %w", target, err)
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, entry.Mode|0o600)
		if err != nil {
			return fmt.Errorf("arkfs: create %s: %w", target, err)
		}
		if r != nil {
			if _, err := io.Copy(f, r); err != nil {
				_ = f.Close()
				return fmt.Errorf("arkfs: write %s: %w", target, err)
			}
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("arkfs: close %s: %w", target, err)
		}
		// Chmod after close so umask doesn't interfere.
		_ = os.Chmod(target, entry.Mode)
		setOwner(target, entry.UID, entry.GID)
		writeXattrs(target, entry.Xattrs)
		_ = os.Chtimes(target, entry.ModTime, entry.ModTime)

	case ark.FileTypeHardlink:
		// LinkDest names another entry of this snapshot, already restored:
		// the core hands hardlinks to the sink after their target.
		source := filepath.Join(s.root, filepath.FromSlash(entry.LinkDest))
		if !isInsideDir(s.root, source) || s.underLink(entry.LinkDest) {
			return fmt.Errorf("arkfs: hardlink target %q escapes root directory", entry.LinkDest)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("arkfs: mkdir parent of %s: %w", target, err)
		}
		_ = os.Remove(target) // remove stale entry if present
		if linkErr := os.Link(source, target); linkErr != nil {
			// A target filesystem without hardlinks, or a restore split
			// across devices: a copy keeps the content, not the sharing.
			if copyErr := copyFile(source, target, entry); copyErr != nil {
				return fmt.Errorf("arkfs: hardlink %s -> %s: %w (copy fallback: %w)",
					target, source, linkErr, copyErr)
			}
		}

	case ark.FileTypeUnsupported:
		// The core drops these during backup; nothing reaches a sink.
	}
	return nil
}

// copyFile duplicates src to dst and applies the entry's metadata. It is the
// fallback when [os.Link] fails.
func copyFile(src, dst string, entry ark.FileInfo) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, entry.Mode|0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("write %s: %w", dst, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close %s: %w", dst, err)
	}
	_ = os.Chmod(dst, entry.Mode)
	setOwner(dst, entry.UID, entry.GID)
	writeXattrs(dst, entry.Xattrs)
	_ = os.Chtimes(dst, entry.ModTime, entry.ModTime)
	return nil
}

// Close applies deferred directory modes and mtimes in reverse order (deepest
// first) so that parent directories are not affected by child writes.
func (s *fileSink) Close() error {
	for _, d := range slices.Backward(s.dirs) {
		_ = os.Chmod(d.path, d.mode)
		_ = os.Chtimes(d.path, d.modTime, d.modTime)
	}
	return nil
}

// isInsideDir reports whether target is inside root after path cleaning.
func isInsideDir(root, target string) bool {
	cleaned := filepath.Clean(target)
	return cleaned == root || strings.HasPrefix(cleaned, root+string(filepath.Separator))
}

// underLink reports whether the slash path p, or a directory above it, is a
// symlink this restore created.
func (s *fileSink) underLink(p string) bool {
	for p = path.Clean(p); p != "." && p != "/"; p = path.Dir(p) {
		if _, ok := s.links[p]; ok {
			return true
		}
	}
	return false
}
