package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	ark "github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkdocker"
	"github.com/alexjoedt/ark/arklocal"
)

type typedSource struct {
	typ ark.SourceType
}

func (s typedSource) Meta() ark.SourceMeta { return ark.SourceMeta{Name: "pgdata", Type: s.typ} }

func (typedSource) Walk(ctx context.Context, fn ark.WalkFunc) error {
	return volumeSource{}.Walk(ctx, fn)
}

func seriesSnapshot(t *testing.T, repo string, typ ark.SourceType, host string, age time.Duration) string {
	t.Helper()
	b, err := arklocal.New(repo)
	if err != nil {
		t.Fatal(err)
	}
	r, err := ark.Open(context.Background(), b, ark.NoPassword())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	s, err := r.Backup(context.Background(), typedSource{typ}, ark.WithHost(host), ark.WithTime(time.Now().Add(-age)))
	if err != nil {
		t.Fatal(err)
	}
	return string(s.ID)
}

// A volume profile selects the volume series of its host, not a file series
// of the same name or the volume on another host.
func TestVolumeProfileSelectsItsSeries(t *testing.T) {
	dir := profileDirFor(t)
	repo := initRepo(t)
	t.Setenv("ARK_REPOSITORY", "")
	t.Setenv("DOCKER_HOST", "")
	vol := arkdocker.SourceTypeDockerVolume
	old := seriesSnapshot(t, repo, vol, "dbhost", 2*time.Hour)
	seriesSnapshot(t, repo, vol, "dbhost", time.Hour)
	other := seriesSnapshot(t, repo, vol, "otherhost", 3*time.Hour)
	file := seriesSnapshot(t, repo, ark.SourceTypeFile, "dbhost", 4*time.Hour)
	seriesSnapshot(t, repo, ark.SourceTypeFile, "dbhost", time.Hour)
	writeProfile(t, dir, "db", "repo: "+repo+"\nbackup:\n  volume: pgdata\n  host: dbhost\nforget:\n  keep-last: 1\n")

	code, out := runCapture(t, "--json", "snapshots", "-P", "db")
	if code != 0 || strings.Contains(out, file) || !strings.Contains(out, other) || !strings.Contains(out, old) {
		t.Fatalf("snapshots -P db: exit %d, %s", code, out)
	}
	code, out = runCapture(t, "--json", "find", "-P", "db", "pg.conf")
	if code != 0 || strings.Contains(out, file) {
		t.Fatalf("find -P db: exit %d, %s", code, out)
	}
	code, out = runCapture(t, "--json", "forget", "-P", "db", "--dry-run")
	if code != 0 {
		t.Fatalf("forget -P db: exit %d, %s", code, out)
	}
	var res struct {
		Remove []struct {
			ID string `json:"id"`
		} `json:"remove"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || len(res.Remove) != 1 || res.Remove[0].ID != old {
		t.Fatalf("forget -P db removes %+v, want only %s: %s (%v)", res.Remove, old, out, err)
	}
}

// Without backup.host a remote $DOCKER_HOST files the volume under the
// daemon's name, so forget has to ask the daemon.
func TestVolumeProfileForgetAsksARemoteDaemon(t *testing.T) {
	dir := profileDirFor(t)
	repo := initRepo(t)
	t.Setenv("ARK_REPOSITORY", "")
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1")
	writeProfile(t, dir, "db", "repo: "+repo+"\nbackup:\n  volume: pgdata\nforget:\n  keep-last: 1\n")
	code, _, o := runCaptureAll(t, "forget", "-P", "db", "--dry-run")
	if code != 1 || !strings.Contains(o, "127.0.0.1:1") {
		t.Fatalf("exit %d, output %q: want exit 1 naming the daemon address", code, o)
	}
}
