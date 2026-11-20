package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"
)

// docFlagToken matches a --flag token that is not part of a longer dash run (---, a--b),
// and the en or em dash a typographic paste makes of the two hyphens.
var docFlagToken = regexp.MustCompile(`(?:^|[^\w-])(--|[–—])([A-Za-z](?:[\w-]*\w)?)`)

// docFlagAllowed lists flags in the docs that belong to another tool or are
// documented as absent. An entry covers only the lines of its file that contain
// its line text, and every entry must still match a line.
var docFlagAllowed = []struct{ file, flag, line, reason string }{
	{"docs/cli/index.md", "password", "There is no `--password` flag", "documented as absent"},
	{"docs/releasing.md", "push", "a real run needs `--push`", "flag of scripts/release.sh"},
	{"docs/releasing.md", "push", "scripts/release.sh --push", "flag of scripts/release.sh"},
	{"docs/releasing.md", "skip", "`--skip=validate`", "goreleaser flag"},
	{"docs/cli/profiles.md", "user", "`systemctl --user enable --now", "systemctl flag in the timer example"},
	{"docs/cli/profiles.md", "now", "`systemctl --user enable --now", "systemctl flag in the timer example"},
}

// TestDocumentedFlagsExist fails on every --flag in README.md and docs/
// (adr/ and node_modules skipped) that no visible command of the CLI defines.
func TestDocumentedFlagsExist(t *testing.T) {
	known := map[string]bool{}
	for _, f := range []cli.Flag{cli.HelpFlag, cli.VersionFlag} {
		for _, n := range f.Names() {
			known[n] = true
		}
	}
	var walk func(cmd *cli.Command)
	walk = func(cmd *cli.Command) {
		if cmd.Hidden {
			return
		}
		for _, f := range cmd.Flags {
			if v, ok := f.(interface{ IsVisible() bool }); ok && !v.IsVisible() {
				continue
			}
			for _, n := range f.Names() {
				known[n] = true
			}
		}
		for _, sub := range cmd.Commands {
			walk(sub)
		}
	}
	walk(newApp())

	files := []string{"README.md"}
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(filepath.Join(root, "docs"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "adr" || d.Name() == "node_modules" || strings.HasPrefix(d.Name(), ".")) {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(path, ".md") {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	used := make([]bool, len(docFlagAllowed))
	for _, file := range files {
		b, err := os.ReadFile(filepath.Join(root, file))
		if err != nil {
			t.Fatal(err)
		}
		for i, text := range strings.Split(string(b), "\n") {
			for _, m := range docFlagToken.FindAllStringSubmatch(text, -1) {
				dash, flag := m[1], m[2]
				if dash != "--" {
					t.Errorf("%s:%d: %s%s has a typographic dash, the CLI needs --%s", file, i+1, dash, flag, flag)
					continue
				}
				if known[flag] || allowDocFlag(file, flag, text, used) {
					continue
				}
				t.Errorf("%s:%d: --%s is not a flag of the ark CLI", file, i+1, flag)
			}
		}
	}
	for i, a := range docFlagAllowed {
		if !used[i] {
			t.Errorf("allowlist entry %s --%s (%s) matches no line containing %q; drop it",
				a.file, a.flag, a.reason, a.line)
		}
	}
}

func allowDocFlag(file, flag, text string, used []bool) bool {
	for i, a := range docFlagAllowed {
		if a.file == file && a.flag == flag && strings.Contains(text, a.line) {
			used[i] = true
			return true
		}
	}
	return false
}
