# Progress and logging

A program that shows progress, waits on locks, or measures its storage hooks into the repository
when it opens it. This page covers the four hooks: progress events, lock events, the logger, and
backend statistics.

```go
backend := ark.Instrument(local)
repo, err := ark.Open(ctx, backend, ark.NoPassword(),
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

[`examples/observe`](https://github.com/alexjoedt/ark/tree/main/examples/observe) logs every
operation this way.

## Progress events

`ark.WithProgress(fn)` reports an `ark.ProgressEvent` for every step of the operations in the
table below. `Info` with `ark.WithInfoExact` reports as a prune. Other operations, such as export
and import, report nothing.

| Field | Holds |
|---|---|
| `Operation` | `backup`, `restore`, `forget`, `prune`, `check`, or `verify` |
| `Phase` | where the operation is, see below |
| `CurrentPath` | the entry that the event reports: a file path for backup and restore, the snapshot ID for verify and check; empty when a phase works on no single entry |
| `FilesDone`, `BytesDone`, `ChunksDone` | what is finished |
| `FilesTotal`, `BytesTotal`, `ChunksTotal` | the totals, when they're known |

| Phase | Meaning |
|---|---|
| `scan` | walking the source |
| `store` | chunking and writing; for prune, copying into new packs |
| `load` | reading manifests and trees |
| `fetch` | reading chunks |
| `write` | writing into the sink |
| `sweep` | listing and deleting unreferenced objects |
| `finalize` | the closing event of a successful operation |

The function must not block. Concurrent operations call it concurrently.

### Per call

`ark.WithBackupProgress` and `ark.WithRestoreProgress` set a progress function for one call. It
replaces the repository's function for that call.

### Totals for a backup

A backup doesn't know how much it will read until it has walked the source. `ark.WithBackupScan`
adds a second walk that counts files and bytes while the backup already runs, so the events carry
`FilesTotal` and `BytesTotal`. The source has to support it, see
[Totals for progress](sources-and-sinks.md#totals-for-progress).

## Lock events

`ark.WithLockEvent(fn)` reports an `ark.LockEvent` whenever the repository's lock changes.

| `Kind` | Meaning |
|---|---|
| `acquired` | the operation holds its lock |
| `rejected` | an attempt found conflicting locks and backed off; `Holders` lists them; with `ark.WithLockWait`, every attempt reports one |
| `released` | the operation is done and is about to delete its lock |
| `left-over` | deleting the operation's own lock failed; a lock that stays behind blocks others until it's stale or `Unlock` removes it |

`Exclusive` tells which class of lock the operation asked for.

The function runs on the operation's goroutine. The operation waits while it runs, so keep it
short.

## Logs

`ark.WithLogger(l)` sends the library's own log lines to a `*slog.Logger`. Without it, the library
logs nothing.

The password never appears in a log line, at any level.

## Backend statistics

`ark.Instrument(b)` wraps a backend and counts what the repository does with it. `Stats()` returns
one `ark.MethodStats` per method: `Put`, `Get`, `GetRange`, `Exists`, `Delete`, `List`, `Stat`,
and `Sync`.

| Field | Holds |
|---|---|
| `Calls` | how often the method ran |
| `Errors` | how many calls returned an error, `ark.ErrNotFound` included |
| `Duration` | the time spent in the method, summed over all calls; concurrent calls can make it exceed wall time |
| `Bytes` | the bytes moved; set for `Put`, `Get`, and `GetRange` only |

The counters only grow. A `Get` counts its duration and bytes once its reader is closed.

Use it to see what an operation costs on a remote backend, or to assert in a test that an
operation makes no more requests than it should.

## Next steps

- [Backends](backends.md) shows the interface that `ark.Instrument` wraps.
- [Sources and sinks](sources-and-sinks.md) covers `ark.RewalkSource`, which backs the totals.
