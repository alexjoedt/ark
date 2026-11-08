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
	citedTest   = regexp.MustCompile("`(Test[A-Za-z0-9_]+)`")
	endsInTest  = regexp.MustCompile("`Test[A-Za-z0-9_]+`\\.?$")
	definedTest = regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]+)\(`)
)

// TestSecurityDocCitesDefinedTests keeps docs/SECURITY.md and the tests in
// step: every cited test exists somewhere in the go.work tree, every guarantee
// and accepted risk cites one, and the CI step it names exists.
func TestSecurityDocCitesDefinedTests(t *testing.T) {
	doc, err := os.ReadFile("docs/SECURITY.md")
	if err != nil {
		t.Fatal(err)
	}

	defined := map[string]bool{}
	err = filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path != "." && (strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range definedTest.FindAllSubmatch(src, -1) {
			defined[string(m[1])] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	cited := citedTest.FindAllSubmatch(doc, -1)
	if len(cited) == 0 {
		t.Fatal("docs/SECURITY.md cites no test")
	}
	for _, m := range cited {
		if !defined[string(m[1])] {
			t.Errorf("docs/SECURITY.md cites %s, which no _test.go in the tree defines", m[1])
		}
	}

	for _, section := range []string{"Guarantees at rest", "Guarantees in transit", "Accepted risks"} {
		items := bullets(string(doc), section)
		if len(items) == 0 {
			t.Errorf("docs/SECURITY.md: section %q is missing or has no bullets", section)
		}
		for _, b := range items {
			if !endsInTest.MatchString(b) {
				t.Errorf("docs/SECURITY.md, %s: bullet does not end with a cited test: %q", section, b)
			}
		}
	}

	workflow, err := os.ReadFile(".github/workflows/go.yml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc), "`Vulnerability check`") ||
		!strings.Contains(string(workflow), "name: Vulnerability check\n") {
		t.Error("docs/SECURITY.md and go.yml must both name the step Vulnerability check")
	}
}

// bullets returns the list items of the "## " section, each joined with its
// indented continuation lines.
func bullets(doc, section string) []string {
	var out []string
	in := false
	for line := range strings.SplitSeq(doc, "\n") {
		if title, ok := strings.CutPrefix(line, "## "); ok {
			in = title == section
			continue
		}
		switch {
		case !in:
		case strings.HasPrefix(line, "- "):
			out = append(out, strings.TrimSpace(line))
		case strings.HasPrefix(line, "  ") && len(out) > 0:
			out[len(out)-1] += " " + strings.TrimSpace(line)
		}
	}
	return out
}
