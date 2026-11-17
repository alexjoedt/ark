package ark

import (
	"sort"
	"time"
)

// applyForgetPolicy applies policy once per series (source type, source name
// and host) and merges the groups, so one busy series cannot push another's
// snapshots out of the window. Snapshots are expected to be sorted newest
// first.
func applyForgetPolicy(snapshots []*Snapshot, policy ForgetPolicy) *ForgetResult {
	result := &ForgetResult{}
	groups := make(map[seriesKey][]*Snapshot)
	var order []seriesKey
	for _, s := range snapshots {
		key := s.series()
		if _, seen := groups[key]; !seen {
			order = append(order, key)
		}
		groups[key] = append(groups[key], s)
	}
	for _, key := range order {
		g := applyForgetPolicyToGroup(groups[key], policy)
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

// applyForgetPolicyToGroup selects keep and remove for one series.
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

	// Period keys are plain numbers: two time.Time values of one instant are
	// unequal as map keys when their *time.Location pointers differ.
	markPeriod(snapshots, keep, policy.KeepHourly, func(t time.Time) [4]int {
		return [4]int{t.Year(), int(t.Month()), t.Day(), t.Hour()}
	})
	markPeriod(snapshots, keep, policy.KeepDaily, func(t time.Time) [3]int {
		return [3]int{t.Year(), int(t.Month()), t.Day()}
	})
	markPeriod(snapshots, keep, policy.KeepWeekly, weekKey)
	markPeriod(snapshots, keep, policy.KeepMonthly, func(t time.Time) [2]int {
		return [2]int{t.Year(), int(t.Month())}
	})
	markPeriod(snapshots, keep, policy.KeepYearly, time.Time.Year)

	for _, s := range snapshots {
		if _, ok := keep[s.ID]; ok {
			result.Keep = append(result.Keep, s)
		} else {
			result.Remove = append(result.Remove, s)
		}
	}

	return result
}

// markPeriod keeps the newest snapshot of each of the n newest periods; key
// names the period a time falls in.
func markPeriod[K comparable](snapshots []*Snapshot, keep map[string]struct{}, n int, key func(time.Time) K) {
	if n <= 0 {
		return
	}
	periods := make(map[K]struct{})
	for _, s := range snapshots {
		if len(periods) >= n {
			break
		}
		p := key(s.CreatedAt)
		if _, seen := periods[p]; !seen {
			periods[p] = struct{}{}
			keep[s.ID] = struct{}{}
		}
	}
}

// weekKey names the ISO week of t. The year is the ISO year, which differs
// from t.Year() in the days around a year turn.
func weekKey(t time.Time) [2]int {
	year, week := t.ISOWeek()
	return [2]int{year, week}
}
