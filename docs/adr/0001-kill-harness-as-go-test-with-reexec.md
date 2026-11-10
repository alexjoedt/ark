# 0001: Kill harness as a Go test with re-exec

Status: accepted
Date: 2026-11-05

## Context

Crash safety had to be shown, not argued: 1000 runs of backup, forget and prune, each killed at a
point nobody chose by hand, after which the repository must check clean and every snapshot must
restore. Three questions were open. Where the harness lives: a Go test in the root module, or a
separate tool for long local runs. How it picks kill points: SIGKILL after a random delay, which
is realistic but not reproducible without a seed, or a deterministic crash at the N-th backend
operation through a wrapping Backend, which covers every Put and Delete boundary but is an
in-process exit, not a real kill. What the child runs: the `ark` binary, which needs `cmd/ark`
built, or a program that calls only the library. A killed run leaves its lock behind; same-host
locks whose process is dead are reclaimed, so no run may need `ark unlock`.

## Decision

The harness is `TestKillHarness` in `kill_test.go`. The child is the test binary itself, re-exec'd
with `-test.run` and an environment that names the operation; it calls the library, never the
CLI. The child writes one byte on fd 3 right before the operation starts, and the parent's kill
delay starts at that byte, so process start-up never eats the delay. The parent SIGKILLs the child
after a random delay drawn from a ChaCha8 source; the seed is printed on every run and
`ARK_KILL_SEED` replays it. The first run of each operation finishes and sets that operation's
delay ceiling, a finished run resets the ceiling to its duration and a killed one raises it by
half, so the delays keep reaching the end of an operation as the repository grows. The repository
is replaced every 25 runs. `ARK_KILL_RUNS` sets the count: 5 by default, 10 in CI, 1000 for the
full proof on a developer machine. `TestKillHarnessSFTP` runs the same harness against arksftp.

- Not a separate tool such as `cmd/arkcrash`, because a test runs in CI with the rest of the
  suite, needs no build step and cannot drift from the library it tests.
- Not a deterministic crash at the N-th backend operation, because an in-process exit skips what
  a real kill leaves behind: half-written files, temp files, a lock with a dead PID.
- Not the `ark` binary as child, because the library is the product and the CLI only uses its
  public API; a CLI dependency would make the harness test two things at once.

## Consequences

Every change to the write path runs against real kills in CI, on arklocal and, on the Linux leg,
on arksftp. A failure names its seed and keeps its repository. A seed replays the drawn values,
not the kills: whether a child dies before it finishes depends on timing, so a rerun follows the
failing run only until the first kill that lands differently. The kill points cluster where the
operations spend their time, so a short window such as the rename of a manifest is hit rarely;
the durability tests (`TestDurabilityBarriers` in `sync_test.go`) cover those boundaries directly.
The 1000-run proof is a manual run, CI only keeps a reduced count green.
