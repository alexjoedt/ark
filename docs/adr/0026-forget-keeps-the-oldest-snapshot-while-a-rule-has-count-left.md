# 0026: Forget keeps the oldest snapshot while a rule has count left

Status: accepted
Date: 2026-11-17

## Context

A period rule such as `--keep-daily 7` kept the newest snapshot of each of the 7 newest days and
nothing else. On a series younger than its policy that removes history the policy has room for: a
series with three snapshots over two days and `--keep-daily 7` lost the first snapshot ever taken,
and with `--keep-yearly 5` a first year kept only its last backup, so the state the series started
from was gone a year before the policy needed the room.

restic, whose keep rules ark follows and against whose decisions ark's selection is checked, keeps
the oldest snapshot of a group as long as a counted rule has count left.

## Decision

`Repository.Forget` selects as restic does. Each series is walked once from its newest snapshot to
its oldest (`applyForgetPolicyToGroup` in `forget_impl.go`), and every counted rule decides on each
snapshot by itself, holding the period of the snapshot it kept last and the count it has left. A
rule keeps a snapshot whose period differs from the one it kept last. When the walk reaches the
oldest snapshot of the series and the rule still has count left, it keeps that snapshot although
its period is taken, uses one count for it, and the reason in `ForgetResult.KeepReasons` reads
`oldest daily snapshot`, `oldest weekly snapshot` and so on. An oldest snapshot that starts a
period of its own is kept for the plain reason, as before.

A count of `Unlimited` always has count left, so such a rule always keeps the oldest snapshot.
`KeepLast` counts every snapshot as its own period and `KeepWithin` has no count; neither has the
rule.

- Not the old selection with a note about the difference, because the check against restic's
  decisions would then need an exception for every history shorter than its policy.
- Not a switch to turn the rule off, because restic has none and the snapshot costs one count the
  policy had left.

## Consequences

A policy keeps one snapshot more per series and period rule than before while the series is
shorter than the policy; a dry run shows it with its reason. Once a rule's count is used up before
the oldest snapshot, nothing changes. The snapshot kept as the oldest moves on as the series ages:
it stays until the rule's periods are filled by newer snapshots.

A rule compares a period with the one it kept last, not with all it has seen, as restic does.
Snapshots are walked by time and periods are read on one clock, so the two agree wherever a clock
does not step back into a period it had already left.

`TestForgetKeepsTheOldestWhileARuleHasCountLeft` pins the rule, its reasons, a count used up and
that the oldest is that of each series. `TestPeriodsAreCutInTheGivenLocation`,
`TestForgetCountsPeriodsInTheGivenLocation` and `TestForgetCutsPeriodsInTheZoneOfTZ` used two
snapshots under a count of two and now tell the newest of a day from the oldest of the series by
the reason.
