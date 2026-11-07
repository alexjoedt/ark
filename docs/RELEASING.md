# Releasing

ark ships three modules under one version number: `github.com/alexjoedt/ark` (the library),
`arkdocker` and `cmd/ark`. A release is three tags on `main`, plus the CLI archives built from the
`cmd/ark` tag.

## Versioning: v0.x

ark is on the v0 line. Until `v1.0.0` is cut again:

- The exported API of every module may change between minors without a deprecation cycle.
- The on-disk format may change. `repoFormatVersion` (config.go) is bumped only when an older build
  would misread a repository or a newer build needs a field unconditionally; additive, optional
  fields keep the number. There are no migrations: a repository an older format wrote is re-created.
- CLI flags and `--json` output may change.
- Each finished roadmap epic gets a minor tag.

`v1.0.0` and `v1.0.1` are retracted in all three `go.mod` files: they were published before the API
and the format had settled. The first stable release will be `v1.1.0`, and from then on the API
follows the Go compatibility rules, every 1.x release reads the 1.x on-disk format, flags keep
their meaning and `--json` output only gains fields.

## Tag order

| Order | Tag | Why here |
|---|---|---|
| 1 | `vX.Y.Z` | The library requires nothing from the other two. |
| 2 | `arkdocker/vX.Y.Z` | Requires the root at `vX.Y.Z`; its tidy fetches that tag from the remote. |
| 3 | `cmd/ark/vX.Y.Z` | Requires both. The tag triggers the release build. |

`examples/` gets its requires bumped too but is never tagged. Every bump is its own commit, so the
three tags sit on three different commits.

`scripts/release.sh` does all of it: tag the root, bump and tidy `arkdocker` outside the workspace,
commit, tag, then the same for `cmd/ark` and `examples`. Each tidy resolves the previous tag from
the remote, so a real run needs `--push`: it pushes `main` and each tag before the next step.
Existing tags are skipped, so after a failure you fix the cause and rerun the same command.

## Cutting a release

Run `task test` on `main`, then tag the tip of `main` with the root tag and push it:

```bash
git tag -a v0.1.0 -m "ark v0.1.0" && git push origin v0.1.0
```

The `release` workflow (`.github/workflows/release.yml`) takes it from there. It checks that the tag
is the tip of `main`, runs `scripts/release.sh --push 0.1.0` for the two nested tags, then builds
and publishes the archives from the `cmd/ark` tag. If a step fails, fix the cause and re-run the
job: tags that exist are skipped, and the bump commits of the failed run pass the tip check.

Majors from 2 on are refused, since the module paths carry no `/vN` suffix.

### Publishing

goreleaser runs inside `cmd/ark` (`cmd/ark/.goreleaser.yaml`). It builds the web UI, the binaries
with `CGO_ENABLED=0` for linux and darwin, and the archives.

goreleaser OSS cannot strip the `cmd/ark/` prefix from a tag. The release therefore runs under the
root tag of the same version (`GORELEASER_CURRENT_TAG=vX.Y.Z`) with `--skip=validate`, because that
tag does not point at HEAD. The GitHub release lands on `vX.Y.Z`, and `ark version` prints `X.Y.Z`.
A version with a prerelease suffix such as `0.1.0-rc.1` becomes a GitHub prerelease.

### Without CI

The same steps by hand, with goreleaser v2 and a clean tree on `main`:

1. Preview: `scripts/release.sh --dry-run 0.1.0`
2. Tag and push: `scripts/release.sh --push 0.1.0`
3. `git checkout cmd/ark/v0.1.0`
4. `GITHUB_TOKEN=<token with contents:write> task release-publish`
5. `git checkout main`

`task release-publish` checks for a clean tree and a `cmd/ark/v*` tag at HEAD before it calls
goreleaser. `task release-snapshot` builds everything locally without publishing.
