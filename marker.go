package ark

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// A backup no longer makes each object durable on its own: Sync before the
// manifest does. A power loss before that barrier can leave a chunk or tree
// object with no or partial content under its full name, and a later backup
// whose Exists check finds that name would reference the torn object. So a
// backup puts a marker named like its lock, durable before its first object,
// and deletes it once its manifest is durable. While any marker exists, a
// backup reads every object it would reuse and stores again one that does not
// open to its name. A marker without a live lock of its name is stale: its
// backup never finished, and Prune removes it once it has swept the
// unreferenced objects.

func markerHandle(name string) Handle { return Handle{Type: MarkerFile, Name: name} }

// putMarker marks the write that holds lock and makes the marker durable, so
// it outlives every object the write stores after it.
func (r *Repository) putMarker(ctx context.Context, lock *repoLock) error {
	data, err := json.Marshal(newLockObject(r.now(), lock.exclusive))
	if err != nil {
		return fmt.Errorf("marker: failed to marshal: %w", err)
	}
	if err := r.putSealed(ctx, markerHandle(lock.name), data); err != nil {
		return err
	}
	if err := r.backend.Sync(ctx); err != nil {
		return fmt.Errorf("marker: failed to sync %s: %w", lock.name, err)
	}
	return nil
}

// clearMarker removes the marker of lock once the write's objects are
// durable. synced says a barrier already followed the last object; a failed
// write syncs first, a cancelled one keeps its marker rather than block on a
// Sync. A marker that is not cleared only goes stale: the next backups verify
// the objects they reuse and Prune removes it.
func (r *Repository) clearMarker(ctx context.Context, lock *repoLock, synced bool) {
	if !synced && ctx.Err() != nil {
		r.logger.InfoContext(ctx, "keeping the marker of a cancelled write, prune removes it", "marker", lock.name)
		return
	}
	ctx = context.WithoutCancel(ctx)
	if !synced {
		if err := r.backend.Sync(ctx); err != nil {
			r.logger.WarnContext(ctx, "failed to sync a failed write, keeping its marker",
				"marker", lock.name, "error", err)
			return
		}
	}
	if err := r.backend.Delete(ctx, markerHandle(lock.name)); err != nil {
		r.logger.WarnContext(ctx, "failed to remove marker", "marker", lock.name, "error", err)
	}
}

// staleMarkers returns the markers no live lock of the same name holds, with
// the identity of the process that wrote them. Markers are listed before the
// locks, and a write deletes its marker before its lock, so a marker whose
// lock went away in between is found gone on reading and skipped. One that
// does not open is stale with a zero identity.
func (r *Repository) staleMarkers(ctx context.Context) (map[string]lockObject, error) {
	var names []string
	err := r.backend.List(ctx, MarkerFile, func(info BlobInfo) error {
		names = append(names, info.Name)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("marker: failed to list markers: %w", err)
	}
	if len(names) == 0 {
		return map[string]lockObject{}, nil
	}
	locks, _, err := r.loadLocks(ctx)
	if err != nil {
		return nil, err
	}
	now := r.now()

	stale := make(map[string]lockObject)
	for _, name := range names {
		if l, ok := locks[name]; ok && !l.stale(now) {
			continue
		}
		data, err := r.getSealed(ctx, markerHandle(name))
		if errors.Is(err, ErrNotFound) {
			continue
		}
		var l lockObject
		if err == nil {
			_ = json.Unmarshal(data, &l)
		}
		stale[name] = l
	}
	return stale, nil
}

// writeStores returns the chunk and tree stores a backup or import writes
// through: the repository's own, or repairing copies while any marker exists.
// Lock liveness does not decide this: a crashed backup of another host keeps a
// fresh lock for lockStaleAge, and on this host a reused pid keeps a dead one
// alive. A running write's marker costs the reads of the objects reused.
func (r *Repository) writeStores(ctx context.Context) (chunks, trees *objectStore, err error) {
	n := 0
	err = r.backend.List(ctx, MarkerFile, func(BlobInfo) error {
		n++
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("marker: failed to list markers: %w", err)
	}
	if n == 0 {
		return r.chunks, r.trees, nil
	}
	r.logger.InfoContext(ctx, "another backup is running or did not finish, verifying every object reused",
		"markers", n)
	return r.chunks.repairing(), r.trees.repairing(), nil
}

// removeStaleMarkers deletes every stale marker. Prune calls it once the
// sweep is durable: the objects those writes may have torn are gone or
// rewritten by then.
func (r *Repository) removeStaleMarkers(ctx context.Context) (int, error) {
	stale, err := r.staleMarkers(ctx)
	if err != nil {
		return 0, err
	}
	for name := range stale {
		if err := r.backend.Delete(ctx, markerHandle(name)); err != nil {
			return 0, fmt.Errorf("marker: failed to remove %s: %w", name, err)
		}
	}
	return len(stale), nil
}

// uncleanFindings describes every stale marker for Check, oldest first.
func (r *Repository) uncleanFindings(ctx context.Context) ([]string, error) {
	stale, err := r.staleMarkers(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(stale))
	for name := range stale {
		names = append(names, name)
	}
	sort.Strings(names)
	findings := make([]string, 0, len(names))
	for _, name := range names {
		l := stale[name]
		who := "identity unknown"
		if l != (lockObject{}) {
			who = fmt.Sprintf("host %s pid %d, %s", l.Hostname, l.PID, l.Time.UTC().Format("2006-01-02 15:04:05Z"))
		}
		findings = append(findings, fmt.Sprintf("unclean backup %s (%s), prune cleans up", name, who))
	}
	return findings, nil
}
