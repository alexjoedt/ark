package ark_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkmem"
)

func countMarkers(t *testing.T, b ark.Backend) int {
	t.Helper()
	n := 0
	if err := b.List(context.Background(), ark.MarkerFile, func(ark.BlobInfo) error { n++; return nil }); err != nil {
		t.Fatalf("List markers: %v", err)
	}
	return n
}

// deadPID returns the pid of a process of this host that has exited.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run child: %v", err)
	}
	return cmd.Process.Pid
}

// putMarker stores a marker under name as a backup of process pid writes it.
func putMarker(t *testing.T, repo *ark.Repository, typ ark.HandleType, name string, pid int) {
	t.Helper()
	host, _ := os.Hostname()
	data, err := json.Marshal(map[string]any{"time": time.Now().UTC(), "hostname": host, "pid": pid})
	if err != nil {
		t.Fatal(err)
	}
	if err := ark.PutObject(context.Background(), repo, ark.Handle{Type: typ, Name: name}, data); err != nil {
		t.Fatalf("put %s %s: %v", typ, name, err)
	}
}

// A backup that died before its barrier can leave a chunk torn under its full
// name. Its marker makes the next backup store the chunk again instead of
// trusting Exists; without the marker the torn chunk is reused.
func TestBackupAfterUncleanEndRewritesTornChunks(t *testing.T) {
	for _, marked := range []bool{true, false} {
		name := "marker"
		if !marked {
			name = "control"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			b := arkmem.New()
			repo := openRepo(t, b)
			src := stubSource{name: "app", content: "content the crashed backup stored"}

			crashed, err := repo.Backup(ctx, src)
			if err != nil {
				t.Fatalf("first backup: %v", err)
			}
			if n := countMarkers(t, b); n != 0 {
				t.Fatalf("finished backup left %d markers", n)
			}
			var chunks []string
			_ = b.List(ctx, ark.BlobFile, func(info ark.BlobInfo) error {
				chunks = append(chunks, info.Name)
				return nil
			})
			for _, c := range chunks {
				if err := b.Put(ctx, ark.Handle{Type: ark.BlobFile, Name: c}, bytes.NewReader(nil)); err != nil {
					t.Fatal(err)
				}
			}
			// The crash: no manifest, a torn chunk and, with marked, the marker.
			forget := ark.SnapshotFilter{IDs: []string{crashed.ID}}
			if _, err := repo.Forget(ctx, ark.ForgetPolicy{}, forget); err != nil {
				t.Fatalf("Forget: %v", err)
			}
			if marked {
				putMarker(t, repo, ark.MarkerFile, "01UNCLEAN", deadPID(t))
				res, err := repo.Check(ctx)
				if err != nil || len(res.Errors) != 0 || len(res.Warnings) != 1 ||
					!strings.Contains(res.Warnings[0], "unclean backup 01UNCLEAN") {
					t.Fatalf("Check = %+v, %v; want one unclean backup warning", res, err)
				}
			}

			snap, err := repo.Backup(ctx, src)
			if err != nil {
				t.Fatalf("second backup: %v", err)
			}
			sink := &recordingSink{}
			_, err = repo.Restore(ctx, snap.ID, sink)
			if !marked {
				if err == nil && sink.written["data.txt"] == src.content {
					t.Fatal("control: the torn chunk was not reused, the test would not catch the reuse")
				}
				return
			}
			if err != nil || sink.written["data.txt"] != src.content {
				t.Fatalf("Restore = %q, %v; want the source content", sink.written["data.txt"], err)
			}
			if n := countMarkers(t, b); n != 1 {
				t.Fatalf("%d markers before prune, want the stale one", n)
			}

			if _, err := repo.Prune(ctx); err != nil {
				t.Fatalf("Prune: %v", err)
			}
			if n := countMarkers(t, b); n != 0 {
				t.Fatalf("Prune left %d markers", n)
			}
			res, err := repo.Check(ctx)
			if err != nil || len(res.Errors) != 0 || len(res.Warnings) != 0 {
				t.Fatalf("Check after prune = %+v, %v; want clean", res, err)
			}
		})
	}
}

// A marker whose lock is live belongs to a running backup and is no finding.
func TestMarkerWithLiveLockIsNotStale(t *testing.T) {
	ctx := context.Background()
	b := arkmem.New()
	repo := openRepo(t, b)

	putMarker(t, repo, ark.MarkerFile, "01RUNNING", os.Getpid())
	putMarker(t, repo, ark.LockFile, "01RUNNING", os.Getpid())
	res, err := repo.Check(ctx)
	if err != nil || len(res.Warnings) != 0 {
		t.Fatalf("Check with a live lock = %+v, %v; want no warning", res, err)
	}

	if err := b.Delete(ctx, ark.Handle{Type: ark.LockFile, Name: "01RUNNING"}); err != nil {
		t.Fatal(err)
	}
	res, err = repo.Check(ctx)
	if err != nil || len(res.Warnings) != 1 {
		t.Fatalf("Check without the lock = %+v, %v; want one warning", res, err)
	}
}
