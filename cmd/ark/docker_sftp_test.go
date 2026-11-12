package main

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"path"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ark "github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkdocker"
	"github.com/alexjoedt/ark/arkuri"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	dockerclient "github.com/moby/moby/client"
)

// The sftp runs of the volume tests live here, not in arkdocker: arkuri pulls
// pkg/sftp, which arkdocker's module must not require, and a package shared
// by both modules cannot work either: cmd/ark pins a released arkdocker, which
// has no such package, so go mod tidy would fail outside the workspace. The
// scenarios below are therefore a copy of TestVolumeRoundTrip and
// TestVolumeRestoresAfterARepack in arkdocker/engine_test.go, which runs them
// on arkmem and arklocal. Keep the two in step.

// sftpVolumeEngine returns an engine over a fresh repository on the sftp server
// ARK_SFTP_TEST_URI names. The repository's files are deleted at the end.
func sftpVolumeEngine(t *testing.T) (*volumeDocker, *arkdocker.Engine, *ark.Repository) {
	t.Helper()
	env := os.Getenv(envSFTPTestURI)
	if env == "" {
		t.Skipf("%s is not set", envSFTPTestURI)
	}
	d := newVolumeDocker(t)

	u, err := url.Parse(env)
	if err != nil {
		t.Fatalf("parse %s: %v", envSFTPTestURI, err)
	}
	u.Path = path.Join(u.Path, fmt.Sprintf("volume-%d", time.Now().UnixNano()))
	ctx := context.Background()
	b, err := arkuri.Open(ctx, u.String())
	if err != nil {
		t.Fatalf("open %s: %v", u.Redacted(), err)
	}
	var (
		repo *ark.Repository
		e    *arkdocker.Engine
	)
	// Closing the repository closes b, so the files go first. Empty
	// directories stay on the server.
	t.Cleanup(func() {
		if e != nil {
			e.Close()
		}
		for _, typ := range []ark.HandleType{
			ark.ConfigFile, ark.KeyFile, ark.SnapshotFile, ark.LockFile,
			ark.DataPackFile, ark.TreePackFile, ark.DataIndexFile, ark.TreeIndexFile,
		} {
			var names []string
			_ = b.List(ctx, typ, func(info ark.BlobInfo) error {
				names = append(names, info.Name)
				return nil
			})
			for _, name := range names {
				_ = b.Delete(ctx, ark.Handle{Type: typ, Name: name})
			}
		}
		if repo != nil {
			_ = repo.Close()
		} else {
			_ = b.Close()
		}
	})
	if err := ark.Init(ctx, b, ark.WithoutPassword()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if repo, err = ark.Open(ctx, b, ark.WithoutPassword()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	// The sleep container ignores SIGTERM, a short timeout keeps the kill quick.
	if e, err = arkdocker.New(repo, arkdocker.WithStopTimeout(time.Second)); err != nil {
		t.Fatalf("arkdocker.New: %v", err)
	}
	return d, e, repo
}

// fixture is what the round trip carries: files, a directory, a hardlink and
// a symlink, with a private mode on one file.
const fixture = "mkdir -p /data/d && echo hello > /data/d/a.txt && printf 'bin\\0\\377' > /data/b.bin && " +
	"chmod 600 /data/b.bin && ln /data/d/a.txt /data/hard.txt && ln -s d/a.txt /data/soft"

// listing prints every entry with type, mode and link count, the file
// hashes and the symlink targets, so two volumes compare as strings.
const listing = "cd /data && find . -mindepth 1 | sort | while read -r p; do echo \"$p $(stat -c '%F %a %h' \"$p\")\"; done; " +
	"find . -type f | sort | xargs md5sum; for l in $(find . -type l | sort); do echo \"$l -> $(readlink \"$l\")\"; done"

// volumeDocker is the test's connection to the daemon.
type volumeDocker struct {
	cli   *dockerclient.Client
	image string
}

// newVolumeDocker connects to the daemon the environment names, the one
// arkdocker.New reaches, and skips the test when there is none.
func newVolumeDocker(t *testing.T) *volumeDocker {
	t.Helper()
	cli, err := dockerclient.New(dockerclient.FromEnv)
	if err != nil {
		t.Skipf("no docker client: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := cli.Ping(ctx, dockerclient.PingOptions{}); err != nil {
		cli.Close()
		t.Skipf("no docker daemon: %v", err)
	}
	t.Cleanup(func() { cli.Close() })
	return &volumeDocker{cli: cli, image: arkdocker.DefaultHelperImage}
}

// volume creates an empty volume that is removed when the test ends.
func (d *volumeDocker) volume(t *testing.T) string {
	t.Helper()
	name := fmt.Sprintf("ark-test-%d", time.Now().UnixNano())
	if _, err := d.cli.VolumeCreate(context.Background(), dockerclient.VolumeCreateOptions{Name: name}); err != nil {
		t.Fatalf("create volume: %v", err)
	}
	t.Cleanup(func() { d.removeVolume(name) })
	return name
}

func (d *volumeDocker) removeVolume(name string) error {
	_, err := d.cli.VolumeRemove(context.Background(), name, dockerclient.VolumeRemoveOptions{Force: true})
	return err
}

func (d *volumeDocker) ensureImage(ctx context.Context) error {
	if _, err := d.cli.ImageInspect(ctx, d.image); err == nil {
		return nil
	}
	r, err := d.cli.ImagePull(ctx, d.image, dockerclient.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("pull %s: %w", d.image, err)
	}
	defer r.Close()
	return r.Wait(ctx)
}

func (d *volumeDocker) removeContainer(id string) {
	_, _ = d.cli.ContainerRemove(context.Background(), id, dockerclient.ContainerRemoveOptions{Force: true})
}

// run executes script in a throw-away container with the volume at /data and
// returns its output.
func (d *volumeDocker) run(t *testing.T, volume, script string, readOnly bool) string {
	t.Helper()
	ctx := context.Background()
	if err := d.ensureImage(ctx); err != nil {
		t.Fatalf("%v", err)
	}
	resp, err := d.cli.ContainerCreate(ctx, dockerclient.ContainerCreateOptions{
		Config: &container.Config{Image: d.image, Cmd: []string{"sh", "-c", script}},
		HostConfig: &container.HostConfig{
			Mounts: []mount.Mount{{Type: mount.TypeVolume, Source: volume, Target: "/data", ReadOnly: readOnly}},
		},
	})
	if err != nil {
		t.Fatalf("create container: %v", err)
	}
	defer d.removeContainer(resp.ID)
	if _, err := d.cli.ContainerStart(ctx, resp.ID, dockerclient.ContainerStartOptions{}); err != nil {
		t.Fatalf("start container: %v", err)
	}
	wait := d.cli.ContainerWait(ctx, resp.ID, dockerclient.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	var code int64
	select {
	case err := <-wait.Error:
		if err != nil {
			t.Fatalf("wait: %v", err)
		}
	case status := <-wait.Result:
		code = status.StatusCode
	}
	logs, err := d.cli.ContainerLogs(ctx, resp.ID, dockerclient.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	defer logs.Close()
	var out bytes.Buffer
	if _, err := stdcopy.StdCopy(&out, &out, logs); err != nil {
		t.Fatalf("logs: %v", err)
	}
	if code != 0 {
		t.Fatalf("%s: exit %d\n%s", script, code, out.String())
	}
	return out.String()
}

func (d *volumeDocker) listing(t *testing.T, volume string) string {
	t.Helper()
	return d.run(t, volume, listing, true)
}

// sleeper starts a plain container that keeps the volume in use and returns
// its ID. It is not a helper, so the engine counts it.
func (d *volumeDocker) sleeper(t *testing.T, volume string) string {
	t.Helper()
	ctx := context.Background()
	if err := d.ensureImage(ctx); err != nil {
		t.Fatalf("%v", err)
	}
	resp, err := d.cli.ContainerCreate(ctx, dockerclient.ContainerCreateOptions{
		Config: &container.Config{Image: d.image, Cmd: []string{"sleep", "300"}},
		HostConfig: &container.HostConfig{
			Mounts: []mount.Mount{{Type: mount.TypeVolume, Source: volume, Target: "/data"}},
		},
	})
	if err != nil {
		t.Fatalf("create sleeper: %v", err)
	}
	t.Cleanup(func() { d.removeContainer(resp.ID) })
	if _, err := d.cli.ContainerStart(ctx, resp.ID, dockerclient.ContainerStartOptions{}); err != nil {
		t.Fatalf("start sleeper: %v", err)
	}
	return resp.ID
}

func (d *volumeDocker) inspect(t *testing.T, id string) container.InspectResponse {
	t.Helper()
	resp, err := d.cli.ContainerInspect(context.Background(), id, dockerclient.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("inspect %s: %v", id[:12], err)
	}
	return resp.Container
}

// watchStopped polls the container until stop is called and reports
// whether it was ever seen not running.
func (d *volumeDocker) watchStopped(t *testing.T, id string) (sawStopped *atomic.Bool, stop func()) {
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
				resp, err := d.cli.ContainerInspect(context.Background(), id, dockerclient.ContainerInspectOptions{})
				if err == nil && resp.Container.State.Status != container.StateRunning {
					sawStopped.Store(true)
				}
			}
		}
	}()
	return sawStopped, func() { close(done); <-finished }
}

// volumeRoundTrip backs a volume up while a container uses it, restores it into a
// removed volume and then over a volume in use, and compares the listings.
func volumeRoundTrip(t *testing.T, d *volumeDocker, e *arkdocker.Engine) {
	t.Helper()
	ctx := context.Background()
	vol := d.volume(t)

	d.run(t, vol, fixture, false)
	want := d.listing(t, vol)
	if !strings.Contains(want, "./hard.txt regular file 644 2") {
		t.Fatalf("fixture has no hardlink pair:\n%s", want)
	}

	user := d.sleeper(t, vol)
	sawStopped, stopWatching := d.watchStopped(t, user)
	snap, err := e.BackupVolume(ctx, vol)
	stopWatching()
	if err != nil {
		t.Fatalf("BackupVolume: %v", err)
	}
	if !sawStopped.Load() {
		t.Error("the container using the volume was never stopped during the backup")
	}
	if got := d.inspect(t, user).State.Status; got != container.StateRunning {
		t.Errorf("the container is %q after the backup, want running", got)
	}
	if snap.SourceName != vol || snap.SourceType != arkdocker.SourceTypeDockerVolume {
		t.Errorf("snapshot source is %s/%s, want %s/%s", snap.SourceType, snap.SourceName, arkdocker.SourceTypeDockerVolume, vol)
	}

	ctr := d.inspect(t, user)
	for key, want := range map[string]string{
		"container.0.name":     strings.TrimPrefix(ctr.Name, "/"),
		"container.0.image":    d.image,
		"container.0.image_id": ctr.Image,
		"container.0.state":    "running",
	} {
		if got := snap.SourceMeta[key]; got == "" || got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}

	// Drop the volume, a restore has to create it again.
	d.removeContainer(user)
	if err := d.removeVolume(vol); err != nil {
		t.Fatalf("remove volume: %v", err)
	}
	if _, err := e.RestoreVolume(ctx, snap.ID, vol); err != nil {
		t.Fatalf("RestoreVolume into the removed volume: %v", err)
	}
	if got := d.listing(t, vol); got != want {
		t.Errorf("restored volume differs\n--- want\n%s--- got\n%s", want, got)
	}

	// Restore over a volume in use: contents replaced, container back.
	d.run(t, vol, "echo stale > /data/stale.txt && rm /data/soft", false)
	user = d.sleeper(t, vol)
	sawStopped, stopWatching = d.watchStopped(t, user)
	_, err = e.RestoreVolume(ctx, snap.ID, vol)
	stopWatching()
	if err != nil {
		t.Fatalf("RestoreVolume over the used volume: %v", err)
	}
	if !sawStopped.Load() {
		t.Error("the container using the volume was never stopped during the restore")
	}
	if got := d.inspect(t, user).State.Status; got != container.StateRunning {
		t.Errorf("the container is %q after the restore, want running", got)
	}
	if got := d.listing(t, vol); got != want {
		t.Errorf("second restore differs\n--- want\n%s--- got\n%s", want, got)
	}
}

// volumeRestoresAfterARepack backs a volume up twice with one directory changed in
// between, forgets the first snapshot and prunes. The first backup stored the
// nodes of both directories in one tree pack and only one of them is still
// referenced, so the prune copies it into a new pack and deletes the old one.
// The second snapshot then restores into a volume that is gone.
func volumeRestoresAfterARepack(t *testing.T, d *volumeDocker, e *arkdocker.Engine, repo *ark.Repository) {
	t.Helper()
	ctx := context.Background()
	vol := d.volume(t)

	d.run(t, vol, fixture+" && mkdir /data/keep && echo stays > /data/keep/k.txt", false)
	first, err := e.BackupVolume(ctx, vol)
	if err != nil {
		t.Fatalf("first BackupVolume: %v", err)
	}
	d.run(t, vol, "echo changed > /data/d/a.txt && echo new > /data/d/n.txt && rm /data/soft", false)
	want := d.listing(t, vol)
	second, err := e.BackupVolume(ctx, vol)
	if err != nil {
		t.Fatalf("second BackupVolume: %v", err)
	}

	if _, err := repo.Forget(ctx, ark.ForgetPolicy{}, ark.SnapshotFilter{IDs: []string{first.ID}}); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	pruned, err := repo.Prune(ctx)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if pruned.BytesRepacked == 0 || pruned.PacksDeleted == 0 {
		t.Fatalf("Prune = %+v, want a pack repacked and the old one deleted", pruned)
	}
	if check, err := repo.Check(ctx); err != nil || len(check.Errors) != 0 {
		t.Fatalf("Check after the prune = %+v, %v", check, err)
	}

	if err := d.removeVolume(vol); err != nil {
		t.Fatalf("remove volume: %v", err)
	}
	if _, err := e.RestoreVolume(ctx, second.ID, vol); err != nil {
		t.Fatalf("RestoreVolume into the removed volume: %v", err)
	}
	if got := d.listing(t, vol); got != want {
		t.Errorf("restored volume differs\n--- want\n%s--- got\n%s", want, got)
	}
}

func TestVolumeRoundTripOnSftp(t *testing.T) {
	d, e, _ := sftpVolumeEngine(t)
	volumeRoundTrip(t, d, e)
}

func TestVolumeRestoresAfterARepackOnSftp(t *testing.T) {
	d, e, repo := sftpVolumeEngine(t)
	volumeRestoresAfterARepack(t, d, e, repo)
}
