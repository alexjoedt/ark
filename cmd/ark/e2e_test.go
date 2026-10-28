package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"testing"
)

// envSFTPTestURI is the sftp server the arksftp conformance run uses; the
// lifecycle runs against it too when set.
const envSFTPTestURI = "ARK_SFTP_TEST_URI"

// TestE2ELifecycle drives the built binary through a repository's life:
// init, backup, an incremental backup after changes, restore of both
// snapshots, forget, prune, check, and a restore after prune.
func TestE2ELifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary, skipped with -short")
	}
	bin := filepath.Join(t.TempDir(), "ark")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build ark: %v\n%s", err, out)
	}

	t.Run("local", func(t *testing.T) {
		lifecycle(t, bin, filepath.Join(t.TempDir(), "repo"))
	})
	t.Run("sftp", func(t *testing.T) {
		base := os.Getenv(envSFTPTestURI)
		if base == "" {
			t.Skipf("%s is not set", envSFTPTestURI)
		}
		u, err := url.Parse(base)
		if err != nil {
			t.Fatalf("parse %s: %v", envSFTPTestURI, err)
		}
		u.Path = path.Join(u.Path, "e2e-"+rand.Text())
		lifecycle(t, bin, u.String())
	})
}

func lifecycle(t *testing.T, bin, repo string) {
	ark := func(want int, args ...string) []byte {
		t.Helper()
		var stdout, stderr bytes.Buffer
		cmd := exec.CommandContext(t.Context(), bin, append([]string{"--json", "-q", "-r", repo}, args...)...)
		// a password protected repository, TestMain's opt-out cleared
		cmd.Env = append(os.Environ(), "ARK_INSECURE_NO_PASSWORD=", "ARK_PASSWORD=e2e-password")
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		if code := cmd.ProcessState.ExitCode(); code != want {
			t.Fatalf("ark %v: exit %d, want %d: %v\nstdout: %s\nstderr: %s",
				args, code, want, err, stdout.String(), stderr.String())
		}
		return stdout.Bytes()
	}
	decode := func(data []byte, v any) {
		t.Helper()
		if err := json.Unmarshal(data, v); err != nil {
			t.Fatalf("decode %q: %v", data, err)
		}
	}
	type snapshot struct {
		ID       string `json:"id"`
		ParentID string `json:"parent_id"`
	}

	src := t.TempDir()
	writeTree(t, src, map[string]string{
		"a.txt":         "hello",
		"empty":         "",
		"sub/b.txt":     "world",
		"sub/deep/c.md": "# c",
		"big.bin":       randomBytes(t, 3<<20),
	})
	if err := os.Symlink("sub/b.txt", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(src, "a.txt"), 0o600); err != nil {
		t.Fatal(err)
	}

	ark(0, "init")
	var first snapshot
	decode(ark(0, "backup", src), &first)
	want1 := readTree(t, src)

	// modify, add, delete, then back up again: the second snapshot has the first as parent
	writeTree(t, src, map[string]string{"a.txt": "hello again", "sub/new.txt": "new"})
	if err := os.Remove(filepath.Join(src, "sub/deep/c.md")); err != nil {
		t.Fatal(err)
	}
	var second snapshot
	decode(ark(0, "backup", src), &second)
	if second.ParentID != first.ID {
		t.Fatalf("parent %q, want %q", second.ParentID, first.ID)
	}
	want2 := readTree(t, src)

	var list []snapshot
	decode(ark(0, "snapshots"), &list)
	if len(list) != 2 {
		t.Fatalf("snapshots: got %d, want 2", len(list))
	}

	for _, c := range []struct {
		id   string
		want map[string]string
	}{{first.ID, want1}, {second.ID, want2}} {
		dest := filepath.Join(t.TempDir(), "out")
		ark(0, "restore", c.id, dest)
		compareTrees(t, c.id, c.want, readTree(t, dest))
		ark(0, "verify", c.id)
	}

	var forgot struct {
		Remove []snapshot `json:"remove"`
	}
	decode(ark(0, "forget", "--keep-last", "1", "--yes"), &forgot)
	if len(forgot.Remove) != 1 || forgot.Remove[0].ID != first.ID {
		t.Fatalf("forget removed %+v, want only %s", forgot.Remove, first.ID)
	}
	var pruned struct {
		BlobsRemoved int `json:"blobs_removed"`
	}
	decode(ark(0, "prune", "--yes"), &pruned)
	if pruned.BlobsRemoved == 0 {
		t.Fatal("prune removed nothing after forgetting a snapshot with unique chunks")
	}

	var checked struct {
		SnapshotsChecked int      `json:"snapshots_checked"`
		Errors           []string `json:"errors"`
	}
	decode(ark(0, "check"), &checked)
	if checked.SnapshotsChecked != 1 || len(checked.Errors) != 0 {
		t.Fatalf("check: %+v", checked)
	}

	dest := filepath.Join(t.TempDir(), "out")
	ark(0, "restore", second.ID, dest)
	compareTrees(t, second.ID+" after prune", want2, readTree(t, dest))
	ark(4, "restore", first.ID, filepath.Join(t.TempDir(), "gone"))
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// readTree describes every entry under root by type, permissions and
// content or link target, keyed by slash path.
func readTree(t *testing.T, root string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || p == root {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		desc := info.Mode().String()
		switch {
		case d.Type()&os.ModeSymlink != 0:
			var target string
			if target, err = os.Readlink(p); err != nil {
				return err
			}
			desc = "link " + target
		case d.Type().IsRegular():
			var data []byte
			if data, err = os.ReadFile(p); err != nil {
				return err
			}
			desc += " " + string(data)
		}
		tree[filepath.ToSlash(rel)] = desc
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

func compareTrees(t *testing.T, label string, want, got map[string]string) {
	t.Helper()
	for p, w := range want {
		g, ok := got[p]
		switch {
		case !ok:
			t.Errorf("%s: %s missing", label, p)
		case g != w:
			t.Errorf("%s: %s differs: got %.60q, want %.60q", label, p, g, w)
		}
	}
	for p := range got {
		if _, ok := want[p]; !ok {
			t.Errorf("%s: unexpected %s", label, p)
		}
	}
}

func randomBytes(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return string(b)
}
