package ark

import "time"

// Snapshot describes a completed backup, as returned by the public API.
type Snapshot struct {
	ID         string
	SourceName string
	SourceType SourceType
	SourceMeta map[string]string // labels the source recorded, see SourceMeta.Labels
	Host       string
	Tags       []string
	Comment    string
	CreatedAt  time.Time
	ParentID   string
	Stats      SnapshotStats
}

// SnapshotStats holds deduplication and compression statistics for a snapshot.
// Skipped counts entries the source yielded that the v1 format does not model:
// device nodes, fifos and sockets.
type SnapshotStats struct {
	FileCount          int           `json:"total_files"`
	DirCount           int           `json:"total_dirs"`
	Skipped            int           `json:"skipped"`
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

// countChunks folds one file's chunk references into the statistics. A
// reference with a zero stored size is a chunk the CAS already held.
func (s *SnapshotStats) countChunks(chunks []chunkRef) {
	for _, c := range chunks {
		s.ChunksTotal++
		if c.Size > 0 {
			s.ChunksNew++
			s.StoredSize += int64(c.Size)
			s.NewChunksRawSize += c.Length
		} else {
			s.ChunksReused++
		}
	}
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
// prefix. Without it the parent is the newest snapshot of the same source.
func WithParent(id string) BackupOption {
	return func(o *backupOptions) { o.parent = id }
}

// WithBackupProgress sets the observer for this backup. One observer per
// call, so two operations on the same Repository never interleave their
// events. fn must not block.
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

// WithRestoreProgress sets the observer for this restore. One observer per
// call, so two operations on the same Repository never interleave their
// events. fn must not block.
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

// PruneResult is returned by Prune.
type PruneResult struct {
	BlobsRemoved int
	BytesFreed   int64
	DryRun       bool
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
	Errors           []string
}
