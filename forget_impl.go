package ark

import (
	"sort"
	"time"
)

// applyForgetPolicy applies policy once per source name and merges the
// groups, so one busy source cannot push another's snapshots out of the
// window. Snapshots are expected to be sorted newest first.
func applyForgetPolicy(snapshots []*Snapshot, policy ForgetPolicy) *ForgetResult {
	result := &ForgetResult{}
	groups := make(map[string][]*Snapshot)
	var order []string
	for _, s := range snapshots {
		if _, seen := groups[s.SourceName]; !seen {
			order = append(order, s.SourceName)
		}
		groups[s.SourceName] = append(groups[s.SourceName], s)
	}
	for _, name := range order {
		g := applyForgetPolicyToGroup(groups[name], policy)
		result.Keep = append(result.Keep, g.Keep...)
		result.Remove = append(result.Remove, g.Remove...)
	}
	sortNewestFirst(result.Keep)
	sortNewestFirst(result.Remove)
	return result
}

func sortNewestFirst(snaps []*Snapshot) {
	sort.SliceStable(snaps, func(i, j int) bool { return snaps[i].CreatedAt.After(snaps[j].CreatedAt) })
}

// applyForgetPolicyToGroup selects keep and remove for one source name.
func applyForgetPolicyToGroup(snapshots []*Snapshot, policy ForgetPolicy) *ForgetResult {
	result := &ForgetResult{}
	if len(snapshots) == 0 {
		return result
	}

	keep := make(map[string]struct{})

	// KeepWithin: everything younger than the window, measured from the
	// newest snapshot.
	if policy.KeepWithin > 0 {
		cutoff := snapshots[0].CreatedAt.Add(-policy.KeepWithin)
		for _, s := range snapshots {
			if s.CreatedAt.After(cutoff) {
				keep[s.ID] = struct{}{}
			}
		}
	}

	// KeepLast: keep the most recent N.
	if policy.KeepLast > 0 {
		for i := 0; i < policy.KeepLast && i < len(snapshots); i++ {
			keep[snapshots[i].ID] = struct{}{}
		}
	}

	markPeriod := func(n int, periodStart func(time.Time) time.Time) {
		if n <= 0 {
			return
		}
		periods := make(map[time.Time]struct{})
		for _, s := range snapshots {
			if len(periods) >= n {
				break
			}
			p := periodStart(s.CreatedAt)
			if _, seen := periods[p]; !seen {
				periods[p] = struct{}{}
				keep[s.ID] = struct{}{}
			}
		}
	}

	markPeriod(policy.KeepHourly, func(t time.Time) time.Time {
		return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, t.Location())
	})
	markPeriod(policy.KeepDaily, func(t time.Time) time.Time {
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	})
	markPeriod(policy.KeepWeekly, func(t time.Time) time.Time {
		year, week := t.ISOWeek()
		// Monday of the ISO week.
		jan4 := time.Date(year, 1, 4, 0, 0, 0, 0, t.Location())
		monday := jan4.AddDate(0, 0, -int(jan4.Weekday()-time.Monday))
		return monday.AddDate(0, 0, (week-1)*7)
	})
	markPeriod(policy.KeepMonthly, func(t time.Time) time.Time {
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location())
	})
	markPeriod(policy.KeepYearly, func(t time.Time) time.Time {
		return time.Date(t.Year(), 1, 1, 0, 0, 0, 0, t.Location())
	})

	for _, s := range snapshots {
		if _, ok := keep[s.ID]; ok {
			result.Keep = append(result.Keep, s)
		} else {
			result.Remove = append(result.Remove, s)
		}
	}

	return result
}
