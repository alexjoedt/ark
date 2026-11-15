package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexjoedt/ark/cmd/ark/internal/out"
)

func TestFind(t *testing.T) {
	repo := initRepo(t)
	t.Setenv("ARK_REPOSITORY", repo)
	src := t.TempDir()
	for _, p := range []string{"keep.txt", "dir/x.txt"} {
		full := filepath.Join(src, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(p), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	type snap struct {
		ID        string    `json:"id"`
		CreatedAt time.Time `json:"created_at"`
	}
	backup := func(args ...string) snap {
		t.Helper()
		code, stdout := runCapture(t, append(append([]string{"--json", "backup"}, args...), src)...)
		var s snap
		if code != 0 || json.Unmarshal([]byte(stdout), &s) != nil {
			t.Fatalf("backup %v: exit %d, %q", args, code, stdout)
		}
		return s
	}
	header := func(s snap) string {
		return "snapshot " + out.ShortID(s.ID) + " from " + out.TimeLong(s.CreatedAt)
	}
	first := backup("--name", "one", "--host", "h1", "-t", "a")
	second := backup("--name", "one", "--host", "h2", "-t", "a", "-t", "b")
	if err := os.RemoveAll(filepath.Join(src, "dir")); err != nil {
		t.Fatal(err)
	}
	third := backup("--name", "two", "--host", "h2", "-t", "b")

	find := func(args ...string) (int, string, string) {
		t.Helper()
		var code int
		var stdout string
		stderr := capture(t, &os.Stderr, func() { code, stdout = runCapture(t, args...) })
		return code, stdout, stderr
	}
	text := func(args ...string) string {
		t.Helper()
		code, stdout, stderr := find(append([]string{"find"}, args...)...)
		if code != 0 {
			t.Fatalf("find %v: exit %d, stderr %q", args, code, stderr)
		}
		return stdout
	}

	group := func(s snap, paths ...string) string {
		return header(s) + "\n" + strings.Join(paths, "\n") + "\n"
	}
	want := group(second, "dir/", "dir/x.txt") + "\n" + group(first, "dir/", "dir/x.txt")
	if got := text("dir"); got != want {
		t.Fatalf("find dir:\n%s\nwant:\n%s", got, want)
	}
	if got := text("-t", "a", "-t", "b", "x.txt"); got != group(second, "dir/x.txt") {
		t.Fatalf("all tags must match:\n%s", got)
	}
	if got := text("--host", "h1", "x.txt"); got != group(first, "dir/x.txt") {
		t.Fatalf("--host:\n%s", got)
	}
	if got := text("-N", "two", "keep.txt"); got != group(third, "keep.txt") {
		t.Fatalf("--name:\n%s", got)
	}
	writeProfile(t, profileDirFor(t), "two", "backup:\n  path: "+src+"\n")
	if got := text("-P", "two", "keep.txt"); got != group(third, "keep.txt") {
		t.Fatalf("-P two:\n%s", got)
	}

	code, stdout, stderr := find("-q", "find", "dir")
	if code != 0 || stdout != "dir/\ndir/x.txt\ndir/\ndir/x.txt\n" || stderr != "" {
		t.Fatalf("-q: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	code, stdout, _ = find("--json", "find", "x.txt", "keep.txt")
	if code != 0 {
		t.Fatalf("--json: exit %d", code)
	}
	var got []string
	for line := range strings.SplitSeq(strings.TrimSuffix(stdout, "\n"), "\n") {
		var m matchJSON
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("not one object per line: %q", line)
		}
		if m.Mtime.IsZero() || (m.Path == "keep.txt" && m.Size != int64(len("keep.txt"))) {
			t.Fatalf("match %+v lacks mtime or size", m)
		}
		got = append(got, m.SnapshotID[:8]+" "+m.Path)
	}
	wantJSON := []string{
		third.ID[:8] + " keep.txt",
		second.ID[:8] + " dir/x.txt", second.ID[:8] + " keep.txt",
		first.ID[:8] + " dir/x.txt", first.ID[:8] + " keep.txt",
	}
	if !slices.Equal(got, wantJSON) {
		t.Fatalf("--json matches %q, want %q", got, wantJSON)
	}

	for _, mode := range [][]string{nil, {"--json"}} {
		code, stdout, stderr := find(append(mode, "find", "nothing-here")...)
		if code != 0 || stdout != "" || stderr != "no matches\n" {
			t.Fatalf("%v no match: exit %d, stdout %q, stderr %q", mode, code, stdout, stderr)
		}
	}
	if code, stdout, stderr := find("-q", "find", "nothing-here"); code != 0 || stdout != "" || stderr != "" {
		t.Fatalf("-q no match: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	// usage errors come before the repository is opened
	t.Setenv("ARK_REPOSITORY", filepath.Join(t.TempDir(), "missing"))
	for _, args := range [][]string{{"find"}, {"find", "["}, {"find", "ok", "!neg"}} {
		if code, _, _ := find(args...); code != 2 {
			t.Fatalf("%v: exit %d, want 2", args, code)
		}
	}
}
