# 0020: Read session with an io/fs view per snapshot

Status: accepted
Date: 2026-11-15

## Context

A mount, a file server or an embedder that browses snapshots reads many small pieces of many
files. Every public read method takes the shared lock for the call, which costs a List, a Put, a
settle delay and a second List, and loads the tree and chunk index again. A store's index must
not outlive its operation either. A per-call `OpenFile(ctx, id, path)` would therefore pay a lock
and an index load per FUSE lookup, and Restore streams a file from its first byte only. The FUSE
frontend lives in `cmd/ark`, which uses only the public API, so the root package has to offer
random access itself. Open were the API shape, how long the lock is held, whether the file system
methods take a context and what a symlink does.

## Decision

The root package gains a long-lived read session with a standard `io/fs` view per snapshot:

```go
func (r *Repository) OpenReader(ctx context.Context) (*Reader, error)
func (rd *Reader) Snapshots(ctx context.Context, f SnapshotFilter) ([]*Snapshot, error)
func (rd *Reader) FS(id string) (*SnapshotFS, error)
func (rd *Reader) Close() error
```

`OpenReader` takes one shared lock and holds it, refreshed, until `Close`. The session owns one
chunk and one tree store, so each index loads once, one chunk fetcher with a 256 MiB chunk cache
and an LRU of parsed tree nodes keyed by tree hash. `Close` cancels the reads still running,
waits for the calls in progress and releases the lock; later calls fail with `fs.ErrClosed`.

`SnapshotFS` implements `fs.FS`, `fs.StatFS`, `fs.ReadDirFS` and `fs.ReadLinkFS`, so
`http.FileServerFS`, `os.CopyFS` and `fs.WalkDir` work on it unchanged. Names are io/fs paths
without the trailing slash Walk gives directories. A `FileInfo` carries mode, size and
modification time from the snapshot and its `Sys()` returns the entry as an `ark.FileInfo`.
Regular files are `io.ReaderAt`, `io.Seeker` and `io.Reader`. `ReadAt` binary-searches the
offsets the tree entry stores for each chunk and asks the fetcher for every chunk it overlaps at
once, so neither an index lookup for sizes nor a streamed restore is needed, and concurrent
`ReadAt` calls share the fetcher's batching and cache. A hardlink reads as a regular file with
its target's content. A symlink is served by `Lstat` and `ReadLink` only; `Open` and `Stat` on
one fail with an error wrapping `fs.ErrInvalid`.

- Not a per-call `OpenFile` or a per-snapshot `SnapshotFS(ctx, id)`, because a lock each makes a
  mount that browses N snapshots hold and refresh N locks. A one-call helper can wrap the
  session later.
- Not a lock per request, because one acquire costs at least the settle delay.
- Not a context on the file system methods, because io/fs has none and a second API beside it
  doubles the surface; the context passed to `OpenReader` and `Close` bound every read.
- Not following symlinks inside the snapshot, because targets are often absolute paths of the
  host the snapshot was taken on. The cost is that `testing/fstest.TestFS` reports the one
  `Open` of each symlink it lists.
- No prefetch beyond the chunk cache until a sequential read is measured against restore.

## Consequences

A Prune waits or fails with `ErrRepoLocked` for as long as a session is open; a Forget still
runs and deletes only the manifest, so open file systems keep reading. Each index loads once per
session, so a snapshot a backup stores after the session's first read lists in `Snapshots`, but
the trees and chunks it added are not found; a new session reads it, and a refresh is left to the
mount. One hung backend
read cannot be cancelled by the caller of `ReadAt`, only by the session's context or `Close`.
Tests in `reader_test.go` pin the io/fs conformance (`TestReaderFSPassesFSTest`), random reads
against restore across chunk boundaries (`TestReaderReadAtMatchesRestore`), stat, hardlink and
symlink behaviour (`TestReaderStatAndLinks`) and the lock and goroutine count around `Close`
(`TestReaderHoldsOneLockAndLeavesNothingBehind`).
