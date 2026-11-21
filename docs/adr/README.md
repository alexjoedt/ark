# Architecture decision records

An ADR records one architecture, format, API or protocol decision of ark: what was open, what was
chosen, what it costs. The code shows the what; the ADR keeps the why, including the options that
were rejected.

An ADR is written for every decision that settles such a question, in the same commit that
implements it. Files are `NNNN-<slug>.md`, numbered in the order the decisions were taken. The
shape is the MADR short form, see [template.md](template.md): Context, Decision, Consequences, a
status and a date. Tests and files are cited as backticked names, never as links. A decision that
is replaced keeps its file; the new ADR is added and the old status becomes `superseded by NNNN`,
or `partly superseded by NNNN` when the new ADR replaces only a part of it and names that part.

`adr_doc_test.go` in the root module keeps this index and the files in step.

| Number | Title | Status | Date |
|---|---|---|---|
| 0001 | [Kill harness as a Go test with re-exec](0001-kill-harness-as-go-test-with-reexec.md) | accepted | 2026-11-05 |
| 0002 | [Benchmark against restic](0002-benchmark-against-restic.md) | accepted | 2026-11-07 |
| 0003 | [Unchanged-file detection](0003-unchanged-file-detection.md) | accepted | 2026-11-07 |
| 0004 | [v0 line and release tagging](0004-v0-line-and-release-tagging.md) | accepted | 2026-11-07 |
| 0005 | [Sync barrier instead of per-object fsync](0005-sync-barrier-instead-of-per-object-fsync.md) | partly superseded by 0012 | 2026-11-08 |
| 0006 | [Threat model boundaries](0006-threat-model-boundaries.md) | partly superseded by 0016 | 2026-11-08 |
| 0007 | [Embedding hooks on Open](0007-embedding-hooks-on-open.md) | accepted | 2026-11-08 |
| 0008 | [Path matcher package](0008-path-matcher-package.md) | accepted | 2026-11-11 |
| 0009 | [Exclude patterns on the snapshot](0009-exclude-patterns-on-the-snapshot.md) | accepted | 2026-11-11 |
| 0010 | [Pack object layout and blob binding](0010-pack-object-layout-and-blob-binding.md) | partly superseded by 0033 | 2026-11-11 |
| 0011 | [Pack index format and loading](0011-pack-index-format-and-loading.md) | partly superseded by 0014 | 2026-11-11 |
| 0012 | [Crash and concurrent-writer model for packs](0012-crash-and-writer-model-for-packs.md) | partly superseded by 0014 | 2026-11-11 |
| 0013 | [Prune repacks by a repository-wide unused budget](0013-prune-repacks-by-an-unused-budget.md) | accepted | 2026-11-11 |
| 0014 | [Index records of packs that are not stored are passed over](0014-index-records-of-packs-that-are-not-stored.md) | partly superseded by 0015 | 2026-11-12 |
| 0015 | [A marker makes unchanged backups see a lost data pack](0015-lost-pack-marker-for-unchanged-backups.md) | accepted | 2026-11-13 |
| 0016 | [Drop the web UI](0016-drop-the-web-ui.md) | accepted | 2026-11-14 |
| 0017 | [A counting scan beside the store](0017-counting-scan-beside-the-store.md) | accepted | 2026-11-14 |
| 0018 | [Profile files and the arkprofile package](0018-profile-files-and-the-arkprofile-package.md) | partly superseded by 0030 | 2026-11-15 |
| 0019 | [Find and the arkfind package](0019-find-and-the-arkfind-package.md) | accepted | 2026-11-15 |
| 0020 | [Read session with an io/fs view per snapshot](0020-read-session-with-an-io-fs-view.md) | accepted | 2026-11-15 |
| 0021 | [One series key: source type, source name and host](0021-one-series-key-type-source-host.md) | accepted | 2026-11-16 |
| 0022 | [Snapshot labels](0022-snapshot-labels.md) | accepted | 2026-11-16 |
| 0023 | [A VitePress docs site with Node only in docs/](0023-vitepress-docs-site.md) | accepted | 2026-11-16 |
| 0024 | [Forget periods count in the local time of the forget run](0024-forget-periods-in-the-local-time-of-the-forget-run.md) | partly superseded by 0027 | 2026-11-17 |
| 0025 | [A typed keep count with a named unlimited value](0025-typed-keep-count-with-unlimited.md) | accepted | 2026-11-17 |
| 0026 | [Forget keeps the oldest snapshot while a rule has count left](0026-forget-keeps-the-oldest-snapshot-while-a-rule-has-count-left.md) | accepted | 2026-11-17 |
| 0027 | [Keep-within takes restic's calendar durations](0027-keep-within-takes-calendar-durations.md) | partly superseded by 0028 | 2026-11-17 |
| 0028 | [Forget keeps future-dated snapshots out of its rules](0028-forget-keeps-future-snapshots-out-of-its-rules.md) | accepted | 2026-11-18 |
| 0029 | [Volume profiles without the Docker SDK](0029-volume-profiles-without-the-docker-sdk.md) | accepted | 2026-11-19 |
| 0030 | [ark profile backup runs the backup a profile names](0030-profile-backup-runs-the-backup-a-profile-names.md) | accepted | 2026-11-20 |
| 0031 | [No pool for chunk buffers](0031-no-pool-for-chunk-buffers.md) | accepted | 2026-11-21 |
| 0032 | [A raw codec and an authenticated format byte](0032-raw-codec-and-authenticated-format-byte.md) | accepted | 2026-11-21 |
| 0033 | [A pack table that opens from the tail of its pack](0033-pack-table-opens-from-a-tail-read.md) | accepted | 2026-11-21 |
