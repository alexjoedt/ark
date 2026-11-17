package ark

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

// applyForgetPolicy applies policy once per series (source type, source name
// and host) and merges the groups, so one busy series cannot push another's
// snapshots out of the window. Snapshots are expected to be sorted newest
// first. The period rules count in loc; a snapshot after now is in the future.
func applyForgetPolicy(snapshots []*Snapshot, policy ForgetPolicy, loc *time.Location, now time.Time) *ForgetResult {
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
		applyForgetPolicyToGroup(result, groups[key], policy, loc, now)
	}
	sortNewestFirst(result.Keep)
	sortNewestFirst(result.Remove)
	return result
}

func sortNewestFirst(snaps []*Snapshot) {
	sort.SliceStable(snaps, func(i, j int) bool { return snaps[i].CreatedAt.After(snaps[j].CreatedAt) })
}

// periodKey names the period of one counted rule a snapshot falls in: the
// numbers a clock shows, down to the rule's unit, zero beyond it.
type periodKey [4]int

// keepBucket is one counted rule on its walk through a series.
type keepBucket struct {
	reason string
	// left is the count the rule still has, Unlimited for no limit.
	left KeepCount
	// period names the period of the snapshot at index nr, taken at t.
	period func(t time.Time, nr int) periodKey
	// last is the period of the snapshot the rule kept last.
	last periodKey
}

// applyForgetPolicyToGroup adds the keep and remove picks and the keep
// reasons for one series, which is not empty and sorted newest first, to
// result. It is restic's walk: one pass from the newest snapshot to the
// oldest, every rule deciding on each snapshot by itself.
func applyForgetPolicyToGroup(
	result *ForgetResult,
	snapshots []*Snapshot,
	policy ForgetPolicy,
	loc *time.Location,
	now time.Time,
) {
	reasons := result.KeepReasons

	// KeepWithin: everything younger than the window, measured from the
	// newest snapshot that is not in the future. When all are, latest stays
	// the zero time and every snapshot is after the cutoff, as for restic.
	var latest time.Time
	if i := slices.IndexFunc(snapshots, func(s *Snapshot) bool { return !s.CreatedAt.After(now) }); i >= 0 {
		latest = snapshots[i].CreatedAt
	}
	cutoff := policy.KeepWithin.before(latest.In(loc))
	within := "within " + policy.KeepWithin.String()

	// Period keys are the numbers on a clock in loc. The hour a clock shows
	// twice when summer time ends is one period, as it is for restic.
	// KeepLast counts every snapshot as a period of its own.
	buckets := []keepBucket{
		{reason: "last snapshot", left: policy.KeepLast, period: func(_ time.Time, nr int) periodKey {
			return periodKey{nr}
		}},
		{reason: "hourly snapshot", left: policy.KeepHourly, period: func(t time.Time, _ int) periodKey {
			return periodKey{t.Year(), int(t.Month()), t.Day(), t.Hour()}
		}},
		{reason: "daily snapshot", left: policy.KeepDaily, period: func(t time.Time, _ int) periodKey {
			return periodKey{t.Year(), int(t.Month()), t.Day()}
		}},
		{reason: "weekly snapshot", left: policy.KeepWeekly, period: func(t time.Time, _ int) periodKey {
			return weekKey(t)
		}},
		{reason: "monthly snapshot", left: policy.KeepMonthly, period: func(t time.Time, _ int) periodKey {
			return periodKey{t.Year(), int(t.Month())}
		}},
		{reason: "yearly snapshot", left: policy.KeepYearly, period: func(t time.Time, _ int) periodKey {
			return periodKey{t.Year()}
		}},
	}

	for nr, s := range snapshots {
		for _, list := range policy.KeepTags {
			if hasTags(s.Tags, list) {
				reasons[s.ID] = append(reasons[s.ID], "has tags ["+strings.Join(list, ", ")+"]")
			}
		}
		if policy.KeepWithin != (Within{}) && s.CreatedAt.After(cutoff) {
			reasons[s.ID] = append(reasons[s.ID], within)
		}
		t := s.CreatedAt.In(loc)
		for i := range buckets {
			b := &buckets[i]
			if b.left == 0 {
				continue
			}
			// A rule keeps the newest snapshot of each period, and with
			// count left at the end of the series its oldest snapshot too,
			// so a series shorter than the policy is kept whole.
			period := b.period(t, nr)
			reason := b.reason
			if nr > 0 && period == b.last {
				if nr < len(snapshots)-1 {
					continue
				}
				reason = "oldest " + reason
			}
			b.last = period
			if b.left != Unlimited {
				b.left--
			}
			reasons[s.ID] = append(reasons[s.ID], reason)
		}

		if _, ok := reasons[s.ID]; ok {
			result.Keep = append(result.Keep, s)
		} else {
			result.Remove = append(result.Remove, s)
		}
	}
}

// hasTags reports whether a snapshot with tags matches one KeepTags list: it
// carries every tag of the list. As in restic, the empty tag matches a
// snapshot without tags, whatever the list names after it. Blanks around a
// tag count on neither side: ParseTagList drops them, a backup stores a tag
// as given, and a tag the user named must not miss its snapshot over them.
func hasTags(tags, list []string) bool {
	for _, want := range list {
		want = strings.TrimSpace(want)
		if want == "" && len(tags) == 0 {
			return true
		}
		if !slices.ContainsFunc(tags, func(tag string) bool { return strings.TrimSpace(tag) == want }) {
			return false
		}
	}
	return true
}

// checkSeriesKept returns ErrNoSnapshotKept when a series among snapshots
// has no snapshot in keep, naming the first such series.
func checkSeriesKept(snapshots, keep []*Snapshot) error {
	kept := make(map[seriesKey]bool)
	for _, s := range keep {
		kept[s.series()] = true
	}
	for _, s := range snapshots {
		if !kept[s.series()] {
			return fmt.Errorf("%w: %s %q of host %q", ErrNoSnapshotKept, s.SourceType, s.SourceName, s.Host)
		}
	}
	return nil
}

// weekKey names the ISO week of t. The year is the ISO year, which differs
// from t.Year() in the days around a year turn.
func weekKey(t time.Time) periodKey {
	year, week := t.ISOWeek()
	return periodKey{year, week}
}
