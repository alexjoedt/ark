package ark

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"
)

// afterAll is a now that leaves no fixture snapshot in the future.
var afterAll = time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC)

// forgetFixture is one snapshot per index, newest first, spread far enough
// apart that every policy period has a distinct group to select from.
//
//	0  2026-09-10 12:00  Thursday
//	1  2026-09-10 11:00  the hour before
//	2  2026-09-10 10:00
//	3  2026-09-09 12:00  the day before
//	4  2026-09-08 12:00
//	5  2026-09-01 12:00  the ISO week before, Monday Aug 31
//	6  2026-08-10 12:00  the month before
//	7  2026-07-10 12:00
//	8  2025-09-10 12:00  the year before
//	9  2024-09-10 12:00
func forgetFixture() []*Snapshot {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	times := []time.Time{
		now,
		now.Add(-time.Hour),
		now.Add(-2 * time.Hour),
		now.AddDate(0, 0, -1),
		now.AddDate(0, 0, -2),
		now.AddDate(0, 0, -9),
		now.AddDate(0, -1, 0),
		now.AddDate(0, -2, 0),
		now.AddDate(-1, 0, 0),
		now.AddDate(-2, 0, 0),
	}
	snaps := make([]*Snapshot, len(times))
	for i, ts := range times {
		snaps[i] = &Snapshot{ID: fmt.Sprintf("s%d", i), CreatedAt: ts}
	}
	return snaps
}

// TestForgetPolicyFieldsSelectTheirPeriod covers every field of ForgetPolicy
// on its own, which is the part of the scenario list that needs snapshots at
// chosen times rather than at whatever the clock said during a backup.
func TestForgetPolicyFieldsSelectTheirPeriod(t *testing.T) {
	tests := []struct {
		name   string
		policy ForgetPolicy
		keep   []string
	}{
		{"zero policy keeps nothing", ForgetPolicy{}, nil},
		{"KeepLast", ForgetPolicy{KeepLast: 3}, []string{"s0", "s1", "s2"}},
		{"KeepHourly", ForgetPolicy{KeepHourly: 2}, []string{"s0", "s1"}},
		{"KeepDaily", ForgetPolicy{KeepDaily: 2}, []string{"s0", "s3"}},
		{"KeepWeekly", ForgetPolicy{KeepWeekly: 2}, []string{"s0", "s5"}},
		{"KeepMonthly", ForgetPolicy{KeepMonthly: 2}, []string{"s0", "s6"}},
		{"KeepYearly", ForgetPolicy{KeepYearly: 2}, []string{"s0", "s8"}},
		{"KeepWithin", ForgetPolicy{KeepWithin: Within{Days: 1, Hours: 1}}, []string{"s0", "s1", "s2", "s3"}},
		// Every window ends on a snapshot, which is exactly the window old
		// and so outside it: s2, s4, s5, s7 and s9.
		{"KeepWithinHourly", ForgetPolicy{KeepWithinHourly: Within{Hours: 2}}, []string{"s0", "s1"}},
		{"KeepWithinDaily", ForgetPolicy{KeepWithinDaily: Within{Days: 2}}, []string{"s0", "s3"}},
		{"KeepWithinWeekly", ForgetPolicy{KeepWithinWeekly: Within{Days: 9}}, []string{"s0"}},
		{"KeepWithinWeekly a day longer", ForgetPolicy{KeepWithinWeekly: Within{Days: 10}}, []string{"s0", "s5"}},
		{"KeepWithinMonthly", ForgetPolicy{KeepWithinMonthly: Within{Months: 2}}, []string{"s0", "s6"}},
		{"KeepWithinYearly", ForgetPolicy{KeepWithinYearly: Within{Years: 2}}, []string{"s0", "s8"}},
		{
			"a window rule with a counted rule and KeepWithin",
			ForgetPolicy{KeepWithin: Within{Hours: 2}, KeepMonthly: 2, KeepWithinDaily: Within{Days: 2}},
			[]string{"s0", "s1", "s3", "s6"},
		},
		{
			"fields combine",
			ForgetPolicy{KeepLast: 1, KeepYearly: 3},
			[]string{"s0", "s8", "s9"},
		},
		{
			"KeepLast unlimited",
			ForgetPolicy{KeepLast: Unlimited},
			[]string{"s0", "s1", "s2", "s3", "s4", "s5", "s6", "s7", "s8", "s9"},
		},
		{"KeepHourly unlimited", ForgetPolicy{KeepHourly: Unlimited}, []string{
			"s0", "s1", "s2", "s3", "s4", "s5", "s6", "s7", "s8", "s9",
		}},
		{
			"KeepDaily unlimited",
			ForgetPolicy{KeepDaily: Unlimited},
			[]string{"s0", "s3", "s4", "s5", "s6", "s7", "s8", "s9"},
		},
		{
			"KeepWeekly unlimited",
			ForgetPolicy{KeepWeekly: Unlimited},
			[]string{"s0", "s5", "s6", "s7", "s8", "s9"},
		},
		{"KeepMonthly unlimited", ForgetPolicy{KeepMonthly: Unlimited}, []string{"s0", "s6", "s7", "s8", "s9"}},
		{"KeepYearly unlimited", ForgetPolicy{KeepYearly: Unlimited}, []string{"s0", "s8", "s9"}},
	}

	snapshots := forgetFixture()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := applyForgetPolicy(snapshots, tc.policy, time.UTC, afterAll)

			got := ids(result.Keep)
			if !slices.Equal(got, tc.keep) {
				t.Errorf("keeps %v, want %v", got, tc.keep)
			}
			if len(result.Keep)+len(result.Remove) != len(snapshots) {
				t.Errorf("keeps %d and removes %d of %d snapshots",
					len(result.Keep), len(result.Remove), len(snapshots))
			}
			for _, s := range result.Remove {
				if slices.Contains(tc.keep, s.ID) {
					t.Errorf("%s is in both Keep and Remove", s.ID)
				}
			}
		})
	}
}

