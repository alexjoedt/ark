# 0007: Embedding hooks on Open

Status: accepted
Date: 2026-11-08

## Context

A service that embeds ark observes six operations (Backup, Restore, Forget, Prune, Check, Verify)
through the public API. Before the hooks, progress was a per-call option of Backup and Restore
only, an event named only its phase so a consumer running several operations could not tell them
apart, lock waits were visible through a package-level hook, and backend traffic could be counted
only by wrapping the backend by hand. Single-stream data such as a database dump had no source
short of writing a temporary file. Open were where hooks attach, how observers behave under
concurrency, how failures are reported, how metrics leave the library and how a stream becomes a
source.

## Decision

Hooks are options of Open, not of Init: Init returns no handle and reports nothing.
`WithProgress(fn)` sets one repository-wide progress observer; `WithBackupProgress` and
`WithRestoreProgress` override it for one call and nil restores the default. `ProgressEvent` gains
an `Operation` field, so one observer can serve every operation. Observer calls never overlap
within one operation but may overlap across concurrent operations on one Repository; the library
holds no repository-wide mutex and consumers lock or use the per-call option. A failure or a cancel
stays on the return path: there is no error event and no run identifier on the event. Check
re-tags the events of its inner verify as check.

`WithLockEvent(fn)` makes lock changes public as acquired, rejected, released and left-over events.
A rejected event carries the conflicting `Holders` with host, user and pid, so a prune waiting on a
shared lock can say who it waits for.

`ark.Instrument(backend)` wraps any Backend and counts calls, errors, duration and bytes per method
with plain atomic counters read through `Stats()`. A Get is timed until its reader is closed. There
is no reset and no metrics dependency; the embedder turns the numbers into whatever exporter it
runs.

`StreamSource` and `StreamSink` are the single-blob pair: an `io.Reader` becomes one file of a
snapshot and one file of a snapshot is written to an `io.Writer`, which the sink never closes.

Out of this decision: scheduling that keeps a prune from starving behind shared locks (lock events
only make the wait visible), an HTTP repository server, and any exporter for Prometheus,
OpenTelemetry or expvar in ark.

- Not hooks on Init, because it has no handle to observe.
- Not an error event, because the return value already carries the failure and a second channel
  would report it twice or disagree.
- Not a mutex around the observer, because it would serialise unrelated operations on a slow
  consumer.
- Not a metrics library or exporter in ark, because every embedder already has its own and a
  dependency would be paid by all of them.
- Not a package-level lock hook, because two repositories in one process would share it.

## Consequences

An embedder gets a complete picture through the library alone, and the CLI is the first consumer
(prune, check, verify and forget print progress and lock waits show). The observer contract
puts the burden of locking on consumers that run operations concurrently. Pinned by
`TestEmbeddingObservesEveryOperation` in `embedding_test.go`, which runs all six operations with an
observer, a lock hook and an instrumented backend, `TestInstrumentedBackendCountsWhatTheBackendSaw`
in `instrument_test.go` against a backend that counts independently, and `TestStreamConformance` in
`stream_test.go`. `examples/observe` and `examples/streambackup` are compiling consumers: the CI step
`go build github.com/alexjoedt/ark/...` reaches the `examples` module through `go.work`, and
`adr_doc_test.go` fails when `go.work` stops listing it.
