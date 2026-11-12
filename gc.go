package ark

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
)

// pruneUnusedPercent is the share of the stored blob bytes a prune leaves
// unused at most: blobs no snapshot references and second copies of blobs, in
// the packs it keeps.
const pruneUnusedPercent = 5

// packUse is what a prune knows about one pack the index lists.
type packUse struct {
	// blobs and bytes are what the index lists in the pack, unreferenced the
	// bytes of its blobs no snapshot references.
	blobs               int
	bytes, unreferenced int64
	// liveBlobs and live are the referenced blobs that count as stored here:
	// a blob in several packs is live in one of them.
	liveBlobs int
	live      int64
	// stored says the pack exists. repack says its live blobs are copied into
	// a new pack and it is deleted.
	stored, repack bool
}

// unused returns the bytes a repack of the pack gives back.
func (u *packUse) unused() int64 { return u.bytes - u.live }

// share returns n as a share of the pack's bytes.
func (u *packUse) share(n int64) float64 { return float64(n) / float64(u.bytes) }

// prunePlan is what a prune does to the packs and the index of one blob
// type.
type prunePlan struct {
	store *objectStore
	// live names every referenced blob of the type.
	live  map[string]struct{}
	table *indexTable
	// use describes the packs of table.packs, by their number.
	use []packUse
	// stored is every pack in the namespace.
	stored []BlobInfo
	// twice says a referenced blob is listed in more than one pack.
	twice bool
	// repacked lists the packs a repack stored.
	repacked []indexedPack
}

// runGC deletes the packs without a live blob and the packs no index lists,
// copies the live blobs of partly dead packs into new packs until the unused
// bytes fit the budget, and rewrites the index. Nothing is deleted before
// every snapshot was walked and every blob it references was found in the
// index: a prune that trusted an incomplete index would delete packs whose
// index object was lost.
//
// The order is what a kill at any point leaves readable: the new packs, Sync,
// the complete new index, Sync, the old index objects, Sync, then the old
// packs. Until the last step every listed pack is stored; after a kill in it
// the index lists nothing that is gone, and the next prune removes the packs
// left over. A kill before it leaves blobs listed in an old pack and a new
// one, of which the next prune keeps the new one (liveCopy), once it has read
// it (reads).
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

	plans := []*prunePlan{{store: chunks, live: liveChunks}, {store: trees, live: liveTrees}}
	scanned := 0
	for _, p := range plans {
		if err := p.plan(ctx); err != nil {
			return nil, fmt.Errorf("gc: %w", err)
		}
		scanned += len(p.stored)
	}
	result := &PruneResult{DryRun: dryRun}
	result.BytesRepacked, result.BytesUnused = chooseRepacks(plans)

	if !dryRun {
		indexed, err := r.repack(ctx, plans, result.BytesRepacked, progress)
		if err != nil {
			return nil, fmt.Errorf("gc: %w", err)
		}
		if err := r.rewriteIndexes(ctx, plans, indexed); err != nil {
			return nil, fmt.Errorf("gc: %w", err)
		}
	}
	ev := ProgressEvent{Phase: PhaseSweep, ChunksTotal: scanned}
	progress(ev)
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
		"packs_deleted", result.PacksDeleted,
		"freed_bytes", result.BytesFreed,
		"repacked_bytes", result.BytesRepacked,
		"unused_bytes", result.BytesUnused,
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

