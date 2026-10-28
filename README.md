<p align="center">
  <img src="assets/ark-logo.png" alt="ark" width="800" />
</p>

# ark

Deduplicated backups as a Go library, with a CLI on top.

ark backs up anything you can walk as a tree of files: a directory, a Docker volume, a database dump,
an API export. Content is split with FastCDC, stored once, compressed with zstd and encrypted.

- `github.com/alexjoedt/ark` is the library module: the core package `ark`, the backends `arklocal`,
  `arksftp` and `arkmem`, `arkuri`, `arkfs` for directories, the test suites in `arktest` and
  `chunker`.
- `github.com/alexjoedt/ark/arkdocker` adds Docker volumes, in its own module so the Docker SDK stays
  out of the core.
- `github.com/alexjoedt/ark/cmd/ark` is the `ark` binary, a restic-style CLI with an optional web UI.

## Library

```bash
go get github.com/alexjoedt/ark
```

Back up a directory and restore it:

```go
b, err := arklocal.New("/srv/ark")
if err != nil {
	return err
}
pw := ark.WithPassword(os.Getenv("ARK_PASSWORD"))
if err := ark.Init(ctx, b, pw); err != nil { // once per repository
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

`Open` never creates a repository, so a mistyped path fails with `ark.ErrNotInitialized` instead of
starting a second one. Restore, export and verify stream the snapshot: memory stays at the chunk cache
(256 MiB by default) no matter how large the snapshot is.

### Packages

| Package | What it is |
|---|---|
| **Core** | |
| [`ark`](https://pkg.go.dev/github.com/alexjoedt/ark) | Repositories, snapshots, backup, restore, forget, prune, verify, check, export, import, locks |
| [`chunker`](https://pkg.go.dev/github.com/alexjoedt/ark/chunker) | FastCDC content-defined chunking |
| **Backends**, where the repository lives | |
| [`arklocal`](https://pkg.go.dev/github.com/alexjoedt/ark/arklocal) | A local directory, atomic writes through temp file and rename |
| [`arksftp`](https://pkg.go.dev/github.com/alexjoedt/ark/arksftp) | A directory over SFTP, host key checked against known_hosts by default |
| [`arkmem`](https://pkg.go.dev/github.com/alexjoedt/ark/arkmem) | In memory, for tests |
| [`arkuri`](https://pkg.go.dev/github.com/alexjoedt/ark/arkuri) | Opens one of the above from a string |
| **Sources and sinks**, what gets backed up and restored | |
| [`arkfs`](https://pkg.go.dev/github.com/alexjoedt/ark/arkfs) | A local directory, with hardlinks and xattrs |
| [`arkdocker`](https://pkg.go.dev/github.com/alexjoedt/ark/arkdocker) | A Docker volume, and an `Engine` that stops and restarts the containers using it |
| **Testing** | |
| [`arktest`](https://pkg.go.dev/github.com/alexjoedt/ark/arktest) | Conformance suites for your own backends, sources and sinks |

### Backends

A backend stores opaque, encrypted objects. Pick one and hand it to `ark.Init` and `ark.Open`:

```go
b, err := arklocal.New("/srv/ark")

b, err := arksftp.New(arksftp.Config{Host: "nas", User: "backup", Path: "/srv/ark"})

b, err := arkuri.Open(ctx, "sftp://backup@nas/srv/ark") // or "/srv/ark", "local:///srv/ark"

b := arkmem.New() // its objects are gone on Close
```

`arksftp` reads `~/.ssh/config`, so a `Host` alias works. It authenticates with ssh-agent, then
`IdentityFile` or the default keys in `~/.ssh`, then the password in `ARK_SFTP_PASSWORD`. An encrypted
key file is unlocked with `ARK_SFTP_KEY_PASSPHRASE`. Secrets never go into `Config` or a URI.
`arkuri` takes the non-secret options as query parameters: `known_hosts`, `identity` and `insecure`.

Anything else, such as S3 or a NAS API, is an `ark.Backend`: seven methods (`Put`, `Get`, `Exists`,
`Delete`, `List`, `Stat`, `Close`). The suite in `arktest` checks the same contract the built-in
backends pass:

```go
func TestConformance(t *testing.T) {
	arktest.RunBackendTests(t, func(t *testing.T) ark.Backend {
		return mybackend.New()
	})
}
```

### Sources and sinks

A backup reads an `ark.Source` and a restore writes into an `ark.Sink`. `arkfs.Backup` and
`arkfs.Restore` above wrap `repo.Backup(ctx, src)` and `repo.Restore(ctx, id, sink)` for a directory.
A Docker volume goes through `arkdocker`, which stops the containers using the volume and starts them
again:

```go
eng, err := arkdocker.New(repo) // DOCKER_HOST, or arkdocker.WithDockerHost
snap, err := eng.BackupVolume(ctx, "postgres_data")
_, err = eng.RestoreVolume(ctx, snap.ID, "postgres_copy") // created if missing
```

Your own data goes in through `ark.Source`, two methods: `Meta` names the snapshot, `Walk` yields
entries. A matching `ark.Sink` (`Open`, `Receive`, `Close`) brings it back, and
`arktest.RunSourceSinkTests` checks that what the sink receives, the source yields again.

`examples/` has three runnable programs:

| Example | Shows |
|---|---|
| [`examples/filebackup`](examples/filebackup) | a directory into a local repository and back |
| [`examples/dockervolume`](examples/dockervolume) | a Docker volume into a snapshot and into a second volume |
| [`examples/customsource`](examples/customsource) | an `ark.Source` over in-memory files |

### Encryption

Every repository is encrypted and authenticated. The password unlocks a random master key stored in
`keys/`; chunks, trees, snapshot manifests and locks are sealed with XChaCha20-Poly1305 and named by an
HMAC, so the backend sees neither contents nor file names. A changed object fails to open instead of
restoring garbage.

- A lost password is lost data. There is no recovery and no backdoor.
- `ark.WithoutPassword()` creates or opens a repository without one. The format is the same, but the
  master key is stored unprotected: anyone with access to the backend can read it.
- A wrong password fails with `ark.ErrWrongPassword` before any data is read.
- `repo.ChangePassword` seals the same master key under a new password. It does not revoke a key that
  already leaked.
- `Open` derives the key with argon2id and needs 64 MiB of memory for it. Size containers accordingly.

## CLI

```bash
go install github.com/alexjoedt/ark/cmd/ark@latest
```

Every command needs a repository through `--repo` or `ARK_REPOSITORY`. There is no default path.
The password comes from, in this order:

| Source | Flag or variable |
|---|---|
| a file, one trailing newline stripped | `--password-file` or `ARK_PASSWORD_FILE` |
| the environment | `ARK_PASSWORD` |
| a prompt, only on a terminal | `ark init` asks twice |
| no password at all | `--insecure-no-password` or `ARK_INSECURE_NO_PASSWORD=1` |

There is no `--password` flag, it would show in the process list. `ark key passwd` changes the
password and reads the new one from `--new-password-file`, `ARK_NEW_PASSWORD` or a prompt;
`--new-insecure-no-password` removes it.

```bash
export ARK_REPOSITORY=/srv/ark        # or sftp://backup@nas/srv/ark
export ARK_PASSWORD_FILE=/etc/ark/password

