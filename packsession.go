package ark

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
	"time"
)

// checkpointInterval is the longest a stored pack waits for its index object
// while a run goes on: what an interrupted run uploaded before its last
// checkpoint, the next run finds in the index and does not upload again.
const checkpointInterval = 10 * time.Minute

// heldBlobs is the set of blobs a run put into packs. No index lists them
// before the run's next checkpoint, so the run asks here first.
//
// ponytail: the set keeps every blob of the run, about 80 bytes each, also
// those a checkpoint has listed since. Release them after the index write
// once a run of many million chunks shows the memory.
type heldBlobs struct {
	mu    sync.Mutex
	names map[[sha256.Size]byte]struct{}
}

// claim adds name and reports whether it was new.
func (h *heldBlobs) claim(name [sha256.Size]byte) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.names[name]; ok {
		return false
	}
	h.names[name] = struct{}{}
	return true
}

func (h *heldBlobs) release(name [sha256.Size]byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.names, name)
}

// packSession is the write side of one backup or import: the chunk and tree
// stores it writes through, and the order that makes their objects present.
// Packs are stored first, then Sync, then the index objects that list them:
// an index object is never stored before a Sync that followed the Put of
// every pack it lists, so a power loss cannot leave an index entry without
// its blob. A crash before the index object leaves packs no index lists,
// which nothing reads and prune deletes.
type packSession struct {
	backend       Backend
	now           func() time.Time
	chunks, trees *objectStore

	// flushing holds a token while a caller syncs and writes index objects.
	flushing chan struct{}

	mu sync.Mutex
	// stored lists the packs whose Put returned and no index object lists
	// yet, data packs then tree packs; pending counts their blobs.
	stored  [2][]indexedPack
	pending int
	last    time.Time
	// indexed names the index objects the session stored, by side. Only
	// flushIndex appends, under flushing.
	indexed [2][]string
}

// writeSession returns the session of a run that is about to start.
func (r *Repository) writeSession() *packSession {
	p := &packSession{backend: r.backend, now: r.now, flushing: make(chan struct{}, 1), last: r.now()}
	p.chunks, p.trees = r.stores()
	uploads := newPackUploads()
	for i, s := range []*objectStore{p.chunks, p.trees} {
		s.held = &heldBlobs{names: make(map[[sha256.Size]byte]struct{})}
		s.writer = newPackWriter(r.backend, s.space, uploads,
			func(ctx context.Context, pack string, entries []packEntry) error {
				return p.packStored(ctx, i, indexedPack{name: pack, entries: entries})
			})
	}
	return p
}

// packStored records a pack whose Put returned and writes a checkpoint when
// one is due.
func (p *packSession) packStored(ctx context.Context, side int, pack indexedPack) error {
	p.mu.Lock()
	p.stored[side] = append(p.stored[side], pack)
	p.pending += len(pack.entries)
	due := p.due()
	p.mu.Unlock()
	if !due {
		return nil
	}
	return p.flushIndex(ctx, false)
}

// due reports whether the stored packs are to be listed now. The caller
// holds p.mu.
func (p *packSession) due() bool {
	return p.pending >= indexMaxRecords || p.now().Sub(p.last) >= checkpointInterval
}

// flushIndex syncs and then stores index objects for the packs stored before
// the Sync began. Without force it does so only while a checkpoint is due. It
// runs under the context of the operation and never detached from it: a
// writer that was stopped, or whose lock went stale, must not publish an
// index for packs a prune may have deleted.
func (p *packSession) flushIndex(ctx context.Context, force bool) error {
	if err := take(ctx, p.flushing); err != nil {
		return err
	}
	defer func() { <-p.flushing }()

	p.mu.Lock()
	if p.pending == 0 || !force && !p.due() {
		p.mu.Unlock()
		return nil
	}
	packs := p.stored
	p.stored, p.pending, p.last = [2][]indexedPack{}, 0, p.now()
	p.mu.Unlock()

	if err := p.backend.Sync(ctx); err != nil {
		return fmt.Errorf("ark: failed to sync packs: %w", err)
	}
	for i, s := range []*objectStore{p.chunks, p.trees} {
		if len(packs[i]) == 0 {
			continue
		}
		names, err := s.index.write(ctx, packs[i])
		p.indexed[i] = append(p.indexed[i], names...)
		if err != nil {
			return err
		}
	}
	return nil
}

// finish stores the open packs and lists every pack of the run in the index.
// It is called once every write of the run returned, before the manifest.
func (p *packSession) finish(ctx context.Context) error {
	for _, s := range []*objectStore{p.chunks, p.trees} {
		if err := s.writer.flush(ctx); err != nil {
			return err
		}
	}
	return p.flushIndex(ctx, true)
}
