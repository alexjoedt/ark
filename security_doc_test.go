package ark_test

import (
	"go/build"
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
	workUse     = regexp.MustCompile(`(?s)\nuse \((.*?)\)`)
)

// TestSecurityDocCitesDefinedTests keeps docs/SECURITY.md and the tests in
// step: every cited test is defined in a go.work module and builds without
// extra tags, every guarantee and accepted risk cites one, and the CI step and
// the module list of Dependency scanning match go.yml, Taskfile.yml and go.work.
func TestSecurityDocCitesDefinedTests(t *testing.T) {
	modules := workModules(t)
	doc, err := os.ReadFile("docs/SECURITY.md")
	if err != nil {
		t.Fatal(err)
	}

	defined := definedTests(t, modules)
	cited := citedTest.FindAllSubmatch(doc, -1)
	if len(cited) == 0 {
		t.Fatal("docs/SECURITY.md cites no test")
	}
	for _, m := range cited {
		if !defined[string(m[1])] {
			t.Errorf("docs/SECURITY.md cites %s, which no _test.go of a go.work module defines", m[1])
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

	loop := "for dir in " + strings.Join(modules, " ") + ";"
	for _, f := range []string{".github/workflows/go.yml", "Taskfile.yml"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(src), loop) {
			t.Errorf("%s: govulncheck loop is not %q, the go.work modules", f, loop)
		}
	}
	var labels []string
	for _, m := range modules {
		if m == "." {
			m = "root"
		}
		labels = append(labels, m)
	}
	names := strings.Join(labels, ", ")
	if !strings.Contains(string(doc), "("+names+")") {
		t.Errorf("docs/SECURITY.md: Dependency scanning must list the go.work modules as (%s)", names)
	}
}

// workModules returns the module directories of go.work and skips the test
// when the checkout has none.
func workModules(t *testing.T) []string {
	t.Helper()
	work, err := os.ReadFile("go.work")
	if os.IsNotExist(err) {
		t.Skip("needs the go.work checkout, not a module copy of the root")
	}
	if err != nil {
		t.Fatal(err)
	}
	use := workUse.FindSubmatch(work)
	if use == nil {
		t.Fatal("go.work has no use block")
	}
	var modules []string
	for f := range strings.FieldsSeq(string(use[1])) {
		modules = append(modules, filepath.Clean(f))
	}
	return modules
}

// definedTests returns the Test functions of the go.work modules that go test
// compiles without extra tags: testdata, vendor, dot and underscore
// directories, nested modules outside go.work and files excluded by a build
// constraint are left out.
func definedTests(t *testing.T, modules []string) map[string]bool {
	t.Helper()
	defined := map[string]bool{}
	for _, root := range modules {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if path == root {
					return nil
				}
				name := d.Name()
				if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") ||
					name == "testdata" || name == "vendor" || name == "node_modules" {
					return filepath.SkipDir
				}
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, "_test.go") {
				return nil
			}
			ok, err := build.Default.MatchFile(filepath.Dir(path), d.Name())
			if err != nil || !ok {
				return err
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
	}
	return defined
}

// bullets returns the list items of the "## " section, each joined with the
// indented lines that directly follow it. A blank line, a heading or any other
// line ends the item, so a later code block or note is never counted as part
// of it; "### " sub-headings stay inside the section.
func bullets(doc, section string) []string {
	var out []string
	in, open := false, false
	for line := range strings.SplitSeq(doc, "\n") {
		if strings.HasPrefix(line, "#") {
			level := len(line) - len(strings.TrimLeft(line, "#"))
			if level <= 2 {
				in = level == 2 && strings.TrimSpace(line[level:]) == section
			}
			open = false
			continue
		}
		switch {
		case !in:
		case strings.HasPrefix(line, "- "):
			out = append(out, strings.TrimSpace(line))
			open = true
		case open && strings.HasPrefix(line, "  ") && strings.TrimSpace(line) != "":
			out[len(out)-1] += " " + strings.TrimSpace(line)
		default:
			open = false
		}
	}
	return out
}
