package ark

import (
	"context"
	"fmt"
)

func (r *Repository) runGC(ctx context.Context, dryRun bool, progress ProgressFunc) (*PruneResult, error) {
	logger := r.logger.With("component", "gc")

	manifests, err := r.readManifests(ctx)
	if err != nil {
		return nil, fmt.Errorf("gc: failed to list manifests: %w", err)
	}

	// Trees and chunks live in separate namespaces, so each namespace is
	// swept against its own live set. A snapshot whose tree cannot be read
	// stops the prune: an incomplete live set would delete its objects.
	liveTrees := make(map[string]struct{})
	liveChunks := make(map[string]struct{})
	for _, m := range manifests {
		if m.RootTree == "" {
			continue
		}
		if err := collectLiveObjects(ctx, r.trees, m.RootTree, liveTrees, liveChunks); err != nil {
			return nil, fmt.Errorf("gc: snapshot %s: %w", m.ID, err)
		}
	}

	// Both namespaces are listed before the sweep starts, so the sweep
	// reports one total.
	type namespace struct {
		store *objectStore
		live  map[string]struct{}
		names []string
	}
	spaces := []namespace{{store: r.chunks, live: liveChunks}, {store: r.trees, live: liveTrees}}
	scanned := 0
	for i := range spaces {
		names, err := spaces[i].store.Names(ctx)
		if err != nil {
			return nil, fmt.Errorf("gc: failed to enumerate objects: %w", err)
		}
		spaces[i].names = names
		scanned += len(names)
	}

	result := &PruneResult{DryRun: dryRun}
	ev := ProgressEvent{Phase: PhaseSweep, ChunksTotal: scanned}
	progress(ev)
	for _, ns := range spaces {
		if err := r.sweep(ctx, ns.store, ns.names, ns.live, dryRun, result, func() {
			ev.ChunksDone++
			progress(ev)
		}); err != nil {
			return nil, err
		}
	}
	if !dryRun {
		if err := r.backend.Sync(ctx); err != nil {
			return nil, fmt.Errorf("gc: failed to sync deletions: %w", err)
		}
		n, err := r.removeStaleMarkers(ctx)
		if err != nil {
			return nil, fmt.Errorf("gc: %w", err)
		}
		if n > 0 {
			logger.InfoContext(ctx, "gc removed the markers of unfinished backups", "markers", n)
		}
	}

	logger.InfoContext(ctx, "gc complete",
		"scanned", scanned,
		"live", len(liveChunks)+len(liveTrees),
		"deleted", result.BlobsRemoved,
		"freed_bytes", result.BytesFreed,
		"dry_run", dryRun,
	)
	ev.Phase = PhaseFinalize
	progress(ev)
	return result, nil
}

// sweep deletes every object of names that is absent from live and adds what
// it removed to result. It calls scanned once per name.
func (r *Repository) sweep(
	ctx context.Context,
	store *objectStore,
	names []string,
	live map[string]struct{},
	dryRun bool,
	result *PruneResult,
	scanned func(),
) error {
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return err //nolint:wrapcheck // the caller's own cancellation
		}
		if _, ok := live[name]; !ok {
			r.sweepOne(ctx, store, name, dryRun, result)
		}
		scanned()
	}
	return nil
}

func (r *Repository) sweepOne(ctx context.Context, store *objectStore, name string, dryRun bool, result *PruneResult) {
	size, err := store.StoredSize(ctx, name)
	if err != nil {
		r.logger.WarnContext(ctx, "gc: failed to size orphaned object, freed bytes undercounted",
			"name", name, "error", err)
	}

	if !dryRun {
		if err := store.Delete(ctx, name); err != nil {
			r.logger.WarnContext(ctx, "gc: failed to delete orphaned object", "name", name, "error", err)
			return
		}
	}
	result.BlobsRemoved++
	result.BytesFreed += size
}
