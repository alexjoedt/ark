package ark

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

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
		{"KeepWithin", ForgetPolicy{KeepWithin: 25 * time.Hour}, []string{"s0", "s1", "s2", "s3"}},
		{
			"fields combine",
			ForgetPolicy{KeepLast: 1, KeepYearly: 3},
			[]string{"s0", "s8", "s9"},
		},
	}

	snapshots := forgetFixture()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := applyForgetPolicy(snapshots, tc.policy)

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
			res := applyForgetPolicy(snaps, ForgetPolicy{KeepLast: 1})

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
			got := ids(applyForgetPolicy(snaps, ForgetPolicy{KeepWeekly: 2}).Keep)
			if !slices.Equal(got, tc.keep) {
				t.Errorf("keeps %v, want %v", got, tc.keep)
			}
		})
	}
}

func TestWeekKeyIsDistinctPerISOWeek(t *testing.T) {
	seen := make(map[[2]int]time.Time)
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
