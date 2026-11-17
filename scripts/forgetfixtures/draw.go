package main

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// header is the first line of the fixture file.
type header struct {
	Restic string `json:"restic"`
	Seed   uint64 `json:"seed"`
	Cases  int    `json:"cases"`
}

// fixtureCase is one further line: a history, a policy and what restic made
// of them. The root package's replay test reads the same fields.
type fixtureCase struct {
	// TZ is the IANA zone restic ran in, for the backups and the forget.
	TZ string `json:"tz"`
	// Policy maps a restic keep flag without its dashes to its value.
	Policy map[string]string `json:"policy,omitempty"`
	// KeepTag holds one tag list per --keep-tag flag.
	KeepTag [][]string `json:"keep_tag,omitempty"`
	// Times are the snapshots as Unix seconds, newest first, no two equal.
	Times []int64 `json:"times"`
	// Tags holds the comma-separated tags of each snapshot, absent when the
	// history has none.
	Tags []string `json:"tags,omitempty"`
	// Keep indexes the snapshots restic keeps, Reasons holds its reasons
	// for each of them.
	Keep    []int      `json:"keep,omitempty"`
	Reasons [][]string `json:"reasons,omitempty"`
	// Refused marks a policy restic refuses to run because it would keep
	// nothing of the history.
	Refused bool `json:"refused,omitempty"`
}

// timeLayout is the form restic's backup --time takes: a wall clock without
// a zone, read in the local zone.
const timeLayout = "2006-01-02 15:04:05"

const (
	day  = 24 * 3600
	year = 365 * day
)

// zones are drawn evenly: no summer time, summer time on both hemispheres,
// a change at midnight, a half-hour change and offsets off the full hour.
var zones = []string{
	"UTC",
	"Europe/Berlin",
	"America/New_York",
	"America/Sao_Paulo",
	"America/St_Johns",
	"Asia/Kolkata",
	"Asia/Kathmandu",
	"Australia/Lord_Howe",
	"Pacific/Auckland",
}

var (
	countedFlags = []string{"keep-last", "keep-hourly", "keep-daily", "keep-weekly", "keep-monthly", "keep-yearly"}
	withinFlags  = []string{
		"keep-within", "keep-within-hourly", "keep-within-daily",
		"keep-within-weekly", "keep-within-monthly", "keep-within-yearly",
	}
	tagLists = [][]string{{"a"}, {"b"}, {"c"}, {"a", "b"}, {"b", "c"}, {""}}
)

// schedules are the steps of a history that is taken on a schedule.
var schedules = []struct{ months, days, hours int }{
	{0, 0, 1}, {0, 0, 6}, {0, 1, 0}, {0, 7, 0}, {1, 0, 0}, {12, 0, 0},
}

func unix(y int, m time.Month, d int) int64 { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix() }

// drawCase draws the history and the policy of case nr from the zone files
// in zoneinfo. A shorter run draws the first cases of a longer one. The
// draw also depends on how math/rand/v2 derives IntN, Int64N and Perm from
// PCG, which Go does not promise to keep across releases.
func drawCase(seed uint64, nr int, zoneinfo string) (fixtureCase, error) {
	rng := rand.New(rand.NewPCG(seed, uint64(nr)))
	c := fixtureCase{TZ: zones[rng.IntN(len(zones))]}
	loc, err := loadZone(zoneinfo, c.TZ)
	if err != nil {
		return c, err
	}
	c.Times = drawTimes(rng, loc)
	c.Tags = drawTags(rng, len(c.Times))
	c.Policy, c.KeepTag = drawPolicy(rng, c.Times, loc)
	return c, nil
}

// loadZone reads zone name from dir, which holds the zone files the fixtures
// were decided with. [time.LoadLocation] would read the host's zone database,
// whose rules for the same zone may differ.
func loadZone(dir, name string) (*time.Location, error) {
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return nil, err
	}
	return time.LoadLocationFromTZData(name, data)
}

// logUniform draws from [lo, hi), each doubling of lo as likely as the next.
// It uses integers only, so every platform draws the same numbers.
func logUniform(rng *rand.Rand, lo, hi int64) int64 {
	doublings := 0
	for v := lo; v < hi; v *= 2 {
		doublings++
	}
	v := lo << rng.IntN(doublings)
	return v + rng.Int64N(min(v, hi-v))
}

