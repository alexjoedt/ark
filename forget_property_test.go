package ark

import (
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The environment of TestForgetProperties: the seed that replays a failure
// and the number of cases to draw.
const (
	forgetEnvSeed = "ARK_FORGET_SEED"
	forgetEnvRuns = "ARK_FORGET_RUNS"
)

const (
	propDay  = 24 * 3600
	propYear = 365 * propDay
)

// propTagLists are the KeepTags lists a policy draws from; the snapshots
// carry tags from a, b and c.
var propTagLists = [][]string{{"a"}, {"b"}, {"c"}, {"a", "b"}, {"b", "c"}, {""}}

// propSchedules are the steps of a history that is taken on a schedule.
var propSchedules = []struct{ months, days, hours int }{
	{0, 0, 1}, {0, 0, 6}, {0, 1, 0}, {0, 7, 0}, {1, 0, 0}, {12, 0, 0},
}

// forgetCase is a drawn history of one series, a policy, the zone forget
// counts in and the clock it runs by.
type forgetCase struct {
	loc    *time.Location
	now    time.Time
	snaps  []*Snapshot // newest first
	policy ForgetPolicy
}

func (c forgetCase) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "zone %s, now %s\npolicy %+v\n", c.loc, c.now.In(c.loc).Format(time.RFC3339), c.policy)
	for _, s := range c.snaps {
		fmt.Fprintf(&b, "%5s  %s  %v\n", s.ID, s.CreatedAt.In(c.loc).Format(time.RFC3339), s.Tags)
	}
	return b.String()
}

func (c forgetCase) apply(snaps []*Snapshot, policy ForgetPolicy) *ForgetResult {
	return applyForgetPolicy(snaps, policy, c.loc, c.now)
}

// reference returns the time the keep-within windows of snaps are measured
// from, that of the newest snapshot that is not in the future, and the zero
// time when all are.
func (c forgetCase) reference(snaps []*Snapshot) time.Time {
	for _, s := range snaps {
		if !s.CreatedAt.After(c.now) {
			return s.CreatedAt.In(c.loc)
		}
	}
	return time.Time{}.In(c.loc)
}

// windowStartsLater reports whether a keep-within window of the policy
// starts later over the snapshots in kept than over all of the case.
func (c forgetCase) windowStartsLater(kept []*Snapshot) bool {
	first, second := c.reference(c.snaps), c.reference(kept)
	for _, rule := range c.policy.windows() {
		if rule.w != (Within{}) && rule.w.before(second).After(rule.w.before(first)) {
			return true
		}
	}
	return false
}

func unixDate(y int, m time.Month, d int) int64 {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix()
}

// logUniform draws from [lo, hi), each doubling of lo as likely as the next.
func logUniform(rng *rand.Rand, lo, hi int64) int64 {
	doublings := 0
	for v := lo; v < hi; v *= 2 {
		doublings++
	}
	v := lo << rng.IntN(doublings)
	return v + rng.Int64N(min(v, hi-v))
}

// forgetZones loads the zones of the restic fixtures: no summer time, summer
// time on both hemispheres, a change at midnight, a half-hour change and
// offsets off the full hour. The files are pinned, so a seed draws the same
// cases on every host.
func forgetZones(t *testing.T) []*time.Location {
	t.Helper()
	var zones []*time.Location
	err := filepath.WalkDir(resticZoneinfo, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		name, err := filepath.Rel(resticZoneinfo, path)
		if err != nil {
			return err
		}
		loc, err := time.LoadLocationFromTZData(filepath.ToSlash(name), data)
		zones = append(zones, loc)
		return err
	})
	if err != nil || len(zones) == 0 {
		t.Fatalf("%d zones in %s: %v", len(zones), resticZoneinfo, err)
	}
	return zones
}

