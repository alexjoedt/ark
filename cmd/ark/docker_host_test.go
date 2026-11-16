package main

import (
	"encoding/json"
	"testing"
	"time"

	ark "github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkdocker"
	"github.com/alexjoedt/ark/cmd/ark/internal/out"
)

func TestLastVolumeSnapshotsAreOfOneHost(t *testing.T) {
	at := func(m int) time.Time { return time.Date(2026, 1, 1, 0, m, 0, 0, time.UTC) }
	snap := func(typ ark.SourceType, name, host string, m int) *ark.Snapshot {
		return &ark.Snapshot{SourceType: typ, SourceName: name, Host: host, CreatedAt: at(m)}
	}
	snaps := []*ark.Snapshot{
		snap(arkdocker.SourceTypeDockerVolume, "pg", "b", 9),
		snap(arkdocker.SourceTypeDockerVolume, "pg", "a", 5),
		snap(ark.SourceType("file"), "web", "a", 4),
		snap(arkdocker.SourceTypeDockerVolume, "pg", "a", 1),
	}
	got := lastVolumeSnapshots(snaps, "a")
	if len(got) != 1 || !got["pg"].Equal(at(5)) {
		t.Errorf("host a: got %v, want pg at minute 5 only", got)
	}
	if got := lastVolumeSnapshots(snaps, "c"); len(got) != 0 {
		t.Errorf("host c: got %v, want none", got)
	}
}

// Each --host has its own series of a volume: the parent is that host's
// previous snapshot, a new host has none.
func TestDockerBackupRecordsTheHostAndParentsPerHost(t *testing.T) {
	d := newVolumeDocker(t)
	repo := initRepo(t)
	vol := d.volume(t)
	d.run(t, vol, "echo hi > /data/a", false)

	backup := func(host string) out.Snapshot {
		t.Helper()
		code, o := runCapture(t, "--json", "-r", repo, "docker", "backup", "--host", host, vol)
		if code != 0 {
			t.Fatalf("docker backup --host %s: exit %d: %s", host, code, o)
		}
		var s out.Snapshot
		if err := json.Unmarshal([]byte(o), &s); err != nil {
			t.Fatalf("decode %q: %v", o, err)
		}
		if s.Host != host {
			t.Fatalf("recorded host %q, want %q", s.Host, host)
		}
		return s
	}
	a1 := backup("a")
	b1 := backup("b")
	a2 := backup("a")
	c1 := backup("c")
	if a1.ParentID != "" || b1.ParentID != "" || c1.ParentID != "" {
		t.Errorf("first backup of a host has a parent: a %q b %q c %q", a1.ParentID, b1.ParentID, c1.ParentID)
	}
	if a2.ParentID != a1.ID {
		t.Errorf("second backup on a: parent %q, want %q", a2.ParentID, a1.ID)
	}
}
