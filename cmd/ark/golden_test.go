package main

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The golden files pin what --json and --quiet print for the six
// repository commands. Values that change per run are replaced by key
// (snapshot IDs, timestamps, durations, the source name) and by literal
// (known IDs, temp directories), so field order and wording stay exact.
// ARK_UPDATE_GOLDEN=1 regenerates testdata/golden.

var (
	volatileKey = regexp.MustCompile(`^(\s*"(id|parent_id|created_at|duration|source_name)": )"(.*)"(,?)$`)
	clockTime   = regexp.MustCompile(`\d{4}-\d\d-\d\d \d\d:\d\d(:\d\d)?`)
)

type normalizer struct {
	ids     map[string]string
	literal map[string]string
}

func (n *normalizer) id(v string) string {
	if v == "" {
		return v
	}
	if _, ok := n.ids[v]; !ok {
		n.ids[v] = fmt.Sprintf("<id%d>", len(n.ids)+1)
	}
	return n.ids[v]
}

func (n *normalizer) apply(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		m := volatileKey.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		v := "<" + m[2] + ">"
		switch m[2] {
		case "id", "parent_id":
			v = n.id(m[3])
		case "source_name":
			continue
		}
		if m[3] == "" {
			v = ""
		}
		lines[i] = m[1] + `"` + v + `"` + m[4]
	}
	// Longest first, so a temp dir that is a prefix of another never wins.
	repl := maps.Clone(n.literal)
	maps.Copy(repl, n.ids)
	keys := slices.SortedFunc(maps.Keys(repl), func(a, b string) int { return len(b) - len(a) })
	pairs := make([]string, 0, 2*len(keys))
	for _, k := range keys {
		pairs = append(pairs, k, repl[k])
	}
	s = strings.NewReplacer(pairs...).Replace(strings.Join(lines, "\n"))
	return clockTime.ReplaceAllString(s, "<time>")
}

func goldenRun(t *testing.T, mode string) string {
	t.Helper()
	repo := initRepo(t)
	t.Setenv("ARK_REPOSITORY", "")
	src := t.TempDir()
	for name, body := range map[string]string{"a.txt": "hello", "sub/b.txt": "world"} {
		path := filepath.Join(src, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dest := filepath.Join(t.TempDir(), "out")
	n := &normalizer{ids: map[string]string{}, literal: map[string]string{
		src: "<src>", dest: "<dest>", repo: "<repo>",
	}}
	var sb strings.Builder
	var lastID string
	step := func(args ...string) {
		t.Helper()
		var code int
		var stdout string
		stderr := capture(t, &os.Stderr, func() {
			code, stdout = runCapture(t, append([]string{mode, "-r", repo}, args...)...)
		})
		if args[0] == "backup" && code == 0 {
			if mode == "--quiet" {
				lastID = strings.TrimSpace(stdout)
				n.id(lastID)
			} else {
				for l := range strings.SplitSeq(stdout, "\n") {
					if m := volatileKey.FindStringSubmatch(l); m != nil && m[2] == "id" {
						lastID = m[3]
						n.id(lastID)
						break
					}
				}
			}
		}
		shown := n.apply(strings.Join(args, " "))
		fmt.Fprintf(
			&sb,
			"=== %s %s (exit %d)\n--- stdout\n%s--- stderr\n%s",
			mode,
			shown,
			code,
			n.apply(stdout),
			n.apply(stderr),
		)
	}
	step("backup", "--host", "h", src)
	first := lastID
	step("backup", "--host", "h", src)
	step("check")
	step("verify", lastID)
	step("verify", "nonexistent")
	step("restore", first, dest)
	step("forget", "--keep-last", "1", "--yes")
	step("prune", "--yes")
	return sb.String()
}

func TestGoldenOutput(t *testing.T) {
	for _, mode := range []string{"--json", "--quiet"} {
		t.Run(strings.TrimPrefix(mode, "--"), func(t *testing.T) {
			got := goldenRun(t, mode)
			file := filepath.Join("testdata", "golden", strings.TrimPrefix(mode, "--")+".txt")
			if os.Getenv("ARK_UPDATE_GOLDEN") != "" {
				if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Fatalf("output differs from %s (ARK_UPDATE_GOLDEN=1 regenerates):\n%s", file, got)
			}
		})
	}
}
