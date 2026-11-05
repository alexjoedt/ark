package ark_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkfs"
	"github.com/alexjoedt/ark/arklocal"
	"github.com/alexjoedt/ark/chunker"
)

// The kill harness backs up a changing tree into an arklocal repository in a
// child process, the re-exec'd test binary, and SIGKILLs it after a random
// delay. After every kill the repository must pass Check, every snapshot
// must restore to the tree it was taken of, and a backup that finished must
// have left its snapshot. Restores run once per snapshot and again in a full
// pass before the repository is replaced; Check covers the rest in between.
//
//	ARK_KILL_RUNS  iterations, default 5
//	ARK_KILL_SEED  seed of the tree changes and kill delays, printed on every run
const (
	killEnvRepo = "ARK_KILL_CHILD_REPO"
	killEnvSrc  = "ARK_KILL_CHILD_SRC"
	killEnvTag  = "ARK_KILL_CHILD_TAG"
)

func TestKillHarnessBackup(t *testing.T) {
	if testing.Short() {
		t.Skip("kill harness spawns processes")
	}
	runs := killEnvInt(t, "ARK_KILL_RUNS", 5)
	seed := uint64(time.Now().UnixNano())
	if s := os.Getenv("ARK_KILL_SEED"); s != "" {
		var err error
		if seed, err = strconv.ParseUint(s, 10, 64); err != nil {
			t.Fatalf("ARK_KILL_SEED: %v", err)
		}
	}
	t.Logf("kill harness: %d runs, rerun with ARK_KILL_SEED=%d", runs, seed)

	var key [32]byte
	binary.LittleEndian.PutUint64(key[:], seed)
	bytesSrc := rand.NewChaCha8(key)
	rng := rand.New(bytesSrc)

	ctx := context.Background()
	src := t.TempDir()
	for i := range killTreeFiles {
		writeKillFile(t, bytesSrc, src, i, rng.IntN(256<<10))
	}
	want := map[string]map[string]string{} // tag -> tree the snapshot was taken of
	restored := map[string]bool{}          // snapshot ids already compared
	var full time.Duration
	kills := 0
	var repoDir string
	var b ark.Backend

	for i := range runs {
		// ponytail: Check verifies every snapshot, so a fresh repository every
		// killRepoSpan runs keeps the harness linear in runs.
		if i%killRepoSpan == 0 {
			if b != nil {
				clear(restored)
				checkKilledRepo(ctx, t, b, fmt.Sprintf("seed %d before iteration %d, full pass", seed, i),
					want, restored, false, "")
			}
			repoDir, b = newKillRepo(ctx, t)
		}
		mutateKillTree(t, rng, bytesSrc, src)
		tag := fmt.Sprintf("kill-%d", i)
		want[tag] = readTree(t, src)

		// The first backup runs to the end; the longest finished one bounds the
		// kill delays.
		var delay time.Duration
		if i > 0 {
			delay = time.Duration(rng.Int64N(int64(full)))
		}
		took, killed, out, err := runKillChild(repoDir, src, tag, delay)
		point := fmt.Sprintf("seed %d iteration %d, exited after %v", seed, i, took)
		if killed {
			kills++
			point = fmt.Sprintf("seed %d iteration %d, killed after %v", seed, i, delay)
		}
		if err != nil {
			t.Fatalf("%s: child: %v\n%s", point, err, out)
		}
		if !killed {
			full = max(full, took)
		}
		checkKilledRepo(ctx, t, b, point, want, restored, !killed, tag)
	}

	t.Logf("%d of %d runs killed before the backup finished", kills, runs)

	clear(restored)
	checkKilledRepo(ctx, t, b, fmt.Sprintf("seed %d final full pass", seed), want, restored, false, "")
}

// killRepoSpan is the number of runs that share one repository.
const killRepoSpan = 25

