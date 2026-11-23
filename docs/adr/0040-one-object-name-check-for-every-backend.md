# 0040: One object name check for every backend

Status: accepted
Date: 2026-11-23

## Context

Each backend checked object names on its own, and the checks drifted. arklocal and arksftp
refused `.`, `..`, a `/` or `\` and their own temporary prefix, but let `..x` through: a pack
named `..x` lands in the shard `..`, which is the namespace's parent, so `..x` as a data pack and
as a tree pack were the same file. arkmem refused only the empty name, so a name that failed on
disk worked in memory, and a test on arkmem could pass with a name no file backend stores. The
conformance suite in arktest checked none of it, so an out-of-tree backend had no way to learn
the rule.

docs/format.md already said that no object of the format has a name that starts with `.`: every
name ark writes is hex or a fixed word, and the backends' temporary files start with `.ark-tmp-`.

## Decision

- `ark.CheckName` in the root package holds the rule: an object name is not empty, does not start
  with `.`, and holds no `/` or `\`. The rule belongs to the format, so it lives beside `Handle`
  and not in one backend.
- arklocal, arksftp, arkmem and the custom backend example call it on every handle; their own
  checks are gone. The `Backend` contract says a method taking a handle refuses what `CheckName`
  refuses, and that a method called with a done context returns an error wrapping it.
- arktest refuses `..x`, `.`, `..`, `.ark-tmp-x`, `a/b` and `a\b` on every method that takes a
  handle in every namespace, and calls every method on a cancelled context.

Rejected options:

- Not a check inside the repository before it calls the backend, because a backend is a public
  API that other code calls too, and the escape is a property of the backend's layout.
- Not a stricter rule such as hex names only, because lock, key and config names are not hex, and
  a backend has no reason to know which namespace holds which kind of name.

## Consequences

- A leading `.` stays free for a backend's own files: temporary objects now, and the names a
  backend sets aside, which its listing passes over.
- A backend written against ark gets the rule from one call and the suite tells it when it is
  missing. The method set of `Backend` is unchanged.
- The arktest names for chunks and packs are SHA-256 hashes, so they spread over the shards of a
  sharding backend instead of all landing in `ff`.
- Tests pin it: `TestCheckName` and the `RefusesInvalidNames` and `CancelledContext` subtests of
  `RunBackendTests`, run by every backend's `conformance_test.go`.
