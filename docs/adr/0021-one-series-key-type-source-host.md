# 0021: One series key: source type, source name and host

Status: accepted
Date: 2026-11-16

## Context

A series is the run of snapshots one backup job writes: the next backup takes its parent from it
and a keep policy thins it. Three places formed series and each used another key. Parent selection
compared source name and host, the forget keep policy grouped by source name alone, and
`ark snapshots --last` did the same. With two machines backing up `/srv/app` into one repository,
`ark forget --keep-last 1` without `--host` kept one snapshot for both and removed the other
machine's last backup. A file source and a docker volume sharing a name and a host were one series
for all three, so a volume backup could take a file from the directory's snapshot unread, with only
path, size and mtime to tell them apart.

## Decision

A series is the set of snapshots that share source type, source name and host, compared verbatim
as the manifest stores them. The root package holds the key once, the unexported `seriesKey` in
`snapshot.go`, and both library users go through it: the automatic parent of a backup is the newest
snapshot of its series (`latestSnapshotID` in `manifest.go`), and `Repository.Forget` applies a keep
policy once per series among the snapshots its filter matches (`applyForgetPolicy` in
`forget_impl.go`). `ark snapshots --last N` groups on the same three public `Snapshot` fields in
`cmd/ark/list.go`. A parent named with `WithParent` lends its files only when source type and
source name match; its host is not checked, naming it is the caller's statement that it fits.

The library leaves the host to the embedder: `WithHost` sets it and the default is empty.
`ark backup` and `ark docker backup` pass the machine's hostname unless `--host` says otherwise;
for a remote Docker daemon `ark docker backup` passes the daemon's name, the machine the volume
lives on.

- Not source name alone, because one repository shared by several machines is the normal case and a
  policy run on one of them must not decide about the others' snapshots.
- Not name and host without the type, because the type decides what an entry's metadata is worth:
  a tar entry carries no ctime or inode, so a match against a file source's parent proves nothing.
- Not the hostname as the library default, because an embedder may run where it changes with every
  run, a pod name for one, and each run would open a new series that no policy ever thins.
- Not a series id stored in the manifest, because the three fields are already there, older
  snapshots need no migration and a caller can filter on each of them.
- Not a public series type, because grouping on three exported fields needs no API.

## Consequences

A forget with a keep policy and an empty filter is safe in a shared repository: every series keeps
what the policy says (`TestForgetKeepsTheNewestOfEverySeries`, `TestForgetPolicyAppliesPerSeries`).
A backup never takes a parent across hosts or source types on its own
(`TestBackupParentNeverCrossesHosts`, `TestBackupParentNeverCrossesSourceTypes`), and an explicit
parent of another type lends no file (`TestBackupRereadsRacyAndForeignParentFiles`).
`ark snapshots --last 1` prints one row per series (`TestLastPerSeries`,
`TestSnapshotsLastAndLsPrefix`).

A series that stops receiving backups keeps its last snapshots for good: a renamed machine or a
changed source name starts a new series, the first backup of it has no parent and reads every file,
and the old one is removed by id or with `ark forget --all` and a filter. Snapshots without a host
form a series of their own per source type and name: docker volume snapshots written before
`ark docker backup` recorded a host are such a series, so the first backup after the update has no
parent and reads the volume once unless `--parent` names the last of them. Nothing is migrated: existing snapshots keep the fields they were
written with.
