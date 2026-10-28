# Releasing

ark ships three modules under one version number: `github.com/alexjoedt/ark` (the library),
`arkdocker` and `cmd/ark`. A release is three tags on `main`, plus the CLI archives built from the
`cmd/ark` tag.

## Compatibility promise for 1.x

- The exported API of `ark`, `arkfs`, `arklocal`, `arksftp`, `arkuri`, `arkmem`, `arktest`, `chunker`
  and `arkdocker` follows the Go compatibility rules: minors add, only a major removes.
- Every 1.x release reads the v1 on-disk format. Minors may add optional fields.
- CLI flags keep their meaning. `--json` output only gains fields.
- `ark export` writes plain tar, and `ark import` reads it.
- Not covered: dependency versions (including the Docker SDK), the web UI, and `ark serve`.

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
git tag -a v1.0.0 -m "ark v1.0.0" && git push origin v1.0.0
```

The `release` workflow (`.github/workflows/release.yml`) takes it from there. It checks that the tag
is the tip of `main`, runs `scripts/release.sh --push 1.0.0` for the two nested tags, then builds
and publishes the archives from the `cmd/ark` tag. If a step fails, fix the cause and re-run the
job: tags that exist are skipped, and the bump commits of the failed run pass the tip check.

Majors from 2 on are refused, since the module paths carry no `/vN` suffix.

### Publishing

goreleaser runs inside `cmd/ark` (`cmd/ark/.goreleaser.yaml`). It builds the web UI, the binaries
with `CGO_ENABLED=0` for linux and darwin, and the archives.

goreleaser OSS cannot strip the `cmd/ark/` prefix from a tag. The release therefore runs under the
root tag of the same version (`GORELEASER_CURRENT_TAG=vX.Y.Z`) with `--skip=validate`, because that
tag does not point at HEAD. The GitHub release lands on `vX.Y.Z`, and `ark version` prints `X.Y.Z`.
A version with a prerelease suffix such as `1.0.0-rc.1` becomes a GitHub prerelease.

### Without CI

The same steps by hand, with goreleaser v2 and a clean tree on `main`:

1. Preview: `scripts/release.sh --dry-run 1.0.0`
2. Tag and push: `scripts/release.sh --push 1.0.0`
3. `git checkout cmd/ark/v1.0.0`
4. `GITHUB_TOKEN=<token with contents:write> task release-publish`
5. `git checkout main`

`task release-publish` checks for a clean tree and a `cmd/ark/v*` tag at HEAD before it calls
goreleaser. `task release-snapshot` builds everything locally without publishing.
