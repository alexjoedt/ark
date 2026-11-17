package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexjoedt/ark"
)

const root = "../.."

// generated runs the generator once for every test that reads its output:
// each run backs every scenario up.
var generated = sync.OnceValue(func() (g struct {
	files    []file
	outcomes map[string]*outcome
	err      error
},
) {
	g.files, g.outcomes, g.err = generate(context.Background(), scenarios())
	return g
})

func generatedFiles(t *testing.T) ([]file, map[string]*outcome) {
	t.Helper()
	g := generated()
	if g.err != nil {
		t.Fatal(g.err)
	}
	return g.files, g.outcomes
}

func TestGeneratedFilesAreCurrent(t *testing.T) {
	files, outcomes := generatedFiles(t)
	for _, f := range files {
		committed, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f.path)))
		if err != nil {
			t.Errorf("%s: %v; run task docs:gen", f.path, err)
			continue
		}
		if bytes.Equal(committed, f.content) {
			continue
		}
		where := "the drawing"
		if f.path == pagePath {
			where = staleScenarios(string(committed), outcomes)
		}
		t.Errorf("%s differs from what scripts/retentiondocs generates (%s); "+
			"edit scripts/retentiondocs (template, scenarios or drawing), never the file, and run task docs:gen",
			f.path, where)
	}
}

// staleScenarios names the scenarios whose table the committed page lacks.
func staleScenarios(page string, outcomes map[string]*outcome) string {
	var stale []string
	for name, o := range outcomes {
		if !strings.Contains(page, o.Table) {
			stale = append(stale, "scenario "+name)
		}
	}
	slices.Sort(stale)
	if len(stale) == 0 {
		return "the template text"
	}
	return strings.Join(stale, ", ")
}

// TestEveryKeepFlagIsShown reads the keep flags from the definitions of
// ark forget's flags and looks for each in a scenario the page shows.
func TestEveryKeepFlagIsShown(t *testing.T) {
	files, outcomes := generatedFiles(t)
	page := string(files[0].content)
	shown := map[string]bool{}
	for _, s := range scenarios() {
		if !strings.Contains(page, "]("+outcomes[s.name].Image+")") {
			continue
		}
		for _, r := range rules(s.policy) {
			flag, _, _ := strings.Cut(r.flag, " ")
			shown[flag] = true
		}
	}
	flags := forgetKeepFlags(t)
	if len(flags) == 0 {
		t.Fatal("found no keep flag in cmd/ark/forget.go")
	}
	for _, f := range flags {
		if !shown["--"+f] {
			t.Errorf("--%s of ark forget appears in no scenario of %s", f, pagePath)
		}
	}
}

// forgetKeepFlags returns the names of the flags cmd/ark/forget.go defines
// that start with keep-.
func forgetKeepFlags(t *testing.T) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, "cmd", "ark", "forget.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var flags []string
	ast.Inspect(f, func(n ast.Node) bool {
		kv, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		key, ok := kv.Key.(*ast.Ident)
		lit, isLit := kv.Value.(*ast.BasicLit)
		if !ok || key.Name != "Name" || !isLit || lit.Kind != token.STRING {
			return true
		}
		if name, err := strconv.Unquote(lit.Value); err == nil && strings.HasPrefix(name, "keep-") {
			flags = append(flags, name)
		}
		return true
	})
	return flags
}

func TestTimelinesAreWellFormed(t *testing.T) {
	files, _ := generatedFiles(t)
	again, _, err := generate(context.Background(), scenarios())
	if err != nil {
		t.Fatal(err)
	}
	for i, f := range files {
		if !bytes.Equal(f.content, again[i].content) {
			t.Errorf("%s differs between two runs", f.path)
		}
		if path.Ext(f.path) == ".svg" {
			checkXML(t, f.path, f.content)
		}
	}
}