// newKillRepo initialises a repository in a directory that outlives a failed
// test, so the broken state can be inspected.
func newKillRepo(ctx context.Context, t *testing.T) (string, ark.Backend) {
	t.Helper()
	dir, err := os.MkdirTemp("", "ark-kill-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("repository kept at %s", dir)
			return
		}
		_ = os.RemoveAll(dir)
	})
	b, err := arklocal.New(dir)
	if err != nil {
		t.Fatalf("arklocal.New: %v", err)
	}
	cfg := chunker.Config{MinSize: 4 << 10, AvgSize: 8 << 10, MaxSize: scenarioMaxChunk}
	if err := ark.Init(ctx, b, testPassword, ark.WithChunkerConfig(cfg)); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return dir, b
}

// TestKillHarnessChild is the child process of TestKillHarnessBackup: one
// backup of the tree in killEnvSrc into killEnvRepo.
func TestKillHarnessChild(t *testing.T) {
	repoDir := os.Getenv(killEnvRepo)
	if repoDir == "" {
		t.Skip("runs only as the child of TestKillHarnessBackup")
	}
	ctx := context.Background()
	b, err := arklocal.New(repoDir)
	if err != nil {
		t.Fatalf("arklocal.New: %v", err)
	}
	repo, err := ark.Open(ctx, b, testPassword)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = repo.Close() }()
	src, err := arkfs.NewSource(os.Getenv(killEnvSrc))
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	// fd 3 is the pipe runKillChild waits on: the kill delay starts here.
	ready := os.NewFile(3, "ready")
	if _, err := ready.Write([]byte{1}); err != nil {
		t.Fatalf("signal ready: %v", err)
	}
	_ = ready.Close()
	if _, err := repo.Backup(ctx, src, ark.WithTags(os.Getenv(killEnvTag))); err != nil {
		t.Fatalf("Backup: %v", err)
	}
}

// runKillChild backs up src in a child process and kills it delay after the
// child reports it is about to call Backup; a zero delay lets it finish. took
// is the time from that report to the exit. killed is false when the child
// exited first, err is set when it exited unsuccessfully.
func runKillChild(
	repoDir, src, tag string, delay time.Duration,
) (took time.Duration, killed bool, out []byte, err error) {
	var buf bytes.Buffer
	ready, w, err := os.Pipe()
	if err != nil {
		return 0, false, nil, err
	}
	defer func() { _ = ready.Close() }()
	cmd := exec.Command(os.Args[0], "-test.run=^TestKillHarnessChild$")
	cmd.Env = append(os.Environ(), killEnvRepo+"="+repoDir, killEnvSrc+"="+src, killEnvTag+"="+tag)
	cmd.Stdout, cmd.Stderr = &buf, &buf
	cmd.ExtraFiles = []*os.File{w}
	err = cmd.Start()
	_ = w.Close()
	if err != nil {
		return 0, false, nil, err
	}
	// The read returns once the child writes its byte, or with EOF once it
	// exits without writing one.
	var b [1]byte
	if _, err := ready.Read(b[:]); err != nil {
		err = cmd.Wait()
		return 0, false, buf.Bytes(), fmt.Errorf("child exited before the backup started: %w", err)
	}
	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var timeout <-chan time.Time
	if delay > 0 {
		timeout = time.After(delay)
	}
	select {
	case err := <-done:
		return time.Since(start), false, buf.Bytes(), err
	case <-timeout:
		if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return 0, true, nil, err
		}
		<-done
		return time.Since(start), true, buf.Bytes(), nil
	}
}

