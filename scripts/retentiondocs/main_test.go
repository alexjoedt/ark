package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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
		where := staleScenarios(string(committed), outcomes)
		t.Errorf("%s differs from what scripts/retentiondocs generates (%s); "+
			"edit scripts/retentiondocs/retention.md.tmpl or the scenarios, never the file, and run task docs:gen",
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
