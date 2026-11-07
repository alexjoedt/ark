package ark

import (
	"context"
	"io"
	"os"
	"time"
)

// SourceType tags a snapshot with its origin.
// Core defines only SourceTypeFile. Plugin packages (e.g. pkg/arkdocker)
// define their own SourceType constants; the string value is stored verbatim
// in the snapshot manifest and compared at restore time.
type SourceType string

// SourceTypeFile identifies a local filesystem backup.
const SourceTypeFile SourceType = "file"

// SourceMeta is stored verbatim in the snapshot manifest.
type SourceMeta struct {
	// Name is the logical name used as the snapshot label (e.g. volume name, path).
	Name string

	// Type identifies the origin of the data.
	Type SourceType

	// Labels holds arbitrary key-value metadata (e.g. docker_host, docker_version).
	Labels map[string]string
}

// FileType represents the type of a filesystem entry.
type FileType string

const (
	// FileTypeRegular is a normal file.
	FileTypeRegular FileType = "regular"

	// FileTypeDirectory is a directory.
	FileTypeDirectory FileType = "directory"

	// FileTypeSymlink is a symbolic link.
	FileTypeSymlink FileType = "symlink"

	// FileTypeHardlink is a second name for a file already yielded under
	// another path. LinkDest names that earlier path and the content is
	// stored once, under the first name.
	FileTypeHardlink FileType = "hardlink"

	// FileTypeUnsupported marks an entry the v1 format does not model:
	// device nodes, fifos, sockets. The core drops it and counts it in
	// SnapshotStats.Skipped.
	FileTypeUnsupported FileType = "unsupported"
)

// FileInfo describes a single entry yielded by a Source or received by a Sink.
//
// The JSON tags are a convenience for callers that log or print entries. They
// are not the repository wire format; the tree entry type owns that.
type FileInfo struct {
	Path     string            `json:"path"`
	Size     int64             `json:"size"`
	Mode     os.FileMode       `json:"mode"`
	UID      int               `json:"uid"`
	GID      int               `json:"gid"`
	ModTime  time.Time         `json:"mtime"`
	Type     FileType          `json:"type"`
	LinkDest string            `json:"link_dest,omitempty"`
	Xattrs   map[string][]byte `json:"xattrs,omitempty"`

	// ChangeTime and Inode let a backup trust that a regular file is
	// unchanged since its parent snapshot: ctime cannot be set from user
	// space, so it catches a rewrite that kept the mtime. A source that
	// cannot tell (a tar stream) leaves both zero, and only Size and ModTime
	// are compared.
	ChangeTime time.Time `json:"ctime,omitzero"`
	Inode      uint64    `json:"inode,omitempty"`
}

// WalkFunc is called for each entry produced by a Source.
//
// open is nil for every entry except regular files; the caller must check
// entry.Type. For a regular file, open returns a reader for entry.Size bytes.
// The caller calls open at most once and closes the reader before
// returning, or never calls it when the parent snapshot already holds the
// file unchanged. The next call to fn advances the source past the entry,
// read or not ([archive/tar.Reader.Next] skips unread bytes).
type WalkFunc func(entry FileInfo, open func() (io.ReadCloser, error)) error

// Source drives the backup pipeline entry by entry.
//
// Entries may arrive in any order; the core sorts every tree node by name
// before hashing it.
//
// Implementations must:
//   - yield a hardlink target before the FileTypeHardlink entries naming it
//   - pass open = nil for everything but regular files
//   - for regular files, pass an open that yields at most entry.Size bytes
//     and does no work until it is called; a walk that never calls open
//     must succeed
//   - be safe for a single concurrent Walk call
type Source interface {
	Meta() SourceMeta
	Walk(ctx context.Context, fn WalkFunc) error
}

// Sink receives file entries during a restore.
//
// Lifecycle:
//  1. Open    — called once before any Receive; Sink may inspect meta and reject
//     incompatible source types by returning an error (wrap
//     ErrIncompatibleSourceType where appropriate).
//  2. Receive — called once per entry, in walk order.
//  3. Close   — called once after the last Receive (or on abort).
//
// The core reconstructs each file's byte stream from the CAS and calls
// Receive once per entry, parents before children. A FileTypeHardlink entry
// always arrives after the entry it points at.
//
// Implementations must:
//   - validate source type in Open and return ErrIncompatibleSourceType on mismatch
//   - handle r = nil for everything but regular files
//   - fully consume r for regular files before returning
//   - call Close exactly once after the last Receive
type Sink interface {
	Open(ctx context.Context, meta SourceMeta) error
	Receive(ctx context.Context, entry FileInfo, r io.Reader) error
	Close() error
}
