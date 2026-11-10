package ark_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	adrFile   = regexp.MustCompile(`^(\d{4})-[a-z0-9-]+\.md$`)
	adrTitle  = regexp.MustCompile(`(?m)\A# (\d{4}): (.+)$`)
	adrStatus = regexp.MustCompile(`(?m)^Status: (accepted|deprecated|superseded by \d{4})$`)
	adrDate   = regexp.MustCompile(`(?m)^Date: (\d{4}-\d{2}-\d{2})$`)
	adrRow    = regexp.MustCompile(`(?m)^\| (\d{4}) \| \[(.+)\]\(([^)]+)\) \| (.+) \| (\d{4}-\d{2}-\d{2}) \|$`)
)

// TestADRIndexMatchesFiles keeps docs/adr in step: every NNNN-*.md is indexed
// in README.md with the title, status and date the file carries, every index
// row points at a file, numbers are unique, each ADR has the three MADR
// sections, never mentions the agent memory directory, and cites only tests a
// go.work module defines.
func TestADRIndexMatchesFiles(t *testing.T) {
	readme, err := os.ReadFile("docs/adr/README.md")
	if err != nil {
		t.Fatal(err)
	}
	type row struct{ number, title, status, date string }
	rows := map[string]row{}
	for _, m := range adrRow.FindAllStringSubmatch(string(readme), -1) {
		if _, dup := rows[m[3]]; dup {
			t.Errorf("docs/adr/README.md indexes %s twice", m[3])
		}
		rows[m[3]] = row{m[1], m[2], m[4], m[5]}
	}

	files, err := filepath.Glob("docs/adr/[0-9][0-9][0-9][0-9]-*.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("docs/adr has no ADR file")
	}

	var defined map[string]bool
	if work, err := os.ReadFile("go.work"); err == nil {
		use := workUse.FindSubmatch(work)
		if use == nil {
			t.Fatal("go.work has no use block")
		}
		var modules []string
		for f := range strings.FieldsSeq(string(use[1])) {
			modules = append(modules, filepath.Clean(f))
		}
		defined = definedTests(t, modules)
	}

	numbers := map[string]string{}
	for _, path := range files {
		name := filepath.Base(path)
		m := adrFile.FindStringSubmatch(name)
		if m == nil {
			t.Errorf("%s: name is not NNNN-<slug>.md", name)
			continue
		}
		if other, dup := numbers[m[1]]; dup {
			t.Errorf("%s and %s share the number %s", other, name, m[1])
		}
		numbers[m[1]] = name

		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		doc := string(src)
		title := adrTitle.FindStringSubmatch(doc)
		status := adrStatus.FindStringSubmatch(doc)
		date := adrDate.FindStringSubmatch(doc)
		if title == nil || title[1] != m[1] {
			t.Errorf("%s: first line must be \"# %s: <title>\"", name, m[1])
		}
		if status == nil {
			t.Errorf("%s: missing a valid \"Status:\" line", name)
		}
		if date == nil {
			t.Errorf("%s: missing a \"Date: YYYY-MM-DD\" line", name)
		}
		for _, section := range []string{"Context", "Decision", "Consequences"} {
			if !strings.Contains("\n"+doc, "\n## "+section+"\n") {
				t.Errorf("%s: missing section \"## %s\"", name, section)
			}
		}
		if strings.Contains(doc, ".agent/") {
			t.Errorf("%s: mentions .agent/, which never reaches the repository", name)
		}
		if defined != nil {
			for _, c := range citedTest.FindAllStringSubmatch(doc, -1) {
				if !defined[c[1]] {
					t.Errorf("%s: cites %s, which no _test.go of a go.work module defines", name, c[1])
				}
			}
		}

		r, ok := rows[name]
		if !ok {
			t.Errorf("%s: missing from the docs/adr/README.md index", name)
			continue
		}
		delete(rows, name)
		if r.number != m[1] {
			t.Errorf("%s: index number %s, file number %s", name, r.number, m[1])
		}
		if title != nil && r.title != title[2] {
			t.Errorf("%s: index title %q, file title %q", name, r.title, title[2])
		}
		if status != nil && r.status != status[1] {
			t.Errorf("%s: index status %q, file status %q", name, r.status, status[1])
		}
		if date != nil && r.date != date[1] {
			t.Errorf("%s: index date %q, file date %q", name, r.date, date[1])
		}
	}
	for name := range rows {
		t.Errorf("docs/adr/README.md indexes %s, which does not exist", name)
	}
}
