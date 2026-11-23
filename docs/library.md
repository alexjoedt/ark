# Library

ark is a Go library first. The `ark` command is one program built on it, and your program can use
the same packages: open a repository, back up a directory, a Docker volume, or a stream, restore
it, and apply a retention policy.

This page gets you to a first backup and gives you the map. The pages after it go into one topic
each:

- [Backends](library/backends.md): where a repository lives, and how to write your own backend.
- [Sources and sinks](library/sources-and-sinks.md): what a backup reads and a restore writes, and
  how to write your own.
- [Forget policies](library/forget.md): retention from Go.
- [Profiles](library/profiles.md): the CLI's profile format as a package.
- [Progress and logging](library/observing.md): progress events, lock events, logs, and backend
  statistics.

The full API is on [pkg.go.dev](https://pkg.go.dev/github.com/alexjoedt/ark).

## Install

```bash
go get github.com/alexjoedt/ark
go get github.com/alexjoedt/ark/arkdocker   # only for Docker volumes
```

ark ships as three modules, so the Docker SDK stays out of programs that don't need it:

| Module | Holds |
|---|---|
| `github.com/alexjoedt/ark` | the core package `ark`, the backends, `arkfs`, `arkprofile`, `arkfind`, `arkglob`, `arktest`, and `chunker` |
| `github.com/alexjoedt/ark/arkdocker` | Docker volumes |
| `github.com/alexjoedt/ark/cmd/ark` | the `ark` binary, see [CLI](cli/index.md) |

## Back up and restore a directory

```go
if err := os.MkdirAll("/srv/ark", 0o700); err != nil {
	return err
}
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

Three steps:

1. **Pick a backend.** A backend is where the repository lives. `arklocal` is a local directory.
2. **Create and open the repository.** `ark.Init` creates it once, `ark.Open` opens it.
3. **Back up and restore.** `arkfs.Backup` reads a directory, `arkfs.Restore` writes one.

`Init` and `Open` are separate on purpose:

- `Init` on an existing repository fails with `ark.ErrAlreadyInitialized`. A program that runs
  every day can call `Init` each time, as above, and let the first run create the repository.
- `Open` never creates a repository. A mistyped path fails with `ark.ErrNotInitialized` instead of
  quietly starting a second repository.

`arkfs.Backup` and `arkfs.Restore` are shortcuts. Underneath, a backup is
`repo.Backup(ctx, source)` and a restore is `repo.Restore(ctx, snapshotID, sink)`, with a
directory as the source and the sink. See [Sources and sinks](library/sources-and-sinks.md).

## What a repository does

Everything else is a method on `*ark.Repository`:

| Task | Methods |
|---|---|
| back up and restore | `Backup`, `Restore` |
| list snapshots and their files | `Snapshots`, `Snapshot`, `Walk` |
| read a snapshot as a file system | `OpenReader`, then `Reader.FS` |
| remove snapshots and free space | `Forget`, `Prune` |
| check for damage | `Verify`, `Check` |
| rebuild the index | `RebuildIndex` |
| move snapshots between repositories | `Export`, `Import` |
| report the size | `Info` |
| change the password | `ChangePassword` |
| list and remove locks | `Locks`, `Unlock` |

## Snapshot IDs

A snapshot ID has its own type, `ark.SnapshotID`. `Snapshot.ID` is one. A string converts as it
is: `ark.SnapshotID("3f9a")`.

Wherever a method takes an ID, a unique prefix works too, in upper or lower case. That covers
`Restore`, `Snapshot`, `Verify`, `Walk`, `Export`, `Reader.FS`, `WithParent`, and the
`SnapshotFilter.IDs` passed to `Snapshots` or `Forget`.

| Prefix matches | Error wraps |
|---|---|
| no snapshot | `ark.ErrNotFound` |
| several snapshots | `ark.ErrAmbiguousID` |

`SnapshotFilter.Match` is the exception. It resolves nothing: called on its own, it compares `IDs`
with the full, lowercase `Snapshot.ID` exactly.

## Passwords and encryption

Every repository is encrypted and authenticated. The password unlocks a random master key that is
stored in the repository, and that key seals everything else. The backend sees neither file
contents nor file names.

```go
pw := ark.NewPassword("correct horse battery staple")
```

What to know when you handle passwords:

- **A lost password is lost data.** There is no recovery and no backdoor.
- **A wrong password** fails with `ark.ErrWrongPassword` before any data is read.
- **`repo.ChangePassword`** seals the same master key under a new password. It doesn't revoke a
  key that already leaked.
- **Key derivation needs memory.** `Init`, `Open`, and `ChangePassword` derive the key with
  argon2id and need 64 MiB for it. Size containers accordingly.

`ark.NoPassword()` creates or opens a repository without a password. The format is the same, but
the master key is stored unprotected, so anyone with access to the backend can read the
repository. `Init` records the choice, and `Open` holds you to it:

| Repository | Opened with | Error |
|---|---|---|
| protected | `ark.NoPassword()` | `ark.ErrPasswordRequired` |
| unprotected | a password | `ark.ErrNotPasswordProtected` |

[Security](SECURITY.md) has the threat model, the accepted risks, and how to report a
vulnerability.

## Host, series, and labels

Source type, source name, and host make up a snapshot's [series](series.md). A backup takes the
newest snapshot of its series as the parent, and `Forget` applies a policy once per series.

The core library records no host unless you pass `ark.WithHost`. If several machines share one
repository, set a host. Without one, their snapshots of the same source fall into one series.

Labels are key-value pairs stored with a snapshot. Set them with `ark.WithLabels` and match them
with `SnapshotFilter.Labels`:

```go
snap, err := arkfs.Backup(ctx, repo, "/home/me/docs",
	ark.WithHost("laptop"),
	ark.WithLabels(map[string]string{"app": "docs"}),
)
if err != nil {
	return err
}
snaps, err := repo.Snapshots(ctx, ark.SnapshotFilter{
	Host:   "laptop",
	Labels: map[string]string{"app": "docs"},
})
```

Keys that start with `ark/` belong to ark's own sources. A backup that sets one fails with
`ark.ErrInvalidLabel`. [Labels](labels.md) has the key rules and the reserved keys.

### Hostnames and locks

The library does read the machine's hostname for one thing: locks. It stamps the locks it writes
with the hostname, so `Locks` and lock errors can name the holder. A lock counts as stale only
once it is 30 minutes old, also one of the same hostname.

`arkprofile` and the CLI differ from the core here. Both always record a host. Without
`backup.host`, a profile's `BackupOptions` and `Filter` use the machine's hostname. In a container
whose hostname changes on every deploy, set `backup.host`. Otherwise each deploy starts a new
series, and `Forget` no longer sees the older snapshots.

## Find files

`arkfind.Find` searches the snapshots that a filter selects, newest first, for entries whose path
matches a pattern:

```go
err := arkfind.Find(ctx, repo, ark.SnapshotFilter{Host: "laptop"}, []string{"*.pdf"},
	func(m arkfind.Match) error {
		fmt.Println(m.Snapshot.ID, m.File.Path)
		return nil
	})
```

- Patterns follow the exclude rules. Without a `/`, a pattern matches a base name at any depth. A
  leading `/` anchors it at the snapshot root. A matched directory yields everything below it.
- No file content is read.
- The callback runs while the repository's shared lock is held. It must not call `Forget`
  (unless as a dry run), `Prune`, `RebuildIndex`, `ChangePassword`, or any other operation that
  takes the exclusive lock.

## Repository size

`repo.Info` reports how many snapshots a repository holds and how large it is:

```go
info, err := repo.Info(ctx, ark.WithInfoExact())
if err != nil {
	return err
}
fmt.Printf("%d snapshots, %d of %d bytes unused\n",
	info.Snapshots, info.BytesFreed+info.BytesUnused, info.Stored.Total())
```

Without options, `Info` is cheap:

- It counts the snapshots and sums their `Stats.TotalSize`. That sum isn't deduplicated: a file in
  ten snapshots counts ten times.
- It adds up the object sizes that the backend lists, per kind, in `Stored`: data packs, tree
  packs, indexes, snapshots, keys, and other. `Stored.Total()` is the size of the repository.
- It reads the snapshot manifests but no pack and no index, and it takes no lock. A manifest that
  is deleted while it runs is left out.

`ark.WithInfoExact()` adds the unused space. It runs a prune dry run, so it reads every tree and
index, holds the exclusive lock like `Prune`, and reports progress as a prune does.

| Field | Meaning |
|---|---|
| `BytesFreed` | unused bytes that a prune deletes |
| `BytesUnused` | unused bytes that a prune leaves in place |

Together they are all the unused space.

## Memory use

Restore and export stream the snapshot. Memory stays at the chunk cache plus one chunk per worker,
no matter how large the snapshot is.

| Operation | Memory |
|---|---|
| `Restore` | the chunk cache, 256 MiB by default; `ark.WithRestoreCache` changes it |
| `Export` | the chunk cache at its default size |
| `Verify` | about 150 bytes per distinct chunk of the snapshot, plus one pack range of up to about 20 MiB |

`Verify` reads pack by pack instead of file by file, so its memory grows with the snapshot.

## Packages

| Package | What it is |
|---|---|
| **Core** | |
| [`ark`](https://pkg.go.dev/github.com/alexjoedt/ark) | repositories, snapshots, backup, restore, forget, prune, verify, check, export, import, locks |
| [`chunker`](https://pkg.go.dev/github.com/alexjoedt/ark/chunker) | FastCDC content-defined chunking |
| **Backends** | |
| [`arklocal`](https://pkg.go.dev/github.com/alexjoedt/ark/arklocal) | a local directory |
| [`arksftp`](https://pkg.go.dev/github.com/alexjoedt/ark/arksftp) | a directory over SFTP |
| [`arkmem`](https://pkg.go.dev/github.com/alexjoedt/ark/arkmem) | memory, for tests |
| [`arkuri`](https://pkg.go.dev/github.com/alexjoedt/ark/arkuri) | opens one of the above from a string |
| **Sources and sinks** | |
| [`arkfs`](https://pkg.go.dev/github.com/alexjoedt/ark/arkfs) | a local directory, with hardlinks, xattrs, and excludes |
| [`arkdocker`](https://pkg.go.dev/github.com/alexjoedt/ark/arkdocker) | a Docker volume, and an `Engine` that stops and restarts the containers that use it |
| **Tools on top** | |
| [`arkprofile`](https://pkg.go.dev/github.com/alexjoedt/ark/arkprofile) | profiles: a stored repository, source, and forget policy, in code or YAML |
| [`arkfind`](https://pkg.go.dev/github.com/alexjoedt/ark/arkfind) | searches snapshots for paths that match a pattern |
| [`arkglob`](https://pkg.go.dev/github.com/alexjoedt/ark/arkglob) | matches snapshot paths against exclude patterns with restic's rules |
| **Testing** | |
| [`arktest`](https://pkg.go.dev/github.com/alexjoedt/ark/arktest) | conformance suites for your own backends, sources, and sinks |

## Examples

Each example is a `main` package in the
[examples directory](https://github.com/alexjoedt/ark/tree/main/examples). Run one with
`go run ./<name>` from there.

| Example | Shows |
|---|---|
| [`filebackup`](https://github.com/alexjoedt/ark/tree/main/examples/filebackup) | a directory into a local repository and back |
| [`dockervolume`](https://github.com/alexjoedt/ark/tree/main/examples/dockervolume) | a Docker volume into a snapshot and into a second volume |
| [`streambackup`](https://github.com/alexjoedt/ark/tree/main/examples/streambackup) | a command's stdout into a snapshot and back to stdout |
| [`customsource`](https://github.com/alexjoedt/ark/tree/main/examples/customsource) | an `ark.Source` over in-memory files |
| [`customsink`](https://github.com/alexjoedt/ark/tree/main/examples/customsink) | an `ark.Sink` that restores a snapshot into a zip archive |
| [`custombackend`](https://github.com/alexjoedt/ark/tree/main/examples/custombackend) | an `ark.Backend` over a key-value store, with its conformance test |
| [`observe`](https://github.com/alexjoedt/ark/tree/main/examples/observe) | progress, lock events, and backend statistics logged through slog |
| [`profile`](https://github.com/alexjoedt/ark/tree/main/examples/profile) | a backup and a forget from a profile built in code, and a scheduler's configuration that embeds a profile |
