# 0039: Forget takes the exclusive lock

Status: accepted
Date: 2026-11-22

## Context

Forget took the shared lock: it only deletes manifests, so it could run beside backups, and what
it left unreferenced waited for the next prune. But a policy run decides from the manifests it
lists. Two forgets with different policies that list the same manifests at once each remove what
only the other keeps. `--keep-tag pinned` beside `--keep-last 1` removes the newest snapshot and
the pinned one, and the series is empty, although each policy alone keeps one. The check that
refuses a policy that keeps nothing of a series runs per forget, so it cannot see this.

## Decision

- Every forget that removes snapshots takes the exclusive lock, a removal by ID too.
- A dry run takes the shared lock: it removes nothing and keeps running beside backups.
- The CLI keeps `--lock-wait` at 0. The docs point a scheduled forget that may overlap a backup to
  it.
- `forget --prune` keeps two acquisitions in a row: forget releases its lock, prune takes the
  exclusive lock again. A backup that slips in between is harmless, since prune deletes only what
  no index lists and no snapshot references.

Not chosen: a check against a second listing before the deletes. It narrows the window and does
not close it, and every other operation that decides from a listing already holds the lock that
keeps the deciders apart.

## Consequences

- A forget fails with `ErrRepoLocked`, exit 3, while a backup, a restore or a mount runs, unless it
  waits. A scheduled job that ran backup and forget in parallel now needs `--lock-wait` or runs
  them in sequence.
- docs/format.md lists forget under the exclusive lock and forget with `--dry-run` under the
  shared one; a writer of another implementation follows the same table.
- Tests pin it: `TestEveryOperationTakesItsLockClass`, `TestConcurrentForgetsKeepTheSeries`,
  `TestForgetWaitsForASharedLock` and `TestForgetDryRunRunsBesideABackup`.
