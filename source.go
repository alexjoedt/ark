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
}

// WalkFunc is called for each entry produced by a Source.
//
// r is nil for every entry except regular files; the caller must check
// entry.Type before reading. For regular files, r is an [io.LimitReader] for
// entry.Size bytes. The caller must fully consume r before returning — the
// next call to fn advances the underlying stream (e.g. the tar reader inside
// a Docker volume source).
type WalkFunc func(entry FileInfo, r io.Reader) error

// Source drives the backup pipeline entry by entry.
//
// Entries may arrive in any order; the core sorts every tree node by name
// before hashing it.
//
// Implementations must:
//   - yield a hardlink target before the FileTypeHardlink entries naming it
//   - pass r = nil for everything but regular files
//   - pass r = [io.LimitReader](..., entry.Size) for regular files
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
