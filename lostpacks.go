package ark

import (
	"context"
	"fmt"
)

// The lost-pack marker tells a backup that a data pack the index lists is not
// stored. A backup that takes a file unchanged from its parent keeps the
// parent's chunk references without asking the index, so without the marker
// it would not notice that one of those chunks is gone (see ADR 0015).
//
// Any operation whose load of the data index finds a lost data pack or an
// index object that does not open stores the marker, a prune dry run too: the
// marker only ever makes a backup read more. It is removed only by a prune,
// under the exclusive lock, once every chunk a snapshot references is listed
// in a stored pack. Nothing else deletes it, so storing it under a shared lock
// is safe. A marker that is there for no reason costs a load of the data index
// per backup, nothing more.

// lostPacksMarker returns the handle of the marker: one object, named by the
// keyed hash of its namespace name like a content-named object.
func (r *Repository) lostPacksMarker() Handle {
	return Handle{Type: LostPackFile, Name: keyedName(r.key.IDKey[:], []byte(LostPackFile.String()))}
}

// markLostPacks stores the marker and makes it durable before the caller goes
// on: a backup after a power loss must still find it. A marker that is stored
// already is left as it is, with no Put and no Sync, since only a prune
// deletes it and not while this caller holds its lock; the writer that put it
// synced it before it relied on it.
func (r *Repository) markLostPacks(ctx context.Context) error {
	h := r.lostPacksMarker()
	if ok, err := r.backend.Exists(ctx, h); err == nil && ok {
		return nil
	}
	if err := r.putSealed(ctx, h, nil); err != nil {
		return err
	}
	if err := r.backend.Sync(ctx); err != nil {
		return fmt.Errorf("ark: failed to sync %s object %s: %w", h.Type, h.Name, err)
	}
	return nil
}

// lostPacksMarked reports whether the marker is stored, with one request. A
// marker that cannot be asked about counts as stored: the cost of a wrong yes
// is a load of the data index, of a wrong no a snapshot that does not restore.
func (r *Repository) lostPacksMarked(ctx context.Context) bool {
	h := r.lostPacksMarker()
	ok, err := r.backend.Exists(ctx, h)
	if err != nil {
		r.logger.WarnContext(ctx, "failed to ask for the lost-pack marker, counting it as stored",
			"namespace", h.Type.String(), "error", err)
		return true
	}
	return ok
}

// dropLostPacks removes the marker. The caller, a prune, holds the exclusive
// lock and has made durable an index that lists every referenced chunk in a
// stored pack. A marker that stays only costs reads, so a failed delete is
// logged. Asking first keeps a prune that has nothing to do from writing.
func (r *Repository) dropLostPacks(ctx context.Context) {
	if !r.lostPacksMarked(ctx) {
		return
	}
	h := r.lostPacksMarker()
	if err := r.backend.Delete(ctx, h); err != nil {
		r.logger.WarnContext(ctx, "failed to delete the lost-pack marker, backups keep loading the data index",
			"namespace", h.Type.String(), "error", err)
	}
}