// TestForgetReasonsNameEveryRule pins restic's wording for the reason of
// each rule, and that a snapshot kept by several rules lists them all in the
// order the rules apply.
func TestForgetReasonsNameEveryRule(t *testing.T) {
	tests := []struct {
		name   string
		policy ForgetPolicy
		want   map[string][]string
	}{
		{"KeepLast", ForgetPolicy{KeepLast: 2}, map[string][]string{
			"s0": {"last snapshot"}, "s1": {"last snapshot"},
		}},
		{"KeepHourly", ForgetPolicy{KeepHourly: 2}, map[string][]string{
			"s0": {"hourly snapshot"}, "s1": {"hourly snapshot"},
		}},
		{"KeepDaily", ForgetPolicy{KeepDaily: 2}, map[string][]string{
			"s0": {"daily snapshot"}, "s3": {"daily snapshot"},
		}},
		{"KeepWeekly", ForgetPolicy{KeepWeekly: 2}, map[string][]string{
			"s0": {"weekly snapshot"}, "s5": {"weekly snapshot"},
		}},
		{"KeepMonthly", ForgetPolicy{KeepMonthly: 2}, map[string][]string{
			"s0": {"monthly snapshot"}, "s6": {"monthly snapshot"},
		}},
		{"KeepYearly", ForgetPolicy{KeepYearly: 2}, map[string][]string{
			"s0": {"yearly snapshot"}, "s8": {"yearly snapshot"},
		}},
		{"KeepWithin", ForgetPolicy{KeepWithin: Within{Days: 1, Hours: 1}}, map[string][]string{
			"s0": {"within 1d1h"}, "s1": {"within 1d1h"}, "s2": {"within 1d1h"}, "s3": {"within 1d1h"},
		}},
		{"KeepWithinHourly", ForgetPolicy{KeepWithinHourly: Within{Hours: 2}}, map[string][]string{
			"s0": {"hourly within 2h"}, "s1": {"hourly within 2h"},
		}},
		{"KeepWithinDaily", ForgetPolicy{KeepWithinDaily: Within{Days: 2}}, map[string][]string{
			"s0": {"daily within 2d"}, "s3": {"daily within 2d"},
		}},
		{"KeepWithinWeekly", ForgetPolicy{KeepWithinWeekly: Within{Days: 10}}, map[string][]string{
			"s0": {"weekly within 10d"}, "s5": {"weekly within 10d"},
		}},
		{"KeepWithinMonthly", ForgetPolicy{KeepWithinMonthly: Within{Months: 2}}, map[string][]string{
			"s0": {"monthly within 2m"}, "s6": {"monthly within 2m"},
		}},
		{"KeepWithinYearly", ForgetPolicy{KeepWithinYearly: Within{Years: 2}}, map[string][]string{
			"s0": {"yearly within 2y"}, "s8": {"yearly within 2y"},
		}},
		{
			"window rules come last, each with its own window",
			ForgetPolicy{
				KeepWithin: Within{Hours: 2}, KeepLast: 1, KeepMonthly: 2,
				KeepWithinHourly: Within{Hours: 1}, KeepWithinDaily: Within{Days: 2},
				KeepWithinWeekly: Within{Days: 10}, KeepWithinMonthly: Within{Months: 1, Days: 1},
				KeepWithinYearly: Within{Years: 1, Days: 1},
			},
			map[string][]string{
				"s0": {
					"within 2h", "last snapshot", "monthly snapshot", "hourly within 1h", "daily within 2d",
					"weekly within 10d", "monthly within 1m1d", "yearly within 1y1d",
				},
				"s1": {"within 2h"},
				"s3": {"daily within 2d"},
				"s5": {"weekly within 10d"},
				"s6": {"monthly snapshot", "monthly within 1m1d"},
				"s8": {"yearly within 1y1d"},
			},
		},
		{
			"rules combine",
			ForgetPolicy{KeepWithin: Within{Hours: 2}, KeepLast: 1, KeepDaily: 1, KeepYearly: 2},
			map[string][]string{
				"s0": {"within 2h", "last snapshot", "daily snapshot", "yearly snapshot"},
				"s1": {"within 2h"},
				"s8": {"yearly snapshot"},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := applyForgetPolicy(forgetFixture(), tc.policy, time.UTC, afterAll)
			if !maps.EqualFunc(res.KeepReasons, tc.want, slices.Equal) {
				t.Errorf("reasons %v, want %v", res.KeepReasons, tc.want)
			}
			if len(res.KeepReasons) != len(res.Keep) {
				t.Errorf("%d reasons for %d kept snapshots", len(res.KeepReasons), len(res.Keep))
			}
		})
	}
}

