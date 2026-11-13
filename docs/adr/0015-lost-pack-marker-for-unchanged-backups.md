# 0015: A marker makes unchanged backups see a lost data pack

Status: accepted
Date: 2026-11-13

## Context

Since 0014 an index load passes over the records of a pack that is not stored, so a backup stores
again what it reads. A file that a backup takes as unchanged from its parent is not read: it keeps
the parent's chunk references without a lookup, so that a backup of unchanged files reads no data
index (see 0003 and 0011). After a data pack was lost, every such backup wrote a snapshot that did
not restore, until the snapshots Check names were forgotten. Loading the data index on every backup
would close the gap at the cost 0011 rejected for the healthy case.

## Decision

An index load of the data index that finds a listed pack gone stores a marker object,
`lostpacks/<name>`, before it passes over the records (`index.go`, `lostpacks.go`). The name is the
keyed hash of the namespace name, the content is empty and sealed like every single object; only
its presence counts. The marker is made durable with a Sync, so a backup after a power loss still
finds it. Backup, import, Check, prune and the index rebuild fail when they cannot store it, since
they write anyway. Restore, export and Verify log the failure and go on, so a read never fails for
it.

A backup whose parent offers files it could take unread asks for the marker with one `Exists`
(`backup.go`, `parentFiles`). No marker: nothing changes, the backup reads no data index. A marker,
or a request that fails: the backup loads the data index and leaves out of the parent's files every
file of which a chunk resolves to no stored pack, so it reads them again and stores what is gone.

Only the exclusive lock removes the marker, and only once the index is durable. Prune removes it
when the index it leaves lists no pack that is gone; while it keeps the records of a lost pack that
holds the only copy of a referenced chunk (0014), the marker stays. The index rebuild lists stored
packs only, but a lost pack whose records it drops may have held a chunk a snapshot references,
which then no index lists: it removes the marker only when it dropped no lost pack. Prune deletes
nothing while a snapshot references a chunk no index lists, so it removes such a marker only once
none does.

A marker stored by a reader is never lost to a clear. Readers and backups store it under a shared
lock, prune and the rebuild delete it under the exclusive lock before they release it, and the two
classes never overlap: nothing deletes the marker under a shared lock, so storing it there is safe.
The marker can only make a backup read more: one that is missing because no load saw the loss yet
is the case before this decision, one that is present for no reason costs a data index load per
backup until the next prune.

- Not loading the data index on every backup, because that is the cost 0011 avoids for the healthy
  case, and a lost pack is rare.
- Not checking the marker before the parent's tree is walked, because a backup without files to
  take unread needs no answer.
- Not a marker for tree packs, because a backup never inherits a tree node without a lookup: the
  parent's tree is read through the tree index, which passes over a lost pack, so the walk fails
  and every file is read; an unchanged directory is looked up before its node is reused.
- Not removing the marker in a backup or Check that finds no lost pack, because that would race a
  reader under the same shared lock class that just stored it.
- Not reading the marker's content, because a marker that does not open must still count.

## Consequences

A backup after a load that saw a lost pack reads again the unchanged files that lost a chunk, and
its snapshot restores: `TestUnchangedFileIsReadAgainOnceALoadFoundALostPack`, which also pins that
the marker stays through a prune that keeps the lost pack and goes with the prune that drops it.
A read that cannot store the marker still restores, Check fails:
`TestReadersStoreTheMarkerBestEffort`. A rebuild keeps the marker while it drops a lost pack:
`TestRebuildKeepsTheMarkerWhileItDropsALostPack`. The delete falls inside the exclusive lock:
`TestMarkerGoesWhileTheExclusiveLockIsHeld`.

An unchanged backup costs one request more and still reads no data index:
`TestUnchangedBackupReadsNoDataIndex`. A prune that may remove the marker asks for it once and
deletes it only when it is there, so a prune with nothing to do writes nothing.

The limit stays: a pack loss that no index load has seen yet is not detected. A run of backups that
only take files unchanged keeps writing snapshots that miss the lost chunks until a Check, a
Restore, a Verify, an export, a backup with a changed file or a prune loads the data index and
stores the marker. The first test above pins that a backup right after the loss still takes the
file unread.

The format version stays 3: a build that predates the marker never reads its namespace and ignores
it. The namespace is a new `HandleType`, `LostPackFile`, which a backend maps by its name like the
others. This supersedes the part of 0014 that says the repair by the next backup reaches only what
a backup reads.
