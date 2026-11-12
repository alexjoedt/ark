package ark

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/alexjoedt/ark/chunker"
)

// openConfig holds the options of an Open call. It is the caller's runtime
// configuration; the repository's own on-disk configuration is
// repositoryConfig in config.go.
type openConfig struct {
	logger      *slog.Logger
	concurrency int
	lockWait    time.Duration
	progress    ProgressFunc
	lockEvent   func(LockEvent)
}

// Option configures a Repository at Open time.
type Option func(*openConfig)

// WithLogger sets the logger for the repository.
func WithLogger(l *slog.Logger) Option {
	return func(c *openConfig) { c.logger = l }
}

// WithLockWait makes an operation retry a blocked lock for up to d instead of
// failing at once with ErrRepoLocked. The default is zero, no waiting.
func WithLockWait(d time.Duration) Option {
	return func(c *openConfig) {
		if d > 0 {
			c.lockWait = d
		}
	}
}

// WithProgress sets the default progress observer of the repository. Every
// operation reports to it unless the call passes its own observer, see
// WithBackupProgress and WithRestoreProgress; a nil per-call observer falls
// back to this one. Operations running concurrently call fn concurrently, see
// ProgressFunc.
func WithProgress(fn ProgressFunc) Option {
	return func(c *openConfig) { c.progress = fn }
}

// WithLockEvent makes the repository call fn whenever one of its operations
// acquires a lock, has an attempt rejected by conflicting locks, or releases
// one. Released fires once the operation is done and before its lock object
// is deleted, so a consumer that blocks the call orders the release before
// any acquire it allows elsewhere; a failed delete follows as LockLeftOver.
// fn runs on the operation's goroutine and may block, which holds up that
// operation; operations running concurrently call fn concurrently.
func WithLockEvent(fn func(LockEvent)) Option {
	return func(c *openConfig) { c.lockEvent = fn }
}

// WithConcurrency caps how many backend operations run at once. Values below
// one fall back to the default, twice the number of CPUs.
func WithConcurrency(n int) Option {
	return func(c *openConfig) {
		if n > 0 {
			c.concurrency = n
		}
	}
}

// Repository is the primary entry point for all backup operations.
type Repository struct {
	backend     Backend
	codec       *objectCodec
	logger      *slog.Logger
	concurrency int
	lockWait    time.Duration
	progress    ProgressFunc    // repository default observer, may be nil
	lockEvent   func(LockEvent) // lock hook from WithLockEvent, may be nil

	id      string         // repository id from the config object
	key     *masterKey     // unsealed from the key file Open matched
	keyName string         // name of that key file
	chunker chunker.Config // chunker parameters of the config, keyed from key

	// prefetchBytes is the floor of the restore prefetch budget, zero for the
	// default; tests lower it to see the budget at work on small fixtures.
	prefetchBytes int64

	// clock is time.Now except in tests, which drive the lock staleness
	// rules through it.
	clock func() time.Time
}

// now is the repository's notion of the current time.
func (r *Repository) now() time.Time {
	if r.clock == nil {
		return time.Now()
	}
	return r.clock()
}

// Open opens an existing repository in b.
//
// It reads the config object first and returns ErrNotInitialized when the
// backend holds none, so Open never creates a repository; use Init for that.
// A config or key file naming a format this build does not read returns
// ErrUnsupportedFormat.
//
// Then it unseals the master key with pw before any other object is read. It
// returns ErrWrongPassword when no key file opens, ErrPasswordRequired for
// WithoutPassword on a protected repository and ErrNotPasswordProtected for a
// password on a repository created with WithoutPassword, and ErrCorruptConfig
// when a key opens but the config is not the one it was sealed with. Unsealing
// a password protected key file runs argon2id with 64 MiB of memory.
func Open(ctx context.Context, b Backend, pw Passphrase, opts ...Option) (*Repository, error) {
	if err := pw.validate(); err != nil {
		return nil, err
	}
	cfg := &openConfig{
		logger:      slog.Default(),
		concurrency: runtime.NumCPU() * 2,
	}
	for _, o := range opts {
		o(cfg)
	}

	data, err := readRepositoryConfig(ctx, b)
	if err != nil {
		return nil, err
	}

	codec := newObjectCodec()
	key, keyName, err := loadMasterKey(ctx, b, codec, data, pw)
	if err != nil {
		codec.Close()
		return nil, err
	}
	rc, err := parseRepositoryConfig(data)
	if err != nil {
		codec.Close()
		return nil, err
	}
	cc := rc.Chunker.toChunkerConfig()
	cc.Seed = key.ChunkerSeed
	cc.IDKey = key.IDKey
	return &Repository{
		backend:     b,
		codec:       codec,
		logger:      cfg.logger,
		concurrency: cfg.concurrency,
		lockWait:    cfg.lockWait,
		progress:    cfg.progress,
		lockEvent:   cfg.lockEvent,
		id:          rc.ID,
		key:         key,
		keyName:     keyName,
		chunker:     cc,
	}, nil
}

