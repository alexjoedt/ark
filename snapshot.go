package ark

import "time"

// Snapshot describes a completed backup, as returned by the public API.
type Snapshot struct {
	ID         string
	SourceName string
	SourceType SourceType
	SourceMeta map[string]string // labels the source recorded, see SourceMeta.Labels
	Excludes   []string          // patterns the source excluded, see SourceMeta.Excludes
	Host       string
	Tags       []string
	Comment    string
	CreatedAt  time.Time
	ParentID   string
	Stats      SnapshotStats
}

// SnapshotStats holds deduplication and compression statistics for a snapshot.
// Skipped counts entries the source yielded that the v1 format does not model:
// device nodes, fifos and sockets. FilesUnchanged counts the regular files a
// backup took from the parent snapshot without reading them.
type SnapshotStats struct {
	FileCount          int           `json:"total_files"`
	DirCount           int           `json:"total_dirs"`
	Skipped            int           `json:"skipped"`
	FilesUnchanged     int           `json:"files_unchanged,omitempty"`
	TotalSize          int64         `json:"total_size"`
	StoredSize         int64         `json:"stored_size"`
	NewChunksRawSize   int64         `json:"new_chunks_raw_size"`
	ChunksTotal        int           `json:"chunks_total"`
	ChunksNew          int           `json:"chunks_new"`
	ChunksReused       int           `json:"chunks_reused"`
	DeduplicationRatio float64       `json:"deduplication_ratio"`
	CompressionRatio   float64       `json:"compression_ratio"`
	Duration           time.Duration `json:"-"`
}

// computeRatios derives the two ratios from the counters, once they are final.
func (s *SnapshotStats) computeRatios() {
	if s.TotalSize > 0 {
		s.DeduplicationRatio = 1.0 - float64(s.NewChunksRawSize)/float64(s.TotalSize)
	}
	if s.NewChunksRawSize > 0 {
		s.CompressionRatio = 1.0 - float64(s.StoredSize)/float64(s.NewChunksRawSize)
	}
}

// SnapshotFilter narrows which snapshots are returned by Snapshots or Forget.
// Zero value means no filtering — all snapshots are included.
type SnapshotFilter struct {
	// IDs selects exactly these snapshots. Snapshots resolves prefixes to
	// full ids before matching, so Match itself compares them verbatim.
	IDs []string

	// Name filters by source name (volume name, path, etc.); empty = all.
	Name string

	// Type filters by source type, e.g. SourceTypeFile; empty = all.
	Type SourceType

	// Tags filters to snapshots carrying ALL the given tags; empty = all.
	Tags []string

	// Host filters by host; empty = all.
	Host string

	// Before includes only snapshots created before this time (exclusive).
	Before time.Time

	// After includes only snapshots created after this time (inclusive).
	After time.Time
}

// backupOptions collects the BackupOption values of one Backup call. It is
// unexported so the With* functions stay the only way to build it.
type backupOptions struct {
	comment  string
	tags     []string
	host     string
	parent   string
	progress ProgressFunc
}

// BackupOption is a functional option for Backup.
type BackupOption func(*backupOptions)

// WithComment attaches a human-readable description to the snapshot.
func WithComment(s string) BackupOption {
	return func(o *backupOptions) { o.comment = s }
}

// WithTags attaches one or more tags to the snapshot.
func WithTags(tags ...string) BackupOption {
	return func(o *backupOptions) { o.tags = tags }
}

// WithHost overrides the hostname recorded in the snapshot.
func WithHost(h string) BackupOption {
	return func(o *backupOptions) { o.host = h }
}

// WithParent names the parent snapshot explicitly, as a full id or a unique
// prefix. Without it the parent is the newest snapshot of the same source. A
// parent of another source is recorded but no file is taken from it unread.
func WithParent(id string) BackupOption {
	return func(o *backupOptions) { o.parent = id }
}

// WithBackupProgress sets the observer for this backup, overriding the
// repository default of WithProgress; nil keeps the default. Events of one
// call never interleave with another operation's. fn must not block.
func WithBackupProgress(fn ProgressFunc) BackupOption {
	return func(o *backupOptions) { o.progress = fn }
}

