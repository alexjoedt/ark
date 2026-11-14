# 0017: A counting scan beside the store

Status: accepted
Date: 2026-11-14

## Context

A backup walks the source and stores its files in one pass, so it learns how many files and bytes
there are only when it is done. A progress display without totals can show what was stored, not
how much is left. restic shows totals early because a counting scanner walks the source beside the
archiver.

The Source contract promises a single concurrent Walk. A directory can be walked twice, but
`StreamSource` reads one stream and the Docker volume source reads a tar stream from the daemon;
neither can be walked a second time. A second walk also costs a stat per entry, which an embedder
that shows no progress should not pay.

## Decision

An optional interface `RewalkSource` (`Source` plus `Rewalk() bool`) marks a source that can be
walked a second time while the first Walk runs, with open never called. arkfs implements it and
returns true; `StreamSource` and the Docker volume source do not. A wrapper that embeds a Source
hides the method and forwards it, as with `ConcurrentSource`.

The scan is opt-in: `WithBackupScan()` starts a counting Walk beside the store for a
`RewalkSource` whose `Rewalk` returns true, and does nothing for any other source or without the
option. It is meant for a caller that shows totals, such as a live progress display.

- The count is what the snapshot records: regular files and hardlinks count as files, regular file
  sizes as bytes, the source's own excludes apply and unchanged files count. For a source that does
  not change during the run, the totals equal the snapshot's `FileCount` and `TotalSize`.
- While the scan runs, `PhaseScan` events carry its running `FilesTotal` and `BytesTotal`, at most
  one every 100 ms; the last one carries the totals, and every `PhaseStore` event after it carries
  them too. Scan events go through the mutex the store's events use, so calls never overlap.
- The backup never depends on the scan: a scan that fails is dropped, one that is still running
  when every file is stored is cancelled, and the backup waits for it to return before its
  `PhaseFinalize` event.
- The cost bar, on the 200k-file benchmark dataset on SSD, median of 3: at most 5% more wall time
  for an initial backup and 10% for an incremental one than without the scan. A miss reopens the
  decision instead of shipping the scan.
- Not a wider Source contract with two concurrent Walks for every source, because a stream source
  would have to buffer or re-open what it reads.
- Not counting inside arkfs alone, because the core would need an arkfs-only path, and an
  embedder's own directory source could not offer totals.
- Not on by default, because every embedder that shows no totals would pay the second walk.

## Consequences

Totals exist for directory sources only; a Docker volume or stream backup reports progress without
them. The running count can lag the store on a slow file system, and a source that changes during
the run yields totals that differ from the snapshot. A source whose Walk ignores a cancelled ctx
delays the end of the backup until its scan returns.