// TestForgetKeepsTheOldestWhileARuleHasCountLeft pins restic's keep-oldest
// rule on a series shorter than its policy, three snapshots over two days:
//
//	0  2026-01-03 10:00  Saturday
//	1  2026-01-02 12:00
//	2  2026-01-02 08:00  the oldest, in the day, week, month and year of s1
//
// A counted period rule that reaches the oldest snapshot with count left
// keeps it for the reason "oldest ...", although its period is taken. A
// keep-within period rule does when the oldest snapshot is inside its window.
func TestForgetKeepsTheOldestWhileARuleHasCountLeft(t *testing.T) {
	series := func(source string) []*Snapshot {
		return []*Snapshot{
			{ID: source + "0", SourceName: source, CreatedAt: time.Date(2026, 1, 3, 10, 0, 0, 0, time.UTC)},
			{ID: source + "1", SourceName: source, CreatedAt: time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)},
			{ID: source + "2", SourceName: source, CreatedAt: time.Date(2026, 1, 2, 8, 0, 0, 0, time.UTC)},
		}
	}
	tests := []struct {
		name   string
		policy ForgetPolicy
		want   map[string][]string
	}{
		{"count left", ForgetPolicy{KeepDaily: 7}, map[string][]string{
			"s0": {"daily snapshot"}, "s1": {"daily snapshot"}, "s2": {"oldest daily snapshot"},
		}},
		{"the last count", ForgetPolicy{KeepDaily: 3}, map[string][]string{
			"s0": {"daily snapshot"}, "s1": {"daily snapshot"}, "s2": {"oldest daily snapshot"},
		}},
		{"count used up", ForgetPolicy{KeepDaily: 2}, map[string][]string{
			"s0": {"daily snapshot"}, "s1": {"daily snapshot"},
		}},
		{"unlimited", ForgetPolicy{KeepDaily: Unlimited}, map[string][]string{
			"s0": {"daily snapshot"}, "s1": {"daily snapshot"}, "s2": {"oldest daily snapshot"},
		}},
		{"with another rule", ForgetPolicy{KeepLast: 1, KeepDaily: 5}, map[string][]string{
			"s0": {"last snapshot", "daily snapshot"}, "s1": {"daily snapshot"}, "s2": {"oldest daily snapshot"},
		}},
		{
			"every rule for itself",
			ForgetPolicy{KeepWeekly: 2, KeepMonthly: 1, KeepYearly: Unlimited},
			map[string][]string{
				"s0": {"weekly snapshot", "monthly snapshot", "yearly snapshot"},
				"s2": {"oldest weekly snapshot", "oldest yearly snapshot"},
			},
		},
		{"an oldest that starts a period", ForgetPolicy{KeepHourly: 7}, map[string][]string{
			"s0": {"hourly snapshot"}, "s1": {"hourly snapshot"}, "s2": {"hourly snapshot"},
		}},
		{"KeepLast has no oldest", ForgetPolicy{KeepLast: 7}, map[string][]string{
			"s0": {"last snapshot"}, "s1": {"last snapshot"}, "s2": {"last snapshot"},
		}},
		{"KeepWithin has no oldest", ForgetPolicy{KeepWithin: Within{Hours: 1}}, map[string][]string{
			"s0": {"within 1h"},
		}},
		{"a window with the oldest inside", ForgetPolicy{KeepWithinDaily: Within{Days: 10}}, map[string][]string{
			"s0": {"daily within 10d"}, "s1": {"daily within 10d"}, "s2": {"oldest daily within 10d"},
		}},
		// 2026-01-02 10:00 on: s1 is inside, the oldest is not
		{"a window ending before the oldest", ForgetPolicy{KeepWithinDaily: Within{Days: 1}}, map[string][]string{
			"s0": {"daily within 1d"}, "s1": {"daily within 1d"},
		}},
		{"a window ending on the oldest", ForgetPolicy{KeepWithinDaily: Within{Days: 1, Hours: 2}}, map[string][]string{
			"s0": {"daily within 1d2h"}, "s1": {"daily within 1d2h"},
		}},
		{"a window whose oldest starts a period", ForgetPolicy{KeepWithinHourly: Within{Days: 10}}, map[string][]string{
			"s0": {"hourly within 10d"}, "s1": {"hourly within 10d"}, "s2": {"hourly within 10d"},
		}},
		{
			"every window rule for itself",
			ForgetPolicy{
				KeepWithinWeekly: Within{Days: 10}, KeepWithinMonthly: Within{Days: 1},
				KeepWithinYearly: Within{Years: 1},
			},
			map[string][]string{
				"s0": {"weekly within 10d", "monthly within 1d", "yearly within 1y"},
				"s2": {"oldest weekly within 10d", "oldest yearly within 1y"},
			},
		},
		// restic 0.18 gives these reasons for --keep-daily 5 --keep-within-daily 10d --keep-last 1
		{
			"counted and window rule side by side",
			ForgetPolicy{KeepDaily: 5, KeepWithinDaily: Within{Days: 10}, KeepLast: 1},
			map[string][]string{
				"s0": {"last snapshot", "daily snapshot", "daily within 10d"},
				"s1": {"daily snapshot", "daily within 10d"},
				"s2": {"oldest daily snapshot", "oldest daily within 10d"},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := applyForgetPolicy(series("s"), tc.policy, time.UTC, afterAll)
			if !maps.EqualFunc(res.KeepReasons, tc.want, slices.Equal) {
				t.Errorf("reasons %v, want %v", res.KeepReasons, tc.want)
			}
			if got := ids(res.Keep); !slices.Equal(got, slices.Sorted(maps.Keys(tc.want))) {
				t.Errorf("keeps %v, want the snapshots of %v", got, tc.want)
			}
		})
	}

	// The oldest is that of each series: a series of one snapshot older
	// than all of another does not take the rule from it.
	both := append(series("s"), &Snapshot{
		ID: "t0", SourceName: "t", CreatedAt: time.Date(2025, 12, 1, 8, 0, 0, 0, time.UTC),
	})
	res := applyForgetPolicy(both, ForgetPolicy{KeepDaily: 7}, time.UTC, afterAll)
	want := map[string][]string{
		"s0": {"daily snapshot"}, "s1": {"daily snapshot"}, "s2": {"oldest daily snapshot"}, "t0": {"daily snapshot"},
	}
	if !maps.EqualFunc(res.KeepReasons, want, slices.Equal) {
		t.Errorf("two series: reasons %v, want %v", res.KeepReasons, want)
	}
}

