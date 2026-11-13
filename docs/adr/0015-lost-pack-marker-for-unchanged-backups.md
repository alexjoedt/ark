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

An index load of the data index that finds a listed pack gone, or skips an index object that does
not open, stores a marker object, `lostpacks/<name>`, before any lookup passes over what they held
(`index.go`, `lostpacks.go`). The chunks of an object that does not open sit in stored packs but no
index lists them, which a reused reference survives no better than a lost pack. The name is the
keyed hash of the namespace name, the content is empty and sealed like every single object; only its
presence counts. The marker is made durable with a Sync, so a backup after a power loss still finds
it; a load that finds it stored already does not put it again but syncs, because the writer that put
it may have died before its own Sync. Backup and import fail when they cannot store it: the backups
after them would take unread what is lost. Every other operation logs the failure and goes on: a
read must restore from a repository that takes no write, and Check, prune and the index rebuild
diagnose or repair the loss the marker is about, so a full disk or a backend that refuses the
namespace must not stop them. A prune dry run that finds the loss stores the marker too and writes
nothing else: the marker can only make a backup read more, never less, and the dry run holds the
exclusive lock like any prune.

A backup whose parent offers files it could take unread asks for the marker with one `Exists`
(`backup.go`, `parentFiles`). No marker: nothing changes, the backup reads no data index. A marker,
or a request that fails: the backup loads the data index and leaves out of the parent's files every
file of which a chunk resolves to no stored pack, so it reads them again and stores what is gone.

The marker check runs before the backup's own load of the data index, which a changed file
triggers. Once the walk has passed on a file with data to read, it waits for that load before it
takes another file unread, and when the load found the loss it looks up the chunks of each such
file and reads the file when one resolves to no stored pack (`backup.go`, `runBackup`). A chunk
this backup stores again counts as stored, although no index lists it before the backup's next
checkpoint. Files the walk took unread before that load may reference what is lost and can no
longer be read: a stream source walks once. Before it stores any tree the backup looks up their
chunks, in memory since the index is loaded, and fails when one resolves to no stored pack and was
not stored again (`checkReused`). A backup that loaded no data index asks for the marker a second
time there, since another operation may have stored it after the first request, and loads the
data index when it is there. The load stored the marker, so the next backup reads those files
again. A backup whose load found no loss checks nothing, and one that found the marker at its
first request looked up the parent's files already and looks up none of them twice.

Only prune removes the marker, under the exclusive lock and after its index is durable. Prune
stops before any delete while a snapshot references a chunk no index lists, and it removes the
marker only when no referenced chunk is listed in a lost pack alone; so when it removes it, every
chunk a snapshot references is listed in a stored pack. While it keeps the records of a lost pack
that holds the only copy of a referenced chunk (0014), the marker stays. The index rebuild never
removes it: it lists stored packs only, so the records of a lost pack and of an index object that
did not open are gone after it, while a snapshot may still reference such a chunk, and a second
rebuild cannot tell that from a healthy index. A marker left after a rebuild costs a data index
load per backup until the next prune.

A marker stored by a reader is never lost to a clear. Readers and backups store it under a shared
lock, prune deletes it under the exclusive lock before it releases it, and the two
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
- Not letting the index rebuild remove the marker when it dropped no lost pack, because a rebuild
  after another rebuild sees no lost pack although snapshots still reference what the first one
  dropped.
- Not re-reading the files a backup took unread once its own load finds the loss, because a source
  may walk once; failing that one backup is the smallest change that commits no unchecked
  reference.

## Consequences

A backup after a load that saw a lost pack reads again the unchanged files that lost a chunk, and
its snapshot restores: `TestUnchangedFileIsReadAgainOnceALoadFoundALostPack`, which also pins that
the marker stays through a prune that keeps the lost pack and goes with the prune that drops it.
Restore, Verify, Check, prune and the rebuild run on a repository that refuses the marker and log
the failed write, a backup fails, a dry run stores the marker and nothing else:
`TestOnlyAWritingRunNeedsTheMarker`. Two rebuilds keep the marker and the backup after them reads
the file again: `TestOnlyPruneDropsTheMarker`. The backup whose own load finds a lost pack or an
index object that does not open fails and commits nothing:
`TestBackupThatFindsTheLossCommitsNoUnreadFileOfIt`. It reads a file the walk reaches after that
load, and a chunk it stores again counts for a file it took unread before:
`TestBackupThatFindsTheLossReadsWhatItCan`. A marker another operation stores after the backup asked
for it fails a backup that loaded no data index: `TestBackupAsksForTheMarkerAgainBeforeItCommits`.
An index object that does not open stores the marker:
`TestIndexObjectThatDoesNotOpenStoresTheMarker`. The delete falls inside the exclusive lock:
`TestMarkerGoesWhileTheExclusiveLockIsHeld`. A stored marker is not put again but synced:
`TestMarkerIsDurableOnceStored`.

An unchanged backup costs two requests more, an `Exists` before the walk and one before its trees,
and still reads no data index:
`TestUnchangedBackupReadsNoDataIndex`. A prune that may remove the marker asks for it once and
deletes it only when it is there, so a prune with nothing to do writes nothing.

The limit stays: a pack loss that no index load has seen yet is not detected. A run of backups that
only take files unchanged keeps writing snapshots that miss the lost chunks until a Check, a
Restore, a Verify, an export, a backup with a changed file (which then fails once) or a prune loads
the data index and stores the marker. The first test above pins that a backup right after the loss
still takes the file unread. A marker stored after a backup's second request is not seen by that
backup: the window is its tree build and the end of its write session.

The format version stays 3: a build that predates the marker never reads its namespace and ignores
it. The namespace is a new `HandleType`, `LostPackFile`, which a backend maps by its name like the
others. This supersedes the part of 0014 that says the repair by the next backup reaches only what
a backup reads.
