# Sources and sinks

A backup reads an `ark.Source`, and a restore writes into an `ark.Sink`. ark comes with three
pairs: a directory, a Docker volume, and a stream of bytes. This page shows each of them, and then
how to write your own source and sink for data that lives somewhere else.

```go
snap, err := repo.Backup(ctx, source)
_, err = repo.Restore(ctx, snap.ID, sink)
```

## A directory

`arkfs.Backup` and `arkfs.Restore` cover the plain case. To set options, create the source
yourself with `arkfs.NewSource`:

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

| Option | Effect |
|---|---|
| `arkfs.WithName` | a source name other than the path |
| `arkfs.WithExclude` | leave out paths that match a pattern |
| `arkfs.WithExcludeFile` | read exclude patterns from a file |
| `arkfs.WithExcludeCloudFiles` | skip files that exist only in the cloud |

The patterns follow restic's rules, with a few differences. They are the rules of the CLI's
`--exclude`, see [Excludes](../cli/commands.md#excludes). The package `arkglob` implements the
matching.

## A Docker volume

`arkdocker` lives in its own module. Its `Engine` stops the containers that use a volume, copies
the volume, and starts them again:

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

- The daemon comes from `DOCKER_HOST`, or from `arkdocker.WithDockerHost`.
- `RestoreVolume` creates a missing volume.
- `RestoreVolume` replaces the contents of an existing volume. The running containers that use it
  are stopped for the restore and started again afterward. Restore into a new volume name unless
  replacing the data is the point.

## A byte stream

A database dump, or any other single stream, needs no file on disk. `ark.NewStreamSource` backs
up one reader as one file, and `ark.NewStreamSink` restores it to a writer:

```go
snap, err := repo.Backup(ctx, ark.NewStreamSource("dump", stdout))
if err != nil {
	return err
}
_, err = repo.Restore(ctx, snap.ID, ark.NewStreamSink(os.Stdout))
```

## Backup options

`repo.Backup` and the shortcuts take the same options:

| Option | Effect |
|---|---|
| `ark.WithTags` | tags |
| `ark.WithComment` | free text |
| `ark.WithHost` | the host; empty by default |
| `ark.WithLabels` | labels |
| `ark.WithParent` | an explicit parent snapshot |
| `ark.WithTime` | the time of the snapshot instead of now |
| `ark.WithBackupScan` | count files and bytes in a second walk while the backup runs, so progress events carry totals |
| `ark.WithBackupProgress` | a progress function for this call |

## Write your own source

A source has two methods. `Meta` names the snapshot, and `Walk` yields the entries:

```go
type Source interface {
	Meta() SourceMeta
	Walk(ctx context.Context, fn WalkFunc) error
}
```

This source backs up a set of files held in memory, the shape a database export or an API
response would have:

```go
type memSource struct {
	name  string
	files map[string]string
}

func (s memSource) Meta() ark.SourceMeta {
	return ark.SourceMeta{Name: s.name, Type: "memory"}
}

func (s memSource) Walk(ctx context.Context, fn ark.WalkFunc) error {
	now := time.Now()
	// Directories are yielded explicitly, the core only nests what it sees.
	if err := fn(ark.FileInfo{Path: "data", Mode: 0o755, ModTime: now, Type: ark.FileTypeDirectory}, nil); err != nil {
		return err
	}
	for name, content := range s.files {
		if err := ctx.Err(); err != nil {
			return err
		}
		info := ark.FileInfo{
			Path:    "data/" + name,
			Size:    int64(len(content)),
			Mode:    0o644,
			ModTime: now,
			Type:    ark.FileTypeRegular,
		}
		open := func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(content)), nil }
		if err := fn(info, open); err != nil {
			return err
		}
	}
	return nil
}
```

The complete program is
[`examples/customsource`](https://github.com/alexjoedt/ark/tree/main/examples/customsource).

### Rules for a source

- **Order.** Entries may come in any order. The one rule: a hardlink target comes before the
  `ark.FileTypeHardlink` entries that name it.
- **The open function.** Pass `nil` for everything but a regular file. For a regular file, the
  function yields at most `Size` bytes and does no work until it's called.
- **How often open is called.** At most once, and never when the parent snapshot already holds the
  file unchanged. The core closes the reader.
- **Cancellation.** `Walk` stops with the context's error when the context is cancelled.
- **Modification time.** A zero `ModTime` means unknown. Such a file never counts as unchanged and
  is read on every backup. Set `ModTime`, and `ChangeTime` and `Inode` where the medium has them,
  if unchanged files should be skipped.
- **Special files.** Yield a fifo as `ark.FileTypeFifo` and a device node as
  `ark.FileTypeCharDevice` or `ark.FileTypeBlockDevice` with `DevMajor` and `DevMinor`, all with
  open `nil`. Yield anything else the format doesn't model, such as a socket, as
  `ark.FileTypeUnsupported`: the backup leaves it out and counts it in `SnapshotStats.Skipped`.
- **Source type.** `Meta` returns your own `ark.SourceType`, a string that is stored with the
  snapshot. Sinks use it to reject snapshots they can't restore.

### Parallel reads

By default, a backup reads one file at a time, inside the walk callback. That fits a source that
can only move forward, such as a tar stream.

A source whose open functions are independent of the walk implements `ark.ConcurrentSource`. For
the `memSource` above, that is one more method, which the example program doesn't have:

```go
func (s memSource) ConcurrentOpen() bool { return true }
```

The open functions may then run after the callback returned, on any goroutine, and several at
once. The backup reads such a source in parallel. Implement the interface only when every open is
independent of the walk's position.

### Totals for progress

`ark.RewalkSource` adds a second, cheap walk that reads only names and sizes. A backup with
`ark.WithBackupScan` uses it to count files and bytes while the backup already runs, so progress
events can report totals.

A wrapper that embeds a `Source` hides both optional interfaces and has to forward them.

## Write your own sink

A sink has three methods:

```go
type Sink interface {
	Open(ctx context.Context, meta SourceMeta) error
	Receive(ctx context.Context, entry FileInfo, r io.Reader) error
	Close() error
}
```

This sink restores the snapshot of a directory into a zip archive:

```go
type zipSink struct {
	zw *zip.Writer
}

// Open runs once, before any entry. A sink rejects here what it cannot
// restore.
func (s *zipSink) Open(_ context.Context, meta ark.SourceMeta) error {
	if meta.Type != ark.SourceTypeFile {
		return fmt.Errorf("%w: the zip sink restores file snapshots, not %q",
			ark.ErrIncompatibleSourceType, meta.Type)
	}
	return nil
}

// Receive runs once per entry, parents before children. r is nil for
// everything but a regular file, and a regular file is read to the end.
func (s *zipSink) Receive(_ context.Context, e ark.FileInfo, r io.Reader) error {
	h := &zip.FileHeader{Name: e.Path, Modified: e.ModTime}
	switch e.Type {
	case ark.FileTypeDirectory:
		h.Name = strings.TrimSuffix(e.Path, "/") + "/"
		h.SetMode(e.Mode | fs.ModeDir)
		_, err := s.zw.CreateHeader(h)
		return err
	case ark.FileTypeRegular:
		h.Method = zip.Deflate
		h.SetMode(e.Mode)
		w, err := s.zw.CreateHeader(h)
		if err != nil {
			return err
		}
		_, err = io.Copy(w, r)
		return err
	case ark.FileTypeSymlink, ark.FileTypeHardlink, ark.FileTypeFifo, ark.FileTypeCharDevice,
		ark.FileTypeBlockDevice, ark.FileTypeUnsupported:
		// Links, fifos and devices are left out of the archive.
	}
	return nil
}

// Close runs once after a successful Open, also after a failed Receive or a
// cancelled restore. It does not run when Open fails.
func (s *zipSink) Close() error {
	return s.zw.Close()
}
```

Use it like any other sink:

```go
_, err := repo.Restore(ctx, snap.ID, &zipSink{zw: zip.NewWriter(f)})
```

The complete program is
[`examples/customsink`](https://github.com/alexjoedt/ark/tree/main/examples/customsink).

### Rules for a sink

| Method | Called | Rule |
|---|---|---|
| `Open` | once, before any entry | reject a source type the sink can't restore with an error that wraps `ark.ErrIncompatibleSourceType` |
| `Receive` | once per entry | read a regular file's reader to the end; the reader is `nil` for every other type; return an error that wraps `ark.ErrEntrySkipped` for a fifo or device node the sink can't create |
| `Close` | once after a successful `Open` | also runs after a failed `Receive` or a cancelled restore; doesn't run when `Open` fails or the restore fails before `Open` |

Entries arrive in walk order: parents before children, and a hardlink after its target.

An entry skipped with `ark.ErrEntrySkipped` doesn't fail the restore: `RestoreResult.Skipped`
lists one error per such entry, and every other entry is restored. `arkfs` skips a device node this
way when the restore runs without the privilege to create one.

## Test your source and sink

`arktest.RunSourceSinkTests` checks a source and a sink as a pair: what the sink receives, the
source has to yield again. It needs a medium that both can open, a directory for `arkfs` or a
volume for `arkdocker`:

```go
func TestSourceSinkConformance(t *testing.T) {
	arktest.RunSourceSinkTests(t, ark.SourceTypeFile,
		func(t *testing.T, location string) ark.Source { ... },
		func(t *testing.T) (ark.Sink, string) { ... },
	)
}
```

`arktest.RunStreamTests` does the same for sources and sinks that carry a single stream.

The contract in full is in the package documentation under
[Writing a Source or Sink](https://pkg.go.dev/github.com/alexjoedt/ark#hdr-Writing_a_Source_or_Sink).

## Next steps

- [Backends](backends.md) covers the third extension point.
- [Progress and logging](observing.md) shows how to watch a backup.
