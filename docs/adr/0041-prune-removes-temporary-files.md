# 0041: Prune removes temporary files through an optional backend interface

Status: accepted
Date: 2026-11-23

## Context

arklocal and arksftp write each object to a `.ark-tmp-` file in its directory and rename it into
place. A `Put` cut off by a kill, a crash or a cancelled sftp request leaves that file behind, and
nothing ever removed it: List passes over every name `ark.CheckName` refuses, so the core never
saw it, and the files piled up in the repository.

Where an sftp server has no posix-rename, a `Put` over an existing object has to rename the old
object aside to a `.ark-old-` name first. A failed write can leave that file behind as well, and it
may hold the only copy of the object, so it must never be cleaned up the same way.

Only the backend knows its layout and its prefixes. The `Backend` interface has no method for
files that are not objects.

## Decision

- `ark.Leftovers` in backend.go is an optional interface with `RemoveTemp(ctx)` and
  `ListAside(ctx, fn)`. A backend that leaves no files behind does not implement it.
- `Prune` calls `RemoveTemp` once it holds the exclusive lock, before it lists the manifests. No
  `Put` outside `locks` runs under that lock, so every temporary file there is a leftover,
  whatever its age. A dry run does not call it.
- `RemoveTemp` deletes every `.ark-tmp-` file in every namespace and shard directory except
  `locks`, where another process may be writing its lock and the pruning process refreshes its own.
  It never touches a `.ark-old-` file. A file it may not remove, such as another user's in a shared
  repository, is logged and kept. arksftp also removes the empty `.ark-tmp-` directories its
  private `mkdir` leaves in the root and the namespace directories. `ListAside` yields each
  `.ark-old-` file as a slash-separated path relative to the repository root; `Check` reports them
  and nothing removes or restores them.
- arklocal and arksftp implement it with one walk over the namespaces of the known handle types
  and the shards of the pack namespaces, links followed. `ark.Instrument` forwards both calls.

Rejected options:

- Not a cleanup when a repository opens, because a reader holds no exclusive lock and a `Put` of
  another process may be writing the file it would remove.
- Not an age threshold, because the exclusive lock already rules out a `Put` in flight, and a
  threshold would keep the files of a kill a minute ago.
- Not new methods on `Backend`, because most stores write an object in one step and would carry
  two methods that do nothing.

## Consequences

- A prune leaves no `.ark-tmp-` file outside `locks` that it may remove, whatever killed the
  writes before it. A lock write cut off by a kill leaves a small file in `locks` for good, and a
  temporary directory from creating the repository root stays in the root's parent, outside the
  repository. A failed listing fails the prune before it deletes anything.
- An aside file survives every prune until someone moves it back or removes it.
- An out-of-tree backend with temporary files opts in by implementing two methods.
- Tests pin it: `TestPruneRemovesTempFiles` in the root package and in arksftp, and
  `TestRemoveTempKeepsAsideFiles` in arklocal and arksftp.
