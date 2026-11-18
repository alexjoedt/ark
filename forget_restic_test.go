package ark

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// resticFixtures holds snapshot histories and keep policies together with
// what restic keeps of them and why, written by scripts/forgetfixtures.
const resticFixtures = "testdata/forget/restic.jsonl"

// resticZoneinfo holds the zone files restic decided the fixtures with. The
// host's zone database may hold other rules for the same zone.
const resticZoneinfo = "testdata/forget/zoneinfo"

// resticZone loads the zone of that name from resticZoneinfo.
func resticZone(name string) (*time.Location, error) {
	data, err := os.ReadFile(filepath.Join(resticZoneinfo, name))
	if err != nil {
		return nil, err
	}
	return time.LoadLocationFromTZData(name, data)
}

// resticDeviations names every fixture case, by its line in the file, where
// ark is meant to decide differently from restic, with the reason. A case
// that differs without an entry fails the test, and so does an entry whose
// case agrees.
//
// There is none. The places where ark departs from restic by decision are
// outside what the fixtures hold: restic cuts periods in the zone a snapshot
// was taken in and ark in the zone forget runs in, which is one zone per
// fixture history; restic leaves snapshots of the same instant in the order
// it loaded them, so no two fixture snapshots share one; restic reads a
// keep-within of more than 11 months, which ark refuses, so the fixtures
// stay below.
var resticDeviations = map[int]string{}

// resticCase is one line of the fixture file after the header; the fields
// are documented on fixtureCase in scripts/forgetfixtures.
type resticCase struct {
	TZ      string            `json:"tz"`
	Policy  map[string]string `json:"policy"`
	KeepTag [][]string        `json:"keep_tag"`
	Times   []int64           `json:"times"`
	Tags    []string          `json:"tags"`
	Keep    []int             `json:"keep"`
	Reasons [][]string        `json:"reasons"`
	Refused bool              `json:"refused"`
}

// forgetPolicy reads the restic flags of the case through the parsers the
// command line uses.
func (c resticCase) forgetPolicy() (ForgetPolicy, error) {
	p := ForgetPolicy{KeepTags: c.KeepTag}
	counts := map[string]*KeepCount{
		"keep-last": &p.KeepLast, "keep-hourly": &p.KeepHourly, "keep-daily": &p.KeepDaily,
		"keep-weekly": &p.KeepWeekly, "keep-monthly": &p.KeepMonthly, "keep-yearly": &p.KeepYearly,
	}
	windows := map[string]*Within{
		"keep-within": &p.KeepWithin, "keep-within-hourly": &p.KeepWithinHourly,
		"keep-within-daily": &p.KeepWithinDaily, "keep-within-weekly": &p.KeepWithinWeekly,
		"keep-within-monthly": &p.KeepWithinMonthly, "keep-within-yearly": &p.KeepWithinYearly,
	}
	for flag, value := range c.Policy {
		var err error
		switch {
		case counts[flag] != nil:
			*counts[flag], err = ParseKeepCount(value)
		case windows[flag] != nil:
			*windows[flag], err = ParseWithin(value)
		default:
			err = errors.New("no such rule")
		}
		if err != nil {
			return p, fmt.Errorf("--%s %s: %w", flag, value, err)
		}
	}
	return p, p.validate()
}

// decide runs the policy over the history and returns the answer in the
// form the fixture holds restic's.
func (c resticCase) decide() (keep []int, reasons [][]string, refused bool, err error) {
	policy, err := c.forgetPolicy()
	if err != nil {
		return nil, nil, false, err
	}
	loc, err := resticZone(c.TZ)
	if err != nil {
		return nil, nil, false, err
	}
	snapshots := make([]*Snapshot, len(c.Times))
	index := make(map[SnapshotID]int)
	for i, at := range c.Times {
		s := &Snapshot{ID: SnapshotID(strconv.Itoa(i)), SourceName: "s", CreatedAt: time.Unix(at, 0).UTC()}
		if c.Tags != nil && c.Tags[i] != "" {
			s.Tags = strings.Split(c.Tags[i], ",")
		}
		snapshots[i], index[s.ID] = s, i
	}
	res := applyForgetPolicy(snapshots, policy, loc, afterAll)
	if n := len(res.Keep) + len(res.Remove); n != len(snapshots) {
		return nil, nil, false, fmt.Errorf("%d of %d snapshots kept or removed", n, len(snapshots))
	}
	// restic refuses a policy that keeps nothing of a history and gives no
	// answer; Forget does the same after this check.
	if errors.Is(checkSeriesKept(snapshots, res), ErrNoSnapshotKept) {
		return nil, nil, true, nil
	}
	for _, s := range res.Keep {
		keep = append(keep, index[s.ID])
		reasons = append(reasons, res.KeepReasons[s.ID])
	}
	return keep, reasons, false, nil
}

// TestForgetMatchesRestic replays every fixture case: ark keeps the
// snapshots restic keeps, for restic's reasons, and refuses what restic
// refuses.
func TestForgetMatchesRestic(t *testing.T) {
	f, err := os.Open(resticFixtures)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	lines := bufio.NewScanner(f)
	lines.Buffer(nil, 1<<20)

	var head struct {
		Restic string `json:"restic"`
		Cases  int    `json:"cases"`
	}
	if !lines.Scan() || json.Unmarshal(lines.Bytes(), &head) != nil || head.Restic == "" {
		t.Fatalf("%s: no header line naming the restic version", resticFixtures)
	}

	cases, rules, refusals, oldest := 0, make(map[string]bool), 0, 0
	for line := 2; lines.Scan(); line++ {
		var c resticCase
		if err := json.Unmarshal(lines.Bytes(), &c); err != nil {
			t.Fatalf("line %d: %v", line, err)
		}
		cases++
		for flag := range c.Policy {
			rules[flag] = true
		}
		if len(c.KeepTag) > 0 {
			rules["keep-tag"] = true
		}
		if c.Refused {
			refusals++
		}
		if strings.Contains(fmt.Sprint(c.Reasons), "oldest ") {
			oldest++
		}

		keep, reasons, refused, err := c.decide()
		if err != nil {
			t.Fatalf("line %d: %v", line, err)
		}
		same := refused == c.Refused && reflect.DeepEqual(keep, c.Keep) && reflect.DeepEqual(reasons, c.Reasons)
		why, deviates := resticDeviations[line]
		switch {
		case same && deviates:
			t.Errorf("line %d agrees with restic, drop its entry in resticDeviations (%s)", line, why)
		case !same && !deviates:
			t.Errorf("line %d: zone %s, policy %v, keep-tag %q, %d snapshots\n"+
				"   ark: refused %t, keep %v\n        reasons %q\nrestic: refused %t, keep %v\n        reasons %q",
				line, c.TZ, c.Policy, c.KeepTag, len(c.Times),
				refused, keep, reasons, c.Refused, c.Keep, c.Reasons)
		}
	}
	if err := lines.Err(); err != nil {
		t.Fatal(err)
	}

	// The fixtures are worth what they cover.
	if cases != head.Cases || cases < 500 {
		t.Errorf("%d cases, the header says %d, want at least 500", cases, head.Cases)
	}
	if len(rules) != 13 {
		t.Errorf("the cases use %d of the 13 rules: %v", len(rules), rules)
	}
	if refusals == 0 || oldest == 0 {
		t.Errorf("%d cases restic refuses and %d with a kept oldest snapshot, want some of each", refusals, oldest)
	}
}
