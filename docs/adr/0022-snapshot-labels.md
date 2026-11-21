# 0022: Snapshot labels

Status: partly superseded by 0035
Date: 2026-11-16

## Context

A snapshot carried key-value labels only from its source: `SourceMeta.Labels`, stored in the
manifest as `source_meta` and returned as `Snapshot.SourceMeta`. arkdocker filled them with the
Docker host and the containers that used a volume. A caller had no way to set a label or to find
snapshots by one, while tags are plain strings without a value. Two backup jobs of the same source
told apart by an application name or an environment needed a key-value pair that a filter can match
exactly.

## Decision

A caller sets labels with `ark.WithLabels(map[string]string)` and finds snapshots with
`SnapshotFilter.Labels`, which matches when every pair is on the snapshot with exactly that value.
`Snapshot.Labels` replaces `Snapshot.SourceMeta` and holds the source's labels with the caller's
added. The manifest keeps the key `source_meta`, so snapshots written before keep their labels and
the format version stays.

A key follows the Kubernetes label key syntax: an optional prefix, a lower-case DNS subdomain of at
most 253 characters, and a slash, then a name of 1 to 63 letters, digits, `-`, `_` or `.`, starting
and ending with a letter or digit. A value is UTF-8 of at most 1 KiB without control characters and
may be empty. Keys with the prefix `ark/` are reserved for the labels ark's own sources record. A
caller's key that the source sets as well is refused rather than overriding the source.
`ark.ValidateLabels` checks a caller's labels, so the CLI and profiles reject them before a
repository opens, and `Backup` runs the same check before it writes anything; both errors wrap
`ErrInvalidLabel`. Labels from a source are stored as the source sets them.

- Not the Kubernetes value rules (63 characters out of the key name's character set), because
  values such as a Docker host address or an image reference break them.
- Not free keys, because a key ends up in CLI arguments, profile files and later in mount paths,
  where spaces and leading dashes cause trouble, and a known syntax is easier to explain.
- Not a caller label overriding a source label, because the source's value records what was backed
  up and a silent override would hide it.
- Not tags folded into value-less labels, because tags are a separate manifest field already,
  restic users know them, and folding them would need a migration of every manifest's `tags`.
- Not a new manifest field for caller labels, because one map keeps a filter and the sink's view
  simple and needs no change to the format.

## Consequences

Validation is tested key by key (`TestBackupRejectsInvalidLabels`), as is the exact AND filter
(`TestSnapshotFilterMatchesLabels`, `TestBackupLabelsAreStoredAndFiltered`) and the manifest key
(`TestManifestKeepsLabelsUnderLabels`, since 0035 renamed the key to `labels`). A Sink sees the merged labels in
`SourceMeta.Labels` on restore. Labels are set at backup only; changing them later needs a manifest
rewrite of its own. The filter knows equality only; selector operators such as `!=` or `in` can be
added beside it. Code that read `Snapshot.SourceMeta` now reads `Snapshot.Labels`, an API break on
the v0 line.