// checkKilledRepo opens the repository the way a fresh process would, runs
// Check and restores every snapshot not in restored. With mustExist, a
// snapshot tagged tag has to be among them.
func checkKilledRepo(ctx context.Context, t *testing.T, b ark.Backend, point string,
	want map[string]map[string]string, restored map[string]bool, mustExist bool, tag string,
) {
	t.Helper()
	repo, err := ark.Open(ctx, b, testPassword)
	if err != nil {
		t.Fatalf("%s: Open: %v", point, err)
	}
	defer func() { _ = repo.Close() }()

	res, err := repo.Check(ctx)
	if err != nil {
		t.Fatalf("%s: Check: %v", point, err)
	}
	if len(res.Errors) > 0 {
		t.Fatalf("%s: Check found %d errors: %v", point, len(res.Errors), res.Errors)
	}

	snaps, err := repo.Snapshots(ctx, ark.SnapshotFilter{})
	if err != nil {
		t.Fatalf("%s: Snapshots: %v", point, err)
	}
	found := false
	for _, s := range snaps {
		if len(s.Tags) != 1 || want[s.Tags[0]] == nil {
			t.Fatalf("%s: snapshot %s has unknown tags %v", point, s.ID, s.Tags)
		}
		found = found || s.Tags[0] == tag
		if restored[s.ID] {
			continue
		}
		dst := t.TempDir()
		sink, err := arkfs.NewSink(dst)
		if err != nil {
			t.Fatalf("NewSink: %v", err)
		}
		if _, err := repo.Restore(ctx, s.ID, sink); err != nil {
			t.Fatalf("%s: Restore %s (%s): %v", point, s.ID, s.Tags[0], err)
		}
		if diff := diffTree(want[s.Tags[0]], readTree(t, dst)); diff != "" {
			t.Fatalf("%s: snapshot %s (%s) restores a different tree: %s", point, s.ID, s.Tags[0], diff)
		}
		restored[s.ID] = true
	}
	if mustExist && !found {
		t.Fatalf("%s: the backup finished but left no snapshot tagged %s", point, tag)
	}
}

const killTreeFiles = 24

func killFilePath(root string, i int) string {
	return filepath.Join(root, fmt.Sprintf("d%d", i%3), fmt.Sprintf("f%02d.bin", i))
}

func writeKillFile(t *testing.T, bytesSrc *rand.ChaCha8, root string, i, size int) {
	t.Helper()
	path := killFilePath(root, i)
	data := make([]byte, size)
	_, _ = bytesSrc.Read(data)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// mutateKillTree deletes, recreates or edits four random files. An edit
// replaces one 4 KiB window, so most chunks dedupe against the parent.
func mutateKillTree(t *testing.T, rng *rand.Rand, bytesSrc *rand.ChaCha8, root string) {
	t.Helper()
	for range 4 {
		i := rng.IntN(killTreeFiles)
		path := killFilePath(root, i)
		data, err := os.ReadFile(path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			writeKillFile(t, bytesSrc, root, i, rng.IntN(256<<10))
			continue
		case err != nil:
		case rng.IntN(5) == 0:
			err = os.Remove(path)
		default:
			if len(data) > 0 {
				off := rng.IntN(len(data))
				_, _ = bytesSrc.Read(data[off:min(off+4<<10, len(data))])
			}
			err = os.WriteFile(path, data, 0o644)
		}
		if err != nil {
			t.Fatalf("mutate %s: %v", path, err)
		}
	}
}

// readTree maps every path under root to its type and, for a file, the
// SHA-256 of its content.
func readTree(t *testing.T, root string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == root {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			tree[rel] = "dir"
			return nil
		}
		data, err := os.ReadFile(path)
		tree[rel] = fmt.Sprintf("file %x", sha256.Sum256(data))
		return err
	})
	if err != nil {
		t.Fatalf("read tree %s: %v", root, err)
	}
	return tree
}

// diffTree names the first path where got differs from want, or returns "".
func diffTree(want, got map[string]string) string {
	for p, w := range want {
		g, ok := got[p]
		switch {
		case !ok:
			return p + " is missing"
		case g != w:
			return fmt.Sprintf("%s is %s, want %s", p, g, w)
		}
	}
	for p := range got {
		if _, ok := want[p]; !ok {
			return p + " is extra"
		}
	}
	return ""
}

func killEnvInt(t *testing.T, name string, def int) int {
	t.Helper()
	s := os.Getenv(name)
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		t.Fatalf("%s=%q: want a positive integer", name, s)
	}
	return n
}