// The window of KeepWithin is restic's: calendar steps for years, months and
// days on the clock of the forget zone, elapsed time for hours, measured back
// from the newest snapshot that is not in the future, the boundary excluded.
// Snapshots are named s0, s1, ... in the order given, newest first.
func TestKeepWithinIsACalendarWindowFromTheNewestSnapshotNotInTheFuture(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("no zone data: %v", err)
	}
	// The day before summer time starts in Berlin: 12:00 the day after is
	// 23 hours later.
	dst := []string{
		"2026-03-29T12:00:00+02:00",
		"2026-03-28T12:00:00+01:00", // a calendar day back, 23 hours
		"2026-03-28T11:30:00+01:00",
		"2026-03-28T11:00:00+01:00", // 24 hours back
	}
	future := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		within Within
		loc    *time.Location
		now    time.Time
		times  []string
		keep   []string
	}{
		{
			"a snapshot exactly the window old is not kept",
			Within{Days: 1}, time.UTC, afterAll,
			[]string{"2026-03-10T12:00:00Z", "2026-03-09T12:00:01Z", "2026-03-09T12:00:00Z", "2026-01-01T00:00:00Z"},
			[]string{"s0", "s1"},
		},
		{
			"a month back from 31 March is 3 March",
			Within{Months: 1}, time.UTC, afterAll,
			[]string{"2026-03-31T12:00:00Z", "2026-03-03T12:00:01Z", "2026-03-03T12:00:00Z", "2026-03-01T12:00:00Z"},
			[]string{"s0", "s1"},
		},
		{
			"and 2 March in a leap year",
			Within{Months: 1}, time.UTC, afterAll,
			[]string{"2028-03-31T12:00:00Z", "2028-03-02T12:00:01Z", "2028-03-02T12:00:00Z"},
			[]string{"s0", "s1"},
		},
		{
			"a year back from 29 February is 1 March",
			Within{Years: 1}, time.UTC, afterAll,
			[]string{"2028-02-29T12:00:00Z", "2027-03-01T12:00:01Z", "2027-03-01T12:00:00Z"},
			[]string{"s0", "s1"},
		},
		{
			"the units add up",
			Within{Years: 1, Months: 2, Days: 3, Hours: 4}, time.UTC, afterAll,
			[]string{"2026-09-10T12:00:00Z", "2025-07-07T08:00:01Z", "2025-07-07T08:00:00Z"},
			[]string{"s0", "s1"},
		},
		{"a day is a calendar day in the forget zone", Within{Days: 1}, berlin, afterAll, dst, []string{"s0"}},
		{"which is 24 hours in UTC", Within{Days: 1}, time.UTC, afterAll, dst, []string{"s0", "s1", "s2"}},
		{"hours are elapsed time in any zone", Within{Hours: 24}, berlin, afterAll, dst, []string{"s0", "s1", "s2"}},
		{
			"a future snapshot does not move the window, and is kept",
			Within{Days: 7}, time.UTC, future,
			[]string{"2027-06-01T00:00:00Z", "2026-11-10T00:00:00Z", "2026-11-05T00:00:00Z", "2026-11-01T00:00:00Z"},
			[]string{"s0", "s1", "s2"},
		},
		{
			"a snapshot taken this instant is not in the future",
			Within{Days: 1}, time.UTC, future,
			// measured from s1 the window would reach s2
			[]string{"2026-11-16T00:00:00Z", "2026-11-15T12:00:00Z", "2026-11-14T18:00:00Z"},
			[]string{"s0", "s1"},
		},
		{
			"only future snapshots are all kept",
			Within{Days: 1}, time.UTC, future,
			[]string{"2027-06-01T00:00:00Z", "2027-01-01T00:00:00Z", "2026-12-01T00:00:00Z"},
			[]string{"s0", "s1", "s2"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			snaps := make([]*Snapshot, len(tc.times))
			for i, text := range tc.times {
				at, err := time.Parse(time.RFC3339, text)
				if err != nil {
					t.Fatal(err)
				}
				snaps[i] = &Snapshot{ID: fmt.Sprintf("s%d", i), CreatedAt: at}
			}
			res := applyForgetPolicy(snaps, ForgetPolicy{KeepWithin: tc.within}, tc.loc, tc.now)
			if got := ids(res.Keep); !slices.Equal(got, tc.keep) {
				t.Errorf("keeps %v, want %v", got, tc.keep)
			}
			if len(res.Keep)+len(res.Remove) != len(snaps) {
				t.Errorf("keeps %d and removes %d of %d", len(res.Keep), len(res.Remove), len(snaps))
			}
			for _, id := range tc.keep {
				if want := []string{"within " + tc.within.String()}; !slices.Equal(res.KeepReasons[id], want) {
					t.Errorf("%s kept for %q, want %q", id, res.KeepReasons[id], want)
				}
			}
		})
	}
}

