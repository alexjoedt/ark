# 0028: Forget keeps future-dated snapshots out of its rules

Status: accepted
Date: 2026-11-18

## Context

A machine with a wrong clock leaves a snapshot dated after the present, and `Repository.Import`
stores whatever date an archive carries. The keep rules walk a series from its newest snapshot to
its oldest, so such a snapshot is the first every rule sees: it takes the first count of `KeepLast`
and of each period rule, and it is the newest of its hour, day, week, month and year. A real
snapshot is removed in its place, the one `KeepLast: 1` was meant to keep or the newest real one
of the current day. restic selects the same way.

It also made forget unstable. A keep-within window is measured from the newest snapshot that is
not in the future (0027). A period rule passed that snapshot over when a future-dated one was the
newest of the same period, so a second run with the same policy and the same clock measured from
an older snapshot and removed one the first run had kept. And once the clock passes the date, the
snapshot is an ordinary one, so the policy means something else from one run to the next.

Never losing data ranks above selecting as restic does.

## Decision

Per series, `Repository.Forget` sets the snapshots dated after the time of the run aside before a
rule runs, and keeps them. A snapshot taken at the very instant of the run is not one of them, as
in 0027. Every rule, the counted ones, the keep-within ones and `KeepTags`, sees the other
snapshots only: a future-dated one uses no count, takes no period, moves no window and is not the
oldest snapshot of its series.

Such a snapshot has the single reason `in the future` in `ForgetResult.KeepReasons`, also when
its tags match a `KeepTags` list. `ForgetResult.Future` lists these snapshots, newest first; each
is in `Keep` too. The field is typed so a caller can warn about them without reading reasons.
Forget removes one only by ID, with a policy that has no rule, as it removes any snapshot.

A future-dated snapshot does not count as a kept snapshot of its series. A policy whose rules keep
nothing of a series, as `KeepTags` alone can, still fails with `ErrNoSnapshotKept` and removes
nothing, and the message names the future-dated snapshots of that series by the first eight
characters of their IDs. A series that holds future-dated snapshots only has nothing a rule could
keep, so it passes and keeps them all.

The time of the run is the repository's clock, the one its locks use, read once per run.

- Not restic's selection, because it removes a real snapshot for one whose date is wrong, and the
  owner of the wrong clock is often not the one who runs forget.
- Not refusing to forget while a future-dated snapshot exists, because one stray import would stop
  retention for the whole repository until someone removes it by hand.
- Not a tolerance, such as counting a snapshot up to an hour ahead as present: the rule would get
  a number to tune and the unstable second run would stay for everything beyond it.
- Not removing them, because the date is wrong and the data may be the only copy.
- Not the tag reason next to `in the future`: the reasons name the rules that selected a snapshot,
  and no rule saw this one.
- Not letting a kept future-dated snapshot satisfy the series check: a `KeepTags` policy that
  matches no real snapshot would then remove every real snapshot of the series and leave only the
  one with the wrong date.

## Consequences

ark and restic keep different snapshots of a series that holds a future-dated one; they agree on
every other history. The restic fixtures hold none, so `TestForgetMatchesRestic` is unchanged.
This supersedes the part of 0027 that has a keep-within rule keep a future-dated snapshot, a
series of them alone included: no rule keeps one now, and its reason is `in the future` instead of
`within 7d`. The rest of 0027 stands: the window ends at the newest snapshot that is not after the
run.

A second forget with the same policy and clock removes nothing. `TestForgetProperties` checks
that on random histories without an exception, and that a future-dated snapshot changes nothing
for the others: each is decided as in a history without it. `TestForgetTwiceUnderAFutureSnapshot`
pins the case that used to lose a snapshot on the second run.

A future-dated snapshot stays until its date has passed, when the rules see it like any other, or
until it is removed by ID. Callers that print a forget result should say so:
`ForgetResult.Future` is there for that. `TestForgetKeepsFutureSnapshotsOutOfTheRules` has the
rules one by one, `TestForgetSeriesCheckPassesOverFutureSnapshots` the series check and its
message, and `TestForgetSetsFutureSnapshotsAsideByTheRepositoryClock` runs both through
`Repository.Forget`.
