package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDebugNode(t *testing.T) {
	repo := initRepo(t)
	t.Setenv("ARK_REPOSITORY", repo)
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"a.txt": "hello", "sub/b.txt": "b"} {
		if err := os.WriteFile(filepath.Join(src, name), []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("a.txt", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	code, stdout := runCapture(t, "--json", "backup", src)
	var s struct {
		ID string `json:"id"`
	}
	if code != 0 || json.Unmarshal([]byte(stdout), &s) != nil {
		t.Fatalf("backup: exit %d, %q", code, stdout)
	}

	type node struct {
		Entries []struct {
			Name   string `json:"name"`
			Type   string `json:"type"`
			Mode   string `json:"mode"`
			Target string `json:"target"`
			Tree   string `json:"tree"`
			Chunks []struct {
				ID     string `json:"id"`
				Length int64  `json:"length"`
			} `json:"chunks"`
		} `json:"entries"`
	}
	dump := func(args ...string) node {
		t.Helper()
		code, stdout := runCapture(t, append([]string{"debug", "node", s.ID[:8]}, args...)...)
		var n node
		if code != 0 || json.Unmarshal([]byte(stdout), &n) != nil {
			t.Fatalf("debug node %v: exit %d, %q", args, code, stdout)
		}
		return n
	}
	root := dump()
	if len(root.Entries) != 3 {
		t.Fatalf("root has %d entries: %+v", len(root.Entries), root)
	}
	for _, e := range root.Entries {
		switch e.Name {
		case "a.txt":
			if e.Type != "regular" || e.Mode != "0640" || len(e.Chunks) != 1 || e.Chunks[0].Length != 5 {
				t.Errorf("a.txt: %+v", e)
			}
		case "link":
			if e.Type != "symlink" || e.Target != "a.txt" {
				t.Errorf("link: %+v", e)
			}
		case "sub":
			if e.Type != "directory" || len(e.Tree) != 64 {
				t.Errorf("sub: %+v", e)
			}
		default:
			t.Errorf("unexpected entry %q", e.Name)
		}
	}
	if sub := dump("sub"); len(sub.Entries) != 1 || sub.Entries[0].Name != "b.txt" {
		t.Errorf("sub: %+v", sub)
	}

	for _, args := range [][]string{{"a.txt"}, {"nope"}} {
		code, stdout := runCapture(t, append([]string{"debug", "node", s.ID}, args...)...)
		if code == 0 || stdout != "" {
			t.Errorf("debug node %v: exit %d, stdout %q, want a failure", args, code, stdout)
		}
	}
	for _, args := range [][]string{{"debug"}, {"debug", "nope"}, {"debug", "node"}} {
		if code, _ := runCapture(t, args...); code != 2 {
			t.Errorf("ark %v: exit %d, want 2", args, code)
		}
	}
}

func TestDebugNodeKeepsRawNames(t *testing.T) {
	repo := initRepo(t)
	t.Setenv("ARK_REPOSITORY", repo)
	src := t.TempDir()
	odd := "a\xffb"
	if err := os.WriteFile(filepath.Join(src, odd), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(odd, filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	code, stdout := runCapture(t, "--json", "backup", src)
	var s struct {
		ID string `json:"id"`
	}
	if code != 0 || json.Unmarshal([]byte(stdout), &s) != nil {
		t.Fatalf("backup: exit %d, %q", code, stdout)
	}
	code, stdout = runCapture(t, "debug", "node", s.ID)
	want := base64.StdEncoding.EncodeToString([]byte(odd))
	if code != 0 || strings.Count(stdout, `"name_raw": "`+want+`"`) != 1 ||
		strings.Count(stdout, `"target_raw": "`+want+`"`) != 1 {
		t.Fatalf("exit %d, output %q", code, stdout)
	}
}