// TestTimelineLanes draws a policy of several rules, which no scenario of
// the page may show yet, as one lane per rule.
func TestTimelineLanes(t *testing.T) {
	history := running()
	history[3].tags = []string{"monthly"}
	s := scenario{name: "lanes", history: history, policy: ark.ForgetPolicy{
		KeepDaily: 3, KeepWeekly: 2, KeepWithin: ark.Within{Days: 5}, KeepTags: [][]string{{"monthly"}},
	}}
	rows, err := forget(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	img, err := timeline(s.policy, rows)
	if err != nil {
		t.Fatal(err)
	}
	checkXML(t, s.name, img)
	for _, want := range []string{">--keep-tag<", ">--keep-within<", ">--keep-daily<", ">--keep-weekly<", ">kept<", ">3/3<", ">2/2<"} {
		if !bytes.Contains(img, []byte(want)) {
			t.Errorf("the timeline lacks %s", want)
		}
	}
}

func TestTimelineRefusesAnUnknownReason(t *testing.T) {
	rows := []row{{at: running()[0].at, kept: true, reasons: []string{"weekly snapshot"}}}
	if _, err := timeline(ark.ForgetPolicy{KeepDaily: 1}, rows); err == nil {
		t.Error("a reason of no rule of the policy drew a timeline")
	}
}

// A snapshot in the future would not move forget's window, while the
// timeline measures the window from the newest snapshot: the drawing holds
// only as long as no history can have one.
func TestForgetRefusesASnapshotInTheFuture(t *testing.T) {
	s := scenario{
		name:    "future",
		history: []snap{{at: time.Now().Add(time.Hour)}},
		policy:  ark.ForgetPolicy{KeepLast: 1},
	}
	if _, err := forget(context.Background(), s); !errors.Is(err, ark.ErrInvalidTime) {
		t.Errorf("a history with a snapshot in the future: %v, want ErrInvalidTime", err)
	}
}

func TestForgetReturnsRowsInTheZoneItCutsIn(t *testing.T) {
	east := time.FixedZone("east", 5*60*60)
	s := scenario{
		name:    "zone",
		history: []snap{{at: time.Date(2015, 3, 3, 2, 0, 0, 0, east)}},
		policy:  ark.ForgetPolicy{KeepDaily: 1},
	}
	rows, err := forget(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := day.start(rows[0].at), time.Date(2015, 3, 2, 0, 0, 0, 0, zone); !got.Equal(want) ||
		rows[0].at.Location() != zone {
		t.Errorf("the day of %s starts at %s, want %s", rows[0].at, got, want)
	}
}

func TestWeekLabelNamesTheISOYear(t *testing.T) {
	lines, coarse := week.labels(time.Date(2025, 12, 29, 0, 0, 0, 0, time.UTC))
	if lines[0] != "W1" || coarse != "2026" {
		t.Errorf("the week of Mon 29 Dec 2025 is labelled %s of %s, want W1 of 2026", lines[0], coarse)
	}
}

// daily returns n kept snapshots, one a day, newest first.
func daily(newest time.Time, n int, reason string) []row {
	rows := make([]row, n)
	for i := range rows {
		rows[i] = row{at: newest.AddDate(0, 0, -i), kept: true, reasons: []string{reason}}
	}
	return rows
}

// April starts two bands after the label of March, which still covers it.
func TestBandLabelsNameAMonthThatStartsUnderTheLabelBefore(t *testing.T) {
	rows := daily(time.Date(2015, 5, 8, 12, 0, 0, 0, time.UTC), 40, "")
	var s svg
	s.bandLabels(newAxis(rows, day, 16, width-16), 0)
	for _, want := range []string{">Mar 2015<", ">Apr 2015<", ">May 2015<"} {
		if n := strings.Count(s.b.String(), want); n != 1 {
			t.Errorf("the band labels hold %s %d times, want once", want, n)
		}
	}
}

// An hourly rule over daily backups counts across 23 hours without a
// snapshot a day, on an axis of weeks.
func TestTimelineHatchesAGapOfFinerPeriodsOnce(t *testing.T) {
	rows := daily(time.Date(2015, 5, 8, 12, 0, 0, 0, time.UTC), 90, "hourly snapshot")
	img, err := timeline(ark.ForgetPolicy{KeepHourly: ark.Unlimited}, rows)
	if err != nil {
		t.Fatal(err)
	}
	checkXML(t, "hourly", img)
	// One per gap between two snapshots, and the legend's.
	if n := bytes.Count(img, []byte(`class="hatch"`)); n != len(rows) {
		t.Errorf("the timeline hatches %d rectangles, want %d", n, len(rows))
	}
}

func TestTimelineDrawsTheWindowOfARuleThatKeepsNothing(t *testing.T) {
	rows := []row{{at: time.Date(2015, 3, 22, 18, 0, 0, 0, time.UTC)}}
	img, err := timeline(ark.ForgetPolicy{KeepWithin: ark.Within{Days: 5}}, rows)
	if err != nil {
		t.Fatal(err)
	}
	if want := "5d window from Tue 17 Mar 18:00"; !bytes.Contains(img, []byte(want)) {
		t.Errorf("the timeline lacks %q", want)
	}
}

func TestNoStaleTimelines(t *testing.T) {
	files, _ := generatedFiles(t)
	generated := map[string]bool{}
	for _, f := range files {
		generated[f.path] = true
	}
	entries, err := os.ReadDir(filepath.Join(root, imageDir))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if p := path.Join(imageDir, e.Name()); !generated[p] {
			t.Errorf("%s is no scenario's timeline; delete it", p)
		}
	}
}

func checkXML(t *testing.T, name string, content []byte) {
	t.Helper()
	d := xml.NewDecoder(bytes.NewReader(content))
	for {
		_, err := d.Token()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			t.Errorf("%s is no well-formed XML: %v", name, err)
			return
		}
	}
}
