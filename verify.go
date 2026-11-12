package ark

import (
	"context"
	"errors"
	"fmt"
)

// runVerify checks one snapshot and reports a PhaseFetch event per chunk
// reference to progress, CurrentPath set to the snapshot ID. Callers emit the
// events that open and close their operation.
func (r *Repository) runVerify(
	ctx context.Context,
	snapshotID string,
	chunks, trees *objectStore,
	progress ProgressFunc,
) (*VerifyResult, error) {
	logger := r.logger.With("component", "verify", "snapshot", snapshotID)

	m, err := r.loadManifest(ctx, snapshotID)
	if err != nil {
		return nil, err
	}

	result := &VerifyResult{SnapshotID: snapshotID, OK: true}

	logger.InfoContext(ctx, "verifying snapshot", "chunks", m.Stats.ChunksTotal)

	// Streams the tree like restore does, holding one chunk at a time. The
	// set of ids already seen is hashes only, so it grows with the chunk
	// count and not with the size of the data. Both stores recompute the
	// name of every object they read, so a tree that opens but does not match
	// its name fails the walk.
	seen := make(map[string]struct{})
	chunks = chunks.verifying()
	done := 0
	err = walkSnapshot(ctx, trees.verifying(), m, pathFilter{}, func(fe fileEntry) error {
		for _, ref := range fe.Chunks {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			if _, dup := seen[ref.ID]; !dup {
				seen[ref.ID] = struct{}{}
				if err := verifyChunk(ctx, chunks, ref.ID, result); err != nil {
					return err
				}
			}
			done++
			progress(ProgressEvent{
				Phase:       PhaseFetch,
				CurrentPath: snapshotID,
				ChunksDone:  done,
				ChunksTotal: m.Stats.ChunksTotal,
			})
		}
		return nil
	})
	// verifyChunk records chunk failures, so an error here wrapping
	// ErrCorruptTree is a tree that does not open or match its name. A walk
	// that did not finish verified nothing past where it stopped.
	if err != nil {
		result.OK = false
		return result, err
	}

	if result.OK {
		logger.InfoContext(ctx, "verification passed", "verified", result.ChunksChecked)
	} else {
		logger.WarnContext(ctx, "verification found issues",
			"verified", result.ChunksChecked,
			"missing", len(result.MissingChunks),
			"corrupt", len(result.CorruptChunks),
		)
	}
	return result, nil
}

// verifyChunk records one chunk as checked, missing or corrupt. Only a backend
// failure that says nothing about the chunk itself is returned as an error.
func verifyChunk(ctx context.Context, chunks *objectStore, id string, result *VerifyResult) error {
	// A chunk no index lists, or whose pack is gone, is missing. One that
	// fails its tag or does not hash to its name is corrupt.
	_, err := chunks.Get(ctx, id)
	switch {
	case err == nil:
		result.ChunksChecked++
		return nil
	case errors.Is(err, ErrNotFound):
		result.MissingChunks = append(result.MissingChunks, id)
	case errors.Is(err, ErrCorruptChunk):
		result.CorruptChunks = append(result.CorruptChunks, id)
	default:
		return fmt.Errorf("verify: failed to read chunk %s: %w", id, err)
	}
	result.OK = false
	return nil
}
