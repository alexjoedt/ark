//go:build !(linux && (386 || arm || mips || mipsle))

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"syscall"
	"testing"
	"time"

	ark "github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/cmd/ark/internal/out"
)

func TestBuildLayout(t *testing.T) {
	at := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	snaps := []*ark.Snapshot{
		{
			ID: "aaaaaaaa11", SourceName: "/srv/data", Host: ".hidden", Tags: []string{"a/b", "x"},
			CreatedAt: at.Add(500 * time.Millisecond),
		},
		{ID: "aaaaaaaa22", SourceName: "/srv/data", Host: "", Tags: []string{"x", "x"}, CreatedAt: at},
		{ID: "bbbbbbbb33", SourceName: "..", Host: "h", CreatedAt: at.Add(time.Hour)},
	}
	root := buildLayout(snaps)
	sec := out.TimeLong(at)

	ids := root.dirs["ids"].snaps
	if len(ids) != 3 || ids["aaaaaaaa11"] != "aaaaaaaa11" || ids["bbbbbbbb"] != "bbbbbbbb33" {
		t.Errorf("ids = %v, want full IDs for the shared prefix and a short one otherwise", ids)
	}
	data := root.dirs["names"].dirs["_srv_data"]
	if data == nil {
		t.Fatalf("names = %v, want _srv_data", keys(root.dirs["names"].dirs))
	}
	if data.snaps[sec] != "aaaaaaaa22" || data.snaps[sec+"-1"] != "aaaaaaaa11" {
		t.Errorf("same-second names = %v", data.snaps)
	}
	if data.links["latest"] != sec+"-1" {
		t.Errorf("latest = %q, want %q", data.links["latest"], sec+"-1")
	}
	if got := keys(root.dirs["names"].dirs); !slices.Equal(got, []string{"_..", "_srv_data"}) {
		t.Errorf("names = %v", got)
	}
	if got := keys(root.dirs["hosts"].dirs); !slices.Equal(got, []string{"_", "_.hidden", "h"}) {
		t.Errorf("hosts = %v", got)
	}
	tags := root.dirs["tags"].dirs
	if got := keys(tags); !slices.Equal(got, []string{"a_b", "x"}) {
		t.Errorf("tags = %v", got)
	}
	if len(tags["x"].snaps) != 2 {
		t.Errorf("tags/x = %v, want each snapshot once", tags["x"].snaps)
	}
	if len(tags["x"].links) != 0 {
		t.Errorf("tags/x links = %v, want latest only per name", tags["x"].links)
	}
}

func keys[V any](m map[string]V) []string {
	return slices.Sorted(func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	})
}

func TestMountRefusesNestedRepository(t *testing.T) {
	repo := initRepo(t)
	inside := filepath.Join(repo, "mnt")
	if err := os.Mkdir(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	if code, _ := runCapture(t, "-r", repo, "mount", inside); code != 2 {
		t.Errorf("mountpoint inside the repository: exit %d, want 2", code)
	}
	if code, _ := runCapture(t, "-r", repo, "mount", filepath.Dir(repo)); code != 2 {
		t.Errorf("repository inside the mountpoint: exit %d, want 2", code)
	}
	if code, _ := runCapture(t, "-r", repo, "mount", filepath.Join(t.TempDir(), "missing")); code != 1 {
		t.Errorf("missing mountpoint: exit %d, want 1", code)
	}
}

// mountable skips the test where no FUSE mount can be made.
func mountable(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("FUSE tests run on Linux")
	}
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skip("no /dev/fuse")
	}
	if _, err := exec.LookPath("fusermount3"); err != nil {
		if _, err := exec.LookPath("fusermount"); err != nil {
			t.Skip("no fusermount")
		}
	}
}

