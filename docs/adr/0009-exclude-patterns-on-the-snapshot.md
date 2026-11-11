# 0009: Exclude patterns on the snapshot

Status: accepted
Date: 2026-11-11

## Context

A backup that skips paths by pattern holds less than its source, and a reader of the snapshot
cannot tell. restic keeps the `excludes` of a backup on its snapshots. ark's snapshot records what
the source reports through `SourceMeta`: a name, a type and string labels. The patterns could ride
along as a label or get a field of their own.

## Decision

`SourceMeta` gains `Excludes []string`. Backup writes it to the manifest as `excludes`, omitted when
empty, and `Snapshot.Excludes` carries it out through Backup, Snapshot, Snapshots and Import, as a
copy so a caller cannot change the stored value. The manifest version and `repoFormatVersion` stay
as they are: older builds decode manifests without rejecting unknown fields and ignore it.

- Not a source label, because a label is a string and a list would need an encoding every reader
  has to know, and a label of the same name could be set by hand and mistaken for the patterns.
- Not a format bump, because the field is optional and no older build depends on it.
- Not recording which flags the patterns came from, because the patterns are what decides which
  paths are missing.

## Consequences

`inspect` and embedders read the patterns as a typed list. The patterns are snapshot metadata, so
they are sealed with the manifest and `TestBackendHoldsNoPlaintext` looks for them in the backend.
A source that filters without reporting its patterns leaves the list empty; the list describes
what the source says it excluded, not what it did. Pinned by `TestSnapshotCarriesTheSourceExcludes`.
