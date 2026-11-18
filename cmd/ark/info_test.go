package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInfo(t *testing.T) {
	t.Run("local", func(t *testing.T) { checkInfo(t, initRepo(t)) })
	t.Run("sftp", func(t *testing.T) {
		repo := sftpTestRepo(t, "info-")
		if code, _ := runCapture(t, "-r", repo, "init"); code != 0 {
			t.Fatalf("init exit %d", code)
		}
		checkInfo(t, repo)
	})
}

func checkInfo(t *testing.T, repo string) {
	t.Helper()
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte(strings.Repeat("x", 5000)), 0o644); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if code, _ := runCapture(t, "-r", repo, "backup", src); code != 0 {
			t.Fatalf("backup exit %d", code)
		}
	}

	type infoJSON struct {
		Snapshots   int              `json:"snapshots"`
		TotalSize   int64            `json:"total_size"`
		StoredSize  int64            `json:"stored_size"`
		Stored      map[string]int64 `json:"stored"`
		BytesUnused *int64           `json:"bytes_unused"`
	}
	for _, exact := range []bool{false, true} {
		args := []string{"--json", "-r", repo, "info"}
		if exact {
			args = append(args, "--exact")
		}
		code, stdout := runCapture(t, args...)
		if code != 0 {
			t.Fatalf("%v: exit %d", args, code)
		}
		var got infoJSON
		if err := json.Unmarshal([]byte(stdout), &got); err != nil {
			t.Fatalf("%v: %v in %s", args, err, stdout)
		}
		if got.Snapshots != 2 || got.TotalSize != 10000 {
			t.Errorf("%v: snapshots %d, total_size %d; want 2, 10000", args, got.Snapshots, got.TotalSize)
		}
		var sum int64
		for _, n := range got.Stored {
			sum += n
		}
		if len(got.Stored) != 6 || got.Stored["datapacks"] == 0 || got.Stored["keys"] == 0 || sum != got.StoredSize {
			t.Errorf("%v: stored %v, stored_size %d", args, got.Stored, got.StoredSize)
		}
		if (got.BytesUnused != nil) != exact {
			t.Errorf("%v: bytes_unused present %v, want %v", args, got.BytesUnused != nil, exact)
		}

		code, stdout = runCapture(t, args[1:]...)
		if code != 0 || !strings.Contains(stdout, "Ratio:") || strings.Contains(stdout, "Unused:") != exact {
			t.Errorf("%v: exit %d, text output:\n%s", args[1:], code, stdout)
		}
	}
}
