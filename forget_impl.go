package ark

import (
	"fmt"
	"sort"
	"time"
)

// applyForgetPolicy applies policy once per series (source type, source name
// and host) and merges the groups, so one busy series cannot push another's
// snapshots out of the window. Snapshots are expected to be sorted newest
// first. The period rules count in loc.
func applyForgetPolicy(snapshots []*Snapshot, policy ForgetPolicy, loc *time.Location) *ForgetResult {
	result := &ForgetResult{KeepReasons: make(map[string][]string)}
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
		applyForgetPolicyToGroup(result, groups[key], policy, loc)
	}
	sortNewestFirst(result.Keep)
	sortNewestFirst(result.Remove)
	return result
}

func sortNewestFirst(snaps []*Snapshot) {
	sort.SliceStable(snaps, func(i, j int) bool { return snaps[i].CreatedAt.After(snaps[j].CreatedAt) })
}

// applyForgetPolicyToGroup adds the keep and remove picks and the keep
// reasons for one series, which is not empty, to result.
func applyForgetPolicyToGroup(result *ForgetResult, snapshots []*Snapshot, policy ForgetPolicy, loc *time.Location) {
	reasons := result.KeepReasons
	keep := func(s *Snapshot, reason string) { reasons[s.ID] = append(reasons[s.ID], reason) }

	// KeepWithin: everything younger than the window, measured from the
	// newest snapshot.
	if policy.KeepWithin > 0 {
		cutoff := snapshots[0].CreatedAt.Add(-policy.KeepWithin)
		reason := "within " + withinString(policy.KeepWithin)
		for _, s := range snapshots {
			if s.CreatedAt.After(cutoff) {
				keep(s, reason)
			}
		}
	}

	// KeepLast: keep the most recent N.
	for i, s := range snapshots {
		if policy.KeepLast != Unlimited && i >= int(policy.KeepLast) {
			break
		}
		keep(s, "last snapshot")
	}

	// Period keys are the numbers on a clock in loc. The hour a clock shows
	// twice when summer time ends is one period, as it is for restic.
	markPeriod(snapshots, keep, "hourly snapshot", policy.KeepHourly, loc, func(t time.Time) [4]int {
		return [4]int{t.Year(), int(t.Month()), t.Day(), t.Hour()}
	})
	markPeriod(snapshots, keep, "daily snapshot", policy.KeepDaily, loc, func(t time.Time) [3]int {
		return [3]int{t.Year(), int(t.Month()), t.Day()}
	})
	markPeriod(snapshots, keep, "weekly snapshot", policy.KeepWeekly, loc, weekKey)
	markPeriod(snapshots, keep, "monthly snapshot", policy.KeepMonthly, loc, func(t time.Time) [2]int {
		return [2]int{t.Year(), int(t.Month())}
	})
	markPeriod(snapshots, keep, "yearly snapshot", policy.KeepYearly, loc, time.Time.Year)

	for _, s := range snapshots {
		if _, ok := reasons[s.ID]; ok {
			result.Keep = append(result.Keep, s)
		} else {
			result.Remove = append(result.Remove, s)
		}
	}
}

// markPeriod keeps the newest snapshot of each of the n newest periods, of
// every period when n is Unlimited, for reason; key names the period a time
// in loc falls in.
func markPeriod[K comparable](
	snapshots []*Snapshot,
	keep func(*Snapshot, string),
	reason string,
	n KeepCount,
	loc *time.Location,
	key func(time.Time) K,
) {
	periods := make(map[K]struct{})
	for _, s := range snapshots {
		if n != Unlimited && len(periods) >= int(n) {
			break
		}
		p := key(s.CreatedAt.In(loc))
		if _, seen := periods[p]; !seen {
			periods[p] = struct{}{}
			keep(s, reason)
		}
	}
}

// weekKey names the ISO week of t. The year is the ISO year, which differs
// from t.Year() in the days around a year turn.
func weekKey(t time.Time) [2]int {
	year, week := t.ISOWeek()
	return [2]int{year, week}
}

// withinString writes d the way restic writes a keep-within duration, in
// days and hours with zero parts left out, such as "30d" or "1d12h". A
// window that is not whole hours, which restic cannot express, keeps Go's
// form.
func withinString(d time.Duration) string {
	if d%time.Hour != 0 {
		return d.String()
	}
	days, hours := d/(24*time.Hour), d%(24*time.Hour)/time.Hour
	switch {
	case hours == 0:
		return fmt.Sprintf("%dd", days)
	case days == 0:
		return fmt.Sprintf("%dh", hours)
	}
	return fmt.Sprintf("%dd%dh", days, hours)
}
