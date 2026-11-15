// Package arkfind searches the snapshots of a repository for entries whose
// path matches a glob pattern. It is built only on [ark.Repository.Snapshots]
// and [ark.Repository.Walk], and matches with [arkglob] under the same rules
// as excludes, which are restic's: a pattern without a "/" matches a base
// name anywhere, a pattern with one matches any run of path components, a
// leading "/" anchors it at the snapshot root, and a matched directory yields
// every entry below it too.
package arkfind

import (
	"context"
	"errors"
	"fmt"
	"path"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkglob"
)

// Match is one entry of one snapshot whose path matched a pattern.
type Match struct {
	Snapshot *ark.Snapshot
	// File is the entry as Walk yields it: a directory's path carries a
	// trailing slash.
	File ark.FileInfo
}

// Find calls fn for every entry of the snapshots selected by filter whose path
// matches any of patterns.
//
// Snapshots are searched newest first, the order of Repository.Snapshots. The
// matches of one snapshot arrive one after another in tree order, parents
// before children, so a caller groups them by watching Match.Snapshot change.
// fn runs on the calling goroutine.
//
// No patterns, or a pattern arkglob rejects, return an error wrapping
// [path.ErrBadPattern] before the repository is touched. An error from fn ends
// the search and is returned unchanged. A snapshot forgotten after the
// snapshot list was read is skipped; any other error from walking a snapshot
// ends the search and names the snapshot.
//
// Cost: one read of every snapshot manifest, then one Walk per selected
// snapshot, each taking the shared lock and loading the tree index, and
// reading every tree node of that snapshot. No file content is read.
func Find(
	ctx context.Context,
	repo *ark.Repository,
	filter ark.SnapshotFilter,
	patterns []string,
	fn func(Match) error,
) error {
	if len(patterns) == 0 {
		return fmt.Errorf("arkfind: no pattern: %w", path.ErrBadPattern)
	}
	matcher, err := arkglob.New(patterns...)
	if err != nil {
		return fmt.Errorf("arkfind: %w", err)
	}
	snaps, err := repo.Snapshots(ctx, filter)
	if err != nil {
		return fmt.Errorf("arkfind: list snapshots: %w", err)
	}
	for _, snap := range snaps {
		var fnErr error
		err := repo.Walk(ctx, snap.ID, func(fi ark.FileInfo) error {
			if !matcher.Match(fi.Path) {
				return nil
			}
			fnErr = fn(Match{Snapshot: snap, File: fi})
			return fnErr
		})
		if fnErr != nil {
			return fnErr
		}
		if err == nil || vanished(ctx, repo, snap.ID, err) {
			continue
		}
		return fmt.Errorf("arkfind: snapshot %s: %w", snap.ID, err)
	}
	return nil
}

// vanished reports whether err comes from a snapshot that was forgotten after
// the list was read. A missing tree also reports ErrNotFound, so the snapshot
// is looked up again before its error is dropped.
func vanished(ctx context.Context, repo *ark.Repository, id string, err error) bool {
	if !errors.Is(err, ark.ErrNotFound) {
		return false
	}
	_, err = repo.Snapshot(ctx, id)
	return errors.Is(err, ark.ErrNotFound)
}