// drawForgetCase draws a case in the shape of the restic fixtures, and what
// those leave out: snapshots of the same instant, both instants of an hour
// the clocks repeat, and a clock inside the history, which puts the
// snapshots after it in the future.
func drawForgetCase(rng *rand.Rand, zones []*time.Location) forgetCase {
	c := forgetCase{loc: zones[rng.IntN(len(zones))], now: afterAll}
	c.snaps = drawHistory(rng, c.loc, "a", seriesKey{SourceTypeFile, "s", "h"})
	if rng.IntN(5) == 0 {
		// The snapshot at the clock is not in the future, the one a second
		// after it is.
		at := c.snaps[rng.IntN(len(c.snaps))].CreatedAt
		c.now = at.Add(-time.Duration(rng.IntN(2)) * time.Second)
	}
	newest := c.reference(c.snaps)
	if newest.IsZero() {
		newest = c.snaps[0].CreatedAt.In(c.loc)
	}
	c.policy = drawPolicy(rng, c.snaps, newest)
	return c
}

// drawHistory draws 1 to 200 snapshots of series, newest first and not
// before 2000, with gaps from a minute to two years. Their IDs start with id.
func drawHistory(rng *rand.Rand, loc *time.Location, id string, series seriesKey) []*Snapshot {
	n := 1
	if rng.IntN(100) >= 4 {
		n = int(logUniform(rng, 2, 201))
	}

	// The history ends, a third of the time each, at the turn of 2015 to
	// 2016 (2015 has an ISO week 53), just after the zone changes its
	// offset, and anywhere. A zone without a change ends anywhere instead.
	// A change from 2025 on is none: some zone files end in a placeholder
	// for 2038.
	//
	// One history of five is a schedule, the same wall clock every hour,
	// day, week, month or year counted from the newest snapshot, so that a
	// keep-within window ends exactly on a snapshot; of the histories that
	// end after a change of the offset it is every second.
	end := unixDate(2012, 1, 1) + rng.Int64N(13*propYear)
	scheduled := rng.IntN(5) == 0
	switch p := rng.IntN(100); {
	case p < 35:
		end = unixDate(2015, 12, 28) + rng.Int64N(24*propDay)
	case p < 65:
		some := time.Unix(unixDate(2012, 1, 1)+rng.Int64N(10*propYear), 0).In(loc)
		if _, shift := some.ZoneBounds(); !shift.IsZero() && shift.Unix() < unixDate(2025, 1, 1) {
			end = shift.Unix() + rng.Int64N(3*propDay)
			scheduled = scheduled || rng.IntN(2) == 0
		}
	}

	maxGap := logUniform(rng, 3600, 2*propYear)
	at := time.Unix(end, 0).In(loc)
	back := func(int) time.Time {
		t := at
		at = at.Add(-time.Duration(logUniform(rng, 60, maxGap+1)) * time.Second)
		return t
	}
	if scheduled {
		s := propSchedules[rng.IntN(len(propSchedules))]
		back = func(k int) time.Time {
			return at.AddDate(0, -k*s.months, -k*s.days).Add(-time.Duration(k*s.hours) * time.Hour)
		}
	}

	// One history in four holds snapshots of the same instant, half of the
	// histories tags, the later ones rarer.
	twice := rng.IntN(4) == 0
	tagged := []int{0, 0, 25, 60}[rng.IntN(4)]
	var snaps []*Snapshot
	var t time.Time
	for k := 0; len(snaps) < n; k++ {
		if k == 0 || !twice || rng.IntN(8) > 0 {
			t = back(k)
		}
		if t.Year() < 2000 {
			break
		}
		s := &Snapshot{
			ID:         id + strconv.Itoa(len(snaps)),
			SourceType: series.sourceType,
			SourceName: series.sourceName,
			Host:       series.host,
			CreatedAt:  t.UTC(),
		}
		for i, tag := range []string{"a", "b", "c"} {
			if rng.IntN(100) < tagged>>i {
				s.Tags = append(s.Tags, tag)
			}
		}
		snaps = append(snaps, s)
	}
	sortNewestFirst(snaps)
	return snaps
}

// policyCounts and policyWindows list the rules of p in the order of
// p.counted and p.windows, which name them.
func policyCounts(p *ForgetPolicy) [6]*KeepCount {
	return [6]*KeepCount{&p.KeepLast, &p.KeepHourly, &p.KeepDaily, &p.KeepWeekly, &p.KeepMonthly, &p.KeepYearly}
}