// ChangePassword replaces the key file the repository was opened with by one
// sealed under pw, which may also be WithoutPassword. The master key stays,
// so no data is rewritten; for the same reason the change does not revoke
// access for anyone who could read the master key before, through the old
// password or a copy of the old key file.
//
// It takes the exclusive lock. The new key file is written before the old
// one is deleted: when the write fails the old password still opens the
// repository, when the delete fails both do and the error says so.
func (r *Repository) ChangePassword(ctx context.Context, pw Passphrase) error {
	if err := pw.validate(); err != nil {
		return err
	}
	ctx, lock, err := r.acquire(ctx, true)
	if err != nil {
		return err
	}
	defer lock.release(ctx)

	name, err := putKeyFile(ctx, r.backend, r.codec, r.key, pw)
	if err != nil {
		return err
	}
	// The old key goes only once the new one is durable: a power loss in
	// between must leave at least one key file that opens the repository.
	if err := r.backend.Sync(ctx); err != nil {
		return fmt.Errorf("ark: failed to sync the new key file %s: %w", name, err)
	}
	if err := r.backend.Delete(ctx, Handle{Type: KeyFile, Name: r.keyName}); err != nil {
		return fmt.Errorf(
			"ark: new key file %s written, failed to delete the old key file %s: %w",
			name,
			r.keyName,
			err,
		)
	}
	r.keyName = name
	return nil
}

// ID returns the repository id written by Init.
func (r *Repository) ID() string { return r.id }

// Backup walks src and stores its entries as a new snapshot.
//
// A cancelled backup leaves packs behind, no snapshot: the manifest is
// written last, so the snapshot never becomes visible. A later backup of the
// same data reuses the packs an index object lists already, which is what the
// backup stored up to its last checkpoint; the rest stays until Prune deletes
// it.
func (r *Repository) Backup(ctx context.Context, src Source, opts ...BackupOption) (*Snapshot, error) {
	o := &backupOptions{}
	for _, opt := range opts {
		opt(o)
	}
	o.progress = r.observer(OperationBackup, o.progress)

	ctx, lock, err := r.acquire(ctx, false)
	if err != nil {
		return nil, err
	}
	defer lock.release(ctx)

	if o.parent != "" {
		parent, resolveErr := r.resolveID(ctx, o.parent)
		if resolveErr != nil {
			return nil, resolveErr
		}
		o.parent = parent
	}
	m, err := r.runBackup(ctx, src, *o, r.writeSession())
	if err != nil {
		return nil, err
	}
	return manifestToSnapshot(m), nil
}

// Restore streams the snapshot into sink. id is a full snapshot id or a
// unique prefix.
func (r *Repository) Restore(ctx context.Context, id string, sink Sink, opts ...RestoreOption) (*RestoreResult, error) {
	ctx, lock, err := r.acquire(ctx, false)
	if err != nil {
		return nil, err
	}
	defer lock.release(ctx)

	id, err = r.resolveID(ctx, id)
	if err != nil {
		return nil, err
	}
	o := &restoreOptions{}
	for _, opt := range opts {
		opt(o)
	}
	o.progress = r.observer(OperationRestore, o.progress)
	stats, err := r.runRestore(ctx, id, sink, *o)
	if err != nil {
		return nil, err
	}
	return &RestoreResult{FilesRestored: stats.FilesRestored, BytesRestored: stats.BytesRestored}, nil
}

