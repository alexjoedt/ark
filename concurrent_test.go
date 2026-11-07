package ark_test

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkfs"
	"github.com/alexjoedt/ark/arkuri"
)

// The concurrent backup test starts concWriters children, the re-exec'd test
// binary, that each open the same repository and run one Backup. The sources
// share most of their files, so the children Put the same chunk names
// concurrently. Every child reports its lock events on fd 3 and blocks after
// each until the parent acks it with a byte on fd 4, so the parent sees every
// release before the lock object goes and can order the children:
//
//  1. the backups hold their shared locks until all of them hold one, so they
//     overlap however small they are;
//  2. a prune child, started then with WithLockWait, is rejected at least once
//     before the backups go on, and acquires its exclusive lock after them;
//  3. it holds that lock until a late backup, started with WithLockWait, was
//     rejected; the late backup completes after the prune.
//
// No two children may hold conflicting locks at once. The base snapshot is
// taken afterwards.
//
//	ARK_CONC_RUNS  iterations, default 1
//	ARK_CONC_SEED  seed of the file contents, printed on every run
const (
	concEnvRepo = "ARK_CONC_CHILD_REPO"
	concEnvOp   = "ARK_CONC_CHILD_OP" // backup or prune
	concEnvSrc  = "ARK_CONC_CHILD_SRC"
	concEnvTag  = "ARK_CONC_CHILD_TAG"
)

const concWriters = 8

// concLockWait is the children's WithLockWait. The prune and the late backup
// each wait up to that long, so the parent waits longer than both.
const concLockWait = time.Minute

// Child indexes after the concWriters backups.
const (
	concPruner = concWriters
	concLate   = concWriters + 1
)

func TestConcurrentBackups(t *testing.T) {
	if testing.Short() {
		t.Skip("concurrent backup test spawns processes")
	}
	runConcurrentBackups(t, newKillRepo)
}

// TestConcurrentBackupsSFTP is the same run against the sftp server named by
// ARK_SFTP_TEST_URI.
func TestConcurrentBackupsSFTP(t *testing.T) {
	if testing.Short() {
		t.Skip("concurrent backup test spawns processes")
	}
	runConcurrentBackups(t, newSFTPKillRepo(t, "conc"))
}

// runConcurrentBackups runs the concurrent backup test ARK_CONC_RUNS times
// against the repositories newRepo creates, stopping at the first failure.
func runConcurrentBackups(t *testing.T, newRepo func(context.Context, *testing.T) (string, ark.Backend, func() error)) {
	t.Helper()
	runs := killEnvInt(t, "ARK_CONC_RUNS", 1)
	for i := range runs {
		if !t.Run(fmt.Sprintf("run%d", i+1), func(t *testing.T) { runConcurrentOnce(t, newRepo) }) {
			return
		}
	}
}

