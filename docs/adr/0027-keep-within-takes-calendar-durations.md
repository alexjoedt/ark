# 0027: Keep-within takes restic's calendar durations

Status: accepted
Date: 2026-11-17

## Context

`--keep-within` and `ForgetPolicy.KeepWithin` took a Go duration, plus a day count such as `30d`
that stood for so many times 24 hours. restic, whose keep rules ark follows and against whose
decisions ark's selection is checked, reads the same flag in calendar units: years, months, days
and hours, where `1m` is a month. A `time.Duration` cannot hold "one month", so the two could not
select the same snapshots, and the same text meant different things in the two tools: `1m` was a
minute here. restic also measures the window from the newest snapshot that is not in the future,
where ark took the newest one whatever its date.

## Decision

`ForgetPolicy.KeepWithin` is of type `Within`, four whole numbers: `Years`, `Months`, `Days`,
`Hours`. The window ends at the newest snapshot of the series that is not after the time of the
run, and starts where `AddDate` leads from there for the years, months and days, on a clock in the
zone of the forget run (`WithForgetLocation`, `time.Local` by default), less the hours as elapsed
time. A snapshot is kept when it is after that start, so one exactly the window old is not. When
every snapshot of a series is in the future, all of them are kept. This is restic's rule, with one
difference: a snapshot taken at the very instant of the run counts as not in the future.

The text form, read by `ParseWithin` for `--keep-within` and the profile key `keep-within`, is
whole numbers followed by `y`, `m`, `d` or `h`, such as `30d`, `72h` or `1y2m3d4h`. The reason in
`ForgetResult.KeepReasons` writes the window unit by unit, `within 72h`, as restic does; it writes
the numbers, not the text that was typed, so `030d` reads `within 30d`. `Within` marshals as this
text, so a profile key is parsed once, when the profile is read, and JSON holds `"30d"`.

- Not Go durations next to the calendar units, because `m` cannot be a month and a minute, and a
  policy that reads the same in both tools should keep the same snapshots.
- Not restic's free form, where the units come in any order and a repeated unit replaces the
  earlier one (`1d1d2d` is two days): each unit at most once, from years to hours. What ark takes
  restic reads the same way.
- Not any number of months: `m` goes up to 11. `90m` and `1h30m` are what someone used to Go
  durations writes, and 90 months of retention is not what they meant; 18 months are `1y6m`, the
  same window. `Repository.Forget` refuses `Within{Months: 18}` with `ErrInvalidPolicy` too:
  otherwise its reason would read `within 18m`, which the text form cannot read back.
- Not a normalised reason such as `within 3d` for 72 hours: across a summer time change three days
  and 72 hours are different windows.
- Not a window of zero or a negative number: the text form refuses both, `Repository.Forget`
  refuses a negative number with `ErrInvalidPolicy`. It refuses a number above a million the same
  way, because more hours than a `time.Duration` holds would wrap the start of the window into the
  future and the rule would keep nothing.

## Consequences

A policy written for the old syntax has to be rewritten: `90m`, `1h30m` and `1.5d` are usage
errors, and `1m` to `11m` are read without complaint and now mean months. No check can tell an old
`5m` from a new one, so the CLI documentation carries the warning. Code that set
`KeepWithin` to a `time.Duration` no longer compiles and names the units instead,
`ark.Within{Days: 30}`.

A window with years, months or days depends on the zone of the forget run, as the period rules do:
a day back across the start of summer time is 23 hours. A step that lands on a time the clocks
skip that night starts the window an hour before or after it, whichever `AddDate` picks; restic
has the same arithmetic. This supersedes the part of 0024 that says
`KeepWithin` compares instants and does not depend on the zone; that holds for its hours only.

`TestParseWithin` pins the text form, `TestKeepWithinIsACalendarWindowFromTheNewestSnapshotNotInTheFuture`
the window: the excluded boundary, a month back from 31 March, both readings of a day across the
start of summer time, and future snapshots. `TestKeepWithinKeepsRecentSnapshots` runs it through
`Repository.Forget` with a snapshot dated a year ahead, `TestForgetTakesUnlimitedAndRefusesOtherNegativeCounts`
has the refused numbers and `TestForgetAndPrune` the flag, its usage errors and the reason.
