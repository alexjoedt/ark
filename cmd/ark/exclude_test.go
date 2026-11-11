package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// files maps each name to itself as content.
func files(names ...string) map[string]string {
	m := map[string]string{}
	for _, n := range names {
		m[n] = n
	}
	return m
}

type excludeSnapshot struct {
	ID       string   `json:"id"`
	Tags     []string `json:"tags"`
	Excludes []string `json:"excludes"`
	Stats    struct {
		TotalFiles     int `json:"total_files"`
		FilesUnchanged int `json:"files_unchanged"`
	} `json:"stats"`
}

func backupJSON(t *testing.T, repo string, args ...string) excludeSnapshot {
	t.Helper()
	code, out := runCapture(t, append([]string{"--json", "-r", repo, "backup"}, args...)...)
	if code != 0 {
		t.Fatalf("backup exit %d: %s", code, out)
	}
	var s excludeSnapshot
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		t.Fatalf("stdout is not a snapshot: %q", out)
	}
	return s
}

func lsPaths(t *testing.T, repo, id string) []string {
	t.Helper()
	code, out := runCapture(t, "--json", "-r", repo, "ls", id)
	if code != 0 {
		t.Fatalf("ls exit %d: %s", code, out)
	}
	var paths []string
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		var e struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("not one object per line: %q", line)
		}
		paths = append(paths, e.Path)
	}
	slices.Sort(paths)
	return paths
}

func TestBackupExcludes(t *testing.T) {
	repo := initRepo(t)
	t.Setenv("ARK_REPOSITORY", "")
	src := t.TempDir()
	writeTree(t, src, files(
		"keep.txt", "a", "b.tmp", "a,b.tmp",
		"app/main.go", "app/build/out.o", "app/cache/y",
		"logs/x.log", "logs/keep.md", "cache/data",
	))
	// older than the parent's racy window, so the second backup can skip them
	old := time.Now().Add(-time.Hour)
	err := filepath.WalkDir(src, func(p string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(p, old, old)
	})
	if err != nil {
		t.Fatal(err)
	}
	exFile := filepath.Join(t.TempDir(), "excludes")
	if err := os.WriteFile(exFile, []byte("# comment\n\n*.log\n/cache\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{"-t", "x,y", "-e", "a,b.tmp", "--exclude", "build", "--exclude-file", exFile, src}

	first := backupJSON(t, repo, args...)
	wantEx := []string{"a,b.tmp", "build", "*.log", "/cache"}
	if !slices.Equal(first.Excludes, wantEx) || len(first.Tags) != 2 {
		t.Fatalf("excludes %q tags %q, want %q and two tags", first.Excludes, first.Tags, wantEx)
	}
	want := []string{
		"a", "app/", "app/cache/", "app/cache/y", "app/main.go", "b.tmp", "keep.txt", "logs/", "logs/keep.md",
	}
	if got := lsPaths(t, repo, first.ID); !slices.Equal(got, want) {
		t.Fatalf("ls %q, want %q", got, want)
	}

	second := backupJSON(t, repo, args...)
	if second.Stats.FilesUnchanged != 6 || second.Stats.TotalFiles != 6 {
		t.Fatalf("second backup stats %+v, want 6 files, all unchanged", second.Stats)
	}

	code, out := runCapture(t, "--json", "-r", repo, "snapshots")
	var list []excludeSnapshot
	if err := json.Unmarshal([]byte(out), &list); err != nil || code != 0 || len(list) != 2 {
		t.Fatalf("snapshots: exit %d, %q", code, out)
	}
	if !slices.Equal(list[0].Excludes, wantEx) {
		t.Fatalf("snapshots excludes %q", list[0].Excludes)
	}
	code, out = runCapture(t, "--json", "-r", repo, "inspect", first.ID)
	var one excludeSnapshot
	if err := json.Unmarshal([]byte(out), &one); err != nil || code != 0 || !slices.Equal(one.Excludes, wantEx) {
		t.Fatalf("inspect --json: exit %d, %q", code, out)
	}
	code, out = runCapture(t, "-r", repo, "inspect", first.ID)
	if code != 0 || strings.Count(out, "Excludes:") != len(wantEx) || !strings.Contains(out, "/cache") {
		t.Fatalf("inspect: exit %d, %q", code, out)
	}

	plain := backupJSON(t, repo, "--name", "plain", src)
	if plain.Excludes == nil || len(plain.Excludes) != 0 {
		t.Fatalf("excludes without patterns %#v, want []", plain.Excludes)
	}
	if code, out := runCapture(t, "--json", "-r", repo, "inspect", plain.ID); code != 0 ||
		!strings.Contains(out, `"excludes": []`) && !strings.Contains(out, `"excludes":[]`) {
		t.Fatalf("inspect --json without patterns: exit %d, %q", code, out)
	}
	if code, out := runCapture(t, "-r", repo, "inspect", plain.ID); code != 0 || strings.Contains(out, "Excludes:") {
		t.Fatalf("inspect without patterns: exit %d, %q", code, out)
	}
}

func TestBackupExcludeErrors(t *testing.T) {
	t.Setenv("ARK_REPOSITORY", "")
	src := t.TempDir()
	// a repository that does not exist: opening it would fail differently
	repo := filepath.Join(t.TempDir(), "missing")
	missing := filepath.Join(t.TempDir(), "nope")
	tests := []struct {
		args     []string
		code     int
		kind     string
		contains string
	}{
		{[]string{"-e", "["}, 2, "usage", `"["`},
		{[]string{"-e", "!keep"}, 2, "usage", `"!keep"`},
		{[]string{"--exclude-file", missing}, 1, "error", missing},
	}
	for _, tt := range tests {
		args := append([]string{"--json", "-r", repo, "backup"}, tt.args...)
		code, out := runCapture(t, append(args, src)...)
		var res struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(out), &res); err != nil {
			t.Fatalf("%v: not an error object: %q", tt.args, out)
		}
		if code != tt.code || res.Error.Code != tt.kind || !strings.Contains(res.Error.Message, tt.contains) ||
			strings.Contains(res.Error.Message, repo) {
			t.Errorf("%v: exit %d, %+v", tt.args, code, res.Error)
		}
	}
}

func TestBackupExcludeCloudFiles(t *testing.T) {
	repo := initRepo(t)
	t.Setenv("ARK_REPOSITORY", "")
	src := t.TempDir()
	writeTree(t, src, files("doc.txt", ".x.icloud"))

	with := backupJSON(t, repo, "--exclude-cloud-files", src)
	if got := lsPaths(t, repo, with.ID); !slices.Equal(got, []string{"doc.txt"}) {
		t.Fatalf("with flag: %q", got)
	}
	without := backupJSON(t, repo, src)
	if got := lsPaths(t, repo, without.ID); !slices.Equal(got, []string{".x.icloud", "doc.txt"}) {
		t.Fatalf("without flag: %q", got)
	}
}
