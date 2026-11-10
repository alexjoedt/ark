package ark_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	adrFile    = regexp.MustCompile(`^(\d{4})-[a-z0-9-]+\.md$`)
	adrTitle   = regexp.MustCompile(`(?m)\A# (\d{4}): (.+)$`)
	adrStatus  = regexp.MustCompile(`(?m)^Status: (accepted|deprecated|superseded by (\d{4}))$`)
	adrFileRef = regexp.MustCompile("`([A-Za-z0-9_./-]+\\.(?:go|sh|md|yml|yaml|mod))`")
	adrDate    = regexp.MustCompile(`(?m)^Date: (\d{4}-\d{2}-\d{2})$`)
	adrRow     = regexp.MustCompile(`(?m)^\| (\d{4}) \| \[(.+)\]\(([^)]+)\) \| (.+) \| (\d{4}-\d{2}-\d{2}) \|$`)
)

// TestADRIndexMatchesFiles keeps docs/adr in step: every NNNN-*.md is indexed
// in README.md with the title, status and date the file carries, every index
// row points at a file, numbers are unique, each ADR has the three MADR
// sections, never mentions the agent memory directory, cites only tests a
// go.work module defines and only files that exist, and points every
// "superseded by" status at an existing ADR.
func TestADRIndexMatchesFiles(t *testing.T) {
	modules := workModules(t)
	readme, err := os.ReadFile("docs/adr/README.md")
	if err != nil {
		t.Fatal(err)
	}
	type row struct{ number, title, status, date string }
	rows := map[string]row{}
	for _, line := range strings.Split(string(readme), "\n") {
		if !strings.HasPrefix(line, "| ") || strings.HasPrefix(line, "| Number ") {
			continue
		}
		m := adrRow.FindStringSubmatch(line)
		if m == nil {
			t.Errorf("docs/adr/README.md: malformed index row %q", line)
			continue
		}
		if _, dup := rows[m[3]]; dup {
			t.Errorf("docs/adr/README.md indexes %s twice", m[3])
		}
		rows[m[3]] = row{m[1], m[2], m[4], m[5]}
	}

	all, err := filepath.Glob("docs/adr/*.md")
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, path := range all {
		if base := filepath.Base(path); base != "README.md" && base != "template.md" {
			files = append(files, path)
		}
	}
	if len(files) == 0 {
		t.Fatal("docs/adr has no ADR file")
	}

	defined := definedTests(t, modules)
	exists := repoFiles(t)

	numbers := map[string]string{}
	superseders := map[string]string{}
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
		head, _, _ := strings.Cut(doc, "\n## ")
		title := adrTitle.FindStringSubmatch(head)
		status := adrStatus.FindStringSubmatch(head)
		date := adrDate.FindStringSubmatch(head)
		if title == nil || title[1] != m[1] {
			t.Errorf("%s: first line must be \"# %s: <title>\"", name, m[1])
		}
		if status == nil {
			t.Errorf("%s: missing a valid \"Status:\" line under the title", name)
		} else if status[2] != "" {
			superseders[name] = status[2]
		}
		if date == nil {
			t.Errorf("%s: missing a \"Date: YYYY-MM-DD\" line under the title", name)
		}
		for _, section := range []string{"Context", "Decision", "Consequences"} {
			if !strings.Contains("\n"+doc, "\n## "+section+"\n") {
				t.Errorf("%s: missing section \"## %s\"", name, section)
			}
		}
		if strings.Contains(doc, ".agent/") {
			t.Errorf("%s: mentions .agent/, which never reaches the repository", name)
		}
		for _, c := range citedTest.FindAllStringSubmatch(doc, -1) {
			if !defined[c[1]] {
				t.Errorf("%s: cites %s, which no _test.go of a go.work module defines", name, c[1])
			}
		}
		for _, c := range adrFileRef.FindAllStringSubmatch(doc, -1) {
			if !exists[c[1]] {
				t.Errorf("%s: cites file %s, which does not exist", name, c[1])
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
	for name, n := range superseders {
		if _, ok := numbers[n]; !ok {
			t.Errorf("%s: superseded by %s, which does not exist", name, n)
		}
	}
	for name := range rows {
		t.Errorf("docs/adr/README.md indexes %s, which does not exist", name)
	}
}

// repoFiles returns every file of the checkout by its slash path and, for a
// bare name, by that name too, so an ADR may cite config.go without its
// directory. Dot directories and vendor are left out.
func repoFiles(t *testing.T) map[string]bool {
	t.Helper()
	exists := map[string]bool{}
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); path != "." && (strings.HasPrefix(name, ".") || name == "vendor" || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		exists[filepath.ToSlash(path)] = true
		exists[d.Name()] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return exists
}
