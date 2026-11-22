# Forget policies

`repo.Forget` removes snapshots, either by ID or by a retention policy. This page shows the policy
type and how its fields behave. [Retention policies](../retention.md) explains with timelines
what each rule keeps; the rules here are the same ones.

## Apply a policy

```go
policy := ark.ForgetPolicy{
	KeepDaily:  7,
	KeepWeekly: 4,
	KeepYearly: ark.Unlimited,
}
filter := ark.SnapshotFilter{Source: "/home/me/docs", Host: "laptop"}

res, err := repo.Forget(ctx, policy, filter)
if err != nil {
	return err
}
for _, snap := range res.Remove {
	fmt.Println("removed", snap.ID)
}
if _, err := repo.Prune(ctx); err != nil {
	return err
}
```

- The policy applies once per [series](../series.md) among the snapshots that the filter
  matches.
- `Forget` removes the snapshots. Their chunks stay until `Prune` runs.
- `ark.WithForgetDryRun()` computes the result and removes nothing.
- `ark.WithForgetConfirm(fn)` calls `fn` with the result under the exclusive lock, before anything
  is removed. An error from `fn` stops `Forget`, which removes nothing. Use it to ask a person, so
  what they confirm is what goes.

To remove snapshots by ID, leave the policy empty and name them in the filter:

```go
res, err := repo.Forget(ctx, ark.ForgetPolicy{}, ark.SnapshotFilter{IDs: []ark.SnapshotID{id}})
```

## The result

| Field of `ark.ForgetResult` | Holds |
|---|---|
| `Keep` | the snapshots that stay |
| `Remove` | the snapshots that were removed, or would be in a dry run |
| `KeepReasons` | per kept snapshot ID, every rule that selected it, such as `daily snapshot` or `within 30d` |
| `Future` | the kept snapshots that are dated in the future, see below |

## Rules

A snapshot that any rule keeps stays.

| Field | Type | Keeps |
|---|---|---|
| `KeepLast` | `ark.KeepCount` | the N newest snapshots |
| `KeepHourly`, `KeepDaily`, `KeepWeekly`, `KeepMonthly`, `KeepYearly` | `ark.KeepCount` | the newest snapshot of each of the N most recent periods that have one |
| `KeepWithin` | `ark.Within` | every snapshot inside the window |
| `KeepWithinHourly`, `KeepWithinDaily`, `KeepWithinWeekly`, `KeepWithinMonthly`, `KeepWithinYearly` | `ark.Within` | the newest snapshot of every period inside the window |
| `KeepTags` | `[][]string` | every snapshot that carries all tags of one of the lists |

### Counts

An `ark.KeepCount` is a number or `ark.Unlimited`.

- `ark.Unlimited` keeps every snapshot for `KeepLast`, and the newest snapshot of every period for
  the period rules.
- Any other negative count is refused with `ark.ErrInvalidPolicy`.
- `ark.ParseKeepCount` reads the text form that profiles and the CLI use: a whole number or
  `unlimited`.
- In JSON, a count is a number, and `ark.Unlimited` is the string `"unlimited"`.

### Windows

An `ark.Within` is a window of calendar `Years`, `Months`, and `Days`, plus `Hours`:

```go
policy := ark.ForgetPolicy{KeepWithin: ark.Within{Days: 30}}
```

This keeps every snapshot of a series that is less than 30 days older than the series' newest
snapshot. The window is measured from that snapshot, not from the current time. A snapshot dated
in the future doesn't count as the newest.

- Years, months, and days are stepped back on the calendar of the forget time zone. Hours are
  elapsed time.
- `Forget` refuses a negative number, more than 11 months, and a number above a million with
  `ark.ErrInvalidPolicy`. These are the bounds of the text form too. Write 18 months as
  `ark.Within{Years: 1, Months: 6}`.
- `ark.ParseWithin` reads the text form `30d`, `72h`, or `1y6m`, where `m` means months.
- In JSON, a window is that text: `"30d"`.

The `KeepWithin...` period rules use the same windows. `KeepWithinDaily: ark.Within{Days: 14}`
keeps the newest snapshot of every day inside the window. When the oldest snapshot of a series
lies inside the window, the rule keeps it in addition. The reasons read `daily within 14d` and
`oldest daily within 14d`.

### Tags

`KeepTags` holds lists of tags. A snapshot is kept when it carries all tags of at least one list.

```go
policy := ark.ForgetPolicy{KeepTags: [][]string{{"pinned"}, {"db", "prod"}}}
```

- The empty tag stands for "no tag": `[][]string{{""}}` keeps the untagged snapshots.
- `ark.ParseTagList` reads the text form of one list, `db,prod`. Blanks around a tag are dropped.
- A list that holds the empty tag next to other tags, as a stray comma produces, matches nothing.
  `Forget` refuses it with `ark.ErrInvalidPolicy`.
- The rule has no count and takes nothing from the counts of other rules.

Because `KeepTags` is a slice, two policies compare with `reflect.DeepEqual`, not with `==`.
`policy.Empty()` reports whether a policy has no rule that keeps a snapshot.

## Errors

| Error | When |
|---|---|
| `ark.ErrEmptyPolicy` | the policy has no rule and the filter names no IDs |
| `ark.ErrInvalidPolicy` | a count, window, or tag list is out of bounds; returned before anything is read |
| `ark.ErrNoSnapshotKept` | the policy keeps no snapshot of one of the series |

`ark.ErrNoSnapshotKept` can only come from a policy of `KeepTags` alone, because every other rule
keeps at least the newest snapshot of a series. `Forget` then removes nothing at all, of no
series, in a dry run too.

## Time zone

`Forget` counts hours, days, weeks, months, and years in `time.Local`. The result therefore
depends on the zone of the machine.

A program that has to keep the same snapshots wherever it runs passes a fixed zone:

```go
res, err := repo.Forget(ctx, policy, filter, ark.WithForgetLocation(time.UTC))
```

## Snapshots dated in the future

A snapshot dated after the time of the run, as a machine with a wrong clock produces, is kept
whatever the policy says. A snapshot dated exactly at that time isn't one of these.

No rule sees such a snapshot:

- It uses no count, takes no period, and moves no window.
- `KeepTags` doesn't match it.
- The other snapshots are decided as if it weren't there.
- Its one reason in `KeepReasons` is `in the future`.
- It doesn't count as the snapshot that keeps its series. A policy that keeps nothing else of the
  series still returns `ark.ErrNoSnapshotKept`.

`ForgetResult.Future` lists these snapshots, newest first, so a program can warn about them as
`ark forget` does. Each of them is in `Keep` too. `Future` is nil when there is none, and when
`Forget` removes named IDs without a policy, which removes such a snapshot like any other.

## Next steps

- [Retention policies](../retention.md) shows every rule on a timeline.
- [Profiles](profiles.md) builds a policy from a profile.
