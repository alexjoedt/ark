package arkdocker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkfs"
	"github.com/alexjoedt/ark/arkmem"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	dockerclient "github.com/moby/moby/client"
)

func testRepo(t *testing.T) *ark.Repository {
	t.Helper()
	ctx := context.Background()
	b := arkmem.New()
	if err := ark.Init(ctx, b, ark.WithoutPassword()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	repo, err := ark.Open(ctx, b, ark.WithoutPassword())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return repo
}

func testEngine(t *testing.T, opts ...Option) *Engine {
	t.Helper()
	// The sleep container ignores SIGTERM, a short timeout keeps the kill quick.
	e, err := New(testRepo(t), append([]Option{WithStopTimeout(time.Second)}, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}

// fixture is what the round trip carries: files, a directory, a hardlink and
// a symlink, with a private mode on one file.
const fixture = "mkdir -p /data/d && echo hello > /data/d/a.txt && printf 'bin\\0\\377' > /data/b.bin && " +
	"chmod 600 /data/b.bin && ln /data/d/a.txt /data/hard.txt && ln -s d/a.txt /data/soft"

// listing prints every entry with type, mode and link count, the file
// hashes and the symlink targets, so two volumes compare as strings.
const listing = "cd /data && find . -mindepth 1 | sort | while read -r p; do echo \"$p $(stat -c '%F %a %h' \"$p\")\"; done; " +
	"find . -type f | sort | xargs md5sum; for l in $(find . -type l | sort); do echo \"$l -> $(readlink \"$l\")\"; done"

func volumeListing(t *testing.T, c *Client, volume string) string {
	t.Helper()
	ctx := context.Background()
	out, err := c.runContainer(
		ctx,
		c.helperConfig("sh", "-c", listing),
		&container.HostConfig{
			Mounts: []mount.Mount{{Type: mount.TypeVolume, Source: volume, Target: "/data", ReadOnly: true}},
		},
	)
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	return out
}

// sleeper starts a plain container that keeps the volume in use and returns
// its ID. It is not a helper, so the engine counts it.
func sleeper(t *testing.T, c *Client, volume string) string {
	t.Helper()
	ctx := context.Background()
	resp, err := c.cli.ContainerCreate(ctx, dockerclient.ContainerCreateOptions{
		Config: &container.Config{Image: c.helperImage, Cmd: []string{"sleep", "300"}},
		HostConfig: &container.HostConfig{
			Mounts: []mount.Mount{{Type: mount.TypeVolume, Source: volume, Target: "/data"}},
		},
	})
	if err != nil {
		t.Fatalf("create sleeper: %v", err)
	}
	t.Cleanup(func() { c.removeContainer(resp.ID) })
	if _, err := c.cli.ContainerStart(ctx, resp.ID, dockerclient.ContainerStartOptions{}); err != nil {
		t.Fatalf("start sleeper: %v", err)
	}
	return resp.ID
}

func containerState(t *testing.T, c *Client, id string) string {
	t.Helper()
	info, err := c.GetContainer(context.Background(), id)
	if err != nil {
		t.Fatalf("inspect %s: %v", id[:12], err)
	}
	return info.State
}

// watchStopped polls the container until stop is called and reports
// whether it was ever seen not running.
func watchStopped(c *Client, id string) (sawStopped *atomic.Bool, stop func()) {
	sawStopped = new(atomic.Bool)
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for {
			select {
			case <-done:
				return
			case <-time.After(50 * time.Millisecond):
				info, err := c.GetContainer(context.Background(), id)
				if err == nil && info.State != string(container.StateRunning) {
					sawStopped.Store(true)
				}
			}
		}
	}()
	return sawStopped, func() { close(done); <-finished }
}

func TestVolumeRoundTrip(t *testing.T) {
	c := testClient(t)
	e := testEngine(t)
	ctx := context.Background()
	vol := testVolume(t, c)

	if err := runInVolume(ctx, c, vol, []string{"sh", "-c", fixture}); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	want := volumeListing(t, c, vol)
	if !strings.Contains(want, "./hard.txt regular file 644 2") {
		t.Fatalf("fixture has no hardlink pair:\n%s", want)
	}

	user := sleeper(t, c, vol)
	sawStopped, stopWatching := watchStopped(c, user)
	snap, err := e.BackupVolume(ctx, vol)
	stopWatching()
	if err != nil {
		t.Fatalf("BackupVolume: %v", err)
	}
	if !sawStopped.Load() {
		t.Error("the container using the volume was never stopped during the backup")
	}
	if got := containerState(t, c, user); got != string(container.StateRunning) {
		t.Errorf("the container is %q after the backup, want running", got)
	}
	if snap.SourceName != vol || snap.SourceType != SourceTypeDockerVolume {
		t.Errorf("snapshot source is %s/%s, want %s/%s", snap.SourceType, snap.SourceName, SourceTypeDockerVolume, vol)
	}

	ctr, err := c.GetContainer(ctx, user)
	if err != nil {
		t.Fatalf("inspect the container: %v", err)
	}
	for key, want := range map[string]string{
		"container.0.name":     ctr.Name,
		"container.0.image":    imageReference(c.helperImage),
		"container.0.image_id": ctr.ImageID,
		"container.0.state":    "running",
	} {
		if got := snap.SourceMeta[key]; got == "" || got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}

	// Drop the volume, a restore has to create it again.
	c.removeContainer(user)
	if _, err := c.cli.VolumeRemove(ctx, vol, dockerclient.VolumeRemoveOptions{Force: true}); err != nil {
		t.Fatalf("remove volume: %v", err)
	}
	if _, err := e.RestoreVolume(ctx, snap.ID, vol); err != nil {
		t.Fatalf("RestoreVolume into the removed volume: %v", err)
	}
	if got := volumeListing(t, c, vol); got != want {
		t.Errorf("restored volume differs\n--- want\n%s--- got\n%s", want, got)
	}

	// Restore over a volume in use: contents replaced, container back.
	if err := runInVolume(
		ctx,
		c,
		vol,
		[]string{"sh", "-c", "echo stale > /data/stale.txt && rm /data/soft"},
	); err != nil {
		t.Fatalf("dirty the volume: %v", err)
	}
	user = sleeper(t, c, vol)
	sawStopped, stopWatching = watchStopped(c, user)
	_, err = e.RestoreVolume(ctx, snap.ID, vol)
	stopWatching()
	if err != nil {
		t.Fatalf("RestoreVolume over the used volume: %v", err)
	}
	if !sawStopped.Load() {
		t.Error("the container using the volume was never stopped during the restore")
	}
	if got := containerState(t, c, user); got != string(container.StateRunning) {
		t.Errorf("the container is %q after the restore, want running", got)
	}
	if got := volumeListing(t, c, vol); got != want {
		t.Errorf("second restore differs\n--- want\n%s--- got\n%s", want, got)
	}
}

func TestBackupVolumeNamesTheMissingVolume(t *testing.T) {
	testClient(t)
	e := testEngine(t)

	_, err := e.BackupVolume(context.Background(), "ark-test-does-not-exist")
	if err == nil || !strings.Contains(err.Error(), "ark-test-does-not-exist") {
		t.Errorf("got %v, want an error naming the volume", err)
	}
}

func TestRestoreVolumeRejectsAFileSnapshot(t *testing.T) {
	c := testClient(t)
	e := testEngine(t)
	ctx := context.Background()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("file"), 0o644); err != nil {
		t.Fatal(err)
	}
	snap, err := arkfs.Backup(ctx, e.repo, dir)
	if err != nil {
		t.Fatalf("arkfs.Backup: %v", err)
	}

	vol := testVolume(t, c)
	_, err = e.RestoreVolume(ctx, snap.ID, vol)
	if !errors.Is(err, ark.ErrIncompatibleSourceType) {
		t.Errorf("got %v, want ErrIncompatibleSourceType", err)
	}
}