// A keep-within period rule cuts its periods exactly as the counted rule of
// the same period does: with a window longer than the series it keeps what
// the unlimited count keeps, for the matching reasons, in any zone. The
// series has two snapshots in one hour, an hour the Berlin clock shows twice,
// days and a week that start at another instant in Berlin than in UTC, and
// the ISO week 53 of 2026, which ends in 2027.
func TestKeepWithinPeriodsAreThoseOfTheCountedRules(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	utc := func(y int, m time.Month, d, h, minute int) *Snapshot {
		at := time.Date(y, m, d, h, minute, 0, 0, time.UTC)
		return &Snapshot{ID: at.Format("2006-01-02T15:04"), CreatedAt: at}
	}
	snaps := []*Snapshot{
		utc(2027, 1, 6, 12, 0),
		utc(2027, 1, 3, 23, 30), // Monday 01-04 00:30 CET, week 1
		utc(2027, 1, 3, 12, 0),  // Sunday, week 53 of 2026
		utc(2026, 12, 31, 23, 30),
		utc(2026, 12, 30, 12, 0),
		utc(2026, 10, 25, 1, 30),  // 02:30 CET
		utc(2026, 10, 25, 0, 30),  // 02:30 CEST
		utc(2026, 10, 24, 23, 30), // 01:30 CEST
		utc(2026, 10, 24, 21, 59),
		utc(2026, 6, 30, 22, 30),
		utc(2026, 6, 30, 22, 10),
		utc(2025, 3, 1, 12, 0),
		utc(2025, 3, 1, 8, 0),
	}
	forever := Within{Years: 100}
	rules := []struct {
		name    string
		counted ForgetPolicy
		window  ForgetPolicy
	}{
		{"hourly", ForgetPolicy{KeepHourly: Unlimited}, ForgetPolicy{KeepWithinHourly: forever}},
		{"daily", ForgetPolicy{KeepDaily: Unlimited}, ForgetPolicy{KeepWithinDaily: forever}},
		{"weekly", ForgetPolicy{KeepWeekly: Unlimited}, ForgetPolicy{KeepWithinWeekly: forever}},
		{"monthly", ForgetPolicy{KeepMonthly: Unlimited}, ForgetPolicy{KeepWithinMonthly: forever}},
		{"yearly", ForgetPolicy{KeepYearly: Unlimited}, ForgetPolicy{KeepWithinYearly: forever}},
	}
	seen := make(map[string]string)
	for _, loc := range []*time.Location{berlin, time.UTC} {
		for _, rule := range rules {
			name := rule.name + " in " + loc.String()
			want := applyForgetPolicy(snaps, rule.counted, loc, afterAll).KeepReasons
			for id, reasons := range want {
				for i, reason := range reasons {
					reasons[i] = strings.Replace(reason, rule.name+" snapshot", rule.name+" within 100y", 1)
				}
				want[id] = reasons
			}
			got := applyForgetPolicy(snaps, rule.window, loc, afterAll).KeepReasons
			if !maps.EqualFunc(got, want, slices.Equal) {
				t.Errorf("%s: reasons %v, want those of the counted rule: %v", name, got, want)
			}
			// the fixture tells every period and both zones apart
			kept := strings.Join(slices.Sorted(maps.Keys(got)), " ")
			if other, dup := seen[kept]; dup {
				t.Errorf("%s keeps what %s keeps: %s", name, other, kept)
			}
			seen[kept] = name
		}
	}
}

func ids(snaps []*Snapshot) []string {
	if len(snaps) == 0 {
		return nil
	}
	out := make([]string, len(snaps))
	for i, s := range snaps {
		out[i] = s.ID
	}
	return out
}

