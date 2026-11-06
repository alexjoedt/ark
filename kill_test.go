package ark_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkfs"
	"github.com/alexjoedt/ark/arklocal"
	"github.com/alexjoedt/ark/chunker"
)

// The kill harness runs backup, forget and prune against an arklocal
// repository in a child process, the re-exec'd test binary, and SIGKILLs it
// after a random delay. Backups take a changing tree, forgets remove a few
// snapshots by ID. After every kill the repository must pass Check, every
// snapshot must restore to the tree it was taken of, a snapshot that was seen
// once must stay until a forget names it, and a forget that finished must
// have removed its targets. A killed prune is rerun and must leave Check
// clean. Restores run once per snapshot and again in a full pass before the
// repository is replaced; Check covers the rest in between.
//
//	ARK_KILL_RUNS  iterations, default 5
//	ARK_KILL_SEED  seed of the ops, tree changes and kill delays, printed on every run
const (
	killEnvOp   = "ARK_KILL_CHILD_OP"
	killEnvRepo = "ARK_KILL_CHILD_REPO"
	killEnvSrc  = "ARK_KILL_CHILD_SRC"
	killEnvTag  = "ARK_KILL_CHILD_TAG"
	killEnvIDs  = "ARK_KILL_CHILD_IDS"
)

// killOps is drawn uniformly, so backup runs half of the iterations.
var killOps = []string{"backup", "backup", "forget", "prune"}

func TestKillHarness(t *testing.T) {
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
	st := &killState{
		want:     map[string]map[string]string{},
		restored: map[string]bool{},
		must:     map[string]bool{},
		gone:     map[string]bool{},
		ids:      map[string]string{},
	}
	full := map[string]time.Duration{} // longest finished run per op
	runsOf, kills := map[string]int{}, map[string]int{}
	var repoDir string
	var b ark.Backend

	for i := range runs {
		// ponytail: Check verifies every snapshot, so a fresh repository every
		// killRepoSpan runs keeps the harness linear in runs.
		if i%killRepoSpan == 0 {
			if b != nil {
				st.fullPass(ctx, t, b, fmt.Sprintf("seed %d before iteration %d, full pass", seed, i))
			}
			repoDir, b = newKillRepo(ctx, t)
			clear(st.must)
			clear(st.ids)
		}
		// Every iteration draws the same values from rng whatever the op and
		// whatever earlier kills left behind, so a seed replays the sequence.
		mutateKillTree(t, rng, bytesSrc, src)
		op := killOps[rng.IntN(len(killOps))]
		frac, pick := rng.Float64(), rng.Uint64()

		var targets []string
		if op == "forget" {
			if targets = st.forgetTargets(pick); targets == nil {
				op = "backup"
			}
		}
		tag := fmt.Sprintf("kill-%d", i)
		env := []string{killEnvOp + "=" + op, killEnvRepo + "=" + repoDir}
		switch op {
		case "backup":
			st.want[tag] = readTree(t, src)
			env = append(env, killEnvSrc+"="+src, killEnvTag+"="+tag)
		case "forget":
			ids := make([]string, len(targets))
			for j, tg := range targets {
				ids[j] = st.ids[tg]
			}
			env = append(env, killEnvIDs+"="+strings.Join(ids, ","))
		}

		// The first run of each op goes to the end; the longest finished one
		// bounds that op's kill delays.
		delay := time.Duration(frac * float64(full[op]))
		took, killed, out, err := runKillChild(env, delay)
		runsOf[op]++
		point := fmt.Sprintf("seed %d iteration %d, %s exited after %v", seed, i, op, took)
		if killed {
			kills[op]++
			point = fmt.Sprintf("seed %d iteration %d, %s killed after %v", seed, i, op, delay)
		}
		if err != nil {
			t.Fatalf("%s: child: %v\n%s", point, err, out)
		}
		if !killed {
			full[op] = max(full[op], took)
		}

		switch op {
		case "backup":
			if !killed {
				st.must[tag] = true
			}
		case "forget":
			for _, tg := range targets {
				delete(st.must, tg)
				if !killed {
					st.gone[tg] = true
				}
			}
		}
		st.check(ctx, t, b, point)
		if op == "prune" && killed {
			pruneKillRepo(ctx, t, b, point)
			st.check(ctx, t, b, point+", after prune rerun")
		}
	}

	for _, op := range []string{"backup", "forget", "prune"} {
		t.Logf("%s: %d runs, %d killed before finishing", op, runsOf[op], kills[op])
	}
	st.fullPass(ctx, t, b, fmt.Sprintf("seed %d final full pass", seed))
}

// killRepoSpan is the number of runs that share one repository.
const killRepoSpan = 25

// killState is what the parent expects of the repository.
type killState struct {
	want     map[string]map[string]string // tag -> tree the snapshot was taken of
	restored map[string]bool              // snapshot ids already compared
	must     map[string]bool              // tags that have to be present
	gone     map[string]bool              // tags a finished forget removed
	ids      map[string]string            // tag -> snapshot id
}

