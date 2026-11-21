package ark

import (
	"fmt"
	"maps"
	"time"
)

// SnapshotID names a snapshot. A full ID is 32 lowercase hex characters. A
// method or option that takes one also accepts a unique prefix of it, in
// upper or lower case, and fails with ErrNotFound or ErrAmbiguousID when it
// names no snapshot or more than one. Chunk names and lock names are plain strings.
type SnapshotID string

// Snapshot describes a completed backup, as returned by the public API.
type Snapshot struct {
	ID         SnapshotID
	SourceName string
	SourceType SourceType
	Labels     map[string]string // the source's labels and those of WithLabels
	Excludes   []string          // patterns the source excluded, see SourceMeta.Excludes
	Host       string
	Tags       []string
	Comment    string
	CreatedAt  time.Time
	ParentID   SnapshotID
	Stats      SnapshotStats
}

// seriesKey identifies a series: the snapshots of one source type and source
// name written from one host. Parent selection and the forget keep policy
// group by it. The three fields are compared verbatim as the manifest stores
// them, so an empty host is a series of its own.
type seriesKey struct {
	sourceType SourceType
	sourceName string
	host       string
}

func (s *Snapshot) series() seriesKey { return seriesKey{s.SourceType, s.SourceName, s.Host} }

// SnapshotStats holds deduplication and compression statistics for a snapshot.
// Skipped counts entries the source yielded that the v1 format does not model:
// device nodes, fifos and sockets. FilesUnchanged counts the regular files a
// backup took from the parent snapshot without reading them.
//
// FileCount counts regular files and hardlink entries. TotalSize sums the
// bytes of the regular files, so the content of a hardlinked file counts once.
// NewChunksRawSize is the raw size of the distinct data chunks the backup
// wrote, each counted once however often the snapshot references it, and
// StoredSize is what they take in packs after compression and encryption.
// Neither includes the snapshot's tree objects.
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
	IDs []SnapshotID

	// Source filters by source name (volume name, path, etc.); empty = all.
	Source string

	// Type filters by source type, e.g. SourceTypeFile; empty = all.
	Type SourceType

	// Tags filters to snapshots carrying ALL the given tags; empty = all.
	Tags []string

	// Host filters by host; empty = all.
	Host string

	// Labels filters to snapshots carrying every given label with exactly
	// the given value; empty = all. A pair with an empty value needs the key
	// with an empty value, not just the key.
	Labels map[string]string

	// Before includes only snapshots created before this time (exclusive).
	Before time.Time

	// After includes only snapshots created at or after this time
	// (inclusive). Until the fix that matched this documentation it was
	// exclusive: a caller that polls with the last seen CreatedAt as a cursor
	// gets that snapshot again and skips it by ID.
	After time.Time
}

// backupOptions collects the BackupOption values of one Backup call. It is
// unexported so the With* functions stay the only way to build it.
type backupOptions struct {
	comment  string
	tags     []string
	labels   map[string]string
	host     string
	parent   SnapshotID
	time     time.Time
	timeSet  bool
	progress ProgressFunc
	scan     bool
}

// checkTime refuses a WithTime that is the zero time or lies in the future.
func (o *backupOptions) checkTime() error {
	switch {
	case !o.timeSet:
		return nil
	case o.time.IsZero():
		return fmt.Errorf("%w: the zero time", ErrInvalidTime)
	case o.time.After(time.Now()):
		return fmt.Errorf("%w: %s is in the future", ErrInvalidTime, o.time.Format(time.RFC3339))
	}
	return nil
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

// WithLabels adds labels to the snapshot, beside those the source records.
// Several calls add up; for a key set twice the later value wins. Backup
// checks them with [ValidateLabels] and refuses a key the source sets as
// well, both before it writes anything.
func WithLabels(labels map[string]string) BackupOption {
	return func(o *backupOptions) {
		if o.labels == nil {
			o.labels = make(map[string]string, len(labels))
		}
		maps.Copy(o.labels, labels)
	}
}

// WithHost sets the host recorded in the snapshot; the default is empty, the
// library never reads the machine's hostname. The host is part of the series
// key with the source type and source name: snapshots of one source from two
// hosts are two series, each with its own parent chain and its own keep
// policy in [Repository.Forget].
func WithHost(h string) BackupOption {
	return func(o *backupOptions) { o.host = h }
}

// WithTime sets the time recorded as the snapshot's CreatedAt, stored in UTC,
// in place of the time the backup starts: for a backup of data as it was at
// an earlier time, and for tests of a keep policy. Backup refuses the zero
// time and a time in the future with [ErrInvalidTime] before it writes
// anything.
//
// The time does not change which snapshot is the parent, still the newest of
// the series by CreatedAt, so a backup dated before its parent is allowed.
// Files changed since the recorded time are read again by the next backup
// that takes this snapshot as its parent, which is why a future time, which
// would let that backup pass over a file changed during this one, is refused.
func WithTime(t time.Time) BackupOption {
	return func(o *backupOptions) { o.time, o.timeSet = t, true }
}

// WithParent names the parent snapshot explicitly, as a full ID or a unique
// prefix. Without it the parent is the newest snapshot of the same series:
// source type, source name and host ([WithHost], empty by default). A parent
// of another source type or name is recorded but no file is taken from it
// unread.
func WithParent(snapshotID SnapshotID) BackupOption {
	return func(o *backupOptions) { o.parent = snapshotID }
}

// WithBackupProgress sets the observer for this backup, overriding the
// repository default of WithProgress; nil keeps the default. Events of one
// call never interleave with another operation's. fn must not block.
func WithBackupProgress(fn ProgressFunc) BackupOption {
	return func(o *backupOptions) { o.progress = fn }
}

// WithBackupScan counts the files and bytes of the source in a second walk
// beside the store, so progress events carry FilesTotal and BytesTotal: the
// running count goes in FilesDone and BytesDone of PhaseScan events while the
// scan runs, the final count in the totals of the last PhaseScan event and of
// every PhaseStore event after it. It applies to a RewalkSource and does
// nothing for any other source or when the backup has no progress handler,
// per call or on the repository. The count follows the source's excludes and
// includes unchanged files, as do the FilesDone and BytesDone of PhaseStore
// events. A scan that fails is dropped, one still running when the files are
// stored is cancelled; neither changes the backup, so totals are best effort.
func WithBackupScan() BackupOption {
	return func(o *backupOptions) { o.scan = true }
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
	// Skipped holds one error per entry the sink left out with an error
	// wrapping ErrEntrySkipped, in restore order. Each names the entry's
	// path; the restore of every other entry went on.
	Skipped []error
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
	SnapshotID    SnapshotID
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
