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

func TestForgetPolicyAppliesPerSourceName(t *testing.T) {
	snaps := forgetFixture()
	for i, s := range snaps {
		s.SourceName = "a"
		if i%2 == 1 {
			s.SourceName = "b"
		}
	}
	res := applyForgetPolicy(snaps, ForgetPolicy{KeepLast: 1})

	var keep []string
	for _, s := range res.Keep {
		keep = append(keep, s.ID)
	}
	if !slices.Equal(keep, []string{"s0", "s1"}) {
		t.Fatalf("keep = %v, want the newest of each source", keep)
	}
	if len(res.Remove) != 8 || res.Remove[0].ID != "s2" || res.Remove[7].ID != "s9" {
		t.Fatalf("remove = %d entries, first %s", len(res.Remove), res.Remove[0].ID)
	}
}
