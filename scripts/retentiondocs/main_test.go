package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alexjoedt/ark"
)

const root = "../.."

func TestGeneratedFilesAreCurrent(t *testing.T) {
	files, outcomes, err := generate(context.Background(), scenarios())
	if err != nil {
		t.Fatal(err)
	}
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

func TestTimelinesAreWellFormed(t *testing.T) {
	files, _, err := generate(context.Background(), scenarios())
	if err != nil {
		t.Fatal(err)
	}
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

func TestNoStaleTimelines(t *testing.T) {
	files, _, err := generate(context.Background(), scenarios())
	if err != nil {
		t.Fatal(err)
	}
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
