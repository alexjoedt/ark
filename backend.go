package ark

import (
	"context"
	"io"
)

// HandleType identifies the category of an object stored in a Backend.
// Each type owns a namespace named by String(); a backend maps that name to a
// directory or key prefix. New types may appear in minor versions, so a
// backend must derive its layout from String() instead of switching on the
// constants.
type HandleType uint8

const (
	// ConfigFile is the repository config object. One per repository.
	ConfigFile HandleType = 1 + iota
	// BlobFile is a CAS content chunk. Backends shard these by name prefix.
	BlobFile
	// TreeFile is a tree node, named by the hash of its canonical JSON.
	TreeFile
	// SnapshotFile is a snapshot manifest, named by the snapshot id.
	SnapshotFile
	// LockFile is a repository lock object.
	LockFile
	// KeyFile holds the master key sealed under one password.
	KeyFile
)

// UnknownNamespace is the namespace name String returns for a handle type this
// version of ark does not define. Backends compare against it to reject one.
const UnknownNamespace = "unknown"

// String returns the namespace name of the handle type, or UnknownNamespace
// for a type this version of ark does not define.
func (t HandleType) String() string {
	switch t {
	case ConfigFile:
		return "config"
	case BlobFile:
		return "chunks"
	case TreeFile:
		return "trees"
	case SnapshotFile:
		return "snapshots"
	case LockFile:
		return "locks"
	case KeyFile:
		return "keys"
	default:
		return UnknownNamespace
	}
}

// Handle identifies a single object in the Backend.
type Handle struct {
	Type HandleType
	Name string
}

// BlobInfo describes a stored object.
type BlobInfo struct {
	Name string
	Size int64 // size in bytes as stored (e.g. compressed)
}

// Backend is the storage abstraction for all repository objects.
//
// All methods must be safe for concurrent use. The core issues at most
// WithConcurrency operations at a time, defaulting to [runtime.NumCPU]*2, so a
// remote backend can size its connection pool from that bound.
//
// A backend maps Handle.Type to a namespace by its String() value and applies
// sharding only to BlobFile. An unknown type maps to the namespace "unknown";
// implementations may reject it instead.
type Backend interface {
	// Put stores r under h atomically: a reader never observes a partial
	// object. Implementations write to a temporary name in the same
	// namespace and rename it into place. Put is idempotent, an existing
	// object is replaced.
	Put(ctx context.Context, h Handle, r io.Reader) error

	// Get retrieves a reader for the object stored under h.
	// Callers must close the returned ReadCloser.
	Get(ctx context.Context, h Handle) (io.ReadCloser, error)

	// Exists returns true if the object identified by h is present.
	Exists(ctx context.Context, h Handle) (bool, error)

	// Delete removes the object identified by h.
	// Removing a non-existent object is not an error.
	Delete(ctx context.Context, h Handle) error

	// List calls fn once for each object of the given type with its name and
	// stored size. If fn returns an error, List stops and returns it.
	List(ctx context.Context, t HandleType, fn func(BlobInfo) error) error

	// Stat returns metadata for the object stored under h.
	Stat(ctx context.Context, h Handle) (BlobInfo, error)

	// Close releases any resources held by the backend.
	Close() error
}
