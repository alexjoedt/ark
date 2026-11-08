package ark

// Phase names the step an operation is currently in. The values are stable
// identifiers, safe to compare and to emit as JSON; rendering them for humans
// is the caller's job.
//
// The phases per operation, in order. Forget, prune, verify and check emit
// their first event before any I/O. Every operation emits exactly one
// PhaseFinalize event, last and only on success.
//
//   - backup: PhaseScan, PhaseStore, PhaseFinalize
//   - restore: PhaseLoad, PhaseFetch and PhaseWrite interleaved, PhaseFinalize
//     with the totals and the files, bytes and chunks restored
//   - forget: PhaseLoad, PhaseFinalize with FilesDone = manifests removed
//   - prune: PhaseLoad, PhaseSweep with ChunksDone/ChunksTotal over the
//     chunks and trees listed, PhaseFinalize
//   - verify: PhaseLoad, PhaseFetch per chunk reference, PhaseFinalize;
//     CurrentPath is the resolved snapshot ID, empty on PhaseLoad
//   - check: PhaseLoad, then per snapshot the PhaseFetch events of its verify
//     and one PhaseFetch with FilesDone = snapshots checked, PhaseFinalize;
//     CurrentPath is the snapshot ID
type Phase string

const (
	PhaseScan     Phase = "scan"     // walking the source
	PhaseStore    Phase = "store"    // chunking and writing
	PhaseFinalize Phase = "finalize" // the closing event of a successful operation
	PhaseLoad     Phase = "load"     // manifests and trees
	PhaseFetch    Phase = "fetch"    // chunks
	PhaseWrite    Phase = "write"    // sink
	PhaseSweep    Phase = "sweep"    // listing and deleting unreferenced objects
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