// Each field of the series key splits the fixture into two groups on its own.
func TestForgetPolicyAppliesPerSeries(t *testing.T) {
	tests := []struct {
		name string
		odd  func(*Snapshot)
	}{
		{"source name", func(s *Snapshot) { s.SourceName = "b" }},
		{"source type", func(s *Snapshot) { s.SourceType = "docker-volume" }},
		{"host", func(s *Snapshot) { s.Host = "b" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			snaps := forgetFixture()
			for i, s := range snaps {
				s.SourceName, s.SourceType, s.Host = "a", SourceTypeFile, "a"
				if i%2 == 1 {
					tc.odd(s)
				}
			}
			res := applyForgetPolicy(snaps, ForgetPolicy{KeepLast: 1}, time.UTC, afterAll)

			if keep := ids(res.Keep); !slices.Equal(keep, []string{"s0", "s1"}) {
				t.Fatalf("keep = %v, want the newest of each series", keep)
			}
			if len(res.Remove) != 8 || res.Remove[0].ID != "s2" || res.Remove[7].ID != "s9" {
				t.Fatalf("remove = %d entries, first %s", len(res.Remove), res.Remove[0].ID)
			}
		})
	}
}

// A week belongs to the ISO year its Thursday falls in, so a year turn must
// neither merge the last week of a year with the first of the next nor split
// one week in two.
func TestKeepWeeklyAcrossYearTurns(t *testing.T) {
	day := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 12, 0, 0, 0, time.UTC) }
	tests := []struct {
		name  string
		times []time.Time // newest first
		keep  []string
	}{
		{
			"week 53 of 2026 and week 1 of 2027",
			[]time.Time{day(2027, 1, 6), day(2026, 12, 30)},
			[]string{"s0", "s1"},
		},
		{
			"week 53 of 2020 and week 1 of 2021",
			[]time.Time{day(2021, 1, 6), day(2020, 12, 30)},
			[]string{"s0", "s1"},
		},
		{
			"week 53 of 2020 spans the year turn",
			[]time.Time{day(2021, 1, 3), day(2020, 12, 28), day(2020, 12, 27)},
			[]string{"s0", "s2"},
		},
		{
			"a year starting on Monday",
			[]time.Time{day(2024, 1, 1), day(2023, 12, 31), day(2023, 12, 25)},
			[]string{"s0", "s1"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			snaps := make([]*Snapshot, len(tc.times))
			for i, ts := range tc.times {
				snaps[i] = &Snapshot{ID: fmt.Sprintf("s%d", i), CreatedAt: ts}
			}
			got := ids(applyForgetPolicy(snaps, ForgetPolicy{KeepWeekly: 2}, time.UTC, afterAll).Keep)
			if !slices.Equal(got, tc.keep) {
				t.Errorf("keeps %v, want %v", got, tc.keep)
			}
		})
	}
}

// Two times in one period share it even when each carries its own
// [time.Location], as timestamps decoded with a foreign offset do. The
// conversion to the one location of the run is what makes this hold; a
// [time.Time] key taken from the times as they come would compare the Location
// pointers. The offset is +05:30 because the time package shares one Location
// per unnamed whole-hour offset.
func TestPeriodKeysIgnoreLocationIdentity(t *testing.T) {
	zone := func() *time.Location { return time.FixedZone("", 5*60*60+30*60) }
	at := func(y int, m time.Month, d, h, minute int) time.Time {
		return time.Date(y, m, d, h, minute, 0, 0, zone())
	}
	snaps := []*Snapshot{
		{ID: "s0", CreatedAt: at(2020, 1, 2, 10, 30)},
		{ID: "s1", CreatedAt: at(2020, 1, 2, 10, 10)},
		{ID: "s2", CreatedAt: at(2018, 12, 1, 9, 0)},
	}
	policies := map[string]ForgetPolicy{
		"hourly":  {KeepHourly: 2},
		"daily":   {KeepDaily: 2},
		"weekly":  {KeepWeekly: 2},
		"monthly": {KeepMonthly: 2},
		"yearly":  {KeepYearly: 2},
	}
	for name, policy := range policies {
		t.Run(name, func(t *testing.T) {
			got := ids(applyForgetPolicy(snaps, policy, zone(), afterAll).Keep)
			if !slices.Equal(got, []string{"s0", "s2"}) {
				t.Errorf("keeps %v, want [s0 s2]", got)
			}
		})
	}
}