// plan loads the index, lists the packs of p's namespace and counts what
// every listed pack holds.
func (p *prunePlan) plan(ctx context.Context) error {
	x := p.store.index
	t, err := x.load(ctx)
	if err != nil {
		return err
	}
	p.table = t

	p.use = make([]packUse, len(t.packs))
	// Before the blobs are counted: liveCopy asks which packs are stored.
	err = p.store.backend.List(ctx, x.space.packs, func(info BlobInfo) error {
		p.stored = append(p.stored, info)
		if i, listed := t.pack(info.Name); listed {
			p.use[i].stored = true
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("ark: failed to list %s objects: %w", x.space.packs, err)
	}

	p.eachBlob(func(copies []indexRecord, referenced bool) {
		for _, rec := range copies {
			u := &p.use[rec.pack]
			u.blobs++
			u.bytes += int64(rec.length)
			if !referenced {
				u.unreferenced += int64(rec.length)
			}
		}
	})
	// Only once every pack knows its unreferenced bytes: liveCopy compares them.
	p.eachBlob(func(copies []indexRecord, referenced bool) {
		if referenced {
			rec := p.liveCopy(copies)
			p.use[rec.pack].liveBlobs++
			p.use[rec.pack].live += int64(rec.length)
			p.twice = p.twice || len(copies) > 1
		}
	})
	return nil
}

// eachBlob calls fn once per blob the index lists, with every record of it
// and whether a snapshot references the blob.
func (p *prunePlan) eachBlob(fn func(copies []indexRecord, referenced bool)) {
	var name [2 * sha256.Size]byte
	for _, records := range &p.table.records {
		for len(records) > 0 {
			n := 1
			for n < len(records) && records[n].name == records[0].name {
				n++
			}
			hex.Encode(name[:], records[0].name[:])
			_, referenced := p.live[string(name[:])]
			fn(records[:n], referenced)
			records = records[n:]
		}
	}
}

// liveCopy returns the record a referenced blob counts as stored at: of its
// copies the one in the pack with the smallest share of unreferenced bytes,
// of several such the one in the pack with the smallest name. The output of
// an interrupted repack holds no unreferenced blob, so it wins over the pack
// it was copied from, which then goes without being read again. A copy in a
// pack that is lost counts only when no stored pack holds the blob: counted
// there, the blob would be live in no pack the prune keeps.
func (p *prunePlan) liveCopy(copies []indexRecord) indexRecord {
	// Records are in the order of their pack names.
	return slices.MinFunc(copies, func(a, b indexRecord) int {
		ua, ub := &p.use[a.pack], &p.use[b.pack]
		switch {
		case ua.stored == ub.stored:
			return cmp.Compare(ua.share(ua.unreferenced), ub.share(ub.unreferenced))
		case ua.stored:
			return -1
		default:
			return 1
		}
	})
}

// chooseRepacks marks the packs a prune rewrites and returns the bytes of
// the blobs it copies for that and the unused bytes it leaves. A tree pack
// with an unreferenced node is always rewritten: tree packs are read whole.
// Data packs are rewritten in the order of their unused share until the unused
// bytes they hold fit pruneUnusedPercent of the stored blob bytes. A pack
// whose only unused bytes are second copies is never chosen: no rewrite frees
// them, so they are reported and do not count against the budget, which would
// otherwise have every partly dead pack rewritten for nothing. A pack that is
// lost is not chosen and not counted: its entries stay for Check to report.
func chooseRepacks(plans []*prunePlan) (repacked, unused int64) {
	// kept is the unused bytes of the packs that are never chosen.
	var live, kept int64
	var data []*packUse
	for _, p := range plans {
		for i := range p.use {
			u := &p.use[i]
			if u.live == 0 || !u.stored {
				continue
			}
			live += u.live
			switch {
			case u.unreferenced == 0:
				kept += u.unused()
			case p.store.space.blobs == TreeFile:
				u.repack = true
				repacked += u.live
			default:
				unused += u.unused()
				data = append(data, u)
			}
		}
	}
	slices.SortStableFunc(data, func(a, b *packUse) int {
		return cmp.Compare(b.share(b.unused()), a.share(a.unused()))
	})
	for _, u := range data {
		if unused*100 <= pruneUnusedPercent*(live+unused+kept) {
			break
		}
		u.repack = true
		repacked += u.live
		unused -= u.unused()
	}
	return repacked, unused + kept
}

// repack copies the live blobs of the packs chosen for it into new packs,
// which it makes durable and lists in the index, and returns the index
// objects it stored for that, data then tree. The blobs are copied as they
// are sealed, each one authenticated first: one that does not open ends the
// prune, with nothing deleted yet.
func (r *Repository) repack(
	ctx context.Context, plans []*prunePlan, total int64, progress ProgressFunc,
) ([2][]string, error) {
	// The session of a backup: packs, checkpoints, Sync before every index
	// object. What it lists in the index is listed in the old packs too until
	// the index is rewritten.
	session := r.writeSession()
	err := r.readAndCopy(ctx, session, plans, total, progress)
	if err != nil {
		// The packs stored so far are listed, so that the next prune counts
		// their blobs as copied. Unlisted they would be stored again by every
		// prune that fails the same way, none of which gets to delete them.
		if lerr := session.flushIndex(ctx, true); lerr != nil {
			r.logger.WarnContext(ctx, "gc: failed to list the packs of a repack that failed", "error", lerr)
		}
	} else {
		err = session.finish(ctx)
	}
	if err != nil {
		return [2][]string{}, fmt.Errorf("repack: %w", err)
	}
	return session.indexed, nil
}

// readAndCopy reads what p.reads names of every plan, opens the seal of each
// blob and adds those of a pack chosen for a repack to the writers of
// session.
func (r *Repository) readAndCopy(
	ctx context.Context, session *packSession, plans []*prunePlan, total int64, progress ProgressFunc,
) error {
	ev := ProgressEvent{Phase: PhaseStore, BytesTotal: total}
	if total > 0 {
		progress(ev)
	}
	for i, p := range plans {
		w := []*objectStore{session.chunks, session.trees}[i].writer
		stored := w.stored
		// One add at a time, so one call at a time.
		w.stored = func(ctx context.Context, pack string, entries []packEntry) error {
			p.repacked = append(p.repacked, indexedPack{name: pack, entries: entries})
			return stored(ctx, pack, entries)
		}
		// ponytail: one pack after the other, each read before it is written.
		// Read ahead with the repository's concurrency once a prune over a
		// slow backend shows the wait.
		reads := p.reads()
		for n, pack := range p.table.packs {
			entries, u := reads[uint32(n)], &p.use[n]
			if len(entries) == 0 {
				continue
			}
			slices.SortFunc(entries, func(a, b packEntry) int { return cmp.Compare(a.offset, b.offset) })
			name := hex.EncodeToString(pack[:])
			err := p.store.sealedMany(ctx, name, entries, func(e packEntry, sealed []byte, err error) error {
				if err == nil {
					err = p.store.space.checkSeal(name, e, sealed)
				}
				if err != nil || !u.repack {
					return err
				}
				return w.add(ctx, hex.EncodeToString(e.name[:]), sealed)
			})
			if err != nil {
				if !u.repack && errors.Is(err, p.store.space.corruptErr) {
					err = fmt.Errorf("%w: another pack holds a copy of it that this prune would delete; "+
						"nothing was deleted, and RebuildIndex lists the blob there alone if this pack was changed", err)
				}
				return err
			}
			if u.repack {
				ev.BytesDone += u.live
				progress(ev)
			}
		}
	}
	return nil
}

// reads returns the blobs a prune reads before it deletes a pack, by the
// number of their pack: the live blobs of the packs chosen for a repack,
// which are copied, and of the packs that stay the live copy of every blob
// with another copy in a pack that goes. That copy is the only one afterwards
// and nothing has read it since it was stored, so the other one goes only
// once this one opened.
func (p *prunePlan) reads() map[uint32][]packEntry {
	if !p.twice && !slices.ContainsFunc(p.use, func(u packUse) bool { return u.repack }) {
		return nil
	}
	goes := func(rec indexRecord) bool {
		u := &p.use[rec.pack]
		return u.stored && (u.live == 0 || u.repack)
	}
	reads := make(map[uint32][]packEntry)
	p.eachBlob(func(copies []indexRecord, referenced bool) {
		if !referenced {
			return
		}
		rec := p.liveCopy(copies)
		if u := &p.use[rec.pack]; u.repack || u.stored && slices.ContainsFunc(copies, goes) {
			reads[rec.pack] = append(reads[rec.pack], rec.packEntry)
		}
	})
	return reads
}

// rewrite reports whether the index is written again: when a listed pack
// goes, an index object does not open, or more objects exist than a rewrite
// leaves, one per indexMaxRecords blobs.
func (p *prunePlan) rewrite() bool {
	kept := 0
	for i := range p.use {
		if p.use[i].live == 0 || p.use[i].repack {
			return true
		}
		kept += p.use[i].blobs
	}
	return len(p.table.unreadable) > 0 || len(p.table.objects) > (kept+indexMaxRecords-1)/indexMaxRecords
}

// indexSet is the index of one blob type as a prune or a rebuild leaves it:
// packs returns what the new index objects list, old names the objects they
// replace.
type indexSet struct {
	space *packSpace
	packs func() []indexedPack
	old   []string
}

// rewriteIndexes stores the index of the packs that stay and of those a
// repack stored, and deletes the index objects it replaces: those the prune
// loaded and indexed, those the repack stored, data then tree. An object that
// does not open goes last, with the packs.
func (r *Repository) rewriteIndexes(ctx context.Context, plans []*prunePlan, indexed [2][]string) error {
	var sets []indexSet
	for i, p := range plans {
		if !p.rewrite() {
			continue
		}
		old := slices.DeleteFunc(slices.Clone(p.table.objects), func(name string) bool {
			return slices.Contains(p.table.unreadable, name)
		})
		sets = append(sets, indexSet{
			space: p.store.space,
			packs: func() []indexedPack {
				keep := make([]bool, len(p.use))
				for n := range p.use {
					keep[n] = p.use[n].live > 0 && !p.use[n].repack
				}
				return append(p.table.listed(keep), p.repacked...)
			},
			old: append(old, indexed[i]...),
		})
	}
	return r.replaceIndexes(ctx, sets)
}

// replaceIndexes stores the index objects of every set and then deletes the
// objects they replace, each step made durable before the next: at any point
// a kill leaves an index that lists every blob the new one does.
func (r *Repository) replaceIndexes(ctx context.Context, sets []indexSet) error {
	if len(sets) == 0 {
		return nil
	}
	written := make([][]string, len(sets))
	for i, set := range sets {
		// A fresh index: one that is loaded would merge what it writes into
		// its table, which nothing reads here.
		names, err := newPackIndex(r.backend, set.space, r.logger).write(ctx, set.packs())
		if err != nil {
			return err
		}
		written[i] = names
	}
	if err := r.backend.Sync(ctx); err != nil {
		return fmt.Errorf("failed to sync the new index: %w", err)
	}
	for i, set := range sets {
		for _, name := range set.old {
			// An object named like a new one is the new one: the name is
			// the hash of the content.
			if slices.Contains(written[i], name) {
				continue
			}
			h := Handle{Type: set.space.index, Name: name}
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

// runRebuildIndex replaces the index of both blob types with what the stored
// packs say about themselves.
func (r *Repository) runRebuildIndex(ctx context.Context) (*RebuildIndexResult, error) {
	result := &RebuildIndexResult{}
	var sets []indexSet
	chunks, trees := r.stores()
	for _, s := range []*objectStore{chunks, trees} {
		old, err := s.index.load(ctx)
		if err != nil {
			return nil, fmt.Errorf("rebuild index: %w", err)
		}
		packs, unreadable, err := s.index.scanPacks(ctx)
		if err != nil {
			return nil, fmt.Errorf("rebuild index: %w", err)
		}
		// A pack that does not open describes none of its blobs, and no later
		// rebuild could: what the old index says about it is all that still
		// locates the blobs in it that are intact.
		keep := make([]bool, len(old.packs))
		for _, name := range unreadable {
			if i, listed := old.pack(name); listed {
				keep[i] = true
			}
		}
		packs = append(packs, withoutCopies(old.listed(keep), packs)...)

		result.PacksIndexed += len(packs)
		for _, p := range packs {
			result.BlobsIndexed += len(p.entries)
		}
		result.UnreadablePacks = append(result.UnreadablePacks, unreadable...)
		sets = append(sets, indexSet{
			space: s.space,
			packs: func() []indexedPack { return packs },
			old:   old.objects,
		})
	}
	if err := r.replaceIndexes(ctx, sets); err != nil {
		return nil, fmt.Errorf("rebuild index: %w", err)
	}
	r.logger.InfoContext(ctx, "index rebuilt",
		"packs", result.PacksIndexed,
		"blobs", result.BlobsIndexed,
		"unreadable_packs", len(result.UnreadablePacks),
	)
	return result, nil
}

// withoutCopies returns kept, the packs that do not open with what the old
// index lists in them, without the blobs a pack of readable holds as well. A
// lookup resolves a blob in several packs to the pack with the smallest name,
// so an entry kept for a damaged pack would hide the copy that still opens.
func withoutCopies(kept, readable []indexedPack) []indexedPack {
	copied := make(map[[sha256.Size]byte]bool)
	for _, p := range kept {
		for _, e := range p.entries {
			copied[e.name] = false
		}
	}
	if len(copied) == 0 {
		return kept
	}
	for _, p := range readable {
		for _, e := range p.entries {
			if _, ok := copied[e.name]; ok {
				copied[e.name] = true
			}
		}
	}
	for i := range kept {
		kept[i].entries = slices.DeleteFunc(kept[i].entries, func(e packEntry) bool { return copied[e.name] })
	}
	// A pack left without a blob is no longer listed: Prune removes it.
	return slices.DeleteFunc(kept, func(p indexedPack) bool { return len(p.entries) == 0 })
}

// sweep deletes the packs that hold no live blob, those a repack copied from
// and those no index lists, then the index objects that do not open, and adds
// what it removed to result. It calls scanned once per stored pack. A pack
// that fails to delete stays for the next prune, which finds it listed
// nowhere.
func (p *prunePlan) sweep(
	ctx context.Context, r *Repository, dryRun bool, result *PruneResult, scanned func(),
) error {
	space := p.store.space
	for _, info := range p.stored {
		if err := ctx.Err(); err != nil {
			return err //nolint:wrapcheck // the caller's own cancellation
		}
		// A pack no index lists goes with its bytes only.
		blobs, copied, remove := 0, int64(0), true
		if i, listed := p.table.pack(info.Name); listed {
			u := &p.use[i]
			switch {
			case u.live == 0:
				blobs = u.blobs
			case u.repack:
				blobs, copied = u.blobs-u.liveBlobs, u.live
			default:
				remove = false
			}
		}
		if remove && !dryRun {
			if err := r.backend.Delete(ctx, Handle{Type: space.packs, Name: info.Name}); err != nil {
				r.logger.WarnContext(ctx, "gc: failed to delete an unreferenced pack",
					"namespace", space.packs.String(), "name", info.Name, "error", err)
				remove = false
			}
		}
		if remove {
			result.PacksDeleted++
			result.BlobsRemoved += blobs
			result.BytesFreed += info.Size - copied
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