// forgetTargets picks one to three tags to forget, always leaving one
// snapshot. It returns nil when fewer than two snapshots exist.
func (st *killState) forgetTargets(pick uint64) []string {
	tags := slices.Sorted(maps.Keys(st.must))
	if len(tags) < 2 {
		return nil
	}
	r := rand.New(rand.NewPCG(pick, pick))
	n := 1 + r.IntN(min(3, len(tags)-1))
	targets := make([]string, n)
	for j, k := range r.Perm(len(tags))[:n] {
		targets[j] = tags[k]
	}
	return targets
}

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

// TestKillHarnessChild is the child process of TestKillHarness: one backup,
// forget or prune, named by killEnvOp, against the repository in killEnvRepo.
func TestKillHarnessChild(t *testing.T) {
	repoDir := os.Getenv(killEnvRepo)
	if repoDir == "" {
		t.Skip("runs only as the child of TestKillHarness")
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

	var run func() error
	switch op := os.Getenv(killEnvOp); op {
	case "backup":
		src, err := arkfs.NewSource(os.Getenv(killEnvSrc))
		if err != nil {
			t.Fatalf("NewSource: %v", err)
		}
		run = func() error {
			_, err := repo.Backup(ctx, src, ark.WithTags(os.Getenv(killEnvTag)))
			return err
		}
	case "forget":
		ids := strings.Split(os.Getenv(killEnvIDs), ",")
		run = func() error {
			_, err := repo.Forget(ctx, ark.ForgetPolicy{}, ark.SnapshotFilter{IDs: ids})
			return err
		}
	case "prune":
		run = func() error {
			_, err := repo.Prune(ctx)
			return err
		}
	default:
		t.Fatalf("unknown op %q", op)
	}

	// fd 3 is the pipe runKillChild waits on: the kill delay starts here.
	ready := os.NewFile(3, "ready")
	if _, err := ready.Write([]byte{1}); err != nil {
		t.Fatalf("signal ready: %v", err)
	}
	_ = ready.Close()
	if err := run(); err != nil {
		t.Fatalf("%s: %v", os.Getenv(killEnvOp), err)
	}
}

// runKillChild runs one op in a child process with env added to its
// environment and kills it delay after the child reports it is about to
// start; a zero delay lets it finish. took is the time from that report to
// the exit. killed is false when the child exited first, err is set when it
// exited unsuccessfully.
func runKillChild(env []string, delay time.Duration) (took time.Duration, killed bool, out []byte, err error) {
	var buf bytes.Buffer
	ready, w, err := os.Pipe()
	if err != nil {
		return 0, false, nil, err
	}
	defer func() { _ = ready.Close() }()
	cmd := exec.Command(os.Args[0], "-test.run=^TestKillHarnessChild$")
	cmd.Env = append(os.Environ(), env...)
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
		return 0, false, buf.Bytes(), fmt.Errorf("child exited before the op started: %w", err)
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

// pruneKillRepo reruns prune after a killed one.
func pruneKillRepo(ctx context.Context, t *testing.T, b ark.Backend, point string) {
	t.Helper()
	repo, err := ark.Open(ctx, b, testPassword)
	if err != nil {
		t.Fatalf("%s: Open: %v", point, err)
	}
	defer func() { _ = repo.Close() }()
	if _, err := repo.Prune(ctx); err != nil {
		t.Fatalf("%s: prune rerun: %v", point, err)
	}
}

// fullPass restores every snapshot again.
func (st *killState) fullPass(ctx context.Context, t *testing.T, b ark.Backend, point string) {
	t.Helper()
	clear(st.restored)
	st.check(ctx, t, b, point)
}

// check opens the repository the way a fresh process would, runs Check and
// restores every snapshot not restored yet. Every tag in must has to be
// present, none in gone; every present tag joins must.
func (st *killState) check(ctx context.Context, t *testing.T, b ark.Backend, point string) {
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
	found := map[string]bool{}
	for _, s := range snaps {
		if len(s.Tags) != 1 || st.want[s.Tags[0]] == nil {
			t.Fatalf("%s: snapshot %s has unknown tags %v", point, s.ID, s.Tags)
		}
		tag := s.Tags[0]
		if st.gone[tag] {
			t.Fatalf("%s: snapshot %s (%s) was forgotten but is back", point, s.ID, tag)
		}
		found[tag] = true
		st.ids[tag] = s.ID
		if st.restored[s.ID] {
			continue
		}
		dst := t.TempDir()
		sink, err := arkfs.NewSink(dst)
		if err != nil {
			t.Fatalf("NewSink: %v", err)
		}
		if _, err := repo.Restore(ctx, s.ID, sink); err != nil {
			t.Fatalf("%s: Restore %s (%s): %v", point, s.ID, tag, err)
		}
		if diff := diffTree(st.want[tag], readTree(t, dst)); diff != "" {
			t.Fatalf("%s: snapshot %s (%s) restores a different tree: %s", point, s.ID, tag, diff)
		}
		st.restored[s.ID] = true
	}
	for _, tag := range slices.Sorted(maps.Keys(st.must)) {
		if !found[tag] {
			t.Fatalf("%s: snapshot %s is missing", point, tag)
		}
	}
	maps.Copy(st.must, found)
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
