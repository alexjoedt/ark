# 0024: Forget periods count in the local time of the forget run

Status: partly superseded by 0027
Date: 2026-11-17

## Context

`--keep-hourly`, `--keep-daily`, `--keep-weekly`, `--keep-monthly` and `--keep-yearly` keep the
newest snapshot of each of the N newest periods, so every snapshot has to be put into an hour, a
day, a week, a month and a year. A backup stores its time in UTC, and the keep policy read the
period from the stored time as it was, so periods were UTC periods. On a machine in Berlin a
backup at 00:30 counted as the previous day: a nightly backup shortly after midnight and the one
of the evening before shared a day, and `--keep-daily` kept one of them. Week, month and year
boundaries were off by the same hour or two.

restic, whose keep rules ark follows, stores the local time of the backup run with its zone offset
and cuts periods on that stored clock, so each snapshot is counted in the zone it was taken in.

## Decision

Periods are cut on one clock for all snapshots of a run: the time zone of the forget run.
`Repository.Forget` converts every snapshot time to that zone before it reads the period
(`applyForgetPolicyToGroup` in `forget_impl.go`). The zone is `time.Local` unless the caller passes the
`ForgetOption` `WithForgetLocation`. `ark forget` passes nothing, so the machine's zone, or `TZ`,
decides. Snapshots keep their time in UTC; nothing in the repository format changes.

A period is what a wall clock in that zone shows. The day summer time ends has 25 hours and is
one day, the day it starts has 23 and is one day, and the hour the clock shows twice is one hour
for `KeepHourly`, as it is for restic. `KeepLast` and `KeepWithin` compare instants and do not
depend on the zone.

- Not UTC, because the days a person names when choosing `--keep-daily 7` are local days, and a
  backup just after local midnight belongs to the new one.
- Not the zone of each backup run, restic's rule, because ark stores no zone with a snapshot, and
  one clock for the whole run means two snapshots are in the same day exactly when a reader of
  `ark snapshots` on that machine sees the same date.
- Not a field of `ForgetPolicy`, because the policy is data a profile stores and the zone is where
  the run happens, like a dry run.
- Not a `--timezone` flag, because `TZ` already does that for one command.

## Consequences

A forget run and its dry run agree with the dates the same machine prints. The same policy run on
two machines in different zones can keep different snapshots of one repository; a job that must
not depend on the machine sets `TZ` or passes `WithForgetLocation`. ark and restic select the same
snapshots when backups and forget run under one zone, and can differ when a backup was taken under
another zone than the forget run.

This changes what an existing policy keeps, for the CLI and for every caller of
`Repository.Forget` that passes no location: the first run after the change can keep another
snapshot of a period than the UTC rule did and remove the one that rule kept. A dry run shows the
difference; `WithForgetLocation(time.UTC)` or `TZ=UTC` counts in UTC as before.

`TestPeriodsAreCutInTheGivenLocation` pins the boundaries and both summer time shifts in
Europe/Berlin against UTC, `TestPeriodsIgnoreTheZoneASnapshotCarries` that a snapshot's own zone
does not count, `TestForgetCountsPeriodsInTheGivenLocation` the option and
`TestForgetCutsPeriodsInTheZoneOfTZ` the CLI under two values of `TZ`.