// Periods are those of a clock in the given location, whatever zone a
// snapshot's time carries. Stored times are UTC, so every case names UTC
// instants and the Berlin wall clock they show.
func TestPeriodsAreCutInTheGivenLocation(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	utc := func(y int, m time.Month, d, h, minute int) time.Time {
		return time.Date(y, m, d, h, minute, 0, 0, time.UTC)
	}
	tests := []struct {
		name     string
		policy   ForgetPolicy
		times    []time.Time // newest first
		inBerlin []string
		inUTC    []string
	}{
		{
			"midnight splits two days",
			ForgetPolicy{KeepDaily: 2},
			[]time.Time{
				utc(2026, 1, 14, 23, 30), // 01-15 00:30 CET
				utc(2026, 1, 14, 22, 30), // 01-14 23:30 CET
			},
			[]string{"s0", "s1"},
			[]string{"s0"},
		},
		{
			"the 25 hours of the day summer time ends are one day",
			ForgetPolicy{KeepDaily: 3},
			[]time.Time{
				utc(2026, 10, 25, 23, 0),  // 10-26 00:00 CET
				utc(2026, 10, 25, 22, 59), // 10-25 23:59 CET
				utc(2026, 10, 24, 22, 0),  // 10-25 00:00 CEST
				utc(2026, 10, 24, 21, 59), // 10-24 23:59 CEST
			},
			[]string{"s0", "s1", "s3"},
			[]string{"s0", "s2"},
		},
		{
			"the 23 hours of the day summer time starts are one day",
			ForgetPolicy{KeepDaily: 3},
			[]time.Time{
				utc(2026, 3, 29, 22, 0),  // 03-30 00:00 CEST
				utc(2026, 3, 29, 21, 59), // 03-29 23:59 CEST
				utc(2026, 3, 28, 23, 0),  // 03-29 00:00 CET
				utc(2026, 3, 28, 22, 59), // 03-28 23:59 CET
			},
			[]string{"s0", "s1", "s3"},
			[]string{"s0", "s2"},
		},
		{
			"the hour the clock shows twice is one hour",
			ForgetPolicy{KeepHourly: 2},
			[]time.Time{
				utc(2026, 10, 25, 1, 30),  // 02:30 CET
				utc(2026, 10, 25, 0, 30),  // 02:30 CEST
				utc(2026, 10, 24, 23, 30), // 01:30 CEST
			},
			[]string{"s0", "s2"},
			[]string{"s0", "s1"},
		},
		{
			"Monday starts a week at local midnight",
			ForgetPolicy{KeepWeekly: 2},
			[]time.Time{
				utc(2026, 1, 11, 23, 30), // Monday 01-12 00:30 CET
				utc(2026, 1, 11, 22, 30), // Sunday 01-11 23:30 CET
			},
			[]string{"s0", "s1"},
			[]string{"s0"},
		},
		{
			"a month starts at local midnight",
			ForgetPolicy{KeepMonthly: 2},
			[]time.Time{
				utc(2026, 6, 30, 22, 30), // 07-01 00:30 CEST
				utc(2026, 6, 30, 21, 30), // 06-30 23:30 CEST
			},
			[]string{"s0", "s1"},
			[]string{"s0"},
		},
		{
			"a year starts at local midnight",
			ForgetPolicy{KeepYearly: 2},
			[]time.Time{
				utc(2026, 12, 31, 23, 30), // 2027-01-01 00:30 CET
				utc(2026, 12, 31, 22, 30), // 2026-12-31 23:30 CET
			},
			[]string{"s0", "s1"},
			[]string{"s0"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			snaps := make([]*Snapshot, len(tc.times))
			for i, ts := range tc.times {
				snaps[i] = &Snapshot{ID: fmt.Sprintf("s%d", i), CreatedAt: ts}
			}
			got := newestOfPeriods(applyForgetPolicy(snaps, tc.policy, berlin, afterAll))
			if !slices.Equal(got, tc.inBerlin) {
				t.Errorf("in Berlin keeps %v, want %v", got, tc.inBerlin)
			}
			got = newestOfPeriods(applyForgetPolicy(snaps, tc.policy, time.UTC, afterAll))
			if !slices.Equal(got, tc.inUTC) {
				t.Errorf("in UTC keeps %v, want %v", got, tc.inUTC)
			}
		})
	}
}

// newestOfPeriods lists the snapshots a rule keeps as the newest of a period,
// leaving out one kept only as the oldest of its series, which says nothing
// about where a period starts.
func newestOfPeriods(result *ForgetResult) []string {
	var out []string
	for _, s := range result.Keep {
		plain := func(reason string) bool { return !strings.HasPrefix(reason, "oldest ") }
		if slices.ContainsFunc(result.KeepReasons[s.ID], plain) {
			out = append(out, s.ID)
		}
	}
	return out
}

// Two snapshots whose own zones show the same wall clock are still a day
// apart on the one clock the periods are cut on.
func TestPeriodsIgnoreTheZoneASnapshotCarries(t *testing.T) {
	snaps := []*Snapshot{
		{ID: "s0", CreatedAt: time.Date(2026, 5, 2, 8, 0, 0, 0, time.FixedZone("", -12*60*60))},
		{ID: "s1", CreatedAt: time.Date(2026, 5, 2, 8, 0, 0, 0, time.FixedZone("", 14*60*60))},
	}
	for name, policy := range map[string]ForgetPolicy{"hourly": {KeepHourly: 2}, "daily": {KeepDaily: 2}} {
		t.Run(name, func(t *testing.T) {
			got := ids(applyForgetPolicy(snaps, policy, time.UTC, afterAll).Keep)
			if !slices.Equal(got, []string{"s0", "s1"}) {
				t.Errorf("keeps %v, want [s0 s1]", got)
			}
		})
	}
}

