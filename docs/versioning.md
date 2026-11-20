# Versioning and compatibility

ark is on the v0 line. This page says what may change between releases today, and what will be
stable from the first stable release, `v1.1.0`, onward.

## Today: v0

Until `v1.1.0`, a minor release may break things:

| Area | What may change |
|---|---|
| Go API | the exported API of every module, without a deprecation cycle |
| repository format | the on-disk format |
| CLI | flags and `--json` output |

There are no migrations. A repository that an older format wrote has to be created again: run
`ark init` on a new location and back up again. A build refuses a repository of another format
version with a clear error instead of misreading it.

The format version changes only when it has to: when an older build would misread a repository,
or when a newer build needs a field unconditionally. Additive, optional fields keep the number.

## From v1.1.0

The first stable release will be `v1.1.0`. From then on:

| Area | Promise |
|---|---|
| Go API | follows the Go compatibility rules: minor releases add, only a major release removes |
| repository format | every 1.x release reads the 1.x format; minor releases may add optional fields |
| CLI flags | keep their meaning |
| `--json` output | only gains fields |
| `ark export` | writes plain tar, and `ark import` reads it |

The API promise covers the exported API of `ark`, `arkfs`, `arkglob`, `arklocal`, `arksftp`,
`arkuri`, `arkmem`, `arktest`, `chunker`, and `arkdocker`.

Repository format version 1 is frozen with `v1.1.0`. See [Repository format](format.md).

Dependency versions, including the Docker SDK, aren't covered.

## Why not v1.0.0

`v1.0.0` and `v1.0.1` were published before the API and the format had settled. Both are
retracted in all three `go.mod` files, so `go get` doesn't pick them. A version number can't be
reused, because the Go module proxy keeps the old content. That is why the first stable release is
`v1.1.0`.

## Modules

ark ships three modules under one version number:

| Module | Holds |
|---|---|
| `github.com/alexjoedt/ark` | the library |
| `github.com/alexjoedt/ark/arkdocker` | Docker volumes |
| `github.com/alexjoedt/ark/cmd/ark` | the `ark` command |

A release tags all three with the same version. Use the same version of the library and of
`arkdocker` in one program.

How a release is cut is described in
[releasing.md](https://github.com/alexjoedt/ark/blob/main/docs/releasing.md) in the repository.