func policyWindows(p *ForgetPolicy) [6]*Within {
	return [6]*Within{
		&p.KeepWithin, &p.KeepWithinHourly, &p.KeepWithinDaily,
		&p.KeepWithinWeekly, &p.KeepWithinMonthly, &p.KeepWithinYearly,
	}
}

// drawPolicy draws one to three rules most of the time, sometimes more and
// sometimes every rule at once. newest is the snapshot the windows are
// measured from, on the clock forget counts by.
func drawPolicy(rng *rand.Rand, snaps []*Snapshot, newest time.Time) ForgetPolicy {
	var p ForgetPolicy
	c, w := policyCounts(&p), policyWindows(&p)
	rules := len(c) + len(w) + 1
	n := rules
	switch pick := rng.IntN(100); {
	case pick < 30:
		n = 1
	case pick < 60:
		n = 2
	case pick < 80:
		n = 3
	case pick < 94:
		n = 4 + rng.IntN(3)
	}
	for _, r := range rng.Perm(rules)[:n] {
		switch {
		case r < len(c):
			*c[r] = Unlimited
			if rng.IntN(100) >= 12 {
				*c[r] = KeepCount(logUniform(rng, 1, 100))
			}
		case r < len(c)+len(w):
			*w[r-len(c)] = drawWithin(rng, snaps, newest)
		default:
			for _, i := range rng.Perm(len(propTagLists))[:1+rng.IntN(2)] {
				p.KeepTags = append(p.KeepTags, propTagLists[i])
			}
		}
	}
	return p
}

// drawWithin draws a keep-within window. Half of the windows are fitted to
// the history: the hours, days or months from one of its snapshots to
// newest, so that the window ends at that snapshot. The others are random,
// half of them of one unit.
func drawWithin(rng *rand.Rand, snaps []*Snapshot, newest time.Time) Within {
	if rng.IntN(2) == 0 {
		t := snaps[rng.IntN(len(snaps))].CreatedAt.In(newest.Location())
		date := func(t time.Time) int { return int(unixDate(t.Year(), t.Month(), t.Day()) / propDay) }
		months := (newest.Year()-t.Year())*12 + int(newest.Month()-t.Month())
		fitted := []Within{
			{Hours: int(newest.Sub(t).Hours())},
			{Days: date(newest) - date(t)},
			{Years: months / 12, Months: months % 12},
		}[rng.IntN(3)]
		if fitted != (Within{}) && fitted.validate() == nil {
			return fitted
		}
	}
	for {
		var w Within
		parts := []struct {
			n   *int
			end int64
		}{{&w.Years, 13}, {&w.Months, maxWithinMonths + 1}, {&w.Days, 1000}, {&w.Hours, 2000}}
		single, one := rng.IntN(2) == 0, rng.IntN(len(parts))
		for i, part := range parts {
			if single && i == one || !single && rng.IntN(100) < 40 {
				*part.n = int(logUniform(rng, 1, part.end))
			}
		}
		if w != (Within{}) {
			return w
		}
	}
}

// raised returns a count above n, sometimes far above and sometimes
// Unlimited. An unlimited count has none above it.
func raised(rng *rand.Rand, n KeepCount) (KeepCount, bool) {
	switch {
	case n == Unlimited:
		return n, false
	case rng.IntN(8) == 0:
		return Unlimited, true
	}
	return n + KeepCount(logUniform(rng, 1, 100)), true
}

// widened returns w with one of its numbers raised.
func widened(rng *rand.Rand, w Within) Within {
	switch part := rng.IntN(4); {
	case part == 0:
		w.Years += int(logUniform(rng, 1, 13))
	case part == 1 && w.Months < maxWithinMonths:
		w.Months += 1 + rng.IntN(maxWithinMonths-w.Months)
	case part == 2:
		w.Days += int(logUniform(rng, 1, 1000))
	default:
		w.Hours += int(logUniform(rng, 1, 2000))
	}
	return w
}

func keepSet(res *ForgetResult) map[*Snapshot]bool {
	kept := make(map[*Snapshot]bool, len(res.Keep))
	for _, s := range res.Keep {
		kept[s] = true
	}
	return kept
}

