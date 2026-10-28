package ark

import (
	"context"
	"fmt"
)

func (r *Repository) runGC(ctx context.Context, dryRun bool) (*PruneResult, error) {
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

	result := &PruneResult{DryRun: dryRun}
	scanned := 0
	for _, ns := range []struct {
		store *objectStore
		live  map[string]struct{}
	}{
		{r.chunks, liveChunks},
		{r.trees, liveTrees},
	} {
		n, err := r.sweep(ctx, ns.store, ns.live, dryRun, result)
		if err != nil {
			return nil, err
		}
		scanned += n
	}

	logger.InfoContext(ctx, "gc complete",
		"scanned", scanned,
		"live", len(liveChunks)+len(liveTrees),
		"deleted", result.BlobsRemoved,
		"freed_bytes", result.BytesFreed,
		"dry_run", dryRun,
	)
	return result, nil
}

// sweep deletes every object of store that is absent from live and adds what
// it removed to result. It returns the number of objects it scanned.
func (r *Repository) sweep(
	ctx context.Context,
	store *objectStore,
	live map[string]struct{},
	dryRun bool,
	result *PruneResult,
) (int, error) {
	names, err := store.Names(ctx)
	if err != nil {
		return 0, fmt.Errorf("gc: failed to enumerate objects: %w", err)
	}

	for _, name := range names {
		if _, ok := live[name]; ok {
			continue
		}

		size, err := store.StoredSize(ctx, name)
		if err == nil {
			result.BytesFreed += size
		}

		if !dryRun {
			if err := store.Delete(ctx, name); err != nil {
				r.logger.WarnContext(ctx, "gc: failed to delete orphaned object", "name", name, "error", err)
				continue
			}
		}
		result.BlobsRemoved++
	}
	return len(names), nil
}
