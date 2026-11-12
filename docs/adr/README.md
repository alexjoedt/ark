# Architecture decision records

An ADR records one architecture, format, API or protocol decision of ark: what was open, what was
chosen, what it costs. The code shows the what; the ADR keeps the why, including the options that
were rejected.

An ADR is written for every decision that settles such a question, in the same commit that
implements it. Files are `NNNN-<slug>.md`, numbered in the order the decisions were taken. The
shape is the MADR short form, see [template.md](template.md): Context, Decision, Consequences, a
status and a date. Tests and files are cited as backticked names, never as links. A decision that
is replaced keeps its file; the new ADR is added and the old status becomes `superseded by NNNN`.

`adr_doc_test.go` in the root module keeps this index and the files in step.

| Number | Title | Status | Date |
|---|---|---|---|
| 0001 | [Kill harness as a Go test with re-exec](0001-kill-harness-as-go-test-with-reexec.md) | accepted | 2026-11-05 |
| 0002 | [Benchmark against restic](0002-benchmark-against-restic.md) | accepted | 2026-11-07 |
| 0003 | [Unchanged-file detection](0003-unchanged-file-detection.md) | accepted | 2026-11-07 |
| 0004 | [v0 line and release tagging](0004-v0-line-and-release-tagging.md) | accepted | 2026-11-07 |
| 0005 | [Sync barrier instead of per-object fsync](0005-sync-barrier-instead-of-per-object-fsync.md) | accepted | 2026-11-08 |
| 0006 | [Threat model boundaries](0006-threat-model-boundaries.md) | accepted | 2026-11-08 |
| 0007 | [Embedding hooks on Open](0007-embedding-hooks-on-open.md) | accepted | 2026-11-08 |
| 0008 | [Path matcher package](0008-path-matcher-package.md) | accepted | 2026-11-11 |
| 0009 | [Exclude patterns on the snapshot](0009-exclude-patterns-on-the-snapshot.md) | accepted | 2026-11-11 |
| 0010 | [Pack object layout and blob binding](0010-pack-object-layout-and-blob-binding.md) | accepted | 2026-11-11 |
| 0011 | [Pack index format and loading](0011-pack-index-format-and-loading.md) | accepted | 2026-11-11 |
| 0012 | [Crash and concurrent-writer model for packs](0012-crash-and-writer-model-for-packs.md) | accepted | 2026-11-11 |
| 0013 | [Prune repacks by a repository-wide unused budget](0013-prune-repacks-by-an-unused-budget.md) | accepted | 2026-11-11 |
