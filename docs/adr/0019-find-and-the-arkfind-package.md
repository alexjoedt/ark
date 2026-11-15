# 0019: Find and the arkfind package

Status: accepted
Date: 2026-11-15

## Context

Users ask which snapshots hold a file and where it sat. Embedders such as a cluster service want
the same search without the CLI, and the root package's API is frozen since v1.1.0. Open were
where the search lives, its signature, what makes an entry a match, the order and grouping of the
results, how errors behave and what a search over many snapshots costs.

## Decision

Find lives in a new root package `arkfind`, standard library plus `ark` and `arkglob`, built only
on `Repository.Snapshots` and `Repository.Walk`, beside `arkglob` (ADR 0008) and `arkprofile`
(ADR 0018):

```go
func Find(ctx context.Context, repo *ark.Repository, filter ark.SnapshotFilter,
	patterns []string, fn func(Match) error) error

type Match struct {
	Snapshot *ark.Snapshot
	File     ark.FileInfo
}
```

An entry matches when the `arkglob.Matcher` compiled from the patterns accepts its path, the rule
excludes use and restic's find uses: a pattern without a `/` matches a base name anywhere, one
with a `/` matches any run of components, a leading `/` anchors at the snapshot root, and a
matched directory yields every entry below it. `File` is the entry as Walk yields it, a directory
with its trailing slash. Snapshots are searched newest first, the order of `Snapshots`, and the
matches of one snapshot arrive together in tree order, so a caller groups them without buffering.
No patterns or a bad one wrap `path.ErrBadPattern` before the repository is read. An error from
`fn` ends the search and is returned as is, and a cancelled context ends it with the context's
error. A snapshot forgotten between the list and its walk is skipped; it is told apart from a
missing tree by looking the snapshot up again. Any other walk error ends the search and names the
snapshot.

- Not a `Repository` method, because the root API is frozen and the search needs nothing
  unexported.
- Not a returned slice, because a search over many snapshots can match a large tree whole, and a
  callback lets the CLI print as it goes and stop early.
- Not oldest first as restic prints, because every other listing in ark is newest first.
- Not an exact-name match mode in `arkglob`, because restic lists a matched directory's contents
  too and the matcher's API stays as small as it is.
- Not one lock over the whole search, because Walk takes its own and the public API offers no
  other; each Walk takes the shared lock and loads the tree index again.

## Consequences

A find over N snapshots costs one read of the manifests and N walks, each with a lock, a tree
index load and a read of every tree node of the snapshot; no file content is read. A prune can run
between two walks, which the skip of a forgotten snapshot absorbs. Tests in
`arkfind/arkfind_test.go` pin the order and grouping (`TestFindNewestFirstGroupedInTreeOrder`),
the match rule (`TestFindMatchRule`), the filter (`TestFindFilter`) and the error behaviour
(`TestFindBadPatternsTouchNoRepository`, `TestFindStopsOnCallbackError`,
`TestFindReturnsCancellationUnwrapped`, `TestFindSkipsSnapshotForgottenDuringSearch`,
`TestFindNamesSnapshotOnWalkError`).
