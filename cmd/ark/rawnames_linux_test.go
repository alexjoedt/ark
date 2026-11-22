package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOddNamesInListings(t *testing.T) {
	repo := initRepo(t)
	t.Setenv("ARK_REPOSITORY", repo)
	src := t.TempDir()
	odd := "a\xffb\x1b[2J\\"
	for name, target := range map[string]string{odd: "", "plain\\x41.txt": "", "link": odd} {
		if target == "" {
			if err := os.WriteFile(filepath.Join(src, name), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		} else if err := os.Symlink(target, filepath.Join(src, name)); err != nil {
			t.Fatal(err)
		}
	}
	code, stdout := runCapture(t, "--json", "backup", src)
	var s struct {
		ID string `json:"id"`
	}
	if code != 0 || json.Unmarshal([]byte(stdout), &s) != nil {
		t.Fatalf("backup: exit %d, %q", code, stdout)
	}

	code, stdout = runCapture(t, "ls", s.ID)
	if code != 0 || !strings.Contains(stdout, "\n"+odd+"\n") && !strings.HasPrefix(stdout, odd+"\n") {
		t.Fatalf("a pipe gets the raw bytes: exit %d, %q", code, stdout)
	}
	code, stdout = runCapture(t, "find", "plain*")
	if code != 0 || !strings.Contains(stdout, "plain\\x41.txt\n") {
		t.Fatalf("find: exit %d, %q", code, stdout)
	}

	code, stdout = runCapture(t, "--json", "ls", s.ID)
	if code != 0 {
		t.Fatalf("ls --json: exit %d", code)
	}
	raw := map[string]string{}
	for line := range strings.SplitSeq(strings.TrimSpace(stdout), "\n") {
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		path, _ := e["path"].(string)
		r, has := e["path_raw"].(string)
		if has {
			raw[path] = r
		}
		if path == "link" {
			if e["link_dest_raw"] != base64.StdEncoding.EncodeToString([]byte(odd)) {
				t.Fatalf("link_dest_raw of %v", e)
			}
		} else if _, has := e["link_dest_raw"]; has {
			t.Fatalf("link_dest_raw on %v", e)
		}
		if path == "plain\\x41.txt" && has {
			t.Fatalf("path_raw on a printable name: %v", e)
		}
	}
	if len(raw) != 1 {
		t.Fatalf("path_raw on %v, want only the odd name", raw)
	}
	for _, r := range raw {
		b, err := base64.StdEncoding.DecodeString(r)
		if err != nil || string(b) != odd {
			t.Fatalf("path_raw %q decodes to %q", r, b)
		}
	}

	code, stdout = runCapture(t, "--json", "find", "link", "a*")
	if code != 0 || !strings.Contains(stdout, `"path_raw":"`+base64.StdEncoding.EncodeToString([]byte(odd))+`"`) {
		t.Fatalf("find --json: exit %d, %q", code, stdout)
	}
}