// checkPartition reports a snapshot of in that is not exactly once in Keep
// or Remove, one in there that is not of in, and keep reasons that are not
// those of the kept snapshots.
func checkPartition(in []*Snapshot, res *ForgetResult) error {
	times := make(map[*Snapshot]int)
	for _, s := range slices.Concat(res.Keep, res.Remove) {
		times[s]++
	}
	for _, s := range in {
		if times[s] != 1 {
			return fmt.Errorf("partition: snapshot %s is %d times in Keep and Remove", s.ID, times[s])
		}
	}
	if len(times) != len(in) {
		return fmt.Errorf("partition: Keep and Remove hold %d snapshots, the input %d", len(times), len(in))
	}
	if len(res.KeepReasons) != len(res.Keep) {
		return fmt.Errorf("partition: reasons for %d snapshots, %d kept", len(res.KeepReasons), len(res.Keep))
	}
	for _, s := range res.Keep {
		if len(res.KeepReasons[s.ID]) == 0 {
			return fmt.Errorf("partition: snapshot %s is kept for no reason", s.ID)
		}
	}
	return nil
}

// checkNewest reports a series of in, which is sorted newest first, whose
// newest snapshot is not kept. A policy of KeepTags alone has no such
// snapshot: it keeps what carries the tags and nothing else.
func checkNewest(in []*Snapshot, policy ForgetPolicy, res *ForgetResult) error {
	policy.KeepTags = nil
	if policy.Empty() {
		return nil
	}
	kept, seen := keepSet(res), make(map[seriesKey]bool)
	for _, s := range in {
		if !seen[s.series()] && !kept[s] {
			return fmt.Errorf("newest: snapshot %s, the newest of its series, is removed", s.ID)
		}
		seen[s.series()] = true
	}
	return nil
}

// check holds the case against the invariants of a keep policy. rng draws
// the looser policies and the second series.
func (c forgetCase) check(rng *rand.Rand) error {
	res := c.apply(c.snaps, c.policy)
	if err := errors.Join(checkPartition(c.snaps, res), checkNewest(c.snaps, c.policy, res)); err != nil {
		return err
	}

	// Monotonic: every rule by itself, raised, keeps what was kept before.
	superset := func(rule string, loose ForgetPolicy) error {
		kept := keepSet(c.apply(c.snaps, loose))
		for _, s := range res.Keep {
			if !kept[s] {
				return fmt.Errorf("monotonic: snapshot %s is removed with a larger %s:\n%+v", s.ID, rule, loose)
			}
		}
		return nil
	}
	for i, rule := range c.policy.counted() {
		loose := c.policy
		if n, ok := raised(rng, rule.n); ok {
			*policyCounts(&loose)[i] = n
			if err := superset(rule.name, loose); err != nil {
				return err
			}
		}
	}
	for i, rule := range c.policy.windows() {
		loose := c.policy
		*policyWindows(&loose)[i] = widened(rng, rule.w)
		if err := superset(rule.name, loose); err != nil {
			return err
		}
	}
	loose := c.policy
	loose.KeepTags = append(slices.Clone(loose.KeepTags), propTagLists[rng.IntN(len(propTagLists))])
	if err := superset("KeepTags", loose); err != nil {
		return err
	}

	// Idempotent: the policy removes nothing of what it kept, unless the
	// second run measures a window from another snapshot and the window
	// starts later for it; see TestForgetTwiceUnderAFutureSnapshot.
	if again := c.apply(res.Keep, c.policy); len(again.Remove) > 0 && !c.windowStartsLater(res.Keep) {
		return fmt.Errorf("idempotent: a second run removes %v of what the first kept", ids(again.Remove))
	}

	// Independent: next to a series that differs in one part of the series
	// key, every snapshot is decided as before, for the same reasons.
	series := c.snaps[0].series()
	switch rng.IntN(3) {
	case 0:
		series.sourceType = SourceTypeStream
	case 1:
		series.sourceName = "t"
	default:
		series.host = "g"
	}
	other := drawHistory(rng, c.loc, "b", series)
	both := slices.Concat(c.snaps, other)
	if rng.IntN(2) == 0 {
		both = slices.Concat(other, c.snaps) // the other series first among equal instants
	}
	sortNewestFirst(both)
	with := c.apply(both, c.policy)
	if err := errors.Join(checkPartition(both, with), checkNewest(both, c.policy, with)); err != nil {
		return fmt.Errorf("next to series %q: %w", series, err)
	}
	for _, s := range c.snaps {
		if alone, next := res.KeepReasons[s.ID], with.KeepReasons[s.ID]; !slices.Equal(alone, next) {
			return fmt.Errorf("independent: snapshot %s is kept for %q, next to series %q for %q",
				s.ID, alone, series, next)
		}
	}
	return nil
}