// startMount runs ark mount on mnt and returns once the layout is served.
// The channel yields the exit code.
func startMount(t *testing.T, mnt string) <-chan int {
	t.Helper()
	done := make(chan int, 1)
	go func() { done <- run([]string{"ark", "mount", mnt}) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(mnt, "ids")); err == nil {
			return done
		}
		select {
		case code := <-done:
			t.Fatalf("mount exited %d before serving", code)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("mount not served after 10s")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// interrupt sends SIGINT, as Ctrl-C does, and returns the exit code.
func interrupt(t *testing.T, done <-chan int) int {
	t.Helper()
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		return code
	case <-time.After(15 * time.Second):
		t.Fatal("mount did not exit after SIGINT")
		return -1
	}
}

func TestMount(t *testing.T) {
	mountable(t)
	repo := initRepo(t)
	t.Setenv("ARK_REPOSITORY", repo)
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "dir"), 0o750); err != nil {
		t.Fatal(err)
	}
	content := []byte("hello from the snapshot\n")
	if err := os.WriteFile(filepath.Join(src, "dir", "file.txt"), content, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(src, "dir", "file.txt"), filepath.Join(src, "hard.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("dir/file.txt", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	mtime := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(src, "dir", "file.txt"), mtime, mtime); err != nil {
		t.Fatal(err)
	}
	code, stdout := runCapture(t, "--json", "backup", "--name", "app/data", "--host", "h1", "-t", ".t", src)
	var snap struct {
		ID        string    `json:"id"`
		CreatedAt time.Time `json:"created_at"`
	}
	if code != 0 || json.Unmarshal([]byte(stdout), &snap) != nil {
		t.Fatalf("backup: exit %d, %q", code, stdout)
	}

	mnt := t.TempDir()
	done := startMount(t, mnt)
	when := out.TimeLong(snap.CreatedAt)
	for _, dir := range []string{
		filepath.Join("ids", out.ShortID(snap.ID)),
		filepath.Join("names", "app_data", when),
		filepath.Join("names", "app_data", "latest"),
		filepath.Join("tags", "_.t", when),
		filepath.Join("hosts", "h1", when),
	} {
		got, err := os.ReadFile(filepath.Join(mnt, dir, "dir", "file.txt"))
		if err != nil || string(got) != string(content) {
			t.Errorf("%s/dir/file.txt = %q, %v", dir, got, err)
		}
	}
	snapDir := filepath.Join(mnt, "ids", out.ShortID(snap.ID))
	if entries, err := os.ReadDir(snapDir); err != nil || len(entries) != 3 {
		t.Errorf("snapshot root lists %v, %v; want dir, hard.txt, link", entries, err)
	}
	if target, err := os.Readlink(filepath.Join(mnt, "names", "app_data", "latest")); err != nil || target != when {
		t.Errorf("latest -> %q, %v; want %q", target, err, when)
	}
	if target, err := os.Readlink(filepath.Join(snapDir, "link")); err != nil || target != "dir/file.txt" {
		t.Errorf("link -> %q, %v", target, err)
	}
	fi, err := os.Stat(filepath.Join(snapDir, "dir", "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode() != 0o640 || fi.Size() != int64(len(content)) || !fi.ModTime().Equal(mtime) {
		t.Errorf("stat = %v %d %v, want -rw-r----- %d %v", fi.Mode(), fi.Size(), fi.ModTime(), len(content), mtime)
	}
	hard, err := os.Stat(filepath.Join(snapDir, "hard.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(fi, hard) {
		t.Error("hardlink and its target have different inodes")
	}
	if err := os.WriteFile(filepath.Join(snapDir, "new"), nil, 0o644); err == nil {
		t.Error("write into the mount succeeded")
	}

	if code := interrupt(t, done); code != 0 {
		t.Errorf("exit after SIGINT = %d, want 0", code)
	}
	if _, err := os.Stat(filepath.Join(mnt, "ids")); !os.IsNotExist(err) {
		t.Errorf("mountpoint still served after exit: %v", err)
	}
}

func TestMountBusyUnmountExits1(t *testing.T) {
	mountable(t)
	repo := initRepo(t)
	t.Setenv("ARK_REPOSITORY", repo)
	if code, _ := runCapture(t, "backup", "--name", "n", t.TempDir()); code != 0 {
		t.Fatalf("backup exit %d", code)
	}
	mnt := t.TempDir()
	done := startMount(t, mnt)
	f, err := os.Open(filepath.Join(mnt, "ids"))
	if err != nil {
		t.Fatal(err)
	}
	code := interrupt(t, done)
	_ = f.Close()
	for _, cmd := range []string{"fusermount3", "fusermount"} {
		if exec.Command(cmd, "-u", mnt).Run() == nil {
			break
		}
	}
	if code != 1 {
		t.Errorf("exit with a busy mountpoint = %d, want 1", code)
	}
}
