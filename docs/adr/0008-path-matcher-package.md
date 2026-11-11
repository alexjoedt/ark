# 0008: Path matcher package

Status: accepted
Date: 2026-11-11

## Context

A backup of a directory should skip paths by pattern, and a search across snapshots matches
snapshot paths too, including those of a Docker volume. ark had no path matcher: restore only
selects by path prefix. Users who come from restic bring exclude files written for its rules, so
the rules were open as well as where the matcher lives and how much API it exposes.

## Decision

The matcher is its own package, `arkglob`, in the root module, standard library only. It exports
`New(patterns ...string) (*Matcher, error)` and `(*Matcher).Match(path string) bool` and nothing
else; a Matcher without patterns matches nothing.

The rules are restic's: pattern and path are split on `/` and compared component by component with
`path.Match`; `**` matches zero or more components; a pattern without a leading `/` matches any
consecutive run of components; a trailing `/` is ignored; a path below a matched path matches, so a
file inside an excluded directory needs no second check. Two rules differ from restic. A leading
`/` anchors at the root of the matched tree, the source directory, because snapshot paths are
relative to it. A pattern starting with `!` is rejected: restic re-includes with it, and read
literally an exclude file written for restic would silently exclude more than intended. An empty
pattern and bad syntax are rejected too, every rejection wraps `path.ErrBadPattern` and names the
pattern, so `Match` never returns an error. A `.` or `..` component is rejected instead of cleaned,
so `./foo` is an error rather than an unanchored `foo`. The root of the tree never matches, so `**`
excludes everything below the source directory but not the directory itself.

- Not in `arkfs`, because a search over Docker volume snapshots needs the matcher without the
  directory source.
- Not in the root package `ark`, because it would grow the engine API for a helper the engine
  never calls.
- Not under `internal`, because embedders building their own sources should filter with the same
  rules as ark.
- Not `filepath.Match`, because snapshot paths are slash paths on every OS.
- Not negation with `!`, because it makes the result depend on pattern order; it can be added
  later without changing what an accepted pattern means.

## Consequences

Every filter and search in ark answers with the same rules, and restic exclude files port as long
as they use relative patterns and no `!`. The package is part of the compatibility promise, so its
rules cannot change in a minor release. Matching tracks reachable positions instead of backtracking on `**`,
so its cost is linear in pattern length times path depth. Pinned by `TestMatch` and `TestNewRejectsBadPatterns` in `arkglob_test.go`.
