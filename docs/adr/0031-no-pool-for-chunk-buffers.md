# 0031: No pool for chunk buffers

Status: accepted
Date: 2026-11-21

## Context

The chunker allocates a fresh buffer for every chunk (`emit` in `chunker/chunker.go`) and hands
it to the consumer, which compresses, seals and packs it. A heap profile of an initial backup of
the bench dataset (`go run ./scripts/bench gen`, 200122 files, 10.19 GiB) on a local repository,
taken with `ARK_MEMPROFILE`, put this buffer at 30% of all allocated bytes, the largest single
site. The backup is CPU-bound there (about 65 s at about 7 cores), so allocation cost could show
in wall time.

A `sync.Pool` of chunk buffers was built and measured against the unpooled build: a discarded
warm-up, then 3 rounds in alternating order, `GODEBUG=gctrace=1` for GC CPU and `/usr/bin/time
-v` for peak RSS. The rule set before the measurement: the pool goes in when wall time or GC CPU
share improve by more than the run spread.

| | unpooled | one pool, 4 MiB buffers | one pool per power-of-two capacity |
|---|---|---|---|
| wall (median) | 64.4 to 65.1 s | 64.5 s | 63.0 s, inside the spread |
| GC CPU share | 1.34 to 1.44% | 0.77% | 0.96% |
| GC CPU | 6.1 s of about 423 s | 3.3 s | 4.0 s |
| collections | 95 | 48 | 65 |
| allocated | 36.1 GB | 25.7 GB | 26.1 GB |
| peak RSS | 880 to 889 MiB | 1288 MiB (+45%) | 938 MiB (+7%) |

Both pooled variants pass the rule on GC CPU share. Neither moves wall time. The 4 MiB variant
buys half of its GC gain with a larger live heap. On an in-memory benchmark of 42 MiB
(`BenchmarkBackup`) the pool shows no gain at all: a short backup ends before buffers return.

## Decision

Chunk buffers stay unpooled. Every chunk gets its own buffer, and the consumer owns `Data`.

- Not the power-of-two pool, although it met the rule: it saves about 2 s of CPU in 423 s (0.5%)
  and no wall time, and costs 7% peak memory, an exported `Chunker.SetAlloc` that the API freeze
  of `v1.1.0` would keep for the release line, and an ownership rule in the write path where a
  buffer released before its pack is sealed corrupts a backup without an error.
- Not the 4 MiB pool: +45% peak memory.
- Not a process-wide pool for many concurrent backups in one process: not built, not measured.

## Consequences

The write path keeps one owner per chunk buffer and no release points. GC stays at about 1.4% of
CPU time in a backup, which bounds what any allocation work on the backup path can gain. The
larger allocation sites are the sealed copy plus the zstd encoder in `seal` (`objectstore.go`,
47%) and the pack buffer in `packWriter.add` (`pack.go`, 20%); a CPU profile comes before work on
either. `ARK_MEMPROFILE` stays for the next measurement. A pool comes back with a measured wall
time gain, or when one process runs many backups and a process-wide pool shows a gain there.
