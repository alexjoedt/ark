package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkuri"
)

const e2ePassword = "e2e-password"

// TestE2ENode drives the built binary through what the binary tree node was
// made for: raw names, mode bits, times in another zone and special files,
// on arklocal and arksftp, with a check after each case.
func TestE2ENode(t *testing.T) {
	bin := buildArk(t)
	t.Run("local", func(t *testing.T) {
		nodeCases(t, bin, filepath.Join(t.TempDir(), "repo"))
	})
	t.Run("sftp", func(t *testing.T) {
		nodeCases(t, bin, sftpTestRepo(t, "e2e-node-"))
	})
}

// mayMknod reports whether this process may create device nodes. Not 0,0:
// Linux lets anyone make that one, an overlayfs whiteout.
func mayMknod(t *testing.T) bool {
	return unix.Mknod(filepath.Join(t.TempDir(), "probe"), unix.S_IFCHR|0o600, int(unix.Mkdev(1, 3))) == nil
}

func nodeCases(t *testing.T, bin, repo string) {
	run := func(env []string, want int, args ...string) ([]byte, string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		cmd := exec.CommandContext(t.Context(), bin, append([]string{"--json", "-r", repo}, args...)...)
		cmd.Env = append(os.Environ(), "ARK_INSECURE_NO_PASSWORD=", "ARK_PASSWORD="+e2ePassword)
		cmd.Env = append(cmd.Env, env...)
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		if code := cmd.ProcessState.ExitCode(); code != want {
			t.Fatalf("ark %v: exit %d, want %d: %v\nstdout: %s\nstderr: %s",
				args, code, want, err, stdout.String(), stderr.String())
		}
		return stdout.Bytes(), stderr.String()
	}
	decode := func(data []byte, v any) {
		t.Helper()
		if err := json.Unmarshal(data, v); err != nil {
			t.Fatalf("decode %q: %v", data, err)
		}
	}
	type stats struct {
		Files     int `json:"total_files"`
		Unchanged int `json:"files_unchanged"`
	}
	type snapshot struct {
		ID    string `json:"id"`
		Stats stats  `json:"stats"`
	}
	backup := func(env []string, src string) snapshot {
		t.Helper()
		var s snapshot
		out, _ := run(env, 0, "backup", src)
		decode(out, &s)
		return s
	}
	restore := func(id string) (string, string) {
		t.Helper()
		dest := filepath.Join(t.TempDir(), "out")
		_, stderr := run(nil, 0, "restore", id, dest)
		return dest, stderr
	}
	check := func() {
		t.Helper()
		var c struct {
			Errors []string `json:"errors"`
		}
		out, _ := run(nil, 0, "check")
		decode(out, &c)
		if len(c.Errors) != 0 {
			t.Fatalf("check: %v", c.Errors)
		}
	}
	treePacks := func() int64 {
		t.Helper()
		var info struct {
			Stored struct {
				TreePacks int64 `json:"treepacks"`
			} `json:"stored"`
		}
		out, _ := run(nil, 0, "info")
		decode(out, &info)
		return info.Stored.TreePacks
	}

	run(nil, 0, "init")

	t.Run("raw names", func(t *testing.T) {
		src := t.TempDir()
		writeTree(t, src, map[string]string{"caf\xe9.txt": "e9", "caf\xe8.txt": "e8", "d\xfe/caf\xe9.txt": "inner"})
		if err := os.Symlink("caf\xe9\xff", filepath.Join(src, "link")); err != nil {
			t.Fatal(err)
		}
		const xattr = "user.caf\xe9"
		xattrOK := unix.Setxattr(filepath.Join(src, "caf\xe9.txt"), xattr, []byte("v"), 0) == nil
		if !xattrOK {
			if os.Getenv("CI") != "" {
				t.Fatal("user xattrs unsupported in CI, the xattr name would go unchecked")
			}
			t.Log("user xattrs unsupported here, the xattr name goes unchecked")
		}
		dest, _ := restore(backup(nil, src).ID)
		compareTrees(t, "raw names", nodeTree(t, src), nodeTree(t, dest))
		if xattrOK {
			buf := make([]byte, 8)
			n, err := unix.Lgetxattr(filepath.Join(dest, "caf\xe9.txt"), xattr, buf)
			if err != nil || string(buf[:n]) != "v" {
				t.Errorf("xattr %q restored %q (%v), want v", xattr, buf[:max(n, 0)], err)
			}
		}
		check()
	})

	t.Run("mode bits", func(t *testing.T) {
		src := t.TempDir()
		writeTree(t, src, map[string]string{"setuid": "x", "setgid/f": "y", "sticky/f": "z"})
		for name, mode := range map[string]os.FileMode{
			"setuid": 0o755 | os.ModeSetuid,
			"setgid": 0o775 | os.ModeDir | os.ModeSetgid,
			"sticky": 0o777 | os.ModeDir | os.ModeSticky,
		} {
			if err := os.Chmod(filepath.Join(src, name), mode); err != nil {
				t.Fatal(err)
			}
		}
		want := nodeTree(t, src)
		for name, perm := range map[string]string{"setuid": "urwxr-xr-x", "setgid": "dgrwxrwxr-x", "sticky": "dtrwxrwxrwx"} {
			if got, _, _ := strings.Cut(want[name], " "); got != perm {
				t.Fatalf("source %s is %s, want %s", name, got, perm)
			}
		}
		dest, _ := restore(backup(nil, src).ID)
		compareTrees(t, "mode bits", want, nodeTree(t, dest))
		check()
	})

	t.Run("time zones", func(t *testing.T) {
		// The ark binary embeds no tzdata; without the zone TZ falls back to UTC.
		if _, err := os.Stat("/usr/share/zoneinfo/Asia/Tokyo"); err != nil {
			if os.Getenv("CI") != "" {
				t.Fatalf("no system zone in CI, the case would compare UTC with UTC: %v", err)
			}
			t.Skipf("no system zone: %v", err)
		}
		src := t.TempDir()
		writeTree(t, src, map[string]string{"a.txt": "a", "sub/b.txt": "b"})
		// older than the racy window, or no file counts as unchanged
		old := time.Now().Add(-time.Hour)
		for _, name := range []string{"a.txt", "sub/b.txt", "sub"} {
			if err := os.Chtimes(filepath.Join(src, name), old, old); err != nil {
				t.Fatal(err)
			}
		}
		first := backup([]string{"TZ=UTC"}, src)
		before := treePacks()
		second := backup([]string{"TZ=Asia/Tokyo"}, src)
		if second.Stats.Unchanged != second.Stats.Files || second.Stats.Files != first.Stats.Files {
			t.Errorf("Tokyo backup %+v after UTC %+v: want every file unchanged", second.Stats, first.Stats)
		}
		if after := treePacks(); after != before {
			t.Errorf("Tokyo backup stored tree packs: %d bytes, %d before", after, before)
		}
		check()
	})

	t.Run("special files", func(t *testing.T) {
		privileged := mayMknod(t)
		src := t.TempDir()
		writeTree(t, src, map[string]string{"a.txt": "keep"})
		if err := unix.Mkfifo(filepath.Join(src, "pipe"), 0o640); err != nil {
			t.Fatal(err)
		}
		if privileged {
			mknod(t, filepath.Join(src, "null"), unix.S_IFCHR, 1, 3)
			mknod(t, filepath.Join(src, "loop"), unix.S_IFBLK, 7, 0)
		}
		dest, _ := restore(backup(nil, src).ID)
		compareTrees(t, "special files", nodeTree(t, src), nodeTree(t, dest))
		check()

		// A snapshot with devices that an unprivileged backup cannot take.
		dest, stderr := restore(backupDevices(t, repo))
		got := nodeTree(t, dest)
		if got["a.txt"] != "-rw-r--r-- keep" {
			t.Errorf("a.txt restored as %q", got["a.txt"])
		}
		if privileged {
			if got["null"] != "Dcrw-rw-rw- 1,3" || got["loop"] != "Drw-rw---- 7,0" {
				t.Errorf("devices restored as %q and %q", got["null"], got["loop"])
			}
		} else {
			for _, name := range []string{"null", "loop"} {
				if _, ok := got[name]; ok || !strings.Contains(stderr, "warning: restore: skipped "+name+":") {
					t.Errorf("%s: restored %t, stderr %q", name, ok, stderr)
				}
			}
		}
		check()
	})
}