// restoreOptions collects the RestoreOption values of one Restore call.
type restoreOptions struct {
	progress   ProgressFunc
	paths      []string
	cacheLimit int64
	cacheSet   bool
}

// RestoreOption is a functional option for Restore.
type RestoreOption func(*restoreOptions)

// WithRestoreProgress sets the observer for this restore, overriding the
// repository default of WithProgress; nil keeps the default. Events of one
// call never interleave with another operation's. fn must not block.
func WithRestoreProgress(fn ProgressFunc) RestoreOption {
	return func(o *restoreOptions) { o.progress = fn }
}

// WithRestorePaths restores only the entries under the given paths. Prefixes
// are matched against the snapshot's own paths, and a path that leads nowhere
// in the snapshot restores nothing. The walk is pruned before any chunk is
// read, so the skipped part costs nothing.
func WithRestorePaths(paths ...string) RestoreOption {
	return func(o *restoreOptions) { o.paths = paths }
}

// WithRestoreCache caps the bytes of chunk content the restore keeps around to
// serve content that repeats within the snapshot. The default is 256 MiB, and
// zero disables the cache.
//
// The memory a restore holds is this cap plus one chunk per worker in flight,
// where the workers are bounded by WithConcurrency. It does not grow with the
// size of the snapshot or of any single file.
func WithRestoreCache(bytes int64) RestoreOption {
	return func(o *restoreOptions) {
		o.cacheLimit = bytes
		o.cacheSet = true
	}
}

// RestoreResult is returned by Restore.
type RestoreResult struct {
	FilesRestored int
	BytesRestored int64
}

// PruneResult is returned by Prune. A dry run fills it with what the same
// prune would do.
type PruneResult struct {
	// BlobsRemoved counts the chunks and tree nodes the index listed in the
	// packs that were deleted, less those copied into new packs. A pack no
	// index listed adds its bytes only.
	BlobsRemoved int
	// BytesFreed is the stored size of the deleted packs less BytesRepacked.
	BytesFreed int64
	// PacksDeleted counts the packs that were deleted: those without a
	// referenced object, those no index listed, and those whose referenced
	// objects were copied into new packs.
	PacksDeleted int
	// BytesRepacked is the sealed size of the objects copied into new packs.
	BytesRepacked int64
	// BytesUnused is the sealed size of what stays in the repository without
	// use, objects no snapshot references and second copies of objects: at
	// most 5% of the object bytes, not counting the second copies in packs
	// without an unreferenced object, which no prune rewrites.
	BytesUnused int64
	DryRun      bool
}

// VerifyResult is returned by Verify.
type VerifyResult struct {
	SnapshotID    string
	ChunksChecked int
	MissingChunks []string
	CorruptChunks []string
	OK            bool
}

// CheckResult is returned by Check, which walks all snapshots.
type CheckResult struct {
	SnapshotsChecked int
	// Errors are findings that break a snapshot or a later backup: a
	// manifest, tree or chunk that is missing or does not open, a pack the
	// index lists that is not stored, a pack whose own table disagrees with
	// the index.
	Errors []string
	// Warnings are findings that leave every snapshot intact: a pack no index
	// lists or an index object that does not open, both of which Prune
	// removes, or a lock that does not open, which Unlock WithForce removes.
	// A pack with a blob that did not open and that could not be read again
	// to compare its table with the index is one as well, beside the errors
	// about the blob.
	Warnings []string
}

// RebuildIndexResult is returned by RebuildIndex.
type RebuildIndexResult struct {
	// PacksIndexed and BlobsIndexed count what the new index lists.
	PacksIndexed int
	BlobsIndexed int
	// UnreadablePacks names the stored packs whose own table does not open,
	// data and tree packs alike. The new index lists in them what the old
	// one did, without the blobs a pack that opens holds as well, and
	// nothing when the old one did not list them.
	UnreadablePacks []string
}