func TestWeekKeyIsDistinctPerISOWeek(t *testing.T) {
	seen := make(map[periodKey]time.Time)
	// 2000-01-03 is the Monday of week 1 of 2000.
	for monday := time.Date(2000, 1, 3, 0, 0, 0, 0, time.UTC); monday.Year() <= 2040; monday = monday.AddDate(0, 0, 7) {
		key := weekKey(monday)
		if other, dup := seen[key]; dup {
			t.Fatalf("weeks of %s and %s share key %v", other.Format(time.DateOnly), monday.Format(time.DateOnly), key)
		}
		seen[key] = monday
		if sunday := monday.AddDate(0, 0, 6).Add(23 * time.Hour); weekKey(sunday) != key {
			t.Fatalf("week of %s: Sunday has key %v, Monday %v", monday.Format(time.DateOnly), weekKey(sunday), key)
		}
	}
}

// A tag the user names keeps its snapshot whatever blanks either side has:
// backup stores a tag as given, ParseTagList trims.
func TestHasTagsIgnoresBlanks(t *testing.T) {
	for _, tc := range []struct{ tags, list []string }{
		{[]string{" pinned"}, []string{"pinned"}},
		{[]string{"pinned"}, []string{" pinned "}},
		{[]string{"db", " prod"}, []string{"db", "prod"}},
	} {
		if !hasTags(tc.tags, tc.list) {
			t.Errorf("hasTags(%q, %q) = false", tc.tags, tc.list)
		}
	}
	if hasTags([]string{"pinned x"}, []string{"pinned"}) {
		t.Error("a tag matched a longer one")
	}
}

// taggedFixture is one series, newest first, one snapshot a day:
//
//	s0  a, b
//	s1  a
//	s2  b
//	s3  no tag
//	s4  a, b
func taggedFixture() []*Snapshot {
	tags := [][]string{{"a", "b"}, {"a"}, {"b"}, nil, {"a", "b"}}
	snaps := make([]*Snapshot, len(tags))
	for i := range tags {
		at := time.Date(2026, 1, 5-i, 12, 0, 0, 0, time.UTC)
		snaps[i] = &Snapshot{ID: fmt.Sprintf("s%d", i), CreatedAt: at, Tags: tags[i]}
	}
	return snaps
}

// The cases and their reasons are what restic 0.18.1 answers for --keep-tag
// on these five snapshots.
func TestForgetKeepTags(t *testing.T) {
	const a, b, ab, last = "has tags [a]", "has tags [b]", "has tags [a, b]", "last snapshot"
	tests := []struct {
		name   string
		policy ForgetPolicy
		want   map[string][]string
	}{
		{"one tag", ForgetPolicy{KeepTags: [][]string{{"a"}}}, map[string][]string{
			"s0": {a}, "s1": {a}, "s4": {a},
		}},
		{"two lists, any of them", ForgetPolicy{KeepTags: [][]string{{"a"}, {"b"}}}, map[string][]string{
			"s0": {a, b}, "s1": {a}, "s2": {b}, "s4": {a, b},
		}},
		{"one list, all of its tags", ForgetPolicy{KeepTags: [][]string{{"a", "b"}}}, map[string][]string{
			"s0": {ab}, "s4": {ab},
		}},
		{"a tag no snapshot has", ForgetPolicy{KeepTags: [][]string{{"zz"}}}, map[string][]string{}},
		{"the empty tag is no tag", ForgetPolicy{KeepTags: [][]string{{""}}}, map[string][]string{
			"s3": {"has tags []"},
		}},
		{"the empty tag first decides for an untagged snapshot", ForgetPolicy{KeepTags: [][]string{{"", "a"}}},
			map[string][]string{"s3": {"has tags [, a]"}}},
		{"the empty tag after a tag is never reached", ForgetPolicy{KeepTags: [][]string{{"a", ""}}},
			map[string][]string{}},
		{"a tagged snapshot uses a KeepLast slot", ForgetPolicy{KeepLast: 3, KeepTags: [][]string{{"a"}}},
			map[string][]string{"s0": {a, last}, "s1": {a, last}, "s2": {last}, "s4": {a}}},
		{"tags come before within", ForgetPolicy{KeepWithin: Within{Hours: 36}, KeepTags: [][]string{{"b"}}},
			map[string][]string{"s0": {b, "within 36h"}, "s1": {"within 36h"}, "s2": {b}, "s4": {b}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			snaps := taggedFixture()
			res := applyForgetPolicy(snaps, tc.policy, time.UTC, afterAll)
			if !maps.EqualFunc(res.KeepReasons, tc.want, slices.Equal) {
				t.Errorf("reasons %v, want %v", res.KeepReasons, tc.want)
			}
			if got := len(res.Keep); got != len(tc.want) || got+len(res.Remove) != len(snaps) {
				t.Errorf("keeps %d and removes %d of %d, want %d kept", got, len(res.Remove), len(snaps), len(tc.want))
			}
			if err := checkSeriesKept(snaps, res.Keep); (err != nil) != (len(tc.want) == 0) {
				t.Errorf("checkSeriesKept = %v with %d kept", err, len(tc.want))
			}
		})
	}
}