// drawTimes draws 1 to 200 snapshot times, newest first, with gaps from a
// minute to two years. All lie in the past, where restic has no special
// rule, and not before 2000, where the zone rules are settled.
func drawTimes(rng *rand.Rand, loc *time.Location) []int64 {
	n := 1
	if rng.IntN(100) >= 4 {
		n = int(logUniform(rng, 2, 201))
	}

	// The history ends, a third of the time each, at the turn of 2015 to
	// 2016 (2015 has an ISO week 53), just after the zone changes its
	// offset, and anywhere. A zone without a change around the drawn
	// instant (UTC, Kolkata, Kathmandu, Sao Paulo after 2019) ends anywhere
	// instead, so about a fifth of the histories end after a change. A
	// change from 2025 on is none: some zone files end in a placeholder for
	// 2038, which would put the history into the future.
	//
	// One history of five is a schedule, the same wall clock every hour,
	// day, week, month or year counted from the newest snapshot, so that a
	// keep-within window ends exactly on a snapshot. AddDate rolls a month
	// back from the 31st over into the next month, as restic's window does.
	// After a change of the offset it is every second, for backups that meet
	// the hour the clocks skip and windows that reach across the change.
	end := unix(2012, 1, 1) + rng.Int64N(13*year)
	scheduled := rng.IntN(5) == 0
	switch p := rng.IntN(100); {
	case p < 35:
		end = unix(2015, 12, 28) + rng.Int64N(24*day)
	case p < 65:
		some := time.Unix(unix(2012, 1, 1)+rng.Int64N(10*year), 0).In(loc)
		if _, shift := some.ZoneBounds(); !shift.IsZero() && shift.Unix() < unix(2025, 1, 1) {
			end = shift.Unix() + rng.Int64N(3*day)
			scheduled = scheduled || rng.IntN(2) == 0
		}
	}

	maxGap := logUniform(rng, 3600, 2*year)
	at := time.Unix(end, 0).In(loc)
	back := func(int) time.Time {
		t := at
		at = at.Add(-time.Duration(logUniform(rng, 60, maxGap+1)) * time.Second)
		return t
	}
	if scheduled {
		s := schedules[rng.IntN(len(schedules))]
		back = func(k int) time.Time {
			return at.AddDate(0, -k*s.months, -k*s.days).Add(-time.Duration(k*s.hours) * time.Hour)
		}
	}

	var times []int64
	for k := 0; len(times) < n; k++ {
		// restic gets the time as a wall clock, so of the two instants an
		// hour repeated at the end of summer time has, only one exists.
		wall := back(k).Format(timeLayout)
		t, err := time.ParseInLocation(timeLayout, wall, loc)
		if err != nil {
			panic(err) // the layout wrote it
		}
		if t.Unix() < unix(2000, 1, 1) {
			break
		}
		if !slices.Contains(times, t.Unix()) {
			times = append(times, t.Unix())
		}
	}
	slices.Sort(times)
	slices.Reverse(times)
	return times
}

// drawTags gives half of the histories tags from a, b and c, the later ones
// rarer.
func drawTags(rng *rand.Rand, n int) []string {
	p := []int{0, 0, 25, 60}[rng.IntN(4)]
	if p == 0 {
		return nil
	}
	tags := make([]string, n)
	for i := range tags {
		var list []string
		for j, tag := range []string{"a", "b", "c"} {
			if rng.IntN(100) < p>>j {
				list = append(list, tag)
			}
		}
		tags[i] = strings.Join(list, ",")
	}
	return tags
}

// drawPolicy draws one to three rules most of the time, sometimes more and
// sometimes every rule at once.
func drawPolicy(rng *rand.Rand, times []int64, loc *time.Location) (map[string]string, [][]string) {
	rules := len(countedFlags) + len(withinFlags) + 1
	n := rules
	switch p := rng.IntN(100); {
	case p < 30:
		n = 1
	case p < 60:
		n = 2
	case p < 80:
		n = 3
	case p < 94:
		n = 4 + rng.IntN(3)
	}
	policy := make(map[string]string)
	var keepTag [][]string
	for _, r := range rng.Perm(rules)[:n] {
		switch {
		case r < len(countedFlags):
			policy[countedFlags[r]] = "unlimited"
			if rng.IntN(100) >= 12 {
				policy[countedFlags[r]] = strconv.FormatInt(logUniform(rng, 1, 100), 10)
			}
		case r < len(countedFlags)+len(withinFlags):
			policy[withinFlags[r-len(countedFlags)]] = drawWithin(rng, times, loc)
		default:
			for _, i := range rng.Perm(len(tagLists))[:1+rng.IntN(2)] {
				keepTag = append(keepTag, tagLists[i])
			}
		}
	}
	return policy, keepTag
}

// drawWithin draws a keep-within window in restic's text form. Half of the
// windows are fitted to the history: the hours, days or months from one of
// its snapshots to the newest, so that the window ends at that snapshot. The
// others are random, half of them of one unit. Months stay below twelve, the
// most ark reads.
func drawWithin(rng *rand.Rand, times []int64, loc *time.Location) string {
	if len(times) > 1 && rng.IntN(2) == 0 {
		newest := time.Unix(times[0], 0).In(loc)
		t := time.Unix(times[1+rng.IntN(len(times)-1)], 0).In(loc)
		date := func(t time.Time) int64 { return unix(t.Year(), t.Month(), t.Day()) / day }
		months := (newest.Year()-t.Year())*12 + int(newest.Month()-t.Month())
		calendar := ""
		if months >= 12 {
			calendar = fmt.Sprintf("%dy", months/12)
		}
		if months%12 > 0 {
			calendar += fmt.Sprintf("%dm", months%12)
		}
		fitted := []string{
			fmt.Sprintf("%dh", int(newest.Sub(t).Hours())),
			fmt.Sprintf("%dd", date(newest)-date(t)),
			calendar,
		}[rng.IntN(3)]
		if fitted != "" && fitted[0] != '0' {
			return fitted
		}
	}

	units := []struct {
		name string
		end  int64
	}{{"y", 13}, {"m", 12}, {"d", 1000}, {"h", 2000}}
	for {
		var b strings.Builder
		single, one := rng.IntN(2) == 0, rng.IntN(len(units))
		for i, u := range units {
			if single && i == one || !single && rng.IntN(100) < 40 {
				fmt.Fprintf(&b, "%d%s", logUniform(rng, 1, u.end), u.name)
			}
		}
		if b.Len() > 0 {
			return b.String()
		}
	}
}
