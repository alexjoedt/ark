package main

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"
)

// docFlagToken matches a --flag token that is not part of a longer dash run (---, a--b).
var docFlagToken = regexp.MustCompile(`(?:^|[^\w-])--([a-z][a-z0-9-]*[a-z0-9])`)

// docFlagAllowed lists flags in the docs that belong to another tool or are
// documented as absent, by file and flag, each with its reason.
var docFlagAllowed = map[[2]string]string{
	{"docs/getting-started.md", "password"}: "the page says there is no --password flag",
	{"docs/cli/index.md", "password"}:       "the page says there is no --password flag",
	{"docs/releasing.md", "push"}:           "flag of scripts/release.sh",
	{"docs/releasing.md", "skip"}:           "goreleaser flag",
	{"docs/cli/profiles.md", "user"}:        "systemctl flag in the timer example",
	{"docs/cli/profiles.md", "now"}:         "systemctl flag in the timer example",
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

	for _, file := range files {
		f, err := os.Open(filepath.Join(root, file))
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		for line := 1; sc.Scan(); line++ {
			for _, m := range docFlagToken.FindAllStringSubmatch(sc.Text(), -1) {
				flag := m[1]
				if known[flag] || docFlagAllowed[[2]string{file, flag}] != "" {
					continue
				}
				t.Errorf("%s:%d: --%s is not a flag of the ark CLI", file, line, flag)
			}
		}
		if err := sc.Err(); err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		f.Close()
	}
}
