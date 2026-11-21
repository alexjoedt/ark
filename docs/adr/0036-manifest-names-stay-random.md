# 0036: Manifest names stay random

Status: accepted
Date: 2026-11-21

## Context

A backup finds its parent by listing `snapshots/` and opening every manifest, one `Get` after
another, to pick the newest of its series. That is one round trip per snapshot of every series in
the repository, on every backup. A repository that holds many Docker volumes holds many series.

Repository format version 1 is frozen with the first stable release. A manifest name that says
which series it belongs to would have to exist before then, or never: after the freeze only a
reader can change, not the names already stored.

The proposal was to name a manifest `<keyed hash of the series>-<random>`, so that one listing
finds every snapshot of a series without opening any manifest.

## Decision

Manifest names stay what they are: 128 random bits as 32 lowercase hex characters. The name says
nothing about the series or the time of a snapshot. The format does not change, and no
measurement was taken first.

- Not a series prefix in the name, because it helps only the parent lookup of a backup, while
  forget, `snapshots`, `info` and the mount read every manifest anyway, and because it shows
  whoever can list `snapshots/` how many series the repository holds and how many snapshots each
  one has. A storage observer would see which series is backed up how often, and which manifests
  belong together. Today the listing shows how many snapshots there are in all and the size of
  each manifest, not which of them belong together. The prefix would be keyed, so the series
  names themselves stay hidden, but the grouping would not.
- Not a measurement of parallel reads on sftp before deciding, because the answer does not depend
  on it: a reader can get faster after the freeze, and the names cannot.

## Consequences

The cost of a parent lookup stays one `Get` per manifest. These ways to lower it stay open after
the freeze, because none of them changes what is stored:

- reading the manifests in parallel instead of one after another
- keeping the manifests read once per open repository, for an embedder that backs up many
  volumes through one handle
- a local cache of manifests, and of indexes and trees, on the machine that runs the backup

What a listing of `snapshots/` shows stays what `docs/SECURITY.md` names, counts and sizes, and a
snapshot ID carries neither a series nor a time: `TestListingRevealsCountsAndSizes`,
`TestSnapshotIDsCarryNoTimestamp`.