// TestForgetProperties holds the keep policy against what must be true of
// every history and every policy, on random cases: Keep and Remove partition
// the snapshots, the newest snapshot of a series is kept, a larger count or
// window keeps a superset, a second run removes nothing, and a series is
// decided without regard to the others. A failure prints the seed;
// ARK_FORGET_SEED replays it, ARK_FORGET_RUNS sets the number of cases.
func TestForgetProperties(t *testing.T) {
	env := func(name string, unset uint64) uint64 {
		s := os.Getenv(name)
		if s == "" {
			return unset
		}
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			t.Fatalf("%s=%q: want a whole number", name, s)
		}
		return n
	}
	runs := uint64(10_000)
	if testing.Short() {
		runs = 1000
	}
	seed, runs := env(forgetEnvSeed, uint64(time.Now().UnixNano())), env(forgetEnvRuns, runs)
	if runs == 0 {
		t.Fatalf("%s=0: want a case to check", forgetEnvRuns)
	}

	zones := forgetZones(t)
	for nr := range runs {
		rng := rand.New(rand.NewPCG(seed, nr))
		c := drawForgetCase(rng, zones)
		err := c.policy.validate()
		if err == nil {
			err = c.check(rng)
		}
		if err != nil {
			t.Fatalf("%s=%d, case %d: %v\n%s", forgetEnvSeed, seed, nr, err, c)
		}
	}
}

// TestForgetTwiceUnderAFutureSnapshot pins the one case in which a second
// forget removes what the first kept. A keep-within window is measured from
// the newest snapshot that is not in the future. A period rule passes that
// snapshot over when one dated in the future is the newest of the same
// period, so the second run measures from an older snapshot, and a calendar
// window does not move back evenly with its end: a month before 1 April
// starts on 1 March, a month before 31 March on 3 March. restic measures
// and steps the same way.
func TestForgetTwiceUnderAFutureSnapshot(t *testing.T) {
	at := func(m time.Month, d, h int) time.Time { return time.Date(2021, m, d, h, 0, 0, 0, time.UTC) }
	c := forgetCase{
		loc: time.UTC,
		now: at(4, 1, 12),
		snaps: []*Snapshot{
			{ID: "future", CreatedAt: at(4, 1, 13)},
			{ID: "apr1", CreatedAt: at(4, 1, 10)},
			{ID: "mar31", CreatedAt: at(3, 31, 10)},
			{ID: "mar2", CreatedAt: at(3, 2, 10)},
		},
		policy: ForgetPolicy{KeepWithinMonthly: Within{Months: 1}},
	}
	first := c.apply(c.snaps, c.policy)
	if got, want := ids(first.Keep), []string{"future", "mar31", "mar2"}; !slices.Equal(got, want) {
		t.Fatalf("first run keeps %v, want %v", got, want)
	}
	second := c.apply(first.Keep, c.policy)
	if got, want := ids(second.Remove), []string{"mar2"}; !slices.Equal(got, want) {
		t.Errorf("second run removes %v, want %v", got, want)
	}
	if !c.windowStartsLater(first.Keep) {
		t.Error("the window of the second run does not start later, so the property test would not pass this case over")
	}
	if err := c.check(rand.New(rand.NewPCG(1, 1))); err != nil {
		t.Errorf("the case breaks another invariant: %v", err)
	}
}
