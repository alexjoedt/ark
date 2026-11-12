package ark

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
)

// prunePlan is what a prune does to the packs and the index of one blob
// type.
type prunePlan struct {
	store *objectStore
	table *indexTable
	// referenced marks the packs of table.packs that hold a referenced blob
	// and stay, blobs counts what the index lists in each. The index is
	// written from the packs that stay when rewrite is set.
	referenced []bool
	blobs      []int
	rewrite    bool
	// stored is every pack in the namespace. remove names those of them that
	// no index lists or that hold no referenced blob, with the number of
	// blobs the index lists in each: none for a pack it does not list.
	stored []BlobInfo
	remove map[string]int
}

// runGC deletes the packs without a referenced blob and the packs no index
// lists, and rewrites the index without them. Nothing is deleted before
// every snapshot was walked and every blob it references was found in the
// index: a prune that trusted an incomplete index would delete packs whose
// index object was lost.
//
// The order is what a kill at any point leaves readable: the complete new
// index, Sync, the old index objects, Sync, then the packs. Until the last
// step every listed pack is stored; after a kill in it the index lists
// nothing that is gone, and the next prune removes the packs left over.
func (r *Repository) runGC(ctx context.Context, dryRun bool, progress ProgressFunc) (*PruneResult, error) {
	logger := r.logger.With("component", "gc")

	manifests, err := r.readManifests(ctx)
	if err != nil {
		return nil, fmt.Errorf("gc: failed to list manifests: %w", err)
	}

	// A snapshot whose tree cannot be read stops the prune: an incomplete
	// live set would delete its objects.
	chunks, trees := r.stores()
	liveTrees := make(map[string]struct{})
	liveChunks := make(map[string]struct{})
	for _, m := range manifests {
		if m.RootTree == "" {
			continue
		}
		if err := collectLiveObjects(ctx, trees, m.RootTree, liveTrees, liveChunks); err != nil {
			return nil, fmt.Errorf("gc: snapshot %s: %w", m.ID, notIndexedHint(err))
		}
	}
	for name := range liveChunks {
		_, _, ok, err := chunks.index.lookup(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("gc: %w", err)
		}
		if !ok {
			err := fmt.Errorf("%w: chunk %s: %w", ErrNotFound, name, errNotIndexed)
			return nil, fmt.Errorf("gc: %w", notIndexedHint(err))
		}
	}

	plans := []*prunePlan{{store: chunks}, {store: trees}}
	scanned := 0
	for i, live := range []map[string]struct{}{liveChunks, liveTrees} {
		if err := plans[i].plan(ctx, live); err != nil {
			return nil, fmt.Errorf("gc: %w", err)
		}
		scanned += len(plans[i].stored)
	}

	result := &PruneResult{DryRun: dryRun}
	ev := ProgressEvent{Phase: PhaseSweep, ChunksTotal: scanned}
	progress(ev)
	if !dryRun {
		if err := r.rewriteIndexes(ctx, plans); err != nil {
			return nil, fmt.Errorf("gc: %w", err)
		}
	}
	for _, p := range plans {
		if err := p.sweep(ctx, r, dryRun, result, func() {
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

// notIndexedHint says what to do about a referenced object no index lists.
func notIndexedHint(err error) error {
	if !errors.Is(err, errNotIndexed) {
		return err
	}
	return fmt.Errorf("%w: a snapshot references it, so an index object was lost or does not open; "+
		"nothing was deleted, RebuildIndex restores the index from the packs", err)
}

// plan loads the index and lists the packs of p's namespace, and decides
// what goes: live names every referenced blob of the type. A pack stays
// while the index lists a referenced blob in it, also when that is a second
// copy of the blob.
func (p *prunePlan) plan(ctx context.Context, live map[string]struct{}) error {
	x := p.store.index
	t, err := x.load(ctx)
	if err != nil {
		return err
	}
	p.table = t

	p.referenced = make([]bool, len(t.packs))
	p.blobs = make([]int, len(t.packs))
	var name [2 * sha256.Size]byte
	for _, records := range &t.records {
		for _, rec := range records {
			p.blobs[rec.pack]++
			hex.Encode(name[:], rec.name[:])
			if _, ok := live[string(name[:])]; ok {
				p.referenced[rec.pack] = true
			}
		}
	}
	dead := make(map[string]int)
	kept := 0
	for i, pack := range t.packs {
		if p.referenced[i] {
			kept += p.blobs[i]
			continue
		}
		dead[hex.EncodeToString(pack[:])] = p.blobs[i]
	}
	// One object per indexMaxRecords blobs is what a rewrite leaves at most.
	p.rewrite = len(dead) > 0 || len(t.unreadable) > 0 ||
		len(t.objects) > (kept+indexMaxRecords-1)/indexMaxRecords

	p.remove = make(map[string]int)
	err = p.store.backend.List(ctx, x.space.packs, func(info BlobInfo) error {
		p.stored = append(p.stored, info)
		blobs, isDead := dead[info.Name]
		sum, ok := parseName(info.Name)
		_, listed := slices.BinarySearchFunc(t.packs, sum, compareNames)
		if isDead || !ok || !listed {
			p.remove[info.Name] = blobs
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("ark: failed to list %s objects: %w", x.space.packs, err)
	}
	return nil
}

// keep returns the packs that stay, each with every blob the index lists in
// it. That is a second copy of their records, so only a rewrite asks for it.
func (p *prunePlan) keep() []indexedPack {
	entries := make([][]packEntry, len(p.table.packs))
	for i, n := range p.blobs {
		if p.referenced[i] {
			entries[i] = make([]packEntry, 0, n)
		}
	}
	for _, records := range &p.table.records {
		for _, rec := range records {
			if p.referenced[rec.pack] {
				entries[rec.pack] = append(entries[rec.pack], rec.packEntry)
			}
		}
	}
	var keep []indexedPack
	for i, pack := range p.table.packs {
		if p.referenced[i] {
			keep = append(keep, indexedPack{name: hex.EncodeToString(pack[:]), entries: entries[i]})
		}
	}
	return keep
}

// rewriteIndexes stores the index of the packs that stay and then deletes
// the index objects it replaces, each step made durable before the next.
func (r *Repository) rewriteIndexes(ctx context.Context, plans []*prunePlan) error {
	written := make([][]string, len(plans))
	rewrite := false
	for i, p := range plans {
		if !p.rewrite {
			continue
		}
		rewrite = true
		// A fresh index: one that is loaded would merge what it writes into
		// its table, which nothing reads here.
		names, err := newPackIndex(r.backend, p.store.space, r.logger).write(ctx, p.keep())
		if err != nil {
			return err
		}
		written[i] = names
	}
	if !rewrite {
		return nil
	}
	if err := r.backend.Sync(ctx); err != nil {
		return fmt.Errorf("failed to sync the new index: %w", err)
	}
	for i, p := range plans {
		if !p.rewrite {
			continue
		}
		for _, name := range p.table.objects {
			// An object named like a new one is the new one: the name is
			// the hash of the content. One that does not open goes last,
			// with the packs.
			if slices.Contains(written[i], name) || slices.Contains(p.table.unreadable, name) {
				continue
			}
			h := Handle{Type: p.store.space.index, Name: name}
			if err := r.backend.Delete(ctx, h); err != nil {
				return fmt.Errorf("failed to delete %s object %s: %w", h.Type, name, err)
			}
		}
	}
	// Without this barrier a power loss could bring an old index object
	// back after the packs it lists were deleted.
	if err := r.backend.Sync(ctx); err != nil {
		return fmt.Errorf("failed to sync the index: %w", err)
	}
	return nil
}

// sweep deletes the packs of p.remove and the index objects that do not
// open, and adds what it removed to result. It calls scanned once per stored
// pack. A pack that fails to delete stays for the next prune, which finds it
// listed nowhere.
func (p *prunePlan) sweep(
	ctx context.Context, r *Repository, dryRun bool, result *PruneResult, scanned func(),
) error {
	space := p.store.space
	for _, info := range p.stored {
		if err := ctx.Err(); err != nil {
			return err //nolint:wrapcheck // the caller's own cancellation
		}
		blobs, remove := p.remove[info.Name]
		if remove && !dryRun {
			if err := r.backend.Delete(ctx, Handle{Type: space.packs, Name: info.Name}); err != nil {
				r.logger.WarnContext(ctx, "gc: failed to delete an unreferenced pack",
					"namespace", space.packs.String(), "name", info.Name, "error", err)
				remove = false
			}
		}
		if remove {
			result.BlobsRemoved += blobs
			result.BytesFreed += info.Size
		}
		scanned()
	}
	if dryRun {
		return nil
	}
	for _, name := range p.table.unreadable {
		if err := r.backend.Delete(ctx, Handle{Type: space.index, Name: name}); err != nil {
			r.logger.WarnContext(ctx, "gc: failed to delete an index object that does not open",
				"namespace", space.index.String(), "name", name, "error", err)
		}
	}
	return nil
}