func runConcurrentOnce(t *testing.T, newRepo func(context.Context, *testing.T) (string, ark.Backend, func() error)) {
	ctx := context.Background()
	ref, raw, remove := newRepo(ctx, t)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("repository kept at %s", ref)
		} else if err := remove(); err != nil {
			t.Errorf("remove %s: %v", ref, err)
		}
		_ = raw.Close()
	})

	bytesSrc, seed := seededChaCha8(t, "ARK_CONC_SEED")
	t.Logf("rerun with ARK_CONC_SEED=%d", seed)
	shared := make([][]byte, 4)
	for i := range shared {
		shared[i] = make([]byte, 96<<10)
		_, _ = bytesSrc.Read(shared[i])
	}
	repo, err := ark.Open(ctx, keepOpen{raw}, testPassword)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = repo.Close() }()

	want := map[string]map[string]string{}
	backupEnv := func(tag string) []string {
		src := t.TempDir()
		writeSharedTree(t, src, shared)
		for j := range 2 {
			data := make([]byte, 64<<10)
			_, _ = bytesSrc.Read(data)
			writeTestFile(t, filepath.Join(src, fmt.Sprintf("own%d.bin", j)), data)
		}
		want[tag] = readTree(t, src)
		return []string{concEnvRepo + "=" + ref, concEnvOp + "=backup", concEnvSrc + "=" + src, concEnvTag + "=" + tag}
	}
	lateEnv := backupEnv("late")

	children := make([]*concChild, concWriters+2)
	events := make(chan concEvent)
	start := func(i int, name string, env []string) {
		if children[i] != nil {
			t.Fatalf("child %d (%s) started twice", i, name)
		}
		children[i] = startConcChild(t, name, env)
		go children[i].readEvents(t.Context(), i, events)
	}
	for i := range concWriters {
		tag := fmt.Sprintf("conc-%d", i)
		start(i, tag, backupEnv(tag))
	}
	ack := func(i int) {
		if _, err := children[i].gate.Write([]byte{1}); err != nil {
			t.Fatalf("ack child %d: %v", i, err)
		}
	}

	n := len(children)
	acquired, released, rejected := make([]int, n), make([]int, n), make([]int, n)
	held := map[int]bool{} // child -> exclusive
	open := concWriters
	timeout := time.After(3 * concLockWait)
	for open > 0 {
		var ev concEvent
		select {
		case ev = <-events:
		case <-timeout:
			t.Fatalf("timed out, %d children still running, holders %v", open, held)
		}
		i := ev.child
		if ev.event == "" {
			open--
			if acquired[i] != 1 || released[i] != 1 {
				t.Fatalf("child %d stopped reporting after %d acquired and %d released locks, want 1 each",
					i, acquired[i], released[i])
			}
			continue
		}
		if ev.exclusive != (i == concPruner) {
			t.Fatalf("child %d reported %s with exclusive %v", i, ev.event, ev.exclusive)
		}
		switch ev.event {
		case ark.LockAcquired:
			for j, exclusive := range held {
				if exclusive || ev.exclusive {
					t.Fatalf("child %d acquired a lock while child %d held a conflicting one", i, j)
				}
			}
			held[i] = ev.exclusive
			acquired[i]++
		case ark.LockReleased:
			if _, ok := held[i]; !ok {
				t.Fatalf("child %d released a lock it does not hold", i)
			}
			delete(held, i)
			released[i]++
		case ark.LockLeftOver:
			t.Fatalf("child %d could not delete its released lock, which blocks the others until it is stale", i)
		case ark.LockRejected:
			if i < concWriters {
				t.Fatalf("backup child %d was rejected", i)
			}
			rejected[i]++
		default:
			t.Fatalf("child %d reported %s", i, ev.event)
		}

		switch {
		case i < concWriters && ev.event == ark.LockAcquired:
			// Held until the pruner was rejected.
			if countPositive(acquired[:concWriters]) == concWriters {
				start(concPruner, "prune", []string{concEnvRepo + "=" + ref, concEnvOp + "=prune"})
				open++
			}
			continue
		case i == concPruner && ev.event == ark.LockRejected && rejected[i] == 1:
			for j := range concWriters {
				ack(j)
			}
		case i == concPruner && ev.event == ark.LockAcquired:
			// Held until the late backup was rejected.
			start(concLate, "late", lateEnv)
			open++
			continue
		case i == concLate && ev.event == ark.LockRejected && rejected[i] == 1:
			ack(concPruner)
		}
		ack(i)
	}
	if rejected[concPruner] == 0 || rejected[concLate] == 0 {
		t.Fatalf("prune rejected %d times, late backup %d times, want at least once each",
			rejected[concPruner], rejected[concLate])
	}
	for i, c := range children {
		if err := c.cmd.Wait(); err != nil {
			t.Fatalf("child %d: %v", i, err)
		}
	}

	// The base snapshot is taken last: it shares the children's chunks and
	// owns one file they lack, so forgetting it leaves chunks for Prune to
	// remove.
	baseSrc := t.TempDir()
	writeSharedTree(t, baseSrc, shared)
	baseOnly := make([]byte, 64<<10)
	_, _ = bytesSrc.Read(baseOnly)
	writeTestFile(t, filepath.Join(baseSrc, "base.bin"), baseOnly)
	fsSrc, err := arkfs.NewSource(baseSrc)
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	base, err := repo.Backup(ctx, fsSrc, ark.WithTags("base"))
	if err != nil {
		t.Fatalf("Backup base: %v", err)
	}

	forgot, err := repo.Forget(ctx, ark.ForgetPolicy{}, ark.SnapshotFilter{IDs: []string{base.ID}})
	if err != nil {
		t.Fatalf("Forget base: %v", err)
	}
	if len(forgot.Remove) != 1 || forgot.Remove[0].ID != base.ID {
		t.Fatalf("Forget removed %d snapshots, want only the base snapshot", len(forgot.Remove))
	}
	pruned, err := repo.Prune(ctx)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	// base.bin's chunks, which no child shares, plus the base root tree.
	if wantMin := base.Stats.ChunksNew + 1; base.Stats.ChunksNew < 1 || pruned.BlobsRemoved < wantMin {
		t.Fatalf("Prune removed %d objects, want at least %d (%d base-only chunks and a tree)",
			pruned.BlobsRemoved, wantMin, base.Stats.ChunksNew)
	}
	snaps, err := repo.Snapshots(ctx, ark.SnapshotFilter{})
	if err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	if len(snaps) != len(want) {
		t.Fatalf("got %d snapshots, want %d", len(snaps), len(want))
	}
	seen := map[string]bool{}
	for _, s := range snaps {
		if len(s.Tags) != 1 || want[s.Tags[0]] == nil || seen[s.Tags[0]] {
			t.Fatalf("snapshot %s has unexpected tags %v", s.ID, s.Tags)
		}
		tag := s.Tags[0]
		seen[tag] = true
		dst := t.TempDir()
		sink, err := arkfs.NewSink(dst)
		if err != nil {
			t.Fatalf("NewSink: %v", err)
		}
		if _, err := repo.Restore(ctx, s.ID, sink); err != nil {
			t.Fatalf("Restore %s (%s): %v", s.ID, tag, err)
		}
		if diff := diffTree(want[tag], readTree(t, dst)); diff != "" {
			t.Fatalf("snapshot %s (%s) restores a different tree: %s", s.ID, tag, diff)
		}
	}
	check, err := repo.Check(ctx)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(check.Errors) > 0 {
		t.Fatalf("Check: %v", check.Errors)
	}
}