// resolveID turns a full snapshot id or a unique prefix into the full id.
// It returns ErrNotFound when nothing matches and ErrAmbiguousID, naming the
// candidates, when more than one does.
func (r *Repository) resolveID(ctx context.Context, id string) (string, error) {
	if id == "" {
		return "", fmt.Errorf("%w: empty snapshot id", ErrNotFound)
	}
	// An exact hit costs one Stat; only a prefix needs the full listing.
	if _, err := r.backend.Stat(ctx, Handle{Type: SnapshotFile, Name: id}); err == nil {
		return id, nil
	}

	var matches []string
	err := r.backend.List(ctx, SnapshotFile, func(info BlobInfo) error {
		if strings.HasPrefix(info.Name, id) {
			matches = append(matches, info.Name)
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("ark: failed to list snapshots: %w", err)
	}

	switch len(matches) {
	case 0:
		return "", fmt.Errorf("%w: %s", ErrNotFound, id)
	case 1:
		return matches[0], nil
	default:
		sort.Strings(matches)
		return "", fmt.Errorf("%w: %s matches %s", ErrAmbiguousID, id, strings.Join(matches, ", "))
	}
}

// Snapshots returns all snapshots matching filter, newest first.
func (r *Repository) Snapshots(ctx context.Context, filter SnapshotFilter) ([]*Snapshot, error) {
	if len(filter.IDs) > 0 {
		// Resolve into a copy: the caller's slice stays untouched.
		resolved := make([]string, len(filter.IDs))
		for i, id := range filter.IDs {
			full, err := r.resolveID(ctx, id)
			if err != nil {
				return nil, err
			}
			resolved[i] = full
		}
		filter.IDs = resolved
	}

	manifests, err := r.readManifests(ctx)
	if err != nil {
		return nil, err
	}

	var out []*Snapshot
	for _, m := range manifests {
		s := manifestToSnapshot(m)
		if !filter.Match(s) {
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

// Snapshot returns one snapshot. id is a full snapshot id or a unique prefix.
func (r *Repository) Snapshot(ctx context.Context, id string) (*Snapshot, error) {
	id, err := r.resolveID(ctx, id)
	if err != nil {
		return nil, err
	}
	m, err := r.loadManifest(ctx, id)
	if err != nil {
		return nil, err
	}
	return manifestToSnapshot(m), nil
}

// deleteSnapshot removes one manifest. Its objects stay in the CAS until
// Prune sweeps them, which is why this is not part of the public surface:
// callers go through Forget.
func (r *Repository) deleteSnapshot(ctx context.Context, id string) error {
	if err := r.backend.Delete(ctx, Handle{Type: SnapshotFile, Name: id}); err != nil {
		return fmt.Errorf("ark: failed to delete snapshot %s: %w", id, err)
	}
	return nil
}

// Forget removes the snapshots that policy does not keep among those matching
// filter. Chunks stay until Prune.
//
// A policy without a positive rule removes every match, so it is only
// accepted together with filter.IDs; otherwise Forget returns ErrEmptyPolicy
// and deletes nothing.
//
// Removing named ids without a policy reads only those manifests, and one
// that fails with ErrCorruptManifest is still removed, reported with its ID
// alone. This is how a corrupt manifest leaves the repository.
func (r *Repository) Forget(
	ctx context.Context,
	policy ForgetPolicy,
	filter SnapshotFilter,
	opts ...ForgetOption,
) (*ForgetResult, error) {
	if policy.keepsNothing() && len(filter.IDs) == 0 {
		return nil, ErrEmptyPolicy
	}

	cfg := &forgetConfig{}
	for _, o := range opts {
		o(cfg)
	}
	progress := r.observer(OperationForget, nil)
	progress(ProgressEvent{Phase: PhaseLoad})

	ctx, lock, err := r.acquire(ctx, false)
	if err != nil {
		return nil, err
	}
	defer lock.release(ctx)

	var result *ForgetResult
	if policy.keepsNothing() {
		result, err = r.forgetNamed(ctx, filter)
	} else {
		var snapshots []*Snapshot
		snapshots, err = r.Snapshots(ctx, filter)
		if err == nil {
			result = applyForgetPolicy(snapshots, policy)
		}
	}
	if err != nil {
		return nil, err
	}
	result.DryRun = cfg.dryRun
	done := ProgressEvent{Phase: PhaseFinalize, FilesTotal: len(result.Remove)}
	if cfg.dryRun {
		done.FilesDone = done.FilesTotal
		progress(done)
		return result, nil
	}

	// A failed removal does not stop the others; Forget still fails.
	var errs []error
	for _, s := range result.Remove {
		if err := r.deleteSnapshot(ctx, s.ID); err != nil {
			errs = append(errs, err)
			continue
		}
		done.FilesDone++
	}
	// A removed snapshot has to stay removed before a prune sweeps its
	// objects, so the removals are durable before Forget returns.
	if err := r.backend.Sync(ctx); err != nil {
		return nil, fmt.Errorf("forget: failed to sync snapshot removals: %w", err)
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("forget: %w", errors.Join(errs...))
	}
	progress(done)
	return result, nil
}

// forgetNamed selects the snapshots in filter.IDs for removal, loading only
// their manifests. The other filter fields still narrow the healthy ones.
func (r *Repository) forgetNamed(ctx context.Context, filter SnapshotFilter) (*ForgetResult, error) {
	rest := filter
	rest.IDs = nil
	result := &ForgetResult{}
	seen := make(map[string]bool, len(filter.IDs))
	for _, id := range filter.IDs {
		full, err := r.resolveID(ctx, id)
		if err != nil {
			return nil, err
		}
		if seen[full] {
			continue
		}
		seen[full] = true
		m, err := r.loadManifest(ctx, full)
		if errors.Is(err, ErrCorruptManifest) {
			result.Remove = append(result.Remove, &Snapshot{ID: full})
			continue
		}
		if err != nil {
			return nil, err
		}
		if s := manifestToSnapshot(m); rest.Match(s) {
			result.Remove = append(result.Remove, s)
		}
	}
	sortNewestFirst(result.Remove)
	return result, nil
}

// pruneConfig holds configuration for a Prune call.
type pruneConfig struct{ dryRun bool }

// PruneOption configures a Prune call.
type PruneOption func(*pruneConfig)

// WithPruneDryRun makes Prune report what would be removed without deleting
// anything.
func WithPruneDryRun() PruneOption { return func(c *pruneConfig) { c.dryRun = true } }

// Prune deletes the packs that hold no object a snapshot references, and the
// packs no index lists. A pack that still holds one referenced object stays
// whole.
//
// It deletes nothing when a snapshot references an object no index lists:
// the error names RebuildIndex, which restores the index from the packs.
// Pass WithPruneDryRun to report what would be removed without making changes.
func (r *Repository) Prune(ctx context.Context, opts ...PruneOption) (*PruneResult, error) {
	cfg := &pruneConfig{}
	for _, o := range opts {
		o(cfg)
	}
	progress := r.observer(OperationPrune, nil)
	progress(ProgressEvent{Phase: PhaseLoad})

	ctx, lock, err := r.acquire(ctx, true)
	if err != nil {
		return nil, err
	}
	defer lock.release(ctx)

	return r.runGC(ctx, cfg.dryRun, progress)
}

// RebuildIndex replaces the index of the repository with what the stored
// packs say about themselves: every pack is read whole, a new index is
// written and the old index objects are deleted. It is the repair for a lost
// or damaged index object, which Check and Prune report. It needs the
// repository to itself, like Prune.
//
// A pack whose own table does not open is named in the result. The new index
// lists in it what the old one did, so a rebuild never loses a blob the old
// index could locate, except for the blobs a pack that opens holds as well:
// those are read from there.
func (r *Repository) RebuildIndex(ctx context.Context) (*RebuildIndexResult, error) {
	ctx, lock, err := r.acquire(ctx, true)
	if err != nil {
		return nil, err
	}
	defer lock.release(ctx)

	return r.runRebuildIndex(ctx)
}

// Export writes a portable tar.gz archive of one snapshot to w. id is a full
// snapshot id or a unique prefix.
func (r *Repository) Export(ctx context.Context, id string, w io.Writer) error {
	ctx, lock, err := r.acquire(ctx, false)
	if err != nil {
		return err
	}
	defer lock.release(ctx)

	id, err = r.resolveID(ctx, id)
	if err != nil {
		return err
	}
	return r.runExport(ctx, id, w)
}

// Import reads a portable tar.gz archive from reader and re-indexes it.
func (r *Repository) Import(ctx context.Context, reader io.Reader) (*Snapshot, error) {
	ctx, lock, err := r.acquire(ctx, false)
	if err != nil {
		return nil, err
	}
	defer lock.release(ctx)

	m, err := r.runImport(ctx, reader, r.writeSession())
	if err != nil {
		return nil, err
	}
	return manifestToSnapshot(m), nil
}

// Verify checks one snapshot: manifest, tree and every chunk.
// id is a full snapshot id or a unique prefix.
func (r *Repository) Verify(ctx context.Context, id string) (*VerifyResult, error) {
	progress := r.observer(OperationVerify, nil)
	progress(ProgressEvent{Phase: PhaseLoad})

	ctx, lock, err := r.acquire(ctx, false)
	if err != nil {
		return nil, err
	}
	defer lock.release(ctx)

	id, err = r.resolveID(ctx, id)
	if err != nil {
		return nil, err
	}
	chunks, trees := r.stores()
	result, err := r.runVerify(ctx, id, chunks, trees, nil, progress)
	if err != nil {
		return result, err
	}
	progress(ProgressEvent{Phase: PhaseFinalize, CurrentPath: id})
	return result, nil
}

// Check verifies every snapshot in the repository, reading and authenticating
// every object they reference, and compares the index with the packs that are
// stored. An object several snapshots share is read once and what was found
// is reported for each of them. What it finds is in the result; the error is
// for a check that could not run.
func (r *Repository) Check(ctx context.Context) (*CheckResult, error) {
	progress := r.observer(OperationCheck, nil)
	progress(ProgressEvent{Phase: PhaseLoad})

	ctx, lock, err := r.acquire(ctx, false)
	if err != nil {
		return nil, err
	}
	defer lock.release(ctx)

	// Names, not manifests: a manifest that does not open is a finding of the
	// check, not a reason to stop it.
	var ids []string
	err = r.backend.List(ctx, SnapshotFile, func(info BlobInfo) error {
		ids = append(ids, info.Name)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("ark: failed to list snapshots: %w", err)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))

	_, unreadable, err := r.loadLocks(ctx)
	if err != nil {
		return nil, err
	}
	result := &CheckResult{SnapshotsChecked: len(ids)}
	for _, name := range unreadable {
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("lock %s does not open and is ignored, Unlock with WithForce removes it", name))
	}
	// One pair of stores for the check: the indexes load once and a tree pack
	// several snapshots share is fetched once.
	chunks, trees := r.stores()
	stores := []*objectStore{chunks, trees}
	for _, s := range stores {
		s.suspects = make(map[string][]packEntry)
		if err := s.checkPacks(ctx, result); err != nil {
			return nil, err
		}
	}
	// ponytail: one verdict per chunk of the repository stays in memory, about
	// 60 bytes each. Verify the union of all references pack by pack and keep
	// only the failures once a repository is too large for that.
	settled := make(map[[sha256.Size]byte]chunkVerdict)
	for i, id := range ids {
		// The inner verify reports as check, counters restart per snapshot.
		vr, err := r.runVerify(ctx, id, chunks, trees, settled, func(e ProgressEvent) {
			e.FilesDone, e.FilesTotal = i, len(ids)
			progress(e)
		})
		if ctx.Err() != nil {
			return nil, ctx.Err() //nolint:wrapcheck // the caller's own cancellation
		}
		progress(ProgressEvent{Phase: PhaseFetch, CurrentPath: id, FilesDone: i + 1, FilesTotal: len(ids)})
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("snapshot %s: %v", id, err))
			continue
		}
		for _, c := range vr.MissingChunks {
			result.Errors = append(result.Errors, fmt.Sprintf("snapshot %s: missing chunk %s", id, c))
		}
		for _, c := range vr.CorruptChunks {
			result.Errors = append(result.Errors, fmt.Sprintf("snapshot %s: corrupt chunk %s", id, c))
		}
	}
	for _, s := range stores {
		if err := s.checkSuspects(ctx, result); err != nil {
			return nil, err
		}
	}
	progress(ProgressEvent{Phase: PhaseFinalize, FilesDone: len(ids), FilesTotal: len(ids)})
	return result, nil
}

// Walk calls fn for every entry of a snapshot in tree order, parents before
// children, without materialising the list. id is a full snapshot id or a
// unique prefix. An error from fn ends the walk and is returned.
//
// Directory paths carry a trailing slash, the way tar names them; every other
// entry carries the plain path a Source yielded.
func (r *Repository) Walk(ctx context.Context, id string, fn func(FileInfo) error) error {
	id, err := r.resolveID(ctx, id)
	if err != nil {
		return err
	}
	m, err := r.loadManifest(ctx, id)
	if err != nil {
		return err
	}
	if m.RootTree == "" {
		return nil
	}
	_, trees := r.stores()
	return walkTree(ctx, trees, m.RootTree, func(e fileEntry) error { return fn(e.FileInfo) })
}

// Close releases any resources held by the repository (and the backend).
func (r *Repository) Close() error {
	r.codec.Close()
	return r.backend.Close()
}

// Match returns true if the snapshot matches all non-zero fields of f.
// IDs are compared verbatim; Snapshots resolves prefixes before calling this.
func (f SnapshotFilter) Match(s *Snapshot) bool {
	if len(f.IDs) > 0 && !slices.Contains(f.IDs, s.ID) {
		return false
	}
	if f.Name != "" && s.SourceName != f.Name {
		return false
	}
	if f.Type != "" && s.SourceType != f.Type {
		return false
	}
	if f.Host != "" && s.Host != f.Host {
		return false
	}
	if !f.Before.IsZero() && !s.CreatedAt.Before(f.Before) {
		return false
	}
	if !f.After.IsZero() && !s.CreatedAt.After(f.After) {
		return false
	}
	if len(f.Tags) > 0 {
		tagSet := make(map[string]struct{}, len(s.Tags))
		for _, t := range s.Tags {
			tagSet[t] = struct{}{}
		}
		for _, ft := range f.Tags {
			if _, ok := tagSet[ft]; !ok {
				return false
			}
		}
	}
	return true
}
