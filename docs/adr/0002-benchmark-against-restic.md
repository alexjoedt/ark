# 0002: Benchmark against restic

Status: accepted
Date: 2026-11-07

## Context

ark's speed goal was "initial, incremental and restore each within 1.2x of restic on a 10 GiB
mixed dataset", and speed is measured, never guessed. The goal did not say what "mixed" means,
what changes before the incremental backup, or how a run is timed. The existing `task bench`
covers the chunker and codec against the in-memory backend and cannot answer it. Every later
performance change would be judged by these choices, so they had to be fixed first.

## Decision

`scripts/bench` is one Go program in the root module (stdlib and `os/exec`, own `main`), with the
subcommands `gen`, `mutate`, `hash` and `run`; `task bench:restic DIR=...` runs it, CI never does.

- Dataset, generated from a fixed seed: about 200k small files of 1 to 64 KiB (about 6 GiB),
  about 120 medium files of 1 to 50 MiB (about 3 GiB), 2 large files of 512 MiB; in each band half
  the bytes are compressible text and half random; tree depth 3 to 4, about 20 entries per
  directory. The dataset is regenerated before every run.
- Incremental change set, from the same seed: rewrite 5% of the files, append to 1% of the large
  ones, add 1000 files, delete 1000.
- Tools: the `ark` binary on arklocal against the `restic` CLI on its local backend, both with
  defaults and passwords from the environment, the restic version recorded. restic is a benchmark
  tool only, never a Go dependency.
- Timing: wall time, ark and restic runs interleaved with the order alternating per round, a
  fresh repository each, median of 3, or of 5 when the spread exceeds 10%. No warm-up, no
  dropped caches, no root.
- Restore: the post-mutation snapshot into an empty directory on the same disk, compared to the
  source by tree hash.
- SFTP: when `ARK_SFTP_TEST_URI` is set, an extra row of ark against restic over SFTP, for
  information only, outside the budget.
- The runner refuses to start with less than 45 GiB free.
- Verdict: the runner prints each phase's ratio ark/restic and passes it at 1.2x or below.
- Not a Go generator plus a shell runner, because one program keeps seed, timing and verdict in
  one language with its own tests.
- Not a discarded warm-up read before the runs, because interleaving already puts both tools under
  the same cache conditions.

## Consequences

The verdict is reproducible on one machine and comparable over time: same seed, same change set,
same median. The first baseline (12 CPUs, one ext4 disk, restic 0.18.1) was initial 8.54x,
incremental 21.26x and restore 2.73x. After performance improvements, median of 5: initial 89.2 s
against 108.2 s (0.82x), incremental 12.3 s against 22.3 s (0.55x), restore 84.2 s against 82.3 s
(1.02x, with a 16% spread; the slowest restore was 1.10x). A full run takes about 25 minutes and
needs a dedicated disk, which is why it stays out of CI. The dataset is about one chunk per small
file, so it says nothing about larger chunk sizes. `TestMedianSpreadVerdict` pins the median and
spread rules, `TestGenerateDeterministic` and `TestMutateDeterministic` the seeded dataset.
