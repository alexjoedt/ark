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

// Operation names the repository operation an event belongs to. The values
// are stable identifiers, like Phase.
type Operation string

const (
	OperationBackup  Operation = "backup"
	OperationRestore Operation = "restore"
	OperationForget  Operation = "forget"
	OperationPrune   Operation = "prune"
	OperationCheck   Operation = "check"
	OperationVerify  Operation = "verify"
)

// ProgressEvent describes the current progress of a repository operation.
type ProgressEvent struct {
	// Operation is the operation that emitted the event.
	Operation Operation

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

// ProgressFunc receives progress events of repository operations. It must not
// block; implementations should drop events when the consumer is slow.
//
// Calls never overlap within one operation, so a per-call handler may update
// its state without a lock. Operations running concurrently on one Repository
// call a handler set with WithProgress concurrently: the repository holds no
// mutex across operations, so that handler locks its own state, or each
// operation gets its own handler through WithBackupProgress and
// WithRestoreProgress. The phases of a restore interleave: chunks of upcoming
// files are fetched while the current file is written.
//
// A failed or cancelled operation reports no terminal event: the error comes
// back from the call. Events carry no run identifier; a handler that needs one
// uses a per-call option.
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

// observer resolves the handler of one operation: the per-call one, else the
// repository default, else nopProgress. A non-nop handler is wrapped to stamp
// the operation on every event.
func (r *Repository) observer(op Operation, perCall ProgressFunc) ProgressFunc {
	fn := perCall
	if fn == nil {
		fn = r.progress
	}
	if fn == nil {
		return nopProgress
	}
	return func(e ProgressEvent) {
		e.Operation = op
		fn(e)
	}
}