ark init
ark backup /etc --tag daily
ark snapshots
ark ls <id>
ark restore <id> /tmp/etc-restore
ark forget --keep-daily 7 --keep-weekly 4 --prune
ark check
```

Docker volumes live under `ark docker`. The daemon comes from `--docker-host` or `DOCKER_HOST`.

```bash
ark docker volumes
ark docker backup postgres_data --comment "before upgrade"
ark snapshots --type docker-volume    # only volume snapshots, file backups have type file
ark docker restore <id>                # into the volume it came from
ark docker restore <id> postgres_copy  # or into another one, created if missing
```

`ark restore` refuses Docker volume snapshots. Use `ark docker restore`, or unpack one into a
directory with `ark export <id> | tar x`.

Destructive commands ask for confirmation on a terminal. Without one they fail unless `--yes` is
set. `--json` prints results on stdout and logs on stderr. `ark <command> --help` lists every flag.

| Exit code | Meaning |
|---|---|
| 0 | success |
| 1 | any other failure |
| 2 | usage error |
| 3 | repository locked |
| 4 | snapshot not found, or an id prefix matches several |
| 5 | verify or check found damage |
| 6 | wrong password |

## Compatibility

From 1.0.0 on, ark follows semantic versioning. All three modules release together under one version
number.

- The exported API of `ark`, `arkfs`, `arklocal`, `arksftp`, `arkuri`, `arkmem`, `arktest`, `chunker`
  and `arkdocker` follows the Go compatibility rules: minors add, only a major removes.
- Every 1.x release reads the v1 on-disk format. Minors may add optional fields.
- CLI flags keep their meaning. `--json` output only gains fields.
- `ark export` writes plain tar, and `ark import` reads it.
- Not covered: dependency versions (including the Docker SDK), the web UI, and `ark serve`.

## Web UI

`ark serve` starts a web UI on `:8080` with scheduled backups and live logs. It is not covered by the
compatibility promise.

```bash
ARK_ADMIN_PASSWORD=... ARK_PASSWORD_FILE=/etc/ark/password ark serve --repo /srv/ark
```

`ark serve` has no terminal to prompt on, so the repository password comes from a file or the
environment, `ARK_PASSWORD` or `ARK_PASSWORD_FILE`, and the repository is created once with
`ark init` before `ark serve` starts.

The SQLite database next to it is a cache for the UI. The repository stays the source of truth.

`scripts/install.sh` installs `ark serve` as a systemd service with its settings in
`/etc/ark/daemon.json`. The admin password only ever comes from `ARK_ADMIN_PASSWORD`, never from a
file.

## Repository layout

```
/srv/ark/
├── config      format version and chunker settings, plaintext
├── keys/       the master key, sealed under the password
├── chunks/     compressed, encrypted content, named by an HMAC, sharded by prefix
├── trees/      one encrypted object per directory, named the same way
├── snapshots/  one encrypted manifest per snapshot, named by its sortable id
└── locks/      encrypted shared and exclusive locks, stale after 30 minutes without a refresh
```

## Development

Four modules in one workspace: the root, `arkdocker/`, `cmd/ark/` and the untagged `examples/`.

```bash
task test    # go test across the workspace, then go mod verify
task vet
task lint    # golangci-lint in each tagged module
task build   # builds the web UI, then bin/ark
task tidy    # tidies every module and syncs the workspace
```

`go build ./...` stops at the root module. Use `go build github.com/alexjoedt/ark/...`.

Releases: [docs/RELEASING.md](docs/RELEASING.md).

## License

Apache License 2.0, see [LICENSE](LICENSE).