func writeSharedTree(t *testing.T, dir string, shared [][]byte) {
	t.Helper()
	for j, data := range shared {
		writeTestFile(t, filepath.Join(dir, "shared", fmt.Sprintf("s%d.bin", j)), data)
	}
}

func countPositive(ns []int) int {
	n := 0
	for _, v := range ns {
		if v > 0 {
			n++
		}
	}
	return n
}

// concChild is a running child: events is the read end of its fd 3, gate the
// write end of its fd 4, which acks its events.
type concChild struct {
	cmd    *exec.Cmd
	out    bytes.Buffer
	events *os.File
	gate   *os.File
}

// concEvent is one line a child reported; an empty event means its fd 3 closed.
type concEvent struct {
	child     int
	event     string
	exclusive bool
}

// startConcChild starts the child named name. A cleanup kills it, waits for
// it and logs its output when the test failed, so a child never outlives the
// test.
func startConcChild(t *testing.T, name string, env []string) *concChild {
	t.Helper()
	evR, evW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	gateR, gateW, err := os.Pipe()
	if err != nil {
		_, _ = evR.Close(), evW.Close()
		t.Fatalf("pipe: %v", err)
	}
	c := &concChild{events: evR, gate: gateW}
	c.cmd, err = startChild("TestConcurrentChild", env, &c.out, evW, gateR)
	if err != nil {
		_, _ = evR.Close(), gateW.Close()
		t.Fatalf("start child %s: %v", name, err)
	}
	t.Cleanup(func() {
		_ = c.cmd.Process.Kill()
		_ = c.cmd.Wait()
		_, _ = evR.Close(), gateW.Close()
		if t.Failed() {
			t.Logf("child %s output:\n%s", name, c.out.Bytes())
		}
	})
	return c
}

// readEvents sends the events of child i until its fd 3 closes or ctx ends.
func (c *concChild) readEvents(ctx context.Context, i int, events chan<- concEvent) {
	send := func(ev concEvent) bool {
		select {
		case events <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}
	sc := bufio.NewScanner(c.events)
	for sc.Scan() {
		event, kind, _ := strings.Cut(sc.Text(), " ")
		if !send(concEvent{child: i, event: event, exclusive: kind == "exclusive"}) {
			return
		}
	}
	send(concEvent{child: i})
}

// TestConcurrentChild is a child of TestConcurrentBackups: one Backup of
// concEnvSrc or one Prune, as concEnvOp says, of the repository at
// concEnvRepo, waiting for a blocked lock.
func TestConcurrentChild(t *testing.T) {
	ref := os.Getenv(concEnvRepo)
	if ref == "" {
		t.Skip("runs only as a child of TestConcurrentBackups")
	}
	ctx := context.Background()
	b, err := arkuri.Open(ctx, ref)
	if err != nil {
		t.Fatalf("open %s: %v", ref, err)
	}
	repo, err := ark.Open(ctx, b, testPassword, ark.WithLockWait(concLockWait))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = repo.Close() }()

	events, gate := os.NewFile(3, "events"), os.NewFile(4, "gate")
	defer func() { _ = events.Close() }()
	ark.SetLockEvent(func(event string, exclusive bool) {
		kind := "shared"
		if exclusive {
			kind = "exclusive"
		}
		if _, err := fmt.Fprintf(events, "%s %s\n", event, kind); err != nil {
			t.Errorf("report %s: %v", event, err)
		}
		// The parent's ack; EOF once it is gone.
		_, _ = gate.Read(make([]byte, 1))
	})
	switch op := os.Getenv(concEnvOp); op {
	case "backup":
		src, err := arkfs.NewSource(os.Getenv(concEnvSrc))
		if err != nil {
			t.Fatalf("NewSource: %v", err)
		}
		if _, err := repo.Backup(ctx, src, ark.WithTags(os.Getenv(concEnvTag))); err != nil {
			t.Fatalf("Backup: %v", err)
		}
	case "prune":
		if _, err := repo.Prune(ctx); err != nil {
			t.Fatalf("Prune: %v", err)
		}
	default:
		t.Fatalf("%s=%q: want backup or prune", concEnvOp, op)
	}
}