func mknod(t *testing.T, path string, typ uint32, major, minor uint32) {
	t.Helper()
	if err := unix.Mknod(path, typ|0o644, int(unix.Mkdev(major, minor))); err != nil {
		t.Fatal(err)
	}
}

// devicesSource yields a file, a character and a block device.
type devicesSource struct{}

func (devicesSource) Meta() ark.SourceMeta {
	return ark.SourceMeta{Name: "devices", Type: ark.SourceTypeFile}
}

func (devicesSource) Walk(_ context.Context, fn ark.WalkFunc) error {
	err := fn(ark.FileInfo{Path: "a.txt", Size: 4, Mode: 0o644, Type: ark.FileTypeRegular},
		func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("keep")), nil })
	if err != nil {
		return err
	}
	if err := fn(ark.FileInfo{Path: "null", Mode: 0o666, Type: ark.FileTypeCharDevice, DevMajor: 1, DevMinor: 3}, nil); err != nil {
		return err
	}
	return fn(ark.FileInfo{Path: "loop", Mode: 0o660, Type: ark.FileTypeBlockDevice, DevMajor: 7}, nil)
}

// backupDevices stores devicesSource in the repository at uri through the
// library and returns the snapshot ID.
func backupDevices(t *testing.T, uri string) string {
	t.Helper()
	ctx := context.Background()
	b, err := arkuri.Open(ctx, uri)
	if err != nil {
		t.Fatal(err)
	}
	r, err := ark.Open(ctx, b, ark.NewPassword(e2ePassword))
	if err != nil {
		_ = b.Close()
		t.Fatal(err)
	}
	snap, err := r.Backup(ctx, devicesSource{})
	if cerr := r.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(snap.ID)
}

// nodeTree is readTree with the device number of each device.
func nodeTree(t *testing.T, root string) map[string]string {
	t.Helper()
	tree := readTree(t, root)
	for p, desc := range tree {
		if desc[0] != 'D' {
			continue
		}
		var st unix.Stat_t
		if err := unix.Lstat(filepath.Join(root, p), &st); err != nil {
			t.Fatal(err)
		}
		tree[p] = fmt.Sprintf("%s %d,%d", desc, unix.Major(st.Rdev), unix.Minor(st.Rdev))
	}
	return tree
}
