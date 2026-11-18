<p align="center">
  <img src="assets/ark-logo.png" alt="ark" width="800" />
</p>

# ark

Deduplicated backups as a Go library, with a CLI on top.

**Documentation: <https://alexjoedt.github.io/ark/>**

> [!WARNING]
> ark is under active development. Breaking changes are expected: the Go API, the CLI and the
> on-disk repository format can change between releases without a migration path. Don't rely on it
> as your only backup yet.

ark backs up anything you can walk as a tree of files: a directory, a Docker volume, a database dump,
an API export. Content is split with FastCDC, stored once, compressed with zstd and encrypted.

- `github.com/alexjoedt/ark` is the library module: the core package `ark`, the backends `arklocal`,
  `arksftp` and `arkmem`, `arkuri`, `arkfs` for directories, `arkprofile`, `arkfind`, `arkglob`, the
  test suites in `arktest` and `chunker`.
- `github.com/alexjoedt/ark/arkdocker` adds Docker volumes, in its own module so the Docker SDK stays
  out of the core.
- `github.com/alexjoedt/ark/cmd/ark` is the `ark` binary, a restic-style CLI.

## Install

Download the archive for your platform from the
[GitHub releases](https://github.com/alexjoedt/ark/releases), or build the CLI from source:

```bash
go install github.com/alexjoedt/ark/cmd/ark@latest
```

The library:

```bash
go get github.com/alexjoedt/ark
```

## Quick start

CLI:

```bash
export ARK_REPOSITORY=/srv/ark        # or sftp://backup@nas/srv/ark
export ARK_PASSWORD_FILE=/etc/ark/password

ark init
ark backup /etc --tag daily
ark snapshots
ark restore <id> /tmp/etc-restore
ark forget --keep-daily 7 --keep-weekly 4 --prune
```

Library:

```go
b, err := arklocal.New("/srv/ark")
if err != nil {
	return err
}
pw := ark.NewPassword(os.Getenv("ARK_PASSWORD"))
if err := ark.Init(ctx, b, pw); err != nil && !errors.Is(err, ark.ErrAlreadyInitialized) {
	return err
}
repo, err := ark.Open(ctx, b, pw)
if err != nil {
	return err
}
defer repo.Close()

snap, err := arkfs.Backup(ctx, repo, "/home/me/docs", ark.WithTags("daily"))
if err != nil {
	return err
}
_, err = arkfs.Restore(ctx, repo, snap.ID, "/tmp/docs")
```

## Documentation

- [Getting started](https://alexjoedt.github.io/ark/getting-started): repository, password, first backup, restore, retention
- [CLI](https://alexjoedt.github.io/ark/cli/): every command and flag, [profiles](https://alexjoedt.github.io/ark/cli/profiles), exit codes
- [Library](https://alexjoedt.github.io/ark/library): packages, backends, sources and sinks, encryption, examples
- [Mount](https://alexjoedt.github.io/ark/MOUNT), [Series](https://alexjoedt.github.io/ark/SERIES), [Labels](https://alexjoedt.github.io/ark/LABELS)
- [Repository format](https://alexjoedt.github.io/ark/FORMAT) and [Security](https://alexjoedt.github.io/ark/SECURITY)
- [Architecture decisions](https://alexjoedt.github.io/ark/adr/) and [Releasing](https://alexjoedt.github.io/ark/RELEASING)

The same pages are in [docs/](docs/).

## Development

Four modules in one workspace: the root, `arkdocker/`, `cmd/ark/` and the untagged `examples/`.

```bash
task test    # go test across the workspace, then go mod verify
task vet
task lint    # golangci-lint in each tagged module
task build   # builds bin/ark
task tidy    # tidies every module and syncs the workspace
```

`go build ./...` stops at the root module. Use `go build github.com/alexjoedt/ark/...`.

## License

Apache License 2.0, see [LICENSE](LICENSE).

## Disclaimer

Use ark at your own risk. You are responsible for your data, including verifying that backups are
complete and restorable. The developer and contributors accept no liability for data loss,
corruption or any other damage resulting from the use of this software. See the warranty and
liability sections of the [LICENSE](LICENSE).
