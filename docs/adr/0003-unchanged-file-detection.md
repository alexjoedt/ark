# 0003: Unchanged-file detection

Status: accepted
Date: 2026-11-07

## Context

A backup resolved its parent snapshot but never used it: every incremental backup read, chunked
and hashed every byte again. At a 5% change rate that is about 20 times the data restic reads, so
the incremental budget of 1.2x restic was out of reach by construction. Open were the match key
(path, size and mtime, which every source has, or also ctime and inode, which tar streams and
arkdocker do not have), how a source skips a file it was already asked to hand over (the walk
callback had to consume every reader), how the parent tree is accessed, and whether users get an
opt-out. restic matches on size, mtime, ctime and inode, loads the parent tree directory by
directory as it walks, and offers `--force`, `--ignore-ctime` and `--ignore-inode` to weaken or
skip the check.

## Decision

A file is unchanged when size and mtime match its entry in the parent and, when both sides carry
them, ctime and inode match too. `FileInfo` and tree entries gain `ChangeTime` and `Inode`, stored
as optional fields (`ctime`, `inode`), so the on-disk format number stays. arkfs fills all four on
Linux and macOS; elsewhere it leaves ctime and inode zero and matches on size and mtime like a tar
source. Sources that cannot, such as tar streams and arkdocker, leave ctime and inode zero and are
matched on size and mtime. A zero mtime never matches: a source that reports no time cannot be
trusted to report a change. ctime cannot be set from user space, so on Linux and macOS a rewrite
that restores the mtime is still caught on arkfs.

The walk callback becomes `WalkFunc(entry, open)`: the backup calls `open` at most once and only for
a file it has to read, arkfs opens the file lazily, tar sources hand over the current entry and skip
its bytes on the next header. An unchanged file takes the parent's chunk list and is counted in
`SnapshotStats.FilesUnchanged`. The parent tree is loaded once into a map keyed by path. A file
whose mtime is not older than 2 seconds before the parent's start, including any file modified after
that start, is read again, since a rewrite in the same timestamp tick leaves the stat unchanged; a
parent of another source lends no files. There is no opt-out.

- Not size and mtime alone, because a rewrite that keeps the mtime would go unnoticed on a
  filesystem that can tell.
- Not ctime and inode as a hard requirement, because tar streams and Docker volumes would never
  match and lose incremental backups entirely.
- Not an opt-out such as `--force` or `--ignore-ctime`, because every option needs a user who asks
  for it; the zero fallback already covers sources that lack ctime or inode.
- Not draining the reader of an unchanged file, because arkfs would still read every byte.
- Not walking the parent tree alongside the source, because a map is simpler and one entry per
  file of the parent is small next to the data it saves reading.

## Consequences

The incremental backup reads only changed files; on the benchmark this change alone took it from
21.3 to 3.4 times restic; the later Sync barrier and restore work brought it to about half of
restic's time (see 0002). The source contract changed for every `Source` implementation and arktest
checks a walk that never calls `open`. Memory grows with the file count of the parent. A file that
changes without touching size, mtime, ctime or inode is missed, and on sources without ctime the
window is wider; that is the same trade every metadata-based backup tool makes. Reused chunks are
safe because forget deletes only the snapshot and prune needs the exclusive lock a running backup's
shared lock blocks. Pinned by `TestBackupReusesUnchangedFilesWithoutOpening`,
`TestBackupComparesChangeTimeOnlyWhenBothSidesHaveIt`, `TestBackupRereadsRacyAndForeignParentFiles`
and `TestForgetAndPruneOfParentDuringChildBackup` in `incremental_test.go`.