# Library

ark is a Go library first; the `ark` CLI is one program built on it. This page walks through the packages and the calls an embedder needs. The full API is on [pkg.go.dev](https://pkg.go.dev/github.com/alexjoedt/ark), and the [examples directory](https://github.com/alexjoedt/ark/tree/main/examples) has runnable programs for each part.

## Modules

| Module | Holds |
|---|---|
| `github.com/alexjoedt/ark` | the core package `ark`, the backends, `arkfs`, `arkprofile`, `arkfind`, `arkglob`, `arktest` and `chunker` |
| `github.com/alexjoedt/ark/arkdocker` | Docker volumes, in its own module so the Docker SDK stays out of the core |
| `github.com/alexjoedt/ark/cmd/ark` | the `ark` binary, see [CLI](cli/index.md) |

```bash
go get github.com/alexjoedt/ark
go get github.com/alexjoedt/ark/arkdocker   # only for Docker volumes
```

## Back up and restore a directory

A repository is created once with `ark.Init` and opened with `ark.Open`. `Init` on an existing repository fails with `ark.ErrAlreadyInitialized`, and `Open` never creates one, so a mistyped path fails with `ark.ErrNotInitialized` instead of starting a second repository. A program that runs every day can call `Init` each time and let the first run create the repository:

```go
b, err := arklocal.New("/srv/ark")
if err != nil {
	return err
}
pw := ark.WithPassword(os.Getenv("ARK_PASSWORD"))
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

`arkfs.Backup` and `arkfs.Restore` are `repo.Backup(ctx, source)` and `repo.Restore(ctx, id, sink)` with a directory as source and sink. Restore takes a full snapshot ID or a unique prefix. Restore and export stream the snapshot: memory stays at the chunk cache plus one chunk per worker, no matter how large the snapshot is. The cache is 256 MiB; `ark.WithRestoreCache` changes it for a restore, export always uses the default. Verify reads pack by pack instead and holds about 150 bytes per distinct chunk of the snapshot plus one pack range of up to about 20 MiB, so its memory grows with the snapshot.

Besides backup and restore, a `*ark.Repository` lists snapshots (`Snapshots`, `Walk`), forgets and prunes (`Forget`, `Prune`), checks (`Verify`, `Check`), exports and imports, changes the password and lists locks.

## Packages

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
| [`arkfs`](https://pkg.go.dev/github.com/alexjoedt/ark/arkfs) | A local directory, with hardlinks, xattrs and excludes |
| [`arkdocker`](https://pkg.go.dev/github.com/alexjoedt/ark/arkdocker) | A Docker volume, and an `Engine` that stops and restarts the containers using it |
| **Tools on top** | |
| [`arkprofile`](https://pkg.go.dev/github.com/alexjoedt/ark/arkprofile) | Profiles: a stored repository, directory and forget policy, in code or YAML |
| [`arkfind`](https://pkg.go.dev/github.com/alexjoedt/ark/arkfind) | Searches snapshots for paths that match a pattern |
| [`arkglob`](https://pkg.go.dev/github.com/alexjoedt/ark/arkglob) | Matches snapshot paths against exclude patterns with restic's rules |
| **Testing** | |
| [`arktest`](https://pkg.go.dev/github.com/alexjoedt/ark/arktest) | Conformance suites for your own backends, sources and sinks |

## Backends

A backend stores opaque, encrypted objects. Pick one and hand it to `ark.Init` and `ark.Open`:

```go
b, err := arklocal.New("/srv/ark")

b, err := arksftp.New(arksftp.Config{Host: "nas", User: "backup", Path: "/srv/ark"})

b, err := arkuri.Open(ctx, "sftp://backup@nas/srv/ark") // or "/srv/ark", "local:///srv/ark"

b := arkmem.New() // its objects are gone on Close
```

`arksftp` reads `~/.ssh/config`, so a `Host` alias works. It authenticates with ssh-agent, then `IdentityFile` or the default keys in `~/.ssh`, then the password in `ARK_SFTP_PASSWORD`. An encrypted key file is unlocked with `ARK_SFTP_KEY_PASSPHRASE`. Secrets never go into `Config` or a URI. `arkuri` takes the non-secret options as query parameters: `known_hosts`, `identity` and `insecure`.

Anything else, such as S3 or a NAS API, is an `ark.Backend`: nine methods (`Put`, `Get`, `GetRange`, `Exists`, `Delete`, `Sync`, `List`, `Stat`, `Close`). The suite in `arktest` checks the same contract the built-in backends pass:

```go
func TestConformance(t *testing.T) {
	arktest.RunBackendTests(t, func(t *testing.T) ark.Backend {
		return mybackend.New()
	})
}
```

## Sources and sinks

A backup reads an `ark.Source` and a restore writes into an `ark.Sink`. Three come with ark.

**A directory.** `arkfs.NewSource` takes options that `arkfs.Backup` leaves at their defaults: a source name other than the path, excludes with restic's rules, and skipping files that exist only in the cloud.

```go
src, err := arkfs.NewSource("/home/me",
	arkfs.WithName("home"),
	arkfs.WithExclude("*.tmp", ".cache"),
)
if err != nil {
	return err
}
snap, err := repo.Backup(ctx, src)
```

**A Docker volume.** `arkdocker` stops the containers using the volume, copies it and starts them again. The daemon comes from `DOCKER_HOST`, or `arkdocker.WithDockerHost`.

```go
eng, err := arkdocker.New(repo)
if err != nil {
	return err
}
defer eng.Close()
snap, err := eng.BackupVolume(ctx, "postgres_data", ark.WithHost("db1"))
if err != nil {
	return err
}
_, err = eng.RestoreVolume(ctx, snap.ID, "postgres_copy")
```

`RestoreVolume` creates a missing volume. An existing one has its contents replaced, and the running containers that use it are stopped for the restore and started again after, so restore into a new volume name unless replacing the data is the point.

**A byte stream.** A database dump or any other single stream needs no file on disk: `ark.NewStreamSource(name, r)` backs up one reader as one file and `ark.NewStreamSink(w)` restores it to a writer.

```go
snap, err := repo.Backup(ctx, ark.NewStreamSource("dump", stdout))
if err != nil {
	return err
}
_, err = repo.Restore(ctx, snap.ID, ark.NewStreamSink(os.Stdout))
```

**Your own.** An `ark.Source` has two methods: `Meta` names the snapshot, `Walk` yields entries. A matching `ark.Sink` (`Open`, `Receive`, `Close`) brings them back. The rules the core relies on, such as when an open func may run and the order a sink sees entries in, are in [Writing a Source or Sink](https://pkg.go.dev/github.com/alexjoedt/ark#hdr-Writing_a_Source_or_Sink). A backup reads a plain `Source` one file at a time, inside the walk callback. A source whose open funcs may run later and on other goroutines, such as one reading over the network, implements `ark.ConcurrentSource` and is read in parallel. `ark.RewalkSource` adds a cheap second walk that `ark.WithBackupScan` uses to count totals before the backup. `arktest.RunSourceSinkTests` checks that what the sink receives, the source yields again; `arktest.RunStreamTests` does the same for one-file streams.

## Host, series and labels

The core library records no host unless you pass `ark.WithHost`; it never reads the machine's hostname. The `ark` CLI always records one, and so does `arkprofile`: without `backup.host`, a profile's `BackupOptions` and `Filter` use the machine's hostname. In a container whose hostname changes on every deploy, set `backup.host`, or each deploy starts a new series and `Forget` no longer sees the older snapshots. Source type, source name and host make up a snapshot's series: a backup takes the newest snapshot of its series as its parent, and `Forget` applies a keep policy once per series. Embedders that share one repository between machines should set a host. [Series](SERIES.md) has the details.

Labels are key-value pairs stored with a snapshot, set at backup with `ark.WithLabels` and matched with `SnapshotFilter.Labels`. Keys that start with `ark/` belong to ark's own sources, and a backup that sets one fails with `ark.ErrInvalidLabel`. [Labels](LABELS.md) has the key rules and the reserved keys.

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

## Encryption

Every repository is encrypted and authenticated. The password unlocks a random master key stored in `keys/`; chunks, trees, snapshot manifests and locks are sealed with XChaCha20-Poly1305 and named by an HMAC, so the backend sees neither contents nor file names. A changed object fails to open instead of restoring garbage.

- A lost password is lost data. There is no recovery and no backdoor.
- `ark.WithoutPassword()` creates or opens a repository without one. The format is the same, but the master key is stored unprotected: anyone with access to the backend can read it. `Init` records the choice, and `Open` holds to it: `WithoutPassword` on a protected repository fails with `ark.ErrPasswordRequired`, a password on an unprotected one with `ark.ErrNotPasswordProtected`.
- A wrong password fails with `ark.ErrWrongPassword` before any data is read.
- `repo.ChangePassword` seals the same master key under a new password. It does not revoke a key that already leaked.
- `Init`, `Open` and `ChangePassword` derive the key with argon2id and need 64 MiB of memory for it. Size containers accordingly.

The threat model, the accepted risks and how to report a vulnerability: [Security](SECURITY.md).

## Watching operations

An embedder that shows progress, waits on locks or measures the backend hooks in at `ark.Open`:

| Hook | Reports |
|---|---|
| `ark.WithProgress(fn)` | an `ark.ProgressEvent` per step of every operation: operation, phase, current path, files, bytes and chunks done. `ark.WithBackupProgress` and `ark.WithRestoreProgress` override it per call, `ark.WithBackupScan` adds totals to a backup |
| `ark.WithLockEvent(fn)` | an `ark.LockEvent` when a lock is acquired, rejected (with the conflicting holders), released or left over |
| `ark.WithLogger(l)` | the library's own log lines through a `*slog.Logger` |
| `ark.Instrument(b)` | wraps a backend and counts calls, errors, durations and bytes per method, read with `Stats()` |

```go
backend := ark.Instrument(local)
repo, err := ark.Open(ctx, backend, ark.WithoutPassword(),
	ark.WithLogger(logger),
	ark.WithProgress(func(e ark.ProgressEvent) {
		logger.Debug("progress", "op", e.Operation, "phase", e.Phase, "files", e.FilesDone, "bytes", e.BytesDone)
	}),
	ark.WithLockEvent(func(e ark.LockEvent) {
		logger.Info("lock", "kind", e.Kind, "exclusive", e.Exclusive, "holders", len(e.Holders))
	}),
)
if err != nil {
	return err
}
defer repo.Close()
// ... operations ...
logger.Info("backend", "puts", backend.Stats().Put.Calls)
```

Concurrent operations call the progress function concurrently, so it must not block. A lock event runs on the operation's goroutine and holds up the operation while it runs. [`examples/observe`](https://github.com/alexjoedt/ark/tree/main/examples/observe) logs every operation this way.

## Profiles

`arkprofile` is the profile format of the CLI as a package: one repository, one directory to back up and one forget policy, built in code or decoded from YAML. The keys are the CLI's flag names, listed in [Profiles](cli/profiles.md).

```go
p := &arkprofile.Profile{
	Name:        "docs",
	Repo:        "sftp://backup@nas/srv/ark",
	PasswordEnv: "ARK_PASSWORD",
	Backup:      arkprofile.Backup{Path: "/home/me/docs", Tags: []string{"daily"}},
	Forget:      arkprofile.Forget{KeepLast: 3},
}
if err := p.Validate(); err != nil {
	return err
}
pw, err := p.Passphrase()
if err != nil {
	return err
}
repo, err := p.Open(ctx, pw)
if err != nil {
	return err
}
defer repo.Close()

src, err := p.Source()
if err != nil {
	return err
}
if _, err := repo.Backup(ctx, src, p.BackupOptions()...); err != nil {
	return err
}
filter, err := p.Filter()
if err != nil {
	return err
}
if _, err := repo.Forget(ctx, p.ForgetPolicy(), filter); err != nil {
	return err
}
if p.Forget.Prune {
	_, err = repo.Prune(ctx)
}
```

The counted rules of an `ark.ForgetPolicy`, `KeepLast` to `KeepYearly`, are of type `ark.KeepCount`: a number, or `ark.Unlimited` to keep every snapshot (`KeepLast`) or the newest of every period. `Forget` refuses any other negative count with `ark.ErrInvalidPolicy`. `ark.ParseKeepCount` reads the text form a profile and the CLI use, a whole number or `unlimited`. In JSON a count is a number and `ark.Unlimited` the string `"unlimited"`.

`KeepWithin` is an `ark.Within`, a window of calendar `Years`, `Months` and `Days` plus `Hours`: `ark.Within{Days: 30}` keeps every snapshot of a series less than 30 days older than its newest one that is not dated in the future. Years, months and days are stepped back on the calendar of the forget zone, hours are elapsed time. `ark.ParseWithin` reads the text form a profile and the CLI use, `30d`, `72h` or `1y6m`, where `m` counts months; earlier versions took a `time.Duration`. `Forget` refuses a negative number, more than 11 months (18 months are `ark.Within{Years: 1, Months: 6}`) and a number above a million with `ark.ErrInvalidPolicy`, the bounds of the text form. In JSON a window is that text, `"30d"`.

`KeepWithinHourly`, `KeepWithinDaily`, `KeepWithinWeekly`, `KeepWithinMonthly` and `KeepWithinYearly` are windows too, each for a period rule without a count: `KeepWithinDaily: ark.Within{Days: 14}` keeps the newest snapshot of every day inside the window, which is measured as the one of `KeepWithin`. The oldest snapshot of a series is kept in addition when it lies inside the window. The reasons in `ForgetResult.KeepReasons` read `daily within 14d` and `oldest daily within 14d`.

`KeepTags` keeps by tag: it holds lists of tags, and a snapshot that carries all tags of one of the lists is kept; the empty tag stands for no tag, so `[][]string{{""}}` keeps the untagged snapshots. `ark.ParseTagList` reads the text form of one list, `db,prod`. Blanks around a tag do not count. A list with the empty tag next to others, as a stray comma leaves one, matches no snapshot, and `Forget` refuses it with `ark.ErrInvalidPolicy`. The rule has no count and can match nothing: when a policy keeps no snapshot of one of the series, `Forget` removes nothing at all and returns `ark.ErrNoSnapshotKept`. With this field a `ForgetPolicy` holds a slice, so two policies compare with `reflect.DeepEqual`, not with `==`; `policy.Empty()` tells whether a policy has a rule that keeps a snapshot.

`Forget` counts the hours, days, weeks, months and years of a policy in `time.Local`; `ark.WithForgetLocation` names another zone. Earlier versions counted in UTC. The result depends on the zone of the machine, so a program that has to keep the same snapshots wherever it runs passes a fixed zone, for example `ark.WithForgetLocation(time.UTC)`.

These calls apply neither the profile's hooks nor `forget.prune` on their own. Read `p.Forget.Prune` as above, and run each step inside `p.Backup.Hooks.Run` or `p.Forget.Hooks.Run` with an `arkprofile.HookEnv`, as the CLI does; a profile with `run-before` or `run-after` hooks otherwise behaves differently in your program than under `ark`.

`arkprofile.Load` reads a profile file. A program with a configuration of its own, such as a scheduler, embeds `arkprofile.Profile` inline in its struct and decodes the whole file with `arkprofile.Decode`, which expands `$VAR` and `~` and resolves relative paths against the directory it is given, as `Load` does with the file's directory. [`examples/profile`](https://github.com/alexjoedt/ark/tree/main/examples/profile) does both.

## Finding files

`arkfind.Find` searches the snapshots a filter selects, newest first, for entries whose path matches a pattern. Patterns follow the exclude rules: without a `/` a pattern matches a base name anywhere, a leading `/` anchors it at the snapshot root, and a matched directory yields everything below it.

```go
err := arkfind.Find(ctx, repo, ark.SnapshotFilter{Host: "laptop"}, []string{"*.pdf"},
	func(m arkfind.Match) error {
		fmt.Println(m.Snapshot.ID, m.File.Path)
		return nil
	})
```

No file content is read. The callback runs while the repository's shared lock is held, so it must not call `Prune`, `RebuildIndex`, `ChangePassword` or another operation that takes the exclusive lock.

## Examples

Each example is a `main` package in the [examples directory](https://github.com/alexjoedt/ark/tree/main/examples), run with `go run ./<name>` from there.

| Example | Shows |
|---|---|
| [`filebackup`](https://github.com/alexjoedt/ark/tree/main/examples/filebackup) | a directory into a local repository and back |
| [`dockervolume`](https://github.com/alexjoedt/ark/tree/main/examples/dockervolume) | a Docker volume into a snapshot and into a second volume |
| [`customsource`](https://github.com/alexjoedt/ark/tree/main/examples/customsource) | an `ark.Source` over in-memory files |
| [`streambackup`](https://github.com/alexjoedt/ark/tree/main/examples/streambackup) | a command's stdout through `ark.NewStreamSource` into a snapshot and back to stdout via `ark.NewStreamSink` |
| [`observe`](https://github.com/alexjoedt/ark/tree/main/examples/observe) | every operation with `ark.WithProgress`, `ark.WithLockEvent` and `ark.Instrument` logged through slog |
| [`profile`](https://github.com/alexjoedt/ark/tree/main/examples/profile) | a backup and a forget from a profile built in code, and a scheduler's configuration that embeds a profile |
