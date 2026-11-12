# 0005: Sync barrier instead of per-object fsync

Status: partly superseded by 0012
Date: 2026-11-08

## Context

Crash safety first made arklocal and arksftp fsync every object, its directory and new parent
directories. The profile of the first benchmark (see 0002) put a chunk Put at 73.8 ms, 95% of it
fsync of the file, of the directory and of new shard directories; the walk sat blocked on busy put
workers for 24% of the initial backup, and the initial backup took 8.5 times restic. restic writes
packs of many blobs and pays far fewer fsyncs per GiB. Larger chunks would not help, since nearly
every file of the dataset is a single chunk, and more parallelism cannot make one disk flush
faster. Whatever replaced per-object fsync had to keep the kill harness (see 0001) and the power
loss guarantees: no snapshot may reference an object that a power loss can still undo.

## Decision

`Backend` gets a required `Sync(ctx)`: every Put and Delete that returned before the call is
durable after it. Put and Delete no longer promise durability on their own.

arklocal: Put and Delete do not fsync. Sync calls `syncfs` on a descriptor of the repository
root (Linux) or `sync` (macOS), and falls back to `sync` when `syncfs` fails, so the flush is
never skipped, though the failure itself is not logged (`arklocal/sync_linux.go`).

arksftp keeps `fsync@openssh.com` per object, because a deferred fsync would cost an open, fsync
and close per object again; its Sync does nothing. arkmem does nothing.

Barriers: a backup syncs before and after it saves its manifest, init after the keys and the
config, a password change after the new key, forget after deleting the manifests and before any
chunk sweep, prune at the end. Locks are never synced: a lock that survives a power loss is a
stale lock anyway.

Torn objects: a power loss before the barrier can leave a chunk with no or partial content
under its full name, which a later backup's Exists check would trust. A backup therefore puts a
marker, durable before its first object, and deletes it once its manifest is durable. While any
marker exists, backups read every object they would reuse and store again one that does not
open to its name. A marker whose lock is gone is stale, and prune removes it after sweeping the
unreferenced objects. This paragraph is superseded by 0012: with packs, an index entry written
after a Sync says what is present, and markers and repair mode are gone.

The repository format number stays, and pack files stay open for a later decision.

- Not parallelism alone, because more workers do not make one disk flush faster.
- Not larger chunks, because nearly every file of the benchmark is one chunk.
- Not pack files now, because they change the on-disk format and the SFTP numbers that would
  justify them are not measured yet.

## Consequences

The chunk Put cost falls to the write itself: the initial backup went from 6.7 times restic (after
the unchanged-file change of 0003 had lowered it from the baseline 8.5) to 1.5 times restic with
this change alone, and later work brought it to 0.82x (see 0002). The `Backend`
interface broke, which the v0 line allows (see 0004); every backend now has to implement Sync and
the conformance suite calls it. Durability depends on the barrier points being in the right
places, which `TestDurabilityBarriers` in `sync_test.go` pins by counting the Syncs of each
operation. After an unclean end, the next backup paid for reading every reused object until a
prune cleared the stale marker; a cancelled backup kept its marker too. That cost ended with 0012.
