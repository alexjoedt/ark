package ark

// Phase names the step an operation is currently in. The values are stable
// identifiers, safe to compare and to emit as JSON; rendering them for humans
// is the caller's job.
type Phase string

const (
	// PhaseScan opens a backup, PhaseStore and PhaseFinalize follow.
	PhaseScan     Phase = "scan"     // walking the source
	PhaseStore    Phase = "store"    // chunking and writing
	PhaseFinalize Phase = "finalize" // tree and manifest

	// PhaseLoad opens a restore, PhaseFetch follows.
	PhaseLoad  Phase = "load"  // manifest and tree
	PhaseFetch Phase = "fetch" // chunks
	PhaseWrite Phase = "write" // sink
)

// ProgressEvent describes the current progress of a backup or restore.
type ProgressEvent struct {
	Phase Phase

	// CurrentPath is the entry the event reports, empty when the phase works
	// on no single entry. A backup reports a file once it is stored; files
	// read in parallel finish, and are reported, out of walk order.
	CurrentPath string

	FilesTotal  int
	FilesDone   int
	BytesTotal  int64
	BytesDone   int64
	ChunksTotal int
	ChunksDone  int
}

// ProgressFunc receives progress events from one backup or restore operation.
// It must not block; implementations should drop events when the consumer is
// slow. Calls never overlap, so a handler may update its state without a
// lock. The phases of a restore interleave: chunks of upcoming files are
// fetched while the current file is written.
//
// Example with a channel:
//
//	ch := make(chan ark.ProgressEvent, 32)
//	snap, err := repo.Backup(ctx, src, ark.WithBackupProgress(func(e ark.ProgressEvent) {
//		select { case ch <- e: default: }
//	}))
type ProgressFunc func(ProgressEvent)

// nopProgress is the zero-cost handler used when no handler is configured.
var nopProgress ProgressFunc = func(ProgressEvent) {}
