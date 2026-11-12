package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path"
	"testing"
	"time"

	ark "github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkdocker"
	"github.com/alexjoedt/ark/arkuri"
)

// The sftp runs of the volume tests live here, not in arkdocker: arkuri pulls
// pkg/sftp, which arkdocker's module must not require. arkdocker runs the same
// scenarios on arkmem and arklocal.

const (
	volumeFixture = "mkdir -p /data/d && echo hello > /data/d/a.txt && printf 'bin\\0\\377' > /data/b.bin && " +
		"chmod 600 /data/b.bin && ln /data/d/a.txt /data/hard.txt && ln -s d/a.txt /data/soft"
	volumeListing = "cd /data && find . -mindepth 1 | sort | while read -r p; do echo \"$p $(stat -c '%F %a %h' \"$p\")\"; done; " +
		"find . -type f | sort | xargs md5sum; for l in $(find . -type l | sort); do echo \"$l -> $(readlink \"$l\")\"; done"
)

func inVolume(t *testing.T, vol, script string) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "docker", "run", "--rm", "-v", vol+":/data",
		arkdocker.DefaultHelperImage, "sh", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", script, err, out)
	}
	return string(out)
}

func sftpVolumeEngine(t *testing.T) (*arkdocker.Engine, *ark.Repository, string) {
	t.Helper()
	env := os.Getenv(envSFTPTestURI)
	if env == "" {
		t.Skipf("%s is not set", envSFTPTestURI)
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("no docker CLI: %v", err)
	}
	c, err := arkdocker.NewClient("")
	if err != nil {
		t.Skipf("no docker client: %v", err)
	}
	pingCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := c.Ping(pingCtx); err != nil {
		t.Skipf("no docker daemon: %v", err)
	}
	c.Close()

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
	// Empty directories stay on the server.
	t.Cleanup(func() {
		defer func() { _ = b.Close() }()
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
	})
	if err := ark.Init(ctx, b, ark.WithoutPassword()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	repo, err := ark.Open(ctx, b, ark.WithoutPassword())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	e, err := arkdocker.New(repo, arkdocker.WithStopTimeout(time.Second))
	if err != nil {
		t.Fatalf("arkdocker.New: %v", err)
	}
	t.Cleanup(func() { e.Close(); _ = repo.Close() })

	vol := fmt.Sprintf("ark-cli-sftp-%d", time.Now().UnixNano())
	if out, err := exec.CommandContext(ctx, "docker", "volume", "create", vol).CombinedOutput(); err != nil {
		t.Fatalf("create volume: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "volume", "rm", "-f", vol).Run() })
	return e, repo, vol
}

func removeVolume(t *testing.T, vol string) {
	t.Helper()
	if out, err := exec.CommandContext(t.Context(), "docker", "volume", "rm", "-f", vol).CombinedOutput(); err != nil {
		t.Fatalf("remove volume: %v\n%s", err, out)
	}
}

func TestVolumeRoundTripOnSftp(t *testing.T) {
	e, _, vol := sftpVolumeEngine(t)
	ctx := t.Context()

	inVolume(t, vol, volumeFixture)
	want := inVolume(t, vol, volumeListing)
	snap, err := e.BackupVolume(ctx, vol)
	if err != nil {
		t.Fatalf("BackupVolume: %v", err)
	}
	if snap.SourceName != vol || snap.SourceType != arkdocker.SourceTypeDockerVolume {
		t.Errorf("snapshot source is %s/%s", snap.SourceType, snap.SourceName)
	}

	removeVolume(t, vol)
	if _, err := e.RestoreVolume(ctx, snap.ID, vol); err != nil {
		t.Fatalf("RestoreVolume into the removed volume: %v", err)
	}
	if got := inVolume(t, vol, volumeListing); got != want {
		t.Errorf("restored volume differs\n--- want\n%s--- got\n%s", want, got)
	}
}

func TestVolumeRestoresAfterARepackOnSftp(t *testing.T) {
	e, repo, vol := sftpVolumeEngine(t)
	ctx := t.Context()

	inVolume(t, vol, volumeFixture+" && mkdir /data/keep && echo stays > /data/keep/k.txt")
	first, err := e.BackupVolume(ctx, vol)
	if err != nil {
		t.Fatalf("first BackupVolume: %v", err)
	}
	inVolume(t, vol, "echo changed > /data/d/a.txt && echo new > /data/d/n.txt && rm /data/soft")
	want := inVolume(t, vol, volumeListing)
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

	removeVolume(t, vol)
	if _, err := e.RestoreVolume(ctx, second.ID, vol); err != nil {
		t.Fatalf("RestoreVolume into the removed volume: %v", err)
	}
	if got := inVolume(t, vol, volumeListing); got != want {
		t.Errorf("restored volume differs\n--- want\n%s--- got\n%s", want, got)
	}
}
