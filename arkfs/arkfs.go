package arkfs

import (
	"context"

	"github.com/alexjoedt/ark"
)

// Backup stores the directory tree rooted at path as a new snapshot in repo.
// It is NewSource plus repo.Backup, which is all the convenience a path needs.
func Backup(ctx context.Context, repo *ark.Repository, path string, opts ...ark.BackupOption) (*ark.Snapshot, error) {
	src, err := NewSource(path)
	if err != nil {
		return nil, err
	}
	return repo.Backup(ctx, src, opts...)
}

// Restore writes the snapshot to destPath on the local filesystem.
// snapshotID is a full snapshot ID or a unique prefix.
func Restore(
	ctx context.Context,
	repo *ark.Repository,
	snapshotID ark.SnapshotID,
	destPath string,
	opts ...ark.RestoreOption,
) (*ark.RestoreResult, error) {
	sink, err := NewSink(destPath)
	if err != nil {
		return nil, err
	}
	return repo.Restore(ctx, snapshotID, sink, opts...)
}
