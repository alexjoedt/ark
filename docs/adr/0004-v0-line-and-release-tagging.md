# 0004: v0 line and release tagging

Status: accepted
Date: 2026-11-07

## Context

`v1.0.0` and `v1.0.1` were published before the exported API and the on-disk format had settled.
The Go module proxy caches a tag for good, even when it is deleted from the repository, so a
published tag cannot be taken back. At the same time ark has no users yet, and the
hardening work (crash safety, concurrent writers, performance, security) needs room to change the
API and the format without a deprecation cycle.

## Decision

- `v1.0.0` and `v1.0.1` are retracted in the `go.mod` of each tagged module (`go.mod`,
  `arkdocker/go.mod`, `cmd/ark/go.mod`). `examples` is never tagged and needs no `retract`. A
  `retract` directive is the only fix the proxy leaves.
- ark runs on the v0 line. Exported API, CLI flags and `--json` output may change between minors.
- The on-disk format may change, but `repoFormatVersion` (`config.go`) is bumped only when an older
  build would misread a repository or a newer build needs a field unconditionally. Additive,
  optional fields keep the number. There are no migrations before the freeze: a repository an
  older format wrote is re-created.
- Each finished body of work that ships code (a feature, a hardening pass) gets one minor tag,
  not every merge. Docs-only work gets none.
- `v1.1.0` is the first stable release and freezes the API (Go compatibility rules) and the 1.x
  on-disk format. Majors from 2 on are refused, since the module paths carry no `/vN` suffix.
- Not deleting the bad tags and re-tagging `v1.0.0`, because the proxy keeps serving the first
  content it saw.
- Not `v1.0.2` and on, because a v1 line promises compatibility the project cannot keep yet.

## Consequences

Breaking changes before `v1.1.0` are cheap and need no migration code. Embedders pin a minor and
read the release notes of the next one. `docs/RELEASING.md` carries the same rules for the person
cutting a tag, and `scripts/release.sh` tags the three modules in lockstep. The cost is that nothing is stable until `v1.1.0`, and the retracted versions stay
visible in `go list -m -versions` for good.
